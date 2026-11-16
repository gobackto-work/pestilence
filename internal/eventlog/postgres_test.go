package eventlog

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/gobackto-work/pestilence/internal/sqlbackend/testutil"
)

func TestPostgresRecordSurvivesReopen(t *testing.T) {
	if os.Getenv("PESTILENCE_TEST_POSTGRES_URL") == "" {
		t.Skip("requires the isolated PostgreSQL test database")
	}
	dsn := testutil.PostgresSchema(t)
	log, err := OpenPostgres(dsn)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	f := fixture{log: log, runID: newID(), workspaceID: newID(), ownerID: newID()}
	report := f.report(StateRunning)
	first, err := log.Append(ctx, report)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := log.EnsurePull(ctx, f.ownerID)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.AdvanceCursor(ctx, sub.ID, first.Sequence); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	log, err = OpenPostgres(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	again, err := log.Append(ctx, report)
	if err != nil || again.Appended || again.Sequence != first.Sequence {
		t.Fatalf("duplicate after reopen: %+v, %v", again, err)
	}
	stored, events, err := log.Outstanding(ctx, sub.ID, 10)
	if err != nil || stored.Cursor != first.Sequence || len(events) != 0 {
		t.Fatalf("subscription after reopen: %+v, %v, %v", stored, events, err)
	}
	runs, err := log.Runs(ctx, f.ownerID, 10)
	if err != nil || len(runs) != 1 || runs[0].ID != f.runID {
		t.Fatalf("runs after reopen: %+v, %v", runs, err)
	}
}

// A cursor must never pass a lower sequence whose transaction has not committed.
func TestPostgresSerialisesSequenceUntilCommit(t *testing.T) {
	if os.Getenv("PESTILENCE_TEST_POSTGRES_URL") == "" {
		t.Skip("requires the isolated PostgreSQL test database")
	}
	f := newFixture(t)
	ctx := context.Background()
	sub, err := f.log.EnsurePull(ctx, f.ownerID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.log.beginWrite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	first := f.report(StateRunning)
	at := time.Now().UTC()
	sequence, err := insertEvent(ctx, tx, first, KindRunStarted, "", at, "{}")
	if err != nil {
		t.Fatal(err)
	}
	if err := upsertRun(ctx, tx, first, at, sequence); err != nil {
		t.Fatal(err)
	}

	second := f.report(StateRunning)
	second.RunID = newID()
	done := make(chan error, 1)
	go func() {
		_, err := f.log.Append(ctx, second)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("second writer passed an uncommitted event: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	_, visible, err := f.log.Outstanding(ctx, sub.ID, 100)
	if err != nil || len(visible) != 0 {
		t.Fatalf("uncommitted events visible: %v, error %v", visible, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second writer did not resume after commit")
	}
	_, visible, err = f.log.Outstanding(ctx, sub.ID, 100)
	if err != nil || len(visible) != 2 {
		t.Fatalf("read committed events: %v, error %v", visible, err)
	}
	if visible[0].Sequence != sequence || visible[1].Sequence <= sequence {
		t.Fatalf("events are not visible in sequence order: %v", visible)
	}
}

func TestPostgresConcurrentDuplicateIsIdempotent(t *testing.T) {
	if os.Getenv("PESTILENCE_TEST_POSTGRES_URL") == "" {
		t.Skip("requires the isolated PostgreSQL test database")
	}
	f := newFixture(t)
	report := f.report(StateRunning)
	type result struct {
		append AppendResult
		err    error
	}
	const writers = 8
	results := make(chan result, writers)
	for range writers {
		go func() {
			got, err := f.log.Append(context.Background(), report)
			results <- result{got, err}
		}()
	}
	var sequence int64
	appended := 0
	for range writers {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		if sequence == 0 {
			sequence = r.append.Sequence
		}
		if r.append.Sequence != sequence {
			t.Fatalf("duplicate returned sequence %d, want %d", r.append.Sequence, sequence)
		}
		if r.append.Appended {
			appended++
		}
	}
	if appended != 1 || len(f.events(t)) != 1 {
		t.Fatalf("appended %d times, want exactly one", appended)
	}
}
