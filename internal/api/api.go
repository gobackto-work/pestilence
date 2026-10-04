// Package api is the control plane's HTTP surface.
//
// It records INTENT and nothing else. Creating a workspace writes a record in
// PROVISIONING and returns; the controller does the work. That keeps a slow
// Kubernetes operation off the request path and makes provisioning retryable.
//
// There is no session here and no login flow: town is the OIDC client and the
// identity holder, because pestilence has no public hostname and an authorization-code
// flow needs one. town presents a short-lived Ed25519 assertion, this API verifies it,
// and the owner of a workspace comes from the verified subject and from nothing else.
//
// The API is deliberately NOT published. town is a BFF and reaches it over the cluster
// network, which is what makes it safe to trust an assertion rather than a session --
// see networkpolicy-pestilence.yaml in my-opps.
package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gobackto-work/pestilence/internal/eventlog"
	"github.com/gobackto-work/pestilence/internal/store"
	"github.com/gobackto-work/pestilence/internal/tenant"
	"github.com/gobackto-work/pestilence/internal/workspace"

	"context"
	"github.com/gobackto-work/pestilence/internal/auth"
	"github.com/oklog/ulid/v2"
)

// maxBodyBytes bounds a request body. Every request here is small, so anything
// larger is a mistake or an attack.
const maxBodyBytes = 1 << 20

// Config is what the API needs that is not per-request.
type Config struct {
	// Verifier checks the assertion town presents. Required.
	//
	// The owner of a workspace comes from the verified assertion and from nothing
	// else. There is no configuration fallback and no request field: a
	// caller-supplied owner would be a spoofable ownership field, and a configured
	// one would mean every tenant shared a single identity.
	Verifier *auth.Verifier

	// HostnameSuffix is appended to the slug, e.g. "gobackto.work".
	HostnameSuffix string

	// Events records a state transition that a workspace's runtime reports, and serves the
	// events an owner has not taken. Required for the ingest and read endpoints.
	Events EventRecord

	// TokenKeys resolves the public key that signs one workspace's capability token,
	// by slug. Required for the ingest endpoint.
	TokenKeys TokenKeySource
}

// EventRecord is the durable record of run events. eventlog.Log satisfies it.
//
// An interface and not the concrete type so that this package does not depend on where the
// record is stored, and so that a test can refuse a write deliberately.
type EventRecord interface {
	Append(ctx context.Context, r eventlog.Report) (eventlog.AppendResult, error)
	// EnsurePull opens the read subscription for a principal, creating it once.
	EnsurePull(ctx context.Context, principalID string) (eventlog.Subscription, error)
	// Outstanding returns the events a subscription has not taken, and the subscription's
	// cursor.
	Outstanding(ctx context.Context, subscriptionID string, limit int) (eventlog.Subscription, []eventlog.Event, error)
	// AdvanceCursor records what a subscriber has taken.
	AdvanceCursor(ctx context.Context, id string, to int64) error
}

// TokenKeySource resolves the public key that signs one workspace's capability token.
//
// The key is per workspace. The control plane minted it, so an implementation reads it
// back from the control plane's own Secret and never from a tenant namespace.
type TokenKeySource interface {
	PublicKey(ctx context.Context, slug string) (ed25519.PublicKey, error)
}

// Server is the control plane's HTTP handler.
type Server struct {
	store store.Store
	cfg   Config
	log   *slog.Logger
	mux   *http.ServeMux
}

