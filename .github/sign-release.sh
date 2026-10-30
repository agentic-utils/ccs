#!/bin/sh
# Signs checksums.txt for ccs's self-update (signedSums in update.go): Ed25519
# over "ccs <tag>\n" and then the file, so a signature can't move to another tag.
# Run by GoReleaser's signs step; the key is the release environment's secret.
set -eu
tag=$1 sums=$2 sig=$3
if [ -z "${RELEASE_SIGNING_KEY:-}" ]; then
	echo "RELEASE_SIGNING_KEY is not set" >&2
	exit 1
fi
umask 077
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
printf '%s\n' "$RELEASE_SIGNING_KEY" >"$dir/key.pem"
printf 'ccs %s\n' "$tag" | cat - "$sums" >"$dir/msg"
openssl pkeyutl -sign -rawin -inkey "$dir/key.pem" -in "$dir/msg" -out "$sig"
