---
handoff:
  id: feat-001
  from: architect
  to: api-dev
  created: 2026-03-31T00:00:00Z
  context_summary: >
    Unikraft Functions (feat-001) is a greenfield Kubernetes aggregated API server
    (go.datum.net/ufo) that manages the full lifecycle of serverless HTTP functions
    running as Unikraft unikernel VMs. It follows the same deployment pattern as
    datum-cloud/activity (k8s.io/apiserver aggregated server + etcd storage) and the
    same operator pattern as datum-cloud/workload-operator (controller-runtime,
    multi-cluster milo discovery). Functions are namespaced per-project. HTTP routing
    uses the network-services-operator's HTTPProxy CRD (networking.datumapis.com/v1alpha).
    Scale-to-zero is implemented with an activator component that buffers requests.
    Builds are triggered via Kubernetes Jobs that invoke kraft CLI.
  decisions_made:
    - "ufo is a Kubernetes aggregated API server using k8s.io/apiserver (same pattern as datum-cloud/activity); it stores Function and FunctionRevision in etcd via the standard registry/rest pattern"
    - "Function and FunctionRevision are namespace-scoped; namespace = project namespace (same model as Workload in workload-operator)"
    - "Build pipeline: a FunctionBuild CRD (also in functions.datumapis.com) is created by the Function controller; a BuildJob controller reconciles FunctionBuild by creating a Kubernetes Job that calls kraft cloud build; image digest is written back to FunctionBuild.status.imageDigest on success"
    - "VM lifecycle: the Function controller creates/updates a Workload resource (compute.datumapis.com/v1alpha) in the project namespace; Workload.spec.template.spec.runtime.sandbox uses a VirtualMachineRuntime block (not containers) referencing the Unikraft image digest"
    - "HTTP routing: one HTTPProxy resource (networking.datumapis.com/v1alpha) per Function, managed by the Function controller; hostname is auto-assigned via the NSO targetDomain (datumproxy.net)"
    - "Activator runs as a separate Deployment (ufo-activator) in datum-system namespace; it receives requests on port 8080 and signals scale-up via the Function's status or a dedicated ScaleRequest resource"
    - "Scale-to-zero swap: the Function controller watches Workload replica count; when replicas == 0 it patches HTTPProxy backend to point to activator ClusterIP:8080; when replicas > 0 it patches HTTPProxy backend back to the Workload's service endpoint"
    - "Private access: the Function controller creates a Connector resource (networking.datumapis.com/v1alpha1) for functions with spec.access == Private"
    - "Revision retention: a FunctionRevision GC controller keeps the last 10 FunctionRevisions per Function; older ones are deleted along with their Workload if it is not the active revision"
    - "API server binary: cmd/ufo-apiserver; Controller binary: cmd/ufo-controller; Activator binary: cmd/ufo-activator"
    - "Module path: go.datum.net/ufo; API group: functions.datumapis.com; initial version: v1alpha1"
    - "Quota enforcement: admission webhook (ValidatingAdmissionWebhook) on Function Create checks project function count against quota before allowing creation"
    - "Activity events: emitted via the Milo EventsProxy mechanism (same as other resources) — no direct Activity API calls needed from ufo; the Milo API server proxies audit events to the activity-apiserver automatically"
    - "Metrics: the ufo-controller and ufo-activator expose Prometheus metrics; activator exposes cold_start_duration_seconds, request_queue_depth, and active_connections; controller exposes function_reconcile_duration_seconds and build_duration_seconds"
    - "Kraft build integration: kraft CLI is invoked inside the Job container using the Unikraft Cloud API (KRAFTCLOUD_TOKEN secret); build output is an OCI image pushed to Unikraft Cloud registry; the resulting image reference (e.g., registry.unikraft.cloud/namespace/name@sha256:...) is stored on FunctionRevision.status.imageRef"
  open_questions:
    - "NON-BLOCKING: Does the workload-operator's Workload type support a VirtualMachineRuntime sandbox block for Unikraft images, or is a new Instance type needed? The chainsaw tests show Instance resources being created by WorkloadDeployments, but the Kraftfile uses datum/base-compat:latest-pvm as the runtime. The implementation agent should inspect the workload-operator v0.5.0 release for the exact sandbox spec fields before coding the Workload creation logic."
    - "NON-BLOCKING: The activator needs to know the Function's target port (the port the Unikraft VM listens on). This should come from Function.spec.runtime.port (default 8080). Verify that the workload-operator exposes a per-port Service or EndpointSlice that ufo can route to."
    - "NON-BLOCKING: The NSO HTTPProxy webhook name vhttpproxy-v1alpha.kb.io confirms HTTPProxy exists, but the exact field schema (backend ref format, hostname assignment) must be verified from the network-services-operator source before coding. The NSO config shows targetDomain: datumproxy.net — confirm whether hostname assignment is automatic (NSO assigns) or must be set in HTTPProxy.spec."
    - "NON-BLOCKING: Connector CRD (networking.datumapis.com/v1alpha1) was referenced in the discovery brief but not observed in infra YAML. Verify its existence and schema in the network-services-operator release before implementing private access."
    - "NON-BLOCKING: Quota enforcement — identify whether the existing platform Quota capability provides a Go client library or if the admission webhook must make direct API calls to Milo's quota service. The NSO infra includes a quota component; check its interface."
    - "BLOCKING before test-engineer: Define the exact kraft cloud build CLI invocation and Kraftfile template for each supported language (Go, Node.js, Python, Rust). The bun example uses datum/base-compat:latest-pvm as the runtime image — confirm this is the correct base for all languages or if per-language bases exist."
