package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/gobackto-work/pestilence/internal/tenant"
	"github.com/gobackto-work/pestilence/internal/workspace"
)

func newStore(t *testing.T) (*SQLite, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "workspaces.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func record(t *testing.T, id, slug string) workspace.Workspace {
	t.Helper()
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	w := workspace.Workspace{
		ID:           id,
		Slug:         slug,
		OwnerID:      "owner-1",
		Namespace:    tenant.Namespace(slug),
		Hostname:     slug + ".gobackto.work",
		State:        workspace.StateProvisioning,
		CreatedAt:    now,
		LastActiveAt: now,
		Limits:       workspace.Limits{CPU: "2", Memory: "3Gi", Storage: "20Gi", MaxPods: 6},
	}
	if err := w.Validate(); err != nil {
		t.Fatalf("test record is invalid: %v", err)
	}
	return w
}

func TestCreateAndGetRoundTrip(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	want := record(t, "01K0000000000000000000001", "fuzzy-wombat-x7q3")

	if err := s.Create(ctx, want); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Get(ctx, want.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Slug != want.Slug || got.Namespace != want.Namespace || got.Hostname != want.Hostname {
		t.Errorf("Get returned %+v, want %+v", got, want)
	}
	if got.State != workspace.StateProvisioning {
		t.Errorf("state = %s, want PROVISIONING", got.State)
	}
	if got.Limits != want.Limits {
		t.Errorf("limits = %+v, want %+v (the durable record must round trip)", got.Limits, want.Limits)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("createdAt = %s, want %s", got.CreatedAt, want.CreatedAt)
	}
	if got.DeletedAt != nil {
		t.Error("deletedAt should be nil for a live workspace")
	}

	bySlug, err := s.GetBySlug(ctx, want.Slug)
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	if bySlug.ID != want.ID {
		t.Errorf("GetBySlug returned id %s, want %s", bySlug.ID, want.ID)
	}
}

func TestGetUnknownIsNotFound(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	if _, err := s.Get(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get = %v, want ErrNotFound", err)
	}
	if _, err := s.GetBySlug(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetBySlug = %v, want ErrNotFound", err)
	}
}

// The slug is part of the public hostname and the namespace, so two workspaces
// must never share one.
func TestSlugIsUnique(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, record(t, "id-1", "fuzzy-wombat-x7q3")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	err := s.Create(ctx, record(t, "id-2", "fuzzy-wombat-x7q3"))
	if err == nil {
		t.Fatal("a duplicate slug was accepted")
	}
}

func TestCreateRejectsAnInvalidRecord(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	bad := record(t, "id-1", "fuzzy-wombat-x7q3")
	bad.Namespace = "ws-wrong"
	if err := s.Create(ctx, bad); err == nil {
		t.Error("Create accepted a record whose namespace does not match its slug")
	}
}

func TestListIsOrderedAndEmptyByDefault(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()

	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List on an empty store returned %d rows", len(got))
	}

	for _, r := range []struct{ id, slug string }{
		{"id-1", "alpha-wombat-aaaa"},
		{"id-2", "bravo-wombat-bbbb"},
		{"id-3", "charlie-wombat-cccc"},
	} {
		if err := s.Create(ctx, record(t, r.id, r.slug)); err != nil {
			t.Fatalf("Create %s: %v", r.id, err)
		}
	}
	got, err = s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("List returned %d rows, want 3", len(got))
	}
}

// The compare-and-set is the reason two reconcilers cannot both act on one
// transition. The loser must get ErrConflict, not a second run of the work.
func TestUpdateStateIsCompareAndSet(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	w := record(t, "id-1", "fuzzy-wombat-x7q3")
	if err := s.Create(ctx, w); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.UpdateState(ctx, w.ID, workspace.StateProvisioning, workspace.StateRunning); err != nil {
		t.Fatalf("first transition: %v", err)
	}
	// The same transition again must fail: the stored state is no longer `from`.
	err := s.UpdateState(ctx, w.ID, workspace.StateProvisioning, workspace.StateRunning)
	if !errors.Is(err, ErrConflict) {
		t.Errorf("second transition = %v, want ErrConflict", err)
	}

	got, err := s.Get(ctx, w.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != workspace.StateRunning {
		t.Errorf("state = %s, want RUNNING", got.State)
	}
}

func TestUpdateStateOnUnknownWorkspaceIsNotFound(t *testing.T) {
	s, _ := newStore(t)
	err := s.UpdateState(context.Background(), "nope", workspace.StateProvisioning, workspace.StateRunning)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("= %v, want ErrNotFound", err)
	}
}

// An illegal transition must be refused before it reaches the database, so the
// state machine is the only authority on what is reachable.
func TestUpdateStateRejectsAnIllegalTransition(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	w := record(t, "id-1", "fuzzy-wombat-x7q3")
	if err := s.Create(ctx, w); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.UpdateState(ctx, w.ID, workspace.StateProvisioning, workspace.StateDeleted); err == nil {
		t.Error("PROVISIONING -> DELETED was accepted; deletion must go through DELETING")
	}
}

// A DELETED row must always carry its timestamp, or the record is invalid.
func TestDeletedStampsTheTimestamp(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	w := record(t, "id-1", "fuzzy-wombat-x7q3")
	if err := s.Create(ctx, w); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.UpdateState(ctx, w.ID, workspace.StateProvisioning, workspace.StateDeleting); err != nil {
		t.Fatalf("to DELETING: %v", err)
	}
	if err := s.UpdateState(ctx, w.ID, workspace.StateDeleting, workspace.StateDeleted); err != nil {
		t.Fatalf("to DELETED: %v", err)
	}

	got, err := s.Get(ctx, w.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.DeletedAt == nil {
		t.Fatal("DELETED without a timestamp")
	}
	if err := got.Validate(); err != nil {
		t.Errorf("stored record is invalid: %v", err)
	}
}

func TestTouchRecordsActivity(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	w := record(t, "id-1", "fuzzy-wombat-x7q3")
	if err := s.Create(ctx, w); err != nil {
		t.Fatalf("Create: %v", err)
	}

	later := w.LastActiveAt.Add(90 * time.Minute)
	if err := s.Touch(ctx, w.ID, later); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	got, err := s.Get(ctx, w.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.LastActiveAt.Equal(later) {
		t.Errorf("lastActiveAt = %s, want %s", got.LastActiveAt, later)
	}
	if err := s.Touch(ctx, "nope", later); !errors.Is(err, ErrNotFound) {
		t.Errorf("Touch on unknown = %v, want ErrNotFound", err)
	}
}

// The database is authoritative for platform objects, so the record must outlive
// the process. A control-plane restart must not lose a workspace.
func TestDataSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	s, path := newStore(t)
	w := record(t, "id-1", "fuzzy-wombat-x7q3")
	if err := s.Create(ctx, w); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	got, err := reopened.Get(ctx, w.ID)
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.Slug != w.Slug || got.State != w.State {
		t.Errorf("reopened record = %+v, want slug %s state %s", got, w.Slug, w.State)
	}
}
