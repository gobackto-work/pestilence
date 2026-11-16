package eventlog

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/gobackto-work/pestilence/internal/sqlbackend"
)

// OpenPostgres opens the run record in a dedicated PostgreSQL database.
func OpenPostgres(dsn string) (*Log, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres event record: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect postgres event record: %w", err)
	}
	for _, stmt := range schema {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("apply postgres event schema: %w", err)
		}
	}
	return &Log{db: sqlbackend.Wrap(db), maxEvents: retentionCount, maxAge: retentionAge}, nil
}

// beginWrite serialises PostgreSQL event writers until commit. Sequence allocation alone
// does not order commits, and a cursor must not pass an event that is still uncommitted.
func (l *Log) beginWrite(ctx context.Context) (*sqlbackend.Tx, error) {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(726455818)"); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("lock event writer: %w", err)
	}
	return tx, nil
}
