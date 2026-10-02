package workspace

import (
	"testing"
	"time"

	"github.com/gobackto-work/pestilence/internal/tenant"
)

func validWorkspace() Workspace {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	slug := "fuzzy-wombat-x7q3"
	return Workspace{
		ID:           "01K0000000000000000000000",
		Slug:         slug,
		OwnerID:      "owner-1",
		Namespace:    tenant.Namespace(slug),
		Hostname:     slug + ".gobackto.work",
		State:        StateProvisioning,
		CreatedAt:    now,
		LastActiveAt: now,
		Limits:       Limits{CPU: "2", Memory: "3Gi", Storage: "20Gi", MaxPods: 6},
	}
}

func TestValidWorkspacePassesValidation(t *testing.T) {
	if err := validWorkspace().Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// Each of these is a record the platform could not act on, so it must be refused
// at the boundary rather than discovered mid-provision.
func TestValidationRejectsBadRecords(t *testing.T) {
	now := time.Now().UTC()
	cases := map[string]func(*Workspace){
		"empty id":           func(w *Workspace) { w.ID = "" },
		"empty owner":        func(w *Workspace) { w.OwnerID = "" },
		"invalid slug":       func(w *Workspace) { w.Slug = "../escape" },
		"namespace mismatch": func(w *Workspace) { w.Namespace = "ws-something-else" },
		"empty hostname":     func(w *Workspace) { w.Hostname = "" },
		"unknown state":      func(w *Workspace) { w.State = "PENDING" },
		"zero createdAt":     func(w *Workspace) { w.CreatedAt = time.Time{} },
		// Relative to CreatedAt, not to the wall clock: using time.Now() here made
		// the test pass or fail depending on the hour it ran.
		"lastActive before":     func(w *Workspace) { w.LastActiveAt = w.CreatedAt.Add(-time.Hour) },
		"deleted without stamp": func(w *Workspace) { w.State = StateDeleted },
		"stamp without deleted": func(w *Workspace) { w.DeletedAt = &now },
	}
	for name, mutate := range cases {
		w := validWorkspace()
		mutate(&w)
		if err := w.Validate(); err == nil {
			t.Errorf("%s: Validate accepted an invalid record", name)
		}
	}
}

// A DELETED workspace is the only state carrying a timestamp, and it must be
// consistent both ways.
func TestDeletedStateCarriesATimestamp(t *testing.T) {
	w := validWorkspace()
	w.State = StateDeleting
	moved, err := w.Transition(StateDeleted)
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if moved.DeletedAt == nil {
		t.Fatal("Transition to DELETED did not stamp deletedAt")
	}
	if err := moved.Validate(); err != nil {
		t.Errorf("Validate after transition: %v", err)
	}
}

// The transition table is the state machine. Anything not listed must be
// refused, so an illegal move is a bug rather than a state nothing understands.
func TestTransitionTable(t *testing.T) {
	allowed := map[State][]State{
		StateProvisioning: {StateRunning, StateFailed, StateDeleting},
		StateRunning:      {StateSuspended, StateFailed, StateDeleting},
		StateSuspended:    {StateRunning, StateFailed, StateDeleting},
		StateFailed:       {StateProvisioning, StateDeleting},
		StateDeleting:     {StateDeleted},
		StateDeleted:      {},
	}
	for from, want := range allowed {
		permitted := map[State]bool{}
		for _, to := range want {
			permitted[to] = true
		}
		for _, to := range AllStates() {
			got := from.CanTransitionTo(to)
			if got != permitted[to] {
				t.Errorf("%s -> %s = %v, want %v", from, to, got, permitted[to])
			}
		}
	}
}

func TestDeletedIsTerminal(t *testing.T) {
	if !StateDeleted.Terminal() {
		t.Error("DELETED should be terminal")
	}
	for _, s := range AllStates() {
		if s == StateDeleted {
			continue
		}
		if s.Terminal() {
			t.Errorf("%s should not be terminal", s)
		}
	}
}

// A failed provision must be retryable, or a transient error bricks a workspace.
func TestFailedIsRecoverable(t *testing.T) {
	if !StateFailed.CanTransitionTo(StateProvisioning) {
		t.Error("FAILED must be able to re-enter PROVISIONING")
	}
}

func TestTransitionRejectsIllegalMoves(t *testing.T) {
	w := validWorkspace()
	if _, err := w.Transition(StateDeleted); err == nil {
		t.Error("PROVISIONING -> DELETED should be rejected; deletion goes through DELETING")
	}
	if _, err := w.Transition("NONSENSE"); err == nil {
		t.Error("an unknown state should be rejected")
	}
}
