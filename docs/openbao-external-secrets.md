# Deploying with OpenBAO (or Vault) and External Secrets

How to run the responder when the issuer seed lives in OpenBAO (or
HashiCorp Vault — same API, same manifests) and reaches the cluster
through [External Secrets Operator](https://external-secrets.io/) (ESO),
and the ordering that makes the broker's auth-callout flip safe.

Every host, mount and role name below is a placeholder; the namespace
and Secret names are the chart's defaults from
[`values.yaml`](../charts/nats-auth-callout/values.yaml).

## 1. The one secret

The responder needs exactly one secret: the issuer nkey **account**
seed (`SA…`). It signs every authorization response and, re-encoded as
a user nkey, is also the responder's own broker login (see
[Broker login](../pkg/nats-auth-callout/README.md#broker-login)).

Keep it at one KV v2 path with one key, `seed`:

```
secret/nats/auth-callout/issuer   →   { "seed": "SA…" }
```

### Minting

`nk` (from [nats-io/nkeys](https://github.com/nats-io/nkeys)) mints the
seed; the two public keys the broker needs derive from it:

```sh
# The secret. Goes straight into KV, nowhere else.
nk -gen account            # → SA…

# auth_callout.issuer — the account public key.
nk -inkey seed.txt -pubout # → A…
```

The third value, the `U…` user key for `auth_callout.auth_users`, is the
same key material with a user prefix — `nk` has no flag for that
re-encoding, so derive both public forms in one step with the nkeys
library (this is what the responder does internally):

```go
package main

import (
	"fmt"
	"os"

	"github.com/nats-io/nkeys"
)

func main() {
	seed := []byte(os.Args[1]) // SA…
	kp, err := nkeys.FromSeed(seed)
	if err != nil {
		panic(err)
	}
	issuer, _ := kp.PublicKey() // A…
	_, raw, _ := nkeys.DecodeSeed(seed)
	user, _ := nkeys.FromRawSeed(nkeys.PrefixByteUser, raw)
	authUser, _ := user.PublicKey() // U…
	fmt.Println("issuer:   ", issuer)
	fmt.Println("auth_user:", authUser)
}
```

Both public keys are safe to commit: they go into the broker's config.

Mint by **automation**, not by hand: a Pulumi or Terraform apply (for
example `vault_kv_secret_v2` / the OpenBAO provider) that generates the
seed in-process, writes it to the KV path and exports only the two
public keys as outputs. The seed then never exists on a laptop, in a
shell history or in a ticket — the secret manager holds the only copy,
and a re-apply is the rotation procedure ([section 4](#4-rotation)).

## 2. Delivery through External Secrets

### Secret-manager side: a read-only role for one path

A policy that can read that path and nothing else, bound to ESO's
ServiceAccount through the Kubernetes auth method (a JWT auth role bound
to the same ServiceAccount and audience works identically; OpenBAO and
Vault accept either):

```hcl
# policy: nats-auth-callout-read
path "secret/data/nats/auth-callout/issuer" {
  capabilities = ["read"]
}
```

```sh
bao auth enable kubernetes           # once per cluster
bao write auth/kubernetes/role/nats-auth-callout \
  bound_service_account_names=external-secrets \
  bound_service_account_namespaces=external-secrets \
  policies=nats-auth-callout-read \
  ttl=1h
```

### Cluster side: store + ExternalSecret

The `ClusterSecretStore` describes where the secret manager is and how
ESO authenticates. Provider `vault` is what OpenBAO speaks:

```yaml
apiVersion: external-secrets.io/v1
kind: ClusterSecretStore
metadata:
  name: openbao
spec:
  provider:
    vault:
      server: https://openbao.example.internal:8200
      path: secret        # the KV v2 mount
      version: v2
      auth:
        kubernetes:
          mountPath: kubernetes
          role: nats-auth-callout
          serviceAccountRef:
            name: external-secrets
            namespace: external-secrets
```

The `ExternalSecret` lives in the responder's namespace and produces the
Secret the chart reads. `target.name` and `data[].secretKey` are the
chart's defaults (`issuerSecret.name: nats-auth-callout-issuer`,
`issuerSecret.key: seed`), so no chart values need to change:

```yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: nats-auth-callout-issuer
  namespace: nats
spec:
  refreshInterval: 1h
  secretStoreRef:
    kind: ClusterSecretStore
    name: openbao
  target:
    name: nats-auth-callout-issuer   # issuerSecret.name
    creationPolicy: Owner
  data:
    - secretKey: seed                # issuerSecret.key
      remoteRef:
        key: nats/auth-callout/issuer
        property: seed
```

`remoteRef.key` is the path under the mount; ESO inserts the KV v2
`/data/` segment itself. Once the ExternalSecret reports `Ready`, the
chart's Deployment has its `NATS_ISSUER_SEED` source and can start.

## 3. Ordering

1. Mint the seed (by automation).
2. Seed is in KV at `secret/nats/auth-callout/issuer`, key `seed`.
3. Policy + auth role + `ClusterSecretStore` exist and the store is
   `Ready`.
4. `ExternalSecret` materialises `nats-auth-callout-issuer` in the
   responder's namespace.
5. Install this chart (`natsURL`, `projectAccounts`, `tokenAudiences`).
6. **Only now** render the broker's `accounts` and `auth_callout`
   block with the two public keys from step 1 — in the upstream
   `nats/nats` chart that is `config.merge`:

   ```yaml
   config:
     merge:
       accounts:
         AUTH:
           users:
             - nkey: U…          # the responder's login
         my-namespace: {}        # one account per projectAccounts entry
       authorization:
         auth_callout:
           issuer: A…
           auth_users: [U…]
           account: AUTH
   ```

The rule behind the order: **flip auth-callout before any stream holds
data**, and put the responder in before the flip.

- A broker rendered with `auth_callout` and no reachable responder
  denies every client (including the ones writing to its streams).
- A broker without `auth_callout` is open to every pod that can reach
  it.

So the responder goes in first. Until the broker accepts its nkey
login the responder simply is not `Ready` — an initial connection
failure exits and the kubelet restarts it; once connected it reconnects
indefinitely — and it goes `Ready` the moment the broker resolves.
`/readyz` gates on the confirmed auth subscription, the broker
connection and a live TokenReview of the pod's own token (see
[Resilience](../pkg/nats-auth-callout/README.md#resilience-inf-401-incident-hardening)),
which is exactly what makes it a correct signal and an awkward one for
a health gate: if your deploy tool orders waves by health, **exempt the
responder from that gate** (`deploymentAnnotations` in `values.yaml`
exists for this) or the wave that flips the broker never starts and the
two wait on each other.

## 4. Rotation

1. Write a new seed to the KV path (re-run the minting automation).
2. ESO refreshes the Secret on its `refreshInterval` (or annotate the
   ExternalSecret with `force-sync` to hurry it).
3. Restart the responder (`kubectl rollout restart deployment
   nats-auth-callout`) — the seed is read once at start-up.
4. In the **same change**, update the broker's `issuer` and
   `auth_users` (the new `A…`/`U…`) and the `AUTH` account's user nkey.

Until step 4 lands, the broker refuses the responder's login and every
response it signs, so client authentication is down between 3 and 4.
Land 4 with 3 and keep the window to one rollout.

## 5. Troubleshooting

| Symptom | Cause | Check |
| --- | --- | --- |
| `authorization violation` for **every** client | The broker's `auth_callout.issuer` is not the public key of the seed the responder runs with (rotation half-landed, or a hand-minted seed that differs from the one in KV). The responder's own login fails too, so it is also not `Ready`. | Derive `A…`/`U…` from the Secret's `seed` and diff them against the broker config. |
| Responder never `Ready` (`/readyz` 503) | Broker unreachable or refusing the nkey login (block not rendered yet, wrong `natsURL`, NetworkPolicy) — or the self-TokenReview fails because `system:auth-delegator` is not bound / the apiserver is unreachable. | Pod logs: `failed to connect to nats` vs a TokenReview error. `kubectl auth can-i create tokenreviews --as=system:serviceaccount:nats:nats-auth-callout`. |
| Responder `Ready`, one client denied (`NACK`) | The client's ServiceAccount token was projected with an audience that is not in `tokenAudiences` (long-lived controller-minted tokens carry the apiserver URL as audience). | The responder's `auth decision` log line (`allow=false`, reason `token not authenticated: …audience…`); the token's `aud` claim; the chart's `tokenAudiences`. |
| Responder `Pending` on the Secret | The `ExternalSecret` is not `Ready` — store auth (role binding, policy path) or a wrong `remoteRef.key`. | `kubectl describe externalsecret nats-auth-callout-issuer -n nats`. |
