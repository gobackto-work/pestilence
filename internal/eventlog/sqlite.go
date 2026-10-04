package eventlog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	// Pure-Go SQLite, as the workspace registry uses. mattn/go-sqlite3 needs cgo and
	// the build environment sets CGO_ENABLED=0 with no C compiler.
	_ "modernc.org/sqlite"
)

const (
	// retentionAge is the age limit. The record is a queue for delivery and not a
	// history; the run summary is the history.
	retentionAge = 7 * 24 * time.Hour

	// retentionCount is the size limit, in events. It is about 0.5 MB.
	retentionCount = 1700
)

// schema is applied in order on every open. Each statement is idempotent.
var schema = []string{
	// sequence is AUTOINCREMENT and not a bare INTEGER PRIMARY KEY.
	//
	// A bare rowid is reused after the row with the highest value is deleted, and this
	// record deletes its newest rows when a workspace is deleted. A reused sequence
	// would be at or below a subscriber's cursor, so the subscriber would discard a
	// new event as one it had already seen. The event would be lost silently.
	`CREATE TABLE IF NOT EXISTS events (
        sequence       INTEGER PRIMARY KEY AUTOINCREMENT,
        event_id       TEXT NOT NULL UNIQUE,
        run_id         TEXT NOT NULL,
        workspace_id   TEXT NOT NULL,
        owner_id       TEXT NOT NULL,
        kind           TEXT NOT NULL,
        previous_state TEXT NOT NULL,
        state          TEXT NOT NULL,
        occurred_at    INTEGER NOT NULL,
        recorded_at    INTEGER NOT NULL,
        attributes     TEXT NOT NULL
    )`,

	// Ascending sequence within a run is the read path that a resumed subscriber uses.
	`CREATE INDEX IF NOT EXISTS events_run_idx ON events(run_id, sequence)`,

	// The entitlement filter reads by owner, and the delivery filter reads by
	// workspace. Both orders are by sequence.
	`CREATE INDEX IF NOT EXISTS events_owner_idx ON events(owner_id, sequence)`,
	`CREATE INDEX IF NOT EXISTS events_workspace_idx ON events(workspace_id, sequence)`,

	// The prune reads by age.
	`CREATE INDEX IF NOT EXISTS events_recorded_idx ON events(recorded_at)`,

	// One summary row per run. It survives the pruning of the event rows.
	`CREATE TABLE IF NOT EXISTS runs (
        id            TEXT PRIMARY KEY,
        workspace_id  TEXT NOT NULL,
        owner_id      TEXT NOT NULL,
        state         TEXT NOT NULL,
        mode          TEXT NOT NULL,
        started_at    INTEGER NOT NULL,
        updated_at    INTEGER NOT NULL,
        ended_at      INTEGER,
        last_sequence INTEGER NOT NULL
    )`,

	`CREATE INDEX IF NOT EXISTS runs_workspace_idx ON runs(workspace_id)`,

	// One cursor per principal, and nothing else. There is no kind, because there is one way a
	// subscription is served; no lifecycle state, because nothing can fail one; and no callback
	// URL or signing secret, because the push sink does not exist. Columns with one legal value
	// each are a placeholder for the delivery layer rather than the delivery layer.
	`CREATE TABLE IF NOT EXISTS subscriptions (
        id           TEXT PRIMARY KEY,
        principal_id TEXT NOT NULL UNIQUE,
        cursor       INTEGER NOT NULL DEFAULT 0
    )`,
}

// Log is the durable record, backed by one SQLite file.
type Log struct {
	db *sql.DB

	// maxEvents and maxAge are the retention limits. They are fields and not only
	// constants so that a test can lower them: the count limit is 1700, and a test that
	// reached it by appending would be slow for no gain.
	maxEvents int
	maxAge    time.Duration
}

// Open opens or creates the record at path and applies the schema.
func Open(path string) (*Log, error) {
	// The pragmas are in the DSN because two of them are per-connection settings. A
	// pragma run once after opening applies to one pooled connection and not to the
	// others. This driver applies _pragma to every connection it opens.
	//
	// synchronous=NORMAL is the WAL default and it can lose the last few transactions
	// on a node crash. This record drives notifications, so that is accepted.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open event record: %w", err)
	}

	// WAL lets readers run while one writer writes, so this file differs from the
	// registry, which serialises to a single connection. busy_timeout makes a second
	// writer wait rather than fail with SQLITE_BUSY.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)

	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("apply schema: %w", err)
		}
	}
	return &Log{db: db, maxEvents: retentionCount, maxAge: retentionAge}, nil
}

