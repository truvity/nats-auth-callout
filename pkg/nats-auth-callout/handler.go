package natsauthcallout

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"
	authv1 "k8s.io/api/authentication/v1"
)

const (
	// maxUserTTL caps issued user JWTs regardless of the SA token's validity.
	maxUserTTL = time.Hour
	// tokenReviewTimeout bounds each TokenReview attempt. The broker's
	// authorization timeout is the hard deadline for the WHOLE decision, so
	// attempts are short and retried rather than long and single-shot.
	tokenReviewTimeout = 800 * time.Millisecond
	// tokenReviewRetries is how many additional attempts follow a failed
	// TokenReview call (transient apiserver blips, connection resets).
	tokenReviewRetries = 2
	// tokenReviewBackoff separates the attempts.
	tokenReviewBackoff = 150 * time.Millisecond
	// reviewCacheTTL bounds how long a successful authentication is reused
	// without a fresh TokenReview. Keeps reconnect storms off the apiserver
	// and rides short API outages; capped by the SA token's own expiry.
	reviewCacheTTL = time.Minute
	// cacheSweepThreshold: inserts sweep expired entries once the cache
	// reaches this size — lookups only evict their own key, so unique
	// short-lived tokens would otherwise accumulate forever.
	cacheSweepThreshold = 256
	// cacheMaxEntries hard-caps the cache; beyond it inserts are skipped
	// (the client still authenticates — it just isn't cached).
	cacheMaxEntries = 4096
)

type (
	// TokenReviewer validates a bearer token and reports who it belongs to.
	// The Kubernetes implementation lives in kube.go; tests use a fake.
	TokenReviewer interface {
		Review(ctx context.Context, token string, audiences []string) (*authv1.TokenReviewStatus, error)
	}

	// Handler answers NATS authorization requests: it validates the
	// client's ServiceAccount token, maps the namespace to a NATS account,
	// and returns a signed authorization response.
	Handler struct {
		cfg      *Config
		reviewer TokenReviewer
		issuer   nkeys.KeyPair
		logger   *slog.Logger
		// reviewTimeout bounds each TokenReview call; defaults to
		// tokenReviewTimeout and is injectable for tests.
		reviewTimeout time.Duration
		// reviewBackoff separates retry attempts; injectable for tests.
		reviewBackoff time.Duration
		// now is injectable for deterministic expiry tests.
		now func() time.Time

		// cacheMu guards cache: token digest → verified identity. Entries
		// let reconnect storms skip the apiserver and ride short TokenReview
		// outages (a client that authenticated within reviewCacheTTL keeps
		// reconnecting even if the API is briefly down).
		cacheMu sync.Mutex
		cache   map[[sha256.Size]byte]cachedReview
	}

	// cachedReview is a verified token identity with its reuse deadline.
	cachedReview struct {
		namespace string
		name      string
		expires   time.Time
	}
)

// NewHandler builds a Handler with the real clock and the default
// TokenReview timeout.
func NewHandler(cfg *Config, reviewer TokenReviewer, issuer nkeys.KeyPair, logger *slog.Logger) *Handler {
	return &Handler{
		cfg:           cfg,
		reviewer:      reviewer,
		issuer:        issuer,
		logger:        logger,
		reviewTimeout: tokenReviewTimeout,
		reviewBackoff: tokenReviewBackoff,
		now:           time.Now,
		cache:         make(map[[sha256.Size]byte]cachedReview),
	}
}

// Handle processes one $SYS.REQ.USER.AUTH request payload and returns the
// signed authorization response JWT to send back. A deny decision is NOT a
// Go error — it is a signed response with the Error field set. A Go error
// means no response could be produced at all (undecodable request).
func (h *Handler) Handle(ctx context.Context, payload []byte) ([]byte, error) {
	req, err := jwt.DecodeAuthorizationRequestClaims(string(payload))
	if err != nil {
		return nil, fmt.Errorf("decode authorization request: %w", err)
	}

	userJWT, denyReason := h.authorize(ctx, req)

	rc := jwt.NewAuthorizationResponseClaims(req.UserNkey)
	rc.Audience = req.Server.ID

	if denyReason != "" {
		rc.Error = denyReason
	} else {
		rc.Jwt = userJWT
	}

	response, err := rc.Encode(h.issuer)
	if err != nil {
		return nil, fmt.Errorf("encode authorization response: %w", err)
	}

	return []byte(response), nil
}

// authorize runs the full decision: token → TokenReview → account mapping →
// user JWT. It returns either a signed user JWT or a deny reason, and logs
// exactly one line per decision. The token itself is never logged.
func (h *Handler) authorize(ctx context.Context, req *jwt.AuthorizationRequestClaims) (userJWT, denyReason string) {
	token := req.ConnectOptions.Token
	if token == "" {
		token = req.ConnectOptions.Password
	}

	if token == "" {
		return "", h.deny(ctx, "", "", "no auth token or password presented")
	}

	namespace, name, denyReason := h.identity(ctx, token)
	if denyReason != "" {
		return "", denyReason
	}

	account, err := AccountForNamespace(namespace, h.cfg.ProjectAccounts)
	if err != nil {
		return "", h.deny(ctx, namespace, name, err.Error())
	}

	uc := jwt.NewUserClaims(req.UserNkey)
	uc.Name = namespace + "/" + name
	uc.Audience = account
	uc.Expires = h.userExpiry(token).Unix()
	// v1: full access within the mapped account; isolation comes from the
	// account boundary itself.
	uc.Pub.Allow.Add(">")
	uc.Sub.Allow.Add(">")

	encoded, err := uc.Encode(h.issuer)
	if err != nil {
		h.logger.ErrorContext(ctx, "user jwt encoding failed",
			slog.Any("error", err),
		)

		return "", h.deny(ctx, namespace, name, "internal error issuing user jwt")
	}

	h.logger.InfoContext(ctx, "auth decision",
		slog.Bool("allow", true),
		slog.String("namespace", namespace),
		slog.String("serviceaccount", name),
		slog.String("account", account),
	)

	return encoded, ""
}

