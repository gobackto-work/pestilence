// Package workspace holds the platform's durable model of a workspace.
//
// The platform database is authoritative for platform objects; Kubernetes is
// authoritative for current runtime state. Keeping those separate is why State
// below is never derived from what happens to be running.
package workspace

import (
	"fmt"
	"time"

	"github.com/gobackto-work/pestilence/internal/tenant"
)

// State is the explicit lifecycle state of a workspace.
//
// It is NEVER inferred from currently running pods. A control-plane restart must
// not change what the platform believes, and "no pods" is ambiguous between
// suspended, failed, and mid-provision — three states with different recovery
// paths.
type State string

// The lifecycle states. `transitions` below is what may follow what.
const (
	StateProvisioning State = "PROVISIONING"
	StateRunning      State = "RUNNING"
	StateSuspended    State = "SUSPENDED"
	StateFailed       State = "FAILED"
	StateDeleting     State = "DELETING"
	StateDeleted      State = "DELETED"
)

// transitions is the complete set of permitted state changes. Anything absent is
// rejected, so an illegal transition surfaces as an error rather than silently
// producing a state nothing knows how to handle.
var transitions = map[State][]State{
	StateProvisioning: {StateRunning, StateFailed, StateDeleting},
	StateRunning:      {StateSuspended, StateFailed, StateDeleting},
	StateSuspended:    {StateRunning, StateFailed, StateDeleting},
	// FAILED is recoverable: a retry re-enters provisioning.
	StateFailed:   {StateProvisioning, StateDeleting},
	StateDeleting: {StateDeleted},
	// DELETED is terminal. A tombstone may be retained for audit, but it never
	// moves again.
	StateDeleted: nil,
}

// AllStates returns every state, for validation and tests.
func AllStates() []State {
	return []State{StateProvisioning, StateRunning, StateSuspended, StateFailed, StateDeleting, StateDeleted}
}

// Valid reports whether s is one of the known states.
func (s State) Valid() bool {
	for _, known := range AllStates() {
		if s == known {
			return true
		}
	}
	return false
}

// CanTransitionTo reports whether a change from s to next is permitted.
func (s State) CanTransitionTo(next State) bool {
	for _, allowed := range transitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// Terminal reports whether no further transition is possible.
func (s State) Terminal() bool { return s == StateDeleted }

// Limits is the durable record of what a workspace was provisioned with.
//
// It is stored rather than recomputed: the cluster's ResourceQuota is derived
// from it, so a later change to the platform's defaults must not silently resize
// an existing workspace.
type Limits struct {
	CPU     string
	Memory  string
	Storage string
	MaxPods int32
}

// Workspace is the platform's record for one workspace.
type Workspace struct {
	ID      string
	Slug    string
	OwnerID string
	// Namespace is derived from the slug and stored anyway, because it appears
	// in audit records and must not change if the derivation ever does.
	Namespace    string
	Hostname     string
	State        State
	CreatedAt    time.Time
	LastActiveAt time.Time
	DeletedAt    *time.Time
	// LastError is why the workspace is not healthy, for the UI to show. Empty
	// when there is nothing to report, and cleared on success.
	LastError string
	Limits    Limits
}

// Validate rejects a record the platform could not act on. It is called on the
// way into the store, so a malformed workspace is refused at the boundary rather
// than discovered mid-provision.
func (w Workspace) Validate() error {
	if w.ID == "" {
		return fmt.Errorf("workspace: id is empty")
	}
	if w.OwnerID == "" {
		return fmt.Errorf("workspace %s: ownerId is empty", w.ID)
	}
	if err := tenant.ValidateSlug(w.Slug); err != nil {
		return fmt.Errorf("workspace %s: %w", w.ID, err)
	}
	if want := tenant.Namespace(w.Slug); w.Namespace != want {
		return fmt.Errorf("workspace %s: namespace %q does not match slug %q (want %q)", w.ID, w.Namespace, w.Slug, want)
	}
	if w.Hostname == "" {
		return fmt.Errorf("workspace %s: hostname is empty", w.ID)
	}
	if !w.State.Valid() {
		return fmt.Errorf("workspace %s: unknown state %q", w.ID, w.State)
	}
	if w.CreatedAt.IsZero() {
		return fmt.Errorf("workspace %s: createdAt is zero", w.ID)
	}
	if w.LastActiveAt.Before(w.CreatedAt) {
		return fmt.Errorf("workspace %s: lastActiveAt precedes createdAt", w.ID)
	}
	if w.State == StateDeleted && w.DeletedAt == nil {
		return fmt.Errorf("workspace %s: state DELETED without deletedAt", w.ID)
	}
	if w.State != StateDeleted && w.DeletedAt != nil {
		return fmt.Errorf("workspace %s: deletedAt set while state is %s", w.ID, w.State)
	}
	return nil
}

// Transition applies a state change, returning an error if it is not permitted.
func (w Workspace) Transition(to State) (Workspace, error) {
	if !w.State.CanTransitionTo(to) {
		return Workspace{}, fmt.Errorf("workspace %s: cannot move from %s to %s", w.ID, w.State, to)
	}
	w.State = to
	if to == StateDeleted {
		now := time.Now().UTC()
		w.DeletedAt = &now
	}
	return w, nil
}