// Close releases the file.
func (l *Log) Close() error { return l.db.Close() }

// Append records the state that a runtime reported for a run.
//
// It is idempotent on Report.ID, and it derives the event from the run's recorded
// state, so the log is self-consistent by construction.
func (l *Log) Append(ctx context.Context, r Report) (AppendResult, error) {
	if err := r.Validate(); err != nil {
		return AppendResult{}, err
	}
	attributes, err := encodeAttributes(r.Attributes)
	if err != nil {
		return AppendResult{}, err
	}

	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return AppendResult{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The idempotency key is checked before the transition, because a retry of an
	// accepted report must not be refused as an illegal transition.
	sequence, found, err := existingEvent(ctx, tx, r.ID)
	if err != nil {
		return AppendResult{}, err
	}
	if found {
		return AppendResult{Sequence: sequence}, nil
	}

	from, mode, lastSequence, err := recordedState(ctx, tx, r.RunID)
	if err != nil {
		return AppendResult{}, err
	}
	// The mode decides the state machine, so it is set by the first report and every
	// later report must agree. Without this check a client could turn a batch run into
	// an interactive one and let it wait for a person who is not there.
	if from != "" && mode != r.Mode {
		return AppendResult{}, fmt.Errorf("%w: run %s was recorded as %s", ErrModeChanged, r.RunID, mode)
	}
	kind, err := transition(from, r.State, r.Mode)
	if errors.Is(err, ErrNoTransition) {
		// A re-assertion of a state the record already holds. It answers with the
		// sequence that set the state, so the runtime reads it as success.
		return AppendResult{Sequence: lastSequence}, nil
	}
	if err != nil {
		return AppendResult{}, fmt.Errorf("run %s: %w", r.RunID, err)
	}

	now := time.Now().UTC()
	sequence, err = insertEvent(ctx, tx, r, kind, from, now, attributes)
	if err != nil {
		return AppendResult{}, err
	}
	if err := upsertRun(ctx, tx, r, now, sequence); err != nil {
		return AppendResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return AppendResult{}, fmt.Errorf("commit event %s: %w", r.ID, err)
	}
	return AppendResult{Sequence: sequence, Appended: true}, nil
}

// existingEvent returns the sequence of the event that holds this idempotency key.
func existingEvent(ctx context.Context, tx *sql.Tx, id string) (int64, bool, error) {
	var sequence int64
	err := tx.QueryRowContext(ctx, `SELECT sequence FROM events WHERE event_id = ?`, id).Scan(&sequence)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("look up event %s: %w", id, err)
	}
	return sequence, true, nil
}

// recordedState returns the state the record holds for a run, the mode it was recorded
// with, and the sequence that set the state. A run the record has not seen has neither.
func recordedState(ctx context.Context, tx *sql.Tx, runID string) (State, Mode, int64, error) {
	var (
		state    string
		mode     string
		sequence int64
	)
	err := tx.QueryRowContext(ctx, `SELECT state, mode, last_sequence FROM runs WHERE id = ?`, runID).
		Scan(&state, &mode, &sequence)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", "", 0, nil
	case err != nil:
		return "", "", 0, fmt.Errorf("look up run %s: %w", runID, err)
	}
	return State(state), Mode(mode), sequence, nil
}

