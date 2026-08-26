package natsauthcallout

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
)

const (
	// authSubject is the NATS auth-callout request subject.
	authSubject = "$SYS.REQ.USER.AUTH"
	// queueGroup lets replicas share the auth-callout load.
	queueGroup = "nats-auth-callout"
	// flushTimeout bounds the post-subscribe flush that confirms the
	// subscription is registered server-side before we declare readiness.
	flushTimeout = 5 * time.Second
	// requestTimeout is the per-message processing budget. Each auth
	// request gets its own context (independent of the shutdown signal
	// context) so draining doesn't false-deny in-flight clients.
	requestTimeout = 10 * time.Second
	// redactedInvalid is logged in place of a NATS URL we cannot parse.
	redactedInvalid = "invalid"

	usage = `nats-auth-callout — NATS auth-callout responder (INF-387)

Validates connecting clients' Kubernetes ServiceAccount tokens via
TokenReview and places them into the right NATS account (per-project
isolation on a shared broker). Runs in-cluster; no CLI arguments.

Environment:
  NATS_URL                NATS server URL (default nats://127.0.0.1:4222)
  NATS_ISSUER_SEED        account nkey seed "SA..." signing responses
                          (required; must match auth_callout.issuer)
  NATS_AUTH_USER          optional user/password override for the broker
  NATS_AUTH_PASSWORD      login (set both or neither). Default: nkey login —
                          the issuer seed re-encoded as a USER key; list its
                          "U..." public form in auth_callout.auth_users
  NATS_PROJECT_ACCOUNTS   comma-separated namespaces mapped 1:1 to NATS
                          accounts (e.g. "url-shortener,billing")
  NATS_TOKEN_AUDIENCE     comma-separated TokenReview audiences (required;
                          client SA tokens must be projected with one of
                          these audiences)
  NATS_HEALTH_ADDR        /healthz + /readyz listen address (default :8080;
                          /readyz performs a real self-TokenReview)

Mapping rule (v2, uniform): every tenant namespace maps to a DEDICATED
account of the same name — listed project namespaces, employee-{slug},
and ci; anything else is rejected.
`
)

// Run is the entry point for the nats-auth-callout binary.
func Run(args []string, version, gitCommit string) int {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	if len(args) >= 2 {
		if args[1] == "--help" || args[1] == "-h" || args[1] == "help" {
			_, _ = fmt.Fprint(os.Stdout, usage)

			return 0
		}

		fmt.Fprint(os.Stderr, usage)

		return 1
	}

	cfg, err := NewConfigFromEnv()
	if err != nil {
		logger.ErrorContext(ctx, "failed to load config from environment", slog.Any("error", err))

		return 1
	}

	issuer, err := cfg.IssuerKeyPair()
	if err != nil {
		logger.ErrorContext(ctx, "failed to load issuer nkey", slog.Any("error", err))

		return 1
	}

	reviewer, err := NewInClusterTokenReviewer()
	if err != nil {
		logger.ErrorContext(ctx, "failed to build kubernetes token reviewer", slog.Any("error", err))

		return 1
	}

	logger.InfoContext(ctx, "nats-auth-callout starting",
		slog.String("version", version),
		slog.String("commit", gitCommit),
		slog.String("nats_url", redactedURL(cfg.URL)),
		slog.Int("project_accounts", len(cfg.ProjectAccounts)),
	)

	login, err := loginOption(cfg)
	if err != nil {
		logger.ErrorContext(ctx, "failed to build broker login", slog.Any("error", err))

		return 1
	}

	nc, err := nats.Connect(cfg.URL,
		nats.Name(queueGroup),
		login,
		nats.MaxReconnects(-1),
	)
	if err != nil {
		logger.ErrorContext(ctx, "failed to connect to nats", slog.Any("error", err))

		return 1
	}
	defer nc.Close()

	handler := NewHandler(cfg, reviewer, issuer, logger)

	// Health endpoints: /healthz is live from here on (slow startups stay
	// visible to the kubelet); /readyz additionally gates on the confirmed
	// auth subscription, the broker connection, and a real self-TokenReview
	// — a pod that cannot authorize clients must never look ready. A
	// listener failure is FATAL: running without probes would silently
	// disable exactly the safety net the probes exist for.
	health := NewHealthServer(reviewer, nc, cfg.Audiences, logger)

	go func() {
		if serveErr := health.Serve(ctx, cfg.HealthAddr); serveErr != nil {
			logger.ErrorContext(ctx, "health server failed — shutting down", slog.Any("error", serveErr))
			cancel()
		}
	}()

	sub, err := nc.QueueSubscribe(authSubject, queueGroup, func(msg *nats.Msg) {
		// Per-request context: WithoutCancel keeps the ROOT context's
		// values (slog/tracing convention: everything derives from the
		// signal.NotifyContext chain in Run) while detaching cancellation —
		// in-flight requests during drain must still succeed.
		msgCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), requestTimeout)
		defer cancel()

		response, handleErr := handler.Handle(msgCtx, msg.Data)
		if handleErr != nil {
			logger.ErrorContext(msgCtx, "failed to process authorization request", slog.Any("error", handleErr))

			return
		}

		if respondErr := msg.Respond(response); respondErr != nil {
			logger.ErrorContext(msgCtx, "failed to send authorization response", slog.Any("error", respondErr))
		}
	})
	if err != nil {
		logger.ErrorContext(ctx, "failed to subscribe to auth subject", slog.Any("error", err))

		return 1
	}

	// Confirm the subscription reached the server before declaring ready.
	if flushErr := nc.FlushTimeout(flushTimeout); flushErr != nil {
		logger.ErrorContext(ctx, "failed to flush subscription registration", slog.Any("error", flushErr))

		return 1
	}

	// Only now may /readyz go green — the subscription is server-confirmed.
	health.MarkSubscribed()

	logger.InfoContext(ctx, "listening for authorization requests", slog.String("subject", authSubject))

	<-ctx.Done()

	if drainErr := sub.Drain(); drainErr != nil {
		logger.ErrorContext(ctx, "failed to drain subscription", slog.Any("error", drainErr))
	}

	logger.InfoContext(ctx, "shutdown complete")

	return 0
}

