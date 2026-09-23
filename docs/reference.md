# Reference

Every chart value and every environment variable of the responder. For
the chart, `charts/nats-auth-callout/values.yaml` carries the same keys
as commented defaults and `values.schema.json` is the authority on
types: an unknown key fails the render. For the responder, the doc
comments in `pkg/nats-auth-callout/config.go` are the authority, and
[safety.md](safety.md) lists every refusal.

## charts/nats-auth-callout

One responder Deployment beside a NATS broker the estate already runs.
It renders a ServiceAccount, a ClusterRoleBinding to the built-in
`system:auth-delegator` ClusterRole, a Deployment and, when enabled, a
NetworkPolicy. Nothing else: no Secret, no Service, no broker
configuration.

### Values

| Value | Default | Type | Notes |
| --- | --- | --- | --- |
| `image.repository` | `ghcr.io/truvity/nats-auth-callout/responder` | string, non-empty | |
| `image.tag` | `""` | string | empty: the chart's `appVersion`, which the release workflow stamps to the release version, so the image and the chart are always the same version |
| `image.pullPolicy` | `IfNotPresent` | `IfNotPresent`, `Always` or `Never` | |
| `replicas` | `2` | integer, at least `1` | replicas are stateless and share the auth-callout queue group; while no replica is reachable the broker denies every non-AUTH login, so run two |
| `natsURL` | `""` | string, `nats://…` when set | the broker the responder serves, as `NATS_URL`; empty fails the render (`natsURL is required`), so the placeholder in `values.yaml` cannot install |
| `issuerSecret.name` | `nats-auth-callout-issuer` | string, non-empty | the Secret holding the issuer nkey account seed (`SA…`); the estate creates it |
| `issuerSecret.key` | `seed` | string, non-empty | the key in that Secret; it reaches the container as `NATS_ISSUER_SEED` from a `secretKeyRef` |
| `projectAccounts` | `[]` | list of strings | namespaces that map 1:1 to a NATS account of the same name; joined with commas into `NATS_PROJECT_ACCOUNTS` |
| `tokenAudiences` | `[nats]` | list of strings, at least one | the TokenReview audiences; a client's ServiceAccount token must be projected with one of them. Joined into `NATS_TOKEN_AUDIENCE`. See [audiences](#audiences) |
| `serviceAccount.annotations` | `{}` | map of strings | on the ServiceAccount; TokenReview itself needs no cloud identity |
| `deploymentAnnotations` | `{}` | map of strings | on the Deployment's metadata, for a GitOps health exemption while the broker and the responder resolve their start order (see [adoption.md](adoption.md#install-order)) |
| `nodeSelector` | `{}` | map of strings | |
| `tolerations` | `[{key: arch, operator: Exists}]` | list | passed through |
| `resources` | requests `cpu: 10m`, `memory: 32Mi`; limit `memory: 64Mi` | object | passed through |
| `networkPolicy.enabled` | `false` | boolean | renders the egress-only NetworkPolicy below |
| `networkPolicy.natsPodSelector` | `app.kubernetes.io/name: nats` | map of strings | pod labels of the broker, matched **in the responder's namespace only** (a podSelector without a namespaceSelector) |
| `networkPolicy.apiServerCIDRs` | `[]` | list of strings | CIDRs the API server is reached at, for TokenReview on 443/TCP |
| `networkPolicy.dnsCIDRs` | `[]` | list of strings | CIDRs the resolver is reached at, 53/UDP and 53/TCP |
| `networkPolicy.dnsNamespaceSelector` | `{}` | map of strings | labels of the namespace the resolver pods run in, as an alternative or addition to `dnsCIDRs` |

There is no `nameOverride`, `fullnameOverride`, `podAnnotations`,
`affinity` or `priorityClassName`: object names are fixed (next
section), and the pod spec carries what the table lists.

### What the chart fixes, not values

