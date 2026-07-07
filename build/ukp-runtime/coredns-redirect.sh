#!/bin/bash
# coredns-redirect: run the vendor CoreDNS on a non-colliding wildcard port and
# steer guest DNS (destined to each isolate's TAP gateway IP :53) to it with an
# nftables redirect. Upstream forwarding goes to the host resolver.
#
# Why not just bind the gateway IPs? The vendor's CoreDNS build (1.14.4) is
# stripped: `coredns -plugins` lists only
#   cache debug dnstap errors forward hosts log metadata prometheus rewrite template ukpdns
# There is NO `bind` plugin, and an IP in the server-block header is parsed as a
# zone, not a listen address (verified: it still tries `listen :53`). So CoreDNS
# can only bind the wildcard. On Talos that wildcard :53 collides with hostDNS
# on 127.0.0.53:53. We therefore move CoreDNS to an alternate wildcard port and
# DNAT the guest path to it; hostDNS keeps 127.0.0.53:53.
#
# Runs as the `coredns` container entrypoint (privileged, hostNetwork). Both the
# nftables table and the CoreDNS process are re-established on every (re)start,
# so the behaviour survives pod/container restarts.
set -euo pipefail

[ -f /etc/ukp.conf ] && . /etc/ukp.conf

UKP_RUNTIME="${UKP_RUNTIME:-/var/run/ukp}"
IDNS_ZONE="${IDNS_ZONE:-internal}"
IDNS_EMAIL="${IDNS_EMAIL:-ns@internal}"
IDNS_HOSTNAME="${IDNS_HOSTNAME:-ns}"
NET_SEGMENT="${NET_SEGMENT:-172.16.0.0/12}"
IDNS_ENDPOINT="${UKP_RUNTIME}/ukpd-idns.api"   # ukpd's iDNS UNIX socket
UPSTREAM="${UKP_DNS_UPSTREAM:-127.0.0.53:53}"  # Talos hostDNS by default
DNS_PORT="${UKP_DNS_PORT:-5300}"               # CoreDNS wildcard listen port
PIDFILE="${UKP_DNS_PIDFILE:-${UKP_RUNTIME}/coredns.pid}"

install_nft() {
  # A single `iifname "ukp*"` glob covers every current and future
  # ukp<netid>.vif<vifid> gateway, so it never needs updating as isolates come
  # and go. The input chain keeps the wildcard :${DNS_PORT} from being an open
  # resolver on the node's real NICs: only DNAT'd (redirected) guest traffic and
  # host loopback reach CoreDNS directly. Replace the table wholesale so repeat
  # runs (container restarts) are idempotent.
  nft delete table inet ukpdns 2>/dev/null || true
  nft -f - <<EOF
table inet ukpdns {
    chain prerouting {
        type nat hook prerouting priority dstnat; policy accept;
        iifname "ukp*" udp dport 53 redirect to :${DNS_PORT}
        iifname "ukp*" tcp dport 53 redirect to :${DNS_PORT}
    }
    chain input {
        type filter hook input priority filter; policy accept;
        ct status dnat udp dport ${DNS_PORT} accept
        ct status dnat tcp dport ${DNS_PORT} accept
        iifname "lo" udp dport ${DNS_PORT} accept
        iifname "lo" tcp dport ${DNS_PORT} accept
        udp dport ${DNS_PORT} drop
        tcp dport ${DNS_PORT} drop
    }
}
EOF
  echo "coredns-redirect: installed nftables table inet ukpdns (redirect ukp*:53 -> :${DNS_PORT})" >&2
}

install_nft

CONF_DIR="${UKP_RUNTIME}/coredns"
mkdir -p "$CONF_DIR"
CONF="${CONF_DIR}/ukpdns.conf"
{
  echo ".:${DNS_PORT} {"
  echo "    ukpdns ${IDNS_ZONE}. {"
  echo "        endpoint ${IDNS_ENDPOINT}"
  echo "        mbox ${IDNS_EMAIL}"
  echo "        ns ${IDNS_HOSTNAME}."
  echo "        whitelist ${NET_SEGMENT}"
  echo "    }"
  echo "    rewrite stop type AAAA A"
  echo "    forward . ${UPSTREAM}"
  echo "}"
} > "$CONF"

echo "coredns-redirect: rendered ${CONF}; upstream ${UPSTREAM}" >&2
exec /usr/bin/coredns -conf "$CONF" -pidfile "$PIDFILE"
