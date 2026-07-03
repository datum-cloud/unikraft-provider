# Investigation: running the Unikraft runtime in containers

Datum Cloud operates Unikraft Cloud platform (`ukp`) hosts today via the
manual, box-at-a-time install process in
[`unikraft-runtime-provisioning.md`](../../unikraft-runtime-provisioning.md).
To operate edge locations as a fleet, we investigated packaging the
runtime as containers delivered by Kubernetes. This document records what
we examined, how we validated it, and the results — the proposal built on
it is the [containerized runtime enhancement](./README.md). Findings come
from two machines: a vendor-installed reference box (`dal-same-ram`,
Debian 13, 64-core AMD, 251 GiB RAM, `stable` packages) and a blank box
(`decem`, Debian 13, AMD EPYC 9355P, 125 GiB RAM, 2×1.7 TB NVMe) where the
containerized runtime was deployed. No credential values appear in this
document.

## What the runtime is made of

The vendor install ships eight Debian packages:

| Package | Version observed | Ships |
| --- | --- | --- |
| `ukp-platform` | `0.10.0-9` | `ukpd` daemon + launcher, `ukp-vol` tool, OpenResty config templates |
| `ukp-firecracker` | `1.9.0-338` | `/usr/bin/firecracker` (Unikraft fork) |
| `ukp-agent` | `0.2.2` | image agent |
| `ukp-networking` | `1.5.2` | `netsetup` (TAP pre-creation), `firewall`, sysctls |
| `ukp-coredns` | `1.14.3` | CoreDNS + config template |
| `ukp-openresty` | `1.29.2` | edge proxy |
| `ukp-lego` | `5.0.0` | ACME certificate issuance (cron, no daemon) |
| `ukp-config` | `1.4.13` | `/etc/ukp.conf`, systemd targets, mount barrier |

Every service is a plain binary started by a bash launcher that sources
`/etc/ukp.conf` and `exec`s; systemd provides ordering and restart policy
but nothing the binaries require. The daemon as it runs on the reference
box:

```
ukpd -w /var/lib/ukp/data/platform --pid-file /var/run/ukp/ukpd.pid
     --images-path /var/lib/ukp/images --stor-vol-path /var/lib/ukp/volumes
     --stor-vol-tool /usr/lib/ukp/platform/tools/ukp-vol
     --stor-squota-uid 101 --api-endpoint 127.0.0.1:45232
     --users /var/lib/ukp/data/users.json
     --net-ip 172.16.0.0/12 --net-mode isolates --net-count 256
     --net-proxy-api-endpoint /var/run/ukp/ukpd-proxy.api
     --vmm-path /usr/bin/firecracker
     --hac-enable --hac-api-endpoint /var/run/ukp/agent.api
     --vmm-initrd-map-shared
```

