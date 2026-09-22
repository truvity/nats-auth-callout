# Safety — what can break, and what this repository does about it

An auth callout fails in two opposite directions and both look fine
from a distance. A broker configured to ask a responder that is not
there denies every client while reporting itself healthy; a broker not
configured to ask is open to every pod that can reach it. So the
mistakes this repository can see are refused before anything runs: in
the chart at render time, in the responder at start-up, and in the
readiness probe once it runs.

## The chart: refused at render time

Every value the chart defines is checked by `values.schema.json`, which
Helm applies on every `lint`, `template`, `install` and `upgrade`, and
so does a GitOps controller that renders the chart. One rule is a
render-time `fail` in the template. Each rule has a fixture under
`tests/invalid/nats-auth-callout/` that must fail to render, and the
lint recipe proves it; a rule without a fixture is a rule that will
quietly stop working.

| Refusal | What it prevents |
| --- | --- |
| an unknown top-level key (`additionalProperties: false`) | a typo read as "use the default": `replica: 3` next to `replicas`, and the release runs two while the values file says three |
| an unknown key under `image` | `image.version` set while the pinned `image.tag` runs |
| `image.repository` empty | a pod with no image |
| `image.pullPolicy` outside `IfNotPresent`, `Always`, `Never` | a Deployment the API server rejects after the rest of the release applied |
| `replicas` not an integer, or below `1` | a scale-to-zero written into values: with no responder reachable the broker denies every non-AUTH login, and `replicas: 0` would render as a deliberate outage |
| `natsURL` non-empty and not `nats://…` | a URL the client cannot dial, found at start-up instead of at review |
| `natsURL` empty (the template's `required`) | an install of the placeholder in `values.yaml`: the chart ships `""` so that `helm lint` passes on a checkout, and the template refuses it so that it never reaches a cluster |
| an unknown key under `issuerSecret` | `issuerSecret.namespace`, which the chart has no field for: a `secretKeyRef` cannot cross namespaces |
| `issuerSecret.name` or `issuerSecret.key` empty | a `secretKeyRef` the API server rejects, or one that names a key that is not there, after the rest applied |
| `projectAccounts` entries that are not strings | a number joined into `NATS_PROJECT_ACCOUNTS` as a namespace name |
| `tokenAudiences` empty | the responder refusing to start, one step later, for the reason under [the responder](#the-responder-refused-at-start-up) |
| an unknown key under `serviceAccount` | `serviceAccount.name` or `serviceAccount.create`, silently ignored: the ServiceAccount is always `nats-auth-callout` |
| `serviceAccount.annotations`, `deploymentAnnotations` or `nodeSelector` with a non-string value | an annotation or selector the API server rejects |
| an unknown key under `networkPolicy` | `networkPolicy.ingress`, which the policy has no rule for; it is egress-only |
| `networkPolicy.enabled` not a boolean | `enabled: "false"`, which Go templates treat as true |
| `networkPolicy.natsPodSelector` or `dnsNamespaceSelector` with a non-string value; `apiServerCIDRs` or `dnsCIDRs` not a list of strings | a selector or `ipBlock` the API server rejects |

Strictness stops where Kubernetes' own fields begin: `resources` and
`tolerations` are passed through unchecked, and the API server
validates them.

## The responder: refused at start-up

| Refusal | What it prevents |
| --- | --- |
| `NATS_ISSUER_SEED` unset, unparseable, or not an **account** seed (`SA…`) | responses signed with a key the broker does not trust: every client denied with `authorization violation`, and no log line that says why. A user seed (`SU…`) parses and would sign, so the kind is checked, not only the syntax |
| `NATS_TOKEN_AUDIENCE` empty | an empty TokenReview `audiences` list, which Kubernetes reads as "validate against the API server's own audiences", not "no restriction". The responder would then accept tokens that were never projected for it |
| `NATS_AUTH_USER` and `NATS_AUTH_PASSWORD` set one without the other | a login with an empty password, or a password with no user, that fails at connect with a message about the broker rather than the configuration |
| the health listener cannot bind | a responder with no probes: the readiness gate below is the mechanism that keeps a broken responder out of rotation, so running without it is worse than not running |
| the first connection to the broker fails | a process that reports itself alive while it has never served. It exits and the kubelet restarts it; once connected it reconnects forever |

## Refused per request

A denial is a signed response with the reason, not an error, and each
one is logged (`auth decision`, `allow=false`, `reason`):

- no token and no password presented;
- TokenReview unavailable after three attempts (the reason is
  `token review unavailable`, and `/readyz` will be failing too);
- token not authenticated, with the API server's own reason appended
  (typically an audience the token was not projected with);
- a username that is not `system:serviceaccount:<namespace>:<name>`;
- a namespace the [mapping rule](reference.md#the-mapping-rule) does not
  accept.

The token is never logged, and `NATS_URL` is logged with its userinfo
stripped.

## Defaults chosen because the other one failed

**`/readyz` exercises the real chain.** A responder whose TokenReview
calls fail answers every client with `token review unavailable` while
its process is up, its connection is open and its pod reads `Running`.
A consuming estate met exactly this: a callout that could not reach the
API server, denying everything, with nothing unhealthy to look at. So
readiness is not "the process is up". It is the auth subscription
confirmed by the broker, the connection `CONNECTED`, and a TokenReview
of the pod's own token that succeeds. An unready replica leaves the
queue group's competition, and a rollout halts with the old pods still
serving. `/healthz` stays liveness only, so a slow start is visible
without being restarted.

**Readiness waits for the broker's confirmation.** The subscription is
flushed before `/readyz` can go green; a rolling update therefore never
counts a pod as ready that the broker cannot yet route a request to.

**TokenReview is retried in short attempts, not one long one.** The
broker's authorization timeout is the hard deadline for the whole
decision. Three attempts of 800 ms with 150 ms between them stay under
it and absorb a connection reset; one attempt with a long timeout would
turn every blip into a denial.

**Verified identities are cached for a minute, capped at the token's
expiry.** A reconnect storm (a broker restart, a network flap) sends
every client back at once; without the cache each one is a TokenReview
call, and the API server becomes the thing that decides whether NATS
recovers. A token the API server would reject as expired is never
served from cache.

**The issued user expires with the token, capped at one hour.** A
client's NATS session cannot outlive the identity it presented by more
than an hour, and a revoked ServiceAccount stops connecting at the next
reconnect rather than never.

**Two replicas.** They are stateless and share a queue group. During
the window in which no replica is reachable the broker denies every
non-AUTH login, so a single replica turns each node drain into an
outage for every NATS client.

**The seed arrives as a `secretKeyRef`, never as a value.** The chart
takes the name of a Secret and the key in it; the seed is in no values
file, no rendered manifest and no `ps` output.

**One secret.** The login is derived from the signing seed rather than
being a second credential, so there is nothing to rotate out of step
and no user/password pair to keep somewhere. See
[doctrine.md](doctrine.md#one-secret).

## Traps worth knowing

### The adoption order: a broker asks before anyone answers

The broker's `auth_callout` block and this chart are applied by
different tools, and the order matters in both directions:

- A broker rendered with `auth_callout` **and no reachable responder**
  denies every login that is not the AUTH account's own. Every client,
  including the ones writing to its streams, is refused until a
  responder is ready.
- A broker **without** `auth_callout` accepts whatever its other
  configuration accepts; with no other authentication it is open to
  every pod that can reach it.

So the responder goes in first, the broker's block last, and the flip
happens before any stream holds data a client would miss. Until the
block is rendered the responder is not `Ready` (the broker refuses its
login, and it restarts until it is accepted), which is correct and is
also awkward for a deploy tool that orders waves by health: exempt the
responder from that gate (`deploymentAnnotations` exists for this) or
the wave that flips the broker never starts.
[adoption.md](adoption.md#install-order) has the full order.

### A half-landed rotation denies everyone

The broker pins the seed's two public keys (`issuer`, `auth_users`).
After a new seed reaches the Secret and the responder restarts, the
broker refuses both the responder's login and every response it signs
until its configuration carries the new keys. Land the broker change in
the same rollout as the restart;
[openbao-external-secrets.md](openbao-external-secrets.md#4-rotation)
has the sequence.

### The seed is read once

The responder reads `NATS_ISSUER_SEED` at start. A Secret refreshed in
place does nothing until the pods restart.

### An enabled NetworkPolicy with no CIDRs is not tighter

The API server and the resolver are reached at Service ClusterIPs,
which no pod selector covers, so the policy takes CIDRs for them. A
rule whose destination list renders **empty** matches **every**
destination on its port: `networkPolicy.enabled: true` with
`apiServerCIDRs: []` permits egress to anywhere on 443/TCP, and with
neither `dnsCIDRs` nor `dnsNamespaceSelector` to anywhere on 53. The
policy still blocks everything else, so the responder keeps working,
but it is looser than it reads. Supply the cluster's CIDRs, or leave
the policy disabled and write one the estate owns.

### The NetworkPolicy reaches the broker in one namespace only

`natsPodSelector` is a pod selector with no namespace selector, so it
matches broker pods in the responder's namespace. A broker in another
namespace is unreachable the moment the policy is enabled: the
responder never connects, `/readyz` stays 503, and the log says
`failed to connect to nats`.

### One install per namespace

Every object is named `nats-auth-callout`, whatever the release is
called. Two releases in one namespace fight over the same Deployment;
two namespaces are two responders, each with its own ClusterRoleBinding
(named with the namespace so the two do not collide).

### A namespace the rule accepts must be an account the broker has

The responder places a client into the account named after its
namespace; it does not check that the broker has one. A namespace
listed in `projectAccounts` without a matching entry in the broker's
`accounts` block authenticates and then fails inside the broker. Render
both from one list.

### The audience is the whole contract

A client whose token was projected with an audience not in
`tokenAudiences` is denied with `token not authenticated: …audience…`,
the responder stays `Ready`, and every other client works. Controller-
minted long-lived token Secrets carry the API server's URL as their
audience, not `nats`; see [reference.md](reference.md#audiences).

### `tokenAudiences` needs the API server's audience for `/readyz`

`/readyz` reviews the pod's own ServiceAccount token with the configured
audiences, and the kubelet projects that token with the API server's
own audience (`https://kubernetes.default.svc` on most distributions)
and no other. TokenReview intersects the token's audiences with the
requested ones, so with the default `[nats]` alone the self-review is
refused (`self tokenreview not authenticated: … token audiences … is
invalid for the target audiences ["nats"]`) and the responder is never
`Ready`, while it would authorize clients correctly. Add the API
server's audience to `tokenAudiences`; the README's worked example
does. Nothing else in the chain fails, so this one is found by reading
the probe's 503 body.

### `/readyz` needs the pod's own token and the delegator grant

The self-review reads the pod's projected ServiceAccount token and
submits a TokenReview. A pod with `automountServiceAccountToken: false`,
or a ServiceAccount without the `system:auth-delegator` binding (the
chart always renders it, but a hand-edited copy may not), is never
`Ready`, for the same reason it could never authorize a client.
