# go-netdump

Reports a guest's network state **from inside the unikernel**. Built for the
question "what routes does this VM actually have?", which is hard to answer
from the host: a microVM has no shell, and the runtime's own API under-reports
what it configured.

It prints a full report to stdout at boot — the console is the only channel
that still works when the guest has no usable route — and serves the same
report over HTTP for re-querying a guest whose networking does work.

## What it reports

| Section | Source | Notes |
|---|---|---|
| `route table` | `NETLINK_ROUTE` `RTM_GETROUTE` | the direct answer; a dump failure is itself the finding and is printed |
| `neighbours` | `RTM_GETNEIGH` | whether the guest ever resolved its gateway |
| `interfaces` | `net.Interfaces()` | also netlink, so it can fail independently |
| `route probes` | connectionless `net.Dial("udp", …)` | works even with no netlink: the dial performs a route lookup and source selection without putting a packet on the wire |
| `tcp probes` | `net.DialTimeout` | real reachability, opt-in via `NETDUMP_TCP_TARGETS` |
| `files` | `/proc/net/*`, `/etc/resolv.conf`, `/etc/hosts` | absent files are reported, not treated as errors |

The route probes matter because a unikernel may implement no netlink at all.
They answer "is there a route to X, and which source would be chosen" purely
through the socket API, which any stack supports.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `NETDUMP_TARGETS` | — | extra comma-separated `host:port` route probes |
| `NETDUMP_TCP_TARGETS` | — | comma-separated `host:port` connect probes |
| `NETDUMP_TCP_TIMEOUT` | `5s` | per-probe connect timeout |
| `NETDUMP_INTERVAL` | — | re-dump to stdout on this interval, e.g. `15s` |
| `PORT` | `8080` | HTTP listen port |

`NETDUMP_INTERVAL` is how you watch state *arrive* — a Router Advertisement
installing a default route, say — on a guest you cannot yet reach.

## Endpoints

- `GET /` — JSON report
- `GET /text` — the same report in the console format
- `GET /healthz` — `ok`

## Build

Identical to [`../go-hello`](../go-hello/README.md): a **static PIE**, because
Unikraft's elfloader rejects non-PIE binaries and the `scratch` rootfs ships no
dynamic loader. Use the **enterprise** base runtime
(`index.unikraft.io/official/base:latest`) or the instance boots and serves but
never reports boot-complete, leaving the pod `Pending` forever.

```sh
docker run -d --name buildkitd --privileged moby/buildkit:latest

kraft pkg \
  --name docker.io/<user>/<repo>:go-netdump \
  --runtime index.unikraft.io/official/base:latest \
  --plat kraftcloud --arch x86_64 --no-prompt \
  --buildkit-host docker-container://buildkitd .
```

Deploy by digest, not tag — ukpd caches a tag's digest and never re-resolves it.
