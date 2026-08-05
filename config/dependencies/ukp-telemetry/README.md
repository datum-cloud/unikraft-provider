# ukp-telemetry — node telemetry agent

A stock OpenTelemetry Collector (via the OpenTelemetry Operator) plus small
node-local sidecars that collect all runtime + guest telemetry on each compute
node and ship it onward. No custom collector build, no Vector — one agent pod,
one config.

| Signal | Source | Receiver | Destination |
| --- | --- | --- | --- |
| Guest app logs | `vm.log` per instance | `filelog` | OTLP → edge logs collector → ClickHouse |
| Runtime + host metrics | ukpd `:45233` (`/metrics/{controller,host,agent}`) | `prometheus` | remote-write → VictoriaMetrics |
| Pod resource metrics | `ukp-resource-metrics-exporter` sidecar | `prometheus` | remote-write → VictoriaMetrics |

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

The `prometheus` receiver scrapes ukpd's three metrics paths on the node
(`${HOST_IP}:45233`) — controller counters + per-user gauges, the embedded
node_exporter, and agent metrics — and remote-writes them to VictoriaMetrics.
This replaces annotation-based (`prometheus.io/scrape`) discovery, so those
annotations are intentionally absent from the runtime DaemonSet.

The same receiver also scrapes the `ukp-resource-metrics-exporter` sidecar on
`127.0.0.1:9102`. The exporter queries the local ukpd control API
(`127.0.0.1:45232`), joins instance metrics to provider Pods by guest IP
(`ukpd private_ip` / `vmm.json` == `pod.status.podIP`), and emits the
cAdvisor-compatible resource metrics expected by the HPA Resource Metrics path:

```text
datum_compute_instance_cpu_usage_seconds_total{namespace,pod,container,node,...}
datum_compute_instance_memory_working_set_bytes{namespace,pod,container,node,...}
```

The exporter reads the ukpd bearer token from `UKPD_TOKEN`, `KRAFTLET_UKC_TOKEN`,
`--ukpd-token-file`, or, by default, the node-local
`/var/lib/ukp/data/users.json` mounted read-only from the runtime data volume.
The collector pod runs with `hostNetwork: true` because ukpd's control API is
bound to host loopback.

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
- `METRICS_RW_ENDPOINT` — Prometheus remote-write destination (VictoriaMetrics).
  Set to your environment's `vminsert`/remote-write URL. (If Milo grows an OTLP
  metrics ingest, swap the `prometheusremotewrite` exporter for `otlp`.)
- `UKP_METRICS_TOKEN` — bearer token for ukpd's metrics API (same value as the
  runtime's `UKP_PROMETHEUS_API_TOKEN`); provide via the optional
  `ukp-metrics-token` Secret (`token` key).
- `ghcr.io/datum-cloud/unikraft-provider:latest` — sidecar image containing
  `/ukp-resource-metrics-exporter`; override/stamp this image the same way as
  the manager image in real deployments.
- The project comes from the Namespace label `resourcemanager.miloapis.com/project-name`,
  configured on the `k8sattributes` processor in `collector.yaml` — change the
  `key:` there if your environment uses a different label.

## Validation

The log path was validated end to end on a full stack (compute control plane +
provider + Kraftlet + runtime): a real `Instance`'s app stdout emerged from the
stock collector carrying `datum.project.name` / `datum.instance.namespace` /
`datum.instance.name` / `ukp.instance.uuid`.

For HPA resource metrics, validate the sidecar locally first:

```sh
kubectl exec -n ukp-system <ukp-telemetry-collector-pod> -c resource-metrics-exporter -- \
  /ukp-resource-metrics-exporter --help
kubectl port-forward -n ukp-system <ukp-telemetry-collector-pod> 9102:9102
curl http://127.0.0.1:9102/metrics
```

Expected samples for Running kraftlet-backed Pods:

```text
datum_compute_instance_cpu_usage_seconds_total{namespace="...",pod="...",container="app",node="kraftlet-...",...}
datum_compute_instance_memory_working_set_bytes{namespace="...",pod="...",container="app",node="kraftlet-...",...}
```
