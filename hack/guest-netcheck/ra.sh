#!/usr/bin/env bash
# Send an IPv6 Router Advertisement to a guest, giving it a default route via
# the host tap's link-local. No prefix option: each tap is a point-to-point
# link, so advertising the VPC prefix as on-link makes the guest try to resolve
# peers directly and nobody answers.
#
#   ./ra.sh <kubeconfig> <tap> [router-lifetime-seconds]   # 0 withdraws the route
set -euo pipefail

export KUBECONFIG=$1
TAP=$2
LIFETIME=${3:-9000}

kubectl exec -i -n unikraft-system nettest --request-timeout=60s -- sh -s <<SH
set -e
HMAC=\$(cat /sys/class/net/$TAP/address)
GLL=\$(ip -6 neigh show dev $TAP | awk '/^fe80::/ {print \$1; exit}')
[ -n "\$GLL" ] || { echo "no guest link-local on $TAP; is the instance running?" >&2; exit 1; }

# ICMPv6 type 134, code 0, checksum 0 (the kernel fills it on a raw ICMPv6
# socket); cur hop limit 64; no M/O flags; router lifetime; reachable time and
# retrans timer 0. Then the source link-layer address and MTU options.
MACHEX=\$(printf '\\\\x%s' \$(echo \$HMAC | tr ':' ' '))
LIFEHEX=\$(printf '\\\\x%02x\\\\x%02x' \$(( $LIFETIME >> 8 )) \$(( $LIFETIME & 255 )))

printf "\\x86\\x00\\x00\\x00\\x40\\x00\${LIFEHEX}\\x00\\x00\\x00\\x00\\x00\\x00\\x00\\x00\\x01\\x01\${MACHEX}\\x05\\x01\\x00\\x00\\x00\\x00\\x05\\xa0" \
  | socat -u - "IP6-SENDTO:[\${GLL}%$TAP]:58,ipv6-unicast-hops=255"

echo "RA sent on $TAP -> \$GLL (default via \$HMAC, lifetime ${LIFETIME}s)"
SH
