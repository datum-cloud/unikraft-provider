#!/bin/sh
#
# Downloads the kernel that ukpd gives to images packaged without one.
#
# Writes the two files that ukpd expects at --images-kernel-path:
#
#   <dest>/kernel        the kernel
#   <dest>/config.json   the kernel's feature list
#
# Runs while the image is built. The credential arrives as a build secret, so
# it never reaches an image layer or a node.
#
# Always names the image by digest. Tags move; a rebuild must get the same
# kernel.
#
# Based on the vendor's ukp-fetch-platform-kernel, without their build tool.
# The kernel is one plain tar layer, so curl, jq, and tar are enough.

set -eu

REGISTRY=${UKP_KERNEL_REGISTRY:?}
REPO=${UKP_KERNEL_REPO:?}
DIGEST=${UKP_KERNEL_MANIFEST_DIGEST:?}
DEST=${UKP_KERNEL_DEST:?}
AUTH_FILE=${UKP_KERNEL_AUTH_FILE:-/run/secrets/unikraft-registry-auth}

die() { printf 'fetch-kernel: %s\n' "$*" >&2; exit 1; }

[ -s "$AUTH_FILE" ] || die "no registry credential at $AUTH_FILE"

# The credential is base64 of "user:password". The same value works from a
# workstation and from CI.
CRED=$(tr -d '\r\n' < "$AUTH_FILE" | base64 -d) || die "credential is not valid base64"
case "$CRED" in *:*) ;; *) die "credential does not decode to user:password" ;; esac

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

TOKEN=$(curl -fsS --max-time 30 -u "$CRED" \
	"https://$REGISTRY/service/token?service=harbor-registry&scope=repository:$REPO:pull" |
	jq -r '.token // empty') || die "token request failed"
[ -n "$TOKEN" ] || die "registry returned no token"

api() { curl -fsS --max-time 300 -L -H "Authorization: Bearer $TOKEN" "$@"; }
ACCEPT='application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json'

verify() {
	got="sha256:$(sha256sum "$1" | cut -d' ' -f1)"
	[ "$got" = "$2" ] || die "digest mismatch for $1: expected $2, got $got"
}

api -H "Accept: $ACCEPT" "https://$REGISTRY/v2/$REPO/manifests/$DIGEST" > "$WORK/manifest.json"
verify "$WORK/manifest.json" "$DIGEST"

# --- image config ----------------------------------------------------------
config_digest=$(jq -r '.config.digest // empty' "$WORK/manifest.json")
[ -n "$config_digest" ] || die "manifest carries no config descriptor"
api -o "$WORK/config.raw" "https://$REGISTRY/v2/$REPO/blobs/$config_digest"
verify "$WORK/config.raw" "$config_digest"

# Drop config and rootfs. A base image names a start command and a filesystem,
# neither of which applies to a bare kernel. Worse, ukpd could mistake the
# start command for the kernel command line.
jq '{
	architecture,
	created: (.created // "0001-01-01T00:00:00Z"),
	os,
	"os.features": ."os.features"
}' "$WORK/config.raw" > "$WORK/config.json"

jq -e '."os.features" | length > 0' "$WORK/config.json" >/dev/null ||
	die "config carries no os.features -- ukpd needs them for the feature bitmap"

# --- kernel ----------------------------------------------------------------
# Find the kernel by its label. Some images also carry a debug build.
layer=$(jq -r '[.layers[] | select(.annotations."org.unikraft.kernel.image")][0] // empty' \
	"$WORK/manifest.json")
[ -n "$layer" ] || die "manifest has no layer annotated org.unikraft.kernel.image"

layer_digest=$(printf '%s' "$layer" | jq -r '.digest')
kernel_in_pkg=$(printf '%s' "$layer" | jq -r '.annotations."org.unikraft.kernel.image"' | sed 's,^/,,')

api -o "$WORK/layer.tar" "https://$REGISTRY/v2/$REPO/blobs/$layer_digest"
verify "$WORK/layer.tar" "$layer_digest"

tar -xf "$WORK/layer.tar" -C "$WORK" "$kernel_in_pkg" ||
	die "layer has no $kernel_in_pkg"
[ -s "$WORK/$kernel_in_pkg" ] || die "extracted kernel is empty"

mkdir -p "$DEST"
install -m 0644 "$WORK/$kernel_in_pkg" "$DEST/kernel"
install -m 0644 "$WORK/config.json" "$DEST/config.json"

printf 'fetch-kernel: installed %s (%s bytes)\n' "$DEST/kernel" "$(wc -c < "$DEST/kernel")" >&2
jq -r '[."os.features"[] | select(startswith("KERNEL_VCS_COMMIT="))][0] // "KERNEL_VCS_COMMIT=?"' \
	"$DEST/config.json" >&2