// insertEvent writes one event and returns its sequence.
func insertEvent(ctx context.Context, tx *sql.Tx, r Report, kind Kind, from State, at time.Time, attributes string) (int64, error) {
	res, err := tx.ExecContext(ctx, `
        INSERT INTO events (event_id, run_id, workspace_id, owner_id, kind,
                            previous_state, state, occurred_at, recorded_at, attributes)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.RunID, r.WorkspaceID, r.OwnerID, string(kind),
		string(from), string(r.State), millis(r.OccurredAt), millis(at), attributes)
	if err != nil {
		return 0, fmt.Errorf("insert event %s: %w", r.ID, err)
	}
	sequence, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("sequence for event %s: %w", r.ID, err)
	}
	return sequence, nil
}

// upsertRun writes the summary in the same transaction as the event, so that the two can
// never disagree. started_at keeps its first value and ended_at is stamped once.
func upsertRun(ctx context.Context, tx *sql.Tx, r Report, at time.Time, sequence int64) error {
	var ended any
	if r.State.Terminal() {
		ended = millis(at)
	}
	_, err := tx.ExecContext(ctx, `
        INSERT INTO runs (id, workspace_id, owner_id, state, mode,
                          started_at, updated_at, ended_at, last_sequence)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(id) DO UPDATE SET
            state         = excluded.state,
            updated_at    = excluded.updated_at,
            ended_at      = COALESCE(excluded.ended_at, runs.ended_at),
            last_sequence = excluded.last_sequence`,
		r.RunID, r.WorkspaceID, r.OwnerID, string(r.State), string(r.Mode),
		millis(r.OccurredAt), millis(at), ended, sequence)
	if err != nil {
		return fmt.Errorf("update run %s: %w", r.RunID, err)
	}
	return nil
}

// Outstanding returns the events that a subscription has not taken, oldest first.
//
// Every subscription takes the events of ONE principal and no others, so a principal can
// never be sent another principal's run. This is the authorisation the read endpoint depends
// on, and it lives here because only the record can apply it.
func (l *Log) Outstanding(ctx context.Context, subscriptionID string, limit int) (Subscription, []Event, error) {
	s, err := l.Subscription(ctx, subscriptionID)
	if err != nil {
		return Subscription{}, nil, err
	}

	rows, err := l.db.QueryContext(ctx, selectEvents+`
        WHERE sequence > ? AND owner_id = ? ORDER BY sequence LIMIT ?`,
		s.Cursor, s.PrincipalID, limit)
	if err != nil {
		return Subscription{}, nil, fmt.Errorf("read outstanding for %s: %w", subscriptionID, err)
	}
	defer func() { _ = rows.Close() }()
	events, err := scanEvents(rows)
	if err != nil {
		return Subscription{}, nil, err
	}
	return s, events, nil
}

// EnsurePull returns the pull subscription for one principal, creating it if it is absent.
//
// A principal is town's identifier and not ours, so it is bounded and not format-checked. That
// is the same lesson as the owner id on an event: a format belonging to another component
// cannot be pinned here, because the component will change it and this will refuse the new
// shape.
//
// This is the only way to create one, and it takes no id and no kind. A pull subscription is
// identified by its principal, so a caller cannot make a second one for the same owner and
// cannot name an owner that is not its own. The API layer is what checks that the principal
// is the one the request is authenticated as; this package cannot authenticate anything.
func (l *Log) EnsurePull(ctx context.Context, principalID string) (Subscription, error) {
	if !opaqueID.MatchString(principalID) {
		return Subscription{}, fmt.Errorf("%w: principal id %q", ErrInvalid, principalID)
	}
	// The unique constraint on the principal makes a concurrent create a no-op rather than a
	// second cursor.
	if _, err := l.db.ExecContext(ctx, `
        INSERT INTO subscriptions (id, principal_id, cursor)
        VALUES (?, ?, 0)
        ON CONFLICT DO NOTHING`,
		ulid.Make().String(), principalID); err != nil {
		return Subscription{}, fmt.Errorf("create the pull subscription for %s: %w", principalID, err)
	}

	row := l.db.QueryRowContext(ctx, selectSubscriptions+`
        WHERE principal_id = ?`, principalID)
	return scanSubscription(row)
}

// Subscription returns one subscription.
func (l *Log) Subscription(ctx context.Context, id string) (Subscription, error) {
	return scanSubscription(l.db.QueryRowContext(ctx, selectSubscriptions+` WHERE id = ?`, id))
}

// AdvanceCursor records what a subscription has taken.
//
// The cursor only moves forward. An acknowledgement for a sequence at or below it is
// stale, and moving backwards would redeliver events that were already accepted.
func (l *Log) AdvanceCursor(ctx context.Context, id string, to int64) error {
	if _, err := l.Subscription(ctx, id); err != nil {
		return err
	}
	if _, err := l.db.ExecContext(ctx,
		`UPDATE subscriptions SET cursor = ? WHERE id = ? AND cursor < ?`, to, id, to); err != nil {
		return fmt.Errorf("advance cursor for %s: %w", id, err)
	}
	return nil
}

// PruneResult reports what a prune discarded, and what it could not.
type PruneResult struct {
	ByAge   int64
	ByCount int64

	// Held counts the events that an active subscription has not taken. A record that
	// stays above the size limit because Held is large is a fault: something is not
	// consuming. The prune reports it and does not resolve it by discarding events
	// that nobody has seen.
	Held int64
}

// Prune discards events past the age limit or the size limit, whichever comes first.
//
// An event that an active subscription has not taken is never discarded.
func (l *Log) Prune(ctx context.Context, now time.Time) (PruneResult, error) {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return PruneResult{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The bound is the lowest cursor of any subscription. Nothing above it may be
	// discarded, because a subscription has not taken it. With no subscription at all every
	// event is eligible, so the bound is the newest sequence.
	var lowest sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MIN(cursor) FROM subscriptions`).Scan(&lowest); err != nil {
		return PruneResult{}, fmt.Errorf("lowest cursor: %w", err)
	}
	var bound int64
	if lowest.Valid {
		bound = lowest.Int64
	} else if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(sequence), 0) FROM events`).Scan(&bound); err != nil {
		return PruneResult{}, fmt.Errorf("newest sequence: %w", err)
	}

	var out PruneResult

	res, err := tx.ExecContext(ctx,
		`DELETE FROM events WHERE recorded_at < ? AND sequence <= ?`,
		millis(now.Add(-l.maxAge)), bound)
	if err != nil {
		return PruneResult{}, fmt.Errorf("prune by age: %w", err)
	}
	out.ByAge, _ = res.RowsAffected()

	// Keep the newest l.maxEvents events at or below the bound. The sequence at the
	// limit is kept, so everything below it goes.
	var threshold sql.NullInt64
	err = tx.QueryRowContext(ctx,
		`SELECT sequence FROM events WHERE sequence <= ? ORDER BY sequence DESC LIMIT 1 OFFSET ?`,
		bound, l.maxEvents-1).Scan(&threshold)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return PruneResult{}, fmt.Errorf("size threshold: %w", err)
	}
	if threshold.Valid {
		res, err := tx.ExecContext(ctx, `DELETE FROM events WHERE sequence < ?`, threshold.Int64)
		if err != nil {
			return PruneResult{}, fmt.Errorf("prune by count: %w", err)
		}
		out.ByCount, _ = res.RowsAffected()
	}

	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE sequence > ?`, bound).Scan(&out.Held); err != nil {
		return PruneResult{}, fmt.Errorf("count held: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return PruneResult{}, fmt.Errorf("commit prune: %w", err)
	}
	return out, nil
}

