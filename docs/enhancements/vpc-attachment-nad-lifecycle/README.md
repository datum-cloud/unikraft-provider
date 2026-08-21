---
status: provisional
stage: alpha
---

# Where NetworkAttachmentDefinition lifecycle belongs

- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [What exists today](#what-exists-today)
  - [Galactic: data plane only, NAD assumed to exist](#galactic-data-plane-only-nad-assumed-to-exist)
  - [cloud: VPC/VPCAttachment API with no controller](#cloud-vpcvpcattachment-api-with-no-controller)
  - [NSO: claims, interfaces, addresses, and an unowned Programmed](#nso-claims-interfaces-addresses-and-an-unowned-programmed)
  - [compute: intent only](#compute-intent-only)
  - [unikraft-provider: the Pod, the runtime, and the CNI opt-in](#unikraft-provider-the-pod-the-runtime-and-the-cni-opt-in)
  - [infra: what is actually deployed in a cell](#infra-what-is-actually-deployed-in-a-cell)
- [Findings that constrain the design](#findings-that-constrain-the-design)
- [Making one NAD per VPC work](#making-one-nad-per-vpc-work)
  - [Where the identifier has to be unique](#where-the-identifier-has-to-be-unique)
  - [Option A: node-qualify the name, allocate from the kernel](#option-a-node-qualify-the-name-allocate-from-the-kernel)
  - [Option B: keep the name cluster-wide, allocate through the API](#option-b-keep-the-name-cluster-wide-allocate-through-the-api)
  - [Option C: a node-level controller hands out blocks](#option-c-a-node-level-controller-hands-out-blocks)
  - [Deriving the identifier from a UID](#deriving-the-identifier-from-a-uid)
  - [Blockers common to all options](#blockers-common-to-all-options)
  - [What a per-VPC NAD buys](#what-a-per-vpc-nad-buys)
- [Proposal](#proposal)
  - [Division of labor](#division-of-labor)
  - [Sequence](#sequence)
  - [Target shape with a shared NAD](#target-shape-with-a-shared-nad)
  - [Why not the alternatives](#why-not-the-alternatives)
- [The API surface and the bridge](#the-api-surface-and-the-bridge)
  - [NSO already defines the seam](#nso-already-defines-the-seam)
  - [The layer cake](#the-layer-cake)
  - [Downward: intent to data plane](#downward-intent-to-data-plane)
  - [Upward: truth to status](#upward-truth-to-status)
  - [Changes VPCAttachment needs to serve as the bridge](#changes-vpcattachment-needs-to-serve-as-the-bridge)
  - [Naming and ownership](#naming-and-ownership)
- [Plan of record: capability classes drive attachment](#plan-of-record-capability-classes-drive-attachment)
  - [The layering](#the-layering)
  - [What each component stops knowing](#what-each-component-stops-knowing)
  - [Scope: Unikraft and tap only](#scope-unikraft-and-tap-only)
  - [Why compute can decide this after all](#why-compute-can-decide-this-after-all)
  - [The concrete contract](#the-concrete-contract)
  - [Two rejected shapes, and why](#two-rejected-shapes-and-why)
- [Superseded: a NAD per network interface, provider-driven](#superseded-a-nad-per-network-interface-provider-driven)
- [If the kraftlet change ever lands: one NAD per VPC](#if-the-kraftlet-change-ever-lands-one-nad-per-vpc)
- [Sequencing](#sequencing-1)
- [API and behavior changes this requires](#api-and-behavior-changes-this-requires)
- [Open questions and validation items](#open-questions-and-validation-items)

## Summary

A compute Instance reaches a tenant VPC through four layers, each deciding one
thing: the **user** picks a capability class, the **location** binds that name to a
handler it offers, the **runtime class** states the consequences — including whether
the guest consumes a NIC through a network namespace or a hypervisor — and the
**data plane** realizes it.

NAD lifecycle belongs to **a VPC controller running in the cell** — the "companion
operator" that `galactic` and `network-services-operator` both already assume exists
and that nobody has written yet. It creates the `VPCAttachment` and the
`NetworkAttachmentDefinition` when a network interface claim is fulfilled, and
publishes an opaque set of annotations for whoever consumes the interface.

An infrastructure provider reads its own Instance, follows it to the
`NetworkInterface`, copies those annotations onto its Pod, and creates the Pod.
It never learns what a VPC, a NAD or a tap device is. Galactic's CNI chain is
unchanged: it reads identifiers it did not choose.

The first implementation covers one path — the Unikraft runtime provider,
`Hypervisor` mode, `galactic-tap`. See
[Plan of record](#plan-of-record-capability-classes-drive-attachment); the sections
before it record how the design arrived there, including two conclusions it
reverses.

## Motivation

An Instance today gets whatever networking `kraftlet` and `ukp-remote-cni` give
it. To put an instance on a tenant VPC something has to author a
`NetworkAttachmentDefinition` carrying the base62 `vpc`/`vpcattachment`
identifiers galactic's chain keys on, put it in a namespace Multus can resolve,
and reference it from the Pod *before* the sandbox is created. Five repos each
hold one piece of that and none of them holds the whole thing.

### Goals

- Name one owner for NAD create/update/delete, with a clear contract to the four
  other components.
- Keep the ordering guarantee that makes attachment work at all: the NAD exists
  and is complete before the Pod is created.
- Keep compute free of networking internals, and keep galactic free of Datum APIs.
- Make NAD lifecycle work identically for containers (`galactic-veth`) and for
  Unikraft microVMs (`galactic-tap`), which is the case this repo cares about.

### Non-Goals

- Choosing addresses. NSO's `NetworkInterfaceClaim`/`NetworkInterface` already
  decides that, and this proposal consumes the answer.
- Programming the data plane. That is galactic's CNI chain and `galactic-router`.
- Retiring `NetworkBinding`/`NetworkContext`/`SubnetClaim`/`Subnet`.

## What exists today

### Galactic: data plane only, NAD assumed to exist

`galactic` main removed the VPC/VPCAttachment CRDs, the operator, the pod
mutating webhook and the NAD generation in commit `c86a45c` ("VPC and
VPCAttachment CRD management is moving to a separate operator project"). What
remains is six binaries staged by the `galactic-cni` installer DaemonSet, and the
chain is authored elsewhere:

```json
{"cniVersion":"1.0.0","name":"…","plugins":[
  {"type":"galactic-tap","vpc":"…","vpcattachment":"…","namespace":"galactic-system",
   "ipam":{"type":"galactic-ipam","ipv6_subnet":"…"}},
  {"type":"galactic-bgp","vpc":"…","vpcattachment":"…","namespace":"galactic-system"}]}
```

Three things in the current code state the contract explicitly:

- `internal/nadpatch.AnnotateNAD` — "The NAD is expected to already exist
  (created by the external VPC operator before the CNI is invoked), so a
  not-found response is a hard failure."
- `internal/nadpatch.VerifyChainComplete` — the master plugin fetches its own NAD
  and **fails ADD before creating kernel state** if `galactic-bgp` is missing from
  the `plugins` list (galactic#331). A malformed conflist is a hard failure, not a
  degraded attach.
- `docs/agents/ARCHITECTURE-CNI.md` — "VPC and VPCAttachment CRDs are owned by a
  separate companion operator (`go.datum.net/cloud`)."

The example NAD in the request is stale in two ways: `"type": "galactic-cni"` is
not a CNI plugin at all (it is the installer binary — "no NAD ever names it in a
`type` field"), and a single-plugin config with no `galactic-bgp` stanza is
rejected by `VerifyChainComplete`. The shape that works today is the conflist
above, and for a microVM the master plugin is `galactic-tap`, not `galactic-veth`.

### cloud: VPC/VPCAttachment API with no controller

`datum-cloud/cloud` (`cloud.datumapis.com/v1alpha1`) is where VPC and
VPCAttachment landed after galactic dropped them. Its README is blunt: "API-only,
no controller… no controller, no runtime, no binaries." `VPCAttachment.spec` is
`{vpc, interface{name, addresses[]}}`; `status` carries the base62 `vpc` and
`vpcAttachment` identifiers plus node-only facts (`node`, `containerID`,
`podName`, `hostInterface`, `vrfInterface`, `podSubnet`).

**Nothing reconciles these types anywhere in the org today.** The identifier
allocation the deleted galactic operator did (`vpcattachment_controller.go`:
list-and-retry until an unused 16-bit id is found, then `CreateOrUpdate` the NAD
with an owner reference) has no home.

### NSO: claims, interfaces, addresses, and an unowned Programmed

NSO main implements `NetworkInterfaceClaim` → `NetworkInterface`, allocates
addresses, and runs in the POP cell. Two hooks matter here:

- `NetworkInterface.status.vpc` — "the base62 identifier of the VPC backing this
  network in this location… The provider records it when the attachment is
  programmed." Declared, never written.
- `seedProgrammed()` — "The data plane owns this condition; overwriting it would
  revert whoever reported the attachment." Seeded `Unknown`, never set.

`docs/enhancements/network-interfaces.md` sketches the intended chain —
`NetworkInterfaceClaim → NetworkInterface → VPCAttachment → VPC` — and says "the
agent on the node creates the `VPCAttachment` from the interface." NSO imports
nothing from `go.datum.net/cloud`; none of this is wired.

### compute: intent only

`InstanceNetworkInterface` names a network, an interface name, address families,
a reclaim policy and address classes. `WorkloadDeploymentReconciler` creates one
`NetworkInterfaceClaim` per instance per interface in the cell, and releases the
per-instance `Network` scheduling gate when that instance's claims are Bound and
Allocated. `networkInterfaceClaimSatisfied` says why the gate does not wait for
more: "Programmed is deliberately not consulted: no component sets it today…
Tighten this to Ready once a data plane owns Programmed."

Compute deliberately knows nothing about VPCs, Multus, or NADs, and the
network-interfaces enhancement makes keeping it that way an explicit goal.

### unikraft-provider: the Pod, the runtime, and the CNI opt-in

This repo's `InstanceReconciler` is a plain single-cluster manager running in the
cell. It watches Instances federated into the cell and, in the Instance's own
namespace, creates the Pod (plus a Service) that `kraftlet` turns into a microVM.
It already:

- honors `spec.controller.schedulingGates` and defers Pod creation while any gate
  remains (`instance_controller.go:105`);
- stamps `cloud.unikraft.v1.instances/cni-enabled` from `config.enableCNI`, which
  is what makes kraftlet route the instance through `ukp-remote-cni` rather than
  keeping networking internal to the runtime;
- builds the Pod spec **only on creation** — annotations are patched every
  reconcile, but the spec is not.

`ukp-remote-cni` is a hostNetwork DaemonSet that reaches `multus-daemon` over
`/run/multus/multus.sock` using the node's real `/opt/cni/bin`, `/etc/cni/net.d`
and `/var/lib/cni`, "shared intentionally so remote-cni integrates with the
cluster's Multus CNI." Per `config/dependencies/kraftlet/README.md` this was
validated on `us-central-1-lab`: an annotated Pod triggers a real authenticated
`Add` into remote-cni, which invokes the node's Multus chain — and kraftlet
`0.6.0-staging.17` "reads the tap device name from the NAD's
`k8s.v1.cni.cncf.io/host-interface` annotation, rather than assuming one."

That annotation is exactly what `galactic-tap` writes via `nadpatch.AnnotateNAD`.
The VM half of the path is already built and proven; the missing piece is
literally the NAD.

### infra: what is actually deployed in a cell

Multus is in the edge stable channel (`channels/edge/stable/infrastructure/multus.yaml`),
`galactic-cni`/`galactic-router`/`fabric-router` are deployed per region under
`apps/galactic-system/`, and the cells that run Unikraft carry both the `nso-cell`
and `compute-unikraft` components. So NSO, compute, the provider, Multus and the
galactic data plane all run in the same cluster and the workload namespace is the
same one for all of them. Nothing has to cross a cluster boundary.

## Findings that constrain the design

**1. As the code stands, a NAD is per attachment.** Host-side interface names
are `G%09s%03s%s` over `(vpc, vpcattachment)` alone — `GenerateInterfaceNameHost`
has no container component — and the two master plugins react differently to
finding the name already taken. `veth.Add` **deletes the existing pair and
recreates it** ("removing stale host veth left behind by a previous ADD
attempt"); `tap.Add` **adopts it** (`repairTap`, "found existing tap from a
previous ADD attempt, repairing state"). Both are written as crash recovery for
a retried ADD by the same workload. Point two live workloads at one NAD and the
first reads as a rollback artifact of the second: the container case tears down a
running pod's interface, and the microVM case silently hands one guest's tap to
another. Sharing a NAD is therefore not merely unsupported today — it fails
quietly on the path this repo cares about. What it takes to change that is
[the next section](#making-one-nad-per-vpc-work).

**2. The NAD must exist, complete, before the sandbox.** Multus resolves the
annotation at sandbox creation, and `VerifyChainComplete` turns a missing or
incomplete NAD into a failed ADD rather than a degraded interface. Any design
where the NAD is a *consequence* of the attach — including the current NSO
sketch, where the node agent creates the `VPCAttachment` — is inverted.

**3. Identifier allocation is unowned and needs a serializing writer.** The 48-bit
VPC id and the 16-bit per-VPC attachment id have to be unique within a fabric.
The deleted galactic controller did list-and-retry against the API. Something has
to own that again, and it cannot be a CNI plugin invocation.

**4. NSO's addresses and galactic's IPAM do not currently meet.** NSO hands out a
specific address per interface (an IPv6 `/96` endpoint block, optionally an IPv4
`/32`, plus a gateway). `galactic-ipam`'s `static_ip` path takes a single IPv6
address, forces a `/64` mask, and allocates no IPv4 at all; its pool path expects
a `/48`-ish region CIDR to carve a `/96` out of. Neither faithfully carries what
NSO already decided. Feeding the region pool into the NAD and letting
`galactic-ipam` choose means NSO's allocation is fiction.

**5. A NAD is a raw CNI config, so it is a privilege boundary.** Anyone who can
create a NAD in a namespace can name any `vpc`/`vpcattachment` pair and attach to
another tenant's VRF. NAD write access in cell workload namespaces has to stay
platform-only regardless of who owns the lifecycle.

**6. `default-network` vs secondary network is a real choice for microVMs.**
`galactic-tap` never enters the pod netns — the tap lives in the host namespace
and the fd goes to the VMM. Using `v1.multus-cni.io/default-network` replaces
Cilium for the sandbox, which costs the Pod its cluster identity, its Pod IP and
the Service this provider creates alongside every Instance. Using
`k8s.v1.cni.cncf.io/networks` keeps Cilium on `eth0` for the sandbox and adds the
galactic tap for the guest. For containers on a VPC the first is right; for
Unikraft microVMs the second looks right, and it is the variant not yet tested.

## Making one NAD per VPC work

The goal: the NAD carries `vpc` and no `vpcattachment`, and the master plugin
allocates an attachment identifier at ADD time, unique within the VPC. Below is
what the current code requires for that to be safe.

### Where the identifier has to be unique

| Consumer | Scope it actually needs |
|---|---|
| `G<vpc9><att3>{H,G}` host/guest/tap interface names | the node |
| `ifindex_vrf_table` eBPF rows, `FORWARD` iptables rules | the node |
| **`BGPAdvertisement` named `<vpc>-<att>` in `galactic-system`** | **the whole cell** |
| VRF interface, `BGPVRFInstance`, SRv6 Argument, route target | not keyed on the attachment at all — all `(vpc, node)` or `vpc` |

Only one row is a problem. `crdnames.BGPAdvertisementName` says it outright:
"Each VPCAttachment is unique per interface across the cluster, so the
`(vpc, vpcAttachment)` pair is a reliable 1:1 key." A node-local allocator breaks
that assumption, and the failure is silent rather than loud: two nodes landing on
the same id for the same VPC converge on one CRD, each `CreateOrUpdate` flips
`spec.routerName` to its own router, both nodes' prefixes merge into one
advertisement pointing at whichever node wrote last, and
`pruneDeadContainerAnnotations` — which decides liveness by checking whether a
netns path exists *on this node* — deletes the other node's live annotations on
sight. That is a cross-node blackhole with no error anywhere.

So the question is not "can the CNI generate an id" — it can — but "against what
registry." There are two honest answers.

### Option A: node-qualify the name, allocate from the kernel

**Simplest, but see Option C.** Change `BGPAdvertisementName(vpc, att)` to include the node, as
`BGPVRFInstanceName(vpc, node)` already does. The required uniqueness scope
collapses from the cell to `(vpc, node)`, and at that scope **the node's own link
table is the registry**: scan for `G<vpc9>???H`, take the lowest free value, and
let `netlink.LinkAdd` be the atomic claim — `EEXIST` means lost the race, try the
next. No API round trip on the ADD path, no lease, no cross-node race, and
`cmdDel` already deletes the interface ("genuinely private to this attachment —
no sibling pod can ever share it"), so identifiers free promptly without a GC
pass.

This is safe because a `BGPAdvertisement` is **already node-owned in everything
but its name**: `buildAdvertisementSpec` stamps this node's `routerName`, and
`internal/gc` filters by `RouterRef` precisely because "a namespace can hold CRDs
created by routers on other nodes… rather than risk deleting another node's live
resources because they look orphaned from here." `vpcFromName` cuts at the first
`-`, so a node segment containing `-` does not disturb VPC extraction — the code
already tolerates exactly that for `BGPVRFInstanceName`.

The cost is that an attachment id stops being a cell-wide handle. Nothing off-node
reads one today — remote reachability is the advertised prefix plus the SRv6
locator and Argument, and the Argument is the per-`(vpc, node)` VRFID — but any
future cross-node lookup keyed on `(vpc, att)` would be foreclosed. Renaming a CRD
is also a delete-and-recreate on upgrade; `legacyVRFNameRegex` is the precedent
for how galactic has handled its own rename before.

### Option B: keep the name cluster-wide, allocate through the API

Precedent exists in the same file: `allocateArgument` lists, picks the lowest free
slot, writes, and then `checkArgumentCollision` re-reads and fails the ADD so the
rollback path retries. That works today because the keyspace is per router — one
node's own attachments contend with each other and nothing else.

Per-VPC-cell-wide it is a different problem: every node attaching to a popular VPC
races on one keyspace on every pod start, and each ADD pays a full `List` of
`BGPAdvertisement`. If this is the direction, do not port the list-then-write
shape — claim with `Create` on an object named for the candidate id and treat
`AlreadyExists` as "taken, next." `Create` is an atomic answer from the API
server; read-then-write is a race with a detector bolted on.

Either way the allocation has to happen in the **master plugin**, which needs the
id for interface naming, not in `galactic-bgp` which runs later. `galactic-bgp`
would then take the id from `prevResult` rather than its own stanza — consistent
with how it already works, since it "learns everything it needs (interface kind,
allocated addresses) from `prevResult` alone."

### Option C: a node-level controller hands out blocks

**This is the better answer, and it does not require Option A's rename.**

The awkwardness in Options A and B is that a short-lived CNI process is being
asked to do cell-wide coordination on the ADD path. Move the coordination off
that path: a per-node controller claims a **block** of attachment identifiers for
each `(vpc, node)` it serves, and the CNI allocates from its node's block using
purely local state. Cell-wide uniqueness then holds by construction — no race, no
detector, no rename, and `status.vpcAttachment` keeps its cell-wide meaning. It is
the same shape as node podCIDR allocation.

**No new component is needed.** `galactic-router` already is this controller: a
per-node DaemonSet running a controller-runtime manager with `NodeName`, a
Kubernetes client, per-node reconcilers over the BGP CRDs, and a time-driven GC
pass (`GCReconciler`, `gc.RunGC(ctx, k8s, namespace, nodeName)`) that already
reclaims this node's orphaned advertisements and VRFs. A block allocator belongs
beside that GC pass, which is also the natural place to release a block once the
node holds no live attachment on that VPC.

Shape:

- A small CRD (or an annotation on the node's `BGPRouter`) recording
  `(vpc, node) → [base, size)`. Claim with `Create`, so `AlreadyExists` is an
  atomic answer.
- The CNI allocates within the block from the kernel link table, exactly as in
  Option A — scan `G<vpc9>???H`, lowest free, `LinkAdd` is the claim.
- The first attachment for a `(vpc, node)` pays one block claim; every subsequent
  one pays nothing. There is already an API round trip on the ADD path for
  `VerifyChainComplete`, the NAD annotation and the BGP CRDs, so this is not a new
  class of latency.

The constraint to size deliberately: the interface-name budget fixes the
attachment segment at 3 base62 characters, so a VPC has 238,328 identifiers total.
Blocks of 256 give ~930 nodes per VPC; if the old 16-bit framing is kept, 65,536
identifiers give 256 nodes per VPC at the same block size. Either is fine today
and neither is comfortable forever — which is an argument for
[decoupling the interface-name segment from the identifier](#deriving-the-identifier-from-a-uid)
below, not for a bigger block table.

### Deriving the identifier from a UID

Hashing a `NetworkInterface` UID down to the attachment segment does not work, and
the reason is just the keyspace. Three base62 characters is 238,328 values, and
collisions are birthday-bound:

| Attachments in one VPC | P(collision), 238,328 values | P(collision), 16-bit |
|---|---|---|
| 50 | 0.5% | 1.9% |
| 100 | 2.1% | 7.3% |
| 250 | 12.2% | 37.8% |
| 500 | 40.7% | 85.1% |
| 1000 | 87.7% | 99.9% |

A collision is not a retry here — it is two attachments converging on one
`BGPAdvertisement` and one interface name. Hash-then-probe fixes that, but probing
needs a registry and a serialization point, which is the thing hashing was meant
to avoid. So a hash alone is out at any scale worth building for.

The 15-character interface-name limit is the real reason base62 exists here:
`G%09s%03s%s` spends 9 characters on a 48-bit VPC (`62^8` is short of `2^48`, so
nine is the minimum) and 3 on the attachment, for 14 of 15. A UID cannot go there
and nothing below proposes putting one there.

**The useful version of the idea is to stop making one value do both jobs.**
Nothing reverse-parses a host, guest or tap interface name anywhere in galactic —
only the *VRF* name is parsed back (`vrfNameRegex`, `legacyVRFNameRegex`, both
matching `G<vpc9>V`), and host names are always regenerated from `(vpc, att)`
rather than read. The attachment segment in an interface name is a pure
uniqueness token, carrying no information anyone recovers. So it can be a small,
dense, node-local index — while the *logical* attachment identity that appears in
`BGPAdvertisement` names, `VPCAttachment.status.vpcAttachment` and anything a
human or an API reads can be something else entirely, with no density constraint.

At that point the UID is not hashed, it is simply used:

- `BGPAdvertisementName(vpc, interfaceUID)` — a Kubernetes UID is lowercase hex
  with dashes, already a valid name segment, and 9 + 1 + 36 characters is nowhere
  near the 253 limit. (The `vpc` segment still needs
  [blocker 1](#blockers-common-to-all-options) fixed.)
- The identifier is **stable across instance replacement**, because the
  `NetworkInterface` is the object that outlives the instance. A replacement
  instance filling the same slot returns to the same attachment identity and the
  same advertisement, instead of creating a new one and leaving the old for GC.
  `bgp.go` already assumes something close to this — "IPAM re-allocates the same
  subnet for the same vpcAttachment identity" — and a UID-derived identity makes
  it true by construction rather than by luck.
- It is idempotent across retried ADDs for free, which a kernel-scan index is not
  when a rolled-back ADD removed the link.
- It is traceable: an advertisement points back at the interface that caused it.

A cheap alternative, if keeping a single value is worth more than the properties
above: there is **exactly one spare character**. `G` + 9 + 4 + 1 is 15, so widening
the attachment segment from 3 to 4 base62 characters takes the per-VPC ceiling from
238,328 to 14,776,336 — a 62x increase for a one-character change, with no second
identifier anywhere. It does not give stability across instance replacement or
traceability back to the interface, and it is an interface-name change with the
same in-place-upgrade concern as any other, but it removes exhaustion as a concern
outright.

The cost of the two-value approach is one more field. The master plugin's own stanza (or, more likely, its
`CNI_ARGS`) has to carry the identity, and its CNI result has to publish both
values so `galactic-bgp` can name the CRD from one and find the interface from the
other — today `galactic-bgp` reconstructs the interface name from its own
`vpcattachment` field via `hostInterfaceIndex(vpc, vpcAttachment)`. That part is
close to free: the master plugin's result already publishes the host interface as
the `Sandbox: ""` entry in `Interfaces`, and `internal/cnibgp/prevresult.go`
already walks that list checking `Sandbox` to tell a tap master from a veth
master. Reading the name it is standing on removes a derivation rather than adding
one. Getting the
identity to the plugin is the easy part: Multus already passes `K8S_POD_UID` and
`K8S_POD_NAME`/`K8S_POD_NAMESPACE` in `CNI_ARGS`, which galactic already parses
(`nadpatch.ParsePodNamespace`), and the plugin already has a Kubernetes client, so
it can read the interface UID from a Pod annotation this provider stamps. Note the
pod UID itself is *not* the right value — it changes on every replacement, which
is exactly the stability the `NetworkInterface` UID gives you.

**Combine C with this.** The node-level block allocator supplies the dense
node-local index the kernel needs; the `NetworkInterface` UID supplies the durable
identity everything above the kernel reads. Neither has to compromise for the
other, and the 238,328-per-VPC ceiling stops being an identity limit and becomes
what it actually is — a limit on concurrent interfaces per VPC per node.

### Blockers common to all options

**1. Base62 identifiers are not valid Kubernetes object names.** `Digits62` is
`0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ` — the value 36
encodes as `A`. `BGPAdvertisementName` and `BGPVRFInstanceName` interpolate base62
straight into `metadata.name`, which must be a lowercase RFC 1123 subdomain. The
37th attachment on a VPC cannot create its CRD. This is **already latent**: a real
48-bit VPC id rendered as 9 base62 characters is near-certain to carry uppercase
(galactic's own test constant is `0000000jU`), and it has gone unnoticed only
because every id in use today is hand-picked and small. Nothing else on this list
matters until this is fixed — base36, lowercase hex, or an explicit sanitize step.

**2. The NAD `host-interface` annotation breaks, and it is the kraftlet
contract.** `nadpatch.AnnotateNAD` writes a single
`k8s.v1.cni.cncf.io/host-interface` per NAD. Share the NAD and every attachment
overwrites it — and kraftlet `0.6.0-staging.17` reads exactly that annotation to
find the tap for its VM. Shared NAD plus microVMs means every guest races to be
the last writer, and a guest that reads at the wrong moment gets another guest's
tap. The tap name is already in the CNI result `galactic-tap` prints, so the fix
is for kraftlet to read the result instead of the annotation. That is a **vendor
change and a hard external dependency for this repo** — worth raising with
Unikraft before committing to a shared NAD. Keying the annotation by containerID
(as `crdnames` already does for the BGP annotations) is possible, but nothing
reads that form.

**3. Retried ADDs must be idempotent.** Allocation has to be a function of the
container, not of call order. Under Option A that falls out of the kernel scan
when the previous attempt's link survived, and a fresh id after a rolled-back ADD
is harmless. Under Option B the claim object must record the containerID and be
looked up before allocating.

**4. Exhaustion and reclaim become live concerns.** Identifiers now churn at pod
replacement rate rather than sitting still with a long-lived NAD. The 15-character
interface-name budget fixes the attachment segment at 3 base62 characters —
238,328 values, and the old operator's 16-bit framing was tighter. Option A
reclaims on DEL for free. Option B needs an explicit release, and DEL deliberately
leaves shared state to `galactic-router`'s GC, so the claim would have to ride
that same pass.

**5. Per-pod addressing gets harder, not easier.** A per-VPC NAD can only carry a
per-VPC IPAM pool. That is the natural shape for `galactic-ipam` — and it is
flatly incompatible with NSO having already allocated this instance's address
(finding 4). The Multus-native way to keep one NAD and still pin an address per
pod is the `ips` runtime capability: `capabilities: {"ips": true}` on the NAD, the
addresses in the pod's own `k8s.v1.cni.cncf.io/networks` JSON. Galactic reads no
`runtimeConfig` and no `capabilities` anywhere today, so that is new work across
`galactic-ipam` and both master plugins. The alternative — let galactic allocate
from a per-VPC pool and have NSO learn the address afterwards — inverts NSO's
design and gives up per-instance retained addresses.

**6. `galactic-route` terminations become per-VPC.** They live in the NAD, so a
shared NAD means a shared set of static routes. Probably an improvement; worth
confirming no attachment will ever need its own.

### What a per-VPC NAD buys

Real, and worth the work if blocker 2 can be cleared with the vendor:

- One NAD per VPC per namespace instead of one per instance NIC — orders of
  magnitude fewer objects, and no per-instance NAD garbage collection.
- **The ordering inversion in finding 2 disappears.** A per-VPC NAD is a
  long-lived object created when a network first lands in a location, so it is
  already there when any Pod is created. The provider stops having to create an
  object and block on it before writing the Pod; it just references a NAD that
  exists.
- **It re-aligns with NSO's own design.** With the id allocated at ADD time,
  `VPCAttachment` becomes purely an observed record written by the node after the
  attach — which is exactly what `network-interfaces.md` describes ("the agent on
  the node creates the `VPCAttachment` from the interface"). The per-attachment
  NAD is what forced that direction to be wrong; removing it makes NSO's sketch
  correct as written.
- This repo's share of the work shrinks to stamping one annotation on the Pod.

What it does not remove: something still has to create and delete the per-VPC NAD,
and still has to allocate the VPC identifier. That is the same controller — it
just gets much smaller, and never touches an instance.

## Proposal

### Division of labor

| Owns | Component | Where it runs |
|---|---|---|
| Network intent, addresses, `NetworkInterface` | NSO | cell |
| Base62 VPC identity per network-in-location | VPCAttachment controller | cell |
| Base62 attachment identity, **the NAD**, VPCAttachment `Ready` | VPCAttachment controller | cell |
| `VPCAttachment` object per instance NIC, Pod annotation, ordering | **unikraft-provider** | cell |
| Kernel state, NAD `host-interface` annotation, BGP/SRv6 | galactic CNI chain | node |
| Instance intent and status | compute | project + cell |

**The NAD is an implementation detail of the galactic data plane, so it belongs to
the controller that owns the galactic control-plane objects — not to the
component that owns the Pod.** The provider knowing "put this annotation on the
Pod" is a one-line coupling; the provider knowing how to render a galactic
conflist, which plugins chain in which order, and which identifiers are free is a
copy of an operator that already needs to exist for containers too.

Concretely, the VPCAttachment controller (the `go.datum.net/cloud` companion
operator galactic already names):

- reconciles `NetworkContext` → `VPC`, allocating the base62 VPC id once per
  network per location;
- allocates the 16-bit attachment id on `VPCAttachment` create;
- renders the conflist and creates the NAD **in the VPCAttachment's namespace,
  with the VPCAttachment as owner reference**, so deletion cascades and no
  finalizer is needed;
- picks the master plugin from a new `spec.interface.type` (`veth` | `tap`);
- reports `Ready` with the NAD's name once the NAD is written;
- watches `VPCAttachment.status` (written by the node agent) and copies the VPC
  identifier and `Programmed` onto the `NetworkInterface`, closing the condition
  NSO deliberately left unowned.

And this repo:

- for each `instance.spec.networkInterfaces[]`, reads the bound `NetworkInterface`
  (guaranteed allocated, because the `Network` gate cleared) and creates
  `VPCAttachment/<instance>-<iface>` in the Instance's namespace with
  `interface.type: tap` and the interface's addresses;
- **blocks Pod creation** until that VPCAttachment is `Ready` — the existing
  scheduling-gate early return is the natural place, and the Pod spec being
  create-only makes blocking mandatory rather than optional;
- stamps the Multus annotation naming the NAD;
- deletes the VPCAttachment with the Pod, under the existing instance finalizer.

### Sequence

```
compute            WorkloadDeployment → Instance (+ Network scheduling gate)
                   WorkloadDeploymentReconciler → NetworkInterfaceClaim
NSO                claim → NetworkInterface, addresses allocated, Bound+Allocated
compute            Network gate released for this instance
unikraft-provider  VPCAttachment/<instance>-eth0   ← spec: vpc, addresses, type=tap
vpc-controller     allocate ids → NAD (owned by the VPCAttachment) → Ready
unikraft-provider  Pod + k8s.v1.cni.cncf.io/networks: <ns>/<instance>-eth0
                                + cloud.unikraft.v1.instances/cni-enabled: true
kraftlet           → ukp-remote-cni → multus → galactic-tap, galactic-bgp
galactic-tap       creates G…H tap, annotates NAD host-interface
kraftlet           reads host-interface, hands the tap fd to the VMM
node agent         writes VPCAttachment.status (node, containerID, hostInterface…)
vpc-controller     NetworkInterface.status.vpc + Programmed=True
```

The only new cross-repo dependency this repo takes is `go.datum.net/cloud` for
the VPCAttachment type, which is an API-only module with no controller-runtime
weight.

### Target shape with a shared NAD

The division of labor above is written for a NAD per attachment, which is what
galactic supports today. Once the shared-NAD work lands it collapses further, and
this is the shape to aim for:

- The controller reconciles `NetworkContext` → `VPC` → **one NAD per VPC per
  namespace**, allocating only the VPC identifier. It never sees an instance.
- The master plugin allocates the attachment identifier at ADD.
- This repo stamps `k8s.v1.cni.cncf.io/networks` on the Pod naming that NAD —
  and, if per-pod addressing goes through the `ips` capability, the addresses
  from the bound `NetworkInterface` in the same annotation. No `VPCAttachment`
  creation, no wait, no teardown beyond the Pod itself.
- The node agent writes `VPCAttachment` as an observed record; the controller
  copies `Programmed` and the VPC identifier onto the `NetworkInterface`.

The two shapes share the same owner, the same objects and the same status path;
the shared-NAD variant only removes work from this repo. Building the
per-attachment shape first is not wasted, but if the galactic changes are cheap
enough to schedule now, skipping straight to this one avoids writing a
create-and-wait step we would then delete.

### Why not the alternatives

**Galactic.** Explicitly de-scoped — the CRDs, the operator and controller-runtime
itself were deleted, and both its docs and its code now assume an external author.
Putting the NAD back means undoing that decision.

**NSO.** Tempting, since NSO owns addresses and runs in the cell. But
"programming the data plane" is an explicit non-goal of its own enhancement, a
NAD is Multus-and-galactic-specific, and NSO's provider contract is deliberately
"a provider reads one resource to configure a NIC." A NAD written by NSO would
make every future data plane an NSO release.

**compute.** Directly against the network-interfaces goal that "compute stops
reaching into networking internals," and compute does not know the runtime is a
microVM needing `galactic-tap`.

**unikraft-provider owns the NAD directly.** This is the shortest path to a
working lab demo and worth doing as a spike, but it hard-codes galactic's
conflist and plugin ordering into a per-vendor provider, gives us no answer for
container runtimes on the same VPC, and leaves identifier allocation with no
serializing owner. If it ships as an interim step it should be behind a config
flag and framed as temporary.

**A mutating pod webhook**, as galactic's deleted `pod_webhook.go` did. It hides
the coupling at admission time, turns a NAD lookup failure into a Pod rejection,
and is unnecessary here because the provider already writes the Pod.

**kraftlet or ukp-remote-cni.** Vendor components; they consume the NAD and must
not author it.

## The API surface and the bridge

### NSO already defines the seam

No new kind is needed. `NetworkInterface.status` already carries a three-field,
deliberately generic hand-off to whatever realizes the interface:

| Field | NSO's own words |
|---|---|
| `attachmentRef {apiGroup, kind, name}` | "the data-plane resource realizing this interface… written by the provider rather than by a user" |
| `vpc` | "the base62 identifier of the VPC backing this network in this location… The provider records it when the attachment is programmed" |
| `conditions[Programmed]` | "The data plane owns this condition; overwriting it would revert whoever reported the attachment" |

`attachmentRef` is polymorphic by design, so the answer to "what type" is simply:
**`cloud.datumapis.com/v1alpha1.VPCAttachment` is what `attachmentRef` points
at.** All three fields exist in NSO main and none of them is written by anything
today.

### The layer cake

```
networking.datumapis.com  NetworkInterfaceClaim → NetworkInterface   NSO         workload ns
        │  status.attachmentRef ─────────────────────────────────┐
cloud.datumapis.com       VPC, VPCAttachment                     │  VPC ctrl    workload ns
        │  renders spec.config                                   │
k8s.cni.cncf.io           NetworkAttachmentDefinition             │  VPC ctrl    workload ns
        │  CNI ADD                                               │
<bgp group>               BGPVRFInstance, BGPAdvertisement  ──────┘  galactic    galactic-system
```

The namespace split is already what happens and is worth keeping: tenant-shaped
objects (`Network`, `NetworkInterface`, `VPC`, `VPCAttachment`, the NAD) in the
workload namespace where RBAC and garbage collection are natural; BGP CRDs in
`galactic-system` where only the platform can touch them.

### Downward: intent to data plane

`VPCAttachment` is the intent object. The provider creates one per instance NIC,
naming the `NetworkInterface` it realizes and copying its addresses; the VPC
controller allocates identifiers and renders the NAD. This direction is
unambiguous and needs only the spec additions below.

### Upward: truth to status

**Do not teach galactic to write `cloud` types.** Galactic's stated scope is the
SRv6 data plane, it imports nothing from `go.datum.net/cloud` today, and adding
that dependency would put Datum API types on the CNI ADD path.

It does not need to, because the data plane already publishes its own truth.
`BGPAdvertisement` is the record galactic writes when an attachment is realized —
named `<vpc>-<att>`, annotated with the netns path and allocated subnet per
container ID — and `galactic-router` sets `Advertised=True` on it from the **live
GoBGP runtime state**, not from the object existing:

```go
if as, ok := advByName[adv.Name]; ok {
    advCopy.Status.AdvertisedPrefixes = as.AdvertisedPrefixes
    setAdvertisementCondition(advCopy, metav1.Condition{Type: ConditionAdvertised, ...})
}
```

That is a stronger `Programmed` signal than anything a purpose-built write-back
would produce, and it already exists. The VPC controller watches
`BGPAdvertisement`, joins on the `(vpc, attachment)` name, and projects onto
`VPCAttachment.status` and from there onto `NetworkInterface.status`
(`vpc`, `Programmed`). Zero galactic changes; galactic stays free of Datum APIs.

### Changes VPCAttachment needs to serve as the bridge

**Status cannot be written partially, and that is the blocking one.** The
generated CRD lists eight required status fields — `containerID`, `hostInterface`,
`node`, `podName`, `podSubnet`, `vpc`, `vpcAttachment`, `vrfInterface`. Because
`status` is one object, none can be set without all of them. Three consequences:
the controller cannot record an allocated identifier before a pod attaches; a
pending or failed attachment cannot be reported through conditions alone; and a
guest managing its own addressing cannot report at all, since `podSubnet` is
required and must be a CIDR — even though galactic explicitly supports that case
and carries `AnnotationNoAddressing` for it. Everything except `conditions` should
be optional.

Spec additions, all small:

| Field | Why |
|---|---|
| `interface.type: veth \| tap` | Selects the master plugin. Nothing in the spec says which one today, and the choice is the difference between a container and a microVM. |
| `interfaceRef {name, uid}` | The back-pointer to the `NetworkInterface`, and the durable attachment identity (see [Deriving the identifier from a UID](#deriving-the-identifier-from-a-uid)). |
| `interface.addresses` allowing zero | Currently `MinItems=1`. A guest managing its own addressing has none. |
| `nodeName` (optional) | Intent, distinct from `status.node`, for the case where the attachment is planned before scheduling. |

The type staying in `datum-cloud/cloud` and the controller living somewhere else
are separable decisions. Galactic's architecture doc already names
`go.datum.net/cloud` as the owner of these CRDs, so leave the types there; where
the binary runs — a first binary in that repo, or a controller in NSO's cell
manager — is the Phase 0 ownership question.

### Naming and ownership

Name every object in the chain after the interface, so a human can follow it
without a lookup. Compute already derives `<instance>-<interface>` in
`networkInterfaceClaimName`; the claim, the interface, the attachment and (in the
per-attachment shape) the NAD should all carry it.

Ownership needs one distinction. The `VPCAttachment` should **not** be owned by
the `NetworkInterface`: under `reclaimPolicy: Retain` the interface deliberately
outlives the instance, and an attachment must not. Own the attachment from the
`Instance` (or the Pod), reference the interface through `spec.interfaceRef`, and
let the NAD be owned by the `VPCAttachment` in the per-attachment shape or by the
`VPC` in the shared one.

None of this changes between the two shapes. The types, the reference, the status
path and the naming are identical; only the NAD's owner moves, and
`status.vpcAttachment` shifts from assigned to learned. The API work in Phase 3 is
not rework.

## Plan of record: capability classes drive attachment

The sections below this one record how the design got here, and two of their
conclusions are now wrong. They are kept because the reasoning still explains why
the alternatives were rejected, but this section is what to build.

### The layering

Four layers, each deciding exactly one thing, each reading only the layer above.

| Layer | Decides | Example |
|---|---|---|
| **User** | the capability they need | `isolated` |
| **Location** | which handler offers that capability here, and advertises the names it has | `isolated` → the Unikraft runtime |
| **Runtime class** | the consequences of that handler — scheduling constraints, overhead, image contract, and **how the guest consumes a NIC** | `Hypervisor` |
| **Data plane** | how to realize it | `galactic-tap` |

A user never names an implementation. `isolated` is a portable product name that
binds to a different handler in each location, the way a `StorageClass` name binds
to a different provisioner in each cloud — which is also what lets one workload
span locations without carrying an unsatisfiable value.

### What each component stops knowing

- **compute** learns one new thing (which capability class an instance runs under)
  and publishes the bound `NetworkInterface` on `Instance.status`. It never learns
  what a VPC, a NAD or a tap device is.
- **NSO** carries the consumption mode from claim to interface without
  interpreting it, exactly as it already carries `interfaceName` and `mtu`, and
  publishes an opaque set of annotations the consumer must apply. It never talks
  to the data plane.
- **The VPC controller** creates the `VPCAttachment` and the NAD when a claim is
  fulfilled, and publishes those annotations. It is the only component that speaks
  both vocabularies.
- **The infrastructure provider** reads the `Instance`, follows it to the
  `NetworkInterface`, copies the published annotations onto its Pod, and creates
  the Pod. That is the whole of its networking involvement.
- **galactic** is unchanged and never sees a Datum API.

### Scope: Unikraft and tap only

The layering above is the target. What gets built now is a single path through it:
the Unikraft runtime provider, `Hypervisor` mode, `galactic-tap`. Concretely:

- No `AttachmentClass`. A cell hosts one networking implementation today, so the
  object that would choose between implementations has nothing to choose. Add it
  when a second one exists; nothing here forecloses it.
- The consumption mode is a **required** setting on the VPC controller, with no
  default, so a cell must state what it is rather than silently falling back to
  veth and handing a microVM an interface it cannot use. Unikraft cells set
  `Hypervisor`.
- The capability class itself is a compute product API and is tracked separately.
  Until it exists, the controller's required setting stands in for it — the
  layering is honoured, one layer is a config value rather than an object.

### Why compute can decide this after all

An earlier section of this document argues that compute cannot know whether a
guest consumes a NIC through a netns or a hypervisor, because a Unikraft
`Runtime.Sandbox` is a microVM while the same `Sandbox` elsewhere is a container.
That argument holds only while `Instance.spec.runtime` is a *shape* discriminator
with no *handler* selector.

A capability class supplies the handler, so the ambiguity disappears. The
discriminator says what spec you write — containers, or a boot image and volumes.
The class says how it executes. Unikraft is the proof they are independent axes:
a `sandbox` by contract, a microVM by implementation.

That also means the mode belongs on the claim, alongside `interfaceName` and
`ipFamilies` — the placement this document previously rejected.

### The concrete contract

Multus knowledge lives in exactly one repo. Whoever writes a
`NetworkAttachmentDefinition` should be the only thing that knows NADs exist, and
that is the VPC controller — it renders the conflist, creates the NAD, and injects
the annotation that delivers it. Nothing else in the platform names a CNI.

- `NetworkInterfaceClaim.spec.attachmentMode`: `Netns` | `Hypervisor`, defaulting
  to `Netns`. Set by compute from the resolved capability class. NSO copies it to
  `NetworkInterface.spec.attachmentMode` and never interprets it. It describes how
  a guest consumes a NIC — a real distinction on any platform, naming no CNI, no
  Multus, and no Linux device type.
- A `Prepared` condition on the claim and the interface, meaning the data plane's
  pre-Pod artifacts exist. Owned by the VPC controller, seeded and then left alone
  by NSO exactly as `Programmed` is. **Safe to gate on**, unlike `Programmed`,
  which only becomes true at CNI ADD and therefore deadlocks anything that gates
  Pod creation on it.
- `Instance.status.networkInterfaces[].networkInterfaceRef`: the bound
  `NetworkInterface`, so nothing has to re-derive compute's private claim-name
  convention. Read by the webhook below.
- A **mutating admission webhook on Pods**, served by the VPC controller. It
  matches only Pods carrying an opt-in annotation, resolves them to their Instance
  and its interfaces, and injects whatever the delivery mechanism requires — today
  `k8s.v1.cni.cncf.io/networks`.
- `VPCAttachment` and the NAD are created by the VPC controller when the claim is
  fulfilled, the attachment owned by the `NetworkInterface` and the NAD by the
  attachment.

The infrastructure provider's entire networking involvement is then: stamp one
opt-in annotation on Pods for instances that request an interface, and honour a
scheduling gate it already honours. It names no CNI, resolves no interface,
creates no object, and waits on nothing it has to understand.

The opt-in annotation is worth keeping even though the webhook could key off the
Pod's owner reference to the Instance. It is what makes the webhook's
`objectSelector` narrow, and a narrow selector is what makes `failurePolicy: Fail`
safe: an outage then blocks exactly the Pods that need an interface, loudly,
instead of either blocking every Pod in the cell or letting Pods come up silently
unattached.

Two costs are real and worth stating. A mutating webhook means the Pod you applied
is not the Pod you get, so it should record what it injected. And it is an
availability dependency on the Pod creation path, which the narrow selector bounds
but does not remove.

Two consequences worth stating plainly. The attachment is per-interface, so its
identifier is stable across instance replacement. And because the controller
creates the attachment, the NAD, and the annotation, it holds every input at every
step — no lookup crosses a component boundary, so there is no naming convention
for two repos to disagree about.

Ownership reverses with it: an earlier section argues the attachment must not be
owned by the `NetworkInterface`, because under `reclaimPolicy: Retain` the
interface outlives the instance. That was correct when the attachment was per-Pod.
It is now per-interface by construction, so interface ownership is right.

### Two rejected shapes, and why

Both were proposed during review, built, and withdrawn. They are recorded because
the reasons are the whole argument for the webhook.

**An opaque annotation map on the interface** — `status.consumerAnnotations`, which
the provider would copy verbatim onto its Pod. It fails four ways. It puts an
instruction in a status field, which should record what is rather than direct a
third party. It assumes the consumer is a Kubernetes Pod. Its opacity is fiction:
the value is `k8s.v1.cni.cncf.io/networks` in plain sight on a published API, so
every reader learns Multus exists whether or not their code is typed against it.
And an untyped string map has no validation, no schema evolution, and no way to
deprecate a key.

**A typed reference to the delivery object** — `status.consumerRef` naming the
`NetworkAttachmentDefinition`. Better on all four counts, and still wrong: it
requires the provider to map a kind to a mechanism, which is Multus knowledge
compressed into a switch statement rather than removed. Once a webhook exists the
field has no external reader at all — the controller that writes the NAD is the
one that injects the annotation — so publishing it on NSO's API is a component
talking to itself through someone else's schema.

## Superseded: a NAD per network interface, provider-driven

Not per instance — **per `NetworkInterface`**. That distinction is what makes this
shape good rather than merely safe.

A `NetworkInterface` is slot-stable: compute derives its claim name from the
instance slot, "so an instance replaced by another filling the same slot derives
the same name," and under `reclaimPolicy: Retain` the interface deliberately
outlives the instance. Bind the NAD to the interface and four things follow:

- The attachment identifier is stable across instance replacement, so a
  replacement returns to the same tap name and reuses its `BGPAdvertisement`
  instead of orphaning one for GC. `bgp.go` already half-assumes this — "IPAM
  re-allocates the same subnet for the same vpcAttachment identity" — and this
  makes it true rather than lucky.
- The NAD's single `k8s.v1.cni.cncf.io/host-interface` annotation is unambiguous
  and stable, which is exactly what kraftlet reads today. **No vendor change.**
- The provider's create-and-wait fires only when a slot is genuinely new, not on
  every instance replacement.
- Object count scales with concurrent instance slots, not with pod churn.

### The invariant it depends on

At most one live Pod per interface at a time. That holds today: compute's stateful
control replaces by delete-then-recreate, with a `WaitAction` gated on
`DeletionTimestamp` before the slot is recreated, and this provider holds a
finalizer until the backing Pod is observed gone. Old is fully gone before new
exists.

It is worth an explicit e2e test, because the failure mode is silent: two live
pods on one NAD means one interface name, and `veth.Add` destroys the incumbent
while `tap.Add` adopts it. If surge replacement is ever wanted, fall back to a NAD
per *instance* and accept reallocating the identifier on every replacement.

### Who allocates the attachment identifier

The controller, not the CNI. A single controller with leader election is a
serializing writer with a cluster-wide cached view — which is precisely what made
the deleted operator's list-and-retry correct, and what made the same approach
unsafe once a short-lived CNI process was doing it.

Two refinements on that original implementation: list from the informer cache
rather than the API on every allocation, and **allocate randomly rather than
lowest-free** (as the deleted operator did) so a freed identifier is not
immediately handed out again while its `BGPAdvertisement` is still being collected.

### What this removes

Everything the CNI-side allocation required is gone: index allocation in the master
plugins, containerID-keyed marker files for DEL, UID plumbing through `CNI_ARGS`,
the `prevResult` rework in `galactic-bgp`/`galactic-route`, and `ips`
runtime-capability support. The NAD carries `vpc`, `vpcattachment` and the exact
address, exactly as galactic already expects.

### What remains in galactic — two items

1. **Name-safe identifiers.** Base62 emits uppercase at 36; `metadata.name` must be
   a lowercase RFC 1123 subdomain. ~99% of random 48-bit VPC identifiers contain
   uppercase, so this breaks on the first generated VPC regardless of anything
   here.
2. **Prefix-preserving dual-stack static IPAM.** `static_ip` today takes one IPv6
   address, forces a `/64` mask and allocates no IPv4. A per-attachment NAD can
   carry the address NSO allocated — but only if the allocator honors it faithfully,
   both families, prefix length preserved.

Everything else is unchanged from the list below: the `cloud` status/spec fixes and
the controller, NSO accepting an external `Programmed` writer, the provider wiring,
infra RBAC, and compute **not** tightening its gate to `Ready` (Programmed becomes
true at CNI ADD, which is after Pod creation, so gating on it deadlocks).

### Ownership, restated for this shape

The NAD is owned by the `NetworkInterface` and named after it. The `VPCAttachment`
is per-instance and carries the pod-scoped facts — container ID, node, pod name —
owned by the `Instance`. Both reference the same allocated identifier; only the
attachment churns with the pod.

## If the kraftlet change ever lands: one NAD per VPC

Retained as the destination, not the plan. Everything below is achievable except
item 7, which is an upstream change to kraftlet we do not control; the plan of
record above avoids needing it.

Skipping the per-attachment shape is coherent, and adopting the UID identity makes
it *smaller* than the phased path suggested — it removes the need for both the
`BGPAdvertisement` rename and the block allocator. One item is not ours to
schedule.

### The design, settled

- The NAD carries `vpc` and no `vpcattachment`, one per VPC per namespace.
- The master plugin allocates a **node-local index** by scanning the kernel for
  `G<vpc9>???H` and taking the lowest free value; `LinkAdd` is the atomic claim
  and `EEXIST` means try the next. No API call, no registry.
- The **`NetworkInterface` UID is the attachment identity** above the kernel.
  `BGPAdvertisement` is named `<vpc>-<uid>`, which is cell-wide unique by
  construction — so the name needs no node qualification and no block allocator
  exists.
- Per-pod addresses arrive through Multus's `ips` runtime capability rather than
  the NAD.

### Required, by owner

**galactic** — the bulk of it.

1. **Name-safe identifiers.** Base62 emits uppercase at 36; `metadata.name` must be
   lowercase. Breaks on the first real 48-bit VPC identifier regardless of this
   work.
2. **Index allocation in the master plugins.** Kernel scan on ADD, plus a
   containerID-keyed marker file so **DEL can recover the index** — the config no
   longer carries it and `prevResult` is not dependable on DEL. `galactic-ipam`
   already does exactly this (`ipam.DefaultLockDir`, flock-guarded, keyed by
   container ID); mirror it.
3. **Identity plumbed through the chain.** The master plugin reads the interface
   UID (`CNI_ARGS` plus a Pod annotation — it already parses `CNI_ARGS` and holds a
   client), publishes it in the result, and `galactic-bgp` names the advertisement
   from it. `galactic-bgp` and `galactic-route` stop deriving the host interface
   name from `(vpc, vpcattachment)` and read it from `prevResult`, where the master
   already publishes it as the `Sandbox: ""` entry.
4. **`ips` runtime capability.** `capabilities: {"ips": true}` on the NAD, addresses
   in the Pod's `k8s.v1.cni.cncf.io/networks` JSON, honored faithfully — both
   families, prefix length preserved. Galactic parses no `runtimeConfig` or
   `capabilities` anywhere today. Without this a per-VPC NAD can only carry a pool,
   and NSO's allocation becomes decorative.
5. **`vpcattachment` removed** from the master, `galactic-bgp`, `galactic-route` and
   `galactic-ipam` stanzas. `galactic-route` terminations become per-VPC.
6. **`nadpatch` stops annotating the shared NAD** with a single `host-interface` —
   see the kraftlet item.

**kraftlet — external, and the only item that cannot be compressed.**

7. Read the tap device name from the **CNI result** instead of the NAD's
   `k8s.v1.cni.cncf.io/host-interface` annotation. With a shared NAD every guest
   races to be the last writer of that one annotation, and a guest reading at the
   wrong moment gets another guest's tap. There is no workaround on our side: we
   control the writer, not the reader. This gates microVMs specifically — which is
   the entire use case for this repo.

**cloud — types and a controller.**

8. `VPCAttachmentStatus`: make everything but `conditions` optional (eight required
   fields today), and widen `status.vpcAttachment` past `MaxLength=16` if it is to
   hold a UID.
9. `VPCAttachmentSpec`: `interfaceRef {name, uid}`, `interface.type: veth|tap`,
   `interface.addresses` allowing zero.
10. A controller: `NetworkContext` → `VPC` (allocate the VPC identifier) → one NAD
    per VPC per namespace; watch `BGPAdvertisement` and project onto
    `VPCAttachment.status` and `NetworkInterface.status`
    (`vpc`, `attachmentRef`, `Programmed`).
11. **Name its owner.** A first binary in a repo that has never shipped one, or a
    controller in NSO's cell manager. This is the largest new component and the
    decision blocks it entirely.

**network-services-operator.**

12. Accept an external `Programmed` writer — RBAC, and correct the enhancement's
    "the agent on the node creates the `VPCAttachment`" to provider-creates-spec.

**compute — one correction, not a change.**

13. **Do not tighten the gate to `Ready`.** `networkInterfaceClaimSatisfied`'s TODO
    says to consult `Programmed` once a data plane owns it. In this design
    `Programmed` becomes true at CNI ADD, which happens at sandbox creation, which
    the provider defers while any scheduling gate remains — so gating on it
    deadlocks. `Programmed` belongs in `Instance.status` readiness, not the gate.
    The gate stays on Bound + Allocated.

**unikraft-provider.**

14. Stamp `k8s.v1.cni.cncf.io/networks` naming the VPC's NAD, carrying the
    interface UID and the addresses for the `ips` capability.
15. Create a `VPCAttachment` per instance NIC as intent — no wait, since the NAD
    already exists — and tear it down under the existing finalizer. Behind a config
    flag beside `enableCNI`.
16. Lab-validate the secondary-network shape (galactic tap attached while Cilium
    stays the default network), which decides whether the instance keeps its Pod IP
    and Service.

**infra.**

17. Restrict NAD write in cell workload namespaces to the controller's service
    account — a NAD is a raw CNI config, so anyone who can write one can name any
    VPC.
18. Deploy the controller into the cells.

### What determines the date

Three things, and only one is technical work anyone here can accelerate: the
controller owner being named (item 11), the galactic changes (1–6), and the
kraftlet change (7). Everything else is small and parallel.

If the vendor will not commit to item 7, the fallback preserves the entire
architecture and costs only object count: **render a NAD per instance from a
per-VPC template**. Identifier allocation stays in the CNI, the UID stays the
identity, the controller and the status path are unchanged, and each attachment
gets its own NAD to annotate. That is the phased shape arrived at from the other
direction — worth knowing it is available cheaply, because it means the external
dependency threatens the object count, not the design.

## Sequencing

The phased alternative, if the vendor dependency above cannot be
resolved on the timeline the product needs. Five phases. Only one of them ships customer-visible capability; the rest exist to
make that one safe or to raise its ceiling.

**Phase 0 — decisions, in parallel with everything.** Three things, none of which
produce code.

- *Ask Unikraft to move kraftlet off the NAD `host-interface` annotation* and onto
  the CNI result. Longest lead time of anything here and it gates the endgame, so
  it starts on day one even though it lands last. A "no" does not stop the product;
  it caps it at one NAD per instance NIC.
- *Lab test the secondary-network shape* — galactic tap attached while Cilium stays
  the pod's default network. Decides whether an instance keeps its Pod IP and the
  Service this provider creates. An afternoon on `us-central-1-lab`.
- *Name the owner of the VPC controller* — a first binary in `datum-cloud/cloud`,
  or a controller in NSO's cell manager. Organizational, not technical, and Phase 3
  cannot start without it.

**Phase 1 — two galactic fixes, independently valuable.** Name-safe identifiers
(base62 produces uppercase; Kubernetes names must be lowercase) is a live bug that
breaks the first time a real 48-bit VPC identifier is used, whatever else happens.
Prefix-preserving dual-stack static IPAM is what lets a NAD carry the address NSO
already allocated instead of a pool for galactic to re-decide from. Neither depends
on anything above.

**Phase 2 — walking skeleton, hand-authored.** Write a NAD by hand in the lab,
point an Instance's Pod at it, and confirm the guest comes up on the VPC. Exit
criteria: a guest on one node reaches a guest on another node in the same VPC, and
nothing else in the cell notices. This validates the whole chain — remote-CNI,
Multus, `galactic-tap`, `galactic-bgp`, SRv6 — before a line of controller code
exists, and it is where the surprises will be.

**Phase 3 — ship it, one NAD per attachment.** The VPC controller (identifiers, NAD
rendering, status propagation), this provider creating a `VPCAttachment` per
instance NIC and waiting on it before the Pod, NSO gaining a `Programmed` writer,
and compute tightening its scheduling gate to `Ready`. This is the customer-facing
capability and it works against galactic **exactly as it ships today** — no vendor
dependency, no shared-NAD work. It is the whole product on the least risk.

**Phase 4 — raise the ceiling, one NAD per VPC.** Only if Phase 0's vendor answer
is yes. Block allocation in `galactic-router`, `NetworkInterface` UID as the
attachment identity, the interface-name index decoupled from that identity, and
the `ips` runtime capability for per-pod addressing. The controller shrinks to one
NAD per VPC and this provider's create-and-wait step is deleted.

Phase 3 is not throwaway work. The controller, the identifier allocation, the NSO
status path and compute's gate all survive Phase 4 unchanged; what Phase 4 removes
is the per-attachment NAD rendering and this repo's wait step. Building Phase 3
first buys a shipping product that does not depend on an external roadmap, and
Phase 4 becomes a scale and operability improvement rather than a prerequisite.

## API and behavior changes this requires

Ordered by who is blocked on whom.

The list below is for the per-attachment shape. The shared-NAD shape replaces
items 1 and 5 with galactic work — the `BGPAdvertisement` rename, the
name-safe-identifier fix, the allocator, and `ips`-capability support — plus the
kraftlet ask in blocker 2. **The name-safe-identifier fix (blocker 1) is not
optional in either shape**: it breaks as soon as real 48-bit VPC identifiers are
in use, independent of any of this.

1. **`cloud` — VPCAttachment spec additions.** `interface.type` (`veth`|`tap`) so
   the controller picks the master plugin; a reference to the owning
   `NetworkInterface` (or instance) so the object is traceable and NAD names are
   derivable; and, if the node agent is to correlate, an intended node name.
   Today `spec` is only `{vpc, interface{name, addresses[]}}`.
2. **`cloud` — a controller.** The repo currently ships no binary. Identifier
   allocation, NAD rendering and status propagation all land here.
3. **`galactic` — IPAM that can carry a decided address.** Either a static path
   that preserves the given prefix length and supports IPv4 alongside IPv6, or an
   explicit "addresses are pre-decided" mode. Finding 4 is a hard blocker for
   NSO-owned addressing; without it the NAD has to use pool IPAM and NSO's
   allocation is decorative.
4. **`network-services-operator` — accept a `Programmed` writer**, and confirm
   the direction of `VPCAttachment` creation in `network-interfaces.md`
   (provider-creates-spec, agent-writes-status) rather than agent-creates.
5. **`unikraft-provider` — the work in this repo.** Add `cloud` types to the
   scheme; a `VPCAttachment` reconcile step per instance NIC; a Ready wait before
   Pod creation; the Multus annotation; teardown under the existing finalizer.
   Gate it on a config flag beside `enableCNI`, since not every cell runs galactic.
6. **`compute` — tighten `networkInterfaceClaimSatisfied` to Ready** once a data
   plane owns `Programmed`. Its own comment already asks for this.
7. **`infra` — RBAC.** NAD create/update in cell workload namespaces restricted to
   the VPCAttachment controller's service account (finding 5).

## Open questions and validation items

- **Secondary network vs `default-network` for microVMs** (finding 6). Needs a lab
  test: does kraftlet/`ukp-remote-cni` correctly pick the galactic tap out of a
  multi-interface Multus result via the NAD `host-interface` annotation when
  Cilium is still the default network? This determines whether an instance keeps
  its Pod IP and Service.
- **Who allocates the VPC id, and from what scope?** Per cell, per metro or
  globally. It must be stable across cells for a network that spans locations,
  since the fabric keys on it — which argues the VPC id belongs upstream with the
  `Network`, not with the per-location `NetworkContext`, even though the
  attachment id is clearly local.
- **Is `NetworkContext` the right trigger for `VPC` creation**, or should the VPC
  object be created by the same provider step that creates the attachment?
- **Attachment id exhaustion.** 16 bits per VPC, allocated by list-and-retry in
  the deleted implementation. At instance churn rates this needs a real allocator
  and a release path, not retry-until-free.
- **Does the node agent path exist at all?** NSO's design has "the agent on the
  node" writing `VPCAttachment.status`. `galactic-router` is the only candidate
  and it does not know these types today.
- **Will Unikraft move kraftlet off the NAD `host-interface` annotation** and
  onto the CNI result? A shared NAD for microVMs is blocked on this, and it is the
  first conversation to have.
- **Is any cell-wide meaning of an attachment identifier worth preserving?**
  Option A gives it up; Options B and C keep it. The `cloud` `VPCAttachment` API
  publishes `status.vpcAttachment` as though it were a global handle, which argues
  for C. Settle the API's intent before choosing.
- **Block size for Option C**, and what happens when a VPC outgrows the node
  count a block size implies. Related: is the 16-bit framing inherited from the
  deleted operator still a real constraint, or only the 3-character
  interface-name segment?
- **Reconciliation of a live NAD.** The provider builds Pod specs only at
  creation and Multus reads the NAD only at sandbox creation, so a NAD edited
  after the fact changes nothing until the instance is replaced. Whether the
  controller should refuse mutations outright is worth deciding early.