---

# Architecture: Unikraft Functions (Serverless Edge Compute)

## Overview

`ufo` (`go.datum.net/ufo`) is a Kubernetes aggregated API server that provides
a `Function` resource under the `functions.datumapis.com` API group. It manages
the full lifecycle of serverless HTTP functions running as Unikraft unikernel
VMs on Datum Cloud infrastructure.

The system has three binaries: an API server (`ufo-apiserver`), a controller
manager (`ufo-controller`), and a scale-to-zero activator (`ufo-activator`).

---

## 1. API Types

All types live in `internal/api/functions/v1alpha1/`.

### 1.1 Function

The primary user-facing resource. Namespace-scoped (namespace = project namespace).

```
Function
  TypeMeta
  ObjectMeta
    annotations:
      functions.datumapis.com/hostname       # set by controller after HTTPProxy is ready
  Spec:
    source        FunctionSource             # required: what to build
    runtime       FunctionRuntime            # required: execution environment
    scaling       ScalingConfig              # optional: scale-to-zero behaviour
    access        FunctionAccess             # optional: Public (default) | Private
    env           []EnvVar                   # optional: environment variables (name, value or valueFrom secret ref)
    revisionHistoryLimit  *int32             # optional: max revisions to retain, default 10
  Status:
    activeRevision        string             # name of the FunctionRevision currently serving traffic
    readyRevision         string             # name of the last revision that became ready
    observedGeneration    int64
    hostname              string             # assigned FQDN, e.g. <uid>.datumproxy.net
    conditions            []metav1.Condition
      - type: Ready
      - type: BuildSucceeded
      - type: RevisionReady
      - type: RouteConfigured
      - type: ScaleToZeroReady
```

#### FunctionSource (embedded in FunctionSpec)

```
FunctionSource:
  git           *GitSource         # option A: build from git
  image         *ImageSource       # option B: pre-built Unikraft image

GitSource:
  url           string             # https git URL
  ref           GitRef
    branch      string             # default "main"
    tag         string
    commit      string             # SHA, takes precedence over branch/tag
  contextDir    string             # subdirectory in repo, default "/"

ImageSource:
  ref           string             # full image reference incl. digest
                                   # e.g. registry.unikraft.cloud/org/name@sha256:...
```

#### FunctionRuntime (embedded in FunctionSpec)

```
FunctionRuntime:
  language      string             # go | nodejs | python | rust
  port          *int32             # port the function listens on, default 8080
  resources     ResourceRequirements
    memory      string             # e.g. "512Mi", default "256Mi"
  command       []string           # optional override for CMD
  env           []corev1.EnvVar    # merged with Function.spec.env
```

#### ScalingConfig (embedded in FunctionSpec)

```
ScalingConfig:
  minReplicas   *int32             # 0 (scale-to-zero, default) or 1
  maxReplicas   *int32             # default 10
  cooldownSeconds *int32           # idle time before scale-to-zero, default 60
```

### 1.2 FunctionRevision

Immutable snapshot of a deployed version. Created by the Function controller on
each deployment. Namespace-scoped. Name format: `<function-name>-<generation>`.

```
FunctionRevision
  TypeMeta
  ObjectMeta
    labels:
      functions.datumapis.com/function-name    # owning Function name
      functions.datumapis.com/generation       # generation number as string
    ownerReferences:
      - kind: Function (not blockOwnerDeletion; GC is managed by retention controller)
  Spec:                                        # immutable after creation
    functionSpec  FunctionSpec                 # full copy of Function.spec at time of revision
    imageRef      string                       # Unikraft image ref with digest (set by build)
    buildRef      string                       # name of the FunctionBuild that produced this image
  Status:
    phase         RevisionPhase                # Pending | Building | Ready | Failed | Retired
    workloadRef   string                       # name of the Workload resource for this revision
    conditions    []metav1.Condition
      - type: Ready
      - type: WorkloadReady
```

### 1.3 FunctionBuild

Tracks a single build job. Created by the Function controller; reconciled by the
BuildJob controller. Namespace-scoped.

