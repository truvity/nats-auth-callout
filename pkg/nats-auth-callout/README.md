# nats-auth-callout

NATS auth-callout responder (INF-387, capability containers). The shared NATS broker delegates
client authentication here (config-mode `auth_callout`): clients present their Kubernetes
ServiceAccount token as the NATS auth token, the service validates it via TokenReview and answers
with a signed user JWT that places the client into the right NATS account — per-project isolation
on one broker, no NATS credentials to distribute.

## Environment

| Variable                                | Required | Meaning                                                                                  |
| --------------------------------------- | -------- | ---------------------------------------------------------------------------------------- |
| `NATS_URL`                              | no       | NATS server URL (default `nats://127.0.0.1:4222`)                                        |
| `NATS_AUTH_USER` / `NATS_AUTH_PASSWORD` | no       | optional user/password override for the service's own broker login (set both or neither) |
| `NATS_ISSUER_SEED`                      | yes      | account nkey seed (`SA...`) signing responses; must match `auth_callout.issuer`          |
| `NATS_PROJECT_ACCOUNTS`                 | no       | comma-separated namespaces mapped 1:1 to accounts (e.g. `url-shortener,billing`)         |
| `NATS_TOKEN_AUDIENCE`                   | yes      | comma-separated TokenReview audiences the SA tokens must carry                           |
| `NATS_HEALTH_ADDR`                      | no       | `/healthz` + `/readyz` listen address (default `:8080`)                                  |

Client ServiceAccount tokens must be [projected](https://kubernetes.io/docs/concepts/storage/projected-volumes/#serviceaccounttoken)
with one of the `NATS_TOKEN_AUDIENCE` audiences (e.g. `audience: nats`) — audience-bound tokens
can't be replayed against other services. The variable is required: Kubernetes interprets an
_empty_ TokenReview audiences list as "validate against the API server's own audiences", not
"no restriction", so an unset value would silently accept API-server-audience tokens.

## Broker login

By default the service logs in with an **nkey handshake derived from the issuer seed**: an nkey
seed is a prefix byte + raw ed25519 seed, so the account seed (`SA...`) re-encodes as a user key
whose public form (`U...`) the broker pins in `auth_callout.auth_users`. The broker sends a nonce,
the service signs it with the seed — one secret covers response signing _and_ login, and
`auth_users` membership exempts the service from the callout it serves. `NATS_AUTH_USER`/
`NATS_AUTH_PASSWORD` remain as an override for brokers using classic user/password auth.

## Mapping rule (v2)

Uniform across tenancy tiers — every tenant namespace maps to a DEDICATED account of the same
name: namespace listed in `NATS_PROJECT_ACCOUNTS` → account of the same name; `employee-{slug}`
(non-empty slug) or exactly `ci` → account of the same name; anything else is rejected (including
a bare `employee-` prefix). The broker's accounts block and the gitops tenants-stack Account CRs
render per namespace from the same cfg, so all three layers follow one rule. Issued users get
allow-all pub/sub inside their account, expiry = SA-token expiry capped at 1h. One structured log
line per decision; tokens are never logged.

## Resilience (INF-401 incident hardening)

- **TokenReview retry**: each review attempt is short (800ms) and retried
  twice with 150ms backoff — transient apiserver blips don't become client
  denials, and the worst case stays inside the broker's authorization
  timeout.
- **Decision cache**: successful authentications are reused for 60s (capped
  at the SA token's own expiry, keyed by token digest) — reconnect storms
  skip the apiserver, and already-authenticated clients ride short
  TokenReview outages.
- **`/readyz`**: performs a REAL TokenReview of the pod's own SA token
  (verdict cached 5s) and checks the broker connection. A pod that cannot
  authorize clients goes unready — it drops from the queue group and
  rollouts halt with old pods serving — instead of denying everything while
  looking Running (the INF-401 incident class). `/healthz` is liveness only.
