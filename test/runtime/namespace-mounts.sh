#!/usr/bin/env bash
# Exercise the old and corrected CNI mount layouts in a disposable Linux
# container, without host mounts or network access:
# docker run --rm --privileged --network none -i \
#   debian:trixie-slim unshare --mount --propagation private \
#   bash -s -- isolated < test/runtime/namespace-mounts.sh
set -euo pipefail

# Keep all test mounts in a private mount namespace, even when run outside
# Docker. Only an empty temporary directory is visible to the parent.
if [[ ${1:-} != isolated ]]; then
  exec unshare --mount --propagation private bash "$0" isolated
fi

lab=$(mktemp -d /tmp/ukp-netns-test.XXXXXX)
# Detach the whole synthetic tree; exiting this private mount namespace
# releases its nested mounts without shared-peer cleanup ordering races.
trap 'mount --make-rprivate "$lab"; umount -l "$lab"; rmdir "$lab"' EXIT
mount -t tmpfs tmpfs "$lab"

for propagation in None HostToContainer; do
  root="$lab/$propagation"
  mkdir -p "$root/host/run/netns" "$root/pod/host/run" "$root/pod/run/netns" "$root/multus/netns"
  mount --bind "$root/host/run" "$root/host/run"
  mount --make-rshared "$root/host/run"
  mount --bind "$root/host/run/netns" "$root/host/run/netns"
  mount --make-rshared "$root/host/run/netns"

  # The namespace already exists when the CNI service starts or rolls.
  touch "$root/host/run/netns/instance"
  mount --bind /proc/self/ns/net "$root/host/run/netns/instance"
  mount --rbind "$root/host/run" "$root/pod/host/run"
  if [[ $propagation == None ]]; then
    mount --make-rprivate "$root/pod/host/run"
  else
    mount --make-rslave "$root/pod/host/run"
  fi
  mount -o remount,bind,ro "$root/pod/host/run"
  mount --rbind "$root/host/run/netns" "$root/pod/run/netns"
  mount --make-rshared "$root/pod/run/netns"
  mount --rbind "$root/host/run/netns" "$root/multus/netns"
  mount --make-rslave "$root/multus/netns"

  umount "$root/pod/run/netns/instance"
  if [[ $propagation == None ]]; then
    [[ $(stat -f -c %T "$root/pod/host/run/netns/instance") == nsfs ]]
    [[ $(stat -f -c %T "$root/pod/run/netns/instance") == tmpfs ]]
    if rm "$root/pod/run/netns/instance" 2>"$lab/remove-error"; then
      echo 'FAIL: old configuration did not reproduce the cleanup failure' >&2
      exit 1
    fi
    grep -q 'Device or resource busy' "$lab/remove-error"
    echo 'PASS: old configuration reproduces the stuck namespace'
  else
    for view in pod/host/run/netns pod/run/netns multus/netns; do
      [[ $(stat -f -c %T "$root/$view/instance") == tmpfs ]]
    done
    rm "$root/pod/run/netns/instance"
    # Recreate the same instance, then delete it again. Both the alternate
    # host view and Multus must see the new mount and its removal.
    touch "$root/pod/run/netns/instance"
    mount --bind /proc/self/ns/net "$root/pod/run/netns/instance"
    for view in host/run/netns pod/host/run/netns multus/netns; do
      [[ $(stat -f -c %T "$root/$view/instance") == nsfs ]]
    done
    umount "$root/pod/run/netns/instance"
    rm "$root/pod/run/netns/instance"
    echo 'PASS: corrected configuration supports namespace deletion and recreation'
  fi
done