```
FunctionBuild
  TypeMeta
  ObjectMeta
    labels:
      functions.datumapis.com/function-name
      functions.datumapis.com/revision-name
    ownerReferences:
      - kind: FunctionRevision
  Spec:
    source        FunctionSource   # copy from FunctionRevision.spec.functionSpec.source
    language      string           # copy from FunctionRevision.spec.functionSpec.runtime.language
    kraftfileTemplate string       # rendered Kraftfile content for this build
  Status:
    phase         BuildPhase       # Pending | Running | Succeeded | Failed
    jobRef        string           # name of the Kubernetes Job
    imageRef      string           # set on success: full image ref with digest
    startTime     *metav1.Time
    completionTime *metav1.Time
    message       string           # human-readable status / error message
```

### 1.4 Supporting Types Summary

| Type | Group/Version | Scope | Purpose |
|------|--------------|-------|---------|
| Function | functions.datumapis.com/v1alpha1 | Namespaced | User-facing resource |
| FunctionRevision | functions.datumapis.com/v1alpha1 | Namespaced | Immutable deployment snapshot |
| FunctionBuild | functions.datumapis.com/v1alpha1 | Namespaced | Build job tracker |

---

## 2. Control Plane Components

### 2.1 ufo-apiserver

- **Binary**: `cmd/ufo-apiserver/main.go`
- **Pattern**: `k8s.io/apiserver` aggregated API server, same as `datum-cloud/activity`
- **Storage**: etcd (uses `RecommendedOptions` with etcd backend; etcd prefix `/registry/functions.datumapis.com`)
- **Resources served**: `functions`, `functionrevisions`, `functionbuilds` (all in `functions.datumapis.com/v1alpha1`)
- **Registration**: registered as an APIService in the Milo control plane (same pattern as activity-system/base/milo-api-registration.yaml)
- **Auth**: delegates authentication and authorization to Milo via `--authentication-kubeconfig` and `--authorization-kubeconfig`
- **Admission**: admission webhooks for quota enforcement run in `ufo-controller` (separate webhook server)
- **TLS**: served via cert-manager CSI driver, cert issued by `datum-control-plane` ClusterIssuer

### 2.2 ufo-controller

- **Binary**: `cmd/ufo-controller/main.go`
- **Pattern**: controller-runtime manager (same pattern as workload-operator)
- **Discovery**: milo mode — uses project-discovery-kubeconfig to find project namespaces, per-project kubeconfig for reconciling project-scoped resources
- **Webhook server**: hosts the ValidatingAdmissionWebhook for Function quota enforcement (port 9443)
- **Controllers** (see Section 3):
  - `FunctionController` — primary reconciler
  - `FunctionRevisionController` — manages Workload lifecycle per revision
  - `FunctionBuildController` — manages Kubernetes Job per FunctionBuild
  - `FunctionRevisionGCController` — enforces revision retention limit
- **Watches**: Function, FunctionRevision, FunctionBuild (in project namespaces via per-project clients); Workload status (to detect scale changes for HTTPProxy swap)

### 2.3 ufo-activator

- **Binary**: `cmd/ufo-activator/main.go`
- **Purpose**: Holds inbound HTTP requests when a function is scaled to zero; signals scale-up; proxies to the VM endpoint once it is ready
- **Runs as**: Deployment in `datum-system` namespace, NOT in project namespaces
- **Exposed as**: ClusterIP Service `ufo-activator.datum-system.svc.cluster.local:8080`
- **Details**: see Section 4

### 2.4 Deployment Layout

```
datum-system namespace:
  Deployment: ufo-apiserver        (3 replicas, HA)
  Deployment: ufo-controller       (1 replica, leader-elected)
  Deployment: ufo-activator        (2 replicas, HA — stateless)
  Service:    ufo-apiserver        (TLS, port 443)
  Service:    ufo-activator        (HTTP, port 8080)
  Service:    ufo-controller-webhook (TLS, port 443)
```

---

## 3. Reconciliation Flow

### 3.1 FunctionController

**Trigger**: watches Function resources in all project namespaces.

#### 3.1a New Function Created

1. Validate quota: call quota service to check current function count against project limit. Reject if over limit (this is also enforced at admission, but the controller checks again defensively).
2. Set `Function.status.conditions[Ready]=False, reason=Initializing`.
3. Create `FunctionRevision` named `<function-name>-1` with:
   - `spec.functionSpec` = copy of `Function.spec`
   - `spec.imageRef` = "" (empty; will be set after build)
   - `spec.buildRef` = "" (empty; will be set after build for git sources)
4. If `Function.spec.source.git` is set: create `FunctionBuild` for the revision. Move to step 6 when build completes.
5. If `Function.spec.source.image` is set: set `FunctionRevision.spec.imageRef` immediately; skip build. Proceed to step 6.
6. Set `Function.status.activeRevision = FunctionRevision.name`.
7. Emit Activity event `functions.datumapis.com/Function.Created` (happens automatically via Milo EventsProxy on resource creation).

