---
status: provisional
stage: alpha
---

# Instance Provisioning via Kraftlet

## Architecture Overview

A Datum Cloud `Instance` does not start life as an `Instance` — it is the
edge-local materialization of a user-defined `Workload`. This section works
top-down: first the platform path that takes an end user's `Workload` and
federates it out to edge locations as `Instance`s, then the per-location
components that turn each `Instance` into a running unikernel. The sequence
diagrams later in this document trace the individual API calls and CNI
invocations for that per-location flow.

### From Workload to edge Instances

An end user declares a `Workload` describing what to run (an instance template)
and where to run it (one or more placements, each scoped to a set of city
codes). The platform fans that single declaration out across edge locations:

```mermaid
flowchart TB
    user([End User])

    subgraph project["Datum Cloud (project control plane)"]
        WL["Workload<br/>(template + placements)"]
        WLC[Workload Controller]
        WD["WorkloadDeployment<br/>(one per placement / city code)"]
    end

    subgraph fed["Karmada (federation control plane)"]
        FED[WorkloadDeployment Federator]
        PP["PropagationPolicy<br/>(per city code)"]
        KARMADA[Karmada]
    end

    subgraph edge["Edge Location (POP-cell cluster, per city code)"]
        WDC[WorkloadDeployment Controller]
        INST["Instance(s)"]
        flow["per-location provisioning<br/>(see below)"]
    end

    user -->|"create Workload"| WL
    WL --> WLC
    WLC -->|"fan out by placement"| WD
    WD -->|"federate downstream"| FED
    FED -->|"create / select<br/>(label: city-code)"| PP
    FED --> KARMADA
    PP --> KARMADA
    KARMADA -->|"propagate to clusters<br/>with matching city-code"| WDC
    WDC -->|"materialize replicas"| INST
    INST --> flow
```

- **Workload** — the user-facing declaration: an instance template plus a list of placements, each naming the city codes it should run in and its scale settings.
- **Workload Controller** — reconciles a `Workload` into one `WorkloadDeployment` per placement/city code, applying the template and desired replica counts.
- **WorkloadDeployment Federator** — pushes each `WorkloadDeployment` to the downstream Karmada control plane and lazily maintains a `PropagationPolicy` per city code that selects the matching edge clusters. Status aggregated by Karmada is mirrored back up onto the source objects.
- **Karmada** — federation control plane that propagates each `WorkloadDeployment` to the POP-cell clusters whose city-code label matches its `PropagationPolicy`.
- **Edge Location (POP-cell cluster)** — a per-city-code member cluster. Its `WorkloadDeployment` controller materializes the actual `Instance` replicas, each of which is then provisioned by the per-location components described next.

### Within an edge location

Once an `Instance` exists in an edge location, several control-plane components
cooperate to turn that declarative `Instance` into a running, network-attached
unikernel, and a host-side agent drives the actual boot and networking. The
following describes those components and how they interact at a coarse grain.

```mermaid
flowchart TB
    user(["Edge WorkloadDeployment<br/>Controller (materializes Instance)"])

    subgraph datum["Datum Cloud (control plane)"]
        API[API Server]
        CO[Compute Operator]
        UP[Unikraft Provider]
        GO[Galactic Operator]
        NET["Network Services + IPAM<br/>(IP allocation)"]
    end

    subgraph host["Unikraft Host (data plane)"]
        KL[Kraftlet]
        CNI["CNI chain<br/>Multus → IPAM CNI → Galactic CNI"]
        UK[Unikraft Runtime]
    end

    user -->|"create / delete Instance"| API

    API -->|"Instance events"| CO
    CO -->|"allocate private IP"| NET
    CO -->|"create VPCAttachment"| API

    API -->|"Instance events"| UP
    UP -->|"translate Instance → Pod"| API
    UP -.->|"sync Pod status → Instance status"| API

    API -->|"VPCAttachment + Pod events"| GO
    GO -->|"NAD + Multus annotations"| API

    API -->|"scheduled Pod"| KL
    KL -->|"set up interfaces"| CNI
    CNI -->|"confirm IP allocation"| NET
    KL -->|"boot unikernel on TAP device"| UK
    KL -.->|"report Pod status"| API
```

### Component responsibilities

| Component | Role |
| --- | --- |
| **API Server** | Stores the desired state (`Instance`, `Pod`, `VPCAttachment`, `NetworkAttachmentDefinition`) and is the event bus every other component watches. |
| **Compute Operator** | Owns the network prerequisites for an `Instance`: resolves its network interfaces, drives IP allocation, and creates/deletes the `VPCAttachment`. |
| **Network Services + IPAM** | Allocate and release private IPs from a subnet's address pool, backing both the up-front `AddressClaim` and the CNI-time lookup. |
| **Unikraft Provider** | Translates an `Instance` into a `Pod` targeted at a Kraftlet node, and syncs the resulting `Pod` status back onto the `Instance`. Owns the `Instance` finalizer. |
| **Galactic Operator** | Wires the instance into its VPC: builds the `NetworkAttachmentDefinition` (CNI chain) for a `VPCAttachment` and injects the Multus network annotation onto the `Pod`. |
| **Kraftlet** | Host agent that picks up scheduled `Pod`s, parses them into a unikernel config, drives the CNI chain to provision host networking, boots the unikernel, and reports `Pod` status. |
| **CNI chain (Multus → IPAM CNI → Galactic CNI)** | Provisions the host-side network: confirms the IP allocation, then creates the VRF/TAP/veth plumbing and programs SRv6 + BGP routing for the VPC. |
| **Unikraft Runtime** | Boots and runs the unikernel instance, attaching it to the TAP device prepared by the CNI chain. |

