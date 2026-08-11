# Unikraft Runtime Architecture

Datum runs unikernel workloads on dedicated bare-metal compute nodes using
Unikraft's platform. This repository holds two things: the **infra provider**
operator, which turns Datum `Instance` resources into pods scheduled onto a
Unikraft node, and the **node runtime manifests**, which put Unikraft's platform
on those nodes as ordinary Kubernetes workloads.

These documents describe how the runtime is put together and the invariants it
depends on. They are written for whoever has to operate or change it, and they
cover behavior that outlives any single change.

## Node Runtime

Every compute node runs the `ukp-runtime` DaemonSet, which packages Unikraft's
host components in one image:

| Component | Role |
|-----------|------|
| `ukpd` | The platform daemon. Owns guest microVMs, images, and node state. |
| `agent` | The host agent. Owns the node's license and image pulls, and serves `ukpd` over a local socket. |
| `coredns` | Answers DNS for guests, which resolve through their host-side gateway. |
| `netsetup` | Prepares host networking for guests before the runtime starts. |

Alongside it, a Unikraft **kraftlet** joins the node to the cluster as a virtual
kubelet, and the provider operator schedules instances onto it. The
`ukp-remote-cni` DaemonSet is deployed separately so it can integrate with the
node's real CNI configuration.

Runtime state lives on the node, not in the pod: the runtime's data directory is
a host path backed by a quota-enabled filesystem, which is what makes a node's
identity and its license survive pod restarts and image bumps.

## Packaging and Deployment

Runtime configuration is Kustomize, published from this repository as an OCI
bundle:

- [`config/dependencies`](../../config/dependencies) holds the bases, which are
  self-contained and have no cluster dependencies. The hermetic kind e2e applies
  these.
- [`config/overlays`](../../config/overlays) holds the real-cluster overlays,
  which add what only a real cluster can provide — credentials, generated secrets,
  node-specific configuration, and node activation.

Whatever deploys a cluster consumes that bundle and owns the cluster-side inputs
the overlays expect, credentials in particular. The split keeps runtime manifests
deployable from the published bundle into any environment, while cluster
credentials stay with the cluster.

## Documents

- [Node Licensing](./node-licensing.md) — how a node obtains and maintains its
  Unikraft node license, and what binds a license to a specific host
