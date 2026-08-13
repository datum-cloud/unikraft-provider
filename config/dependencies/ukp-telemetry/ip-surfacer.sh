#!/bin/sh
# Filesystem-only IP surfacer. The OTel filelog receiver can only read a log's
# path, and ukpd's vm.log path carries just the uuid — so this puts the guest
# IP on the path too, letting the stock k8sattributes processor enrich each
# record by pod IP (the provider Pod's podIP == the guest IP). It reads the IP
# from vmm.json's boot args and symlinks; no Kubernetes client, no deps beyond
# busybox. All Datum enrichment happens in the pipeline, not here.
set -eu
PLATFORM="${UKP_PLATFORM_DIR:-/var/lib/ukp/data/platform}"
TREE="${UKP_LOG_TREE:-/var/log/ukp-logs}"
POLL="${POLL_SECONDS:-5}"

while true; do
  for d in "$PLATFORM"/*/; do
    [ -f "$d/vm.log" ] || continue
    [ -f "$d/vmm.json" ] || continue
    uuid=$(basename "$d")
    # netdev.ip="<ip>/<prefix>:..." in the guest boot args. Skip the non-digit
    # prefix (the escaped quote) and capture the first dotted-quad.
    ip=$(sed -n 's/.*netdev\.ip=[^0-9]*\([0-9][0-9.]*\).*/\1/p' "$d/vmm.json" | head -1)
    [ -n "$ip" ] || continue
    link_dir="$TREE/ip=$ip/uuid=$uuid"
    mkdir -p "$link_dir"
    [ -L "$link_dir/vm.log" ] || ln -sfn "$d/vm.log" "$link_dir/vm.log"
  done
  # Drop symlinks whose target vm.log is gone (instance removed by ukpd).
  find "$TREE" -type l 2>/dev/null | while read -r l; do
    [ -e "$l" ] || rm -f "$l"
  done
  sleep "$POLL"
done
