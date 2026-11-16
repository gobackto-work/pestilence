// Command migrate-sqlite-postgres copies an offline Pestilence record into PostgreSQL.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gobackto-work/pestilence/internal/eventlog"
	"github.com/gobackto-work/pestilence/internal/store"

	_ "modernc.org/sqlite" // Read the offline legacy databases during migration.
)

type table struct {
	name    string
	columns string
}

var workspaceTables = []table{{"workspaces", "id, slug, owner_id, namespace, hostname, state, created_at, last_active_at, deleted_at, last_error, cpu, memory, storage, max_pods"}}
var eventTables = []table{
	{"events", "sequence, event_id, run_id, workspace_id, owner_id, kind, previous_state, state, occurred_at, recorded_at, attributes"},
	{"runs", "id, workspace_id, owner_id, state, mode, started_at, updated_at, ended_at, last_sequence"},
	{"subscriptions", "id, principal_id, cursor"},
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "migration failed:", err)
		os.Exit(1)
	}
}

func run() error {
	workspacePath := flag.String("workspace-db", "", "offline SQLite workspace database path")
	eventPath := flag.String("event-db", "", "offline SQLite event database path")
	offline := flag.Bool("offline-confirmed", false, "confirm all SQLite writers are stopped")
	flag.Parse()
	if !*offline || *workspacePath == "" || *eventPath == "" {
		return errors.New("both SQLite paths and -offline-confirmed are required")
	}
	url := os.Getenv("PESTILENCE_POSTGRES_URL")
	if url == "" {
		return errors.New("PESTILENCE_POSTGRES_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	counts, err := migrate(ctx, *workspacePath, *eventPath, url)
	if err != nil {
		return err
	}
	fmt.Printf("Migrated workspaces=%d events=%d runs=%d subscriptions=%d\n", counts[0], counts[1], counts[2], counts[3])
	return nil
}

func openReadOnly(path string) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(absolute); err != nil {
		return nil, err
	}
	// mode=ro prevents the migration from changing or creating a source database.
	return sql.Open("sqlite", "file:"+filepath.ToSlash(absolute)+"?mode=ro")
}

func migrate(ctx context.Context, workspacePath, eventPath, dsn string) ([]int64, error) {
	workspaces, err := openReadOnly(workspacePath)
	if err != nil {
		return nil, fmt.Errorf("open workspace source: %w", err)
	}
	defer func() { _ = workspaces.Close() }()
	events, err := openReadOnly(eventPath)
	if err != nil {
		return nil, fmt.Errorf("open event source: %w", err)
	}
	defer func() { _ = events.Close() }()

	db, err := openDestination(dsn)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "LOCK TABLE workspaces, events, runs, subscriptions IN ACCESS EXCLUSIVE MODE"); err != nil {
		return nil, fmt.Errorf("lock destination: %w", err)
	}
	counts, err := copyAll(ctx, workspaces, events, tx)
	if err != nil {
		return nil, err
	}
	if err := restoreSequence(ctx, events, tx); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit migration: %w", err)
	}
	return counts, nil
}

func openDestination(dsn string) (*sql.DB, error) {
	// Initialise the exact same schema the runtime uses.
	workspaceStore, err := store.OpenPostgres(dsn)
	if err != nil {
		return nil, err
	}
	_ = workspaceStore.Close()
	eventStore, err := eventlog.OpenPostgres(dsn)
	if err != nil {
		return nil, err
	}
	_ = eventStore.Close()
	return sql.Open("pgx", dsn)
}

func copyAll(ctx context.Context, workspaces, events *sql.DB, tx *sql.Tx) ([]int64, error) {
	all := append(append([]table{}, workspaceTables...), eventTables...)
	for _, table := range all {
		var count int64
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table.name).Scan(&count); err != nil {
			return nil, err
		}
		if count != 0 {
			return nil, fmt.Errorf("destination table %s is not empty", table.name)
		}
	}
	counts := make([]int64, 0, len(all))
	for i, table := range all {
		source := events
		if i == 0 {
			source = workspaces
		}
		count, err := copyTable(ctx, source, tx, table)
		if err != nil {
			return nil, fmt.Errorf("copy %s: %w", table.name, err)
		}
		counts = append(counts, count)
	}
	return counts, nil
}

func restoreSequence(ctx context.Context, events *sql.DB, tx *sql.Tx) error {
	var highWater int64
	if err := events.QueryRowContext(ctx, "SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'events'), 0)").Scan(&highWater); err != nil {
		return fmt.Errorf("read SQLite sequence: %w", err)
	}
	if highWater < 0 {
		return errors.New("SQLite sequence is negative")
	}
	var err error
	if highWater == 0 {
		_, err = tx.ExecContext(ctx, "SELECT setval(pg_get_serial_sequence('events', 'sequence'), 1, false)")
	} else {
		_, err = tx.ExecContext(ctx, "SELECT setval(pg_get_serial_sequence('events', 'sequence'), $1, true)", highWater)
	}
	if err != nil {
		return fmt.Errorf("restore event sequence: %w", err)
	}
	return nil
}

func copyTable(ctx context.Context, source *sql.DB, dest *sql.Tx, table table) (int64, error) {
	// Table and column names come from the fixed list above, never from input.
	//nolint:gosec // G202: only fixed identifiers are interpolated.
	rows, err := source.QueryContext(ctx, "SELECT "+table.columns+" FROM "+table.name)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	columns := strings.Split(table.columns, ", ")
	params := make([]string, len(columns))
	for i := range params {
		params[i] = "$" + strconv.Itoa(i+1)
	}
	//nolint:gosec // G202: identifiers are fixed and values remain parameters.
	query := "INSERT INTO " + table.name + " (" + table.columns + ") VALUES (" + strings.Join(params, ", ") + ")"
	var count int64
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return 0, err
		}
		for i, value := range values {
			if data, ok := value.([]byte); ok {
				values[i] = string(data)
			}
		}
		if _, err := dest.ExecContext(ctx, query, values...); err != nil {
			return 0, err
		}
		count++
	}
	return count, rows.Err()
}
