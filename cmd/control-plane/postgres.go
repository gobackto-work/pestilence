package main

import (
	"errors"
	"fmt"

	"github.com/gobackto-work/pestilence/internal/eventlog"
	"github.com/gobackto-work/pestilence/internal/store"
)

func openPostgresStores(dsn string) (*store.SQL, *eventlog.Log, func(), error) {
	if dsn == "" {
		return nil, nil, nil, errors.New("PESTILENCE_POSTGRES_URL is required for the postgres backend")
	}
	workspaces, err := store.OpenPostgres(dsn)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open workspace store: %w", err)
	}
	events, err := eventlog.OpenPostgres(dsn)
	if err != nil {
		_ = workspaces.Close()
		return nil, nil, nil, fmt.Errorf("open event record: %w", err)
	}
	return workspaces, events, func() { _ = events.Close(); _ = workspaces.Close() }, nil
}
