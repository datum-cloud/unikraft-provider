# state-projector

A per-node sidecar that turns the Unikraft runtime's `vm.state_change` lifecycle
stream into **windowed, project-attributed usage records** for per-second
compute billing.

It is the runtime-side half of the compute per-second usage billing enhancement
(`compute/docs/enhancements/per-second-usage-billing/README.md`). The other half
is a Vector program in `datum-infra`
(`apps/billing-vector-agent/components/billing-usage-collector-unikraft`) that
tails the projector's output and emits billing Cloudevents to the billing
gateway.

## Why it is necessary

The vendor runtime (`ukpd`) is **single-user and knows nothing about Datum**. Its
`vm.state_change` event carries only a runtime instance `uuid` plus an old/new
lifecycle state — it has **no project, no instance name, no resource amounts, and
no elapsed seconds**:

```json
{"type":"vm.state_change","timestamp":"2026-08-15T12:03:47Z",
 "data":{"vm":"dda7fe99-387a-4d81-80f2-35e2b51ee5c5","prev":"running","new":"stopping"}}
```

A billing system cannot charge that opaque uuid to anyone, and it does not know
how long the instance consumed resources. Something on the node has to close that
gap:

- connect the runtime `uuid` to a Datum **project**,
- compute how long it was **actually running** (so idle/scale-to-zero bills
  nothing),
- and hand a clean, deduplicable record downstream.

That bridging — plus the stateful running-window accounting — is exactly what this
sidecar does. It deliberately does **not** live in Vector, because Vector's
transform language is stateless per event (it can't accumulate an open running
window) and has no clean way to maintain a live pod→project index. The split is:

| concern | owner | why |
|---|---|---|
| consume `vm.state_change` | state-projector | needs a node-local socket sink |
| enrich uuid → project / instance / resources | state-projector | needs k8s-aware, node-local logic |
| stateful running-window + duration | state-projector | Vector VRL is per-event / stateless |
| format + ship billing Cloudevent | Vector | pure stateless transport — its job |

## How it works

```
ukpd ──vm.state_change──▶ /var/run/ukp/vm-state.sock ──▶ state-projector
                                                        │  maintains open windows
                                                        │  appends JSONL
                                                        ▼
                                              /var/run/ukp/vm-state.usage
                                                        │  (hostPath)
                                                        ▼
                                  Vector file source → remap → billing_gateway
```

1. **Ingest.** ukpd streams `vm.state_change` events over a Unix socket to the
   projector (`-socket /var/run/ukp/vm-state.sock`). The sink is configured in the
   runtime's `log-sinks.json` (`vm-state-sink`, `type: socket`,
   `events: ["vm.state_change"]`, `reliable: true`).

   The two halves name that socket differently, and both are correct: ukpd's
   `log-sinks.json` says `/run/ukp/...` because its `debian:trixie-slim` image
   symlinks `/var/run` → `/run`, while the projector must say `/var/run/ukp/...`
   because `distroless/static` has **no such symlink** — there, `/run` and
   `/var/run` are separate real directories and only `/var/run/ukp` is the
   mounted hostPath. Both therefore resolve to the same file on the node. Leaving
   the projector on a `/run/ukp` path would put its socket and usage file in the
   container's ephemeral layer, where ukpd cannot connect and Vector sees
   nothing — with no error from either side. The DaemonSet passes all three paths
   explicitly for this reason.

2. **Attribution.** For each instance, the projector resolves:
   `uuid → guest IP` from the node-local `vmm.json`
   (`/var/lib/ukp/data/platform/<uuid>/vmm.json`, `netdev.ip=...`), then
   `guest IP → project / instance / vcpu / memory` from a **cluster-wide watch of
   provider Pods** (the guest IP equals the provider Pod's `podIP`; the Pod's
   namespace is the project, and its container limits are the requested
   resources). The pod→project index is kept in memory by the watch; no API calls
   happen per event.

3. **Windowing.** Only the `running` state counts as "on" (everything else —
   standby, stopped, stopping, terminated — bills nothing). While an instance is
   running, the projector keeps an open window (its `runningSince` /
   `reportedUntil` watermarks). On scale-to-zero, stop, or a **periodic flush**
   (default 5 minutes), it appends an incremental record covering the elapsed
   running time.

