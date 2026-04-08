---
handoff:
  id: feat-001
  from: product-discovery
  to: architect
  created: 2026-03-31T00:00:00Z
  context_summary: "Serverless edge compute via Unikraft unikernels — Function resource in functions.datumapis.com, activator pattern for scale-to-zero, HTTPProxy for routing, Unikraft Cloud for artifact storage"
  decisions_made:
    - "MVP is HTTP-trigger only; cron and event triggers are deferred to future phases"
    - "Unikraft unikernels are the mandated runtime (not containers); VM-level isolation is a hard requirement"
    - "ufo is the Datum workload provider that runs Unikraft VMs — it is the compute primitive, not a consumer of an existing Workload resource"
    - "API group: functions.datumapis.com (follows compute.datumapis.com / networking.datumapis.com pattern)"
    - "Go module path: go.datum.net/ufo"
    - "Artifact storage: Unikraft Cloud for MVP — kraft cloud build produces an image digest stored on FunctionRevision; no Registry Service needed at MVP"
    - "Datum owns the Unikraft build step and invokes it via kraft tooling (kraft cloud build or equivalent)"
    - "HTTP routing: Function controller creates/manages an HTTPProxy resource (networking.datumapis.com) per Function — this is the established Datum pattern over Envoy"
    - "Scale-to-zero buffering: activator pattern (same as Knative) — dedicated activator component in ufo holds inbound requests while VM cold-starts, then forwards; HTTPProxy backend is swapped between activator endpoint and VM endpoint based on replica count"
    - "Private connectivity: Connector resource (networking.datumapis.com/v1alpha1) already exists; Function controller creates a Connector for private-access functions"
    - "Gateway (HTTPProxy) and Service Connect (Connector) exist as implemented resources in the network-services-operator"
    - "Scale-to-zero is in scope for MVP; region-pinning/placement control is out of scope"
    - "Supported languages for MVP: Go, Node.js, Python, Rust"
    - "Revision history and rollback are in scope for MVP"
  open_questions:
    - "NON-BLOCKING: Does Function map directly to a VM lifecycle (1 Function = 1 VM), or is there a fleet/pool model (1 Function = N VMs that scale)?"
    - "NON-BLOCKING: What namespace model applies — are Function resources per-project/per-consumer namespace, or cluster-scoped?"
    - "NON-BLOCKING: What is the expected scale — how many concurrent functions per project, total across the platform?"
    - "NON-BLOCKING: Should function logs be surfaced through the Insights/Telemetry capability or a separate log tail API?"
    - "NON-BLOCKING: What does rollback mean precisely — redeploy a prior revision's Unikraft image, or restore a prior VM configuration?"
    - "NON-BLOCKING: Does the activator need to handle concurrent cold-starts gracefully (request queuing), or is a simple single-waiter model sufficient for MVP?"
  assumptions:
    - "The upstream spec (enhancements/pull/625) is authoritative for MVP scope and is the primary design input"
    - "Datum Cloud uses a Kubernetes aggregated API server pattern; Function will be a custom resource in that model"
    - "ufo is both the API surface AND the runtime provider — it manages Unikraft VM lifecycle directly"
    - "Unikraft Cloud stores built artifacts as OCI-compatible images identified by digest"
    - "Platform capabilities (Quota, Activity, Insights, Telemetry) follow patterns already established by other Datum resources"
    - "The workload-operator's Instance/VirtualMachineRuntime types may be reused or referenced by ufo for VM scheduling"
    - "The HTTPProxy resource from network-services-operator is the correct routing abstraction; ufo does not need to configure Envoy xDS directly"
---

# Discovery Brief: Unikraft Functions — Serverless Edge Compute

## Problem Statement

Developers building applications at the edge need a way to run code without managing servers or
containers. Today, deploying to Datum requires understanding Workload primitives, network topology,
Gateway configuration, and image building. This is a high barrier for developers whose primary
asset is application code in a Git repository.

Unikraft Functions solves this by providing a single `Function` resource: point it at a repository,
and the platform handles building, deploying, scaling, and routing. The Unikraft unikernel runtime
provides a critical differentiation — 1–10MB images and 10–50ms cold starts via snapshot/restore,
enabling true scale-to-zero with acceptable latency. Container-based alternatives (e.g., Knative)
cannot match this cold-start profile without significant infrastructure overhead.

