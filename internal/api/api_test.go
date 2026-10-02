package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"crypto/ed25519"
	"crypto/rand"
	"github.com/gobackto-work/pestilence/internal/auth"
	"github.com/gobackto-work/pestilence/internal/store"
	"github.com/gobackto-work/pestilence/internal/tenant"
	"github.com/gobackto-work/pestilence/internal/workspace"
	"github.com/golang-jwt/jwt/v5"
	"sync"
	"time"
)

func newServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	s, err := store.OpenSQLite(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	srv := New(s, Config{Verifier: testVerifier(t), HostnameSuffix: "gobackto.work"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return srv, s
}

// The owner every assertion in these tests names. A GitHub numeric id, because
// that is the shape the contract pins.
const testOwner = "github#583231"

var (
	testKey     ed25519.PrivateKey
	testKeyOnce sync.Once
)

// testVerifier builds a verifier from a key generated once for the whole binary.
// The VERIFIER is what is under test here; a fresh key pair per test would only
// be more setup.
func testVerifier(t *testing.T) *auth.Verifier {
	t.Helper()
	testKeyOnce.Do(func() {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		testKey = priv
	})
	pub, ok := testKey.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatal("public key is not ed25519")
	}
	v, err := auth.NewVerifier(pub, "", "", 0)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}

// assertion mints the token town would present for an owner.
func assertion(t *testing.T, owner string) string {
	t.Helper()
	now := time.Now()
	claims := jwt.RegisteredClaims{
		Issuer:    auth.Issuer,
		Audience:  jwt.ClaimStrings{auth.Audience},
		Subject:   owner,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute)),
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims).SignedString(testKey)
	if err != nil {
		t.Fatalf("sign assertion: %v", err)
	}
	return signed
}

func do(t *testing.T, srv *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doAs(t, srv, testOwner, method, path, body)
}

