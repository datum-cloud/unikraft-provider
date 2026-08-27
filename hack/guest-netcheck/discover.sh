#!/usr/bin/env bash
# Print the tap/MAC/address variables the other scripts need, for one instance.
#
#   ./discover.sh <kubeconfig> <vpc-id> <guest-prefix>
#
# vpc-id comes from `kubectl get vpc -A -o custom-columns=NAME:.metadata.name,VPC:.status.vpc`
# guest-prefix is the instance's address prefix, e.g. fd20:0:2::1:0:0
set -euo pipefail

KC=$1; VPC=$2; GUEST=$3
export KUBECONFIG=$KC
k() { kubectl exec -n unikraft-system nettest --request-timeout=60s -- "$@"; }

VRF="G0${VPC}V"
# The tap carrying this instance is the one its prefix routes out of.
TAP=$(k ip -6 route show vrf "$VRF" | awk -v g="$GUEST/96" '$1==g {print $3}')
[ -n "$TAP" ] || { echo "no tap found for $GUEST in $VRF" >&2; exit 1; }

HMAC=$(k cat "/sys/class/net/$TAP/address" | tr -d '\r')
HLL=$(k ip -6 addr show dev "$TAP" | awk '/inet6 fe80::/ {split($2,a,"/"); print a[1]}')
GMAC=$(k ip -6 neigh show dev "$TAP" | awk '/lladdr/ {print $3; exit}')
GLL=$(k ip -6 neigh show dev "$TAP" | awk '/^fe80::/ {print $1; exit}')

cat <<VARS
export KUBECONFIG=$KC
export VRF=$VRF
export TAP=$TAP
export HMAC=$HMAC
export HLL=$HLL
export GMAC=$GMAC
export GLL=$GLL
export GUEST=$GUEST
VARS
