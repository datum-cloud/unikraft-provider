---
status: provisional
stage: alpha
---

# Kernel-less Images and the Staging Package Channel

- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [Two independent tracks](#two-independent-tracks)
  - [Track A: IPv6 via the staging base-compat runtime](#track-a-ipv6-via-the-staging-base-compat-runtime)
  - [Track B: kernel-less images](#track-b-kernel-less-images)
- [Design Details](#design-details)
  - [Staging apt channel](#staging-apt-channel)
  - [Kernel delivery](#kernel-delivery)
  - [ukpd configuration](#ukpd-configuration)
  - [Credential contract](#credential-contract)
  - [Verified facts](#verified-facts)
- [Risks and Mitigations](#risks-and-mitigations)
- [Open Questions for Unikraft](#open-questions-for-unikraft)
- [Alternatives](#alternatives)

## Summary

Unikraft has shipped two features behind a new `cloud-staging` apt channel: IPv6
support (kraftlet `0.6.0-staging.28`) and kernel-less image packaging (ukpd
`--images-kernel-path`). This proposes how our build and deployment pipeline
adopts them.

The central finding is that **these are two independent tracks, not one**. IPv6
needs no build-pipeline change at all and can ship immediately. Kernel-less
packaging is the one that pulls in the staging apt channel, a new node-side
kernel artifact, and a new registry credential.

## Motivation

Today every unikernel app image we build embeds a full copy of the Unikraft
kernel, pulled in via the `runtime:` entry in its Kraftfile. That couples every
application image to a kernel version: a kernel update means rebuilding and
re-pushing every image. Kernel-less packaging moves the kernel to the host,
where ukpd injects it at instance creation, so app images carry only the
application.

### Goals

- Consume the vendor's `cloud-staging` apt channel deterministically, without
  losing the reproducibility the current exact-version pins give us.
- Install the platform kernel on every node and point ukpd at it.
- Unblock IPv6 validation.

### Non-Goals

- Dual-stack address *reporting*. Our controller surfaces exactly one address
  (`instance_controller.go:698`) and the upstream compute API has only
  `NetworkIP`/`ExternalIP` (`go.datum.net/compute@v0.6.0` `instance_types.go:241-248`).
  Dual-stack requires an upstream API change and is out of scope here.
- Migrating existing app images to kernel-less. This lands the capability; the
  migration is a follow-up.
- Building our own kernel. We consume the vendor's.

## Proposal

### Two independent tracks

The vendor's message reads as one workstream, but the dependency graph splits
cleanly. The vendor's own fetch script defaults to `REPO=datum/base-compat` and
extracts `unikraft/bin/kernel` from it — the *same artifact* the Kraftfile
`runtime:` entry consumes. `--images-kernel-path` does not replace the
base-compat image; it relocates it from "once per app image" to "once per host".

|                              | Track A — `runtime:` in Kraftfile | Track B — `--images-kernel-path` |
| ---------------------------- | --------------------------------- | -------------------------------- |
| Kernel source                | staging `datum/base-compat`, per app image | staging `datum/base-compat`, once per host |
| Needs staging ukp packages   | No                                | **Yes**                          |
| Vendor credential needed at  | app image build                   | runtime image build (CI)         |
| Repo blast radius            | Kraftfile + kraftlet pin          | Dockerfile, ukp.conf, e2e        |
| Kernel update = image rebuild| Yes                               | No                               |

Track A is a two-line change that unblocks IPv6 today. Track B is the better end
state and should follow, not block it.

### Track A: IPv6 via the staging base-compat runtime

1. Bump `config/dependencies/kraftlet/daemonset.yaml:49` from
   `0.6.0-staging.26` to `0.6.0-staging.28`.
2. Point the `runtime:` entry in `examples/go-hello/Kraftfile` at the staging
   `index.unikraft.io/datum/base-compat:latest-amd64` and rebuild.
3. Remove or make conditional the `rewrite stop type AAAA A` line that
   `build/ukp-runtime/coredns-redirect.sh:74` writes into every Corefile. As
   written, guests receive A records in response to AAAA queries — IPv6 name
   resolution is off by construction and no amount of kraftlet work will fix it.
4. Widen the iDNS whitelist. `coredns-redirect.sh:72` emits
   `whitelist ${NET_SEGMENT}` from `ukp.conf:38` (`172.16.0.0/12`); a v6-sourced
   query falls outside it.

Note that IPv6 *addressing* is decided by the NAD's IPAM, which galactic owns,
not this repo. Track A makes the runtime capable; it does not by itself hand
instances a v6 address.

### Track B: kernel-less images

1. Add the `cloud-staging` deb822 source and its keyring to the runtime image.
2. Move `ukp-agent` and `ukp-platform` to the staging pins the vendor named.
3. Fetch the kernel **at image build time**, by pinned digest, into
   `/usr/lib/ukp/kernel` — alongside every other vendor binary.
4. Gate `--images-kernel-path` behind an env var so the kind e2e is unaffected.

## Design Details

### Staging apt channel

Add a second file, `build/ukp-runtime/unikraft-cloud-staging.sources`, rather
than editing the existing one — the two suites have separate keyrings and
separate trust stories, and a second file keeps `git log` on the stable source
legible.

**Version ordering is the trap here.** Verified with `dpkg --compare-versions`
in `debian:trixie-slim`:

```
6:0.12.0-61+c30d6b1-2staging+deb13  >  6:0.12.0-5+e6abf75-9stable+deb13
6:0.2.5-71+d7a2b1e-2staging+deb13   >  6:0.2.4-52-ge6b2157-9stable+deb13
```

Staging sorts **above** stable. The `2staging` / `9stable` suffix sits in the
Debian revision, which is only reached if the upstream portions tie — and they
do not (`0.12.0-61` > `0.12.0-5` numerically). So merely enabling the staging
suite makes staging the candidate for every package it carries, including the
five we intend to hold at stable.

Two mitigations, both wanted:

- Keep the exact `pkg=version` pins on the install line. These already force the
  named versions regardless of candidate selection, so the seven ukp packages
  stay deterministic today.
- Add `/etc/apt/preferences.d/unikraft-staging` giving the staging suite
  priority `100`. Explicit `pkg=version` still installs from it, but nothing is
  ever *auto*-selected from staging. This makes the intent enforced rather than
  incidental, and protects the next person who adds a package to the install
  line without thinking about suites.

`ukp-platform`'s `Depends` on `ukp-openresty` and `ukp-networking` are both
explicitly pinned, so they cannot drift. If staging `ukp-platform 0.12.0-61`
requires a newer `ukp-networking` than our stable pin, apt fails to resolve —
a loud build failure, which is the correct outcome.

**Blocker: we do not have `unikraft-cloud-staging.gpg`.** The whole repo is
behind HTTP basic auth — every probe returns 401, including the key endpoints:

```
https://pkg.unikraft.com/debian/cloud-staging/                     -> 401
https://pkg.unikraft.com/debian/cloud-staging/dists/trixie/Release -> 401
https://pkg.unikraft.com/debian/cloud-staging/key.gpg              -> 401
```

The existing stable key is `3D6C 59FF AB8E FA6E 1025 4EFF EBD0 B225 8841 F499`,
uid `cloud@https://pkg.unikraft.com (ProGet)` — a feed-scoped ProGet key, so it
most likely does **not** sign the staging feed. The vendor citing a distinct
`Signed-By` path implies a distinct key. We need them to send it, or confirm the
existing key covers both.

**Also unconfirmed: credential scope.** apt's `auth.conf` `machine` line may be
host-scoped (`machine pkg.unikraft.com`) or path-scoped
(`machine pkg.unikraft.com/debian/cloud/`). If our CI secret is path-scoped it
will not authenticate against `/debian/cloud-staging/` and the build fails at
`apt-get update`. This is a one-line check against the CI secret.

**Weekly bump automation.** `.github/workflows/update-ukp-versions.yaml` scans
via `apt-cache madison`, which reports across all enabled suites. Once staging
is present it will start proposing staging versions for all seven packages. It
must be taught which packages track which channel — otherwise the first bot PR
silently migrates the whole runtime to staging.

### Kernel delivery

**Bake the kernel into the runtime image at build time.** Add a
`--mount=type=secret` step to `build/ukp-runtime/Dockerfile` that fetches the
kernel layer **by pinned digest** with curl/jq/tar into
`/usr/lib/ukp/kernel/{kernel,config.json}`, and pin
`ARG UKP_KERNEL_DIGEST=sha256:…` beside the seven existing `UKP_*_VERSION` ARGs.

The reasoning, by weight:

1. **The kernel is a platform component, not node state.** Every other vendor
   binary — ukpd, agent, CoreDNS, Firecracker — arrives via the image,
   version-pinned, from an authenticated vendor feed, through a BuildKit secret.
   Same vendor, same credential class, same kind of thing. An init container
   invents a second, weaker delivery channel for one more vendor file.
2. **It makes the ABI question moot.** "Same artifact, bumped together, rolled
   back together" is correct under every possible answer to the kernel/platform
   coupling question below. A runtime fetch lets you run the wrong kernel under
   the right ukpd and never notice.
3. **It keeps the vendor credential off the node entirely.** This is the big
   one: there is no `index.unikraft.io` credential in the cluster today, so the
   init-container path means minting a reusable vendor `user:password`, putting
   it on every bare-metal host, and writing a new infra contract for it — all
   for a file that never changes between image builds. Baking deletes that
   entire workstream.
4. **Air-gap and reboot survival come free**, and the kind e2e behaves
   identically to production since it builds this image from source
   (`e2e.yaml:45-53`).
5. **Failure lands in CI in front of a human**, not as node-by-node CrashLoop at
   03:00 when Harbor is slow, on nodes with running guests.

Cost is image size: the `datum/base-compat` kernel layer is **37 MB**
(37,318,656 bytes) — notably larger than the 1.7 MB kernel in the public
`unikraft.org/base`, presumably the compat layer. Acceptable for an image that
already carries `ukp-openresty` we never run, but it is 37 MB and not the ~2 MB
a quick look at the public image would suggest.

**Drop the `kraft` dependency.** Verified against the live private image with
credentials: `datum/base-compat:latest-amd64` is an index with **exactly one**
child manifest (`kraftcloud/x86_64`), which has **exactly one** uncompressed
layer annotated `org.unikraft.kernel.image: /unikraft/bin/kernel`, containing
exactly one file. Pulled with plain `curl`, digest matched byte-for-byte,
extracted with `tar`. So the fetch is the same three registry calls the vendor
script already makes for the config blob — only `jq` needs adding to
`build/ukp-runtime/Dockerfile:29` (`tar`, `curl`, `sha256sum`, `base64` are
already present).

Dropping `kraft` also removes a ~90 MB Go binary and a new install channel from
an image whose whole point is deterministic pinned vendor debs, avoids
downloading the `.dbg` layer, and **digest-verifies the kernel for the first
time** — today it is the one artifact the vendor script does *not* check.

Selection recipe: resolve the index, pick the child matching both
`platform.architecture` **and** `platform.os`, pick the layer whose annotation
key is `org.unikraft.kernel.image` (do not hardcode index 0), verify the digest,
`tar xO` to the destination.

**Fallback if the vendor won't license baking:** an init container following the
`activate-node` precedent (privileged, ukp-runtime image, script as a
hash-suffixed ConfigMap). It cannot use the vendor's default `--dest`, because
`/usr/lib/ukp` is image content and nothing mounts it
(`daemonset.yaml:165-199`) — a write there lands in a container-local layer that
vanishes on exit. It would instead target `/var/lib/ukp/kernel` on the existing
`ukp-data` volume, with `--images-kernel-path` pointed to match, an
"already present and matching → exit 0" fast path modelled on
`activate-node.sh:19-26`, retry with backoff, and fail-open when a valid kernel
is already on disk.

A separate node-provisioning step (systemd unit, Talos extension, one-shot Job)
should be rejected outright — it reintroduces exactly the per-node imperative
provisioning the containerization enhancement set out to eliminate.

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
# Kernel-less images: ukpd supplies this kernel when an image carries none.
# Baked into the runtime image at /usr/lib/ukp/kernel; gated while the flag is
# new, since ukpd's behaviour on a missing path is unverified.
if [ -n "${UKP_KERNEL_PATH:-}" ]; then
	UKPD_EXTRA_ARGS+=("--images-kernel-path=${UKP_KERNEL_PATH}")
fi
```

With the kernel baked in, `UKP_KERNEL_PATH` is `/usr/lib/ukp/kernel` and the
value is the same everywhere. The gate is still worth keeping while the flag is
new: we have not verified how ukpd behaves when `--images-kernel-path` points at
a missing or empty directory, and `ukp.conf` is sourced under `bash -e`, so an
unguarded failure is fatal. Once the flag is proven on the lab box the
conditional can collapse to an unconditional append beside
`--vmm-initrd-map-shared`.

### Credential contract

Baking makes this a **build-time** concern only, which is the main practical
argument for it. No new node-side secret, no new infra contract, nothing on the
bare-metal hosts.

What we do need is an `index.unikraft.io` registry credential in CI, mounted as
a second BuildKit secret alongside the existing `unikraft-apt-auth`. CI today
holds only two vendor secrets — `UNIKRAFT_APT_AUTH` (`build.yaml:46`) and
`UKP_AGENT_CREDENTIALS` (`e2e.yaml:63`) — so this is a new one.

Possible shortcut: `UKP_AGENT_CREDENTIALS` carries `AGENT_PULL_HARBOR_TOKEN`,
and `index.unikraft.io` *is* Harbor. If that token grants pull on
`datum/base-compat`, no new secret is needed. **Check this before adding one.**

Naming trap worth recording, since two unrelated credentials share the name:

- **`KRAFTLET_UKC_TOKEN` / the `kraftlet-ukc-token` Secret** — used in
  production, but **self-issued**: an ESO `Password` generator mints a random
  password in-cluster (`config/overlays/ukp-runtime/auth-token.yaml:11-27`),
  rendered as `base64("kraftlet:<pw>")` and seeded into ukpd's own `users.json`.
  It authenticates kraftlet → ukpd on localhost. No vendor involvement.
- **The vendor `UKC_TOKEN`** — a Unikraft Cloud platform token that also
  authenticates against the Harbor registry (verified: it pulled the kernel blob
  during this investigation). It appears only in developer paths: `.env`,
  `README.md:23`'s superseded Helm flow, and the provisioning walkthrough.

So **no vendor credential exists in the production cluster today**, and the
baking approach keeps it that way.

Had we gone the init-container route, this would instead have been a new
deployment-time contract for infra — a reusable vendor `user:password` on every
node, delivered as a Secret mounted only into that init container and read from
file, documented in `docs/architecture/` beside `NODE_ACTIVATION_TOKEN`.

### Verified facts

Everything below was confirmed directly, not inferred:

- `index.unikraft.io` is Harbor; standard Bearer token flow
  (`www-authenticate: Bearer realm=…/service/token,service="harbor-registry"`).
- `datum/base-compat:latest-amd64` is an OCI index with **exactly one** child
  manifest, `kraftcloud/x86_64`, digest `sha256:1be27d37…`. Only one child means
  the vendor script's arch-only child selection cannot mis-pick for *this*
  image — but the bug is real and should not be carried into our version.
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
- `UKC_TOKEN` from `.env` authenticates to Harbor as `user:password` and grants
  pull on `datum/base-compat`.

## Implementation Status

Landed (kernel delivery, verified end-to-end):

- `build/ukp-runtime/fetch-kernel.sh` — curl/jq/tar fetch, no `kraft`. Addresses
  the manifest by digest, verifies manifest, config and kernel-layer digests,
  selects the kernel layer by its `org.unikraft.kernel.image` annotation rather
  than by position, and strips `config`/`rootfs` from the config.
- `build/ukp-runtime/Dockerfile` — `jq` added; kernel fetched into
  `/usr/lib/ukp/kernel` under a `unikraft-registry-auth` BuildKit secret, pinned
  via `ARG UKP_KERNEL_MANIFEST_DIGEST`.
- `.github/workflows/build.yaml`, `.github/workflows/e2e.yaml` — the new
  `UNIKRAFT_REGISTRY_AUTH` secret plumbed into both image builds.
- `config/dependencies/ukp-runtime/ukp.conf` — `--images-kernel-path` appended
  to `UKPD_EXTRA_ARGS`, gated on `UKP_KERNEL_PATH`.

Verified by building the kernel stage with a real BuildKit secret: the kernel
lands in the image (37,317,056 bytes, ELF), `config.json` renders with
`KERNEL_VCS_COMMIT=f43cb10d…`, and neither the token nor its decoded form
appears anywhere in `docker save` output.

Landed (staging channel), once the vendor sent the keyring:

- `build/ukp-runtime/unikraft-cloud-staging.gpg` — fingerprint
  `E157 4AA0 677C 98B7 ED46 0ED0 9E9F 3104 71E5 8DAB`, uid
  `cloud-staging@https://pkg.unikraft.com (ProGet)`. A **distinct** key from the
  stable feed's, as expected. The vendor sent the stable key alongside it and it
  is byte-identical to our committed copy, which corroborates the channel.
- `build/ukp-runtime/unikraft-cloud-staging.sources` — the deb822 source.
- `build/ukp-runtime/unikraft-staging.pref` — `Pin: release c=staging`,
  priority 100.
- `ukp-agent` → `6:0.2.5-71+d7a2b1e-2staging+deb13` and `ukp-platform` →
  `6:0.12.0-61+c30d6b1-2staging+deb13`; the other five stay on stable.
- `.github/workflows/update-ukp-versions.yaml` — registers both feeds and keeps
  each package on the feed its current pin came from, derived from the
  `-Nstaging`/`-Nstable` suffix. Without this the first bot PR would have
  migrated all seven packages to staging (or, once the staging pins landed,
  silently *downgraded* agent and platform back to stable).
- `--images-kernel-path` now defaults **on**, at `/usr/lib/ukp/kernel`. The
  kernel is always in the image and the staging platform understands the flag,
  so gating it behind a real-cluster overlay would have meant the kind e2e never
  exercised it. `UKP_KERNEL_PATH=""` disables it.

Blocked on the vendor:

- Track A (IPv6) — kraftlet bump, Kraftfile `runtime:`, and the CoreDNS
  `rewrite … AAAA A` removal are all untouched so far. Not vendor-blocked;
  simply not started.

## Risks and Mitigations

| Risk | Mitigation |
| --- | --- |
| Staging pins ship to production as the default | Exact pins + apt priority 100; rollback is a one-line revert |
| Weekly bot migrates everything to staging | Teach it per-package channels before enabling the suite |
| Kernel drifts from `ukp-platform` deb | Baking makes them one immutable artifact, bumped and rolled back together |
| Mutable `:latest-amd64` tag gives different nodes different kernels | Pin `UKP_KERNEL_DIGEST`; this is the same drift class as the ukpd tag→digest cache staleness that has burned us before |
| kraftlet `.28` needs new config | Every prior staging bump did; e2e blocks on `rollout status` so a bad tag fails loudly |
| Instances never reach Ready | Only the enterprise base carries the vsock/BootTimer boot-complete signal; confirm `base-compat` does |
| Image grows 37 MB | Accepted; smaller than the unused `ukp-openresty` already shipped |

## Open Questions for Unikraft

1. Send us `unikraft-cloud-staging.gpg`, or confirm the existing key
   (`3D6C 59FF…F499`) signs the staging feed. **Hard blocker.**
2. Do our existing `pkg.unikraft.com` credentials cover `/debian/cloud-staging/`?
3. Does `datum/base-compat` carry the guest boot-complete signal (vsock /
   BootTimer)? Without it instances boot and serve traffic but never report
   Ready, and IPv6 cannot be validated.
4. Is the host kernel required to version-match `ukp-platform`? Is skew
   detected and rejected, or does it fail later at guest boot? Is the contract
   `KERNEL_VCS_COMMIT`, `os.version`, or the `os.features` bitmap? ukpd clearly
   builds a feature bitmap, but carries no version-compatibility error string we
   could find — so we cannot tell whether skew is rejected or silently degraded.
5. **Can we pin by digest?** We want `datum/base-compat@sha256:…`, not
   `:latest-amd64`. Which digest pairs with `ukp-platform=6:0.12.0-61+c30d6b1`,
   and will you publish that mapping per release?
6. Is baking the kernel into our own private runtime image supported and
   licensed? `datum/base-compat` is a namespace you pushed for us, so we want
   one line in writing.
7. Is the raw-OCI fetch fully equivalent to `kraft pkg pull` — i.e. is anything
   else in the package consumed besides the layer annotated
   `org.unikraft.kernel.image`?
8. Which `config.json` keys does ukpd actually require? Is stripping
   `config`/`rootfs` correct, and is `created` load-bearing (the script
   fabricates `0001-01-01T00:00:00Z`)?
9. Is it safe to replace `<dest>/kernel` under a running ukpd? Does ukpd read it
   per instance creation or cache at start, and what happens to parked/standby
   snapshots taken against the previous kernel?
10. **Does dropping `runtime:` also close the boot-complete gap?** If the node
    supplies the enterprise kernel, does a kernel-less customer image report
    boot-complete with *no* vendor credentials at build time? This is the main
    prize — it would move the credential requirement off every customer image
    build, which is exactly what the `datumctl build` story (compute#174) needs.
11. When do `ukp-agent 0.2.5-71`, `ukp-platform 0.12.0-61` and kraftlet
    `0.6.0-staging.28` reach `cloud/stable`? We would rather not carry a staging
    feed in the production image build any longer than necessary.
12. How does ukpd behave when `--images-kernel-path` points at a missing or empty
    directory — hard failure, or fall back to the image's own kernel?
13. Does `0.6.0-staging.28` still honor
    `KRAFTLET_ENABLE_PLATFORM_HEALTH_CONDITION=false`? Without it our
    direct-connect virtual-kubelet nodes go permanently NotReady.
14. Does IPv6 need a new kraftlet flag, or is it purely CNI/NAD-driven?
15. Is `.28` compatible with our *stable* `ukp-platform 0.12.0-5`, or does IPv6
    also require the staging platform? This decides whether Track A is genuinely
    independent.
16. Is the kernel going to ship as a deb? That would replace the whole fetch
    step with one more pinned `UKP_*_VERSION` ARG.

## Alternatives

**An init container fetching at pod start.** The vendor's own suggestion, and
self-healing on upgrade. Rejected as the primary because it makes every pod
start depend on registry reachability, requires a reusable vendor credential on
every node, and decouples the kernel from the ukp-platform version it runs
under. Retained as the documented fallback if baking turns out to be
unlicensed — see [Kernel delivery](#kernel-delivery).

**A persistent kernel on the `ukp-data` UserVolume.** Better than an emptyDir
(survives reboots, a registry blip cannot block a restart), but still carries
the node-side credential and the version-drift problem. This is the shape the
fallback should take if we need it.

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