// doAs makes a request as a particular owner, or anonymously when owner is empty.
func doAs(t *testing.T, srv *Server, owner, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if owner != "" {
		r.Header.Set("Authorization", "Bearer "+assertion(t, owner))
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	return w
}

func decodeDTO(t *testing.T, w *httptest.ResponseRecorder) workspaceDTO {
	t.Helper()
	var dto workspaceDTO
	if err := json.Unmarshal(w.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode response %q: %v", w.Body.String(), err)
	}
	return dto
}

// Creating records INTENT. It must not provision, and it must return immediately
// with the workspace in PROVISIONING.
func TestCreateRecordsIntent(t *testing.T) {
	srv, s := newServer(t)
	w := do(t, srv, "POST", "/api/workspaces", `{"limits":{"cpu":"1","memory":"2Gi","storage":"10Gi","maxAgents":3}}`)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	dto := decodeDTO(t, w)
	if dto.State != string(workspace.StateProvisioning) {
		t.Errorf("state = %s, want PROVISIONING", dto.State)
	}
	if dto.ID == "" || dto.Slug == "" {
		t.Error("id and slug must be set")
	}
	if dto.Namespace != tenant.Namespace(dto.Slug) {
		t.Errorf("namespace = %q, want %q", dto.Namespace, tenant.Namespace(dto.Slug))
	}
	if dto.URL != "https://"+dto.Hostname {
		t.Errorf("url = %q, want it derived from hostname %q", dto.URL, dto.Hostname)
	}
	if !strings.HasSuffix(dto.Hostname, ".gobackto.work") {
		t.Errorf("hostname = %q, want the configured suffix", dto.Hostname)
	}
	// The owner comes from CONFIG, never from the request.
	if dto.OwnerID != testOwner {
		t.Errorf("ownerId = %q, want the asserted owner", dto.OwnerID)
	}
	if dto.Limits.MaxAgents != 3 {
		t.Errorf("maxAgents = %d, want 3", dto.Limits.MaxAgents)
	}

	// And it is genuinely persisted, not just echoed.
	if _, err := s.Get(context.Background(), dto.ID); err != nil {
		t.Errorf("record was not stored: %v", err)
	}
}

func TestCreateWithNoBodyUsesDefaults(t *testing.T) {
	srv, _ := newServer(t)
	defaults := tenant.DefaultLimits()
	w := do(t, srv, "POST", "/api/workspaces", "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	dto := decodeDTO(t, w)
	if dto.Limits.CPU != defaults.LimitsCPU.String() {
		t.Errorf("cpu = %q, want the default %q", dto.Limits.CPU, defaults.LimitsCPU.String())
	}
}

// A request above the platform maximum is refused at the boundary, with a code
// the UI can branch on, rather than failing later as an admission error.
func TestCreateRejectsLimitsAboveThePlatformMaximum(t *testing.T) {
	srv, _ := newServer(t)
	w := do(t, srv, "POST", "/api/workspaces", `{"limits":{"cpu":"99"}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Error.Code != "invalid_limits" {
		t.Errorf("code = %q, want invalid_limits", body.Error.Code)
	}
}

// An unknown field means the caller believes something had an effect that did
// not. Refusing is the honest response.
func TestCreateRejectsUnknownFields(t *testing.T) {
	srv, _ := newServer(t)
	w := do(t, srv, "POST", "/api/workspaces", `{"limits":{"cpu":"1"},"ownerId":"someone-else"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an unknown field; body=%s", w.Code, w.Body.String())
	}
}

// The owner is not a request field at all, so a caller cannot claim one.
func TestOwnerCannotBeChosenByTheCaller(t *testing.T) {
	srv, s := newServer(t)
	w := do(t, srv, "POST", "/api/workspaces", `{"limits":{}}`)
	dto := decodeDTO(t, w)

	rec, err := s.Get(context.Background(), dto.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.OwnerID != testOwner {
		t.Errorf("stored owner = %q, want the asserted owner", rec.OwnerID)
	}
}

func TestGetAndList(t *testing.T) {
	srv, _ := newServer(t)
	created := decodeDTO(t, do(t, srv, "POST", "/api/workspaces", ""))

	if w := do(t, srv, "GET", "/api/workspaces/"+created.ID, ""); w.Code != http.StatusOK {
		t.Errorf("get status = %d, want 200", w.Code)
	}
	if w := do(t, srv, "GET", "/api/workspaces/nope", ""); w.Code != http.StatusNotFound {
		t.Errorf("get unknown status = %d, want 404", w.Code)
	}

	w := do(t, srv, "GET", "/api/workspaces", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200", w.Code)
	}
	var body struct {
		Workspaces []workspaceDTO `json:"workspaces"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(body.Workspaces) != 1 || body.Workspaces[0].ID != created.ID {
		t.Errorf("list = %+v, want the one created workspace", body.Workspaces)
	}
}

// An empty list must serialise as [] rather than null, or the UI has to special
// case it.
func TestListIsAnEmptyArrayNotNull(t *testing.T) {
	srv, _ := newServer(t)
	w := do(t, srv, "GET", "/api/workspaces", "")
	if !bytes.Contains(w.Body.Bytes(), []byte(`"workspaces":[]`)) {
		t.Errorf("body = %s, want an empty array", w.Body.String())
	}
}

// Delete marks intent and returns; the controller does the work. Asking twice
// must not be an error, because a UI that retries after a dropped response has
// not done anything wrong.
func TestDeleteIsIdempotent(t *testing.T) {
	srv, s := newServer(t)
	created := decodeDTO(t, do(t, srv, "POST", "/api/workspaces", ""))

	first := do(t, srv, "DELETE", "/api/workspaces/"+created.ID, "")
	if first.Code != http.StatusAccepted {
		t.Fatalf("first delete status = %d, want 202; body=%s", first.Code, first.Body.String())
	}
	if got := decodeDTO(t, first).State; got != string(workspace.StateDeleting) {
		t.Errorf("state = %s, want DELETING", got)
	}

	second := do(t, srv, "DELETE", "/api/workspaces/"+created.ID, "")
	if second.Code != http.StatusOK {
		t.Errorf("second delete status = %d, want 200 (idempotent)", second.Code)
	}
	if got := decodeDTO(t, second).State; got != string(workspace.StateDeleting) {
		t.Errorf("state = %s, want it unchanged", got)
	}
	if _, err := s.Get(context.Background(), created.ID); err != nil {
		t.Errorf("record disappeared: %v", err)
	}
}

func TestDeleteUnknownIsNotFound(t *testing.T) {
	srv, _ := newServer(t)
	if w := do(t, srv, "DELETE", "/api/workspaces/nope", ""); w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// Only FAILED can be retried. Anything else is a conflict, so the UI learns the
// rule rather than silently getting a no-op.
func TestRetryOnlyAppliesToFailed(t *testing.T) {
	srv, s := newServer(t)
	created := decodeDTO(t, do(t, srv, "POST", "/api/workspaces", ""))

	if w := do(t, srv, "POST", "/api/workspaces/"+created.ID+"/retry", ""); w.Code != http.StatusConflict {
		t.Errorf("retry of a PROVISIONING workspace = %d, want 409", w.Code)
	}

	if err := s.UpdateState(context.Background(), created.ID, workspace.StateProvisioning, workspace.StateFailed); err != nil {
		t.Fatalf("force FAILED: %v", err)
	}
	w := do(t, srv, "POST", "/api/workspaces/"+created.ID+"/retry", "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("retry status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	if got := decodeDTO(t, w).State; got != string(workspace.StateProvisioning) {
		t.Errorf("state = %s, want PROVISIONING", got)
	}
}

// A lastError recorded by the controller must be visible to the UI: a failed
// workspace with no reason is not actionable.
func TestLastErrorIsSurfaced(t *testing.T) {
	srv, s := newServer(t)
	created := decodeDTO(t, do(t, srv, "POST", "/api/workspaces", ""))
	if err := s.SetError(context.Background(), created.ID, "quota exceeded"); err != nil {
		t.Fatalf("SetError: %v", err)
	}
	w := do(t, srv, "GET", "/api/workspaces/"+created.ID, "")
	if got := decodeDTO(t, w).LastError; got != "quota exceeded" {
		t.Errorf("lastError = %q, want it surfaced", got)
	}
}

func TestHealthz(t *testing.T) {
	srv, _ := newServer(t)
	if w := do(t, srv, "GET", "/healthz", ""); w.Code != http.StatusOK {
		t.Errorf("healthz = %d, want 200", w.Code)
	}
}

// A body larger than any legitimate request is refused rather than buffered.
func TestOversizedBodyIsRefused(t *testing.T) {
	srv, _ := newServer(t)
	huge := `{"limits":{"cpu":"` + strings.Repeat("1", maxBodyBytes+16) + `"}}`
	if w := do(t, srv, "POST", "/api/workspaces", huge); w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an oversized body", w.Code)
	}
}

// ---------------------------------------------------------------------------
// authentication
// ---------------------------------------------------------------------------

func TestHealthzNeedsNoAssertion(t *testing.T) {
	// town's readiness check calls this to decide whether it can serve, and
	// readiness has no user to mint an assertion for. Requiring one would make
	// town report itself unready forever.
	srv, _ := newServer(t)
	if w := doAs(t, srv, "", "GET", "/healthz", ""); w.Code != http.StatusOK {
		t.Errorf("healthz = %d, want 200 with no assertion", w.Code)
	}
}

func TestAPIRejectsAMissingAssertion(t *testing.T) {
	srv, _ := newServer(t)
	requests := []struct{ method, path string }{
		{"GET", "/api/workspaces"},
		{"POST", "/api/workspaces"},
		{"GET", "/api/workspaces/x"},
		{"DELETE", "/api/workspaces/x"},
		{"POST", "/api/workspaces/x/retry"},
	}
	for _, tc := range requests {
		w := doAs(t, srv, "", tc.method, tc.path, "")
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401", tc.method, tc.path, w.Code)
		}
		if got := w.Header().Get("WWW-Authenticate"); got != "Bearer" {
			t.Errorf("%s %s WWW-Authenticate = %q, want Bearer", tc.method, tc.path, got)
		}
	}
}

func TestAPIRejectsAnAssertionThatDoesNotVerify(t *testing.T) {
	srv, _ := newServer(t)
	for name, header := range map[string]string{
		"rubbish":            "Bearer not-a-token",
		"no scheme":          "not-a-token",
		"wrong scheme":       "Basic abc",
		"empty after scheme": "Bearer ",
		"signed elsewhere":   "Bearer " + assertionsFromAnotherKey(t),
	} {
		r := httptest.NewRequest("GET", "/api/workspaces", nil)
		r.Header.Set("Authorization", header)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, w.Code)
		}
	}
}

// Every authenticated member sees every workspace. The org is the boundary, not the
// individual -- so the list is NOT filtered, and ownerId travels with each record so the
// UI can still tell people which ones are theirs to change.
func TestListIsVisibleToEveryMember(t *testing.T) {
	srv, _ := newServer(t)
	mine := decodeDTO(t, doAs(t, srv, "github#1", "POST", "/api/workspaces", ""))
	theirs := decodeDTO(t, doAs(t, srv, "github#2", "POST", "/api/workspaces", ""))

	var listed struct {
		Workspaces []workspaceDTO `json:"workspaces"`
	}
	if err := json.Unmarshal(doAs(t, srv, "github#1", "GET", "/api/workspaces", "").Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed.Workspaces) != 2 {
		t.Fatalf("listed %d workspaces, want both", len(listed.Workspaces))
	}

	seen := map[string]string{}
	for _, ws := range listed.Workspaces {
		seen[ws.ID] = ws.OwnerID
	}
	if seen[mine.ID] != "github#1" || seen[theirs.ID] != "github#2" {
		t.Errorf("ownerId should travel with each record; got %v", seen)
	}
}

// Reading is org-wide; changing is the owner's alone. This is the one distinction the
// model keeps, so it is asserted route by route rather than assumed.
func TestReadingAnotherOwnersWorkspaceIsAllowedButChangingItIsNot(t *testing.T) {
	srv, _ := newServer(t)
	theirs := decodeDTO(t, doAs(t, srv, "github#2", "POST", "/api/workspaces", ""))

	// Reading: allowed.
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/workspaces/" + theirs.ID},
	} {
		if w := doAs(t, srv, "github#1", tc.method, tc.path, ""); w.Code != http.StatusOK {
			t.Errorf("%s %s = %d, want 200", tc.method, tc.path, w.Code)
		}
	}

	// Changing: refused.
	for _, tc := range []struct{ method, path string }{
		{"DELETE", "/api/workspaces/" + theirs.ID},
		{"POST", "/api/workspaces/" + theirs.ID + "/retry"},
	} {
		w := doAs(t, srv, "github#1", tc.method, tc.path, "")
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403", tc.method, tc.path, w.Code)
		}
	}
}

// A refused request must not have changed anything: a 403 that still acted would
// be worse than no check at all.
func TestAForbiddenDeleteDoesNotDelete(t *testing.T) {
	srv, s := newServer(t)
	theirs := decodeDTO(t, doAs(t, srv, "github#2", "POST", "/api/workspaces", ""))

	if w := doAs(t, srv, "github#1", "DELETE", "/api/workspaces/"+theirs.ID, ""); w.Code != http.StatusForbidden {
		t.Fatalf("delete = %d, want 403", w.Code)
	}
	rec, err := s.Get(context.Background(), theirs.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != workspace.StateProvisioning {
		t.Errorf("state = %s, want it untouched at PROVISIONING", rec.State)
	}
}

// assertionsFromAnotherKey signs a well-formed assertion with a key pestilence
// does not trust.
func assertionsFromAnotherKey(t *testing.T) string {
	t.Helper()
	_, other, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	now := time.Now()
	registered := jwt.RegisteredClaims{
		Issuer:    auth.Issuer,
		Audience:  jwt.ClaimStrings{auth.Audience},
		Subject:   testOwner,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute)),
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, registered).SignedString(other)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

// runningWorkspace creates a workspace and moves it to RUNNING, which is the state
// its endpoint actually exists in.
func runningWorkspace(t *testing.T, srv *Server, s store.Store, owner string) workspaceDTO {
	t.Helper()
	dto := decodeDTO(t, doAs(t, srv, owner, "POST", "/api/workspaces", ""))
	if err := s.UpdateState(context.Background(), dto.ID, workspace.StateProvisioning, workspace.StateRunning); err != nil {
		t.Fatalf("UpdateState: %v", err)
	}
	return dto
}

func TestAuthorizeAllowsTheOwnerOfARunningWorkspace(t *testing.T) {
	srv, s := newServer(t)
	ws := runningWorkspace(t, srv, s, testOwner)

	w := do(t, srv, "POST", "/api/authorize", `{"hostname":"`+ws.Hostname+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("authorize = %d, want 200; body %s", w.Code, w.Body.String())
	}
	var got authorizeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.User != testOwner {
		t.Errorf("user = %q, want %q", got.User, testOwner)
	}
	if got.Slug != ws.Slug {
		t.Errorf("slug = %q, want %q", got.Slug, ws.Slug)
	}
	if got.Namespace != ws.Namespace {
		t.Errorf("namespace = %q, want %q", got.Namespace, ws.Namespace)
	}
}

// A Host header may carry a port. Traefik passes the original through, so this has to
// work rather than 404 on a perfectly valid request.
func TestAuthorizeAcceptsAHostnameWithAPort(t *testing.T) {
	srv, s := newServer(t)
	ws := runningWorkspace(t, srv, s, testOwner)

	for _, h := range []string{ws.Hostname + ":443", ws.Hostname + ":80", strings.ToUpper(ws.Hostname)} {
		w := do(t, srv, "POST", "/api/authorize", `{"hostname":"`+h+`"}`)
		if w.Code != http.StatusOK {
			t.Errorf("authorize(%q) = %d, want 200", h, w.Code)
		}
	}
}

// Any authenticated member may open any workspace: the org is the boundary, not the
// individual. What is still enforced is AUTHENTICATION -- requireOwner runs first, so an
// anonymous caller never reaches the decision at all.
func TestAuthorizeAdmitsAnyMember(t *testing.T) {
	srv, s := newServer(t)
	ws := runningWorkspace(t, srv, s, "github#2")

	if w := do(t, srv, "POST", "/api/authorize", `{"hostname":"`+ws.Hostname+`"}`); w.Code != http.StatusOK {
		t.Fatalf("authorize = %d, want 200 -- the caller is a member", w.Code)
	}

	// The same request without an assertion is refused, which is the half that matters
	// for exposure: the workspace endpoint is no longer reachable anonymously.
	r := httptest.NewRequest("POST", "/api/authorize", strings.NewReader(`{"hostname":"`+ws.Hostname+`"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous authorize = %d, want 401", rec.Code)
	}
}

// A workspace that is not RUNNING has no endpoint, so there is nothing to authorize
// against. 404 rather than 403, because the resource genuinely is not there.
func TestAuthorizeHidesWorkspacesThatAreNotRunning(t *testing.T) {
	srv, _ := newServer(t)
	ws := decodeDTO(t, do(t, srv, "POST", "/api/workspaces", "")) // PROVISIONING
	if ws.State != string(workspace.StateProvisioning) {
		t.Fatalf("setup: state = %s, want PROVISIONING", ws.State)
	}

	w := do(t, srv, "POST", "/api/authorize", `{"hostname":"`+ws.Hostname+`"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("authorize = %d, want 404", w.Code)
	}
}

// The hostname is attacker-controlled even though it arrives from town, because Traefik
// passes the ORIGINAL request's Host through. Every rejection must look identical, so a
// probe cannot tell "not a workspace hostname" from "no such workspace".
func TestAuthorizeRejectsHostnamesThatAreNotWorkspaceEndpoints(t *testing.T) {
	srv, s := newServer(t)
	ws := runningWorkspace(t, srv, s, testOwner)

	for _, tc := range []struct{ name, host string }{
		{"empty", ""},
		{"the bare suffix", "gobackto.work"},
		{"a different domain", "evil.example"},
		{"the suffix as a prefix", "gobackto.work.evil.example"},
		{"a nested subdomain", "a.b." + ws.Hostname},
		{"path traversal in the slug", "../" + ws.Slug + ".gobackto.work"},
		{"an invalid slug", "Not A Slug.gobackto.work"},
		{"an IPv6 literal", "[::1]:443"},
		{"no hostname at all", "."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, srv, "POST", "/api/authorize", `{"hostname":"`+tc.host+`"}`)
			if w.Code != http.StatusNotFound {
				t.Errorf("authorize(%q) = %d, want 404", tc.host, w.Code)
			}
		})
	}
}

func TestAuthorizeNeedsAnAssertion(t *testing.T) {
	srv, s := newServer(t)
	ws := runningWorkspace(t, srv, s, testOwner)

	w := doAs(t, srv, "", "POST", "/api/authorize", `{"hostname":"`+ws.Hostname+`"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("authorize with no assertion = %d, want 401", w.Code)
	}
}

// The only thing a caller may vary is which hostname they are asking about. An extra
// field is a sign that one side of the contract has moved without the other.
func TestAuthorizeRejectsUnknownFields(t *testing.T) {
	srv, s := newServer(t)
	ws := runningWorkspace(t, srv, s, testOwner)

	w := do(t, srv, "POST", "/api/authorize", `{"hostname":"`+ws.Hostname+`","owner":"github#1"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("authorize with an extra field = %d, want 400", w.Code)
	}
}
