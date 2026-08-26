# nats-auth-callout

NATS auth-callout responder for Kubernetes: connecting clients present
their **ServiceAccount token**; the responder validates it via
TokenReview and answers the broker's `auth_callout` request with a
signed user JWT mapping the namespace to a NATS account. Workload
identity for NATS — no per-client credentials, no shared passwords.

## How it fits

Deploy this chart **beside** the upstream
[`nats/nats`](https://github.com/nats-io/k8s) chart, never instead of
it. The broker's `auth_callout` block names the issuer account and the
AUTH user (this responder's nkey); until that block is rendered the
responder idles harmlessly, so deploy order is free.

The one secret is the issuer nkey ACCOUNT seed (`SA…`): broker login +
response signing. Provision it however you custody seeds (ESO, sealed
secrets, …) and point `issuerSecret` at it.

## Install

```sh
helm install nats-auth-callout \
  oci://ghcr.io/truvity/charts/nats-auth-callout \
  --namespace nats \
  --set natsURL=nats://nats.nats.svc:4222 \
  --set 'projectAccounts[0]=my-namespace'
```

RBAC: the chart binds `system:auth-delegator` to its ServiceAccount —
that is all TokenReview needs; no cloud access is involved.

`tokenAudiences`: SA tokens must be projected with one of these
audiences. Include your API server's own audience if long-lived
controller-minted token Secrets must authenticate — those carry the
apiserver URL as audience, not `kubernetes.default.svc`.

`/readyz` exercises the real dependency chain (auth subscription +
broker connection + a live TokenReview of the pod's own token), so a
responder that cannot authorize clients goes unready instead of
denying everything while looking Running.

## Releases

One `v*` tag releases the image
(`ghcr.io/truvity/nats-auth-callout/responder`) and the chart
(`ghcr.io/truvity/charts/nats-auth-callout`) at the same version.
