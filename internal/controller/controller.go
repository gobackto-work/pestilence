// Package controller drives workspaces towards their desired state.
//
// The API only records intent; nothing is provisioned on the request path. That
// is what makes provisioning retryable: a crash mid-provision leaves a workspace
// in PROVISIONING, and the next pass picks it up rather than leaving a half-built
// workspace behind (design §32).
//
// Every pass re-derives what to do from the STORED record, never from a live
// request, so a reconcile after a restart reproduces exactly what was asked for.
package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/gobackto-work/pestilence/internal/store"
	"github.com/gobackto-work/pestilence/internal/tenant"
	"github.com/gobackto-work/pestilence/internal/workspace"
)

// Provisioner is the part of the reconciler the controller drives. It is an
// interface so the loop can be tested without a cluster.
type Provisioner interface {
	Ensure(ctx context.Context, spec tenant.Spec) error
	Delete(ctx context.Context, spec tenant.Spec) error

	// WaitForDeletion blocks until the workspace's objects are actually gone.
	//
	// Without it, DELETED would mean "deletion requested" rather than complete,
	// and a namespace that fails to terminate would leave the record claiming
	// success while the residue sat there with nothing left to retry it.
	WaitForDeletion(ctx context.Context, spec tenant.Spec, timeout time.Duration) error
}

// EventForgetter forgets a workspace's run events. *eventlog.Log satisfies it.
//
// An interface so that the controller does not depend on where the record is stored, and so
// that a test can make the delete fail.
type EventForgetter interface {
	DeleteWorkspace(ctx context.Context, workspaceID string) error
}

// Config is what the controller needs to rebuild a provisioning spec from a
// stored record. Images are platform configuration, not tenant input, so they
// live here rather than in the database.
type Config struct {
	BrokerImage string
	AgentImage  string
	HTTPPort    int32

	// DeletionTimeout bounds how long one pass waits for a deletion to actually
	// finish. On expiry the workspace stays in DELETING and retries next pass,
	// rather than being recorded as complete while residue remains.
	DeletionTimeout time.Duration

	// DriftInterval is how often steady workspaces are re-applied. Drift correction
	// is eventual, not immediate, so this is much slower than the intent loop.
	DriftInterval time.Duration

	// Events forgets a workspace's run events once its namespace is gone.
	//
	// Optional, because a test does not need one. A nil value forgets nothing, which is what
	// the behaviour was before this existed: the events outlived the workspace that owned
	// them, and that is tenant data.
	Events EventForgetter

	// TokenTTL bounds a workspace's capability token, and therefore also sets the
	// rotation cadence: a token is re-minted once less than half its life remains,
	// on the drift sweep. Zero takes tenant.DefaultTokenTTL.
	//
	// It is configuration rather than a constant because it is a tradeoff: a shorter
	// TTL bounds how long a leaked token is useful, and a longer one tolerates a
	// control plane that has been down. A TTL shorter than half the plausible outage
	// window breaks running workspaces, which is the failure this knob exists to
	// avoid.
	TokenTTL time.Duration

	// AssertionPublicKeyPEM is town's PUBLIC assertion key, published into every tenant
	// namespace so each workspace's bridge can verify town's identity headers for
	// itself. A copy, not the original -- the original stays in the control plane's
	// namespace -- because a ConfigMap cannot be mounted across namespaces.
	//
	// Configuration rather than a path the controller reads: the control plane loads
	// the key once at startup, and the controller has no business owning a file path.
	//
	// Empty omits the ConfigMap and the mount, so a bundle still renders without one.
	AssertionPublicKeyPEM string

	// BrokerTLS switches the broker hop from plaintext to TLS for NEW workspaces.
	//
	// OFF by default, and it cannot be turned on from this repository alone: scarab must
	// serve TLS and verify the CA before the scheme changes, or every broker call fails.
	// It also only reaches new workspaces -- the root agent Deployment is ClassRuntime,
	// so an existing one keeps the environment it started with, which means enabling
	// this requires RECREATING workspaces rather than restarting them. Both reasons are
	// why this is a switch rather than simply the default.
	BrokerTLS bool
}

// Controller reconciles workspaces.
type Controller struct {
	store           store.Store
	prov            Provisioner
	cfg             Config
	interval        time.Duration
	driftInterval   time.Duration
	deletionTimeout time.Duration
	log             *slog.Logger
}

// New returns a Controller. A zero interval takes 10s, a zero drift interval takes
// 5 minutes, a zero deletion timeout takes 5 minutes, and a nil logger takes the
// default.
func New(s store.Store, p Provisioner, cfg Config, interval time.Duration, log *slog.Logger) *Controller {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	if cfg.HTTPPort == 0 {
		cfg.HTTPPort = tenant.DefaultHTTPPort
	}
	if cfg.DeletionTimeout <= 0 {
		cfg.DeletionTimeout = 5 * time.Minute
	}
	if cfg.DriftInterval <= 0 {
		cfg.DriftInterval = 5 * time.Minute
	}
	return &Controller{
		store:           s,
		prov:            p,
		cfg:             cfg,
		interval:        interval,
		driftInterval:   cfg.DriftInterval,
		deletionTimeout: cfg.DeletionTimeout,
		log:             log,
	}
}

