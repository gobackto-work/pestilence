package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gobackto-work/pestilence/internal/eventlog"
	"github.com/oklog/ulid/v2"
)

// ingestOne appends one event for an owner, the way a broker would.
func ingestOne(t *testing.T, log *eventlog.Log, owner string) eventlog.Event {
	t.Helper()
	workspace := ulid.Make().String()
	report := eventlog.Report{
		ID: ulid.Make().String(), RunID: ulid.Make().String(), WorkspaceID: workspace,
		OwnerID: owner, State: eventlog.StateRunning, Mode: eventlog.ModeInteractive,
		OccurredAt: time.Now().UTC(),
	}
	if _, err := log.Append(context.Background(), report); err != nil {
		t.Fatalf("append: %v", err)
	}
	events, err := log.EventsAfter(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	return events[len(events)-1]
}

func readEvents(t *testing.T, srv *Server) eventsResponse {
	t.Helper()
	w := do(t, srv, http.MethodGet, "/api/events", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200. body: %s", w.Code, w.Body.String())
	}
	var out eventsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func TestEventsAreReadByTheirOwner(t *testing.T) {
	srv, _ := newServer(t)
	log := srv.cfg.Events.(*eventlog.Log)

	want := ingestOne(t, log, testOwner)
	// Another owner's event, which this caller must never see.
	ingestOne(t, log, "github#999999")

	got := readEvents(t, srv)
	if len(got.Events) != 1 {
		t.Fatalf("read %d events, want 1, so a subscriber saw another principal's run", len(got.Events))
	}
	if got.Events[0].RunID != want.RunID {
		t.Errorf("run id = %q, want %q", got.Events[0].RunID, want.RunID)
	}
	if got.Events[0].Kind != string(eventlog.KindRunStarted) {
		t.Errorf("kind = %q, want %q", got.Events[0].Kind, eventlog.KindRunStarted)
	}
	if got.Sequence != want.Sequence {
		t.Errorf("sequence = %d, want %d", got.Sequence, want.Sequence)
	}
	if got.Cursor != 0 {
		t.Errorf("cursor = %d, want 0: a read does not acknowledge", got.Cursor)
	}
}

// Reading does not advance the cursor, so an event is never lost by a reader that fails
// between reading and acting. Only an acknowledgement moves it.
func TestReadingTwiceReturnsTheSameEvents(t *testing.T) {
	srv, _ := newServer(t)
	log := srv.cfg.Events.(*eventlog.Log)
	ingestOne(t, log, testOwner)

	if first, second := readEvents(t, srv), readEvents(t, srv); len(first.Events) != 1 || len(second.Events) != 1 {
		t.Fatalf("read %d then %d events, want 1 both times", len(first.Events), len(second.Events))
	}
}

func TestAcknowledgingAdvancesTheCursor(t *testing.T) {
	srv, _ := newServer(t)
	log := srv.cfg.Events.(*eventlog.Log)
	event := ingestOne(t, log, testOwner)

	body := `{"sequence":` + itoa(event.Sequence) + `}`
	if w := do(t, srv, http.MethodPost, "/api/events/ack", body); w.Code != http.StatusNoContent {
		t.Fatalf("ack: status %d, want 204. body: %s", w.Code, w.Body.String())
	}

	got := readEvents(t, srv)
	if len(got.Events) != 0 {
		t.Errorf("read %d events after acknowledging, want 0", len(got.Events))
	}
	if got.Cursor != event.Sequence {
		t.Errorf("cursor = %d, want %d", got.Cursor, event.Sequence)
	}
}

// An acknowledgement at or below the cursor is stale, and moving backwards would redeliver
// events the subscriber already acted on.
func TestTheCursorNeverMovesBackwards(t *testing.T) {
	srv, _ := newServer(t)
	log := srv.cfg.Events.(*eventlog.Log)

	first := ingestOne(t, log, testOwner)
	second := ingestOne(t, log, testOwner)

	if w := do(t, srv, http.MethodPost, "/api/events/ack", `{"sequence":`+itoa(second.Sequence)+`}`); w.Code != http.StatusNoContent {
		t.Fatalf("first ack: status %d", w.Code)
	}
	if w := do(t, srv, http.MethodPost, "/api/events/ack", `{"sequence":`+itoa(first.Sequence)+`}`); w.Code != http.StatusNoContent {
		t.Fatalf("stale ack: status %d, want 204 and no effect", w.Code)
	}

	if got := readEvents(t, srv); got.Cursor != second.Sequence {
		t.Errorf("cursor = %d, want %d", got.Cursor, second.Sequence)
	}
}

func TestAcknowledgingNeedsAPositiveSequence(t *testing.T) {
	srv, _ := newServer(t)
	for name, body := range map[string]string{
		"zero":     `{"sequence":0}`,
		"negative": `{"sequence":-1}`,
		"absent":   `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			if w := do(t, srv, http.MethodPost, "/api/events/ack", body); w.Code != http.StatusBadRequest {
				t.Errorf("status %d, want 400", w.Code)
			}
		})
	}
}

func TestEventsNeedAnAssertion(t *testing.T) {
	srv, _ := newServer(t)

	if w := doAs(t, srv, "", http.MethodGet, "/api/events", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("read without an assertion: status %d, want 401", w.Code)
	}
	if w := doAs(t, srv, "", http.MethodPost, "/api/events/ack", `{"sequence":1}`); w.Code != http.StatusUnauthorized {
		t.Errorf("ack without an assertion: status %d, want 401", w.Code)
	}
}

// itoa keeps the test bodies readable without pulling strconv in for one call.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