#### 3.1b Build Completes (FunctionBuild.status.phase == Succeeded)

The FunctionBuildController writes `FunctionBuild.status.imageRef` on success. The FunctionController watches FunctionBuild and reacts:

1. Patch `FunctionRevision.spec.imageRef` = `FunctionBuild.status.imageRef`.
2. Set `Function.status.conditions[BuildSucceeded]=True`.

This triggers the FunctionRevisionController (see 3.2).

#### 3.1c Function Updated (new spec.source or spec.runtime)

1. Increment generation counter (Kubernetes handles `metadata.generation`).
2. Create new `FunctionRevision` named `<function-name>-<generation>`.
3. If source changed: create new `FunctionBuild` for the new revision.
4. Do NOT change `Function.status.activeRevision` yet — the old revision continues serving.
5. When new revision becomes `Ready` (FunctionRevisionController sets status):
   - Swap HTTPProxy backend to the new revision's Workload endpoint.
   - Set `Function.status.activeRevision = new revision name`.
   - Set old revision's `FunctionRevision.status.phase = Retired`.
   - Emit Activity event `functions.datumapis.com/Function.Deployed`.
6. Trigger GC controller.

#### 3.1d Function Deleted

1. Kubernetes garbage collection cascades to owned FunctionRevisions via ownerReferences.
2. FunctionRevisionController handles Workload deletion when FunctionRevision is deleted.
3. Function controller deletes the managed HTTPProxy and Connector (if any) via explicit delete calls before the Function object is fully removed (use finalizer `functions.datumapis.com/cleanup`).
4. Activity event `functions.datumapis.com/Function.Deleted` emitted via Milo EventsProxy.

#### 3.1e Rollback (Re-promoting a prior FunctionRevision)

Rollback is a Function update: the user patches `Function.spec.source.image.ref` to match a prior `FunctionRevision.spec.imageRef`. This creates a new FunctionRevision with the old image digest (no rebuild), which goes directly to Ready once the Workload is up.

Alternatively: provide a `rollback` subresource that accepts `{revisionName: string}` and performs the patch atomically.

Decision: implement as a `rollback` subresource (avoids user having to look up the image digest manually). The subresource finds the named FunctionRevision, extracts its imageRef, and patches Function.spec.source to `{image: {ref: <imageRef>}}`. Activity event `functions.datumapis.com/Function.RolledBack` is emitted by the controller.

### 3.2 FunctionRevisionController

**Trigger**: watches FunctionRevision; acts when `spec.imageRef` is non-empty.

1. Create or update `Workload` (compute.datumapis.com/v1alpha) in the same namespace:
   - Name: `fn-<functionrevision-name>` (prefix to avoid collision with user Workloads)
   - `spec.template.spec.runtime.sandbox.virtualMachine.image` = `FunctionRevision.spec.imageRef`
   - `spec.template.spec.runtime.resources.instanceType` = mapped from Function memory/CPU (e.g., `datumcloud/d1-standard-2` for 512Mi, smaller type for 256Mi)
   - `spec.template.spec.networkInterfaces[0]` = default network, ingress allowed on the function's target port
   - `spec.placements` = single placement with automatic city selection; `scaleSettings.minReplicas` = `Function.spec.scaling.minReplicas` (0 for scale-to-zero)
   - Owner reference: NOT on Function (Workloads outlive revision retirement briefly); controller manages deletion explicitly
2. Store `FunctionRevision.status.workloadRef = workload.name`.
3. Watch Workload conditions. When Workload becomes `Available`:
   - Set `FunctionRevision.status.phase = Ready`.
   - Set `FunctionRevision.status.conditions[WorkloadReady] = True`.
   - Notify FunctionController (via annotation or watch event on FunctionRevision).
4. When FunctionRevision is deleted: delete the Workload.
5. When FunctionRevision.status.phase transitions to Retired: scale Workload minReplicas to 0 (but do NOT delete it — the revision remains accessible for rollback inspection until GC removes it).

### 3.3 FunctionBuildController

**Trigger**: watches FunctionBuild resources where `status.phase` is `Pending`.

1. Render Kraftfile from template (language-specific; stored in a ConfigMap in datum-system).
2. Create Kubernetes Job in a dedicated `functions-build` namespace (not the project namespace, to isolate build permissions):
   - Image: `ghcr.io/datum-cloud/ufo-builder:<version>` (a container image that has `kraft` CLI + dependencies installed)
   - Command: `kraft cloud build --platform datum --architecture x86_64 --push --name <build-name> .`
   - Volume mounts: git clone of source (init container), rendered Kraftfile, KRAFTCLOUD_TOKEN secret
   - Resources: 2 CPU, 4Gi memory, 10m activeDeadlineSeconds
