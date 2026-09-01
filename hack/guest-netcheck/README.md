# Inspecting guest networking, and proving cross-cell connectivity

A microVM guest has no shell, and the runtime API under-reports what it
configured. Everything here runs from the **host**, against the guest's tap.

Each runtime host has a `nettest` pod in `unikraft-system` (hostNetwork, so it
shares the host's netns) carrying `tcpdump`, `scapy`, `curl`, `nmap`, `socat`.
All commands below shell through it.

Verified 2026-08-27 across `us-central-1-staging-lab` (Dallas) and
`us-east-1-staging-lab` (Ashburn).

---

## 0. Discover the handles

```bash
# The VPC identifier must be identical in every cell for a network to be one VPC.
for KC in ~/.kube/edge-fleet/us-central-1-staging-lab.yaml \
          ~/.kube/edge-fleet/us-east-1-staging-lab.yaml; do
  KUBECONFIG=$KC kubectl get vpc -A \
    -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,VPC:.status.vpc
done

# Each instance's address and gateway.
KUBECONFIG=$KC kubectl get networkinterface -A \
  -o custom-columns=NAME:.metadata.name,ADDR:.spec.addresses\[\*\].address,GW:.spec.addresses\[\*\].gateway
```

Then derive the per-instance variables:

```bash
eval "$(./discover.sh ~/.kube/edge-fleet/us-central-1-staging-lab.yaml Aty7F5rG fd20:0:2::1:0:0)"
# exports KUBECONFIG VRF TAP HMAC HLL GMAC GLL GUEST
```

Naming: VRF is `G0<vpcid>V`, taps are `G0<vpcid><attach>H`.

---

## 1. Host-side state

Define a helper once (a function, not a string variable — zsh does not
word-split those):

```bash
k() { kubectl exec -n unikraft-system nettest --request-timeout=60s -- "$@"; }

k ip -6 addr show dev $TAP           # gateway /128 lives here
k ip -6 route show vrf $VRF          # local prefixes, one per attachment
k ip -6 neigh show dev $TAP          # has the guest ever been resolved?
```

Route targets must match across cells, and BGP must be up:

```bash
kubectl get bgpvrfinstance -n galactic-system \
  -o custom-columns=NAME:.metadata.name,IMPORT:.spec.importRouteTargets\[\*\].value
kubectl get bgprouter -n galactic-system -o jsonpath='{.items[*].status.peers}{"\n"}'
```

**Do not diagnose with `ip -6 route` alone.** Galactic moved SRv6 egress out of
the kernel into TC-BPF, so remote prefixes never appear in the FIB and
`grep -c seg6` is legitimately `0`. Check the hooks instead:

```bash
k tc filter show dev bond0 ingress     # usid_ingress, also on every bond slave
k tc filter show dev $TAP ingress      # usid_egress, per attachment
k ls /sys/fs/bpf/galactic/             # egress_route_table and friends
```

---

## 2. Is the guest alive?

Link-local is on-link by construction, so this works even with an empty guest
routing table. If this fails the guest itself is broken; if it succeeds, any
further failure is routing.

```bash
k ping -6 -c 3 -I $TAP $GLL
k curl -6 -sS -m 6 "http://[$GLL%$TAP]:8080/"
```

Compare against the guest's **global** address, which needs a return route:

```bash
k curl -6 -sS -m 6 --interface $VRF "http://[$GUEST]:8080/"
```

Alive on link-local but dead on global == the guest has no usable route.

---

## 3. Reconstruct the guest's routing table from outside

Inject echo requests with varied **source** addresses and see which draw a
reply. Each reply proves the guest can build a return path to that scope.

```bash
kubectl cp sweep.py unikraft-system/nettest:/tmp/sweep.py
k python3 /tmp/sweep.py $TAP $HMAC $GMAC $GUEST \
    $HLL \                  # link-local  -> stack alive
    fd20:0:2::1 \           # gateway
    fd20:0:2::2:0:0 \       # peer in the same cell
    fd20:0:2:1:0:1:: \      # peer in the other cell
    2001:db8::1             # anywhere    -> default route present?
```

```
SOURCE                                       REPLIED?
fe80::78c0:63ff:fe77:1379                    yes
fd20:0:2::1                                  yes
fd20:0:2::2:0:0                              NO     <-- same-cell peer unreachable
fd20:0:2:1:0:1::                             yes
2001:db8::1                                  yes
```

Watch what the guest *does* with a failing destination:

```bash
k tcpdump -nn -i $TAP -c 8 icmp6
# fd20:0:2::1:0:0 > ff02::1:ff00:0: neighbor solicitation, who has fd20:0:2::2:0:0
```

Unanswered NS means the guest thinks that peer is **on-link**. Each tap is a
point-to-point link, so it isn't — the host routes between taps.

---

## 4. Give the guest a route

ukpd applies the address but never programs a route, so a fresh guest answers
on link-local and is otherwise unreachable. A Router Advertisement fixes it
until the router lifetime expires.

**Default route via link-local, no prefix option.** Advertising the VPC /64 as
on-link (`PrefixInfo L=1`) is what produced the unanswered NS above.

```bash
kubectl cp ra.py unikraft-system/nettest:/tmp/ra.py
k python3 /tmp/ra.py $TAP $HMAC $HLL $GMAC $GLL

# Withdraw a previously advertised on-link prefix (L=1 + validlifetime=0):
k python3 /tmp/ra.py $TAP $HMAC $HLL $GMAC $GLL fd20:0:2::
```

Re-run the sweep; every source should now reply.

---

## 5. Prove cross-cell connectivity

The encap hook is TC **ingress** on the tap, so it only sees traffic a guest
sends. Host-originated pings bypass it and prove nothing. Make the guest
originate instead: inject a SYN with the far-end guest as source, and its
SYN-ACK crosses the fabric for real.

Capture on both taps, then inject at Ashburn:

```bash
kubectl cp inject-tcp.py unikraft-system/nettest:/tmp/inject-tcp.py

KUBECONFIG=$DFW kubectl exec -n unikraft-system nettest -- \
  timeout 22 tcpdump -nn -i G0Aty7F5rG1y2H -c 10 'ip6 and tcp' &

KUBECONFIG=$IAD kubectl exec -n unikraft-system nettest -- \
  timeout 22 tcpdump -nn -i G0Aty7F5rG8I2H -c 10 'ip6 and tcp' &

sleep 6
KUBECONFIG=$IAD kubectl exec -n unikraft-system nettest -- python3 /tmp/inject-tcp.py \
  G0Aty7F5rG8I2H 6a:7e:93:1a:04:2b 12:b0:ac:10:00:01 \
  fd20:0:2::1:0:0 fd20:0:2:1:0:1::
wait
```

A passing run looks like this — note the ~30ms between the same packet
appearing on the two taps, and that **both** guests originate:

```
ASHBURN  16:37:52.261829  fd20:0:2:1:0:1::.8080 > fd20:0:2::1:0:0.44444  [S.]
DALLAS   16:37:52.292026  fd20:0:2:1:0:1::.8080 > fd20:0:2::1:0:0.44444  [S.]
DALLAS   16:37:52.292282  fd20:0:2::1:0:0.44444 > fd20:0:2:1:0:1::.8080  [R]
ASHBURN  16:37:52.322454  fd20:0:2::1:0:0.44444 > fd20:0:2:1:0:1::.8080  [R]
```

`inject-icmp.py` does the same with echo request/reply if you prefer.

---

## 6. From inside the guest

`examples/go-netdump` reports the guest's own route table (netlink
`RTM_GETROUTE`), neighbours, interfaces, and — for stacks with no netlink —
connectionless UDP-dial route probes. It prints to the console at boot, which
is the only channel that still works when routing is broken.
