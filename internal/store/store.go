// Package store persists the platform's own objects.
//
// The database is authoritative for platform objects; Kubernetes is authoritative
// for current runtime state. Nothing here reads the cluster.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/gobackto-work/pestilence/internal/workspace"
)

var (
	// ErrNotFound means there is no such workspace.
	ErrNotFound = errors.New("workspace not found")

	// ErrConflict means the workspace was not in the state the caller expected,
	// so something else moved it first.
	ErrConflict = errors.New("workspace state changed")
)

// Store is the durable record of workspaces.
type Store interface {
	// Create inserts a new workspace, refusing one that fails validation.
	Create(ctx context.Context, w workspace.Workspace) error

	Get(ctx context.Context, id string) (workspace.Workspace, error)
	GetBySlug(ctx context.Context, slug string) (workspace.Workspace, error)
	List(ctx context.Context) ([]workspace.Workspace, error)

	// UpdateState applies a transition only when the stored state is still `from`.
	//
	// This compare-and-set is why two reconcilers cannot both act on the same
	// transition: the loser gets ErrConflict instead of doing the work twice.
	UpdateState(ctx context.Context, id string, from, to workspace.State) error

	// Touch records activity, for idle suspension later.
	Touch(ctx context.Context, id string, at time.Time) error

	// SetError records why a workspace is not healthy, for the UI. It is separate
	// from UpdateState because a failure reason is diagnostic: recording it must
	// not change lifecycle state.
	SetError(ctx context.Context, id, message string) error

	Close() error
}