// identity resolves the token to a verified (namespace, serviceaccount)
// pair: cache first, then TokenReview with short retried attempts. A
// non-empty denyReason means the caller must deny (already logged).
func (h *Handler) identity(ctx context.Context, token string) (namespace, name, denyReason string) {
	digest := sha256.Sum256([]byte(token))

	if ns, sa, ok := h.cachedIdentity(digest); ok {
		return ns, sa, ""
	}

	status, err := h.reviewWithRetry(ctx, token)
	if err != nil {
		h.logger.ErrorContext(ctx, "token review call failed",
			slog.Any("error", err),
		)

		return "", "", h.deny(ctx, "", "", "token review unavailable")
	}

	if !status.Authenticated {
		reason := "token not authenticated"
		if status.Error != "" {
			reason = "token not authenticated: " + status.Error
		}

		return "", "", h.deny(ctx, "", "", reason)
	}

	namespace, name, err = ParseServiceAccount(status.User.Username)
	if err != nil {
		return "", "", h.deny(ctx, "", "", err.Error())
	}

	h.storeIdentity(digest, token, namespace, name)

	return namespace, name, ""
}

// reviewWithRetry runs TokenReview attempts with short timeouts and
// backoff — transient apiserver blips must not become client denials. The
// broker's authorization timeout is the outer deadline, so the worst case
// (attempts × timeout + backoffs) is kept under ~2s.
func (h *Handler) reviewWithRetry(ctx context.Context, token string) (*authv1.TokenReviewStatus, error) {
	var lastErr error

	for attempt := 0; attempt <= tokenReviewRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("authorization deadline: %w", lastErr)
			case <-time.After(h.reviewBackoff):
			}
		}

		reviewCtx, cancel := context.WithTimeout(ctx, h.reviewTimeout)
		status, err := h.reviewer.Review(reviewCtx, token, h.cfg.Audiences)

		cancel()

		if err == nil {
			return status, nil
		}

		lastErr = err
	}

	return nil, fmt.Errorf("after %d attempts: %w", tokenReviewRetries+1, lastErr)
}

// cachedIdentity returns a still-valid cached verification for the token
// digest, evicting expired entries lazily.
func (h *Handler) cachedIdentity(digest [sha256.Size]byte) (namespace, name string, ok bool) {
	h.cacheMu.Lock()
	defer h.cacheMu.Unlock()

	entry, found := h.cache[digest]
	if !found {
		return "", "", false
	}

	if h.now().After(entry.expires) {
		delete(h.cache, digest)

		return "", "", false
	}

	return entry.namespace, entry.name, true
}

// storeIdentity caches a verified token for reviewCacheTTL, capped at the
// token's own expiry (a token the apiserver would reject as expired must
// never be served from cache). Inserts keep the cache bounded: expired
// entries are swept past cacheSweepThreshold, and cacheMaxEntries is a
// hard cap (skipping the insert only costs one future TokenReview).
func (h *Handler) storeIdentity(digest [sha256.Size]byte, token, namespace, name string) {
	expires := h.now().Add(reviewCacheTTL)
	if exp, ok := tokenExpiry(token); ok && exp.Before(expires) {
		expires = exp
	}

	h.cacheMu.Lock()
	defer h.cacheMu.Unlock()

	if len(h.cache) >= cacheSweepThreshold {
		now := h.now()
		for key, entry := range h.cache {
			if now.After(entry.expires) {
				delete(h.cache, key)
			}
		}
	}

	if len(h.cache) >= cacheMaxEntries {
		return
	}

	h.cache[digest] = cachedReview{namespace: namespace, name: name, expires: expires}
}

// deny logs the decision line and passes the reason through.
func (h *Handler) deny(ctx context.Context, namespace, name, reason string) string {
	h.logger.InfoContext(ctx, "auth decision",
		slog.Bool("allow", false),
		slog.String("namespace", namespace),
		slog.String("serviceaccount", name),
		slog.String("reason", reason),
	)

	return reason
}

// userExpiry aligns the issued user JWT's lifetime with the presented SA
// token's own expiry, capped at maxUserTTL. The exp claim is read without
// signature verification — validity was already established by TokenReview;
// this is only a lifetime hint.
func (h *Handler) userExpiry(token string) time.Time {
	capped := h.now().Add(maxUserTTL)

	exp, ok := tokenExpiry(token)
	if !ok || exp.After(capped) {
		return capped
	}

	return exp
}

// tokenExpiry extracts the exp claim from a JWT-shaped token, if present.
func tokenExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}

	var claims struct {
		Exp int64 `json:"exp"`
	}

	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp <= 0 {
		return time.Time{}, false
	}

	return time.Unix(claims.Exp, 0), true
}
