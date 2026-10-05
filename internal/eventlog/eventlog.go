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

	// ErrModeChanged means a report declared a mode that differs from the one the run was
	// recorded with. A run's mode decides its state machine, so it cannot change.
	ErrModeChanged = errors.New("the run's mode cannot change")

	// ErrInvalidAttribute means an attribute is outside the closed schema. It is the
	// payload rule, enforced.
	ErrInvalidAttribute = errors.New("invalid attribute")

	// ErrInvalid means the report is malformed.
	ErrInvalid = errors.New("invalid report")
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

// Mode says whether a run can block on a person. It is the property that decides which
// states the run can reach, so it is part of the record and not a display detail.
type Mode string

const (
	// ModeInteractive is a run with a person at the other end. It reaches `waiting` when
	// the agent asks a question, and `resumed` when the answer arrives. The root agent is
	// the interactive run.
	ModeInteractive Mode = "interactive"

	// ModeBatch is a run nobody can answer: a worker, spawned as a job. It never reaches
	// `waiting`, because a run that waits for a person who is not there waits for ever.
	ModeBatch Mode = "batch"
)

// Valid reports whether the mode is one of the closed set.
func (m Mode) Valid() bool {
	switch m {
	case ModeInteractive, ModeBatch:
		return true
	}
	return false
}

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

// transition derives the event from the state a run leaves, the state it enters, and the
// mode the run was recorded with.
//
// The runtime reports a state and this package derives the event. A runtime that computed
// its own kind could disagree with the record, and the disagreement would be silent. A
// state that is not reachable is refused.
func transition(from, to State, mode Mode) (Kind, error) {
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
		if mode != ModeInteractive {
			// Without this a batch run could ask for input that can never arrive, and a
			// parent waiting on that run would wait for ever.
			return "", fmt.Errorf("%w: a %s run cannot wait for a person", ErrTransition, mode)
		}
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

	// Mode says whether the run can block on a person. The first report of a run sets it,
	// and every later report must agree.
	Mode Mode

	// OccurredAt is when the runtime observed the transition.
	OccurredAt time.Time

	// Attributes carries counts, durations and identifiers under a closed schema.
	Attributes map[string]any
}

// opaqueID bounds an identifier that another component generates.
//
// It is a bound and not a format. The run id belongs to the runtime and the owner id
// belongs to town, so pinning either shape here would couple the record to a decision in
// another repository. What it refuses is the value that actually breaks a row: an empty
// one, or one long enough to be carrying something.
var opaqueID = regexp.MustCompile(`^[A-Za-z0-9._:#@-]{1,128}$`)

// Validate refuses a malformed report at the boundary.
func (r Report) Validate() error {
	// The workspace is ours, so its identifier format is pinned. The others are not.
	if err := checkID(r.WorkspaceID); err != nil {
		return fmt.Errorf("%w: workspace id %q", ErrInvalid, r.WorkspaceID)
	}
	// Ordered, so that a report with more than one bad field always reports the same
	// one. A map here would make the error depend on iteration order.
	for _, f := range []struct{ name, value string }{
		{"event", r.ID}, {"run", r.RunID}, {"owner", r.OwnerID},
	} {
		if !opaqueID.MatchString(f.value) {
			return fmt.Errorf("%w: %s id %q", ErrInvalid, f.name, f.value)
		}
	}
	if !r.State.Valid() {
		return fmt.Errorf("%w: unknown state %q", ErrInvalid, r.State)
	}
	if !r.Mode.Valid() {
		return fmt.Errorf("%w: unknown mode %q", ErrInvalid, r.Mode)
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

// Every subscription takes the events of ONE principal and no others.
//
// An earlier design had an internal subscription that took every tenant's events, so that
// town could fan them out from one cursor. It needed a credential that is not a user's, and
// town can already speak for any user: it mints the assertions. Reading per principal
// therefore removes a concept, a credential and the invariant that protected it.

// Run is the retained summary of one run. One row survives when the event rows are pruned.
//
// This is the LOG, and the events are the QUEUE. A reader that wants to show what has happened
// reads here. A reader that wants to deliver events reads them with a cursor and acknowledges
// them, and it must acknowledge: the lowest cursor of any subscription is also the retention
// bound, so a consumer that reads and never acknowledges pins the bound at zero and the record
// never prunes, by age as well as by count.
type Run struct {
	ID          string
	WorkspaceID string
	OwnerID     string
	State       State

	// Mode is what the run was recorded with. It does not change.
	Mode Mode

	StartedAt time.Time
	UpdatedAt time.Time

	// EndedAt is set when the run reached a terminal state.
	EndedAt *time.Time

	// LastSequence is the sequence of the event that last changed the run. It orders the list,
	// and it is unique per run, which a timestamp is not.
	LastSequence int64
}

// Subscription is a durable request for events.
//
// It holds a cursor and nothing else. An earlier version carried a kind, a lifecycle state, a
// callback URL and a signing secret, all of them for a push sink with no caller. A column
// with one legal value is a placeholder for a design rather than a design, so they come back
// with the sink, where a test can exercise them against something real.
type Subscription struct {
	ID string

	// PrincipalID is the owner whose events this subscription takes. Every subscription has
	// one, and there is no subscription that sees more than one principal's events.
	PrincipalID string

	// Cursor is the last sequence the subscriber acknowledged.
	Cursor int64
}
