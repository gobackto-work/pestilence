package api

import (
	"net/http"
	"time"
)

// eventBatchLimit bounds one read. A subscriber that is behind pages through the record, and
// a bounded page means a slow consumer cannot make the control plane hold a huge response.
const eventBatchLimit = 200

// eventPayload is one event as a subscriber sees it.
//
// It carries no owner: the caller IS the owner, because the assertion said so and the
// subscription takes one principal's events. It carries no run mode either, because the mode
// belongs to the run and not to the transition; a subscriber that needs it reads the run.
// What a notification needs is the kind, and that is here.
type eventPayload struct {
	Sequence      int64          `json:"sequence"`
	RunID         string         `json:"run_id"`
	WorkspaceID   string         `json:"workspace_id"`
	Kind          string         `json:"kind"`
	PreviousState string         `json:"previous_state"`
	State         string         `json:"state"`
	OccurredAt    string         `json:"occurred_at"`
	RecordedAt    string         `json:"recorded_at"`
	Attributes    map[string]any `json:"attributes,omitempty"`
}

// eventsResponse is a page of events and the cursor they were read from.
type eventsResponse struct {
	Events []eventPayload `json:"events"`

	// Cursor is the sequence the page was read from, and Sequence is the highest sequence in
	// the page. A subscriber acknowledges Sequence once it has processed the page, which is
	// what lets the record know an event has been seen.
	Cursor   int64 `json:"cursor"`
	Sequence int64 `json:"sequence"`
}

// handleEvents returns the events the owner has not taken.
//
// It reads the record and does NOT advance the cursor. A read that advanced would lose an
// event whenever the reader failed between reading and acting, and the contract says delivery
// is at-least-once: the subscriber acknowledges, and until it does the events are still
// there.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())

	sub, err := s.cfg.Events.EnsurePull(r.Context(), owner)
	if err != nil {
		s.log.Error("could not open the pull subscription", "owner", owner, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read events")
		return
	}
	_, events, err := s.cfg.Events.Outstanding(r.Context(), sub.ID, eventBatchLimit)
	if err != nil {
		s.log.Error("could not read events", "owner", owner, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read events")
		return
	}

	out := eventsResponse{Events: make([]eventPayload, 0, len(events)), Cursor: sub.Cursor}
	for _, e := range events {
		out.Events = append(out.Events, eventPayload{
			Sequence:      e.Sequence,
			RunID:         e.RunID,
			WorkspaceID:   e.WorkspaceID,
			Kind:          string(e.Kind),
			PreviousState: string(e.PreviousState),
			State:         string(e.State),
			OccurredAt:    e.OccurredAt.Format(time.RFC3339Nano),
			RecordedAt:    e.RecordedAt.Format(time.RFC3339Nano),
			Attributes:    e.Attributes,
		})
		if e.Sequence > out.Sequence {
			out.Sequence = e.Sequence
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ackEventsRequest acknowledges everything up to a sequence.
type ackEventsRequest struct {
	Sequence int64 `json:"sequence"`
}

// handleAckEvents advances the owner's cursor.
//
// The cursor only moves forward, because an acknowledgement for a sequence at or below it is
// stale and moving backwards would redeliver events the subscriber already acted on.
func (s *Server) handleAckEvents(w http.ResponseWriter, r *http.Request) {
	var req ackEventsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Sequence <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "sequence must be positive")
		return
	}

	owner := ownerFrom(r.Context())
	sub, err := s.cfg.Events.EnsurePull(r.Context(), owner)
	if err != nil {
		s.log.Error("could not open the pull subscription", "owner", owner, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not acknowledge")
		return
	}
	if err := s.cfg.Events.AdvanceCursor(r.Context(), sub.ID, req.Sequence); err != nil {
		s.log.Error("could not advance the cursor", "owner", owner, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not acknowledge")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// runBatchLimit bounds one read of the log.
const runBatchLimit = 50

// runPayload is one run as a reader sees it.
//
// It carries no owner, because the caller IS the owner: the assertion said so. It carries the
// workspace id and not its slug, because the record does not duplicate the registry. A reader
// that needs a name joins it with the workspace it already has.
type runPayload struct {
	ID           string `json:"id"`
	WorkspaceID  string `json:"workspace_id"`
	State        string `json:"state"`
	Mode         string `json:"mode"`
	StartedAt    string `json:"started_at"`
	UpdatedAt    string `json:"updated_at"`
	EndedAt      string `json:"ended_at,omitempty"`
	LastSequence int64  `json:"last_sequence"`
}

// handleRuns returns the owner's runs, most recently changed first.
//
// This is the log and not the queue. It takes no cursor and acknowledges nothing, so a reader
// can call it as often as it likes without consuming anything or holding the retention bound
// at zero.
func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.cfg.Events.Runs(r.Context(), ownerFrom(r.Context()), runBatchLimit)
	if err != nil {
		s.log.Error("could not read the run log", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read runs")
		return
	}

	out := make([]runPayload, 0, len(runs))
	for _, run := range runs {
		payload := runPayload{
			ID:           run.ID,
			WorkspaceID:  run.WorkspaceID,
			State:        string(run.State),
			Mode:         string(run.Mode),
			StartedAt:    run.StartedAt.Format(time.RFC3339Nano),
			UpdatedAt:    run.UpdatedAt.Format(time.RFC3339Nano),
			LastSequence: run.LastSequence,
		}
		if run.EndedAt != nil {
			payload.EndedAt = run.EndedAt.Format(time.RFC3339Nano)
		}
		out = append(out, payload)
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out})
}
