# IPv6 DNS on Galactic-attached Unikraft microVMs

Investigation on 2026-10-01 in `us-central-1-staging-lab`, using disposable
microVMs on `kraftlet-eris-giune`. Existing workloads and shared runtime/DNS
configuration were not changed.

Cleanup was verified: the test namespace, ukpd instances, test BGP resources,
and test TAP/VRF interfaces were gone at the end of the investigation.

## Finding

Public IPv6 DNS already works through Galactic/NAT66. The missing piece is
guest resolver configuration. Writing a public IPv6 nameserver into the guest's
`/etc/resolv.conf` made ordinary application resolution and HTTPS work.

This corrects two assumptions in the older runtime DNS documentation:

- The tested Galactic-attached guest had **no `/etc/resolv.conf`**, rather than
  a file pointing at the host gateway. Inspect the actual guest/image before
  assuming its DNS packets are being sent to CoreDNS.
- Passing `Pod.spec.dnsConfig` to ukpd's existing `nameserver` field alone is
  insufficient: this ukpd build rejects IPv6 values in that field.

## Versions and scope

- Runtime image: `ghcr.io/datum-cloud/ukp-runtime:v0.0.0-main-20261001-135305`.
- `ukp-platform`: `6:0.13.0-46+c3e7b7e-2staging+deb13`.
- Kraftlet: `ghcr.io/unikraft-cloud/kraftlet:0.6.0-staging.56`.
- Guest: `ghcr.io/datum-labs/compute-hello@sha256:08b15de763db3a64067140654ef97fbe02e7ed5415d8509227a8416c09f70542`.
- ukpd reports `type: micro`; the guest exposes a Linux `/proc` filesystem and
  runs Node.js 24.16.0. These results establish behavior for this deployed
  Unikraft microVM runtime, not every native Unikraft/lwIP image.
- Isolated test namespace: `unikraft-dns-probe`; test VPC address space:
  `fd42:da7a:d053::/64`. No shared nftables, BPF, or CoreDNS changes were needed.

## Experiments

| Input or operation | Observed result |
| --- | --- |
| Pod `dnsPolicy: None`, IPv6 `dnsConfig.nameservers` | ukpd instance has no nameserver override; guest resolver file absent |
| Create via ukpd API with `nameserver: 2606:4700:4700::1111` | HTTP 400, `Invalid IPv4 address '2606:4700:4700::1111'` |
| Same API request with `nameserver: 1.1.1.1` | Instance creation succeeds |
| Explicit Cloudflare IPv6 UDP query for `example.com` AAAA | Two IPv6 answers |
| Explicit Cloudflare IPv6 TCP query for `example.com` AAAA | DNS response code 0, two answers |
| Ordinary `dns.lookup` before writing resolver file | `EAI_AGAIN` |
| Write IPv6 resolver file; repeat ordinary `dns.lookup` | IPv6 address returned |
| `fetch("https://example.com")` after writing resolver file | HTTP 200 |
| Add `dns.nameservers` to the instance's `unikraft.com/cni` annotation, stop/start | Resolver file still absent; explicit write still fixes resolution |
| ConfigMap `subPath: resolv.conf` mounted at `/etc/resolv.conf` | Runtime mounts a ROM **directory** at that path; reading it fails with `EISDIR` |
| Directory-mounted ConfigMap and startup script copying the resolver file | Normal lookup, UDP/TCP DNS, and HTTPS succeed without a write in the diagnostic application |
| Cold stop/start of the startup-script VM | Resolver file present before application start; all checks pass again |

The API accepted the CNI annotation update. That experiment establishes that
changing the annotation alone does not configure DNS on a subsequent cold boot;
it does not establish how an alternative create-time CNI integration would behave.

The successful before/after guest output was:

```json
{"tag":"default-lookup-before","value":{"error":"getaddrinfo EAI_AGAIN example.com","code":"EAI_AGAIN"}}
{"tag":"cloudflare-udp-AAAA","value":["2606:4700:10::ac42:93f3","2606:4700:10::6814:179a"]}
{"tag":"cloudflare-tcp-AAAA","value":{"rcode":0,"answers":2,"bytes":87}}
{"tag":"resolv-after","value":"nameserver 2606:4700:4700::1111\noptions timeout:2 attempts:1\n"}
{"tag":"default-lookup-after","value":{"address":"2606:4700:10::ac42:93f3","family":6}}
{"tag":"https-after","value":{"status":200}}
```

## Enable the provider workaround

Set this in the provider's deployment configuration:

```yaml
downstreamResourceManagement:
  instanceDNS:
    initializeGuest: true
    nameservers:
      - "2606:4700:4700::1111"
      - "2001:4860:4860::8888"
    # searches: [example.internal]
```

