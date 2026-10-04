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

// ingestFixture is a server, a workspace in the store, and a valid capability token for
// that workspace, plus a second workspace's token for the cross-workspace test.
type ingestFixture struct {
	srv     *Server
	store   store.Store
	ws      workspace.Workspace
	token   string
	other   workspace.Workspace
	otherID string
}

func newIngestFixture(t *testing.T) ingestFixture {
	t.Helper()
	ctx := context.Background()

	st, err := store.OpenSQLite(t.TempDir() + "/w.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	keys := keyring{}
	create := func() (workspace.Workspace, string) {
		limits, err := tenant.LimitsFrom("", "", "", 0)
		if err != nil {
			t.Fatalf("limits: %v", err)
		}
		// newRecord is the same constructor the create handler uses, so the fixture has
		// a workspace that passes validation and has a real slug and namespace.
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
		pub := priv.Public().(ed25519.PublicKey)
		keys[rec.Slug] = pub
		token, err := tenant.MintReportToken(tenant.Spec{Slug: rec.Slug}.Normalized(), priv, time.Now())
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		return rec, token
	}

	ws, token := create()
	other, otherToken := create()

	srv := New(st, Config{
		Verifier:       testVerifier(t),
		HostnameSuffix: "gobackto.work",
		Events:         newEventLog(t),
		TokenKeys:      keys,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	return ingestFixture{srv: srv, store: st, ws: ws, token: token, other: other, otherID: otherToken}
}

// post sends one report for a workspace.
func (f ingestFixture) post(t *testing.T, workspaceID, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+workspaceID+"/events", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	f.srv.ServeHTTP(w, req)
	return w
}

// reportBody builds a minimal valid report.
func reportBody(eventID, runID, state string) string {
	return `{"event_id":"` + eventID + `","run_id":"` + runID + `","state":"` + state +
		`","mode":"interactive","occurred_at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}`
}

func TestAppendEventRecordsAReportedState(t *testing.T) {
	f := newIngestFixture(t)
	w := f.post(t, f.ws.ID, f.token, reportBody(ulid.Make().String(), ulid.Make().String(), "running"))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202. body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"appended":true`) {
		t.Errorf("body %s does not report that the event was appended", w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"sequence":0`) {
		t.Errorf("body %s reports sequence 0", w.Body.String())
	}
}

func TestAppendEventNeedsACapabilityToken(t *testing.T) {
	f := newIngestFixture(t)

	if w := f.post(t, f.ws.ID, "", reportBody(ulid.Make().String(), ulid.Make().String(), "running")); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: status %d, want 401", w.Code)
	}
}

// A token minted for one workspace must not report for another, which is what the
// workspace claim and the per-workspace key are for.
func TestAppendEventRefusesAnotherWorkspacesToken(t *testing.T) {
	f := newIngestFixture(t)
	w := f.post(t, f.ws.ID, f.otherID, reportBody(ulid.Make().String(), ulid.Make().String(), "running"))

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", w.Code)
	}
}

func TestAppendEventForAnUnknownWorkspaceIsNotFound(t *testing.T) {
	f := newIngestFixture(t)
	w := f.post(t, ulid.Make().String(), f.token, reportBody(ulid.Make().String(), ulid.Make().String(), "running"))

	if w.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", w.Code)
	}
}

// A retry of an accepted report must read as success, because the runtime only stops
// retrying when it receives a sequence.
func TestAppendEventIsIdempotentOnTheEventID(t *testing.T) {
	f := newIngestFixture(t)
	eventID, runID := ulid.Make().String(), ulid.Make().String()
	body := reportBody(eventID, runID, "running")

	first := f.post(t, f.ws.ID, f.token, body)
	second := f.post(t, f.ws.ID, f.token, body)

	if first.Code != http.StatusAccepted || second.Code != http.StatusAccepted {
		t.Fatalf("statuses %d and %d, want 202", first.Code, second.Code)
	}
	if !strings.Contains(second.Body.String(), `"appended":false`) {
		t.Errorf("the retry appended a second event: %s", second.Body.String())
	}
}

func TestAppendEventRefusesAnUnreachableState(t *testing.T) {
	f := newIngestFixture(t)
	runID := ulid.Make().String()

	if w := f.post(t, f.ws.ID, f.token, reportBody(ulid.Make().String(), runID, "running")); w.Code != http.StatusAccepted {
		t.Fatalf("first report: status %d, want 202", w.Code)
	}
	if w := f.post(t, f.ws.ID, f.token, reportBody(ulid.Make().String(), runID, "succeeded")); w.Code != http.StatusAccepted {
		t.Fatalf("terminal report: status %d, want 202", w.Code)
	}
	// A run that has ended cannot start again.
	if w := f.post(t, f.ws.ID, f.token, reportBody(ulid.Make().String(), runID, "running")); w.Code != http.StatusConflict {
		t.Errorf("report after a terminal state: status %d, want 409", w.Code)
	}
}

// The owner comes from the record and not from the body, so a runtime cannot attribute
// its run to another user. An unknown field is refused rather than ignored.
func TestAppendEventCannotChooseTheOwner(t *testing.T) {
	f := newIngestFixture(t)
	body := `{"event_id":"` + ulid.Make().String() + `","run_id":"` + ulid.Make().String() +
		`","state":"running","mode":"interactive","owner_id":"someone-else",` +
		`"occurred_at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}`

	w := f.post(t, f.ws.ID, f.token, body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400. body: %s", w.Code, w.Body.String())
	}
}

func TestAppendEventRefusesAnUnknownAttribute(t *testing.T) {
	f := newIngestFixture(t)
	body := `{"event_id":"` + ulid.Make().String() + `","run_id":"` + ulid.Make().String() +
		`","state":"running","mode":"interactive","attributes":{"prompt":"summarise my repository"},` +
		`"occurred_at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}`

	w := f.post(t, f.ws.ID, f.token, body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400. body: %s", w.Code, w.Body.String())
	}
}