4. **Output.** One JSONL record per window is appended to
   `/var/run/ukp/vm-state.usage`:

   ```json
   {"id":"<md5(uuid|start|end)>","project":"<ns>","instance":"<name>","uuid":"...",
    "vcpu":N,"memory_bytes":M,"start":"...","end":"...","duration_s":N.N}
   ```

   Vector tails this file and wraps each line as a billing Cloudevent
   (`compute.datumapis.com/instance/usage`, `subject: projects/<project>`,
   `data: {vcpu_seconds, memory_byte_seconds}`).

### Data contract — input and output

- **Input** (`vm.state_change` from ukpd):
  `{timestamp, type: "vm.state_change", data: {vm, prev, new}}` — the parser also
  tolerates `uuid`/`old_state`/`new_state` variants for other builds and falls
  back to scanning for a uuid-shaped token.
- **Output** (usage record; one per window): the fields above. The `id` is a
  deterministic `md5(uuid|start|end)`, so replaying a window does not double-count
  billing downstream (the billing gateway/buffer dedups on it).

## Deployment

The projector is a container in the `ukp-runtime` DaemonSet
(`config/dependencies/ukp-runtime/daemonset.yaml`), so it runs **once per compute
node** and ships with the runtime. Deployment is scoped the same way the rest of
the runtime is:

- **Real clusters:** via the `unikraft-provider-kustomize` OCIRepository bundle →
  the `ukp-runtime` Flux Kustomization in `datum-infra`
  (`apps/unikraft-system/edge/ukp-runtime.yaml`, overlay
  `overlays/ukp-runtime`). Publishing the updated bundle rolls the sidecar onto
  every edge cluster automatically.
- **RBAC:** the sidecar only watches pods
  (`ClusterRole ukp-state-projector-pod-reader`: get/list/watch) — it never
  updates anything. See `state-projector-rbac.yaml`.
- **kind e2e:** the sidecar is built via the e2e workflow (repo-root context) and
  pinned to `:e2e` in `overlays/ukp-runtime-e2e`.

The Vector side ships separately in `datum-infra`
(`apps/billing-vector-agent/components/billing-usage-collector-unikraft`).

## Failure modes / guarantees

- **Projector down (transient):** the socket sink is `reliable: true` and
  disk-buffered on the ukpd side (`buffer_size_kb: 64`), so ukpd holds undelivered
  events and replays them on reconnect. Because events carry their original
  timestamps, full running windows are reconstructed from a replay — an outage
  does **not** mean events are lost.
- **Projector crash mid-window:** the open window's watermark lives in memory,
  so a crash after reading a `running` event but before flushing its record loses
  that in-flight segment — bounded to at most one flush interval (~5 min) of a
  running instance.
- **Prolonged outage on a busy node:** the 64 KB sender buffer can overflow at
  the edge if the projector stays down through heavy instance churn.

None of these require changes here to mitigate further, but they're the operational
envelope worth knowing.

## Logs

Every failure here is silent by nature: an event whose fields we cannot read
produces no record, which looks exactly like an idle node. So each stage logs a
greppable `tag key=value` line. Tags: `boot`, `podwatch`, `podindex`, `conn`,
`event`, `window`, `resolve`, `record`, `rotate`, `stats`. `-debug` adds per-event,
per-pod and per-resolution detail; everything below is on by default.

**Start with the `stats` heartbeat** (every `-stats-interval`, default 1m) — one
line that says whether anything is arriving and anything is coming out:

```
stats uptime=5m0s conns=1 events_received=42 events_wrong_type=0 \
  dropped_no_uuid=0 dropped_no_state=0 decode_errors=0 \
  windows_open=3 windows_opened=7 windows_closed=4 \
  records_written=19 write_errors=0 unresolved=0 indexed_pod_ips=12 \
  watch_errors=0 stale_events=0 overbilled=0 rotations=0 rotation_deletes=0
```

Reading it:

| symptom | meaning |
|---|---|
| `events_received=0` | ukpd never connected — check the `vm-state-sink` entry in `log-sinks.json` and that `conn listening` names the socket ukpd dials |
| `dropped_no_uuid` / `dropped_no_state` rising | the vendor renamed its event fields; each drop logs the raw payload verbatim, so grep `event dropped` and compare against `extractTransition` |
| `indexed_pod_ips=0` | the pod watch is empty — look for `podwatch error=` (usually the missing `ukp-state-projector-pod-reader` ClusterRole) |
| `unresolved` rising | attribution failing; `resolve failed` names which stage (`vmm_json_unreadable`, `netdev_ip_missing`, `pod_not_indexed`) |
| `records_written=0` with `windows_open>0` | instances are running but nothing is billed — check `record write_error` and the output path |
| `write_errors` rising | the output hostPath is not writable; the watermark is not advanced, so the next flush retries the span |
| `stale_events` / `overbilled` non-zero | see the time-base hazard below |