// New returns a Server. An empty HostnameSuffix takes the default.
//
// It panics if Config.Verifier is nil. A control plane with no verifier would
// either serve every request unauthenticated or fail on the first one, and a
// wiring mistake should stop the process rather than become a runtime mystery.
func New(s store.Store, cfg Config, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	if cfg.HostnameSuffix == "" {
		cfg.HostnameSuffix = "gobackto.work"
	}
	if cfg.Verifier == nil {
		panic("api.New: Config.Verifier is required")
	}
	if cfg.Events == nil || cfg.TokenKeys == nil {
		panic("api.New: Config.Events and Config.TokenKeys are required by the ingest endpoint")
	}
	srv := &Server{store: s, cfg: cfg, log: log, mux: http.NewServeMux()}

	// Unauthenticated, and not only for probes: town's readiness check calls this,
	// and readiness has no user to mint an assertion for. Everything below it
	// requires one.
	srv.mux.HandleFunc("GET /healthz", srv.handleHealth)
	srv.mux.HandleFunc("POST /api/workspaces", srv.requireOwner(srv.handleCreate))
	srv.mux.HandleFunc("GET /api/workspaces", srv.requireOwner(srv.handleList))
	srv.mux.HandleFunc("GET /api/workspaces/{id}", srv.requireOwner(srv.handleGet))
	srv.mux.HandleFunc("DELETE /api/workspaces/{id}", srv.requireOwner(srv.handleDelete))
	srv.mux.HandleFunc("POST /api/workspaces/{id}/retry", srv.requireOwner(srv.handleRetry))

	// The ForwardAuth decision for tenant endpoints. Traefik asks town, town asks
	// this, and this holds the ownership rule because it holds the records.
	srv.mux.HandleFunc("POST /api/authorize", srv.requireOwner(srv.handleAuthorize))

	// A workspace's runtime reports that one of its runs changed state.
	//
	// The route names no workspace. The token does, and the workspace owns the key that
	// proves the token, so the claim selects a key and the key is what makes the claim
	// true. The caller therefore has nothing to spoof: it cannot report as a workspace
	// other than the one its token was minted for.
	srv.mux.HandleFunc("POST /api/events", srv.requireReporter(srv.handleAppendEvent))

	// The read side. These are owner-scoped like every other route a user calls: the owner
	// comes from the assertion, and a subscription takes the events of one principal and no
	// others, so there is nothing here that can read another user's runs.
	srv.mux.HandleFunc("GET /api/events", srv.requireOwner(srv.handleEvents))
	srv.mux.HandleFunc("POST /api/events/ack", srv.requireOwner(srv.handleAckEvents))

	return srv
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// ---------------------------------------------------------------------------
// authentication
// ---------------------------------------------------------------------------

// ownerKey is the context key for the verified owner id. A private struct rather
// than a string, so nothing outside this package can collide with it.
type ownerKey struct{}

// ownerFrom returns the verified owner id, or "" if the request was not
// authenticated. Handlers behind requireOwner can rely on it being non-empty.
func ownerFrom(ctx context.Context) string {
	owner, _ := ctx.Value(ownerKey{}).(string)
	return owner
}

// requireOwner rejects any request that does not carry a valid assertion, and
// hands the verified owner to the handler through the context.
func (s *Server) requireOwner(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		assertion, ok := bearerToken(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer assertion")
			return
		}
		owner, err := s.cfg.Verifier.Verify(assertion)
		if err != nil {
			// The reason is logged but never returned: telling a caller which half
			// of a forged assertion to fix next is free help.
			s.log.Warn("assertion rejected", "err", err)
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "unauthorized", "assertion is not valid")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ownerKey{}, owner)))
	}
}

// bearerToken pulls the token out of the Authorization header.
func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	token, found := strings.CutPrefix(header, "Bearer ")
	if !found || token == "" {
		return "", false
	}
	return token, true
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

// handleHealth reports readiness. It touches the store, so a control plane whose
// database is gone is reported unhealthy rather than serving errors.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if _, err := s.store.List(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "store is not reachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type createRequest struct {
	Limits struct {
		CPU       string `json:"cpu"`
		Memory    string `json:"memory"`
		Storage   string `json:"storage"`
		MaxAgents int32  `json:"maxAgents"`
	} `json:"limits"`
}

