---
status: provisional
stage: alpha
---

# Instance Provisioning via Kraftlet

## Instance Provisioning Flow

```mermaid
%%{init: {
  "sequence": {
    "showSequenceNumbers": true
  }
}}%%
sequenceDiagram
    box rgb(199,228,255) Datum Cloud
        participant API as Cluster API Server
        participant CO as Compute Operator
        participant NSO as Network Services<br/>Operator
        participant IPAM as IPAM Service<br/>(Datum Cloud)
        participant UP as Unikraft Provider<br/>(Instance Controller)
        participant GO as Galactic Operator<br/>(Webhook + Controller)
        participant KL as Kraftlet<br/>(Virtual Kubelet)
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
        participant API as Cluster API Server
        participant CO as Compute Operator
        participant NSO as Network Services<br/>Operator
        participant IPAM as IPAM Service<br/>(Datum Cloud)
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
