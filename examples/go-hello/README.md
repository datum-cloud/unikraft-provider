# go-hello

A minimal Go HTTP service packaged as a Unikraft unikernel for the Datum
Unikraft runtime (ukpd). It listens on `0.0.0.0:8080`, returns a small JSON
document at `/`, and answers `GET /healthz` with `ok`.

## Layout

- `main.go` — standard-library HTTP server (no third-party deps).
- `Dockerfile` — cross-compiles a static-PIE `linux/amd64` binary on the native
  host (`--platform=$BUILDPLATFORM`) and copies it into a `scratch` rootfs.
- `Kraftfile` — spec `v0.6`; base runtime plus the Dockerfile as the rootfs and
  `cmd: ["/server"]`.

## Static-PIE requirement

Unikraft's elfloader rejects non-PIE executables, and the `scratch` rootfs
ships no dynamic loader — so the binary must be a **static PIE**. A plain
`-buildmode=pie` still records an `INTERP` header for `ld-linux`, which is
absent on `scratch`; the Dockerfile therefore links externally with
`-static-pie` (via the x86_64 cross toolchain) to drop the interpreter and all
shared-library dependencies. Verify with `readelf`: type `DYN`, no `INTERP`
program header, no `NEEDED` entries.

## Choose a base runtime

The base runtime decides whether instances ever report ready:

| Base runtime | Result on the Datum runtime |
|---|---|
| `index.unikraft.io/official/base` (enterprise, registry credentials required) | Instance reaches `running`; pod goes `Running / Ready` |
| `index.unikraft.io/unikraft.org/base` (community, anonymous) | Boots and serves traffic, but never reports boot-complete — the pod stays `Pending` forever |

The difference is guest-side vsock/boot-timer support that only the enterprise
base carries; it is invisible in the image metadata.

## Build, package, and push

Packaging the Dockerfile rootfs needs a BuildKit host:

```sh
docker run -d --name buildkitd --privileged moby/buildkit:latest
```

**Enterprise base** (requires `index.unikraft.io` credentials in your kraft
config; produces a `kraftcloud/x86_64` image directly):

```sh
kraft pkg \
  --name docker.io/<user>/<repo>:go-hello \
  --runtime index.unikraft.io/official/base:latest \
  --plat kraftcloud --arch x86_64 --no-prompt \
  --buildkit-host docker-container://buildkitd .
```

**Community base** (anonymous; for experimentation only — see table above).
Build with `--plat fc`, then relabel the `fc` child's `platform.os` to
`kraftcloud` in the OCI index before pushing — ukpd only accepts images whose
index has a `kraftcloud` child:

```sh
kraft pkg \
  --name docker.io/<user>/<repo>:go-hello \
  --runtime index.unikraft.io/unikraft.org/base:latest \
  --plat fc --arch x86_64 --no-prompt \
  --buildkit-host docker-container://buildkitd .
```

Push the exported OCI layout with a tool that reuses your Docker credentials
(`kraft pkg export -o go-hello.tar …`, then `crane push`).

## Deploying

Reference the pushed image from a pod or Datum `Instance` container spec with
container port `8080`, and add an image pull secret if the repository is
private. Prefer **digest references** (`repo@sha256:…`): the runtime caches
tag→digest resolution per node and does not re-resolve a tag after it moves,
so re-pushing under the same tag silently redeploys the old image.
