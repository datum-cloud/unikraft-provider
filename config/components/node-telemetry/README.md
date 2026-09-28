# Unikraft node telemetry

This component adds Unikraft application logs and runtime metrics to the shared
compute node collector. Infra composes it with the telemetry repository's
`config/node-collector` base and other runtime components. It patches
`compute-node-collector` and deploys no separate collector.

Contract version 1 uses Collector Contrib `0.144.0`. The platform supplies
`file_storage`, `memory_limiter`, `otlp_grpc/project`, and
`prometheusremotewrite/compute`. Unikraft adds `logs/unikraft` and
`metrics/unikraft`, with service-specific receiver and processor names. The
component preserves other pipelines, mounts, and environment variables. See
[`manifest.json`](manifest.json) for the machine-readable contract.

Logs come from `/var/mnt/ukp/data/platform/*/vm.log` on the host. The runtime
UUID associates each file with its virtual Kubernetes Pod's `container.id`.
This lookup must stay cluster-wide: virtual Pods do not live on the collector's
physical node. Namespace labels establish project and upstream namespace;
provider Pod labels establish instance and optional workload identity. The
pipeline sets `project_name` for gateway routing and discards logs with
incomplete ownership metadata. Log bodies cannot choose their project.

The namespace's `resourcemanager.miloapis.com/project-name` label takes
precedence. If it is absent or empty, the pipeline derives the project from
`meta.datumapis.com/upstream-cluster-name=cluster-<project>`, the identity
compute places on mapped namespaces. A missing prefix or empty suffix cannot
resolve a project.

Metrics retain the platform API scrape on `127.0.0.1:45232` and runtime scrape
on the host IP at port `45233`. Host networking is required. Infra must provide
these existing runtime credentials in the shared collector's namespace,
normally `o11y-system`:

| Secret | Key | Use |
| --- | --- | --- |
| `kraftlet-ukc-token` | `token` | Platform API metrics |
| `ukp-runtime-credentials` | `metrics-token` | Runtime metrics |

Secrets are referenced, not created or copied by the component. Configure
shared destinations and node scheduling in infra. The platform remote-write
exporter must enable resource-to-label conversion to retain node identity.

## Rollout and validation

The existing `config/dependencies/ukp-telemetry` deployment remains available
for migration. Do not run it alongside this component on the same nodes: both
would read the same logs and scrape the same metrics. Plan the change in infra,
including queue draining, file offset handling, required secrets, and rollback.
Moving to a new storage path or receiver ID can replay retained logs; this
component does not migrate collector state automatically.

Run `OTELCOL_BIN=/path/to/otelcol-contrib go test ./test/telemetry` with Kustomize
installed to compose the local fixture and validate both pipelines with the
pinned collector. CI also runs a real-collector test with VM log files and a
Kubernetes API fixture. It checks container-ID association across virtual nodes,
mapped namespace identity, explicit-label precedence, body spoofing, and
rejection of incomplete identity. These checks do not verify live runtime
authentication or project-query authorization.

Before enabling an environment, verify project log access, both runtime metric
scrapes, and scaling queries. Check metadata errors, queue usage, duplicates,
and ingestion delay. Retention, delivery limits, and credentials remain owned
by the platform and environment; publishing this component changes no running
deployment.
