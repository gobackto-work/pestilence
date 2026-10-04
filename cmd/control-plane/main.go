// Command control-plane runs the pestilence control plane.
//
// It owns three things: the workspace record, the provisioner that applies
// Kubernetes objects, and the reconcile loop connecting them. The HTTP API only
// records intent; the loop does the work, which keeps slow Kubernetes operations
// off the request path and makes provisioning retryable.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gobackto-work/pestilence/internal/api"
	"github.com/gobackto-work/pestilence/internal/controller"
	"github.com/gobackto-work/pestilence/internal/eventlog"
	"github.com/gobackto-work/pestilence/internal/provisioner"
	"github.com/gobackto-work/pestilence/internal/store"
	"github.com/gobackto-work/pestilence/internal/tenant"

	"github.com/gobackto-work/pestilence/internal/auth"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	var (
		addr        = flag.String("addr", ":8080", "HTTP listen address")
		dbPath      = flag.String("db", "/var/lib/pestilence/pestilence.db", "SQLite database path")
		eventPath   = flag.String("event-db", "/var/lib/pestilence/events.db", "SQLite event record path")
		kubeconfig  = flag.String("kubeconfig", "", "kubeconfig path; empty uses in-cluster config")
		brokerImage = flag.String("broker-image", os.Getenv("PESTILENCE_BROKER_IMAGE"), "broker image ref")
		agentImage  = flag.String("agent-image", os.Getenv("PESTILENCE_AGENT_IMAGE"), "agent image ref")
		hostSuffix  = flag.String("hostname-suffix", "gobackto.work", "suffix appended to a workspace slug")
		pubKeyFile  = flag.String("assertion-pubkey", "/var/run/pestilence/assertion-key.pub", "PKIX PEM Ed25519 public key town signs assertions with")
		assertIss   = flag.String("assertion-issuer", auth.Issuer, "expected assertion issuer")
		assertAud   = flag.String("assertion-audience", auth.Audience, "expected assertion audience")
		interval    = flag.Duration("interval", 10*time.Second, "reconcile interval")
		delTimeout  = flag.Duration("deletion-timeout", 5*time.Minute, "how long one pass waits for a deletion to finish before retrying")
		driftEvery  = flag.Duration("drift-interval", 5*time.Minute, "how often steady workspaces are re-applied")
		tokenTTL    = flag.Duration("token-ttl", tenant.DefaultTokenTTL, "capability token lifetime; a token is re-minted once less than half of this remains")
		brokerTLS   = flag.Bool("broker-tls", false, "switch NEW workspaces to TLS for the broker hop; needs scarab to serve TLS, and needs workspaces recreated")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(opts{
		addr:            *addr,
		dbPath:          *dbPath,
		eventPath:       *eventPath,
		kubeconfig:      *kubeconfig,
		brokerImage:     *brokerImage,
		agentImage:      *agentImage,
		hostSuffix:      *hostSuffix,
		assertionKey:    *pubKeyFile,
		assertionIssuer: *assertIss,
		assertionAud:    *assertAud,
		interval:        *interval,
		driftInterval:   *driftEvery,
		tokenTTL:        *tokenTTL,
		brokerTLS:       *brokerTLS,
		deletionTimeout: *delTimeout,
	}, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

type opts struct {
	addr            string
	dbPath          string
	eventPath       string
	kubeconfig      string
	brokerImage     string
	agentImage      string
	hostSuffix      string
	assertionKey    string
	assertionIssuer string
	assertionAud    string
	interval        time.Duration
	driftInterval   time.Duration
	tokenTTL        time.Duration
	brokerTLS       bool
	deletionTimeout time.Duration
}

// openStores opens the registry and the event record, and returns a function that closes
// both.
//
// They are separate files on purpose. The registry is read-mostly and holds a row per
// workspace; the record is append-heavy and holds a row per transition. One file with one
// writer would make them contend for one lock.
func openStores(o opts) (*store.SQLite, *eventlog.Log, func(), error) {
	st, err := store.OpenSQLite(o.dbPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open store: %w", err)
	}
	events, err := eventlog.Open(o.eventPath)
	if err != nil {
		_ = st.Close()
		return nil, nil, nil, fmt.Errorf("open event record: %w", err)
	}
	return st, events, func() { _ = events.Close(); _ = st.Close() }, nil
}

// startBackground stops the process on a signal, and starts the record's pruning.
func startBackground(log *slog.Logger, events *eventlog.Log) (context.Context, func()) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go pruneEvents(ctx, events, log)
	return ctx, stop
}

// pruneInterval is how often the record discards what it no longer needs.
//
// An hour, because the limits are seven days and 1700 events: a record that is being kept
// below those does not need pruning more often, and one that is not needs an operator rather
// than a faster timer.
const pruneInterval = time.Hour

// pruneEvents discards events past the retention limit, on a timer.
//
// It reports what it could NOT discard, because a record that stays above its size limit means
// a subscription is not consuming. That is a fault, and pruning must never resolve it by
// discarding an event nobody has seen.
func pruneEvents(ctx context.Context, log *eventlog.Log, logger *slog.Logger) {
	ticker := time.NewTicker(pruneInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			result, err := log.Prune(ctx, time.Now().UTC())
			if err != nil {
				logger.Warn("could not prune the event record", "err", err)
				continue
			}
			if result.ByAge > 0 || result.ByCount > 0 {
				logger.Info("pruned the event record",
					"byAge", result.ByAge, "byCount", result.ByCount, "held", result.Held)
			}
			if result.Held > 0 {
				logger.Warn("the event record is holding events a subscription has not taken",
					"held", result.Held)
			}
		}
	}
}