// Run reconciles until ctx is cancelled.
//
// Two cadences, because they answer different questions. The fast loop acts on
// INTENT: a workspace that should be provisioning or deleting, right now. The slow
// sweep corrects DRIFT: a live workspace whose objects have fallen behind the
// desired state.
//
// The first intent pass runs immediately rather than after the interval, so a
// restart reconciles instead of waiting -- which is the whole point of not trusting
// that a previous operation completed.
func (c *Controller) Run(ctx context.Context) error {
	fast := time.NewTicker(c.interval)
	defer fast.Stop()
	drift := time.NewTicker(c.driftInterval)
	defer drift.Stop()

	c.pass(ctx)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-fast.C:
			c.pass(ctx)
		case <-drift.C:
			if err := c.DriftSweep(ctx); err != nil && !errors.Is(err, context.Canceled) {
				c.log.Error("drift sweep failed", "err", err)
			}
		}
	}
}

func (c *Controller) pass(ctx context.Context) {
	if err := c.ReconcileOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
		c.log.Error("reconcile pass failed", "err", err)
	}
}

// ReconcileOnce performs one pass over every workspace. Exported so the startup
// reconcile and the tests can run it directly.
func (c *Controller) ReconcileOnce(ctx context.Context) error {
	all, err := c.store.List(ctx)
	if err != nil {
		return fmt.Errorf("list workspaces: %w", err)
	}

	var errs []error
	for _, w := range all {
		switch w.State {
		case workspace.StateProvisioning:
			if err := c.provision(ctx, w); err != nil {
				errs = append(errs, err)
			}
		case workspace.StateDeleting:
			if err := c.destroy(ctx, w); err != nil {
				errs = append(errs, err)
			}
			// Every other state is either steady (RUNNING, SUSPENDED), terminal
			// (DELETED), or waiting on an explicit retry (FAILED). Intent is not this
			// loop's business for any of them; steady states are DriftSweep's.
		}
	}
	return errors.Join(errs...)
}

// DriftSweep re-applies the reconciled classes to live workspaces.
//
// Without it nothing is ever re-applied once a workspace is running, which
// contradicts the contract's "continuously reconciled; drift is corrected" and has
// a sharp operational cost: a fix to any reconciled object -- a pod-spec change, a
// policy change -- does not reach running workspaces, and the only remedy is to
// recreate them. That bit a live test directly: correcting the broker's mount did
// not repair the running workspace.
//
// It runs on a slower cadence than the intent loop because drift correction is
// eventual, not immediate, and re-applying every workspace every few seconds would
// be pure API traffic.
//
// A failure here does NOT change lifecycle state. The workspace is running and
// mostly fine, so moving it to FAILED would be wrong and would strand it; the error
// is recorded instead so the UI can show that something is behind.
//
// SUSPENDED is deliberately not swept: Ensure applies the runtime as well as the
// boundary, and a suspended workspace should have no runtime. It joins this list
// when suspend is implemented and can reconcile its boundary alone.
func (c *Controller) DriftSweep(ctx context.Context) error {
	all, err := c.store.List(ctx)
	if err != nil {
		return fmt.Errorf("list workspaces: %w", err)
	}
	c.log.Debug("drift sweep", "candidates", len(all))

	var errs []error
	for _, w := range all {
		if w.State != workspace.StateRunning {
			continue
		}
		spec, err := c.specFor(w)
		if err != nil {
			errs = append(errs, c.recordDrift(ctx, w, fmt.Errorf("build spec: %w", err)))
			continue
		}
		if err := c.prov.Ensure(ctx, spec); err != nil {
			errs = append(errs, c.recordDrift(ctx, w, err))
			continue
		}
		// Clear a previously recorded drift error, but only when there is one: a
		// write on every sweep for every workspace would be pointless traffic.
		if w.LastError != "" {
			if err := c.store.SetError(ctx, w.ID, ""); err != nil && !errors.Is(err, store.ErrNotFound) {
				errs = append(errs, fmt.Errorf("clear drift error for %s: %w", w.ID, err))
			}
		}
	}
	return errors.Join(errs...)
}

// recordDrift surfaces a failed re-apply without changing state.
func (c *Controller) recordDrift(ctx context.Context, w workspace.Workspace, cause error) error {
	c.log.Error("drift correction failed", "id", w.ID, "slug", w.Slug, "err", cause)
	if err := c.store.SetError(ctx, w.ID, cause.Error()); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("record drift for %s: %w", w.ID, err)
	}
	return nil
}