// handleCreate records a new workspace in PROVISIONING.
//
// It does NOT provision. The controller picks it up on its next pass, which is
// what makes the request fast and the operation retryable.
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// LimitsFrom enforces the platform maximums, so an impossible request is
	// refused here rather than failing later as a confusing admission error.
	limits, err := tenant.LimitsFrom(req.Limits.CPU, req.Limits.Memory, req.Limits.Storage, req.Limits.MaxAgents)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_limits", err.Error())
		return
	}

	now := time.Now().UTC()
	rec, err := s.newRecord(ownerFrom(r.Context()), limits, now)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	// A slug collision is possible (they are random) and retryable; anything else
	// is not.
	for attempt := 0; ; attempt++ {
		err = s.store.Create(r.Context(), rec)
		if err == nil {
			break
		}
		if attempt >= 4 || !isDuplicate(err) {
			s.log.Error("create workspace failed", "slug", rec.Slug, "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "could not create workspace")
			return
		}
		if rec, err = s.newRecord(ownerFrom(r.Context()), limits, now); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
	}

	s.log.Info("workspace recorded", "id", rec.ID, "slug", rec.Slug, "state", rec.State)
	writeJSON(w, http.StatusCreated, toDTO(rec))
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.List(r.Context())
	if err != nil {
		s.log.Error("list workspaces failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not list workspaces")
		return
	}
	// Everything, to every authenticated member. The org is the boundary, not the
	// individual -- so a workspace is visible to colleagues, and ownerID travels with
	// each record so the UI can still tell people which ones are theirs to change.
	out := make([]workspaceDTO, 0, len(all))
	for _, rec := range all {
		out = append(out, toDTO(rec))
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspaces": out})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	rec, err := s.store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such workspace")
		return
	}
	if err != nil {
		s.log.Error("get workspace failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read workspace")
		return
	}
	// No ownership check: a workspace is visible to every authenticated member.
	writeJSON(w, http.StatusOK, toDTO(rec))
}

// authorizeRequest is the ForwardAuth question. One field, and unknown fields are
// refused like every other body in this API: the only thing a caller may vary is which
// hostname they are asking about.
type authorizeRequest struct {
	Hostname string `json:"hostname"`
}

// authorizeResponse is what town relays to Traefik, which forwards it upstream as
// X-Auth-User so the agent can know who it is talking to.
type authorizeResponse struct {
	User      string `json:"user"`
	Slug      string `json:"slug"`
	Namespace string `json:"namespace"`
}

// handleAuthorize answers "may this caller reach the workspace serving this hostname?".
//
// The decision lives here rather than in town because the records live here: town knows
// WHO the user is and holds the session, pestilence knows WHAT exists. Splitting it the
// other way would mean two implementations of one rule, and the one that drifted would be
// the one nobody tested.
//
// ANY AUTHENTICATED MEMBER MAY OPEN ANY WORKSPACE. The org is the boundary, not the
// individual. `requireOwner` still runs, so an unauthenticated request is refused before
// this is reached -- what is dropped here is the OWNERSHIP comparison, not authentication.
//
// Stated plainly, because "read visibility" undersells it: there is no read-only view of
// a workspace. Opening one reaches its agent, its files, and the model credential on its
// volume, so a colleague who opens someone else's workspace spends that owner's model
// credits against that owner's key. Anyone who may open it may use it.
//
// What does NOT change: deletion and retry stay owner-only. Opening a colleague's
// workspace is collaboration; destroying it is not.
//
// POST with a JSON body rather than GET with a query string, so a hostname cannot end up
// in an access log's URL line.
//
// A workspace that is not RUNNING gets 404, not 403. The endpoint genuinely does not exist
// yet, and 403 would imply the caller could be granted something that is not there.
func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	var req authorizeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	slug, err := s.slugFromHostname(req.Hostname)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no workspace serves that hostname")
		return
	}
	rec, err := s.store.GetBySlug(r.Context(), slug)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no workspace serves that hostname")
		return
	}
	if err != nil {
		s.log.Error("authorize lookup failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read workspace")
		return
	}
	if rec.State != workspace.StateRunning {
		writeError(w, http.StatusNotFound, "not_found", "no workspace serves that hostname")
		return
	}

	// Identity is recorded rather than compared. requireOwner already refused anyone
	// unauthenticated; this is the trail for who opened whose workspace, which is the
	// question worth being able to answer afterwards.
	s.log.Info("workspace opened", "slug", rec.Slug, "owner", rec.OwnerID, "by", ownerFrom(r.Context()))

	writeJSON(w, http.StatusOK, authorizeResponse{
		User:      rec.OwnerID,
		Slug:      rec.Slug,
		Namespace: rec.Namespace,
	})
}

