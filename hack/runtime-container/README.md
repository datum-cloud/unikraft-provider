# ukp runtime containerization prototype

Prototype for running the Unikraft Cloud platform (`ukp`) as containers,
per [`docs/unikraft-runtime-containerization.md`](../../docs/unikraft-runtime-containerization.md).
Tracked by [datum-cloud/infra#3021](https://github.com/datum-cloud/infra/issues/3021).

## Layout

- `Dockerfile` — single image with all 8 ukp packages; the stock package
  launchers are the container entrypoints.
- `debs/` (gitignored) — the ukp `.deb`s, fetched from an existing
  installation (`apt-get download 'ukp-*'` on a box with repo credentials).
- `docker-compose.yaml` — the stack, pod-pattern: `netbase` owns the
  network/IPC namespaces, all services join them.
- `ukp.conf.local` — local test configuration (dummy credentials).
- `seed.sh` — seeds self-signed certs + users.json (API token
  `bG9jYWw6bG9jYWx0ZXN0`).
- `box/host-bootstrap.sh` — bare-metal node prerequisites.
- `box/validate.sh` — box-day validation battery (phases a–d).

## Local use (macOS / Colima)

```console
docker build --platform linux/amd64 -t ukp-runtime:proto .
docker compose up -d
```

Local runs use qemu-user amd64 emulation. Three hard limits there:

1. **No `/dev/kvm`** — instance start stops at the VMM.
2. **No NETLINK_NETFILTER translation** — `ipset`/`nft`/`iptables-nft`
   cannot run, so the `firewall` service sits behind a compose profile
   (`--profile firewall`, real hardware only).
3. **ukpd cannot complete first boot under emulation.** Its
   sparse-mmap-heavy database initialization gets amplified by qemu-user
   (observed: ~405GB VA / 2.4GB RSS ~17min in) until the OOM killer takes
   it — and possibly innocent neighbors in the same VM — down. A killed
   first boot leaves half-written DB files, and **ukpd hangs (rather than
   exits) on a corrupt database** on the next start, so wipe the volumes
   after any interrupted first boot.

What local runs do validate: image build/packaging, launcher behavior,
config handling, TAP pre-creation via netsetup, and ukpd's early boot
(user import, DB creation start). Full-stack bringup is box-only.

## Box use (bare metal)

```console
box/host-bootstrap.sh          # modules, sysctls, data mount, docker
docker build --platform linux/amd64 -t ukp-runtime:proto .
box/validate.sh                # phases: bringup, images, instance, blast radius
```

Real image pulls need real `AGENT_PULL_*` credentials in the mounted
ukp.conf (see the native box's `/etc/ukp.conf`; values in 1password).

## Known evaluation findings to (re)confirm on the box

- Instance boot latency vs native (~82ms observed on `dal-same-ram`).
- Standby wake-on-traffic works identically.
- `docker compose restart ukpd` kills all guests (validate.sh phase d) —
  the central packaging risk for upgrades.
- The firewall script's default-DROP takeover of the host.
