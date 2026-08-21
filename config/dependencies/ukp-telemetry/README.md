# ukp-telemetry — node telemetry agent

A single **stock** OpenTelemetry Collector (via the OpenTelemetry Operator) that
collects all runtime + guest telemetry on each compute node and ships it
onward. No custom collector build, no Vector — one agent, one config.

| Signal | Source | Receiver | Destination |
| --- | --- | --- | --- |
| Guest app logs | `vm.log` per instance | `filelog` | OTLP → edge logs collector → ClickHouse |
| Runtime + host metrics | ukpd `:45233` (`/metrics/{controller,host,agent}`) | `prometheus` | remote-write → edge + hub VictoriaMetrics |
| Instance resource metrics | ukpd `:45232` (`/v1/instances/metrics`) | `prometheus` | remote-write → edge + hub VictoriaMetrics |

## Logs — Datum-enriched

Guest logs are enriched with the **Datum** identity of the owning compute
`Instance`, kept separate from the Unikraft runtime identity:

| Attribute | Source |
| --- | --- |
| `datum.project.name` | project label on the Instance's Namespace |
| `datum.instance.namespace` | provider Pod (`meta.datumapis.com/upstream-namespace`) |
| `datum.instance.name` | provider Pod (`meta.datumapis.com/upstream-name`) |
| `ukp.instance.uuid` | ukpd instance uuid (the `vm.log` directory) |
| `k8s.node.name` | the runtime node |

The enrichment is done by the stock **`k8sattributes`** processor, keyed on the
guest IP: the provider Pod's `podIP` equals the guest IP, so `k8sattributes`
finds the Pod by IP and stamps `datum.instance.{name,namespace}` from its
annotations and `datum.project.name` from the Namespace label — no custom code.

The only node-side piece is a **filesystem-only IP surfacer** (`ip-surfacer.sh`,
a busybox sidecar): `filelog` can only read a log's path, and ukpd's `vm.log`
path carries just the uuid, so the surfacer reads the guest IP from `vmm.json`
and symlinks each log into `/var/log/ukp-logs/ip=<ip>/uuid=<uuid>/vm.log`. The
`filelog` regex lifts `k8s.pod.ip` + `ukp.instance.uuid` from that path;
`k8sattributes` does the rest. The surfacer holds **no Kubernetes logic** — it
only manages symlinks. (To drop even the surfacer, the runtime/provider would
need to put the guest IP on the log record directly — an upstream ask.)

## Metrics

The `prometheus` receiver scrapes ukpd's metrics API on `${HOST_IP}:45233` —
controller counters + per-user gauges, the embedded node_exporter, and agent
metrics — and remote-writes them to edge-local and hub VictoriaMetrics. It also
scrapes ukpd's platform API on `127.0.0.1:45232` at `/v1/instances/metrics` for
per-instance metrics such as `instance_cpu_time_s` and `instance_rss_bytes`. The
platform API scrape uses a ukpd user token (`UKP_API_TOKEN`), separate from the
metrics API token used for `:45233`.

For HPA, record Pod-shaped metrics downstream by joining ukpd `instance_uuid` to
Kraftlet's Pod `status.containerStatuses[].containerID`, exposed by
kube-state-metrics as `kube_pod_container_info{container_id=...}`:

```promql
label_replace(
  instance_cpu_time_s,
  "container_id", "$1",
  "instance_uuid", "(.*)"
)
* on(container_id) group_left(namespace, pod, container)
kube_pod_container_info
```

Record the result as `datum_compute_instance_cpu_usage_seconds_total`; apply the
same join to `instance_rss_bytes` and record it as
`datum_compute_instance_memory_working_set_bytes`.

This replaces annotation-based (`prometheus.io/scrape`) discovery, so those
annotations are intentionally absent from the runtime DaemonSet.

## Requirements

- **OpenTelemetry Operator** in the cluster (provides the `OpenTelemetryCollector`
  CRD). This is why the agent is a separate dependency and is not applied by the
  kind e2e — deploy it to edge clusters that run the operator.
- The runtime deployed (`config/dependencies/ukp-runtime`) on the same nodes; the
  agent matches its node selector (`compute.datumapis.com/runtime=unikraft`) and
  its telemetry emission (metrics API bound, log sink, `--vmm-metrics-emit`).

## Configuration (env in `collector.yaml`)

- `LOGS_OTLP_ENDPOINT` — logs destination; defaults to the `edge-logs-system`
  collector. Add TLS/auth via an overlay for cross-cluster paths.
- `LOCAL_METRICS_RW_ENDPOINT` — edge-local VictoriaMetrics remote-write
  destination. This is where edge `VMAlert` records the Pod-shaped
  `datum_compute_instance_*` metrics consumed by the compute Prometheus Adapter.
- `METRICS_RW_ENDPOINT` — hub Prometheus remote-write destination
  (VictoriaMetrics). Set to your environment's `vminsert`/remote-write URL. (If
  Milo grows an OTLP metrics ingest, swap the `prometheusremotewrite` exporter
  for `otlp`.)
- `UKP_METRICS_TOKEN` — bearer token for ukpd's metrics API (same value as the
  runtime's `UKP_PROMETHEUS_API_TOKEN`); both are sourced from the
  `ukp-runtime-credentials` Secret (`metrics-token` key), provisioned by the
  infra repo's ExternalSecret.
- `UKP_API_TOKEN` — ukpd platform API user token for `/v1/instances/metrics`;
  reuses kraftlet's own ukpd user token, the `kraftlet-ukc-token` Secret
  (`token` key).
- The project comes from the Namespace label `resourcemanager.miloapis.com/project-name`,
  configured on the `k8sattributes` processor in `collector.yaml` — change the
  `key:` there if your environment uses a different label.

## Validation

The log path was validated end to end on a full stack (compute control plane +
provider + Kraftlet + runtime): a real `Instance`'s app stdout emerged from the
stock collector carrying `datum.project.name` / `datum.instance.namespace` /
`datum.instance.name` / `ukp.instance.uuid`.