The API listens on loopback (`127.0.0.1:45232`) and matches the public
[`unikraft-cloud/openapi`](https://github.com/unikraft-cloud/openapi)
spec; lifecycle operations (`/v1/instances[/{uuid}]/start|stop|suspend`)
are `PUT`. Each microVM is a Firecracker process spawned as a direct
child of `ukpd` — no jailer — inheriting its TAP file descriptor and,
for standby instances, a memfd snapshot:

```
firecracker --db-path /var/lib/ukp/data/platform --ukp-sock ukp.sock --no-api
            --uuid <instance-uuid> --ukp-snapshot-mode external
            --config-file vmm.json --startdata-file :81
            --boot-timer --initrd-map-shared --snapshot-prefetch-order 8
```

It reaches KVM directly (`anon_inode:kvm-vm` / `kvm-vcpu` descriptors), so
there is exactly one level of virtualization whether the daemon runs in a
container or not. In `standby`, no process exists at all: guest state is
held as a snapshot and the instance wakes on inbound traffic.

## The container deployment

One image, built from the vendor's apt repository, with the stock
launchers as container entrypoints. It runs as a privileged DaemonSet pod
per node — `netsetup` as an init container, then `ukpd`, the image agent,
and CoreDNS — using the host network and IPC namespaces. The vendor's
edge proxy, certificate stack, and firewall are not deployed: Datum's
platform owns ingress and network policy, and their absence proved
non-fatal (the daemon logs `FW: Failed to initialize firewall state:
ENOENT` and continues; nothing else references them).

What the containers need from the host:

| Requirement | Detail |
| --- | --- |
| Devices | `/dev/kvm`, `/dev/net/tun` |
| Kernel modules | `kvm`, `kvm_amd`/`kvm_intel`, `tun` (no `vhost`) |
| Storage | `/var/lib/ukp` on XFS mounted with `usrquota,grpquota,prjquota`; `/var/run/ukp` (sockets); `/var/log/ukp` |
| Namespaces | host network (TAPs, loopback API) and host IPC (daemon↔VMM shared memory) |
| Sysctls | the vendor's `20-ukp*.conf` set |

The image build lives in
[`build/ukp-runtime/`](../../../build/ukp-runtime/) and the deployment
manifests in
[`config/dependencies/ukp-runtime/`](../../../config/dependencies/ukp-runtime/);
the [enhancement](./README.md#design-details) explains the design.

## Confirming an instance comes online

Everything below is reproducible with `curl` against the loopback API
(`$TOKEN` is the user's `auth_token` from `users.json`). An instance is
created, observed to reach `running`, and — the authoritative check —
its guest answers HTTP over its TAP device, which exercises the VMM,
guest kernel, guest application, and virtio-net path rather than just the
control plane's bookkeeping.

Create the instance (`autostart` boots it immediately; `restart_policy`
and `scale_to_zero` are create-time only — a later PATCH of
`restart_policy` is rejected):

```console
AUTH="Authorization: Bearer $TOKEN"
API='http://127.0.0.1:45232/v1'

curl -s -X POST -H "$AUTH" "$API/instances" -d '{
  "image": "datum/httpserver-go121:latest",
  "memory_mb": 256,
  "autostart": true,
  "restart_policy": "always",
  "scale_to_zero": {"policy": "idle", "stateful": true, "cooldown_time_ms": 3000}
}'
```

```json
{
  "status": "success",
  "data": {
    "instances": [
      {
        "status": "success",
        "uuid": "31020cdc-eee4-440a-8b0b-57c4690ae3d0",
        "name": "httpserver-go121-f3vk3",
        "private_fqdn": "httpserver-go121-f3vk3.internal",
        "private_ip": "172.16.0.9",
        "state": "starting"
      }
    ]
  }
}
```

Poll until it reports `running`; `boot_time_us` is the VMM's
`--boot-timer` measurement, covering VMM spawn through the guest's
boot-complete signal:

```console
curl -s -H "$AUTH" "$API/instances/31020cdc-eee4-440a-8b0b-57c4690ae3d0"
```

```json
{
  "status": "success",
  "data": {
    "instances": [
      {
        "uuid": "31020cdc-eee4-440a-8b0b-57c4690ae3d0",
        "name": "httpserver-go121-f3vk3",
        "state": "running",
        "image": "oci://unikraft.io/datum/httpserver-go121@sha256:b7afd8c4…",
        "memory_mb": 256,
        "vcpus": 1,
        "boot_time_us": 71685,
        "restart_policy": "always",
        "scale_to_zero": {
          "enabled": true,
          "policy": "idle",
          "stateful": true,
          "cooldown_time_ms": 3000
        },
        "network_interfaces": [
          {
            "private_ip": "172.16.0.9",
            "mac": "12:b0:ac:10:00:09"
          }
        ]
      }
    ]
  }
}
```

Confirm the guest is actually serving — this is the signal we trust:

```console
curl -s -o /dev/null -w '%{http_code} %{time_total}s\n' http://172.16.0.9:8080/
```

```text
200 0.000957s
```

Lifecycle verbs are `PUT` (a `POST` to the same path returns
`{"status":"error","message":"No API endpoint"}`):

```console
curl -s -X PUT -H "$AUTH" "$API/instances/31020cdc-eee4-440a-8b0b-57c4690ae3d0/stop"
```

```json
{
  "status": "success",
  "data": {
    "instances": [
      {
        "status": "success",
        "uuid": "31020cdc-eee4-440a-8b0b-57c4690ae3d0",
        "name": "httpserver-go121-f3vk3",
        "state": "stopping",
        "previous_state": "running"
      }
    ]
  }
}
```

```console
curl -s -X PUT -H "$AUTH" "$API/instances/31020cdc-eee4-440a-8b0b-57c4690ae3d0/start"
```

```json
{
  "status": "success",
  "data": {
    "instances": [
      {
        "status": "success",
        "uuid": "31020cdc-eee4-440a-8b0b-57c4690ae3d0",
        "name": "httpserver-go121-f3vk3",
        "previous_state": "stopped",
        "state": "starting"
      }
    ]
  }
}
```

Boot times were measured with five warm cycles per environment — stop,
wait for `stopped`, start, wait for `running`, read `boot_time_us` — on
the **same image digest** in both environments
(`datum/httpserver-go121@sha256:b7afd8c4…`):

| Test (2026-07-02) | `boot_time_us` per cycle |
| --- | --- |
| Native install (`dal-same-ram`) | 82348, 81888, 82184, 82110, 82027 |
| Containerized, compose (`decem`) | 71685, 72665, 71457, 71010, 73011 |
| Containerized, k3s DaemonSet (`decem`) | 72890 |
| nginx image, containerized (`decem`) | 23602, 23523 |

The containerized runtime boots the identical image consistently ~10 ms
faster than the native reference (newer CPU; the delta attributable to
containerization is zero or negative), and the spread within each
environment is under 2 ms. Boot time is dominated by the guest image —
the Go runtime accounts for roughly 50 ms; nginx boots in ~24 ms on the
same stack. A cold start including the on-demand image pull measured
~6.6 s.

## Runtime restarts and guest survival

Guests must outlive the control plane that manages them, so restarts were
tested in every mode:

- **Graceful daemon restart, native** (`systemctl restart ukp-platform`
  with a running guest): the daemon parks the scale-to-zero instance into
  `standby` on shutdown (`start_count` unchanged), and the first HTTP
  probe after the restart wakes it — it answered `200`.
- **Graceful pod replacement, containerized** (`kubectl delete pod`,
  default grace period): identical behavior. The instance reappeared as
  `standby`, the replacement pod was ready in ~4 s, and the first packet
  to its TAP address woke it — `200` in ~1.0 s wall clock including
  post-restart warmup. The park-and-wake behavior is vendor-designed, not
  a container artifact.
- **Ungraceful container kill**: the guest process dies with the
  container; instances created with `restart_policy: always` were
  restarted automatically by the recovering daemon and served traffic
  again within ~10 s (`start_count` increments). This held across
  container restarts, pod deletions, and full stack rebuilds.

One caution from an interrupted **first boot**: a hard kill while the
daemon is still creating its databases leaves them half-written, and on
the next start the daemon hangs on the corrupt state rather than exiting.

## Guest DNS

The boot arguments pin each guest's only DNS resolver to its host-side
TAP gateway address (`netdev.ip="<ip>/30:<gw>:<gw>:…"` — the third field
is the resolver). Without CoreDNS deployed, nothing listens there and
guests have no DNS at all, `.internal` and public names alike. With
CoreDNS restored to the pod, upstream resolution through the exact
resolver address guests use works (public A records resolve via
`dig @<gateway>`). `.internal` lookups returned `NXDOMAIN` for
host-originated queries from any gateway — consistent with the iDNS
scoping answers to queries sourced from guest addresses, but verifying
that from inside a guest needs a DNS-capable image and remains open.

## Validation with official client tooling

To replicate real platform usage, the official Unikraft CLI
([`unikraft`](https://github.com/unikraft/cli), 0.3.0) was pointed at the
containerized runtime. A standalone metro needs no control-plane login —
a profile with a static metro entry is enough:

```yaml
# ~/.config/unikraft/config.yaml
profile: decem
profiles:
  decem:
    type: cloud
    token: <users.json auth_token>
    metros:
      - name: decem
        endpoint: http://127.0.0.1:45232
        insecure: true
    metro_default: decem
```

The full instance lifecycle through the CLI, with its actual output:

```console
$ unikraft instances create --name cli-demo --image datum/httpserver-go121:latest --memory 256 --autostart
metro:        decem
name:         cli-demo
uuid:         f4f68e6b-d35a-4f4a-ab5b-ad8e410f6ceb
state:        starting
image:        datum/httpserver-go121
resources:
  memory:     256MiB
  vcpus:      1
networks:
- uuid:       858d46fa-4d2a-416a-b09c-ef76dfd94905
  private-ip: 172.16.0.25
  mac:        12:b0:ac:10:00:19
timestamps:
  created:    just now

$ unikraft instances get cli-demo
metro:        decem
name:         cli-demo
state:        running
…

$ unikraft instances list
METRO  NAME                    STATE    IMAGE                   ARGS  MEMORY  VCPUS  FQDN  CREATED
decem  cli-demo                running  datum/httpserver-go121        256MiB  1            just now
decem  httpserver-go121-f3vk3  running  datum/httpserver-go121        256MiB  1            2 hours ago
decem  httpserver-go121-pukg7  running  datum/httpserver-go121        256MiB  1            2 hours ago
decem  nginx-b3q5v             stopped  nginx                         128MiB  1            2 hours ago

$ unikraft volumes list
METRO  NAME  STATE  SIZE  CREATED

$ unikraft instances remove cli-demo
cli-demo
```

Create (with `--autostart`), get, list, remove, and volume listing all
work unmodified, and the CLI-created guest answered HTTP `200` on its TAP
address. KraftKit (`kraft` 0.12) was also validated against the same
runtime via the CLI's legacy environment-variable compatibility
(`UKC_METRO`/`UKC_TOKEN`).

Two constraints surfaced: tokens must use the vendor's format
(`base64("<id>$<org>.users.kraftcloud:<secret>")` — the tooling derives
the organization from it), and user names in `users.json` are immutable
once imported (a rename makes the daemon fail startup with "User name
immutability violation"). One difference against the native box remains
open: the organization-scoped image listing (proxied through the agent to
the vendor's Harbor) returns a Harbor 401 on the containerized deployment
while working natively with identical pull credentials; on-demand image
pulls work in both.

As a final check, the complete shipped configuration — the image built
from the vendor's apt repository, ConfigMap-injected configuration, TCP
readiness probing, and CoreDNS — was deployed as a whole and re-ran the
battery in one pass: warm boot 71.1 ms, guest HTTP over TAP, DNS through
the gateway resolver, park-and-wake across a pod replacement, and the
client tooling, all passing.
