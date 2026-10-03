// Package eventlog persists the durable record of run events.
//
// Three things need to name something that happened: a push notification, an MCP
// subscription that resumes after a reconnect, and a parent run that waits for a
// child. All three need one record with an identity and an order, so all three read
// this package.
//
// The record has its own SQLite file. It never shares the file that holds the
// workspace registry. That file is read-mostly and holds a row per workspace; this
// one is append-heavy and holds a row per transition. One writer for both would make
// them contend for one lock, and the two have different durability needs.
//
// See docs/event-record.md for the contract this implements.
package eventlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"

	"github.com/oklog/ulid/v2"
)

var (
	// ErrNotFound means there is no such run or subscription.
	ErrNotFound = errors.New("not found")

	// ErrTransition means the reported state is not reachable from the state the
	// record holds.
	ErrTransition = errors.New("illegal state transition")

	// ErrNoTransition means the record already holds the reported state. It is not a
	// failure: a runtime asserts its current state again after a dropped report, and
	// most of those reports describe a state the record already has.
	ErrNoTransition = errors.New("state is already recorded")

	// ErrInvalidAttribute means an attribute is outside the closed schema. It is the
	// payload rule, enforced.
	ErrInvalidAttribute = errors.New("invalid attribute")

	// ErrInvalid means the report is malformed.
	ErrInvalid = errors.New("invalid report")

	// ErrInternalSubscription means a caller asked for an internal subscription
	// through the general path. Only the control plane may create one, because it
	// receives every tenant's events.
	ErrInternalSubscription = errors.New("internal subscriptions are created by EnsureInternal")
)

// maxAttributesBytes caps a payload. Retention is expressed as a count of events, so
// one large payload would break the equivalence between that count and the size.
const maxAttributesBytes = 1024

// State is a run's lifecycle state.
type State string

// The closed set of run states. `budget_exhausted` is separate from `failed` because
// run-until-complete makes a mid-run limit normal rather than rare, and a person can act
// on a budget stop but not on a failure.
const (
	StateRunning         State = "running"
	StateWaiting         State = "waiting"
	StateSucceeded       State = "succeeded"
	StateFailed          State = "failed"
	StateCancelled       State = "cancelled"
	StateBudgetExhausted State = "budget_exhausted"
)

// Kind is the transition that one event records.
type Kind string

// The closed set of kinds. There is no open extension point: a new kind is a change to
// the contract in docs/event-record.md.
const (
	KindRunStarted         Kind = "run.started"
	KindRunWaiting         Kind = "run.waiting"
	KindRunResumed         Kind = "run.resumed"
	KindRunSucceeded       Kind = "run.succeeded"
	KindRunFailed          Kind = "run.failed"
	KindRunCancelled       Kind = "run.cancelled"
	KindRunBudgetExhausted Kind = "run.budget_exhausted"
)

// Terminal reports whether the run has ended.
func (s State) Terminal() bool {
	switch s {
	case StateSucceeded, StateFailed, StateCancelled, StateBudgetExhausted:
		return true
	}
	return false
}

// Valid reports whether the state is one of the closed set.
func (s State) Valid() bool {
	switch s {
	case StateRunning, StateWaiting, StateSucceeded, StateFailed, StateCancelled, StateBudgetExhausted:
		return true
	}
	return false
}

// Notifiable reports whether the kind reaches a person.
//
// run.resumed is recorded so that a subscriber can reconstruct a run's state. It is
// never sent to a notification sink.
func (k Kind) Notifiable() bool {
	switch k {
	case KindRunWaiting, KindRunSucceeded, KindRunFailed, KindRunCancelled, KindRunBudgetExhausted:
		return true
	}
	return false
}

// terminalKind names the event for a run that ends.
func terminalKind(s State) Kind {
	switch s {
	case StateSucceeded:
		return KindRunSucceeded
	case StateFailed:
		return KindRunFailed
	case StateCancelled:
		return KindRunCancelled
	case StateBudgetExhausted:
		return KindRunBudgetExhausted
	}
	return ""
}

