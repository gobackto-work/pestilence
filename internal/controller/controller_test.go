package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/gobackto-work/pestilence/internal/store"
	"github.com/gobackto-work/pestilence/internal/tenant"
	"github.com/gobackto-work/pestilence/internal/workspace"
)

// fakeProvisioner stands in for the reconciler, so the loop is tested without a
// cluster. It records what it was asked to do and can be made to fail.
type fakeProvisioner struct {
	ensureErr error
	deleteErr error
	waitErr   error
	ensured   []tenant.Spec
	deleted   []tenant.Spec
	waited    []tenant.Spec
}

func (f *fakeProvisioner) Ensure(_ context.Context, spec tenant.Spec) error {
	if f.ensureErr != nil {
		return f.ensureErr
	}
	f.ensured = append(f.ensured, spec)
	return nil
}

func (f *fakeProvisioner) Delete(_ context.Context, spec tenant.Spec) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, spec)
	return nil
}

func (f *fakeProvisioner) WaitForDeletion(_ context.Context, spec tenant.Spec, _ time.Duration) error {
	if f.waitErr != nil {
		return f.waitErr
	}
	f.waited = append(f.waited, spec)
	return nil
}

func newController(t *testing.T, prov Provisioner) (*Controller, store.Store) {
	t.Helper()
	s, err := store.OpenSQLite(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	c := New(s, prov, Config{BrokerImage: "example/broker:dev", AgentImage: "example/agent:dev"},
		time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return c, s
}

func insert(t *testing.T, s store.Store, id, slug string, state workspace.State) {
	t.Helper()
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	w := workspace.Workspace{
		ID:           id,
		Slug:         slug,
		OwnerID:      "owner-1",
		Namespace:    tenant.Namespace(slug),
		Hostname:     slug + ".gobackto.work",
		State:        state,
		CreatedAt:    now,
		LastActiveAt: now,
		Limits:       workspace.Limits{CPU: "1", Memory: "2Gi", Storage: "10Gi", MaxPods: 4},
	}
	if state == workspace.StateDeleted {
		at := now
		w.DeletedAt = &at
	}
	if err := s.Create(context.Background(), w); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

func stateOf(t *testing.T, s store.Store, id string) workspace.Workspace {
	t.Helper()
	w, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return w
}

// A successful provision moves the workspace to RUNNING, and the spec must come
// from the RECORD, not from anything live.
func TestProvisioningBecomesRunning(t *testing.T) {
	prov := &fakeProvisioner{}
	c, s := newController(t, prov)
	insert(t, s, "id-1", "fuzzy-wombat-x7q3", workspace.StateProvisioning)

	if err := c.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}

	got := stateOf(t, s, "id-1")
	if got.State != workspace.StateRunning {
		t.Errorf("state = %s, want RUNNING", got.State)
	}
	if got.LastError != "" {
		t.Errorf("lastError = %q, want empty after success", got.LastError)
	}
	if len(prov.ensured) != 1 {
		t.Fatalf("Ensure called %d times, want 1", len(prov.ensured))
	}
	spec := prov.ensured[0]
	if spec.Slug != got.Slug || spec.Hostname != got.Hostname || spec.OwnerID != got.OwnerID {
		t.Errorf("spec %+v does not match the record %+v", spec, got)
	}
	if spec.BrokerImage == "" || spec.AgentImage == "" {
		t.Error("images from config were not applied")
	}
	if spec.Limits.Pods != got.Limits.MaxPods {
		t.Errorf("spec pods = %d, want the recorded %d", spec.Limits.Pods, got.Limits.MaxPods)
	}
}

// A failed provision must record WHY and become FAILED, and FAILED must be
// retryable: a transient error cannot brick a workspace.
func TestProvisionFailureIsRecordedAndRetryable(t *testing.T) {
	prov := &fakeProvisioner{ensureErr: errors.New("quota exceeded")}
	c, s := newController(t, prov)
	insert(t, s, "id-1", "fuzzy-wombat-x7q3", workspace.StateProvisioning)

	if err := c.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	got := stateOf(t, s, "id-1")
	if got.State != workspace.StateFailed {
		t.Fatalf("state = %s, want FAILED", got.State)
	}
	if got.LastError == "" {
		t.Error("lastError is empty; the UI would show a failure with no reason")
	}

	// The failure clears and a retry succeeds.
	prov.ensureErr = nil
	if err := s.UpdateState(context.Background(), "id-1", workspace.StateFailed, workspace.StateProvisioning); err != nil {
		t.Fatalf("retry transition: %v", err)
	}
	if err := c.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce after retry: %v", err)
	}
	got = stateOf(t, s, "id-1")
	if got.State != workspace.StateRunning {
		t.Errorf("state = %s, want RUNNING after retry", got.State)
	}
	if got.LastError != "" {
		t.Errorf("lastError = %q, want cleared", got.LastError)
	}
}

func TestDeletingBecomesDeleted(t *testing.T) {
	prov := &fakeProvisioner{}
	c, s := newController(t, prov)
	insert(t, s, "id-1", "fuzzy-wombat-x7q3", workspace.StateDeleting)

	if err := c.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	got := stateOf(t, s, "id-1")
	if got.State != workspace.StateDeleted {
		t.Errorf("state = %s, want DELETED", got.State)
	}
	if got.DeletedAt == nil {
		t.Error("DELETED without a timestamp")
	}
	if len(prov.deleted) != 1 {
		t.Errorf("Delete called %d times, want 1", len(prov.deleted))
	}
}

// A failed deletion stays in DELETING so the next pass retries it. Moving to
// FAILED would be a dead end: deletion is the only useful transition left.
func TestDeletionFailureStaysDeletingAndRetries(t *testing.T) {
	prov := &fakeProvisioner{deleteErr: errors.New("namespace stuck terminating")}
	c, s := newController(t, prov)
	insert(t, s, "id-1", "fuzzy-wombat-x7q3", workspace.StateDeleting)

	if err := c.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	got := stateOf(t, s, "id-1")
	if got.State != workspace.StateDeleting {
		t.Fatalf("state = %s, want DELETING so it retries", got.State)
	}
	if got.LastError == "" {
		t.Error("lastError is empty; a stuck deletion would be invisible")
	}

	prov.deleteErr = nil
	if err := c.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce after retry: %v", err)
	}
	if got = stateOf(t, s, "id-1"); got.State != workspace.StateDeleted {
		t.Errorf("state = %s, want DELETED after retry", got.State)
	}
}