// DeleteWorkspace removes a workspace's runs and events.
//
// Subscriptions survive. A subscription belongs to a principal and outlives any one
// workspace of that principal.
func (l *Log) DeleteWorkspace(ctx context.Context, workspaceID string) error {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, stmt := range []string{
		`DELETE FROM events WHERE workspace_id = ?`,
		`DELETE FROM runs WHERE workspace_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, workspaceID); err != nil {
			return fmt.Errorf("delete workspace %s rows: %w", workspaceID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete of workspace %s: %w", workspaceID, err)
	}
	return nil
}

const selectEvents = `SELECT sequence, event_id, run_id, workspace_id, owner_id, kind,
                             previous_state, state, occurred_at, recorded_at, attributes
                      FROM events`

const selectSubscriptions = `SELECT id, principal_id, cursor FROM subscriptions`

// scanSubscription reads one subscription row. It maps sql.ErrNoRows to ErrNotFound, so a
// caller does not have to import database/sql to tell the two apart.
func scanSubscription(row *sql.Row) (Subscription, error) {
	var s Subscription
	err := row.Scan(&s.ID, &s.PrincipalID, &s.Cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return Subscription{}, ErrNotFound
	}
	if err != nil {
		return Subscription{}, fmt.Errorf("read subscription: %w", err)
	}
	return s, nil
}

func scanEvents(rows *sql.Rows) ([]Event, error) {
	var out []Event
	for rows.Next() {
		var (
			e             Event
			kind          string
			previousState string
			state         string
			occurredAt    int64
			recordedAt    int64
			attributes    string
		)
		if err := rows.Scan(&e.Sequence, &e.ID, &e.RunID, &e.WorkspaceID, &e.OwnerID, &kind,
			&previousState, &state, &occurredAt, &recordedAt, &attributes); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		e.Kind = Kind(kind)
		e.PreviousState = State(previousState)
		e.State = State(state)
		e.OccurredAt, e.RecordedAt = fromMillis(occurredAt), fromMillis(recordedAt)
		if err := json.Unmarshal([]byte(attributes), &e.Attributes); err != nil {
			return nil, fmt.Errorf("decode attributes of event %s: %w", e.ID, err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// encodeAttributes writes the payload, and it writes nothing for an empty map so that
// the column is never NULL and never a bare zero value.
func encodeAttributes(a map[string]any) (string, error) {
	if len(a) == 0 {
		return "{}", nil
	}
	encoded, err := json.Marshal(a)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidAttribute, err)
	}
	return string(encoded), nil
}

func millis(t time.Time) int64 { return t.UTC().UnixMilli() }

func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }
