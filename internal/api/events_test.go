package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gobackto-work/pestilence/internal/eventlog"
	"github.com/gobackto-work/pestilence/internal/store"
	"github.com/gobackto-work/pestilence/internal/tenant"
	"github.com/gobackto-work/pestilence/internal/workspace"
	"github.com/oklog/ulid/v2"
)

// keyring is a TokenKeySource backed by one public key per slug.
type keyring map[string]ed25519.PublicKey

func (k keyring) PublicKey(_ context.Context, slug string) (ed25519.PublicKey, error) {
	pub, ok := k[slug]
	if !ok {
		return nil, errors.New("no key for " + slug)
	}
	return pub, nil
}

// ingestFixture is a server, two workspaces, and a reporting token for each.
//
// Two, because the interesting question is which workspace a report lands on. The route
// names none, so the token decides, and the answer must be the workspace the token was
// minted for.
type ingestFixture struct {
	srv    *Server
	store  store.Store
	log    *eventlog.Log
	ws     workspace.Workspace
	token  string
	other  workspace.Workspace
	tokenB string
}

func newIngestFixture(t *testing.T) ingestFixture {
	t.Helper()
	ctx := context.Background()

	st, err := store.OpenSQLite(t.TempDir() + "/w.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	log := newEventLog(t)
	keys := keyring{}

	create := func() (workspace.Workspace, string) {
		limits, err := tenant.LimitsFrom("", "", "", 0)
		if err != nil {
			t.Fatalf("limits: %v", err)
		}
		// newRecord is the constructor the create handler uses, so the fixture has a
		// workspace that passes validation and has a real slug and namespace.
		rec, err := (&Server{}).newRecord("github#583231", limits, time.Now().UTC())
		if err != nil {
			t.Fatalf("newRecord: %v", err)
		}
		rec.Hostname = rec.Slug + ".gobackto.work"
		if err := st.Create(ctx, rec); err != nil {
			t.Fatalf("create workspace: %v", err)
		}
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		keys[rec.Slug] = priv.Public().(ed25519.PublicKey)
		token, err := tenant.MintReportToken(tenant.Spec{Slug: rec.Slug}.Normalized(), priv, time.Now())
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		return rec, token
	}

	ws, token := create()
	other, tokenB := create()

	srv := New(st, Config{
		Verifier:       testVerifier(t),
		HostnameSuffix: "gobackto.work",
		Events:         log,
		TokenKeys:      keys,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	return ingestFixture{srv: srv, store: st, log: log, ws: ws, token: token, other: other, tokenB: tokenB}
}

// post sends one report. There is no workspace in the path: the token carries it.
func (f ingestFixture) post(t *testing.T, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/events", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	f.srv.ServeHTTP(w, req)
	return w
}

// recorded returns the events the owner has not taken, through the real read path.
func (f ingestFixture) recorded(t *testing.T) []eventlog.Event {
	t.Helper()
	ctx := context.Background()
	sub, err := f.log.EnsurePull(ctx, testOwner)
	if err != nil {
		t.Fatalf("open the read subscription: %v", err)
	}
	_, events, err := f.log.Outstanding(ctx, sub.ID, 100)
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}
	return events
}

func reportBody(eventID, runID, state string) string {
	return `{"event_id":"` + eventID + `","run_id":"` + runID + `","state":"` + state +
		`","mode":"interactive","occurred_at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}`
}

func TestAppendEventRecordsAReportedState(t *testing.T) {
	f := newIngestFixture(t)
	w := f.post(t, f.token, reportBody(ulid.Make().String(), ulid.Make().String(), "running"))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202. body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"appended":true`) {
		t.Errorf("body %s does not report that the event was appended", w.Body.String())
	}
	if events := f.recorded(t); len(events) != 1 || events[0].WorkspaceID != f.ws.ID {
		t.Errorf("the record holds %d events and the first names workspace %q, want 1 and %q",
			len(events), events[0].WorkspaceID, f.ws.ID)
	}
}

func TestAppendEventNeedsAToken(t *testing.T) {
	f := newIngestFixture(t)
	if w := f.post(t, "", reportBody(ulid.Make().String(), ulid.Make().String(), "running")); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: status %d, want 401", w.Code)
	}
}

// The route names no workspace, so the token decides which one a report lands on. This is
// the property that replaced a path check: there is nothing for a caller to get wrong.
func TestATokenReportsAsTheWorkspaceItWasMintedFor(t *testing.T) {
	f := newIngestFixture(t)
	w := f.post(t, f.tokenB, reportBody(ulid.Make().String(), ulid.Make().String(), "running"))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202. body: %s", w.Code, w.Body.String())
	}
	events := f.recorded(t)
	if len(events) != 1 {
		t.Fatalf("the record holds %d events, want 1", len(events))
	}
	if events[0].WorkspaceID != f.other.ID {
		t.Errorf("the report landed on workspace %q, want %q", events[0].WorkspaceID, f.other.ID)
	}
	if events[0].OwnerID != f.other.OwnerID {
		t.Errorf("the report carries owner %q, want %q", events[0].OwnerID, f.other.OwnerID)
	}
}

// An unknown workspace answers exactly as a bad token does. A 404 would let an
// unauthenticated caller probe which workspaces exist, using a token it made up.
func TestAnUnknownWorkspaceAnswersLikeABadToken(t *testing.T) {
	f := newIngestFixture(t)

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	absent := tenant.Spec{Slug: "eager-crane-ngun"}.Normalized()
	token, err := tenant.MintReportToken(absent, priv, time.Now())
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	w := f.post(t, token, reportBody(ulid.Make().String(), ulid.Make().String(), "running"))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401. A different answer here discloses which workspaces exist", w.Code)
	}
	if !strings.Contains(w.Body.String(), "the token is not valid") {
		t.Errorf("body %q distinguishes this case from a bad token", w.Body.String())
	}
}

func TestAMadeUpTokenIsRefused(t *testing.T) {
	f := newIngestFixture(t)

	for name, token := range map[string]string{
		"not a token":      "not.a.token",
		"empty parts":      "a.b.",
		"a bare workspace": "amber-shrew-uucs",
	} {
		t.Run(name, func(t *testing.T) {
			if w := f.post(t, token, reportBody(ulid.Make().String(), ulid.Make().String(), "running")); w.Code != http.StatusUnauthorized {
				t.Errorf("status %d, want 401", w.Code)
			}
		})
	}
}

// A token signed by a key the control plane does not know for that workspace is refused,
// which is what makes the unverified claim safe to read.
func TestATokenSignedByAnotherKeyIsRefused(t *testing.T) {
	f := newIngestFixture(t)

	// A fresh keypair, but the token still claims a workspace the store knows, so the
	// claim resolution succeeds and the signature is what fails.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	token, err := tenant.MintReportToken(tenant.Spec{Slug: f.ws.Slug}.Normalized(), priv, time.Now())
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	w := f.post(t, token, reportBody(ulid.Make().String(), ulid.Make().String(), "running"))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", w.Code)
	}
	if events := f.recorded(t); len(events) != 0 {
		t.Errorf("a forged token recorded %d events", len(events))
	}
}

// A retry of an accepted report must read as success, because the broker only stops
// retrying when it receives a sequence.
func TestAppendEventIsIdempotentOnTheEventID(t *testing.T) {
	f := newIngestFixture(t)
	eventID, runID := ulid.Make().String(), ulid.Make().String()
	body := reportBody(eventID, runID, "running")

	first := f.post(t, f.token, body)
	second := f.post(t, f.token, body)

	if first.Code != http.StatusAccepted || second.Code != http.StatusAccepted {
		t.Fatalf("statuses %d and %d, want 202", first.Code, second.Code)
	}
	if !strings.Contains(second.Body.String(), `"appended":false`) {
		t.Errorf("the retry appended a second event: %s", second.Body.String())
	}
	if events := f.recorded(t); len(events) != 1 {
		t.Errorf("the record holds %d events, want 1", len(events))
	}
}

func TestAppendEventRefusesAnUnreachableState(t *testing.T) {
	f := newIngestFixture(t)
	runID := ulid.Make().String()

	if w := f.post(t, f.token, reportBody(ulid.Make().String(), runID, "running")); w.Code != http.StatusAccepted {
		t.Fatalf("first report: status %d, want 202", w.Code)
	}
	if w := f.post(t, f.token, reportBody(ulid.Make().String(), runID, "succeeded")); w.Code != http.StatusAccepted {
		t.Fatalf("terminal report: status %d, want 202", w.Code)
	}
	if w := f.post(t, f.token, reportBody(ulid.Make().String(), runID, "running")); w.Code != http.StatusConflict {
		t.Errorf("a report after a terminal state: status %d, want 409", w.Code)
	}
}

// A batch run has nobody to answer it, so it cannot wait for a person.
func TestABatchRunCannotWait(t *testing.T) {
	f := newIngestFixture(t)
	runID := ulid.Make().String()
	batch := func(state string) string {
		return `{"event_id":"` + ulid.Make().String() + `","run_id":"` + runID +
			`","state":"` + state + `","mode":"batch","occurred_at":"` +
			time.Now().UTC().Format(time.RFC3339Nano) + `"}`
	}

	if w := f.post(t, f.token, batch("running")); w.Code != http.StatusAccepted {
		t.Fatalf("a batch run cannot start: status %d", w.Code)
	}
	if w := f.post(t, f.token, batch("waiting")); w.Code != http.StatusConflict {
		t.Errorf("a batch run reached waiting: status %d, want 409", w.Code)
	}
}

// The owner comes from the record and not from the body, so a runtime cannot attribute its
// run to another user. An unknown field is refused rather than ignored.
func TestAppendEventCannotChooseTheOwner(t *testing.T) {
	f := newIngestFixture(t)
	body := `{"event_id":"` + ulid.Make().String() + `","run_id":"` + ulid.Make().String() +
		`","state":"running","mode":"interactive","owner_id":"someone-else",` +
		`"occurred_at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}`

	if w := f.post(t, f.token, body); w.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400. body: %s", w.Code, w.Body.String())
	}
}

func TestAppendEventRefusesAnUnknownAttribute(t *testing.T) {
	f := newIngestFixture(t)
	body := `{"event_id":"` + ulid.Make().String() + `","run_id":"` + ulid.Make().String() +
		`","state":"running","mode":"interactive","attributes":{"prompt":"summarise my repository"},` +
		`"occurred_at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}`

	if w := f.post(t, f.token, body); w.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400. body: %s", w.Code, w.Body.String())
	}
}