// DELETED must mean the deletion COMPLETED, not merely that it was requested.
// Otherwise a namespace that fails to terminate leaves the record claiming success
// while the residue sits there with nothing left to retry it -- which is exactly
// what an ad-hoc hand-deletion produced during testing.
func TestDeletedIsOnlyRecordedOnceTheDeletionCompletes(t *testing.T) {
	prov := &fakeProvisioner{waitErr: errors.New("namespace still terminating")}
	c, s := newController(t, prov)
	insert(t, s, "id-1", "fuzzy-wombat-x7q3", workspace.StateDeleting)

	if err := c.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	got := stateOf(t, s, "id-1")
	if got.State != workspace.StateDeleting {
		t.Errorf("state = %s, want DELETING until the deletion actually completes", got.State)
	}
	if got.LastError == "" {
		t.Error("lastError is empty; a stuck deletion would be invisible")
	}
	if got.DeletedAt != nil {
		t.Error("deletedAt was stamped before the deletion completed")
	}

	prov.waitErr = nil
	if err := c.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce after the namespace went away: %v", err)
	}
	if got = stateOf(t, s, "id-1"); got.State != workspace.StateDeleted {
		t.Errorf("state = %s, want DELETED once the wait succeeds", got.State)
	}
}

// Steady and terminal states must be left alone. A loop that touched RUNNING
// workspaces would re-apply on every pass for no reason.
func TestSteadyStatesAreNotTouched(t *testing.T) {
	prov := &fakeProvisioner{}
	c, s := newController(t, prov)
	insert(t, s, "run-1", "alpha-wombat-aaaa", workspace.StateRunning)
	insert(t, s, "sus-1", "bravo-wombat-bbbb", workspace.StateSuspended)
	insert(t, s, "del-1", "charlie-wombat-cccc", workspace.StateDeleted)

	if err := c.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if len(prov.ensured) != 0 || len(prov.deleted) != 0 {
		t.Errorf("the loop acted on steady states: ensured=%d deleted=%d", len(prov.ensured), len(prov.deleted))
	}
	for id, want := range map[string]workspace.State{
		"run-1": workspace.StateRunning,
		"sus-1": workspace.StateSuspended,
		"del-1": workspace.StateDeleted,
	} {
		if got := stateOf(t, s, id); got.State != want {
			t.Errorf("%s state = %s, want %s", id, got.State, want)
		}
	}
}