Two alerts are worth paging on, both about billing correctness:

- `window WARN=stale_open … skew=…` — a window was opened from a replayed event
  whose timestamp is well behind wall clock. Because flushes are computed against
  wall clock, the next flush bills the whole gap in one record.
- `window ALERT=overbilled … overbilled_s=…` — a wall-clock flush already billed
  past the point where the instance actually stopped. The number is how many
  seconds were over-billed, and the true close is clamped to zero duration.

A normal instance lifecycle reads:

```
window opened uuid=dda7fe99-… prev="starting" new="running" at=… open_windows=1
resolve ok uuid=dda7fe99-… project=my-project instance=web-1 vcpu_milli=2000 memory_bytes=2147483648
record written cause=flush id=75af2b… project=my-project … duration_s=300.0
window closed uuid=dda7fe99-… prev="running" new="stopping" total_s=612.4 records=3 open_windows=0
```

`record written` logs the emitted record in full — it is the line to diff
against what Vector actually shipped.

### Rotation

`vm-state.usage` lives on `ukp-run`, a hostPath `ukpd` also depends on — left
to grow forever, it can fill that volume and take the real runtime down with
it, not just billing. `-rotate-size-mb` (default `64`) renames the file once
it reaches that size; the next write recreates it fresh. Renaming, not
truncating, is what makes this safe for a consumer that already has the file
open: a rename only changes a directory entry, so an existing open handle
(Vector's, once it exists) keeps reading the same underlying data to
whatever became its final content, then separately picks up the new file by
re-globbing the directory.

Deleting a rotated file is properly **Vector's job**, once it exists — only
it knows a file was fully shipped downstream. `-rotate-max-age` (default
`48h`) is a disk-safety backstop for the gap before that's wired up, or if
Vector is ever down longer than the window — not the primary cleanup path.
It should rarely if ever fire in a healthy system; if `rotation_deletes` in
the `stats` heartbeat is climbing, something upstream (Vector, or the
`billing-usage-collector-unikraft` pipeline) is stuck or missing.

```
rotate done from=/var/run/ukp/vm-state.usage to=/var/run/ukp/vm-state.usage.1755600000 size_bytes=67108992 threshold_bytes=67108864
rotate deleted reason=age_backstop path=/var/run/ukp/vm-state.usage.1755400000 age=48h2m1s max_age=48h0m0s
```

## Verify it works

After an Instance has transitioned to `running` and back on a node:

```bash
# Sidecar received events / emitted windows
kubectl -n unikraft-system logs -l app=ukp-runtime -c state-projector --tail=100

# Health at a glance, and anything that went wrong
kubectl -n unikraft-system logs -l app=ukp-runtime -c state-projector | grep '^.*stats '  | tail -5
kubectl -n unikraft-system logs -l app=ukp-runtime -c state-projector \
  | grep -E 'ALERT|WARN|error=|dropped|write_error|resolve failed'

# The usage file it wrote (what Vector tails)
kubectl -n unikraft-system exec -it <ukp-runtime-pod> -c state-projector -- \
  tail -20 /var/run/ukp/vm-state.usage
```

Each line should be a well-formed usage record with the correct `project`,
`vcpu`, `memory_bytes` and `duration_s`. The `id` must be stable for the same
window (re-read to confirm no double-counting).

## Testing

`cmd/state-projector/main_test.go` covers:

- event-key parsing (including the real `vm`/`prev`/`new` vendor payload),
- the vendor payload driving windowing end-to-end (a parse that reads the states
  off the wrong keys emits nothing at all, silently — this is the guard against
  that),
- on/off transition → single windowed record with correct attribution and
  duration,
- a window closing on any non-running state even with no old-state field, and
  staying open when the new state is unparseable,
- incremental flush + deterministic ids,
- unresolved attribution failing toward `-` (emit rather than drop).

Run with:

```sh
cd /Users/joseszycho/unikraft-provider
go test ./cmd/state-projector/...
```
