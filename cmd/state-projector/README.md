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
| consume `vm.state_change` | state-projector | needs to tail a node-local file |
| enrich uuid → project / instance / resources | state-projector | needs k8s-aware, node-local logic |
| stateful running-window + duration | state-projector | Vector VRL is per-event / stateless |
| format + ship billing Cloudevent | Vector | pure stateless transport — its job |

## How it works

```
ukpd ──vm.state_change──▶ /var/run/ukp/vm-state.events ──▶ state-projector (tails)
                                                          │  maintains open windows
                                                          │  appends JSONL
                                                          ▼
                                                /var/run/ukp/vm-state.usage
                                                          │  (hostPath)
                                                          ▼
                                    Vector file source → remap → billing_gateway
```

1. **Ingest.** ukpd writes `vm.state_change` events, one JSON object per line,
   to a plain file the projector tails (`-events-path /var/run/ukp/vm-state.events`).
   The sink is configured in the runtime's `log-sinks.json`
   (`vm-state-file-sink`, `type: file`, `events: ["vm.state_change"]`,
   `reliable: false`).

   This replaced an earlier Unix-socket transport (ukpd pushing events over a
   live connection) because that requires an active listener at the exact
   moment an event fires — a redeploy or brief crash of this component could
   miss an event a socket would have delivered. A file sink has no such
   requirement: ukpd keeps appending regardless of whether anything is
   reading, and the tailer resumes from a persisted byte offset
   (`<events-path>.offset`) across restarts, so a restart re-reads only what
   it missed rather than replaying everything or skipping a gap. Replaying
   an already-processed event is safe: every transition in `processor.go` is
   idempotent (re-entering `running` or re-closing an already-closed window
   is a no-op), so an at-least-once read is enough — no exactly-once offset
   bookkeeping is needed.

   The two halves name this file differently, and both are correct: ukpd's
   `log-sinks.json` says `/run/ukp/...` because its `debian:trixie-slim` image
   symlinks `/var/run` → `/run`, while the projector must say `/var/run/ukp/...`
   because `distroless/static` has **no such symlink** — there, `/run` and
   `/var/run` are separate real directories and only `/var/run/ukp` is the
   mounted hostPath. Both therefore resolve to the same file on the node. Leaving
   the projector on a `/run/ukp` path would put its events and usage files in the
   container's ephemeral layer, where ukpd cannot write to them and Vector sees
   nothing — with no error from either side. The DaemonSet passes both paths
   explicitly for this reason.

   **This file has no built-in cap** — vendor-confirmed (2026-08-28):
   `--log-rotation` only applies to the primary `--log-path` controller log,
   not sink files, though `SIGHUP` does reopen a sink file the same way it
   reopens that log. Nothing here rotates this file for us; an external
   logrotate-style rotation (rename the file, then `SIGHUP` the `ukpd`
   container) is an operational requirement, not yet automated. The tailer
   itself is rotation-*safe* regardless of who rotates it or when: it detects
   the file at this path being replaced (via file identity, not just a size
   check) and reopens from the start.

