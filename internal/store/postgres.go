package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/gobackto-work/pestilence/internal/sqlbackend"
)

// OpenPostgres opens a dedicated PostgreSQL database for the workspace store.
func OpenPostgres(dsn string) (*SQL, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	for _, stmt := range []string{createTable, createIndex} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("apply postgres workspace schema: %w", err)
		}
	}
	return &SQL{db: sqlbackend.Wrap(db)}, nil
}
