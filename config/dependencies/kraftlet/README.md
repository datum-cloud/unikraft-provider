# kraftlet — per-host virtual-kubelet for the Unikraft runtime

Kraftlet is the vendor's virtual-kubelet: pods scheduled onto a kraftlet node
become Unikraft microVMs ("instances") created through the UKC API. This
directory deploys kraftlet as a **DaemonSet — one kraftlet per ukpd runtime
host**, each driving its node's **local** ukpd, so every runtime host appears
as its own schedulable virtual-kubelet node (`kraftlet-<node>`).

## Why this diverges from the vendor Helm chart

The vendor chart runs a **single** kraftlet StatefulSet that is **per-metro**:
it points at one UKC metro and presents that whole metro as one virtual node.
That does not fit our self-hosted, minimal-profile runtime, where each node's
ukpd is an independent single-host "metro" reachable only on its own loopback.

Two hard constraints in kraftlet **0.4.0** (the latest chart) shaped this design
(all verified on hardware, us-central-1-lab):

1. **The UKC endpoint is hard-coded** to `https://api.<metro>.unikraft.cloud`
   (HTTPS, :443). There is no endpoint/base-URL flag, and the embedded KraftKit
   `UKC_METRO` env is ignored by kraftlet's instance client. You **cannot** point
   kraftlet at the raw `http://127.0.0.1:45232` ukpd API.
2. **`--ukc-allow-insecure` / `KRAFTLET_ALLOW_INSECURE` is inert** — kraftlet
   still verifies the server certificate (self-signed → `x509: unknown
   authority`).

ukpd itself cannot help: it serves **plain HTTP** on `127.0.0.1:45232` and has
**no TLS option** (its API listeners are plain `address:port`; TLS lived in the
vendor OpenResty edge, which the minimal profile drops).

### The bridge

Each pod therefore co-locates a tiny **nginx TLS terminator** with kraftlet:

```
kraftlet --> https://api.local.unikraft.cloud (pinned to 127.0.0.1 by a
             pod hostAlias) --> nginx :443 (TLS) --> http://127.0.0.1:45232 (ukpd)
```

- Metro name is the constant `local`, so one CA + one server cert
  (`api.local.unikraft.cloud`) works fleet-wide; each node's bridge answers on
  its own loopback.
- kraftlet trusts the bridge's CA via Go's `SSL_CERT_FILE` (the robust
  substitute for the broken insecure flag).
- kraftlet serves on `:10251` (`:10250` is the node kubelet); `hostNetwork` is
  required to reach ukpd's loopback, hence the namespace's `privileged` Pod
  Security level.

## Secrets

| Secret | Managed by | Source |
| --- | --- | --- |
| `kraftlet-bridge-tls` | **cert-manager** (`certificates.yaml`) | self-signed Issuer → CA `Certificate` → CA Issuer → bridge leaf `Certificate`. The leaf secret carries `tls.crt`/`tls.key` (served by the nginx bridge) **and** `ca.crt` (mounted into kraftlet for `SSL_CERT_FILE`). |
| `kraftlet-ukc-token` | **committed TEST/DEV Secret** (`token-secret.yaml`); real clusters override via **ESO** in `datum-cloud/infra` | well-known token of a seeded ukpd `ci` user — non-sensitive, grants nothing on a real runtime; keeps this dir self-contained for `kustomize build` + the kind e2e. |

cert-manager is cluster-installed; `kubectl kustomize` does not validate its CRDs.

## Open items (before production)

- **PVC watcher is OFF.** N independent kraftlets would run N cluster-wide
  `ukc-volume` PVC watchers and double-provision volumes. A single-owner/leader
  design is needed before enabling PVC-backed volumes.
- **Real-cluster ukpd token.** The committed `kraftlet-ukc-token` is TEST/DEV
  only; real clusters supply it via the infra `ExternalSecret`
  (`gcp-secret-store`), whose backing GCP Secret Manager entry (the ukpd bearer
  token) must be provisioned before rollout. A dedicated non-root kraftlet user
  per ukpd is the follow-up.

Validated end-to-end on `ludum-lodaar`: the DaemonSet registers VK node
`kraftlet-ludum-lodaar`, kraftlet authenticates to the local ukpd through the
bridge (all calls HTTP 200, zero 401s), and instance create→start→delete works.