// slugFromHostname maps a request hostname to a workspace slug, or fails.
//
// Deliberately strict, because the input is attacker-controlled even though it arrives
// from town: Traefik passes the ORIGINAL request's Host through, so whoever sent that
// request chose it. Every rejection is reported as the same 404 as an unknown
// workspace, so a probe cannot distinguish "not a workspace hostname" from "no such
// workspace".
func (s *Server) slugFromHostname(host string) (string, error) {
	h := strings.ToLower(strings.TrimSpace(host))
	// A Host header may carry a port. Only strip a numeric suffix, so an IPv6
	// literal is left intact and then fails the suffix check below rather than being
	// silently mangled into something that might match.
	if i := strings.LastIndex(h, ":"); i >= 0 && isAllDigits(h[i+1:]) {
		h = h[:i]
	}
	suffix := "." + strings.ToLower(strings.TrimPrefix(s.cfg.HostnameSuffix, "."))
	if !strings.HasSuffix(h, suffix) {
		return "", fmt.Errorf("hostname %q does not end in %q", host, suffix)
	}
	slug := strings.TrimSuffix(h, suffix)
	if err := tenant.ValidateSlug(slug); err != nil {
		return "", err
	}
	return slug, nil
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// owns reports whether the caller may CHANGE a record, and writes the refusal if not.
//
// Used only by the mutating routes. Reading is org-wide; destroying and retrying are the
// owner's alone, which is the one distinction this model keeps.
func (s *Server) owns(w http.ResponseWriter, rec workspace.Workspace, owner string) bool {
	if rec.OwnerID != owner {
		// 403 rather than 404. The ids are unguessable ULIDs, so this discloses
		// nothing useful, and reporting "not found" would make a real ownership
		// problem look like a UI bug for as long as it took to find.
		writeError(w, http.StatusForbidden, "forbidden", "only the owner can change this workspace")
		return false
	}
	return true
}

// handleDelete marks the workspace for deletion; the controller does the work.
//
// It is idempotent: asking twice is not an error, because a UI that retries after
// a dropped response should not be told it did something wrong.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec, err := s.store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such workspace")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not read workspace")
		return
	}
	if !s.owns(w, rec, ownerFrom(r.Context())) {
		return
	}

	switch rec.State {
	case workspace.StateDeleting, workspace.StateDeleted:
		writeJSON(w, http.StatusOK, toDTO(rec)) // already going; idempotent
		return
	}

	if err := s.store.UpdateState(r.Context(), id, rec.State, workspace.StateDeleting); err != nil {
		if errors.Is(err, store.ErrConflict) {
			// Something moved it between our read and our write. Re-read and
			// report the truth rather than guessing.
			if fresh, gerr := s.store.Get(r.Context(), id); gerr == nil {
				writeJSON(w, http.StatusOK, toDTO(fresh))
				return
			}
		}
		s.log.Error("mark deleting failed", "id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not mark workspace for deletion")
		return
	}
	s.log.Info("workspace marked for deletion", "id", id, "slug", rec.Slug)

	if fresh, err := s.store.Get(r.Context(), id); err == nil {
		writeJSON(w, http.StatusAccepted, toDTO(fresh))
		return
	}
	writeJSON(w, http.StatusAccepted, toDTO(rec))
}

// handleRetry moves a FAILED workspace back to PROVISIONING. FAILED is the one
// state with a way out, deliberately, so a transient error is not a brick.
func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec, err := s.store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such workspace")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not read workspace")
		return
	}
	if !s.owns(w, rec, ownerFrom(r.Context())) {
		return
	}
	if rec.State != workspace.StateFailed {
		writeError(w, http.StatusConflict, "conflict",
			fmt.Sprintf("only a FAILED workspace can be retried; this one is %s", rec.State))
		return
	}
	if err := s.store.UpdateState(r.Context(), id, workspace.StateFailed, workspace.StateProvisioning); err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeError(w, http.StatusConflict, "conflict", "the workspace changed; re-read and try again")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "could not retry workspace")
		return
	}
	s.log.Info("workspace retry requested", "id", id, "slug", rec.Slug)

	if fresh, err := s.store.Get(r.Context(), id); err == nil {
		writeJSON(w, http.StatusAccepted, toDTO(fresh))
		return
	}
	writeJSON(w, http.StatusAccepted, toDTO(rec))
}