### Lifecycle at a glance

0. **Materialization** — federation lands a `WorkloadDeployment` in the edge location, whose controller creates the `Instance` that the steps below act on.
1. **Network prerequisites** — the Compute Operator reacts to a new `Instance`, allocates a private IP, and records a `VPCAttachment`.
2. **Pod creation** — the Unikraft Provider translates the `Instance` into a `Pod` bound for a Kraftlet node.
3. **VPC wiring** — the Galactic Operator prepares the CNI chain definition and annotates the `Pod` for Multus.
4. **Boot** — Kraftlet provisions host networking through the CNI chain and boots the unikernel.
5. **Status sync** — `Pod` status flows back through the Unikraft Provider onto the `Instance`.
6. **Teardown** — deletion unwinds the same path in reverse, releasing host networking, the `Pod`, the `VPCAttachment`, and the IP allocation.

## Instance Provisioning Flow

```mermaid
%%{init: {
  "sequence": {
    "showSequenceNumbers": true
  }
}}%%
sequenceDiagram
    box rgb(199,228,255) Datum Cloud
        participant API as API Server
        participant CO as Compute Operator
        participant NSO as Network Services
        participant IPAM as IPAM Service
        participant UP as Unikraft Provider
        participant GO as Galactic Operator
        participant KL as Kraftlet
    end
    box rgb(220,255,220) Unikraft Host
        participant MULTUS as Multus CNI<br/>(Meta-CNI)
        participant ICNI as IPAM CNI
        participant GCNI as Galactic CNI
        participant BGP as BGP
        participant HOST as Linux Network<br/>Stack
        participant UK as Unikraft Runtime
    end

    Note over API,IPAM: Compute operator provisions network resources

    API->>CO: Watch: Instance created<br/>(compute.datumapis.com/v1alpha)
    activate CO
    CO->>CO: Resolve networkInterfaces<br/>(Network → NetworkContext → Subnet)
    CO->>NSO: Request IP for network interface
    activate NSO
    NSO->>IPAM: Create AddressClaim<br/>(from Subnet's AddressPool)
    activate IPAM
    IPAM->>IPAM: Allocate private IP<br/>from AddressPool
    IPAM-->>NSO: AddressClaim bound<br/>(allocated IP)
    deactivate IPAM
    NSO-->>CO: Allocated IP address
    deactivate NSO
    CO->>API: Create VPCAttachment<br/>(VPC ref, interface name,<br/>allocated IP, routes)
    deactivate CO

    Note over UP,GO: Unikraft provider creates Pod

    API->>UP: Watch: Instance created
    activate UP
    UP->>UP: Validate sandbox runtime
    UP->>UP: Add finalizer to Instance
    UP->>API: Read VPCAttachment for Instance
    API-->>UP: VPCAttachment (name, ready status)
    UP->>UP: Translate container spec to Pod spec<br/>(image, env, resources, volumes)
    UP->>UP: Set nodeSelector and tolerations<br/>for Kraftlet scheduling
    UP->>API: CreateOrPatch Pod<br/>(with VPCAttachment annotation)
    deactivate UP

    Note over API,BGP: Galactic operator wires instance into VPC

    API->>GO: VPCAttachment created
    activate GO
    GO->>GO: Assign VPCAttachment identifier<br/>(used in SRv6 encoding)
    GO->>API: Create NetworkAttachmentDefinition<br/>(Multus NAD: IPAM CNI + Galactic CNI chain)
    deactivate GO

    API->>GO: Pod created with<br/>VPCAttachment annotation
    activate GO
    GO->>GO: Mutating webhook injects<br/>Multus network annotation<br/>(k8s.v1.cni.cncf.io/networks)
    deactivate GO

    API->>KL: Pod scheduled to Kraftlet node
    activate KL

    KL->>KL: Parse Pod spec into<br/>unikernel instance config

    KL->>MULTUS: CNI ADD<br/>(namespace, container ID, ifname)
    activate MULTUS

    MULTUS->>ICNI: CNI ADD<br/>(IPAM plugin invocation)
    activate ICNI
    ICNI->>IPAM: Request IP allocation<br/>(VPCAttachment ref)
    activate IPAM
    IPAM->>IPAM: Lookup AddressClaim<br/>confirm allocation
    IPAM-->>ICNI: Allocated IP + prefix + gateway
    deactivate IPAM
    ICNI-->>MULTUS: IPAM Result<br/>(IP, routes)
    deactivate ICNI

    MULTUS->>GCNI: CNI ADD<br/>(ifname, IP config from IPAM CNI)
    activate GCNI
    GCNI->>HOST: Create VRF interface<br/>(network isolation)
    activate HOST
    HOST-->>GCNI: VRF created
    GCNI->>HOST: Create TAP device<br/>(unikernel network interface)
    HOST-->>GCNI: TAP device created<br/>(tap name)
    GCNI->>HOST: Attach TAP device to VRF
    GCNI->>HOST: Create veth pair<br/>(host: G{vpc}{att}H,<br/> guest: G{vpc}{att}G)
    GCNI->>HOST: Assign IP address to<br/>guest interface
    GCNI->>HOST: Configure routes in VRF<br/>(proxy ARP/NDP)
    GCNI->>HOST: Program SRv6<br/>encap/decap routes
    HOST-->>GCNI: Host network provisioned
    deactivate HOST
    GCNI->>BGP: Announce SRv6 endpoint<br/>via BGP control plane
    activate BGP
    BGP->>BGP: Advertise route to peers<br/>("VPC X reachable at<br/>SRv6 endpoint Y")
    deactivate BGP
    GCNI-->>MULTUS: CNI Result<br/>(TAP device name, IP, routes)
    deactivate GCNI

    MULTUS-->>KL: CNI Result<br/>(TAP device name, IPs, routes, DNS)
    deactivate MULTUS

    KL->>UK: Create unikernel instance<br/>(image, resources, TAP device name)
    activate UK
    UK->>HOST: Bind TAP device<br/>(attach instance to VRF)
    activate HOST
    HOST-->>UK: TAP device bound
    deactivate HOST
    UK-->>KL: Instance running
    deactivate UK

    KL->>API: Update Pod status<br/>(phase: Running, podIP, conditions)
    deactivate KL

    Note over API,UP: Status sync

    API-->>UP: Watch: Pod status updated
    activate UP
    UP->>UP: Map Pod status to Instance status<br/>(podIP → networkInterfaces[].networkIP)
    UP->>API: Update Instance status<br/>(Running, Ready, networkInterfaces)
    deactivate UP
```