The secondary problem is isolation. Functions run in Unikraft VMs (VM-level isolation), not shared
processes or containers, which is important for multi-tenant edge deployments where co-tenancy
risks must be minimized.

## Target Users

**Primary: Application Developers (early adopters, indie developers, startup engineers)**
- Want to ship HTTP APIs, webhooks, or background processors without ops overhead
- Are comfortable with Git-driven workflows (push to deploy)
- May be migrating from Cloudflare Workers, AWS Lambda, or Vercel Functions
- Value fast iteration cycles and zero-config defaults

**Secondary: Platform Engineers at Organizations Using Datum**
- Need to expose internal services as HTTP endpoints without managing infrastructure
- Want programmatic control via the Datum API (not just CLI/UI)
- Will use revision history and rollback for change management

**Tertiary: Datum Platform Team**
- Needs observability into function execution (for SLA tracking, billing, quota enforcement)
- Needs a model that composes cleanly with existing Workload, Gateway, and Registry investments

## Scope

### In Scope (MVP)

- `Function` custom resource (Kubernetes CRD pattern, aggregated API server)
- Git-driven build trigger: connect a Repository from the Registry Service, build on push
- Direct upload path (artifact delivered to Registry, Function references it)
- Unikraft unikernel as the exclusive runtime for MVP
- Multi-language support: Go, Node.js, Python, Rust
- HTTP trigger only (no cron, no event bus)
- Scale-to-zero and automatic scale-up on traffic (target: <100ms scale decision)
- Automatic placement only (no user-controlled region selection)
- Stateless workloads only (no persistent local storage)
- Public exposure via Gateway (Envoy) — TLS, authentication, rate limiting inherited
- Private access via Service Connect — direct connectivity within a project
- Revision history: each deployment creates a named Revision
- Rollback: redeploy a prior Revision
- CLI surface: `datum functions deploy`, `logs`, `revisions`, `rollback`, `invoke`
- Quota metering: function count per project, invocation-hours consumed
- Activity events: function created, deployed, invoked, deleted
- Basic observability: invocation count, error rate, latency (p50/p95/p99)

### Out of Scope (MVP)

- Cron / scheduled triggers
- Event-driven triggers (message queues, webhooks from third-party services)
- User-controlled placement / region selection
- Stateful functions (persistent volumes, databases)
- Custom domains (beyond what Gateway already provides)
- Build caching beyond what the Registry/build service natively offers
- VPC peering or private networking beyond Service Connect
- GPU or accelerated compute
- WebAssembly (Wasm) runtime targets
- Cost/billing UI (metering data is collected but billing interface is future)
- Multi-step / chained function composition

### Future Phases

- **Phase 2**: Cron triggers, event-bus triggers (Kafka, NATS, Datum Events)
- **Phase 2**: User-controlled placement (region pinning, affinity rules)
- **Phase 2**: Persistent storage integration (object store mount, KV bindings)
- **Phase 3**: Function composition / chaining
- **Phase 3**: Wasm runtime target alongside Unikraft
- **Phase 3**: Billing UI and cost analytics
- **Phase 4**: Multi-region fan-out invocations

## Platform Capabilities Required

### Quota (Required — MVP)

Functions introduce two new metered dimensions:

1. **Function count per project** — limits the number of `Function` resources a project can have.
   Prevents resource exhaustion in shared control plane storage and scheduling.
2. **Invocation-hours / compute-seconds** — tracks actual execution time consumed.
   Required for billing metering and fair-use enforcement.

The architect must decide whether quota enforcement happens at admission (webhook) or via the
existing Quota capability pattern used by other resources.

### Activity (Required — MVP)

Function lifecycle events must be emitted to the Activity capability so operators and consumers
can audit who deployed, invoked, or deleted a function. Minimum events:

- `functions.datum.net/Function.Created`
- `functions.datum.net/Function.Deployed` (new revision active)
- `functions.datum.net/Function.RolledBack`
- `functions.datum.net/Function.Deleted`

### Insights / Telemetry (Required — MVP, with scoping)

Function invocations are the primary observable unit. The platform needs:

- **Invocation metrics**: total invocations, error rate, latency histogram (per function, per
  revision, per project)
- **Cold-start tracking**: cold vs warm invocation ratio (key product metric for Unikraft
  differentiation)
