# Adoption

## Prerequisites

- **A NATS broker the estate runs,** normally the upstream
  [`nats/nats`](https://github.com/nats-io/k8s) chart, with the
  responder reachable at its client port (4222) and its configuration
  under the estate's control: the `accounts` and `authorization`
  blocks are what the estate renders in the last step.
- **Kubernetes** with the TokenReview API (any supported version),
  projected ServiceAccount tokens for the clients, and Helm 3 with OCI
  registry support to pull the chart. The chart binds the built-in
  `system:auth-delegator` ClusterRole, so installing it needs
  permission to create a ClusterRoleBinding.
- **A place for secrets** the estate already runs (External Secrets,
  SOPS, or a hand-made Secret): the issuer seed has to arrive in the
  responder's namespace as a Secret, and nothing in this repository puts
  it there. [openbao-external-secrets.md](openbao-external-secrets.md)
  is the worked path for a KV-backed secret manager.
- **A namespace layout** in which each tenant namespace maps to a NATS
  account of the same name; see [the mapping
  rule](reference.md#the-mapping-rule).

## Install order

1. **Mint the issuer seed** (`nk -gen account`, or automation that
   generates it in-process) and keep it where the estate keeps secrets.
   Derive its two public keys, `A…` (the issuer) and `U…` (the
   responder's login); both are safe to commit.
   [openbao-external-secrets.md](openbao-external-secrets.md#minting)
   derives both.
2. **The seed Secret** in the responder's namespace, under the name and
   key the chart will read (`issuerSecret.name`, `issuerSecret.key`).
3. **This chart,** with `natsURL`, `projectAccounts` and
   `tokenAudiences`. The pods start, connect, are refused by the broker
   (its `auth_callout` block is not there yet) and restart until it is.
   They are not `Ready`, and must not be: a deploy tool that orders
   waves by health needs the responder exempted from that gate
   (`deploymentAnnotations`), or the next step never starts.
4. **The broker's `accounts` and `auth_callout` block,** only now, with
   the two public keys from step 1: an `AUTH` account whose user is the
   `U…` key, one account per namespace the mapping rule accepts, and
   `authorization.auth_callout` with `issuer: A…`, `auth_users: [U…]`,
   `account: AUTH`. The README's worked example has the block. The
   responder goes `Ready` the moment the broker accepts its login.
5. **The clients:** project their ServiceAccount tokens with an audience
   in `tokenAudiences` and present the token as the NATS auth token.

The rule behind the order: **flip auth-callout before any stream holds
data**, and put the responder in before the flip. A broker with the
block and no reachable responder denies every client; a broker without
it is open to every pod that can reach it.
[safety.md](safety.md#the-adoption-order-a-broker-asks-before-anyone-answers)
has both failures.

## The zero-diff gate

**A consumer adopts a release only when the render it produces is
byte-identical to what runs, or differs exactly by the change the
release announces** in [CHANGELOG.md](../CHANGELOG.md).

Render your values at the pinned version and at the new one and
compare:

```sh
helm template nats-auth-callout oci://ghcr.io/truvity/charts/nats-auth-callout \
  --version <pinned> --namespace nats -f values.yaml > old.yaml
helm template nats-auth-callout oci://ghcr.io/truvity/charts/nats-auth-callout \
  --version <new> --namespace nats -f values.yaml > new.yaml
diff old.yaml new.yaml
```

The image tag moves with every release (the chart's `appVersion` is
stamped), so the expected diff of a patch is that one line. Anything
else is either in the CHANGELOG or a reason to stop.

Moving from a hand-written Deployment to the chart is one change whose
render diff is empty (next section). Tightening something afterwards
(enabling the NetworkPolicy, narrowing `tokenAudiences`) is a separate
change, adopted on its own evidence.

## Adopting what already exists

### A responder already running from hand-written manifests

The chart's object names are fixed: `nats-auth-callout` for the
ServiceAccount and the Deployment,
`nats-auth-callout-auth-delegator-<namespace>` for the
ClusterRoleBinding, and the pod selector
`app.kubernetes.io/name: nats-auth-callout`. Render the chart with your
values and diff it against the live objects; if the names and the
selector match, Helm adopts them when they carry its ownership
metadata:

```sh
for kind in serviceaccount deployment; do
  kubectl -n nats label $kind nats-auth-callout app.kubernetes.io/managed-by=Helm
  kubectl -n nats annotate $kind nats-auth-callout \
    meta.helm.sh/release-name=nats-auth-callout meta.helm.sh/release-namespace=nats
done
kubectl label clusterrolebinding nats-auth-callout-auth-delegator-nats app.kubernetes.io/managed-by=Helm
kubectl annotate clusterrolebinding nats-auth-callout-auth-delegator-nats \
  meta.helm.sh/release-name=nats-auth-callout meta.helm.sh/release-namespace=nats
helm install nats-auth-callout oci://ghcr.io/truvity/charts/nats-auth-callout \
  --version <v> --namespace nats -f values.yaml
```

A GitOps controller that applies rendered manifests needs none of
this: the render is the gate, and the same names mean an in-place
update.

If the live selector differs from the chart's, the switch is a delete
and a create, not an update: a Deployment's selector is immutable.
Orphan the old pods (`kubectl delete deployment … --cascade=orphan`),
install the chart, and remove the orphaned ReplicaSet once the new pods
are `Ready`. Between the two, the broker has responders from both, which
is harmless: they share one queue group.

### A broker already flipped

An estate whose broker already carries an `auth_callout` block keeps it;
the chart never touches the broker. Confirm that `issuer` is the public
key of the seed in the Secret the chart will read, and that the `AUTH`
account's user is the `U…` form of the same seed; a mismatch denies
everyone from the first request
([safety.md](safety.md#a-half-landed-rotation-denies-everyone)).

## Upgrading

There has been no breaking release. Each version below that carried a
consumer-visible change lists the step it needs; a version absent from
[CHANGELOG.md](../CHANGELOG.md) changed dependencies only.

### v1.0.0 to v1.0.1: the schema admits the shipped placeholder

`values.schema.json` accepted only a non-empty `nats://` URL for
`natsURL`, so `helm lint` on a clean checkout failed on the chart's own
`""` placeholder. The rule is now "empty, or `nats://…`"; the template's
`required` still refuses an install with `natsURL` empty. A values file
that installed before installs unchanged.