## Instance Deletion Flow

```mermaid
%%{init: {
  "sequence": {
    "showSequenceNumbers": true
  }
}}%%
sequenceDiagram
    box rgb(199,228,255) Datum Cloud
        participant API as API Server
        participant CO as Compute Operator
        participant NSO as Network Services
        participant IPAM as IPAM Service
        participant UP as Unikraft Provider
        participant KL as Kraftlet
    end
    box rgb(220,255,220) Unikraft Host
        participant MULTUS as Multus CNI<br/>(Meta-CNI)
        participant ICNI as IPAM CNI
        participant GCNI as Galactic CNI
        participant BGP as BGP
        participant HOST as Linux Network<br/>Stack
        participant UK as Unikraft Runtime
    end

    API->>UP: Watch: Instance deleted
    activate UP
    UP->>API: Delete Pod
    deactivate UP

    API->>KL: Pod terminating
    activate KL

    KL->>UK: Stop unikernel instance
    activate UK
    UK->>HOST: Detach from TAP device / VRF
    activate HOST
    HOST-->>UK: Detached
    deactivate HOST
    UK-->>KL: Instance stopped
    deactivate UK

    KL->>MULTUS: CNI DEL<br/>(release network resources)
    activate MULTUS

    MULTUS->>GCNI: CNI DEL<br/>(release VPC resources)
    activate GCNI
    GCNI->>HOST: Remove SRv6 routes
    activate HOST
    GCNI->>BGP: Withdraw route announcement
    activate BGP
    BGP->>BGP: Remove route from peers
    deactivate BGP
    GCNI->>HOST: Remove veth pair
    GCNI->>HOST: Remove TAP device
    GCNI->>HOST: Remove VRF interface
    HOST-->>GCNI: Host network cleaned up
    deactivate HOST
    GCNI-->>MULTUS: Cleanup complete
    deactivate GCNI

    MULTUS->>ICNI: CNI DEL<br/>(release IP allocation)
    activate ICNI
    ICNI->>IPAM: Release IP allocation<br/>(VPCAttachment ref)
    activate IPAM
    IPAM->>IPAM: Return IP to AddressPool
    IPAM-->>ICNI: Released
    deactivate IPAM
    ICNI-->>MULTUS: Released
    deactivate ICNI

    MULTUS-->>KL: Cleanup complete
    deactivate MULTUS

    KL->>API: Pod terminated
    deactivate KL

    API-->>UP: Watch: Pod gone
    activate UP
    UP->>API: Remove finalizer from Instance
    deactivate UP

    Note over CO,NSO: Compute operator cleans up network resources

    API->>CO: Watch: Instance deleted
    activate CO
    CO->>API: Delete VPCAttachment
    CO->>NSO: Release IP allocation
    activate NSO
    NSO->>IPAM: Delete AddressClaim
    activate IPAM
    IPAM->>IPAM: Confirm IP returned<br/>to AddressPool
    IPAM-->>NSO: Released
    deactivate IPAM
    NSO-->>CO: Released
    deactivate NSO
    deactivate CO
```