// transition derives the event from the state a run leaves and the state it enters.
//
// The runtime reports a state and this package derives the event. A runtime that
// computed its own kind could disagree with the record, and the disagreement would be
// silent. A state that is not reachable is refused.
func transition(from, to State) (Kind, error) {
	if !to.Valid() {
		return "", fmt.Errorf("%w: unknown state %q", ErrTransition, to)
	}
	if from == "" {
		if to != StateRunning {
			return "", fmt.Errorf("%w: a run starts in %s, not %s", ErrTransition, StateRunning, to)
		}
		return KindRunStarted, nil
	}
	if !from.Valid() {
		return "", fmt.Errorf("%w: unknown state %q", ErrTransition, from)
	}
	if from.Terminal() {
		return "", fmt.Errorf("%w: %s is terminal, so the run cannot become %s", ErrTransition, from, to)
	}
	if from == to {
		return "", ErrNoTransition
	}
	switch {
	case from == StateRunning && to == StateWaiting:
		return KindRunWaiting, nil
	case from == StateWaiting && to == StateRunning:
		return KindRunResumed, nil
	case to.Terminal():
		return terminalKind(to), nil
	}
	return "", fmt.Errorf("%w: %s to %s", ErrTransition, from, to)
}

// attrType is the type of one attribute value.
type attrType int

const (
	attrInt attrType = iota
	attrIdent
)

// attributeRules is the closed schema for Event.Attributes.
//
// There is no free-text value. That is what makes the payload rule enforceable
// instead of aspirational: no key exists that can carry a prompt, a completion, a
// file path, an error string or a credential. A new attribute needs a change here,
// and that is the point.
var attributeRules = map[string]attrType{
	"tokens":      attrInt,
	"duration_ms": attrInt,
	"tools":       attrInt,
	"exit_code":   attrInt,
	"model":       attrIdent,
	"trigger":     attrIdent,
}

// identRe bounds an identifier. A prompt cannot pass: it has spaces and punctuation.
var identRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// validateAttributes enforces the closed schema and the size cap.
func validateAttributes(a map[string]any) error {
	for k, v := range a {
		want, ok := attributeRules[k]
		if !ok {
			return fmt.Errorf("%w: %q is not an allowed attribute", ErrInvalidAttribute, k)
		}
		if err := validateAttribute(k, v, want); err != nil {
			return err
		}
	}
	if len(a) == 0 {
		return nil
	}
	encoded, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidAttribute, err)
	}
	if len(encoded) > maxAttributesBytes {
		return fmt.Errorf("%w: %d bytes exceeds the %d byte cap",
			ErrInvalidAttribute, len(encoded), maxAttributesBytes)
	}
	return nil
}

// validateAttribute checks one value against the type its key declares.
func validateAttribute(key string, value any, want attrType) error {
	switch want {
	case attrInt:
		return validateInt(key, value)
	case attrIdent:
		s, ok := value.(string)
		if !ok {
			return fmt.Errorf("%w: %q must be a string", ErrInvalidAttribute, key)
		}
		if !identRe.MatchString(s) {
			return fmt.Errorf("%w: %q is not a bounded identifier", ErrInvalidAttribute, key)
		}
	}
	return nil
}

// validateInt accepts the float64 a JSON decoder produces and the int a Go caller
// writes. It refuses a fraction, because no attribute is fractional.
func validateInt(key string, value any) error {
	switch n := value.(type) {
	case float64:
		if n != math.Trunc(n) {
			return fmt.Errorf("%w: %q must be a whole number", ErrInvalidAttribute, key)
		}
	case int, int64:
	default:
		return fmt.Errorf("%w: %q must be a number", ErrInvalidAttribute, key)
	}
	return nil
}

// checkID refuses an identifier that is not a ULID. Every id in the record is one, and a
// malformed id would otherwise become a row that nothing can find.
func checkID(id string) error {
	if _, err := ulid.Parse(id); err != nil {
		return fmt.Errorf("%w: id %q", ErrInvalid, id)
	}
	return nil
}

