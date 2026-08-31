# Adding a Vector e2e dependency for usage-billing

Context and a concrete plan for testing the last leg of per-second billing end
to end in the kind e2e cluster: `state-projector`'s output file → Vector →
a billing Cloudevent. Everything up through `state-projector` writing
`vm-state.usage` is already covered by `test/e2e/instance/02-run-instance`;
this document is about the piece after that, which nothing currently tests.

## How the real pipeline works today

```
ukpd ──file sink──▶ /var/run/ukp/vm-state.events ──▶ state-projector (tails)
                                                     │  windows + attributes
                                                     ▼
                                           /var/run/ukp/vm-state.usage
                                                     │  (hostPath, JSONL)
                                                     ▼
                                    Vector (per-node agent) ──▶ billing_gateway
```

- `state-projector` (this repo, `internal/stateprojector`) appends one JSON
  line per billing window to `/var/run/ukp/vm-state.usage`:
  ```json
  {"id":"<md5(uuid|start|end)>","project":"<project>","instance":"<name>","uuid":"...",
   "vcpu":N,"memory_bytes":M,"start":"...","end":"...","duration_s":N.N}
  ```
- In production/staging/edge, a **Vector** agent (a per-node DaemonSet, in
  `datum-infra`) tails that same hostPath and turns each line into a billing
  Cloudevent. That real config lives at
  `datum-infra/apps/billing-vector-agent/components/billing-usage-collector-unikraft/kustomization.yaml`
  and does two things:
  1. A `file` source (`unikraft_vm_state`) reading `/var/run/ukp/vm-state.usage`.
  2. A `remap` transform (`meter_unikraft_usage`) turning each line into:
     ```json
     {
       "specversion": "1.0",
       "id": "<window id>",
       "type": "compute.datumapis.com/instance/usage",
       "source": "//compute.datumapis.com/instance",
       "subject": "projects/<project>",
       "time": "<window end>",
       "datacontenttype": "application/json",
       "data": {
         "vcpu_seconds": vcpu * duration_s,
         "memory_byte_seconds": memory_bytes * duration_s,
         "dimensions": {"project": ..., "instance": ..., "region": ...}
       }
     }
     ```
     The VRL source for this transform is in that file — copy it verbatim
     rather than re-deriving it, so the e2e config stays a faithful test of
     what production actually runs.
  3. A `billing_gateway` sink that POSTs each Cloudevent to the real billing
     ingestion endpoint, with an API key.

## Why the real config can't be reused as-is for e2e

The production Vector agent is deployed via a Flux `HelmRelease`, sourced
from an **OCIRepository** (`billing-kustomize`) that's a separate,
externally-published artifact — not something built from this repo, and not
something a kind e2e cluster can pull without network access and real
credentials. The `billing_gateway` sink also points at a real external
endpoint requiring an API key Secret. None of that is appropriate (or
possible) to stand up in an isolated, hermetic e2e run.

**The plan is not to reuse that Flux/Helm chain, but to reproduce its Vector
`customConfig` (the source + transform above) in a small, self-contained
manifest that belongs to this repo**, swapping only the sink: instead of the
real `billing_gateway`, point it at a throwaway HTTP endpoint deployed in the
same kind cluster that captures whatever Vector sends it, so the chainsaw
test can assert on it.

## What needs to be built

1. **A minimal Vector manifest** (new `config/overlays/vector-e2e/`, or a
   new dependency under `config/dependencies/`, your call): a single-replica
   Vector `Deployment` (not a DaemonSet — one node's `vm-state.usage` is
   enough for e2e) with:
   - The same `ukp-run` hostPath mount `state-projector` uses, read-only.
   - A `ConfigMap` holding Vector's config: the real `unikraft_vm_state`
     source + `meter_unikraft_usage` transform copied from
     `datum-infra`, with a `sink` pointing at the mock gateway below
     instead of `billing_gateway`.
   - Pin an exact Vector image tag (check what `datum-infra`'s base chart
     pins, for consistency).

2. **A mock billing gateway**: the simplest thing that can receive a POST
   and make its body queryable — e.g. a tiny `Deployment` + `Service`
   running something like `mendhak/http-https-echo` (echoes every request
   back, viewable via its own logs) or a minimal custom container that
   appends each request body to a file `kubectl exec` can read, the same
   way `state-projector`'s own output is asserted on today. Whichever is
   simpler to assert against from a chainsaw `script` step.

3. **Wire both into `test/e2e/kind/setup.sh`**, alongside where
   `kraftlet-e2e`/`test-infra` overlays are already applied — same pattern,
   just one more `kubectl apply -k`.

4. **A new chainsaw test step** (extending `02-run-instance`, or a new
   `03-usage-billing` test) that, after the existing "assert state-projector
   attributed a usage record" step passes, polls the mock gateway and
   asserts a Cloudevent arrived with:
   - `type: compute.datumapis.com/instance/usage`
   - `subject: projects/e2e-test-project` (same project the existing test
     already stamps via the namespace label)
   - `data.vcpu_seconds` / `data.memory_byte_seconds` matching what
     `vcpu`/`memory_bytes`/`duration_s` in `vm-state.usage` would produce.

## Open decisions (yours to make when you start)

- Exact mock-gateway implementation (echo container vs. custom).
- Where the new manifests live (`config/overlays/vector-e2e` vs. a
  dependency directory) — follow whatever this repo's existing convention
  favors for e2e-only infrastructure (see how `kraftlet-e2e`/`test-infra`
  are organized for precedent).
- Whether to assert exact numeric values or just field presence/shape —
  exact values are a stronger test but more brittle if timing/duration
  varies between runs.
