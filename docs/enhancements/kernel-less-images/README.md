---
status: provisional
stage: alpha
---

# Kernel-less Images

- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Design Details](#design-details)
  - [Where the kernel comes from](#where-the-kernel-comes-from)
  - [Kernel delivery](#kernel-delivery)
  - [Staging apt channel](#staging-apt-channel)
  - [ukpd configuration](#ukpd-configuration)
  - [Credential contract](#credential-contract)
  - [Verified facts](#verified-facts)
- [Implementation Status](#implementation-status)
- [Risks and Mitigations](#risks-and-mitigations)
- [Open Questions for Unikraft](#open-questions-for-unikraft)
- [Alternatives](#alternatives)

## Summary

Every unikernel application image we build embeds a full copy of the Unikraft
kernel, pulled in by the `runtime:` entry in its Kraftfile. That ties each image
to a kernel version: a kernel update means rebuilding and re-pushing every image
we have.

Unikraft's staging platform can supply the kernel itself, at instance creation,
via a new ukpd flag `--images-kernel-path`. This describes how our runtime
adopts that, so application images can carry only the application.

## Motivation

Beyond removing the rebuild-everything coupling, the interesting consequence is
for image *builds*. Only the enterprise base carries the guest boot-complete
signal, so building a working image today requires credentialed access to the
vendor registry. If the node supplies the kernel, that requirement may move off
the customer's build entirely — see question 6.

### Goals

- Install the platform kernel on every node and point ukpd at it.
- Consume the vendor's `cloud-staging` apt channel deterministically, without
  losing the reproducibility the current exact-version pins give us.

### Non-Goals

- Migrating existing application images to kernel-less. This lands the
  capability; the migration is a follow-up.
- Building our own kernel. We consume the vendor's.
- Removing the `runtime:` entry from our own examples. That is the follow-up
  that demonstrates the feature, once it is validated on real hardware.

## Design Details

### Where the kernel comes from

Worth stating plainly, because it is easy to assume otherwise: the kernel ukpd
injects is the *same artifact* the Kraftfile `runtime:` entry consumes.
Unikraft's fetch script defaults to `REPO=datum/base-compat` and extracts
`unikraft/bin/kernel` from it. `--images-kernel-path` does not replace the
base-compat image — it relocates it from "once per application image" to "once
per host".

So this change does not remove a vendor dependency. It moves where that
dependency is resolved, from many image builds to one runtime image build.

### Kernel delivery

**The kernel is baked into the runtime image at build time.** A
`--mount=type=secret` step in `build/ukp-runtime/Dockerfile` fetches the kernel
layer **by pinned digest** with curl/jq/tar into
`/usr/lib/ukp/kernel/{kernel,config.json}`, pinned by
`ARG UKP_KERNEL_MANIFEST_DIGEST` beside the seven existing `UKP_*_VERSION` ARGs.

Unikraft suggested an init container instead. The reasoning against, by weight:

1. **The kernel is a platform component, not node state.** Every other vendor
   binary — ukpd, agent, CoreDNS, Firecracker — arrives via the image,
   version-pinned, from an authenticated vendor feed, through a BuildKit secret.
   Same vendor, same credential class, same kind of thing. An init container
   invents a second, weaker delivery channel for one more vendor file.
2. **It makes the ABI question moot.** "Same artifact, bumped together, rolled
   back together" is correct under every possible answer to the kernel/platform
   coupling question below. A runtime fetch lets you run the wrong kernel under
   the right ukpd and never notice.
3. **It keeps the vendor credential off the node entirely.** There is no
   `index.unikraft.io` credential in the cluster today, so the init-container
   path means minting a reusable vendor `user:password`, putting it on every
   bare-metal host, and writing a new infra contract for it — all for a file
   that never changes between image builds.
4. **Air-gap and reboot survival come free**, and the kind e2e behaves
   identically to production, since it builds this image from source.
5. **Failure lands in CI in front of a human**, not as node-by-node CrashLoop at
   03:00 when Harbor is slow, on nodes with running guests.

Cost is image size: the `datum/base-compat` kernel layer is **37 MB**
(37,318,656 bytes) — much larger than the 1.7 MB kernel in the public
`unikraft.org/base`, presumably the compat layer. Acceptable for an image that
already carries `ukp-openresty` we never run, but it is 37 MB and not the ~2 MB
the public image would suggest.

**No KraftKit.** The vendor script shells out to `kraft pkg pull` for the
kernel, but the kernel is a single uncompressed tar layer tagged by an
`org.unikraft.kernel.image` annotation, so `curl` + `jq` + `tar` reach it
directly — the same registry calls the script already makes for the config blob.
Only `jq` needed adding to the image. This keeps a ~90 MB Go binary and a new
install channel out of an image whose whole point is deterministic pinned vendor
debs, avoids downloading the `.dbg` layer, and **digest-verifies the kernel**,
which the vendor script does not.

Selection recipe: resolve the index, pick the child matching both
`platform.architecture` **and** `platform.os`, pick the layer whose annotation
key is `org.unikraft.kernel.image` (do not hardcode index 0), verify the digest,
`tar xO` to the destination.

### Staging apt channel

`ukp-agent` and `ukp-platform` come from the vendor's `cloud-staging` feed —
that is where the feature lives, and the stable platform has no
`--images-kernel-path` flag at all. The other five packages stay on stable.

The staging feed is a separate `.sources` file with its own keyring rather than
an edit to the existing one: the two feeds have separate keys and separate trust
stories, and a second file keeps `git log` on the stable source legible.

**Version ordering is the trap here.** Verified with `dpkg --compare-versions`
in `debian:trixie-slim`:

```
6:0.12.0-61+c30d6b1-2staging+deb13  >  6:0.12.0-5+e6abf75-9stable+deb13
6:0.2.5-71+d7a2b1e-2staging+deb13   >  6:0.2.4-52-ge6b2157-9stable+deb13
```

Staging sorts **above** stable. The `2staging` / `9stable` suffix sits in the
Debian revision, which is only reached if the upstream portions tie — and they
do not (`0.12.0-61` > `0.12.0-5` numerically). So merely enabling the staging
feed would make it the candidate for every package it carries, including the
five we intend to hold at stable.

Two mitigations, both applied:

- Exact `pkg=version` pins on the install line. These force the named versions
  regardless of candidate selection, so the seven ukp packages stay
  deterministic.
- `/etc/apt/preferences.d/unikraft-staging.pref` giving the staging component
  priority `100`. Explicit `pkg=version` still installs from it, but nothing is
  ever *auto*-selected from staging. This makes the intent enforced rather than
  incidental, and protects the next person who adds a package to the install
  line without thinking about feeds.

`ukp-platform`'s `Depends` on `ukp-openresty` and `ukp-networking` are both
explicitly pinned, so they cannot drift. If staging `ukp-platform 0.12.0-61`
requires a newer `ukp-networking` than our stable pin, apt fails to resolve — a
loud build failure, which is the correct outcome.

**Weekly bump automation.** `.github/workflows/update-ukp-versions.yaml` scans
with `apt-cache madison`, which reports across all enabled feeds. It now derives
each package's channel from its current pin's `-Nstaging`/`-Nstable` suffix and
filters madison to match. Without that, its next run would have downgraded agent
and platform back to stable.

### ukpd configuration

`ukp.conf` is ours, not a vendor file, and is rendered into a ConfigMap
(`config/dependencies/ukp-runtime/kustomization.yaml:13-16`) and mounted
read-only at `/etc/ukp.conf`. Launchers source it under `set -a`, so container
env vars are visible to it — the idiom `NET_IFACE` already uses
(`ukp.conf:29-36`).

```bash
## Platform daemon
UKPD_EXTRA_ARGS=()
UKPD_EXTRA_ARGS+=("--vmm-initrd-map-shared")
# Kernel-less images: ukpd supplies this kernel for images packaged without
# one. The runtime image always ships it at this path (build/ukp-runtime/
# fetch-kernel.sh), so the default is on. Set UKP_KERNEL_PATH="" on the
# container to disable.
UKP_KERNEL_PATH="${UKP_KERNEL_PATH:-/usr/lib/ukp/kernel}"
if [ -n "$UKP_KERNEL_PATH" ]; then
	UKPD_EXTRA_ARGS+=("--images-kernel-path=${UKP_KERNEL_PATH}")
fi
```

The flag defaults **on**. The kernel is always in the image and the staging
platform understands the flag, so gating it behind a real-cluster overlay would
have meant the kind e2e never exercised it — the one place a bad interaction
should surface. The env var remains the disable switch.

### Credential contract

Baking makes this a **build-time** concern only, which is the main practical
argument for it: no node-side secret, no new infra contract, nothing on the
bare-metal hosts.

CI needs an `index.unikraft.io` registry credential, mounted as a second
BuildKit secret alongside the existing `unikraft-apt-auth`. It is stored as
`UNIKRAFT_REGISTRY_AUTH` — base64 of `user:password`, the same form as a
workstation's `UKC_TOKEN`, so one value works in both places.

Naming trap worth recording, since two unrelated credentials share the name:

- **`KRAFTLET_UKC_TOKEN` / the `kraftlet-ukc-token` Secret** — used in
  production, but **self-issued**: an ESO `Password` generator mints a random
  password in-cluster (`config/overlays/ukp-runtime/auth-token.yaml:11-27`),
  rendered as `base64("kraftlet:<pw>")` and seeded into ukpd's own `users.json`.
  It authenticates kraftlet → ukpd on localhost. No vendor involvement.
- **The vendor `UKC_TOKEN`** — a Unikraft Cloud platform token that also
  authenticates against the Harbor registry. It appears only in developer paths:
  `.env`, `README.md:23`'s superseded Helm flow, and the provisioning
  walkthrough.

So **no vendor credential exists in the production cluster today**, and this
approach keeps it that way.

### Verified facts

Everything below was confirmed directly, not inferred:

- `index.unikraft.io` is Harbor; standard Bearer token flow
  (`www-authenticate: Bearer realm=…/service/token,service="harbor-registry"`).
- `datum/base-compat:latest-amd64` is an OCI index with **exactly one** child
  manifest, `kraftcloud/x86_64`, digest `sha256:1be27d37…`. Only one child means
  the vendor script's arch-only child selection cannot mis-pick for *this*
  image — but the bug is real and is not carried into our version.
- That manifest has one layer, `application/vnd.oci.image.layer.v1.tar`,
  37,318,656 bytes, annotated `org.unikraft.kernel.image: /unikraft/bin/kernel`.
  Uncompressed tar, so digest == diff_id.
- Pulled with plain `curl`; digest matches; tar contains exactly
  `unikraft/bin/kernel`. **No `kraft` required.**
- It is genuinely the new staging build: annotated
  `index.unikraft.io/official-staging/base-compat`, created
  `2026-08-12T14:10:09Z`.
- Config carries `Cmd: ["/bin/busybox echo Hello world"]`, a `rootfs.diff_ids`,
  and `created: null` — the vendor script's stripping and its
  `// "0001-01-01T00:00:00Z"` fallback are both load-bearing, not defensive.
- `os.features` carries `KERNEL_VCS_COMMIT=f43cb10d70da80c7be46593632c8ed25b1396b8c`
  and `CONFIG_UKP_FEATURES=11110`.
- Staging apt versions sort above stable (`dpkg --compare-versions` in
  `debian:trixie-slim`).
- ukpd really does consume the feature list: `strings` on the shipped
  `ukp-platform 0.10.0` binary shows `os.features`, `num_os_features`,
  `KERNEL_VCS_COMMIT=`, `feature_mask`, `img_config_decode_version_features`,
  `ukp_imgdb_get_kernel`, `img_set_kernel`, and
  `im-%s: Must set kernel for non-ROM image`.
- That same 0.10.0 binary has `images-import-path` and
  `images-never-require-present` but **no** `images-kernel-path` — consistent
  with the vendor requiring the staging platform for this feature.
- The staging feed's key is distinct from stable's:
  `E157 4AA0 677C 98B7 ED46 0ED0 9E9F 3104 71E5 8DAB`, uid
  `cloud-staging@https://pkg.unikraft.com (ProGet)`. The vendor sent the stable
  key alongside it and it is byte-identical to our committed copy, which
  corroborates the channel.

## Implementation Status

Landed:

- `build/ukp-runtime/fetch-kernel.sh` — curl/jq/tar fetch, no `kraft`. Addresses
  the manifest by digest, verifies manifest, config and kernel-layer digests,
  selects the kernel layer by its `org.unikraft.kernel.image` annotation rather
  than by position, and strips `config`/`rootfs` from the config.
- `build/ukp-runtime/Dockerfile` — `jq` added; kernel fetched into
  `/usr/lib/ukp/kernel` under a `unikraft-registry-auth` BuildKit secret;
  staging feed, keyring and preferences registered; `ukp-agent` →
  `6:0.2.5-71+d7a2b1e-2staging+deb13` and `ukp-platform` →
  `6:0.12.0-61+c30d6b1-2staging+deb13`.
- `build/ukp-runtime/unikraft-cloud-staging.{gpg,sources}`,
  `build/ukp-runtime/unikraft-staging.pref`.
- `.github/workflows/build.yaml`, `.github/workflows/e2e.yaml` — the
  `UNIKRAFT_REGISTRY_AUTH` secret plumbed into both image builds.
- `.github/workflows/update-ukp-versions.yaml` — per-package channel awareness.
- `config/dependencies/ukp-runtime/ukp.conf` — `--images-kernel-path`, on by
  default.

Verified by building the kernel stage with a real BuildKit secret: the kernel
lands in the image (37,317,056 bytes, ELF), `config.json` renders with
`KERNEL_VCS_COMMIT=f43cb10d…`, and neither the token nor its decoded form
appears anywhere in `docker save` output. All three kustomize overlays build.

Not yet verified:

- The full image build — whether staging `ukp-platform` resolves against our
  stable-pinned `ukp-networking`/`ukp-openresty`, and whether the apt credential
  is scoped to cover `/debian/cloud-staging/`. Both surface on the first CI run.
- ukpd actually booting a kernel-less instance on real hardware.

## Risks and Mitigations

| Risk | Mitigation |
| --- | --- |
| Staging pins ship to production as the default | Exact pins + apt priority 100; rollback is a one-line revert |
| Weekly bot migrates everything to staging | Per-package channel awareness, landed alongside the feed |
| Kernel drifts from `ukp-platform` deb | Baking makes them one immutable artifact, bumped and rolled back together |
| Mutable `:latest-amd64` tag gives different nodes different kernels | Pin `UKP_KERNEL_MANIFEST_DIGEST`; same drift class as the ukpd tag→digest cache staleness that has burned us before |
| Instances never reach Ready | Only the enterprise base carries the vsock/BootTimer boot-complete signal; confirm `base-compat` does |
| Image grows 37 MB | Accepted; smaller than the unused `ukp-openresty` already shipped |

## Open Questions for Unikraft

1. Is the host kernel required to version-match `ukp-platform`? Is skew detected
   and rejected, or does it fail later at guest boot? Is the contract
   `KERNEL_VCS_COMMIT`, `os.version`, or the `os.features` bitmap? ukpd clearly
   builds a feature bitmap, but carries no version-compatibility error string we
   could find — so we cannot tell whether skew is rejected or silently degraded.
2. **Can we pin by digest?** We want `datum/base-compat@sha256:…`, not
   `:latest-amd64`. Which digest pairs with `ukp-platform=6:0.12.0-61+c30d6b1`,
   and will you publish that mapping per release?
3. Is baking the kernel into our own private runtime image supported and
   licensed? `datum/base-compat` is a namespace you pushed for us, so we want
   one line in writing.
4. Is the raw-OCI fetch fully equivalent to `kraft pkg pull` — i.e. is anything
   else in the package consumed besides the layer annotated
   `org.unikraft.kernel.image`?
5. Which `config.json` keys does ukpd actually require? Is stripping
   `config`/`rootfs` correct, and is `created` load-bearing (the script
   fabricates `0001-01-01T00:00:00Z`)?
6. **Does dropping `runtime:` also close the boot-complete gap?** If the node
   supplies the enterprise kernel, does a kernel-less customer image report
   boot-complete with *no* vendor credentials at build time? This is the main
   prize — it would move the credential requirement off every customer image
   build, which is exactly what the `datumctl build` story (compute#174) needs.
7. Does `datum/base-compat` itself carry the guest boot-complete signal (vsock /
   BootTimer)? Without it, instances boot and serve traffic but never report
   Ready.
8. Is it safe to replace `<dest>/kernel` under a running ukpd? Does ukpd read it
   per instance creation or cache at start, and what happens to parked/standby
   snapshots taken against the previous kernel?
9. How does ukpd behave when `--images-kernel-path` points at a missing or empty
   directory — hard failure, or fall back to the image's own kernel?
10. With `runtime:` dropped, what does `kraft pkg` emit for `platform.os` and
    `os.features`, and does ukpd still require a `kraftcloud` child?
11. When do `ukp-agent 0.2.5-71` and `ukp-platform 0.12.0-61` reach
    `cloud/stable`? We would rather not carry a staging feed in the production
    image build any longer than necessary.
12. Is the kernel going to ship as a deb? That would replace the whole fetch
    step with one more pinned `UKP_*_VERSION` ARG.

## Alternatives

**An init container fetching at pod start.** Unikraft's own suggestion, and
self-healing on upgrade. Rejected because it makes every pod start depend on
registry reachability, requires a reusable vendor credential on every node, and
decouples the kernel from the ukp-platform version it runs under. Retained as
the fallback if baking turns out to be unlicensed (question 3).

Note it cannot use the vendor script's default `--dest=/usr/lib/ukp/kernel`:
that path is image content from the vendor debs and nothing mounts it
(`daemonset.yaml:165-199`), so a write there lands in a container-local layer
that vanishes on exit and ukpd never sees it. The fallback would target
`/var/lib/ukp/kernel` on the existing `ukp-data` volume, with
`--images-kernel-path` pointed to match, an "already present and matching →
exit 0" fast path modelled on `activate-node.sh:19-26`, retry with backoff, and
fail-open when a valid kernel is already on disk. `fetch-kernel.sh` takes its
destination as a parameter, so it would carry over nearly unchanged.

**Node provisioning (systemd unit, Talos extension, one-shot Job).** Rejected
outright: it reintroduces the per-node imperative provisioning the
containerization enhancement set out to eliminate, and a Job misses repaved and
newly-added nodes.

**A kustomize Component.** Components here are independent workloads with their
own lifecycle (`ukp-remote-cni`). A fetch step must complete before ukpd starts,
inside the same pod, so it would be a patch on the ukp-runtime overlay — not a
Component.

**A separate init-container image with kraft+jq.** Cleanest isolation, but a
second image to build, publish and pin — more machinery than this warrants once
`kraft` is off the dependency list.
