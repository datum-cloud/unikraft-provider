#!/bin/sh
#
# Downloads a ukpd plugin image (e.g. the exec sandbox) and registers it for
# import, so nodes never pull it and need no registry credential.
#
# Writes:
#
#   <dest>/rom           the plugin ROM (plugins ship as a single ROM layer)
#   <dest>/config.json   the image config
#   <index>              ukpd --images-import-path entries, one per plugin
#
# The import entry is keyed by <url>, the exact reference kraftlet asks for, so
# ukpd finds the plugin already present instead of asking the agent to pull it.
#
# Runs while the image is built. The credential arrives as a build secret, so
# it never reaches an image layer or a node.

set -eu

REGISTRY=${UKP_PLUGIN_REGISTRY:?}
REPO=${UKP_PLUGIN_REPO:?}
REF=${UKP_PLUGIN_REF:?}
URL=${UKP_PLUGIN_URL:?}
DEST=${UKP_PLUGIN_DEST:?}
INDEX=${UKP_PLUGIN_INDEX:?}
AUTH_FILE=${UKP_PLUGIN_AUTH_FILE:-/run/secrets/unikraft-registry-auth}

die() { printf 'fetch-plugin: %s\n' "$*" >&2; exit 1; }

[ -s "$AUTH_FILE" ] || die "no registry credential at $AUTH_FILE"

CRED=$(tr -d '\r\n' < "$AUTH_FILE" | base64 -d) || die "credential is not valid base64"
case "$CRED" in *:*) ;; *) die "credential does not decode to user:password" ;; esac

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

TOKEN=$(curl -fsS --max-time 30 -u "$CRED" \
	"https://$REGISTRY/service/token?service=harbor-registry&scope=repository:$REPO:pull" |
	jq -r '.token // empty') || die "token request failed"
[ -n "$TOKEN" ] || die "registry returned no token"

api() { curl -fsS --max-time 300 -L -H "Authorization: Bearer $TOKEN" "$@"; }
ACCEPT='application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json'

verify() {
	got="sha256:$(sha256sum "$1" | cut -d' ' -f1)"
	[ "$got" = "$2" ] || die "digest mismatch for $1: expected $2, got $got"
}

api -H "Accept: $ACCEPT" "https://$REGISTRY/v2/$REPO/manifests/$REF" > "$WORK/top.json"
case "$REF" in sha256:*) verify "$WORK/top.json" "$REF" ;; esac
printf 'fetch-plugin: %s/%s:%s is sha256:%s\n' "$REGISTRY" "$REPO" "$REF" \
	"$(sha256sum "$WORK/top.json" | cut -d' ' -f1)" >&2

# An index lists one manifest per platform; Unikraft images name theirs
# kraftcloud/x86_64.
if jq -e '.manifests' "$WORK/top.json" >/dev/null; then
	jq -c '.manifests[] | {digest, platform}' "$WORK/top.json" >&2
	digest=$(jq -r '[.manifests[] | select(.platform.architecture == "x86_64" or .platform.architecture == "amd64")]
		| (map(select(.platform.os == "kraftcloud")) + .)[0].digest // empty' "$WORK/top.json")
	[ -n "$digest" ] || die "index has no x86_64 manifest"
	api -H "Accept: $ACCEPT" "https://$REGISTRY/v2/$REPO/manifests/$digest" > "$WORK/manifest.json"
	verify "$WORK/manifest.json" "$digest"
else
	cp "$WORK/top.json" "$WORK/manifest.json"
fi

jq -c '.layers[] | {mediaType, digest, size, annotations}' "$WORK/manifest.json" >&2

config_digest=$(jq -r '.config.digest // empty' "$WORK/manifest.json")
[ -n "$config_digest" ] || die "manifest carries no config descriptor"
api -o "$WORK/config.json" "https://$REGISTRY/v2/$REPO/blobs/$config_digest"
verify "$WORK/config.json" "$config_digest"

mkdir -p "$DEST"
install -m 0644 "$WORK/config.json" "$DEST/config.json"

jq -c . "$WORK/config.json" >&2

# A ROM layer is the raw filesystem blob, not a tar.
rom=$(jq -r '[.layers[] | select(.mediaType == "application/vnd.unikraft.rom.v1")][0].digest // empty' \
	"$WORK/manifest.json")
[ -n "$rom" ] || die "manifest has no application/vnd.unikraft.rom.v1 layer"
api -o "$WORK/rom" "https://$REGISTRY/v2/$REPO/blobs/$rom"
verify "$WORK/rom" "$rom"
printf 'fetch-plugin: rom magic at 1024: %s\n' "$(od -An -tx1 -j1024 -N4 "$WORK/rom" | tr -d ' ')" >&2
install -m 0644 "$WORK/rom" "$DEST/rom"

entry=$(jq -n --arg url "$URL" --arg d "$DEST" '{url: $url, config: "\($d)/config.json", rom: "\($d)/rom"}')

[ -s "$INDEX" ] || echo '[]' > "$INDEX"
jq --argjson e "$entry" '. + [$e]' "$INDEX" > "$WORK/index.json"
install -m 0644 "$WORK/index.json" "$INDEX"

printf 'fetch-plugin: registered %s\n' "$URL" >&2
jq . "$INDEX" >&2
