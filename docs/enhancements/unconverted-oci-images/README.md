---
status: provisional
stage: alpha
---

# Running Unconverted OCI Images on the Unikraft Runtime

- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [How It Works](#how-it-works)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [Gating](#gating)
  - [Conversion](#conversion)
  - [Serving](#serving)
  - [Caching](#caching)
- [Enablement and Rollback](#enablement-and-rollback)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)

## Summary

Today, an image must be built with `datumctl compute build` before it can run on
the Unikraft runtime. This enhancement moves to a model similar to Kata
Containers, where users deploy ordinary OCI images and the nodes convert them
before their Pods are scheduled. Because conversion happens on the node, there
is no build service or extra registry to operate, and pulls go through the
node's containerd, reusing layers already on the node. Images built with the CLI
continue to work as they do today, so users who build images locally can keep
doing so.

## Motivation

Requiring a build step means every user has to build and push a dedicated image
before deploying, even when the same image already runs on any other container
platform. Moving that build into the platform as a central service would spare
users the step, but it would add a service and a registry to operate.

Kata Containers runs ordinary images in VMs without a separate build: each node
pulls images through containerd and shares their unpacked layers with the guest
as its root filesystem. The Unikraft runtime can't use unpacked layers as its
root, but it can boot from a single filesystem image such as EROFS, and building
one from the unpacked layers is quick. That makes the same node-local model
practical here.

### Goals

- Run any `linux/amd64` OCI image without a build step.
- Avoid a central build service, an extra registry, and changes to Unikraft
  components.
- Pull through the node's containerd using the Pod's pull Secrets, reusing
  layers already on the node.
- Keep images built with `datumctl compute build` working unchanged.
- Keep the feature opt-in, with minimal changes to the provider.

### Non-Goals

- Removing `datumctl compute build`. Users who build images locally can keep
  using it, including its checks for missing libraries in minimal images; other
  images no longer need it.
- Supporting other architectures now. Target platforms are configuration, so
  adding one later shouldn't require code changes.
- Bridging container runtime semantics. The runtime doesn't apply every image
  setting a container runtime does; closing that gap is a separate enhancement,
  which would also take over the adjustments `datumctl compute build` makes.

## Proposal

Add a node-local component, `ukp-oci-adapter`, that runs alongside the
runtime on every Unikraft node. The provider creates Instance Pods behind a
scheduling gate, which gives an adapter the chance to prepare each Pod's images
in the runtime's format before the Pod is scheduled. Once the images are ready,
the adapter points the Pod at them and removes the gate, and from there the Pod
is scheduled and started exactly as it is today.

The design relies only on documented interfaces: the runtime's `http+oci` image
transport, containerd's client API, and Kubernetes scheduling gates. It needs no
changes to Unikraft components, and nothing in it depends on their internals.

### How It Works

```
Instance ─▶ Pod (gated, image: docker.io/library/nginx:1.27)
              │
              ▼  an adapter claims the Pod
   pull through node containerd ─▶ build EROFS image ─▶ store in containerd
              │
              ▼
            Pod (not gated, image: http+oci://ukp-oci-adapter/…@sha256:…)
              │
              ▼  scheduled and started as today
   the runtime fetches the image from the adapter on its node
```

### Risks and Mitigations

| Risk                                                  | Mitigation                                                                                                                              |
| ----------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------- |
| An adapter stops partway through a conversion.        | Claims expire, so another adapter picks the Pod up.                                                                                     |
| A user tries to fetch another project's image.        | Users can't name converted images, since Instances with `http+oci` images are rejected, and only the node itself can reach its adapter. |
| Converted images fill the node's disk.                | Converted images are kept separately from the kubelet's and held under a size limit.                                                    |
| Conversion fails, for example on invalid credentials. | The error surfaces on the Instance as `ImageUnavailable`, and the adapter keeps retrying.                                               |

## Design Details

### Gating

When `enableImageConversion` is set, the provider creates Instance Pods with
the `compute.datumapis.com/image-conversion` scheduling gate. Images already in
the runtime's format, such as those built with the CLI, are detected and left
unchanged, so they need nothing extra. The provider also expands short image
names such as `nginx` to `docker.io/library/nginx`, so the runtime always
receives fully qualified references. If conversion fails, the adapter records
the error on the Pod and the provider reports it on the Instance.

### Conversion

An adapter claims a gated Pod and handles each of its images in turn. It first
resolves the image with the Pod's pull Secrets; if the image already includes a
variant for the runtime's platform, as CLI-built images do, it is left
untouched. Otherwise the adapter pulls and unpacks the image through the
node's containerd, builds an EROFS image from the unpacked files with
`mkfs.erofs`, and stores the result in containerd, packaged in the runtime's
format. When every image is ready, a single update points the Pod at the
converted images and removes the gate.

### Serving

The runtime fetches converted images over `http+oci` from the adapter on its
own node, at a path made of the source repository and the converted image's
digest, such as `/docker.io/library/nginx/@sha256:…`.

Only the node itself can reach its adapter: guests run in their projects' own
networks, and a network policy keeps the rest of the cluster out.
The runtime also checks access on every cold start, and the adapter grants it
only while a Pod uses the image and its credentials still work, so revoking
access upstream stops new starts.

Because images are referenced by digest, a Pod may land on a node that hasn't
converted its image yet. The adapter records each image's source on the Pod,
so the local adapter can rebuild the image from it while holding the request
open.
This relies on conversion being deterministic, producing the same bytes for the
same source image on every node. Fixed timestamps and IDs and a pinned
`mkfs.erofs` version ensure that. Adapters only rebuild images of their
own format, so changing the format means recreating the Pods that use older
images.

### Caching

The adapter caches only converted images. They live in a containerd namespace of
their own, apart from the kubelet's, and the adapter evicts the least recently
used ones once they exceed a configured size.

Source images are pulled into the kubelet's containerd namespace, so a
conversion reuses layers already on the node. The adapter holds pulled content
only for the duration of a conversion and creates no image records there,
leaving the kubelet's view of the node unchanged; containerd's garbage
collection removes the layers afterwards unless something else still uses them.
Rebuilding an evicted image therefore pulls it again, which keeps the node from
storing a second, compressed copy of every converted image.

## Enablement and Rollback

The feature is enabled by adding the `image-conversion` component to the runtime,
along with `image-conversion-cilium` for its network policy, and setting
`enableImageConversion: true` in the provider's configuration.

Rolling back happens in the reverse order. Unsetting the provider setting stops
new Pods from being gated, and the component can be removed once no running Pod
still uses a converted image.

## Drawbacks

- The first start of a new image on a node waits for its conversion, which is
  expected to take a few seconds for most images.
- The adapter is on the path of every cold start of a converted image, so its
  node can't cold-start those instances while it is down.

## Alternatives

| Alternative                                | Why not                                                                                                             |
| ------------------------------------------ | ------------------------------------------------------------------------------------------------------------------- |
| A central build service and registry       | Introduces a stateful service on every deployment's critical path.                                                  |
| Root filesystem over virtio-fs, as in Kata | The runtime's virtio-fs doesn't preserve file ownership.                                                            |
| Converting only in the CLI                 | Leaves every user with a build step.                                                                                |
| containerd's EROFS snapshotter             | Produces one EROFS image per layer, stacked with overlayfs, while the runtime boots a single root filesystem image. |
