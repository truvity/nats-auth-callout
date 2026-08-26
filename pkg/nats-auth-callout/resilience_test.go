package natsauthcallout

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/jwt/v2"
	authv1 "k8s.io/api/authentication/v1"
)

type (
	// seqReviewer answers TokenReviews from a scripted sequence and counts calls.
	seqReviewer struct {
		responses []func() (*authv1.TokenReviewStatus, error)
		calls     int
	}
)

func (s *seqReviewer) Review(_ context.Context, _ string, _ []string) (*authv1.TokenReviewStatus, error) {
	idx := s.calls
	s.calls++

	if idx >= len(s.responses) {
		idx = len(s.responses) - 1
	}

	return s.responses[idx]()
}

func okReview(username string) func() (*authv1.TokenReviewStatus, error) {
	return func() (*authv1.TokenReviewStatus, error) { return authenticatedAs(username), nil }
}

func errReview() (*authv1.TokenReviewStatus, error) {
	return nil, errors.New("transient apiserver blip")
}

// resilienceEnv wires a handler around a scripted reviewer with fast retries.
func resilienceEnv(t *testing.T, seq *seqReviewer) *testEnv {
	t.Helper()

	env := newTestEnv(t, &fakeReviewer{})
	env.handler.reviewer = seq
	env.handler.reviewBackoff = time.Millisecond

	return env
}

func withToken(token string) func(*jwt.AuthorizationRequestClaims) {
	return func(req *jwt.AuthorizationRequestClaims) {
		req.ConnectOptions.Token = token
	}
}

func TestReviewRetriesTransientFailure(t *testing.T) {
	seq := &seqReviewer{responses: []func() (*authv1.TokenReviewStatus, error){
		func() (*authv1.TokenReviewStatus, error) { return errReview() },
		okReview("system:serviceaccount:url-shortener:nack"),
	}}
	env := resilienceEnv(t, seq)

	rc := env.respond(t, env.authRequest(t, withToken("token-a")))
	if rc.Error != "" {
		t.Fatalf("expected allow after retry, got deny %q", rc.Error)
	}

	if seq.calls != 2 {
		t.Errorf("reviewer calls = %d, want 2 (one failure + one retry)", seq.calls)
	}
}

func TestReviewRetryExhaustionDenies(t *testing.T) {
	seq := &seqReviewer{responses: []func() (*authv1.TokenReviewStatus, error){
		func() (*authv1.TokenReviewStatus, error) { return errReview() },
	}}
	env := resilienceEnv(t, seq)

	rc := env.respond(t, env.authRequest(t, withToken("token-a")))
	if rc.Error == "" {
		t.Fatal("expected deny when every review attempt fails")
	}

	if want := tokenReviewRetries + 1; seq.calls != want {
		t.Errorf("reviewer calls = %d, want %d", seq.calls, want)
	}
}

func TestReviewCacheSkipsSecondReview(t *testing.T) {
	seq := &seqReviewer{responses: []func() (*authv1.TokenReviewStatus, error){
		okReview("system:serviceaccount:url-shortener:nack"),
	}}
	env := resilienceEnv(t, seq)

	for i := 0; i < 2; i++ {
		rc := env.respond(t, env.authRequest(t, withToken("token-a")))
		if rc.Error != "" {
			t.Fatalf("request %d: expected allow, got deny %q", i, rc.Error)
		}
	}

	if seq.calls != 1 {
		t.Errorf("reviewer calls = %d, want 1 (second request served from cache)", seq.calls)
	}
}

func TestReviewCacheRidesReviewOutage(t *testing.T) {
	seq := &seqReviewer{responses: []func() (*authv1.TokenReviewStatus, error){
		okReview("system:serviceaccount:url-shortener:nack"),
		func() (*authv1.TokenReviewStatus, error) { return errReview() },
	}}
	env := resilienceEnv(t, seq)

	if rc := env.respond(t, env.authRequest(t, withToken("token-a"))); rc.Error != "" {
		t.Fatalf("warm-up request denied: %q", rc.Error)
	}

	// Reviewer now fails hard, but the cached identity keeps the client in.
	if rc := env.respond(t, env.authRequest(t, withToken("token-a"))); rc.Error != "" {
		t.Fatalf("cached request denied during review outage: %q", rc.Error)
	}
}

