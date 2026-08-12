#!/bin/sh
#
# Install the ukpd platform kernel from a unikraft base image, laying down the
# two files ukpd's --images-kernel-path expects:
#
#   <dest>/kernel        the kernel binary
#   <dest>/config.json   the image config, reduced to its kernel-relevant keys
#
# Runs at image build time with the registry credential mounted as a BuildKit
# secret, so nothing is persisted into a layer and no vendor credential reaches
# the node. Derived from the vendor's ukp-fetch-platform-kernel, with kraft
# dropped: the kernel is a single uncompressed tar layer tagged by the
# org.unikraft.kernel.image annotation, so curl+jq+tar reach it directly.
#
# The manifest is addressed by digest, never by tag, so a rebuild always gets
# the kernel the pins were validated against.

set -eu

REGISTRY=${UKP_KERNEL_REGISTRY:?}
REPO=${UKP_KERNEL_REPO:?}
DIGEST=${UKP_KERNEL_MANIFEST_DIGEST:?}
DEST=${UKP_KERNEL_DEST:?}
AUTH_FILE=${UKP_KERNEL_AUTH_FILE:-/run/secrets/unikraft-registry-auth}

die() { printf 'fetch-kernel: %s\n' "$*" >&2; exit 1; }

[ -s "$AUTH_FILE" ] || die "no registry credential at $AUTH_FILE"

# The credential is stored base64(user:password), matching the vendor's
# UKC_TOKEN, so the same value works from a workstation and from CI.
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

# Strip config and rootfs: a base image carries a Cmd and a rootfs diff_id,
# neither of which describes a bare kernel, and leaving Cmd in risks ukpd
# adopting it as the kernel commandline for every kernel-less instance.
jq '{
	architecture,
	created: (.created // "0001-01-01T00:00:00Z"),
	os,
	"os.features": ."os.features"
}' "$WORK/config.raw" > "$WORK/config.json"

jq -e '."os.features" | length > 0' "$WORK/config.json" >/dev/null ||
	die "config carries no os.features -- ukpd needs them for the feature bitmap"

# --- kernel ----------------------------------------------------------------
# Select by annotation, not position: the layer set also carries a debug image.
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