3. Set `FunctionBuild.status.phase = Running`, `status.jobRef = job.name`.
4. Watch Job. On completion:
   - On success: parse kraft output for image digest; set `status.imageRef`, `status.phase = Succeeded`.
   - On failure: set `status.phase = Failed`, `status.message = <last container log line>`.
5. Set FunctionBuild ownerReference on the Job for automatic cleanup.

### 3.4 FunctionRevisionGCController

**Trigger**: watches Function; runs after any FunctionRevision creation.

1. List all FunctionRevisions for the Function, sorted by generation descending.
2. Keep the `Function.spec.revisionHistoryLimit` most recent (default 10) plus the `activeRevision`.
3. Delete excess FunctionRevisions. The FunctionRevisionController's delete handler cascades to Workload deletion.

---

## 4. Scale-to-Zero Architecture

### 4.1 Activator Design

The activator is a standard HTTP reverse proxy with hold-and-retry semantics.

**Listening**: HTTP on port 8080. Accepts all HTTP methods and paths.

**Request routing**: Each inbound request carries a header `X-Datum-Function-Name` and `X-Datum-Function-Namespace` injected by the HTTPProxy (Envoy) via a request header modifier filter. The activator uses these to identify the target function.

**Scale-up signaling**: When the activator receives a request for a scaled-to-zero function:
1. Check in-memory cache: is scale-up already in progress for this function? If yes, enqueue request.
2. If not: PATCH `Function.status` (or a dedicated annotation) to request scale-up. Specifically, patch the Workload `spec.placements[0].scaleSettings.minReplicas = 1` directly via the controller's service account.
3. Enqueue the request in a per-function queue (channel with configurable buffer, default 100).

**VM readiness detection**:
1. The activator watches for the Workload endpoint to become available via a watch on the Workload's corresponding Service or WorkloadDeployment.
2. Alternatively (simpler for MVP): the activator polls the VM's health endpoint (the function's target port / health path) with exponential backoff (5ms, 10ms, 20ms... up to 150ms total timeout).
3. Once the VM responds: dequeue and proxy all buffered requests to the VM endpoint.

**Concurrent cold-start handling**: Multiple requests for the same function during cold start are queued per-function. All requests see the queue depth. If the queue exceeds its buffer (100), subsequent requests receive `503 Service Unavailable` with `Retry-After: 5`. This is acceptable for MVP.

**Cold-start timeout**: If VM does not become ready within 10 seconds, all queued requests receive `504 Gateway Timeout`.

**Warm path**: When the function has active replicas (minReplicas >= 1), the HTTPProxy points directly to the VM endpoint, NOT through the activator. The activator is only in the hot path during cold start.

### 4.2 HTTPProxy Backend Swap

The Function controller watches Workload replica status. Specifically, it watches `Workload.status` for a replica count field (exact field TBD — check workload-operator API). The swap logic:

```
when Workload.status.availableReplicas == 0 AND Function.spec.scaling.minReplicas == 0:
    patch HTTPProxy.spec.backend to activator endpoint:
        serviceName: ufo-activator
        namespace: datum-system
        port: 8080

when Workload.status.availableReplicas > 0:
    patch HTTPProxy.spec.backend to function VM endpoint:
        serviceName: fn-<workload-name>   # or WorkloadDeployment service
        namespace: <project-namespace>
        port: <Function.spec.runtime.port>
```