func TestReviewCacheExpires(t *testing.T) {
	seq := &seqReviewer{responses: []func() (*authv1.TokenReviewStatus, error){
		okReview("system:serviceaccount:url-shortener:nack"),
	}}
	env := resilienceEnv(t, seq)

	env.respond(t, env.authRequest(t, withToken("token-a")))

	// Advance the clock past the cache TTL.
	expired := env.now.Add(reviewCacheTTL + time.Second)
	env.handler.now = func() time.Time { return expired }

	env.respond(t, env.authRequest(t, withToken("token-a")))

	if seq.calls != 2 {
		t.Errorf("reviewer calls = %d, want 2 (cache expired → fresh review)", seq.calls)
	}
}

func TestHealthReadyz(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("self-token"), 0o600); err != nil {
		t.Fatal(err)
	}

	reviewer := &seqReviewer{responses: []func() (*authv1.TokenReviewStatus, error){
		okReview("system:serviceaccount:nats:nats-auth-callout"),
	}}

	// nc == nil skips the broker-status gate (unit scope).
	health := NewHealthServer(reviewer, nil, []string{"nats"}, slog.New(slog.DiscardHandler))
	health.tokenPath = tokenFile

	// Before the subscription is confirmed, readiness must refuse.
	preRec := httptest.NewRecorder()
	health.handleReady(preRec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil))

	if preRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz before MarkSubscribed = %d, want 503", preRec.Code)
	}

	if reviewer.calls != 0 {
		t.Fatalf("reviewer called %d times before subscription confirmed, want 0", reviewer.calls)
	}

	health.MarkSubscribed()

	rec := httptest.NewRecorder()
	health.handleReady(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("/readyz = %d (%s), want 200", rec.Code, rec.Body.String())
	}

	// Verdict is cached: a second probe within the TTL does not re-review.
	health.handleReady(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil))

	if reviewer.calls != 1 {
		t.Errorf("reviewer calls = %d, want 1 (cached verdict)", reviewer.calls)
	}
}

func TestHealthReadyzFailsWhenReviewFails(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("self-token"), 0o600); err != nil {
		t.Fatal(err)
	}

	reviewer := &seqReviewer{responses: []func() (*authv1.TokenReviewStatus, error){
		func() (*authv1.TokenReviewStatus, error) { return errReview() },
	}}

	health := NewHealthServer(reviewer, nil, []string{"nats"}, slog.New(slog.DiscardHandler))
	health.tokenPath = tokenFile
	health.MarkSubscribed()

	rec := httptest.NewRecorder()
	health.handleReady(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d, want 503 when TokenReview is down", rec.Code)
	}
}

func TestReviewCacheSweepBoundsGrowth(t *testing.T) {
	seq := &seqReviewer{responses: []func() (*authv1.TokenReviewStatus, error){
		okReview("system:serviceaccount:url-shortener:nack"),
	}}
	env := resilienceEnv(t, seq)

	// Fill past the sweep threshold with entries that are already expired
	// (zero-value expiry is in the past for the fixed test clock).
	for i := 0; i < cacheSweepThreshold+10; i++ {
		env.handler.cache[[32]byte{byte(i), byte(i >> 8)}] = cachedReview{expires: env.now.Add(-time.Second)}
	}

	// A fresh authentication triggers the insert-time sweep.
	env.respond(t, env.authRequest(t, withToken("token-sweep")))

	env.handler.cacheMu.Lock()
	size := len(env.handler.cache)
	env.handler.cacheMu.Unlock()

	if size != 1 {
		t.Errorf("cache size after sweep = %d, want 1 (expired entries pruned)", size)
	}
}
