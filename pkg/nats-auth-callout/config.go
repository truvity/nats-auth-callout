// Package natsauthcallout implements a NATS auth-callout responder
// (INF-387, capability containers).
//
// A shared NATS broker delegates client authentication to this service
// (config-mode auth_callout). Connecting clients present their Kubernetes
// ServiceAccount token as the NATS auth token; the service validates it via
// the TokenReview API and answers with a signed user JWT that places the
// client into the right NATS account, giving per-project isolation on one
// broker without distributing NATS credentials.
//
// Mapping rule (v2 — uniform across tenancy tiers, every tenant
// namespace gets a DEDICATED account of the same name):
//   - namespace listed in NATS_PROJECT_ACCOUNTS       → account = namespace
//   - namespace "employee-{slug}" or exactly "ci"     → account = namespace
//   - anything else                                    → rejected
package natsauthcallout

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/nats-io/nkeys"
)

// Environment variable names and defaults.
const (
	// envURL is the NATS server URL. Default: nats://127.0.0.1:4222.
	envURL = "NATS_URL"
	// envUser is an OPTIONAL user/password override for the service's own
	// broker login. Default (both unset): the service logs in with the
	// USER-nkey re-encoding of the issuer seed — the broker pins that
	// public key in auth_callout.auth_users, so no extra credential exists.
	envUser = "NATS_AUTH_USER"
	// envPassword pairs with envUser; set both or neither.
	envPassword = "NATS_AUTH_PASSWORD" //nolint:gosec // env var name, not a credential
	// envIssuerSeed is the nkey ACCOUNT seed ("SA...") that signs
	// authorization responses; must match auth_callout.issuer (required).
	envIssuerSeed = "NATS_ISSUER_SEED"
	// envProjectAccounts is the comma-separated allowed-projects list;
	// each listed namespace maps to the NATS account of the same name.
	envProjectAccounts = "NATS_PROJECT_ACCOUNTS"
	// envTokenAudience is the comma-separated TokenReview audience list
	// (required). Kubernetes interprets an EMPTY audiences list as
	// "validate against the API server's own audiences" — not "no
	// restriction" — so the contract is explicit: SA tokens must be
	// projected with one of these audiences.
	envTokenAudience = "NATS_TOKEN_AUDIENCE" //nolint:gosec // env var name, not a credential
	// envHealthAddr is the listen address for /healthz + /readyz.
	// Default: :8080.
	envHealthAddr = "NATS_HEALTH_ADDR"

	defaultURL = "nats://127.0.0.1:4222"

	// defaultHealthAddr serves the kubelet probes.
	defaultHealthAddr = ":8080"
)

// Config holds the runtime configuration, loaded from the environment.
type (
	Config struct {
		// URL is the NATS server to connect to. Env: NATS_URL.
		URL string

		// User optionally overrides the broker login with user/password
		// auth (see envUser). Empty: nkey login derived from IssuerSeed.
		// Env: NATS_AUTH_USER.
		User string

		// Password pairs with User. Env: NATS_AUTH_PASSWORD.
		Password string

		// IssuerSeed is the account nkey seed ("SA...") used to sign
		// authorization responses and user JWTs. Env: NATS_ISSUER_SEED.
		IssuerSeed string

		// ProjectAccounts is the set of namespaces that map 1:1 to a NATS
		// account of the same name. Env: NATS_PROJECT_ACCOUNTS (comma-sep).
		ProjectAccounts map[string]struct{}

		// Audiences restricts TokenReview to these audiences (required —
		// see envTokenAudience). Env: NATS_TOKEN_AUDIENCE (comma-sep).
		Audiences []string

		// HealthAddr is the /healthz + /readyz listen address.
		// Env: NATS_HEALTH_ADDR.
		HealthAddr string
	}
)

// NewConfigFromEnv builds a Config from environment variables.
func NewConfigFromEnv() (*Config, error) {
	cfg := &Config{
		URL:             envOrDefault(envURL, defaultURL),
		User:            os.Getenv(envUser),
		Password:        os.Getenv(envPassword),
		IssuerSeed:      os.Getenv(envIssuerSeed),
		ProjectAccounts: parseSet(os.Getenv(envProjectAccounts)),
		Audiences:       parseList(os.Getenv(envTokenAudience)),
		HealthAddr:      envOrDefault(envHealthAddr, defaultHealthAddr),
	}

	if (cfg.User == "") != (cfg.Password == "") {
		return nil, fmt.Errorf("%s and %s must be set together or not at all", envUser, envPassword)
	}

	if cfg.IssuerSeed == "" {
		return nil, fmt.Errorf("%s is required", envIssuerSeed)
	}

	if len(cfg.Audiences) == 0 {
		return nil, fmt.Errorf(
			"%s is required: an empty TokenReview audiences list means \"validate against the API server's own audiences\", not \"no restriction\"",
			envTokenAudience)
	}

	return cfg, nil
}

// IssuerKeyPair parses and validates the issuer seed as an ACCOUNT nkey.
func (c *Config) IssuerKeyPair() (nkeys.KeyPair, error) {
	kp, err := nkeys.FromSeed([]byte(c.IssuerSeed))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", envIssuerSeed, err)
	}

	pub, err := kp.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("derive issuer public key: %w", err)
	}

	if !nkeys.IsValidPublicAccountKey(pub) {
		return nil, errors.New(envIssuerSeed + " is not an account seed (want \"SA...\")")
	}

	return kp, nil
}

// parseList splits a comma-separated list, trimming blanks.
func parseList(raw string) []string {
	var items []string

	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}

	return items
}

// parseSet is parseList as a membership set.
func parseSet(raw string) map[string]struct{} {
	set := map[string]struct{}{}
	for _, item := range parseList(raw) {
		set[item] = struct{}{}
	}

	return set
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return fallback
}