The controller also injects the `X-Datum-Function-Name` and `X-Datum-Function-Namespace` headers into the HTTPProxy config (via HTTPProxy's request header modification field) so the activator can identify the target function without inspecting the Host header.

### 4.3 Activator Metrics

The activator exports:
- `ufo_activator_cold_starts_total` (counter, labels: function_name, namespace, result)
- `ufo_activator_cold_start_duration_seconds` (histogram, labels: function_name, namespace)
- `ufo_activator_queue_depth` (gauge, labels: function_name, namespace)
- `ufo_activator_active_connections` (gauge, labels: function_name, namespace)
- `ufo_activator_request_timeout_total` (counter, labels: function_name, namespace)

---

## 5. Build Pipeline

### 5.1 Trigger Paths

**Git source**: Function controller creates FunctionBuild when a new FunctionRevision with a git source is created. Re-builds are triggered by changes to `Function.spec.source.git.ref` (e.g., user pushes a new commit SHA) or by direct update to the Function spec.

For MVP, there is no automatic webhook-based trigger from GitHub pushes. The user must update `Function.spec.source.git.ref.commit` to trigger a rebuild. Webhook-based auto-deploy is a post-MVP feature.

**Image source**: No build is triggered. FunctionRevision.spec.imageRef is set directly from `Function.spec.source.image.ref`.

### 5.2 Build Job Execution

```
Namespace: functions-build (dedicated, not project namespace)
ServiceAccount: function-builder (has kraft cloud credentials secret access)

Job spec:
  initContainers:
    - name: git-clone
      image: alpine/git
      command: [git, clone, --depth=1, --branch=<ref>, <url>, /workspace]
      volumeMounts: [{name: workspace, mountPath: /workspace}]
  containers:
    - name: kraft-build
      image: ghcr.io/datum-cloud/ufo-builder:<version>
      env:
        - name: KRAFTCLOUD_TOKEN
          valueFrom: secretKeyRef (per-project or platform-level kraft cloud token)
        - name: KRAFTCLOUD_METRO
          value: fra0  # or nearest metro; TBD
      command:
        - /bin/sh
        - -c
        - |
          cp /kraftfile/Kraftfile /workspace/Kraftfile
          cd /workspace
          kraft cloud build \
            --platform datum \
            --architecture x86_64 \
            --push \
            --name <project>/<function-name> \
            --no-cache
          # parse output for image digest and write to /output/image-ref
      volumeMounts:
        - {name: workspace, mountPath: /workspace}
        - {name: kraftfile, mountPath: /kraftfile, readOnly: true}
        - {name: output, mountPath: /output}
  volumes:
    - name: workspace (emptyDir)
    - name: kraftfile (ConfigMap: rendered Kraftfile for this build)
    - name: output (emptyDir)
  restartPolicy: Never
  backoffLimit: 2
  activeDeadlineSeconds: 600  # 10 minutes
```

### 5.3 Kraftfile Templates

Templates are stored in a ConfigMap `ufo-kraftfile-templates` in `datum-system`. One template per language. The FunctionBuildController renders the template with function-specific values before creating the Job.

Example Go template:
```
spec: v0.6
name: {{ .Name }}
runtime: datum/base-compat:latest-pvm
labels:
  cloud.unikraft.v1.instances/scale_to_zero.policy: "on"
  cloud.unikraft.v1.instances/scale_to_zero.stateful: "false"
  cloud.unikraft.v1.instances/scale_to_zero.cooldown_time_ms: {{ .CooldownMs }}
rootfs: ./Dockerfile
cmd: {{ .Command }}
```

### 5.4 Build Status Flow

```
FunctionBuild created (phase: Pending)
  -> FunctionBuildController creates Job (phase: Running)
  -> Job completes successfully
    -> Controller reads image-ref from Job output (via pod logs or output volume)
    -> FunctionBuild.status.imageRef = "registry.unikraft.cloud/datum/<proj>/<fn>@sha256:..."
    -> FunctionBuild.status.phase = Succeeded
  -> FunctionController detects FunctionBuild.status.phase == Succeeded
  -> FunctionRevision.spec.imageRef = FunctionBuild.status.imageRef
  -> FunctionRevisionController creates Workload
  -> Workload becomes Available
  -> FunctionRevision.status.phase = Ready
  -> Function.status.conditions[Ready] = True
  -> HTTPProxy backend swapped to VM endpoint
```

---

## 6. Routing Architecture

### 6.1 HTTPProxy per Function

One `HTTPProxy` resource per Function. The Function controller owns it (ownerReference on Function). Name: `fn-<function-name>`.

```yaml
apiVersion: networking.datumapis.com/v1alpha
kind: HTTPProxy
metadata:
  name: fn-<function-name>
  namespace: <project-namespace>
  ownerReferences: [{kind: Function, name: <function-name>}]
spec:
  # hostname is auto-assigned by NSO using targetDomain: datumproxy.net
  # The NSO configuration shows enableDNSIntegration: true
  # Hostname format: <uid>.datumproxy.net (assigned by NSO, read back from HTTPProxy.status)
  backend:
    # Initial state (scale-to-zero): activator
    serviceName: ufo-activator
    serviceNamespace: datum-system
    port: 8080
  # Headers injected for activator routing:
  requestHeaderModifier:
    set:
      - name: X-Datum-Function-Name
        value: <function-name>
      - name: X-Datum-Function-Namespace
        value: <project-namespace>
```

The Function controller reads `HTTPProxy.status` to get the assigned hostname and writes it to `Function.status.hostname`.

**Note**: The exact HTTPProxy field schema (backend ref format, requestHeaderModifier, hostname assignment) must be confirmed from the network-services-operator source before implementation. The webhook validation `vhttpproxy-v1alpha.kb.io` confirms the CRD exists.

### 6.2 Canary / Traffic Splitting

Out of scope for MVP. All traffic goes to the active revision's endpoint. Traffic splitting (gradual rollout) is a future Phase 2 feature.

### 6.3 Private Access via Connector

When `Function.spec.access == Private`:

1. Function controller creates a `Connector` resource (networking.datumapis.com/v1alpha1) in the project namespace.
2. The Connector provides direct connectivity within the project without going through the public Gateway.
3. The HTTPProxy is still created but may have authentication policies applied.
4. The `Connector` schema must be verified from the network-services-operator source.

### 6.4 Hostname Assignment

- Public functions: NSO assigns `<uid>.datumproxy.net` (based on NSO config `targetDomain: datumproxy.net`).
- The uid is derived from the HTTPProxy's UID by the NSO.
- Custom domains are out of scope for MVP (the Gateway already supports custom domains; this can be layered on later).
- The assigned hostname is stored in `Function.status.hostname` and exposed via the `functions.datumapis.com/hostname` annotation.

---

## 7. Repository Structure

```
go.datum.net/ufo/
├── cmd/
│   ├── ufo-apiserver/
│   │   └── main.go                  # aggregated API server binary
│   ├── ufo-controller/
│   │   └── main.go                  # controller manager + webhook server
│   └── ufo-activator/
│       └── main.go                  # scale-to-zero activator HTTP proxy
├── internal/
│   ├── api/
│   │   └── functions/
│   │       ├── install/
│   │       │   └── install.go       # scheme registration
│   │       └── v1alpha1/
│   │           ├── types.go         # Function, FunctionRevision, FunctionBuild
│   │           ├── register.go
│   │           ├── doc.go
│   │           └── zz_generated.deepcopy.go
│   ├── apiserver/
│   │   └── apiserver.go             # GenericAPIServer setup, storage wiring
│   ├── registry/
│   │   ├── function/
│   │   │   └── storage.go           # etcd-backed REST storage for Function
│   │   ├── functionrevision/
│   │   │   └── storage.go
│   │   └── functionbuild/
│   │       └── storage.go
│   ├── controllers/
│   │   ├── function/
│   │   │   └── controller.go        # FunctionController
│   │   ├── functionrevision/
│   │   │   └── controller.go        # FunctionRevisionController
│   │   ├── functionbuild/
│   │   │   └── controller.go        # FunctionBuildController
│   │   └── functionrevisiongc/
│   │       └── controller.go        # GC controller
│   ├── activator/
│   │   ├── server.go                # HTTP server, request handler
│   │   ├── queue.go                 # per-function request queue
│   │   ├── scaler.go                # scale-up signaling logic
│   │   └── metrics.go               # Prometheus metrics
│   ├── webhook/
│   │   └── function_quota.go        # ValidatingWebhook for quota
│   └── version/
│       └── version.go
├── pkg/
│   ├── client/
│   │   └── clientset/               # generated client for functions.datumapis.com
│   └── generated/
│       └── openapi/
│           └── zz_generated.openapi.go
├── config/
│   ├── crd/                         # CRD manifests (generated by controller-gen)
│   ├── rbac/                        # RBAC for controller service accounts
│   ├── webhook/                     # ValidatingWebhookConfiguration
│   ├── apiserver/                   # APIService registration for Milo
│   │   └── api-registration/        # kustomization component for Milo registration
│   ├── milo/                        # Milo service configuration (IAM, quota definitions)
│   │   ├── iam/
│   │   └── quota/
│   └── manager/                     # Deployment manifests for all three binaries
│       ├── apiserver.yaml
│       ├── controller.yaml
│       └── activator.yaml
├── go.mod                           # module go.datum.net/ufo
├── go.sum
└── Makefile
```

---

## 8. Platform Capability Integrations

### 8.1 IAM

- Resource type: `functions.datumapis.com/functions`
- Permissions: `get`, `list`, `watch`, `create`, `update`, `patch`, `delete`, `invoke`
- `invoke` is a custom verb gating HTTP trigger access for private functions; implemented via SecurityPolicy on the HTTPProxy
- IAM configuration is registered in the `config/milo/iam/` directory as a Milo IAM resource configuration (follows same pattern as other services)
- ProtectedResource model: Functions are project-scoped; IAM is inherited from project

### 8.2 Quota

- Metered dimensions:
  - `functions.datumapis.com/functions` — count of Function resources per project
  - `functions.datumapis.com/invocation-seconds` — compute seconds consumed (metered by activator and reported periodically)
- Quota enforcement at admission: `ValidatingWebhook` in `ufo-controller` checks function count before allowing Function Create
- Quota configuration registered in `config/milo/quota/`

### 8.3 Activity

- Events are emitted automatically via Milo EventsProxy (the production Milo apiserver config shows `FEATURE_GATES: EventsProxy=true` with EVENTS_PROVIDER_URL pointing to activity-apiserver)
- No direct Activity API calls needed from ufo; all CRUD operations on Function resources are audited automatically
- Additional semantic events (Deployed, RolledBack) are emitted by the Function controller writing to the Function's EventsProxy-watched annotation or by creating Kubernetes Events that Milo picks up

### 8.4 Observability

Function invocation metrics are emitted by the activator (cold-start path) and by a metrics-sidecar injected into function VMs (warm path). For MVP, activator metrics cover cold-start tracking. Warm-path metrics (invocation count, error rate, latency) require a metrics collection mechanism in the Unikraft VM — this is a partial MVP item: implement activator metrics fully, document warm-path metrics as requiring additional work.

---

## 9. Implementation Plan for api-dev

Steps are ordered. Each step is independently mergeable.

1. **Bootstrap the repo**: Initialize `go.mod` at `go.datum.net/ufo`, add `k8s.io/apiserver`, `k8s.io/apimachinery`, `k8s.io/client-go`, `sigs.k8s.io/controller-runtime` dependencies (match versions from `datum-cloud/activity` go.mod: apiserver v0.34.2, api v0.34.3, etc.)

2. **Define API types**: Implement `internal/api/functions/v1alpha1/types.go` with Function, FunctionRevision, FunctionBuild structs. Run `controller-gen` to generate deepcopy and CRD manifests. Run `openapi-gen` for OpenAPI spec.

3. **Implement ufo-apiserver**: Follow `datum-cloud/activity` pattern exactly — `internal/apiserver/apiserver.go` (GenericAPIServer setup), `internal/registry/*/storage.go` (etcd-backed REST storage using `k8s.io/apiserver/pkg/registry/generic`), `cmd/ufo-apiserver/main.go` (RecommendedOptions with etcd). Register APIService in `config/apiserver/api-registration/`.

4. **Implement FunctionBuildController**: This unblocks the critical path (no VMs until build works). Implement `internal/controllers/functionbuild/controller.go`. Create `functions-build` namespace, Job template, build output parsing. Create the `ufo-builder` Dockerfile separately. This controller is the first to be runnable in isolation.

5. **Implement FunctionRevisionController**: Creates Workload resources. Requires checking the workload-operator v0.5.0 API for the exact VirtualMachineRuntime sandbox spec. Map Function memory to instanceType.

6. **Implement FunctionController** (core reconciler): Wires FunctionSource → FunctionBuild → FunctionRevision → Workload path. Implements HTTPProxy creation and backend swap logic. Implements rollback subresource.

7. **Implement FunctionRevisionGCController**: Simple list-and-delete loop.

8. **Implement ufo-activator**: `internal/activator/` package. Per-function queue, scale-up signaling, VM readiness detection, metrics.

9. **Implement quota webhook**: `internal/webhook/function_quota.go`. Register as ValidatingAdmissionWebhook.

10. **Milo registration**: `config/milo/iam/` and `config/milo/quota/` resource definitions. Match patterns from network-services-operator upstream_resources.

11. **Deployment manifests**: `config/manager/` YAML for all three binaries. Mirror structure from network-services-operator control-plane/base/ (oci-repository, manager kustomization, project-kubeconfig-template).

12. **Integration test scaffolding**: Chainsaw tests covering create → build → ready → invoke → scale-to-zero → cold-start → rollback → delete.

---

## 10. Open Questions for Implementation

### Blocking (must resolve before coding that section)

1. **VirtualMachineRuntime sandbox spec in workload-operator v0.5.0**: The infra YAML for Workload only shows `sandbox.containers`, not a VM-based variant. The Instance resources in the chainsaw test suggest VM instances are created by WorkloadDeployments, but the Workload spec must explicitly support a Unikraft image field. Verify by inspecting the workload-operator source or API docs. If Workload does not support Unikraft natively, ufo must manage `Instance` resources directly rather than via Workload.

2. **kraft cloud build output format**: Determine exactly how the built image reference (digest) is output by `kraft cloud build --push`. Is it stdout, a file, or a log line? This determines how `FunctionBuildController` extracts `imageRef` from the completed Job.

3. **KRAFTCLOUD_TOKEN scope**: Is there a single platform-level token for Datum's Unikraft Cloud account, or a per-project token? This determines how the token secret is managed and injected into build Jobs.

### Non-Blocking (can proceed, resolve before that feature lands)

4. **HTTPProxy exact field schema**: Confirm `backend`, `requestHeaderModifier`, and `hostname` field names in networking.datumapis.com/v1alpha HTTPProxy CRD.

5. **Connector CRD schema**: Confirm existence and fields for networking.datumapis.com/v1alpha1 Connector.

6. **Workload endpoint exposure**: Confirm how the workload-operator exposes a Service or EndpointSlice for a running Instance that the activator and HTTPProxy can route to.

7. **Warm-path invocation metrics**: Decide whether to instrument the Unikraft VM with a metrics sidecar or rely solely on activator metrics for MVP. Document the gap explicitly in the implementation.

8. **Per-language Kraftfile templates**: Define and test Kraftfile templates for Go, Node.js (with bun or node base), Python, and Rust. The bun example (datum/base-compat:latest-pvm) is the reference for Node.js; Go, Python, and Rust may use the same base or different bases.
