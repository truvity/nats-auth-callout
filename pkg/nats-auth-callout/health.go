package natsauthcallout

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
)

const (
	// selfTokenPath is where the pod's own ServiceAccount token is mounted.
	selfTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token" //nolint:gosec // path, not a credential
	// readyCacheTTL bounds how often /readyz performs a real TokenReview —
	// probes within the window reuse the last verdict.
	readyCacheTTL = 5 * time.Second
	// readyReviewTimeout bounds the self-review so a hung apiserver flips
	// readiness instead of hanging the probe.
	readyReviewTimeout = 3 * time.Second
	// healthReadTimeout hardens the tiny HTTP server.
	healthReadTimeout = 5 * time.Second
)

type (
	// HealthServer serves /healthz (liveness: process up) and /readyz
	// (readiness: NATS connected AND a real TokenReview of the pod's own
	// ServiceAccount token succeeds). A callout pod that cannot reach the
	// TokenReview API answers every client with "token review unavailable"
	// while looking perfectly Running — the INF-401 incident class. The
	// readiness probe turns that state into an unready pod: it drops from
	// the queue group's competition and rollouts halt with old pods serving.
	HealthServer struct {
		reviewer  TokenReviewer
		nc        *nats.Conn
		audiences []string
		logger    *slog.Logger

		// tokenPath is selfTokenPath in production; injectable for tests.
		tokenPath string

		// subscribed flips once the auth-callout queue subscription is
		// confirmed server-side (post-flush) — /readyz stays 503 until
		// then, so a rolling update never counts a pod as ready before it
		// can actually answer authorization requests.
		subscribed atomic.Bool

		mu          sync.Mutex
		lastCheck   time.Time
		lastVerdict error
		// now is injectable for deterministic cache tests.
		now func() time.Time
	}
)

// NewHealthServer wires the readiness dependencies.
func NewHealthServer(reviewer TokenReviewer, nc *nats.Conn, audiences []string, logger *slog.Logger) *HealthServer {
	return &HealthServer{
		reviewer:  reviewer,
		nc:        nc,
		audiences: audiences,
		logger:    logger,
		tokenPath: selfTokenPath,
		now:       time.Now,
	}
}

// Serve blocks running the health endpoints until ctx is done.
func (h *HealthServer) Serve(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", h.handleReady)

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: healthReadTimeout,
	}

	go func() {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), healthReadTimeout)
		defer cancel()

		_ = server.Shutdown(shutdownCtx)
	}()

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("health server: %w", err)
	}

	return nil
}

// handleReady answers the kubelet readiness probe.
func (h *HealthServer) handleReady(w http.ResponseWriter, r *http.Request) {
	if err := h.ready(r.Context()); err != nil {
		h.logger.WarnContext(r.Context(), "readiness check failed", slog.Any("error", err))
		http.Error(w, err.Error(), http.StatusServiceUnavailable)

		return
	}

	w.WriteHeader(http.StatusOK)
}

// MarkSubscribed records that the auth-callout subscription is confirmed
// server-side; called after the post-subscribe flush succeeds.
func (h *HealthServer) MarkSubscribed() {
	h.subscribed.Store(true)
}

// ready reports nil when every dependency works: the auth subscription is
// registered, the broker connection is up, and the TokenReview API answers
// for the pod's own token. The review verdict is cached for readyCacheTTL.
func (h *HealthServer) ready(ctx context.Context) error {
	if !h.subscribed.Load() {
		return fmt.Errorf("auth-callout subscription not yet confirmed")
	}

	if h.nc != nil && h.nc.Status() != nats.CONNECTED {
		return fmt.Errorf("nats connection status %s", h.nc.Status())
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.now().Sub(h.lastCheck) < readyCacheTTL {
		return h.lastVerdict
	}

	h.lastCheck = h.now()
	h.lastVerdict = h.selfReview(ctx)

	return h.lastVerdict
}

// selfReview exercises the exact dependency the auth path needs: a real
// TokenReview of this pod's own ServiceAccount token.
func (h *HealthServer) selfReview(ctx context.Context) error {
	token, err := os.ReadFile(h.tokenPath)
	if err != nil {
		return fmt.Errorf("read own serviceaccount token: %w", err)
	}

	reviewCtx, cancel := context.WithTimeout(ctx, readyReviewTimeout)
	defer cancel()

	// The pod's own token carries the apiserver's default audience, which
	// NATS_TOKEN_AUDIENCE includes; a nil audience list would also work but
	// using the configured set exercises the production code path.
	status, err := h.reviewer.Review(reviewCtx, string(token), h.audiences)
	if err != nil {
		return fmt.Errorf("self tokenreview: %w", err)
	}

	if !status.Authenticated {
		return fmt.Errorf("self tokenreview not authenticated: %s", status.Error)
	}

	return nil
}
