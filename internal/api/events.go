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

// requireReporter authenticates a workspace capability token and hands the workspace to
// the handler.
//
// This is not requireOwner. town's assertion says which user is asking; this says which
// workspace's runtime is reporting. The route names the workspace, the token is checked
// against that workspace's own key, and the token's workspace claim must match, so a
// token minted for one workspace cannot report for another.
//
// The token belongs to the root agent. The agent is the party that can tell a run that
// has finished from a run that is waiting for a person, and the broker only forwards
// what the agent says.
func (s *Server) requireReporter(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ws, err := s.store.Get(r.Context(), r.PathValue("id"))
		if err != nil {
			// A read failure and a missing row answer the same way. Distinguishing
			// them would let an unauthenticated caller map which workspace ids exist.
			writeError(w, http.StatusNotFound, "not_found", "no such workspace")
			return
		}

		raw, ok := bearerToken(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
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

		if _, err := tenant.VerifyToken(raw, pub, tenant.TokenAudienceIngest,
			ws.Slug, ws.Namespace, time.Now()); err != nil {
			// The reason is logged but never returned: telling a caller which half of
			// a forged token to fix next is free help.
			s.log.Warn("capability token rejected", "slug", ws.Slug, "err", err)
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "unauthorized", "capability token is not valid")
			return
		}

		next(w, r.WithContext(context.WithValue(r.Context(), workspaceKey{}, ws)))
	}
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
		OccurredAt:  occurredAt,
		Attributes:  req.Attributes,
	})

	switch {
	case errors.Is(err, eventlog.ErrTransition):
		// The record holds a state this report cannot follow. The runtime asserts its
		// current state next, which is what makes a dropped report recoverable, so
		// this is a conflict and not a client error.
		writeError(w, http.StatusConflict, "conflict", "the run cannot move to that state")
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
