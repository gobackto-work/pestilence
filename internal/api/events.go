package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gobackto-work/pestilence/internal/eventlog"
	"github.com/gobackto-work/pestilence/internal/tenant"
	"github.com/gobackto-work/pestilence/internal/workspace"
)

// workspaceKey is the context key for the workspace a runtime is reporting for. A
// private struct rather than a string, so nothing outside this package can collide.
type workspaceKey struct{}

// workspaceFrom returns the workspace behind requireReporter. A handler behind that
// middleware can rely on the value being set.
func workspaceFrom(ctx context.Context) workspace.Workspace {
	ws, _ := ctx.Value(workspaceKey{}).(workspace.Workspace)
	return ws
}

// requireReporter authenticates a workspace's reporting token and hands the workspace to
// the handler.
//
// This is not requireOwner. town's assertion says which user is asking; this says which
// workspace's broker is reporting. The route names no workspace: the token does, and the
// workspace owns the key that proves the token.
func (s *Server) requireReporter(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearerToken(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}

		// The claim selects the KEY, and the key is what makes the claim true. The value
		// here is a lookup key and never an authorisation: it is read from bytes the
		// caller chose, and everything it says is checked again after the signature
		// verifies.
		slug, err := tenant.TokenWorkspace(raw)
		if err != nil {
			s.rejectReporter(w, slug, err)
			return
		}

		ws, err := s.store.GetBySlug(r.Context(), slug)
		if err != nil {
			// An unknown workspace answers exactly as a bad token does. A 404 would let an
			// unauthenticated caller probe which workspaces exist by making up a token,
			// and a slug is the workspace's public hostname.
			s.rejectReporter(w, slug, err)
			return
		}

		pub, err := s.cfg.TokenKeys.PublicKey(r.Context(), ws.Slug)
		if err != nil {
			// Logged and not returned. A failure here is ours, and the caller can do
			// nothing with the detail.
			s.log.Error("could not resolve the workspace token key", "slug", ws.Slug, "err", err)
			writeError(w, http.StatusServiceUnavailable, "unavailable",
				"the workspace token key is not readable")
			return
		}

		// The claim is compared against the workspace whose key was used, so a token
		// naming a workspace it was not minted for cannot pass, even if two workspaces
		// ever shared a key.
		if _, err := tenant.VerifyToken(raw, pub, tenant.TokenAudienceIngest,
			ws.Slug, ws.Namespace, tenant.TokenRoleBroker, time.Now()); err != nil {
			s.rejectReporter(w, ws.Slug, err)
			return
		}

		next(w, r.WithContext(context.WithValue(r.Context(), workspaceKey{}, ws)))
	}
}

// rejectReporter answers one way for every reason a reporting token is not accepted, so a
// caller that is guessing learns nothing from the difference.
func (s *Server) rejectReporter(w http.ResponseWriter, slug string, err error) {
	// The reason is logged but never returned: telling a caller which half of a forged
	// token to fix next is free help.
	s.log.Warn("reporting token rejected", "slug", slug, "err", err)
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, "unauthorized", "the token is not valid")
}

// appendEventRequest is the body of a report.
//
// It cannot name the workspace or the owner. Both come from the record, for the same
// reason the owner of a workspace comes from the assertion and from nothing else: a
// caller-supplied field would be spoofable.
type appendEventRequest struct {
	EventID    string         `json:"event_id"`
	RunID      string         `json:"run_id"`
	State      string         `json:"state"`
	Mode       string         `json:"mode"`
	OccurredAt string         `json:"occurred_at"`
	Attributes map[string]any `json:"attributes"`
}

// handleAppendEvent records the state that a workspace's runtime reported for one run.
//
// It answers with the sequence the record assigned. The runtime treats a report as
// successful only when it receives one, because a report the record refused must be
// retried and a report it accepted must not be.
func (s *Server) handleAppendEvent(w http.ResponseWriter, r *http.Request) {
	var req appendEventRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	occurredAt, err := time.Parse(time.RFC3339Nano, req.OccurredAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"occurred_at must be an RFC 3339 timestamp")
		return
	}

	ws := workspaceFrom(r.Context())
	result, err := s.cfg.Events.Append(r.Context(), eventlog.Report{
		ID:          req.EventID,
		RunID:       req.RunID,
		WorkspaceID: ws.ID,
		OwnerID:     ws.OwnerID,
		State:       eventlog.State(req.State),
		Mode:        eventlog.Mode(req.Mode),
		OccurredAt:  occurredAt,
		Attributes:  req.Attributes,
	})

	switch {
	case errors.Is(err, eventlog.ErrTransition):
		// The record holds a state this report cannot follow. The runtime asserts its
		// current state next, which is what makes a dropped report recoverable, so
		// this is a conflict and not a client error.
		writeError(w, http.StatusConflict, "conflict", "the run cannot move to that state")
	case errors.Is(err, eventlog.ErrModeChanged):
		// The mode is fixed by the first report, because it decides the state machine.
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, eventlog.ErrInvalid), errors.Is(err, eventlog.ErrInvalidAttribute):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case err != nil:
		s.log.Error("could not record an event", "slug", ws.Slug, "run", req.RunID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not record the event")
	default:
		writeJSON(w, http.StatusAccepted, map[string]any{
			"sequence": result.Sequence,
			"appended": result.Appended,
		})
	}
}