// loginOption picks the service's own broker login: the user/password
// override when configured, otherwise an nkey login derived from the issuer
// seed (see userNkeyFromIssuerSeed).
func loginOption(cfg *Config) (nats.Option, error) {
	if cfg.User != "" {
		return nats.UserInfo(cfg.User, cfg.Password), nil
	}

	userKey, err := userNkeyFromIssuerSeed(cfg.IssuerSeed)
	if err != nil {
		return nil, err
	}

	pub, err := userKey.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("derive login user public key: %w", err)
	}

	return nats.Nkey(pub, userKey.Sign), nil
}

// userNkeyFromIssuerSeed re-encodes the issuer ACCOUNT seed ("SA...") as a
// USER nkey ("SU.../U..."). An nkey seed is prefix + raw ed25519 seed, so the
// same key material yields both forms; the broker pins the U-form public key
// in auth_callout.auth_users and the service proves possession by signing the
// server nonce — one secret covers signing AND login.
func userNkeyFromIssuerSeed(issuerSeed string) (nkeys.KeyPair, error) {
	issuer, err := nkeys.FromSeed([]byte(issuerSeed))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", envIssuerSeed, err)
	}

	seed, err := issuer.Seed()
	if err != nil {
		return nil, fmt.Errorf("extract issuer seed: %w", err)
	}

	_, rawSeed, err := nkeys.DecodeSeed(seed)
	if err != nil {
		return nil, fmt.Errorf("decode issuer seed: %w", err)
	}

	userKey, err := nkeys.FromRawSeed(nkeys.PrefixByteUser, rawSeed)
	if err != nil {
		return nil, fmt.Errorf("re-encode issuer seed as user nkey: %w", err)
	}

	return userKey, nil
}

// redactedURL renders a NATS URL (or comma-separated list) as
// scheme://host:port with any userinfo stripped, so credentials embedded in
// NATS_URL never reach the logs. Unparseable input yields "invalid".
func redactedURL(raw string) string {
	parts := strings.Split(raw, ",")
	for i, part := range parts {
		u, err := url.Parse(strings.TrimSpace(part))
		if err != nil || u.Scheme == "" || u.Host == "" {
			return redactedInvalid
		}

		parts[i] = u.Scheme + "://" + u.Host
	}

	return strings.Join(parts, ",")
}