- **Log streaming**: stdout/stderr from function execution surfaced via the existing Telemetry
  capability or a dedicated log tail API

The architect should determine whether function metrics flow through the existing Insights path
(likely yes, given Workload already emits metrics) or require a new aggregation layer.

### IAM / Authorization (Required — MVP)

Functions are project-scoped resources. The IAM `ProtectedResource` model applies:

- `functions.datum.net/functions` is the protected resource type
- Permissions: `get`, `list`, `create`, `update`, `delete`, `invoke`
- The `invoke` permission is distinct from CRUD — it gates HTTP trigger access for private functions
- Public functions bypass invoke permission check (Gateway handles this)

## Dependencies on Existing Platform Components

| Component | Role | Status Unknown — Needs Verification |
|-----------|------|-------------------------------------|
| **ufo (this repo)** | IS the workload provider — manages Unikraft VM lifecycle directly. Function is a first-class resource here, not a wrapper around an external Workload. | This is the service being built. |
| **Registry Service** | Would store built Unikraft artifacts. **Does not exist.** Must be designed or a suitable backend (OCI registry, Unikraft Cloud) chosen. | Confirmed absent. |
| **Gateway (Envoy)** | Envoy-based HTTP routing. Already exists at Datum. Public functions exposed here. Integration mechanism (xDS vs Gateway API) TBD. | Exists; integration depth unknown. |
| **Service Connect** | Private connectivity within a project. Private functions reachable here. | Existence unconfirmed. |
| **Unikraft Build (kraft)** | Converts source code (Go/Node/Python/Rust) into Unikraft unikernel images. Datum owns this step and uses Unikraft's tooling. Invocation model (K8s Job, build controller, external API) TBD. | Ownership confirmed; integration model TBD. |

**Critical observation**: The `ufo` repository is currently greenfield — no Go source files, no
API types, no controllers. It is not yet clear whether `ufo` _is_ the Functions service being
built from scratch, or whether it is a meta-repo that will depend on existing Datum platform
services hosted elsewhere. The architect must resolve this before proceeding.

## Key Design Decisions

### Inherited from Upstream Spec

1. **ufo IS the workload provider**: `ufo` manages Unikraft VM lifecycle directly — it is not
   a consumer of an external Workload resource. The `Function` resource is a first-class type
   in `ufo`'s API. The upstream spec's assumption that Function wraps an existing Workload
   is revised: `ufo` provides the compute primitive itself.

2. **Revision model**: Each deploy creates a new Revision object (immutable snapshot of the
   function spec + image reference). Rollback means re-promoting a prior Revision, not mutating
   the Function object in place.

3. **Unikraft-only runtime for MVP**: The build and runtime path is specialized for Unikraft
   unikernels. Container support is explicitly out of scope at MVP.

4. **Controller-based reconciliation**: A Functions controller watches `Function` resources and
   drives convergence — triggering builds, creating Workloads, configuring Gateway routes.

5. **Scale-to-zero via Workload**: The scaling behavior (scale-to-zero, scale-up) is delegated
   to the Workload layer, which is assumed to support this. If Workload does not support
   scale-to-zero natively, the Functions controller must implement it.

### Decisions Needing Architect Confirmation

6. **API server placement**: Should `Function` and `Revision` types live in a dedicated
   aggregated API server binary (`ufo`) or be added to an existing platform API server? The
   upstream spec implies a dedicated "Functions service" control plane.

7. **Build trigger mechanism**: How does a repo push trigger a build? Options:
   - Webhook from Registry to Functions controller
   - Functions controller watches Registry Repository objects
   - External CI pipeline that pushes a new image and patches the Function spec
   The architect should choose the pattern that fits existing Registry integration.

8. **Gateway integration depth**: Does the Function controller directly create Gateway
   HTTPRoute resources, or does it go through a higher-level abstraction? This affects
   the API surface and coupling.

## Open Questions for Architect

### Blocking

1. **Codebase scope**: Is `ufo` the Functions service being built net-new, or does it integrate
   with other existing Datum service repos? What is the module path and API group for this
   service?

2. **Workload API**: What does the existing `Workload` resource API look like? Specifically:
   - Does Workload support scale-to-zero natively, or does the Function controller need to
     implement this by scaling replicas to zero?
   - What fields control the runtime image and entrypoint?
   - Is Workload namespaced (per-project) or cluster-scoped?

