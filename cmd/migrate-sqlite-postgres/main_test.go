package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gobackto-work/pestilence/internal/eventlog"
	"github.com/gobackto-work/pestilence/internal/store"
	"github.com/gobackto-work/pestilence/internal/testutil"
	"github.com/oklog/ulid/v2"
)

func sqliteSource(t *testing.T, path string, statements ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create legacy fixture: %v", err)
		}
	}
}

const legacyWorkspaces = `CREATE TABLE workspaces (
 id TEXT PRIMARY KEY, slug TEXT NOT NULL UNIQUE, owner_id TEXT NOT NULL,
 namespace TEXT NOT NULL, hostname TEXT NOT NULL, state TEXT NOT NULL,
 created_at TEXT NOT NULL, last_active_at TEXT NOT NULL, deleted_at TEXT,
 last_error TEXT NOT NULL, cpu TEXT NOT NULL, memory TEXT NOT NULL,
 storage TEXT NOT NULL, max_pods INTEGER NOT NULL)`

func TestMigrationPreservesRecordsAndSequence(t *testing.T) {
	if os.Getenv("PESTILENCE_TEST_POSTGRES_URL") == "" {
		t.Skip("requires the isolated PostgreSQL test database")
	}
	ctx := context.Background()
	dsn := testutil.PostgresSchema(t)
	workspacePath := filepath.Join(t.TempDir(), "workspaces.db")
	eventPath := filepath.Join(t.TempDir(), "events.db")
	id := ulid.Make().String()
	sqliteSource(t, workspacePath, legacyWorkspaces,
		`INSERT INTO workspaces VALUES ('`+id+`','quiet-vole-abcd','github#1','ws-quiet-vole-abcd',`+
			`'quiet-vole-abcd.example.com','PROVISIONING','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z',NULL,'','1','1Gi','1Gi',2)`)
	sqliteSource(t, eventPath,
		`CREATE TABLE events (sequence INTEGER PRIMARY KEY AUTOINCREMENT, event_id TEXT NOT NULL UNIQUE,
 run_id TEXT NOT NULL, workspace_id TEXT NOT NULL, owner_id TEXT NOT NULL, kind TEXT NOT NULL,
 previous_state TEXT NOT NULL, state TEXT NOT NULL, occurred_at INTEGER NOT NULL,
 recorded_at INTEGER NOT NULL, attributes TEXT NOT NULL)`,
		`CREATE TABLE runs (id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL, owner_id TEXT NOT NULL,
 state TEXT NOT NULL, mode TEXT NOT NULL, started_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
 ended_at INTEGER, last_sequence INTEGER NOT NULL)`,
		`CREATE TABLE subscriptions (id TEXT PRIMARY KEY, principal_id TEXT NOT NULL UNIQUE, cursor INTEGER NOT NULL)`,
		`INSERT INTO events (event_id,run_id,workspace_id,owner_id,kind,previous_state,state,occurred_at,recorded_at,attributes)
 VALUES ('event-1','run-1','`+id+`','github#1','run.started','','running',1,1,'{}')`,
		`INSERT INTO events (event_id,run_id,workspace_id,owner_id,kind,previous_state,state,occurred_at,recorded_at,attributes)
 VALUES ('event-2','run-2','`+id+`','github#1','run.started','','running',1,1,'{}')`,
		`DELETE FROM events WHERE event_id = 'event-2'`,
		`INSERT INTO runs VALUES ('run-1','`+id+`','github#1','running','interactive',1,1,NULL,1)`,
		`INSERT INTO subscriptions VALUES ('sub-1','github#1',1)`)

	counts, err := migrate(ctx, workspacePath, eventPath, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if len(counts) != 4 || counts[0] != 1 || counts[1] != 1 || counts[2] != 1 || counts[3] != 1 {
		t.Fatalf("unexpected migration counts: %v", counts)
	}
	pgStore, err := store.OpenPostgres(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pgStore.Close() }()
	if got, err := pgStore.Get(ctx, id); err != nil || got.ID != id || got.OwnerID != "github#1" {
		t.Fatalf("workspace: %+v, %v", got, err)
	}
	pgLog, err := eventlog.OpenPostgres(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pgLog.Close() }()
	got, events, err := pgLog.Outstanding(ctx, "sub-1", 10)
	if err != nil || got.Cursor != 1 || len(events) != 0 {
		t.Fatalf("cursor: %+v events: %+v error: %v", got, events, err)
	}
	third, err := pgLog.Append(ctx, eventlog.Report{
		ID: "event-3", RunID: "run-3", WorkspaceID: id, OwnerID: "github#1",
		State: eventlog.StateRunning, Mode: eventlog.ModeInteractive, OccurredAt: time.Now().UTC(),
	})
	if err != nil || third.Sequence <= 2 {
		t.Fatalf("sequence reused: %+v after 2, error %v", third, err)
	}
	if _, err := migrate(ctx, workspacePath, eventPath, dsn); err == nil {
		t.Fatal("migration accepted a populated destination")
	}
}

func TestMigrationRollsBackOnInvalidSource(t *testing.T) {
	if os.Getenv("PESTILENCE_TEST_POSTGRES_URL") == "" {
		t.Skip("requires the isolated PostgreSQL test database")
	}
	dsn := testutil.PostgresSchema(t)
	workspacePath := filepath.Join(t.TempDir(), "workspaces.db")
	sqliteSource(t, workspacePath, legacyWorkspaces,
		`INSERT INTO workspaces VALUES ('id-1','quiet-vole-abcd','github#1','ws-quiet-vole-abcd',`+
			`'quiet-vole-abcd.example.com','PROVISIONING','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z',NULL,'','1','1Gi','1Gi',2)`)
	invalidPath := filepath.Join(t.TempDir(), "invalid.db")
	if err := os.WriteFile(invalidPath, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := migrate(context.Background(), workspacePath, invalidPath, dsn); err == nil {
		t.Fatal("invalid source accepted")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM workspaces").Scan(&count); err != nil || count != 0 {
		t.Fatalf("workspace destination after failed transfer: %d, %v", count, err)
	}
}