func (c *Controller) provision(ctx context.Context, w workspace.Workspace) error {
	spec, err := c.specFor(w)
	if err != nil {
		return c.markFailed(ctx, w, fmt.Errorf("build spec: %w", err))
	}
	if err := c.prov.Ensure(ctx, spec); err != nil {
		return c.markFailed(ctx, w, err)
	}

	if err := c.store.SetError(ctx, w.ID, ""); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("clear error for %s: %w", w.ID, err)
	}
	if err := c.store.UpdateState(ctx, w.ID, workspace.StateProvisioning, workspace.StateRunning); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil // something else moved it; not this pass's business
		}
		return fmt.Errorf("mark %s running: %w", w.ID, err)
	}
	c.log.Info("workspace running", "id", w.ID, "slug", w.Slug)
	return nil
}

func (c *Controller) destroy(ctx context.Context, w workspace.Workspace) error {
	spec, err := c.specFor(w)
	if err != nil {
		return c.retryDeletion(ctx, w, fmt.Errorf("build spec: %w", err))
	}
	if err := c.prov.Delete(ctx, spec); err != nil {
		return c.retryDeletion(ctx, w, err)
	}
	// Wait for the deletion to actually finish before claiming success. Marking
	// DELETED as soon as the deletes are issued would mean "requested", not
	// "complete" -- and a namespace that fails to terminate would leave the record
	// claiming success while the residue sat there with nothing to retry it.
	if err := c.prov.WaitForDeletion(ctx, spec, c.deletionTimeout); err != nil {
		return c.retryDeletion(ctx, w, fmt.Errorf("waiting for deletion: %w", err))
	}
	// The workspace's events go BEFORE it is recorded as deleted.
	//
	// The other order records a workspace as gone with its events still there, and takes it
	// out of the reconcile loop, so nothing ever comes back for them. This order leaves it in
	// DELETING and retries, which is the discipline WaitForDeletion applies just above. The
	// delete is idempotent, so a retry after a partial failure is harmless.
	if c.cfg.Events != nil {
		if err := c.cfg.Events.DeleteWorkspace(ctx, w.ID); err != nil {
			return c.retryDeletion(ctx, w, fmt.Errorf("forgetting the workspace's events: %w", err))
		}
	}
	if err := c.store.UpdateState(ctx, w.ID, workspace.StateDeleting, workspace.StateDeleted); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil
		}
		return fmt.Errorf("mark %s deleted: %w", w.ID, err)
	}
	c.log.Info("workspace deleted", "id", w.ID, "slug", w.Slug)
	return nil
}

// markFailed records why, then moves to FAILED. FAILED deliberately re-enters
// PROVISIONING, so a transient error is retryable rather than a brick.
func (c *Controller) markFailed(ctx context.Context, w workspace.Workspace, cause error) error {
	c.log.Error("provision failed", "id", w.ID, "slug", w.Slug, "err", cause)
	if err := c.store.SetError(ctx, w.ID, cause.Error()); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("record failure for %s: %w", w.ID, err)
	}
	if err := c.store.UpdateState(ctx, w.ID, workspace.StateProvisioning, workspace.StateFailed); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil
		}
		return fmt.Errorf("mark %s failed: %w", w.ID, err)
	}
	return nil
}

// retryDeletion stays in DELETING. There is no other state to move to that would
// help: deletion is the only transition available, and the next pass retries it.
func (c *Controller) retryDeletion(ctx context.Context, w workspace.Workspace, cause error) error {
	c.log.Error("deletion failed, will retry", "id", w.ID, "slug", w.Slug, "err", cause)
	if err := c.store.SetError(ctx, w.ID, cause.Error()); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("record failure for %s: %w", w.ID, err)
	}
	return nil
}

// specFor rebuilds the provisioning spec from the stored record.
//
// The record is authoritative: slug, hostname and limits come from it rather than
// from a live request, which is what lets a reconcile reproduce exactly what was
// asked for even after a restart.
func (c *Controller) specFor(w workspace.Workspace) (tenant.Spec, error) {
	limits, err := tenant.LimitsFrom(w.Limits.CPU, w.Limits.Memory, w.Limits.Storage, w.Limits.MaxPods)
	if err != nil {
		return tenant.Spec{}, err
	}
	return tenant.Spec{
		Slug:                  w.Slug,
		OwnerID:               w.OwnerID,
		Hostname:              w.Hostname,
		HTTPPort:              c.cfg.HTTPPort,
		BrokerImage:           c.cfg.BrokerImage,
		AgentImage:            c.cfg.AgentImage,
		Limits:                limits,
		TokenTTL:              c.cfg.TokenTTL,
		BrokerTLS:             c.cfg.BrokerTLS,
		AssertionPublicKeyPEM: c.cfg.AssertionPublicKeyPEM,
	}, nil
}