3. **Registry API**: What does the `Repository` object in the Registry Service look like?
   What is the reference format for a built image (name, digest, tag)?

4. **Build service ownership**: Who is responsible for running `kraft build` or equivalent?
   Is there an existing build pipeline API, or does this need to be designed?

5. **Gateway integration**: What is the existing Gateway API contract? Does the platform
   already have `HTTPRoute` or equivalent types, or does it use raw Envoy xDS configuration?

### Non-Blocking

6. **Namespace model**: Are Functions namespaced per-project (tenant namespace) or is there a
   cluster-scoped Function with a project reference field? This affects how the IAM
   ProtectedResource model applies.

7. **Invocation routing for scale-to-zero**: When a function is scaled to zero and a request
   arrives, who holds the connection while the function starts? Is there an existing "buffer
   proxy" component in Gateway/Envoy, or does this need to be designed?

8. **Log surfacing**: Should function stdout/stderr be accessible via a `logs` subresource
   on the Function API, or does it flow through an existing Telemetry/Logging API?

9. **Multi-region**: The MVP uses automatic placement. How does Gateway route to the correct
   region? Does the platform already handle anycast/GeoDNS, or is there a single placement
   point for MVP?

10. **Function invocation auth**: For private functions, how does the caller authenticate
    through Service Connect? Is there a token-based auth model, or is it network-policy-only?

## Risk Assessment

### High Risk

- **Dependency on unverified components**: The design assumes Workload, Registry, Gateway, and
  Service Connect exist and have stable APIs. If any are absent or significantly different from
  assumptions, the Function design may need to change substantially. This is the top risk given
  the greenfield state of the `ufo` repo.

- **Unikraft build toolchain integration**: Integrating `kraft` or the Unikraft build pipeline
  into a cloud-native CI path is non-trivial. Build times, caching, and failure modes are
  unknown. If cold-start targets (10–50ms) depend on pre-warmed snapshots, the snapshot
  lifecycle must be designed carefully.

- **Scale-to-zero invocation buffering**: Holding HTTP connections during cold starts requires
  a buffering proxy in the data path. This is either already solved by Gateway/Envoy (e.g.,
  via a custom filter), or it must be built. The complexity and latency impact are significant.

### Medium Risk

- **Revision storage growth**: Each deployment creates a Revision object. Without a retention
  policy, long-lived projects accumulate unbounded Revision history. A default retention limit
  (e.g., keep last 10 revisions) should be designed in from the start.

- **Multi-language build matrix**: Supporting Go, Node.js, Python, and Rust means maintaining
  four Unikraft build configurations. Divergence between language support levels in Unikraft
  (Go and Rust are mature; Python and Node.js may have limitations) could affect MVP scope.

- **Quota enforcement timing**: If quota is enforced at admission, a build-then-reject scenario
  is possible (build succeeds, admission rejects deployment). Quota must be checked before the
  build is triggered.

### Low Risk

- **CLI surface**: `datum functions` commands are thin wrappers over the API. Risk is low if
  the API is well-defined.

- **IAM integration**: The ProtectedResource model for Functions follows the same pattern as
  other Datum resources. Risk is low if the pattern is mature.

## Success Criteria

### MVP is done when:

1. A developer can create a `Function` resource referencing a GitHub repository, push code, and
   have the function automatically built and deployed without any additional configuration.

2. The deployed function is reachable via a public HTTPS endpoint (Gateway) within 60 seconds
   of a successful build.

3. When idle, the function scales to zero. When traffic arrives, the function is invoked within
   100ms of a cold start (with Unikraft snapshot/restore providing the sub-50ms VM start time).

4. A developer can list revisions (`datum functions revisions <name>`), inspect each, and roll
   back to any prior revision.

5. Quota enforcement prevents a project from exceeding its function count limit at admission
   time.

6. Activity events are emitted for all lifecycle transitions and visible via the platform
   Activity API.

7. Invocation metrics (count, error rate, p50/p95/p99 latency) are available via the platform
   Insights API.

8. The `Function` resource is protected by IAM — only authorized principals can create, update,
   delete, or invoke functions.

9. Platform integration tests cover: create, deploy, scale-to-zero, scale-up, rollback, and
   delete lifecycle.
