# Node Licensing

Unikraft licenses the runtime **per node**: every compute host must hold a
current node license certificate to run licensed platform features. A node earns
that certificate by **activating** itself against Unikraft's control plane, and
keeps it by renewing before it expires. This document describes how a Datum
compute node obtains and maintains its license, and which parts of the flow this
repository owns.

## How It Works

Two runtime components divide the work:

- The **host agent** (`agent`) owns the license. It requests the certificate,
  caches it on the node, renews it on a timer, and serves it to the platform
  daemon.
- The **platform daemon** (`ukpd`) consumes the license. It asks the agent for
  the certificate over the host-agent socket and enables licensed features when
  it accepts one.

Activation is a single call, `agent license activate`, which:

1. Builds a certificate signing request whose `machine_id` is read from
   `/etc/machine-id`.
2. Posts it to Unikraft's control plane, which returns a signed certificate.
3. Caches that certificate in the agent's PKI directory (`AGENT_PKI_PATH`,
   `/var/lib/ukp/pki`), which lives on the node's persistent data volume.

The running agent then hands the certificate to `ukpd` over the host-agent
socket, which happens within about a second of activation and needs no restart of
either component.

> [!IMPORTANT]
>
> Only `agent license activate` reads the activation token — `agent run` does
> not. A long-running agent renews a certificate it already has, but will never
> obtain a first one. That is why activation is a separate startup step rather
> than a property of the agent's configuration.

### Activation

A node activates from the **`activate-node` initContainer** in the `ukp-runtime`
DaemonSet, defined in the [real-cluster overlay](../../config/overlays/ukp-runtime).
The overlay carries it, not the base: the base is what the hermetic kind e2e
applies, and that environment has neither an activation token nor a route to the
vendor control plane.

The step is **idempotent**. It first reads the cached certificate's expiry with
`agent license status` and exits immediately if it is still valid, so the common
case — a pod restart or an image bump on a node that is already licensed — costs
one local command and no control-plane call. A node re-activates only when it has
no cached certificate, or when its certificate lapsed while the node was down
past the renewal window.

The step **fails closed**. A node that cannot obtain a license does not start its
runtime: the initContainer exits non-zero and the pod retries under kubelet
backoff, so a node never quietly serves guests unlicensed. Because that turns a
transient control-plane failure into a stopped runtime, activation is retried
in-script — five attempts, ten seconds apart — before the container gives up.

### Renewal

Renewal belongs to the agent, not to this repository. Certificates are valid for
**24 hours**, and the agent re-requests one on its own schedule:

| Setting | Value | Meaning |
|---------|-------|---------|
| `AGENT_LICENSE_CHECK_INTERVAL` | `4h` | How often the agent examines its certificate |
| `AGENT_LICENSE_RENEWAL_BUFFER` | `12h` | How long before expiry it renews |

Both are set in [`ukp.conf`](../../config/dependencies/ukp-runtime/ukp.conf). The
consequence is that the agent must keep running — a node whose agent is down
longer than the renewal buffer will lapse, and the next pod start re-activates it.

## Activation Token

The activation token is a single fleet-wide secret (`NODE_ACTIVATION_TOKEN`),
issued by Unikraft. It authenticates the activation request; it is **not** the
license, and it is not per node.

It reaches the runtime as configuration rather than through the pod spec. The
`ukp-runtime` DaemonSet mounts the `ukp-runtime-credentials` Secret at
`/etc/ukp-secrets`, and `ukp.conf` — which every runtime launcher sources with
`set -a` — sources `/etc/ukp-secrets/ukp.secrets.conf` from it. Any shell
assignment in that file therefore becomes an environment variable in the
launcher's environment, `NODE_ACTIVATION_TOKEN` included. No token is passed on a
command line, set as a container env var, or stored in this repository.

Populating that Secret is [`datum-cloud/infra`](https://github.com/datum-cloud/infra)'s
job: it syncs the token from GCP Secret Manager with an `ExternalSecret`. This
repository consumes the Secret and treats its absence as a fatal configuration
error. The division of ownership is deliberate — cluster secrets live with the
cluster, and the runtime manifests stay deployable from the published bundle.

> [!NOTE]
>
> Because activation fails closed, the Secret must exist in a cluster **before**
> this runtime rolls to it. A cluster that receives the runtime without the
> token holds every node's runtime in `Init:Error`.

## Machine ID Binding

Unikraft binds each activation to a machine ID it has **registered in advance**
for that node. The control plane rejects a request whose `machine_id` it does not
expect, so the token alone is not sufficient to license an arbitrary host.

That makes `/etc/machine-id` load-bearing in two places. The DaemonSet mounts the
**host's** machine-id file into the runtime containers, because the machine ID in
the request becomes the certificate's organizational unit and `ukpd` compares it
against the host's own before accepting the license. Activating against the
container image's baked machine ID would produce a certificate the runtime
refuses.

The operational consequence is that **a node's identity is tied to its OS
install**. Talos regenerates its machine ID on reinstall, so a repaved node
presents an ID Unikraft has not registered and cannot activate until the vendor
registers the new one. Under fail-closed activation, such a node will not start
its runtime — re-registration is a prerequisite for bringing a repaved node back
into service, not a follow-up.

## Observability

| Where | Signal | Meaning |
|-------|--------|---------|
| `ukpd` logs | `License status: active, expires: …, features: …` | The daemon accepted a license |
| `ukpd` logs | `No license available from host agent` (hourly) | The daemon is running unlicensed |
| `activate-node` logs | `licensed through <timestamp>` | Cached certificate valid; activation skipped |
| `activate-node` logs | `FATAL: …` | Activation failed; the pod will retry |
| `agent license status` | Certificate fields and `not_after` | The node's cached certificate |

## Troubleshooting

| Symptom | Cause | Resolution |
|---------|-------|------------|
| `401 … Invalid node activation token` | The token is wrong or was rotated | Update the token in GCP Secret Manager; ESO re-syncs the Secret |
| `400 … machine_id does not match the expected value` | Unikraft has not registered this node's machine ID, typically after a repave | Ask Unikraft to register the node's current `/etc/machine-id` |
| `activate-node` in `Init:Error`, `FATAL: no NODE_ACTIVATION_TOKEN` | The `ukp-runtime-credentials` Secret is missing or lacks the token | Confirm the `ExternalSecret` in `datum-cloud/infra` reconciled in this cluster |
| `ukpd` logs `License machine ID mismatch` | The certificate was issued for a different machine ID than the host's | Confirm the host machine-id mounts are present, then re-activate |

## Learn More

- [`config/overlays/ukp-runtime`](../../config/overlays/ukp-runtime) — the
  activation initContainer and its script
- [`config/dependencies/ukp-runtime/ukp.conf`](../../config/dependencies/ukp-runtime/ukp.conf)
  — runtime configuration, including the renewal settings and the credentials
  overlay chain