// A record asking for more than the node can give must fail WITH A REASON rather
// than being silently resized. Validate() does not police platform maximums — they
// are policy and can change — so the controller is where they are enforced.
func TestUnprovisionableRecordFailsClearly(t *testing.T) {
	prov := &fakeProvisioner{}
	c, s := newController(t, prov)

	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	slug := "fuzzy-wombat-x7q3"
	w := workspace.Workspace{
		ID:           "id-1",
		Slug:         slug,
		OwnerID:      "owner-1",
		Namespace:    tenant.Namespace(slug),
		Hostname:     slug + ".gobackto.work",
		State:        workspace.StateProvisioning,
		CreatedAt:    now,
		LastActiveAt: now,
		Limits:       workspace.Limits{CPU: "99", Memory: "2Gi", Storage: "10Gi", MaxPods: 4},
	}
	if err := s.Create(context.Background(), w); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := c.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	got := stateOf(t, s, "id-1")
	if got.State != workspace.StateFailed {
		t.Errorf("state = %s, want FAILED", got.State)
	}
	if got.LastError == "" {
		t.Error("an unprovisionable record must record why")
	}
	if len(prov.ensured) != 0 {
		t.Error("Ensure was called despite the spec being unbuildable")
	}
}

// A RUNNING workspace must be re-applied, or a change to any reconciled object
// never reaches it and the only remedy is to recreate the workspace. That is the
// gap that bit a live test: correcting the broker's mount did not repair the
// running workspace.
func TestDriftSweepReappliesRunningWorkspaces(t *testing.T) {
	prov := &fakeProvisioner{}
	c, s := newController(t, prov)
	insert(t, s, "id-1", "fuzzy-wombat-x7q3", workspace.StateRunning)

	if err := c.DriftSweep(context.Background()); err != nil {
		t.Fatalf("DriftSweep: %v", err)
	}
	if len(prov.ensured) != 1 {
		t.Fatalf("Ensure called %d times, want 1", len(prov.ensured))
	}
	if got := stateOf(t, s, "id-1").State; got != workspace.StateRunning {
		t.Errorf("state = %s, want RUNNING: drift correction must not change lifecycle state", got)
	}
}

// The sweep is for steady states only. Acting on PROVISIONING or DELETING would
// double up with the intent loop, and acting on DELETED would resurrect a
// tombstone. SUSPENDED is excluded because Ensure applies the runtime too, and a
// suspended workspace should have none.
func TestDriftSweepIgnoresNonRunningStates(t *testing.T) {
	prov := &fakeProvisioner{}
	c, s := newController(t, prov)
	insert(t, s, "prov-1", "alpha-wombat-aaaa", workspace.StateProvisioning)
	insert(t, s, "del-1", "bravo-wombat-bbbb", workspace.StateDeleting)
	insert(t, s, "dead-1", "charlie-wombat-cccc", workspace.StateDeleted)
	insert(t, s, "fail-1", "delta-wombat-dddd", workspace.StateFailed)
	insert(t, s, "sus-1", "echo-wombat-eeee", workspace.StateSuspended)

	if err := c.DriftSweep(context.Background()); err != nil {
		t.Fatalf("DriftSweep: %v", err)
	}
	if len(prov.ensured) != 0 {
		t.Errorf("the sweep acted on %d non-running workspaces", len(prov.ensured))
	}
}

// A failed re-apply is recorded, not fatal. The workspace is running and mostly
// fine, so moving it to FAILED would be wrong and would strand it.
func TestDriftFailureIsRecordedWithoutChangingState(t *testing.T) {
	prov := &fakeProvisioner{ensureErr: errors.New("quota exceeded")}
	c, s := newController(t, prov)
	insert(t, s, "id-1", "fuzzy-wombat-x7q3", workspace.StateRunning)

	if err := c.DriftSweep(context.Background()); err != nil {
		t.Fatalf("DriftSweep: %v", err)
	}
	got := stateOf(t, s, "id-1")
	if got.State != workspace.StateRunning {
		t.Errorf("state = %s, want RUNNING: a failed re-apply must not change lifecycle state", got.State)
	}
	if got.LastError == "" {
		t.Error("lastError is empty; a workspace behind its desired state would be invisible")
	}
}

// A later success clears it, or the UI would show a stale error indefinitely.
func TestDriftSuccessClearsAPreviousError(t *testing.T) {
	prov := &fakeProvisioner{ensureErr: errors.New("transient")}
	c, s := newController(t, prov)
	insert(t, s, "id-1", "fuzzy-wombat-x7q3", workspace.StateRunning)

	if err := c.DriftSweep(context.Background()); err != nil {
		t.Fatalf("DriftSweep: %v", err)
	}
	if stateOf(t, s, "id-1").LastError == "" {
		t.Fatal("expected the failure to be recorded")
	}

	prov.ensureErr = nil
	if err := c.DriftSweep(context.Background()); err != nil {
		t.Fatalf("DriftSweep after recovery: %v", err)
	}
	if got := stateOf(t, s, "id-1").LastError; got != "" {
		t.Errorf("lastError = %q, want cleared after a successful sweep", got)
	}
}
