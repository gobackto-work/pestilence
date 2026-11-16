package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/gobackto-work/pestilence/internal/eventlog"
	"github.com/gobackto-work/pestilence/internal/sqlbackend/testutil"
	"github.com/gobackto-work/pestilence/internal/workspace"
	"github.com/oklog/ulid/v2"
)

func TestPostgresRequiresURL(t *testing.T) {
	if _, _, _, err := openPostgresStores(""); err == nil {
		t.Fatal("an empty PostgreSQL URL was accepted")
	}
}

func TestPostgresStoreSelection(t *testing.T) {
	if os.Getenv("PESTILENCE_TEST_POSTGRES_URL") == "" {
		t.Skip("requires the isolated PostgreSQL test database")
	}
	dsn := testutil.PostgresSchema(t)
	workspaces, events, closeStores, err := openPostgresStores(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStores()
	ctx := context.Background()
	now := time.Now().UTC()
	id := ulid.Make().String()
	w := workspace.Workspace{
		ID: id, Slug: "quiet-vole-abcd", OwnerID: "github#1", Namespace: "ws-quiet-vole-abcd",
		Hostname: "quiet-vole-abcd.example.com", State: workspace.StateProvisioning,
		CreatedAt: now, LastActiveAt: now,
		Limits: workspace.Limits{CPU: "1", Memory: "1Gi", Storage: "1Gi", MaxPods: 2},
	}
	if err := workspaces.Create(ctx, w); err != nil {
		t.Fatal(err)
	}
	if _, err := events.Append(ctx, eventlog.Report{
		ID: "event-1", RunID: "run-1", WorkspaceID: id, OwnerID: w.OwnerID,
		State: eventlog.StateRunning, Mode: eventlog.ModeInteractive, OccurredAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := workspaces.Get(ctx, id)
	if err != nil || got.ID != id {
		t.Fatalf("read workspace: %+v, %v", got, err)
	}
	runs, err := events.Runs(ctx, w.OwnerID, 10)
	if err != nil || len(runs) != 1 || runs[0].WorkspaceID != id {
		t.Fatalf("read runs: %+v, %v", runs, err)
	}
}