// Report is a state that a runtime observed for a run.
//
// A runtime reports a state and not a transition. The record derives the kind and the
// previous state, so a runtime cannot disagree with the record about what came before.
//
// The ingest layer must check that WorkspaceID is the workspace the caller is
// authenticated for. This package cannot check that, because it does not authenticate
// anything.
type Report struct {
	// ID is the idempotency key. The runtime supplies it and a retry carries the same
	// value. It has a unique index, so a repeated report adds no row.
	ID string

	RunID       string
	WorkspaceID string
	OwnerID     string

	// State is the state the run entered. It is the only state a caller reports.
	State State

	// OccurredAt is when the runtime observed the transition.
	OccurredAt time.Time

	// Attributes carries counts, durations and identifiers under a closed schema.
	Attributes map[string]any
}

// Validate refuses a malformed report at the boundary.
func (r Report) Validate() error {
	// Ordered, so that a report with more than one bad field always reports the same
	// one. A map here would make the error depend on iteration order.
	for _, f := range []struct{ name, value string }{
		{"event", r.ID}, {"run", r.RunID}, {"workspace", r.WorkspaceID}, {"owner", r.OwnerID},
	} {
		if _, err := ulid.Parse(f.value); err != nil {
			return fmt.Errorf("%w: %s id %q", ErrInvalid, f.name, f.value)
		}
	}
	if !r.State.Valid() {
		return fmt.Errorf("%w: unknown state %q", ErrInvalid, r.State)
	}
	if r.OccurredAt.IsZero() {
		return fmt.Errorf("%w: occurred_at is required", ErrInvalid)
	}
	return validateAttributes(r.Attributes)
}

// Event is one recorded state transition.
type Event struct {
	// Sequence is assigned by the record.
	Sequence int64

	// ID is the idempotency key that the runtime supplied.
	ID string

	RunID       string
	WorkspaceID string
	OwnerID     string

	// Kind and PreviousState are derived by the record.
	Kind          Kind
	PreviousState State

	// State is the state the run entered.
	State State

	// OccurredAt is when the runtime observed the transition.
	OccurredAt time.Time

	// RecordedAt is when the record appended the event.
	RecordedAt time.Time

	// Attributes carries counts, durations and identifiers under a closed schema.
	Attributes map[string]any
}

// AppendResult reports what the record did with a report.
type AppendResult struct {
	// Sequence is the sequence of the event that records the state. It is the
	// sequence of the first event for a repeated idempotency key, and the sequence of
	// the event that set the state when the record already holds it.
	Sequence int64

	// Appended is false when the report added no event. A runtime treats both cases
	// as success, because it only needs the sequence.
	Appended bool
}

// Run is the retained summary of one run. One row survives when the event rows are
// pruned.
type Run struct {
	ID          string
	WorkspaceID string
	OwnerID     string
	State       State
	StartedAt   time.Time
	UpdatedAt   time.Time

	// EndedAt is set when the run reaches a terminal state.
	EndedAt *time.Time

	// LastSequence is the sequence of the event that last changed the run.
	LastSequence int64
}

// SubscriptionKind separates the two shapes of subscription.
type SubscriptionKind string

const (
	// KindInternal is the subscription that town holds. It takes every event,
	// because town delivers for every user and decides per event and per device.
	KindInternal SubscriptionKind = "internal"

	// KindExternal is a subscription that one principal holds. It takes the events
	// of that principal and no others.
	KindExternal SubscriptionKind = "external"
)

// SubscriptionState is the lifecycle state of a subscription.
type SubscriptionState string

const (
	// SubscriptionActive means the subscription still holds a cursor.
	SubscriptionActive SubscriptionState = "active"

	// SubscriptionFailed means delivery passed its retry limit. A failed subscription
	// stops holding a cursor, so a subscriber that is down for a long time cannot hold
	// events past their age limit for ever.
	SubscriptionFailed SubscriptionState = "failed"
)

// Subscription is a durable request for events.
type Subscription struct {
	ID          string
	Kind        SubscriptionKind
	PrincipalID string

	// URL and Secret are present for an external subscription only. The URL is
	// untrusted input and the delivery layer must treat it as such.
	URL    string
	Secret string

	Cursor    int64
	State     SubscriptionState
	CreatedAt time.Time
}

// Active reports whether the subscription still holds a cursor.
func (s Subscription) Active() bool { return s.State == SubscriptionActive }
