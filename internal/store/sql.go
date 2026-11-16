package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/gobackto-work/pestilence/internal/workspace"
	"github.com/jackc/pgx/v5/pgconn"
)

const createTable = `
CREATE TABLE IF NOT EXISTS workspaces (
    id             TEXT PRIMARY KEY,
    slug           TEXT NOT NULL UNIQUE,
    owner_id       TEXT NOT NULL,
    namespace      TEXT NOT NULL,
    hostname       TEXT NOT NULL,
    state          TEXT NOT NULL,
    created_at     TEXT NOT NULL,
    last_active_at TEXT NOT NULL,
    deleted_at     TEXT,
    last_error     TEXT NOT NULL DEFAULT '',
    cpu            TEXT NOT NULL,
    memory         TEXT NOT NULL,
    storage        TEXT NOT NULL,
    max_pods       INTEGER NOT NULL
)`

const createIndex = `CREATE INDEX IF NOT EXISTS workspaces_state_idx ON workspaces(state)`

const columns = `id, slug, owner_id, namespace, hostname, state,
                  created_at, last_active_at, deleted_at, last_error,
                  cpu, memory, storage, max_pods`

// SQL is the PostgreSQL workspace store.
type SQL struct{ db *sql.DB }

// Close implements Store.
func (s *SQL) Close() error { return s.db.Close() }

// Create implements Store. It validates before inserting, so a malformed record is
// refused at the boundary rather than discovered mid-provision.
func (s *SQL) Create(ctx context.Context, w workspace.Workspace) error {
	if err := w.Validate(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO workspaces (`+columns+`)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		w.ID, w.Slug, w.OwnerID, w.Namespace, w.Hostname, string(w.State),
		formatTime(w.CreatedAt), formatTime(w.LastActiveAt), nullableTime(w.DeletedAt), w.LastError,
		w.Limits.CPU, w.Limits.Memory, w.Limits.Storage, w.Limits.MaxPods,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return fmt.Errorf("insert workspace %s: %w", w.ID, ErrDuplicate)
		}
		return fmt.Errorf("insert workspace %s: %w", w.ID, err)
	}
	return nil
}

// Get implements Store.
func (s *SQL) Get(ctx context.Context, id string) (workspace.Workspace, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM workspaces WHERE id = $1`, id)
	w, err := scanWorkspace(row)
	if errors.Is(err, sql.ErrNoRows) {
		return workspace.Workspace{}, ErrNotFound
	}
	if err != nil {
		return workspace.Workspace{}, fmt.Errorf("get workspace %s: %w", id, err)
	}
	return w, nil
}

// GetBySlug implements Store.
func (s *SQL) GetBySlug(ctx context.Context, slug string) (workspace.Workspace, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM workspaces WHERE slug = $1`, slug)
	w, err := scanWorkspace(row)
	if errors.Is(err, sql.ErrNoRows) {
		return workspace.Workspace{}, ErrNotFound
	}
	if err != nil {
		return workspace.Workspace{}, fmt.Errorf("get workspace by slug %s: %w", slug, err)
	}
	return w, nil
}

// List implements Store, ordered by creation so the output is stable.
func (s *SQL) List(ctx context.Context) ([]workspace.Workspace, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM workspaces ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []workspace.Workspace
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, fmt.Errorf("scan workspace: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// UpdateState implements Store as a compare-and-set: the row is only changed when
// its state is still `from`, which is what stops two reconcilers both acting on
// one transition.
func (s *SQL) UpdateState(ctx context.Context, id string, from, to workspace.State) error {
	if !from.CanTransitionTo(to) {
		return fmt.Errorf("workspace %s: illegal transition %s -> %s", id, from, to)
	}
	// deleted_at is stamped in the same statement, so a DELETED row cannot exist
	// without it and the record stays valid.
	var stamp any
	if to == workspace.StateDeleted {
		stamp = formatTime(time.Now().UTC())
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE workspaces SET state = $1, deleted_at = COALESCE($2, deleted_at)
         WHERE id = $3 AND state = $4`,
		string(to), stamp, id, string(from))
	if err != nil {
		return fmt.Errorf("update state for %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		// Either there is no such workspace, or it was not in `from`. Distinguish
		// them, because the caller's response differs: retry versus give up.
		if _, gerr := s.Get(ctx, id); errors.Is(gerr, ErrNotFound) {
			return ErrNotFound
		} else if gerr != nil {
			return gerr
		}
		return ErrConflict
	}
	return nil
}

// SetError implements Store.
//
// It is deliberately separate from UpdateState: a failure reason is diagnostic,
// and recording it must not change lifecycle state.
func (s *SQL) SetError(ctx context.Context, id, message string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE workspaces SET last_error = $1 WHERE id = $2`, message, id)
	if err != nil {
		return fmt.Errorf("set error for %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Touch implements Store.
func (s *SQL) Touch(ctx context.Context, id string, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE workspaces SET last_active_at = $1 WHERE id = $2`,
		formatTime(at), id)
	if err != nil {
		return fmt.Errorf("touch %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// row helpers
// ---------------------------------------------------------------------------

type rowScanner interface{ Scan(dest ...any) error }

func scanWorkspace(sc rowScanner) (workspace.Workspace, error) {
	var (
		w                        workspace.Workspace
		state, created, lastSeen string
		deleted                  sql.NullString
	)
	if err := sc.Scan(
		&w.ID, &w.Slug, &w.OwnerID, &w.Namespace, &w.Hostname, &state,
		&created, &lastSeen, &deleted, &w.LastError,
		&w.Limits.CPU, &w.Limits.Memory, &w.Limits.Storage, &w.Limits.MaxPods,
	); err != nil {
		return workspace.Workspace{}, err
	}
	w.State = workspace.State(state)

	var err error
	if w.CreatedAt, err = parseTime(created); err != nil {
		return workspace.Workspace{}, fmt.Errorf("created_at: %w", err)
	}
	if w.LastActiveAt, err = parseTime(lastSeen); err != nil {
		return workspace.Workspace{}, fmt.Errorf("last_active_at: %w", err)
	}
	if deleted.Valid {
		at, err := parseTime(deleted.String)
		if err != nil {
			return workspace.Workspace{}, fmt.Errorf("deleted_at: %w", err)
		}
		w.DeletedAt = &at
	}
	return w, nil
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}
