# Doctrine — the design rules

## Workload identity, not credentials

A NATS client in the cluster already has an identity Kubernetes issues,
signs and rotates: its ServiceAccount token. Handing it a second one, a
NATS user credential, means a second thing to mint per workload, to
store, to rotate and to leak. So there are **no per-client
credentials**. The client presents the token it already has; the broker
asks this responder; the responder asks the API server whether the token
is real and whose it is; and the answer is a user JWT the broker trusts
for that one session. Nothing is distributed, and revoking a
ServiceAccount revokes its NATS access at the next reconnect.

The corollary is that a client can authenticate to NATS only from
inside the cluster whose API server can review its token. That is the
intended boundary, not a limitation to work around.

## TokenReview is the only identity check

The responder never parses a client token for who it belongs to. It
submits the token to TokenReview with the configured audiences and
takes the username the API server returns. The token's `exp` claim is
read locally, but only after the API server has accepted the token, and
only to cap the issued user's lifetime and the cache entry. There is no
second verifier, no JWKS fetch, no issuer allow-list: the API server is
the authority on its own tokens, and the responder's one grant,
`system:auth-delegator`, is exactly the permission to ask it.

The audience list is required for the same reason. Kubernetes reads an
empty `audiences` as "the API server's own", which would silently
accept every token in the cluster; an explicit audience means a token
projected for NATS is good for NATS and nothing else.

## One secret

The responder holds one secret: the issuer nkey account seed. It signs
every authorization response and every user JWT (its public key is the
broker's `auth_callout.issuer`), and, re-encoded as a user key, it is
also the responder's own login to the broker (its user-form public key
is in `auth_callout.auth_users`). An nkey seed is a prefix byte over the
same ed25519 material, so the two forms are one key, not two.

Why not a separate login credential: it would be a second secret with
the same custody, the same rotation and the same blast radius as the
first, and nothing would be gained by keeping them apart. The broker
trusts the responder because it signs with the issuer's key; letting it
prove the same key at connect time is the same trust, once. Rotation is
therefore one seed, two derived public keys, and one broker change.

The chart takes the **name** of the Secret and the key in it, and
mounts nothing: the seed reaches the container as a `secretKeyRef`. It
does not know which tool wrote the Secret, and no secret store, cloud
SDK or secret manager is a dependency of this repository.

## Namespace and account, one to one

Every accepted namespace maps to the NATS account **of the same name**.
There is no table from namespace to account, no shared account for a
group of namespaces, no account a namespace can choose. A namespace is
the tenancy boundary Kubernetes already enforces (RBAC, network policy,
quotas), and the NATS account is the tenancy boundary NATS enforces
(subjects, streams, permissions); making them the same name makes them
the same boundary, and a stream's owner is readable from its account
name without a lookup.

Inside the account the issued user is allowed everything (`>` on
publish and subscribe). Isolation comes from the account, not from
per-user permission lists, because a permission list is one more thing
that would have to be minted per workload and would drift from what the
workload does.

### The mapping rule

The rule accepts a namespace listed in `projectAccounts` and two
built-in prefix conventions, `emp-<slug>` and `ci-<org>-<repo>`, plus
the bare `ci` that the per-repository form replaces; the two prefixes
are a convention of the estate this was built for and are kept in the
code so that a namespace created by that convention needs no chart
change. The `ci-` form demands both parts non-empty so that a stray
`ci-scratch` does not acquire an account. A consuming estate with a
different convention lists its namespaces in `projectAccounts`; a
change to the built-in prefixes is a breaking change for anyone who
relies on them and is versioned as one.

The broker's `accounts` block is rendered by the estate from the same
list, so that every namespace the rule accepts is an account the broker
has. The responder does not check that; it cannot, and a check that
lives in two places is one that drifts.

## Readiness is the real chain

A responder that is up, connected and unable to reach TokenReview
denies every client with a reason nobody is looking at. So `/readyz`
does the thing the auth path does: the subscription confirmed by the
broker, the connection open, a TokenReview of the pod's own token. It is
expensive relative to a probe that returns 200 (one API call every five
seconds per replica) and that is the point: a probe that does less than
the work cannot fail when the work fails. A replica that is not ready
leaves the queue group, and a rollout that produces unready replicas
halts with the old ones serving. `/healthz` is liveness only, so a slow
start is seen rather than restarted.

## Beside the broker, never instead of it

This chart renders the responder and its RBAC, and nothing of the
broker: not its accounts, not its `auth_callout` block, not a Secret.
The broker is the estate's, usually the upstream chart, and the two
public keys the block needs are outputs of the estate's own seed
minting. The responder therefore idles harmlessly beside a broker that
has not been flipped, and the flip is a change the estate makes, in its
own repository, when the responder is in place. The one thing this
repository says about that change is its order
([adoption.md](adoption.md#install-order)).

## What the invisible failure earns

A rule is **enforced** rather than documented when the failure it
prevents does not show. The empty audience list is the example: it
would accept every token in the cluster and log an allow for each, so
the responder refuses to start without one. A wrong seed kind would sign
responses the broker rejects with no log line on this side, so the seed
is checked for its kind. A render-time refusal in the chart, likewise,
is a schema rule with a fixture that must fail.

## Ownership contract

| This repository | The consuming estate |
| --- | --- |
| the responder: TokenReview, the mapping rule, the signed response, retries and caching | the broker, its `accounts` and `auth_callout` block, and the flip |
| the Deployment, ServiceAccount and `system:auth-delegator` binding; the optional egress policy | the issuer seed, where it lives, how it reaches the namespace as a Secret, and its rotation |
| that no responder starts without a seed of the right kind and an explicit audience | the list of namespaces, and that each has its account in the broker |
| the `/readyz` chain | the clients' token projection and audience; the health-gate exemption in its deploy tool |
| object names and the pod selector, stable across releases | network policy CIDRs, scheduling, and the namespace |

## Rules a change must keep

- **A particular is an input.** A broker URL, a namespace, a secret path
  or a cluster in a default is a leak; `hack/leak-canary.sh` catches the
  shapes it can.
- **A new capability renders nothing until asked for.** An existing
  values file renders byte-for-byte the same, unless the release says
  otherwise (see [adoption.md](adoption.md#the-zero-diff-gate)).
- **Names are a contract.** A changed object name, selector, environment
  variable or built-in namespace prefix is a major version.
- **A new refusal comes with its fixture** in
  `tests/invalid/nats-auth-callout/`, or with its test in the package.
- **The client token is never logged,** and neither is the seed; a log
  line carries the decision, the identity and the reason.