// checkStartup refuses a configuration that would fail later and confusingly.
func checkStartup(o opts, log *slog.Logger) error {
	// A TTL below the minimum lets the mounted token expire before the kubelet replaces
	// it, and the symptom appears later as a broker rejecting an agent for no visible
	// reason.
	if o.tokenTTL > 0 && o.tokenTTL < tenant.MinimumTokenTTL {
		return fmt.Errorf("token TTL %s is below the %s minimum: the kubelet refreshes a mounted Secret on its own schedule, so a shorter margin expires the token before the agent sees the new one", o.tokenTTL, tenant.MinimumTokenTTL)
	}

	if o.brokerImage == "" || o.agentImage == "" {
		// Worth saying out loud: without images the boundary is provisioned but no
		// runtime or broker is, so a workspace reaches RUNNING with nothing serving its
		// endpoint. That is the intended behaviour while §8.6 is unresolved, and it is
		// confusing if it happens silently.
		log.Warn("image refs not configured; the broker and root agent Deployments will be omitted",
			"brokerImage", o.brokerImage, "agentImage", o.agentImage)
	}
	return nil
}

func run(o opts, log *slog.Logger) error {
	cfg, err := kubeConfig(o.kubeconfig)
	if err != nil {
		return fmt.Errorf("kubernetes config: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("kubernetes client: %w", err)
	}

	st, events, closeStores, err := openStores(o)
	if err != nil {
		return err
	}
	defer closeStores()

	if err := checkStartup(o, log); err != nil {
		return err
	}

	// Built before the controller, because the controller publishes the key into every
	// tenant namespace as part of rendering a workspace.
	verifier, pubPEM, err := newVerifier(o, log)
	if err != nil {
		return err
	}

	prov := provisioner.New(dyn, provisioner.DefaultFieldManager)
	ctrl := controller.New(st, prov, controller.Config{
		BrokerImage:           o.brokerImage,
		AgentImage:            o.agentImage,
		DeletionTimeout:       o.deletionTimeout,
		DriftInterval:         o.driftInterval,
		TokenTTL:              o.tokenTTL,
		AssertionPublicKeyPEM: pubPEM,
		BrokerTLS:             o.brokerTLS,
	}, o.interval, log)

	srv := api.New(st, api.Config{
		Verifier:       verifier,
		HostnameSuffix: o.hostSuffix,
		Events:         events,
		// The provisioner already reads and writes the token-key Secrets, and it is
		// the only thing that knows their names and their namespace.
		TokenKeys: prov,
	}, log)

	ctx, stop := startBackground(log, events)
	defer stop()

	errCh := make(chan error, 2)
	go func() { errCh <- ctrl.Run(ctx) }()
	go serveHTTP(ctx, o.addr, srv, errCh)

	log.Info("control plane started", "addr", o.addr, "db", o.dbPath, "interval", o.interval.String())

	first := <-errCh
	stop()
	if second := <-errCh; second != nil && !isShutdown(second) {
		log.Error("shutdown", "err", second)
	}
	if isShutdown(first) {
		return nil
	}
	return first
}

// serveHTTP runs the API until ctx is cancelled, then shuts it down gracefully.
func serveHTTP(ctx context.Context, addr string, handler http.Handler, errCh chan<- error) {
	httpSrv := &http.Server{
		Addr:    addr,
		Handler: handler,
		// A stalled client must not hold a connection open indefinitely, and every
		// request here is a small JSON blob.
		ReadHeaderTimeout: 10 * time.Second,
	}
	//nolint:gosec // the shutdown context is deliberately detached, see below
	go func() {
		<-ctx.Done()
		// Deliberately NOT derived from ctx: ctx is already cancelled by the time
		// this runs, so a child of it would be cancelled too and shutdown would
		// have no grace period at all. A fresh context is the point, not an
		// oversight -- which is why G118 is excluded here rather than satisfied.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()
	errCh <- httpSrv.ListenAndServe()
}

func isShutdown(err error) bool {
	return err == nil || errors.Is(err, http.ErrServerClosed) || errors.Is(err, context.Canceled)
}

// newVerifier loads the public key town signs assertions with and builds the
// verifier.
//
// Done at startup rather than on the first request: a control plane that cannot
// verify an assertion can serve no API call at all, so failing now beats starting
// and rejecting everything.
func newVerifier(o opts, log *slog.Logger) (*auth.Verifier, string, error) {
	// Both halves are needed. The parsed key verifies; the PEM is PUBLISHED into every
	// tenant namespace, because the bridge there verifies town for itself and this is
	// the only process that may distribute it -- town holds no Kubernetes credential.
	pub, pubPEM, err := auth.LoadPublicKeyPEM(o.assertionKey)
	if err != nil {
		return nil, "", fmt.Errorf("assertion public key: %w", err)
	}
	verifier, err := auth.NewVerifier(pub, o.assertionIssuer, o.assertionAud, 0)
	if err != nil {
		return nil, "", fmt.Errorf("assertion verifier: %w", err)
	}
	log.Info("assertion verification enabled",
		"key", o.assertionKey, "issuer", o.assertionIssuer, "audience", o.assertionAud)
	return verifier, pubPEM, nil
}

// kubeConfig prefers the in-cluster configuration, so the same binary runs inside
// the cluster and on a dev host without a flag.
func kubeConfig(path string) (*rest.Config, error) {
	if path != "" {
		return clientcmd.BuildConfigFromFlags("", path)
	}
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	return clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
}