// ---------------------------------------------------------------------------
// records and DTOs
// ---------------------------------------------------------------------------

func (s *Server) newRecord(owner string, limits tenant.Limits, now time.Time) (workspace.Workspace, error) {
	id, err := ulid.New(ulid.Timestamp(now), rand.Reader)
	if err != nil {
		return workspace.Workspace{}, fmt.Errorf("generate id: %w", err)
	}
	slug, err := tenant.GenerateSlug()
	if err != nil {
		return workspace.Workspace{}, fmt.Errorf("generate slug: %w", err)
	}
	rec := workspace.Workspace{
		ID:           id.String(),
		Slug:         slug,
		OwnerID:      owner,
		Namespace:    tenant.Namespace(slug),
		Hostname:     slug + "." + strings.TrimPrefix(s.cfg.HostnameSuffix, "."),
		State:        workspace.StateProvisioning,
		CreatedAt:    now,
		LastActiveAt: now,
		Limits: workspace.Limits{
			CPU:     limits.LimitsCPU.String(),
			Memory:  limits.LimitsMemory.String(),
			Storage: limits.Storage.String(),
			MaxPods: limits.Pods,
		},
	}
	if err := rec.Validate(); err != nil {
		return workspace.Workspace{}, err
	}
	return rec, nil
}

type limitsDTO struct {
	CPU       string `json:"cpu"`
	Memory    string `json:"memory"`
	Storage   string `json:"storage"`
	MaxAgents int32  `json:"maxAgents"`
}

type workspaceDTO struct {
	ID           string     `json:"id"`
	Slug         string     `json:"slug"`
	OwnerID      string     `json:"ownerId"`
	Namespace    string     `json:"namespace"`
	Hostname     string     `json:"hostname"`
	URL          string     `json:"url"`
	State        string     `json:"state"`
	CreatedAt    time.Time  `json:"createdAt"`
	LastActiveAt time.Time  `json:"lastActiveAt"`
	DeletedAt    *time.Time `json:"deletedAt"`
	LastError    string     `json:"lastError"`
	Limits       limitsDTO  `json:"limits"`
}

func toDTO(w workspace.Workspace) workspaceDTO {
	return workspaceDTO{
		ID:           w.ID,
		Slug:         w.Slug,
		OwnerID:      w.OwnerID,
		Namespace:    w.Namespace,
		Hostname:     w.Hostname,
		URL:          "https://" + w.Hostname,
		State:        string(w.State),
		CreatedAt:    w.CreatedAt,
		LastActiveAt: w.LastActiveAt,
		DeletedAt:    w.DeletedAt,
		LastError:    w.LastError,
		Limits: limitsDTO{
			CPU:       w.Limits.CPU,
			Memory:    w.Limits.Memory,
			Storage:   w.Limits.Storage,
			MaxAgents: w.Limits.MaxPods,
		},
	}
}

// ---------------------------------------------------------------------------
// request/response plumbing
// ---------------------------------------------------------------------------

// decodeJSON reads one JSON object, refusing unknown fields.
//
// Unknown fields are refused rather than ignored because a caller sending a field
// we do not understand believes it had an effect. Silently dropping it is how a
// UI ends up showing a setting that was never applied.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return nil // an empty body is fine: everything is optional
		}
		return fmt.Errorf("could not read request: %w", err)
	}
	return nil
}

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// writeError emits a stable code alongside the message, so the UI can branch on
// the code rather than parsing prose.
func writeError(w http.ResponseWriter, status int, code, message string) {
	var body errorBody
	body.Error.Code = code
	body.Error.Message = message
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// isDuplicate reports whether an error is a uniqueness violation. The store
// returns the driver's error verbatim, so this matches on the SQLite message
// rather than on a driver-specific type.
func isDuplicate(err error) bool {
	return err != nil && strings.Contains(strings.ToUpper(err.Error()), "UNIQUE")
}
