# ukp-logs — guest application-log pipeline

Streams each microVM's application logs (the guest serial console, `vm.log`) to
an OpenTelemetry collector, enriched with the **Datum** identity of the owning
compute `Instance` — kept separate from the Unikraft runtime identity.

Resource attributes on every log record:

| Attribute | Source |
| --- | --- |
| `datum.project.name` | project label on the Instance's Namespace |
| `datum.instance.namespace` | provider Pod (`meta.datumapis.com/upstream-namespace`) |
| `datum.instance.name` | provider Pod (`meta.datumapis.com/upstream-name`) |
| `ukp.instance.uuid` | ukpd instance uuid (the `vm.log` directory) |
| `k8s.node.name` | the runtime node |

## How it works

A **stock** `otelcol-contrib` collector does the whole pipeline — no custom
collector build, no Vector:

```
guest stdout ─(virtio-console)→ /var/lib/ukp/data/platform/<uuid>/vm.log
      │
   [reconciler sidecar]  join vm.log (uuid + guest IP from vmm.json) to the
      │                  provider Pod (podIP → upstream name/namespace) and its
      │                  Namespace (project label); symlink into an
      ▼                  identity-encoded path:
   /var/log/ukp-logs/project=<p>/ns=<ns>/instance=<n>/uuid=<uuid>/vm.log
      │
   [otelcol filelog]  regex on the path → resource attributes above
      ▼
   otlp → edge-logs-collector → Milo telemetry (ClickHouse / Grafana)
```

The reconciler is a ~90-line script (`reconciler.py`, mounted from a ConfigMap)
that only manages symlinks — it never touches a log byte. The join key is the
guest IP, read from `vmm.json` on the host (no ukpd credentials required).

This mirrors how the collector enriches container logs (identity encoded in the
log path); the reconciler just gives microVM logs the same path shape.

## Requirements

- **OpenTelemetry Operator** in the cluster (provides the `OpenTelemetryCollector`
  CRD). This is why the pipeline is a separate dependency and is not applied by
  the kind e2e — deploy it to edge clusters that run the operator.
- The runtime deployed (`config/dependencies/ukp-runtime`) on the same nodes; the
  collector matches its node selector (`compute.datumapis.com/runtime=unikraft`).

## Configuration

- `LOGS_OTLP_ENDPOINT` (env in `collector.yaml`) — the destination OTLP endpoint.
  Defaults to the edge logs collector service; set it to your environment's
  collector. Add TLS/auth via an overlay for cross-cluster or untrusted paths.
- `PROJECT_LABEL` (reconciler env) — the Namespace label holding the project
  name. Defaults to `resourcemanager.miloapis.com/project-name`.

## Validation

The mechanism was validated end to end on a full stack (compute control plane +
provider + Kraftlet + runtime): a real `Instance` produced a `vm.log` that the
stock collector emitted carrying `datum.project.name` / `datum.instance.namespace`
/ `datum.instance.name` / `ukp.instance.uuid` alongside the app's stdout.