`initializeGuest` defaults to false. Enable it only for workloads whose images
provide `/bin/sh`, `cat`, and a writable `/etc/resolv.conf`. Every container must
supply its application `command` explicitly, for example
`command: ["/usr/local/bin/node"]`, `args: ["/app/server.mjs"]`. The provider cannot
recover an image's default entrypoint after overriding it; missing commands fail
Pod creation with a reconciliation error. Shell-less images need runtime support.

For each new Instance Pod, the provider creates an immutable ConfigMap containing
the configured nameservers, optional search domains, and a startup script. It
mounts the directory read-only at `/run/datum-dns`. The script copies the resolver
file to `/etc/resolv.conf`, then uses `exec "$@"` to start the supplied command and
arguments. Copy failures stop startup. The ConfigMap belongs to the Instance and
is garbage-collected with it. User ConfigMap and Secret contents are not read;
provider-owned ConfigMaps are read directly without a cluster-wide informer.

The volume name `unikraft-guest-dns` and mount path `/run/datum-dns` are reserved
when enabled. Mounts overlapping that path or `/etc/resolv.conf` are rejected.
Do not use `subPath` to mount the resolver file directly: the tested runtime
mounts a directory there. Do not mount a DNS-only ConfigMap over `/etc`.

Existing Pods retain their command and resolver configuration even when provider
configuration changes. Recreate Instances through the normal workload rollout
process to adopt the setting. Disabling it affects new Pods; previously created
Pods keep their startup wrapper and immutable resolver ConfigMap.

The provider-generated Pod and ConfigMap were also deployed directly to the lab
as `dns-provider` (ukpd instance `6ea674fa-38c9-4c27-a077-341ee80ca2ec`), with the
application executable in `command` and its script path in `args`. The generated
resolver included both IPv6 servers and the configured search domain. Normal
application lookup, UDP/TCP DNS, and HTTPS passed on initial boot after route
convergence and again after cold stop/start. The first lookup on initial boot
returned `EAI_AGAIN` before the default route appeared; the initializer does not
wait for network readiness. The diagnostic application did not write the resolver
file itself. The shared provider deployment was not replaced for this test. Public resolvers provide
public DNS; they do not provide Kubernetes service or CoreDNS `.internal` names.

## Platform fix

Keep `downstreamResourceManagement.instanceDNS` as the provider's configuration
contract. Complete the path from Pod DNS configuration through kraftlet to
guest initialization, with IPv6 support and actual resolver-file generation.
The existing IPv4-only `nameserver` API is not sufficient for this contract.
Agree the runtime API with Unikraft before implementing the kraftlet mapping;
include multiple servers, search domains, and DNS options if supported.

Accept the fix only after a cold boot proves all three: the intended resolver
file exists before the application starts, ordinary application resolution
works, and UDP/TCP DNS plus an HTTPS request work over the Galactic attachment.
The mounted-startup workaround avoids changes to the shared network datapath.

Host-local DNS remains an architectural option, but does not by itself fix a
missing guest resolver configuration. For guests that actually address a host
resolver, Galactic's TC egress default can bypass nftables PREROUTING. A local
resolver needs a per-VRF pass-through/route, TCP and UDP handling, and a correct
VRF return path. The existing CoreDNS wrapper also matches only `ukp*` interfaces
and rewrites AAAA queries to A; both need attention before reusing it for this
IPv6 path.

## Test details

The diagnostic script is [probe.js](probe.js). It makes explicit UDP and TCP
queries, tests the normal libc resolver, writes the resolver file, and tests
HTTPS. Run it only in a disposable guest: it intentionally changes that guest's
resolver configuration. To validate a startup initializer, remove its explicit
file-write block so normal lookup success cannot be attributed to the probe.

Disable scale-to-zero for the diagnostic Pod with
`cloud.unikraft.v1.instances/scale_to_zero.policy: "off"`. Otherwise the default
one-second idle policy can terminate the guest while a failed DNS query waits.
IPv6 default-route readiness also depends on the guest receiving Galactic's
Router Advertisement; allow route convergence before interpreting early errors.

The exec-enabled guest's kraftlet exec request returned `No API endpoint` for
`/v1/instances/<uuid>/plugins/kexec: exec`, so diagnostics ran as the guest
application and their output was collected with `kubectl logs`.

References: [Unikraft instance API](https://unikraft.com/docs/api/platform/v1/instances),
[Cloudflare resolver addresses](https://developers.cloudflare.com/1.1.1.1/ip-addresses/),
[Google resolver addresses](https://developers.google.com/speed/public-dns/docs/using).
