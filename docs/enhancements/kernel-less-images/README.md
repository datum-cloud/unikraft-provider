---
status: provisional
stage: alpha
---

# Kernel-less Images

- [Summary](#summary)
- [Why this matters](#why-this-matters)
- [What changes](#what-changes)
- [Key decisions](#key-decisions)
- [What we still need from Unikraft](#what-we-still-need-from-unikraft)
- [Risks](#risks)
- [Status](#status)
- [Reference](#reference)

## Summary

Every application image we build today carries its own copy of the Unikraft
kernel. Unikraft can now supply the kernel from the node instead, so images can
carry just the application.

This describes how our runtime adopts that.

## Why this matters

**Kernel updates stop being a fleet-wide rebuild.** Because each image embeds a
kernel, updating the kernel means rebuilding and re-pushing every image we have.
When the node owns the kernel, a kernel update is a runtime update.

**Images get smaller and simpler.** An application image becomes the
application, not the application plus an operating system.

**Building an image may stop requiring vendor credentials.** This is the most
valuable possible outcome, and it is not yet confirmed. Today, a working image
has to be built against Unikraft's enterprise base, because only that base
reports back when a guest has finished booting — so anyone building an image
needs credentialed access to Unikraft's registry. If the node supplies that
kernel, the customer's build may no longer need those credentials at all. That
would remove a real obstacle from the `datumctl build` experience. We have asked
Unikraft to confirm it.

## What changes

The runtime image carries the platform kernel, and the platform daemon is told
where to find it. Images that arrive without a kernel get one; images that carry
their own are unaffected. Nothing changes for anyone until an image is
deliberately built without a kernel.

Two of the seven vendor packages move to Unikraft's staging channel, because
that is where the feature currently lives. The rest stay on the stable channel.

This is a capability, not a migration. Moving our own example images to
kernel-less is a follow-up, once the feature is proven on real hardware.

## Key decisions

**The kernel ships inside the runtime image, rather than being downloaded onto
each node.** Unikraft suggested downloading it on each node at startup. We chose
not to, for three reasons that all point the same way:

- It would put a vendor password on every one of our bare-metal machines. There
  is no such credential in our clusters today, and we would rather keep it that
  way. Building it into the image keeps the credential in CI, alongside the one
  we already use for vendor packages.
- The kernel and the vendor software that runs it stay locked together — one
  thing to update, one thing to roll back. Downloading separately lets the two
  drift apart silently.
- A download failure would take out nodes one at a time, at whatever hour the
  vendor's registry has a bad day. A build failure shows up in CI, in front of a
  person.

The cost is 37 MB of image size and the fact that a kernel update means
rebuilding the runtime image. Both are acceptable.

**The kernel is pinned to an exact version, not "latest".** Otherwise machines
built a week apart could quietly end up running different kernels — a class of
problem that has bitten us before.

**We fetch the kernel directly rather than installing Unikraft's build tool to
do it.** Their script installs a ~90 MB developer tool as a dependency. The
kernel turned out to be reachable with tools the image already has. This also
means we verify the kernel is exactly what we expect, which their script does
not do.

**Staging packages cannot leak in.** Version numbers on the staging channel sort
*above* stable, so simply enabling it would silently upgrade everything it
carries. The channel is configured so nothing is ever chosen from it
automatically — only the two packages we name explicitly. The weekly
version-update job was taught the same rule; without that, its next run would
have quietly undone this change.

**The feature is on by default in testing.** It would have been safer-looking to
enable it only in production clusters, but then our automated tests would never
exercise it — and the tests are exactly where we want problems to appear first.

## What we still need from Unikraft

The two that matter most:

1. **Does a kernel-less image still report when it has finished booting?** If
   not, instances will run and serve traffic but never appear healthy. This
   gates the whole feature.
2. **Does building without a kernel remove the need for vendor credentials at
   build time?** This is the prize described above.

Also outstanding: whether the kernel has to match the platform version exactly
and what happens if it does not; whether we may ship their kernel inside our own
private image; whether they will publish which kernel pairs with which platform
release; whether the kernel can be replaced while the runtime is running; and
when these packages reach the stable channel so we can stop tracking staging.

## Risks

| Risk | Mitigation |
| --- | --- |
| Staging packages become our default | Only two packages, named explicitly; reverting is a one-line change |
| The weekly update job undoes this | Taught to keep each package on its own channel |
| Kernel and platform drift apart | They ship as one artifact, updated and rolled back together |
| Different machines get different kernels | Pinned to an exact version |
| Instances never report healthy | Open question 1 above; gates the feature |
| Image grows 37 MB | Accepted — smaller than a component we already ship and never run |

## Status

Built and verified locally: the kernel is in the image, correct and intact, and
no credential leaks into it. Our configuration still builds cleanly.

Not yet verified, and expected to surface on the first CI run: whether the
staging packages install cleanly alongside the stable ones, and whether our
vendor credentials cover the staging channel. Beyond that, the feature has not
yet booted an instance on real hardware.

## Reference

The specifics behind the decisions above, for whoever picks this up next.

**Kernel source.** The kernel ukpd injects is the same artifact the Kraftfile
`runtime:` entry consumes — Unikraft's fetch script pulls `datum/base-compat`
and extracts `unikraft/bin/kernel` from it. `--images-kernel-path` does not
remove that dependency; it relocates it from every application image build to
one runtime image build.

**Kernel fetch.** `build/ukp-runtime/fetch-kernel.sh` resolves the image index,
picks the child manifest matching both `platform.architecture` and
`platform.os`, selects the layer annotated `org.unikraft.kernel.image` (not by
position — a debug layer sits alongside it in some images), verifies every
digest, and untars to `/usr/lib/ukp/kernel/{kernel,config.json}`. `config` and
`rootfs` are stripped from the config: a base image carries a `Cmd`, and leaving
it risks ukpd adopting it as the kernel command line.

`datum/base-compat:latest-amd64` is an index with exactly one child
(`kraftcloud/x86_64`, `sha256:1be27d37…`) holding one uncompressed 37,318,656-byte
layer whose sole entry is `unikraft/bin/kernel`. Its `os.features` carries
`KERNEL_VCS_COMMIT=f43cb10d70da80c7be46593632c8ed25b1396b8c` and
`CONFIG_UKP_FEATURES=11110`.

**Version ordering.** Confirmed with `dpkg --compare-versions`:

```
6:0.12.0-61+c30d6b1-2staging+deb13  >  6:0.12.0-5+e6abf75-9stable+deb13
6:0.2.5-71+d7a2b1e-2staging+deb13   >  6:0.2.4-52-ge6b2157-9stable+deb13
```

The `staging`/`stable` suffix sits in the Debian revision, which is only
compared if the upstream portions tie — and they do not. Hence
`/etc/apt/preferences.d/unikraft-staging.pref` pinning the staging component to
priority 100, plus the existing exact `pkg=version` pins.

**Feed keys.** Staging is signed by its own key,
`E157 4AA0 677C 98B7 ED46 0ED0 9E9F 3104 71E5 8DAB`
(`cloud-staging@https://pkg.unikraft.com`), distinct from stable's
`3D6C 59FF AB8E FA6E 1025 4EFF EBD0 B225 8841 F499`. Unikraft sent the stable
key alongside it, byte-identical to our committed copy.

**Platform support.** `strings` on the shipped `ukp-platform 0.10.0` binary
shows `os.features`, `KERNEL_VCS_COMMIT=`, `feature_mask`,
`img_config_decode_version_features`, `ukp_imgdb_get_kernel`, `img_set_kernel`
and `im-%s: Must set kernel for non-ROM image` — ukpd genuinely parses the
feature list into a bitmap. That same binary has `images-import-path` but no
`images-kernel-path`, which corroborates the staging requirement. No
version-compatibility error string appears anywhere in it, so whether
kernel/platform skew is rejected or silently degraded is unknown.

**Credentials.** CI holds `UNIKRAFT_REGISTRY_AUTH` — base64 of
`user:password`, the same form as a workstation's `UKC_TOKEN`. Note the name
collision: the `kraftlet-ukc-token` Secret used in production is *self-issued*
(an ESO `Password` generator, `config/overlays/ukp-runtime/auth-token.yaml:11-27`)
and authenticates kraftlet to ukpd over localhost. It has nothing to do with
Unikraft. No vendor credential exists in the production cluster.

**Configuration.** `ukp.conf` is ours, rendered into a ConfigMap and mounted at
`/etc/ukp.conf`; launchers source it under `set -a`, so container env vars reach
it — the idiom `NET_IFACE` already uses. `UKP_KERNEL_PATH` defaults to
`/usr/lib/ukp/kernel`; setting it empty disables the flag.

**If we ever have to fetch on the node instead** (only if question 3 comes back
badly), it cannot use the vendor script's default destination: `/usr/lib/ukp` is
image content and nothing mounts it, so a write there vanishes when the init
container exits. It would target `/var/lib/ukp/kernel` on the existing
`ukp-data` volume with `--images-kernel-path` pointed to match, plus an
already-present fast path, retry with backoff, and fail-open when a valid kernel
is on disk. `fetch-kernel.sh` takes its destination as a parameter, so it would
carry over nearly unchanged.