2. **Attribution.** For each instance, the projector resolves `uuid → project /
   instance name / vcpu / memory` from a **cluster-wide watch of provider
   Pods**, matching on the Pod's own container status: Kraftlet (the
   virtual-kubelet provider driving these Pods) sets each container's
   `status.containerID` to the ukpd instance uuid itself — confirmed against a
   live deployment (2026-08-26) — so the provider Pod carrying that
   containerID is the Pod for this instance. No IP is involved.

   This replaced an earlier guest-IP-based join (`uuid` → `vmm.json`'s
   `netdev.ip` → the Pod's `podIP`), retired after two independent problems
   broke it in production: a CNI integration change on the runtime stopped
   populating `netdev.ip` in `vmm.json` entirely (confirmed 2026-08-26 — every
   `vmm.json` on an affected node had `boot_args: "unikraft"`, no `netdev.ip=`
   anywhere), and separately Kraftlet was observed leaving `status.podIP`
   unset on Pods that were otherwise fully `Running`. Either alone broke
   attribution for every instance on the node; the containerID join depends
   on neither `vmm.json` nor the Pod's IP.

   **The project is not the Pod's namespace.** That namespace name
   (`ns-<uuid>`) is a synthetic, edge-local identifier Karmada federation
   assigns to avoid collisions across projects sharing one control plane — it
   does not exist in the project control plane at all. The real Milo project
   id is a **label on the Namespace object** (`meta.datumapis.com/upstream-cluster-name`,
   e.g. `cluster-project-htxrg` → decodes to `project-htxrg`), stamped there by
   the same federation mechanism before any Instance can exist in it — never on
   the Pod itself (this repo's own tests assert the Pod must *not* carry it,
   calling that the legacy multi-cluster routing bug it replaced). So the
   projector also watches Namespaces cluster-wide, on the same shared informer
   factory as the Pod watch, and decodes this label the same way
   `go.datum.net/compute`'s own controller does. A provider Pod whose namespace
   has no such label is treated as misconfiguration (`podindex ALERT=missing_project_label`),
   not silently attributed to the namespace's raw name — the record emits
   `project: "-"` instead. Both indexes are kept in memory by their watches; no
   API calls happen per event.

3. **Windowing.** Only the `running` state counts as "on" (everything else —
   standby, stopped, stopping, terminated — bills nothing). While an instance is
   running, the projector keeps an open window (its `runningSince` /
   `reportedUntil` watermarks). On scale-to-zero, stop, or a **periodic flush**
   (default 5 minutes), it appends an incremental record covering the elapsed
   running time.

4. **Output.** One JSONL record per window is appended to
   `/var/run/ukp/vm-state.usage`:

   ```json
   {"id":"<md5(uuid|start|end)>","project":"<project>","instance":"<name>","uuid":"...",
    "vcpu":N,"memory_bytes":M,"start":"...","end":"...","duration_s":N.N}
   ```

   Vector tails this file and wraps each line as a billing Cloudevent
   (`compute.datumapis.com/instance/usage`, `subject: projects/<project>`,
   `data: {vcpu_seconds, memory_byte_seconds}`).

### Data contract — input and output

- **Input** (`vm.state_change` from ukpd, one JSON object per line):
  `{timestamp, type: "vm.state_change", data: {vm, prev, new}}` — these are
  the only keys ukpd has ever actually emitted (confirmed against production
  traffic), so the parser reads only these; if `vm` is ever renamed, a
  uuid-shaped-token fallback still recovers the uuid by scanning the raw
  payload.
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
- **RBAC:** the sidecar only watches pods and namespaces (the latter needed to
  resolve the real project — see "Attribution" above)
  (`ClusterRole ukp-state-projector-pod-reader`: get/list/watch on both) — it
  never updates anything. See `state-projector-rbac.yaml`.
- **kind e2e:** the sidecar is built via the e2e workflow (repo-root context) and
  pinned to `:e2e` in `overlays/ukp-runtime-e2e`.

The Vector side ships separately in `datum-infra`
(`apps/billing-vector-agent/components/billing-usage-collector-unikraft`).

## Failure modes / guarantees

- **Projector down (transient):** ukpd's file sink keeps appending regardless
  of whether anything is reading, so no event is lost while the projector is
  down. On restart, the tailer resumes from its persisted offset
  (`<events-path>.offset`) and picks up exactly what it missed. This is the
  main reason this transport replaced the earlier Unix socket, where an
  absent listener at the moment of an event meant that event was gone.
- **Projector crash mid-window:** the open window's watermark lives in memory,
  so a crash after reading a `running` event but before flushing its record loses
  that in-flight segment — bounded to at most one flush interval (~5 min) of a
  running instance.
- **The events file itself has no built-in cap** (vendor-confirmed — see
  "How it works" above): if nothing ever rotates it, it grows unbounded on
  the same `ukp-run` hostPath `ukpd` depends on. Rotating it is an external,
  not-yet-automated operational step (rename + `SIGHUP` to `ukpd`); the
  tailer here only guarantees it won't break when that eventually happens.

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
stats uptime=5m0s events_received=42 events_wrong_type=0 \
  dropped_no_uuid=0 dropped_no_state=0 decode_errors=0 \
  windows_open=3 windows_opened=7 windows_closed=4 \
  records_written=19 write_errors=0 unresolved=0 indexed_instances=12 \
  watch_errors=0 stale_events=0 overbilled=0 rotations=0 rotation_deletes=0 \
  project_label_missing=0
```

`podwatch synced` similarly reports both indexes once caches are usable:
`podwatch synced indexed_instances=12 indexed_projects=4` — `indexed_projects`
counts only namespaces that actually carry the project label, so it's normally
much smaller than the total namespace count in the cluster.

Reading it:

| symptom | meaning |
|---|---|
| `events_received=0` | either ukpd hasn't written anything to the events file yet, or the tailer is pointed at the wrong path — check the `vm-state-file-sink` entry in `log-sinks.json` and that `conn tailing path=` names the file ukpd writes |
| `dropped_no_uuid` / `dropped_no_state` rising | the vendor renamed its event fields; each drop logs the raw payload verbatim, so grep `event dropped` and compare against `extractTransition` |
| `indexed_instances=0` | the pod watch is empty, or no provider Pod has a `status.containerID` set yet — look for `podwatch error=` (usually the missing `ukp-state-projector-pod-reader` ClusterRole) or `podindex skip reason=no_container_id` (Pod not yet started) |
| `unresolved` rising | attribution failing; `resolve failed reason=pod_not_indexed` — no provider Pod's containerID matches this uuid yet (pod watch not synced, or the Pod's container hasn't started) |
| `project_label_missing` rising | a real provider Pod's namespace has no `meta.datumapis.com/upstream-cluster-name` label — `podindex ALERT=missing_project_label` names the namespace; per `compute`'s own controller this is misconfiguration, not transient. Records still emit, with `project: "-"` |
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
# NOT -c state-projector: it runs on distroless/static (no shell, no tail).
# ukpd shares the same ukp-run hostPath and has a real userland.
kubectl -n unikraft-system exec -it <ukp-runtime-pod> -c ukpd -- \
  tail -20 /var/run/ukp/vm-state.usage
```

Each line should be a well-formed usage record with the correct `project`,
`vcpu`, `memory_bytes` and `duration_s`. The `id` must be stable for the same
window (re-read to confirm no double-counting).

## Testing

Tests live in `internal/stateprojector`, one file per source file. Notably:

- `file_source_test.go`: tailing appended lines, holding back an incomplete
  trailing line until it's completed, resuming from a persisted offset,
  and reopening from the start when the file at the events path is
  replaced (simulating an external rotation),
- `event_test.go`: event-key parsing (the real `vm`/`prev`/`new` vendor
  payload, and the uuid-shaped-token fallback for an unrecognized uuid key),
- `pod_index_test.go`: `containerIDs` stripping a `scheme://` prefix, and
  attribution resolving from the Namespace label rather than the raw
  namespace name,
- `processor_test.go`: on/off transition → single windowed record with
  correct attribution and duration; a window closing on any non-running
  state even with no old-state field; staying open when the new state is
  unparseable; incremental flush + deterministic ids; unresolved
  attribution failing toward `-` (emit rather than drop),
- `writer_test.go`: output-file rotation and age-based cleanup.

Run with:

```sh
cd /Users/joseszycho/unikraft-provider
go test ./internal/stateprojector/... -race
```