- **Names:** every object is named `nats-auth-callout`, and the
  ClusterRoleBinding `nats-auth-callout-auth-delegator-<namespace>`.
  The release name is not part of any name, so the chart installs once
  per namespace; a second namespace gets its own responder and its own
  binding.
- **Selector and labels:** `app.kubernetes.io/name: nats-auth-callout`
  on every object and as the pod selector.
- **ServiceAccount:** the pod runs as `nats-auth-callout` (always
  created), bound cluster-wide to `system:auth-delegator`, which is the
  grant TokenReview needs and nothing more.
- **Environment:** `NATS_URL`, `NATS_ISSUER_SEED` (from the Secret),
  `NATS_PROJECT_ACCOUNTS` and `NATS_TOKEN_AUDIENCE`. `NATS_AUTH_USER`,
  `NATS_AUTH_PASSWORD` and `NATS_HEALTH_ADDR` are not set by the chart.
- **Probes:** port `health` (8080/TCP). Readiness `GET /readyz` every
  10 s, unready after 3 failures; liveness `GET /healthz` every 30 s
  after a 10 s delay.
- **Security context:** non-root, user 65534, read-only root
  filesystem, `RuntimeDefault` seccomp, no privilege escalation, every
  capability dropped.
- **NetworkPolicy (when enabled):** egress only. Three rules: 4222/TCP
  to the pods `natsPodSelector` matches; 443/TCP to `apiServerCIDRs`;
  53/UDP and 53/TCP to `dnsNamespaceSelector` and `dnsCIDRs`. A rule
  whose destination list is empty matches every destination on that
  port, so the policy is only as tight as the CIDRs given to it; see
  [safety.md](safety.md#an-enabled-networkpolicy-with-no-cidrs-is-not-tighter).

## The responder

`ghcr.io/truvity/nats-auth-callout/responder` (from the Go module
`github.com/truvity/nats-auth-callout`, `cmd/responder`) is a static
binary on a distroless non-root base, built for `linux/amd64` and
`linux/arm64`; the same binary is attached to every GitHub Release as a
`tar.gz` for Linux and macOS on both architectures.

### Command line

There are no flags. Any argument other than `--help`, `-h` or `help`
prints the usage and exits 1; the three help forms print it and exit 0.
Everything is environment:

### Environment

| Variable | Required | Default | What it does |
| --- | --- | --- | --- |
| `NATS_URL` | no | `nats://127.0.0.1:4222` | the broker to connect to; a comma-separated list is accepted. Credentials in the URL are stripped before it is logged |
| `NATS_ISSUER_SEED` | yes | | the nkey **account** seed (`SA…`) that signs every authorization response and user JWT. Its public key (`A…`) is the broker's `auth_callout.issuer`. Anything that is not an account seed is refused at start |
| `NATS_AUTH_USER` | no | | with `NATS_AUTH_PASSWORD`, replaces the default nkey login with user/password for a broker that authenticates the AUTH account that way. Set both or neither |
| `NATS_AUTH_PASSWORD` | no | | pairs with `NATS_AUTH_USER` |
| `NATS_PROJECT_ACCOUNTS` | no | empty | comma-separated namespaces, each mapped to the NATS account of the same name; blanks are trimmed |
| `NATS_TOKEN_AUDIENCE` | yes | | comma-separated TokenReview audiences; a token must carry one of them. An empty list is refused at start, because Kubernetes reads an empty `audiences` as "the API server's own audiences", not "no restriction" |
| `NATS_HEALTH_ADDR` | no | `:8080` | the `/healthz` and `/readyz` listen address. If the listener cannot start the process exits: running without probes would disable the one safety net the probes are |

### The broker login

With `NATS_AUTH_USER` unset the responder logs in with an nkey
handshake. An nkey seed is a prefix byte plus the raw ed25519 seed, so
the account seed re-encodes as a user key whose public form (`U…`) the
broker lists in `auth_callout.auth_users`; the broker sends a nonce, the
responder signs it. One secret is both the signing key and the login.
[docs/openbao-external-secrets.md](openbao-external-secrets.md#minting)
derives both public keys from a seed.

### The decision

For each request on `$SYS.REQ.USER.AUTH` (queue group
`nats-auth-callout`, so replicas share the load):

1. The client's token is `connect_opts.auth_token`, or `password` when
   the token is empty. Neither is ever logged.
2. **TokenReview** with the configured audiences: attempts of 800 ms,
   retried twice with a 150 ms backoff, so a transient API-server error
   is not a denial and the worst case stays inside the broker's
   authorization timeout. A verified identity is cached by token digest
   for 60 s, capped at the token's own `exp`, so a reconnect storm does
   not fan out to the API server and an already-authenticated client
   rides a short outage. The cache sweeps expired entries at 256 and
   stops inserting at 4096; a client that is not cached is still
   authenticated.
3. The username `system:serviceaccount:<namespace>:<name>` is split;
   anything else is denied.
4. The namespace maps to an account (next section) or is denied.
5. The user JWT: name `<namespace>/<name>`, audience the account,
   `pub`/`sub` allow `>` (full access inside the account: the account
   boundary is the isolation), expiry the token's `exp` capped at one
   hour.

A denial is a signed response carrying the reason, not an error; one
JSON log line per decision (`auth decision`, `allow`, `namespace`,
`serviceaccount`, and `account` or `reason`).

### The mapping rule

Every accepted namespace maps to the account **of the same name**:

| Namespace | Accepted |
| --- | --- |
| listed in `NATS_PROJECT_ACCOUNTS` | yes |
| `emp-<slug>` with a non-empty slug | yes |
| `ci-<org>-<repo>`, at least two non-empty dash-separated parts after `ci-` | yes |
| exactly `ci` | yes (a transitional form; see [doctrine.md](doctrine.md#the-mapping-rule)) |
| anything else | denied: `namespace "<ns>" has no NATS account mapping` |

The broker's `accounts` block must carry an account for every namespace
the rule accepts, or the client is placed into an account the broker
does not have.

### Audiences

A ServiceAccount token is bound to the audiences it was projected with,
and TokenReview accepts it only for those. Project the client's token
with one of `tokenAudiences` (`nats` by default):

```yaml
volumes:
  - name: nats-token
    projected:
      sources:
        - serviceAccountToken:
            audience: nats
            expirationSeconds: 3600
            path: token
```

The list is for clients only. The responder's own pod token, which
`/readyz` reviews, is projected by the kubelet with the API server's
own audience (the first of its `--api-audiences`, by default its
issuer URL, `https://kubernetes.default.svc` on most distributions)
and no other, so the self-review submits it with **no** audience list:
Kubernetes then checks the token against the API server's own
audiences, which is how it was issued. `tokenAudiences: [nats]` alone
gives a `Ready` responder. A client token is never reviewed that way.

**Add the API server's audience only for long-lived token Secrets
minted by a controller.** Those carry the API server's own audience,
not `nats`, and that is the cluster's issuer URL, whichever it is: a
cluster whose issuer is its API server URL lists that URL, not
`kubernetes.default.svc`. See
[safety.md](safety.md#readyz-does-not-need-the-api-servers-audience).

### Health

| Endpoint | Answers 200 when |
| --- | --- |
| `/healthz` | the process is up. Liveness only |
| `/readyz` | the auth-callout subscription is confirmed by the broker (a flush after subscribing), the broker connection is `CONNECTED`, and a TokenReview of the pod's own ServiceAccount token succeeds. The self-review passes no audience list (the token carries the API server's audience, not the clients'), is cached for 5 s and bounded to 3 s. Otherwise 503 with the reason |

An initial connection failure exits the process (the kubelet restarts
it); once connected, the client reconnects indefinitely. On `SIGINT` or
`SIGTERM` the subscription drains; in-flight requests keep their own
10 s budget, detached from the shutdown, so draining never false-denies
a client.
