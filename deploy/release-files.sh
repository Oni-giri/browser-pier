#!/bin/sh
# Writes the files attached to a GitHub release into OUT: the compose
# file, .env example and Caddyfile, pinned to VERSION, and the release
# notes from CHANGELOG.md. CI runs it for each v* tag.
#
#   deploy/release-files.sh 0.1.0 dist
set -eu
version=$1
out=$2
cd "$(dirname "$0")/.."

case $version in
v*) echo "give the version without the v: ${version#v}" >&2; exit 1 ;;
esac
# X.Y.Z only, as the orchestrator picks its browser image by that form.
echo "$version" | grep -Eqx '[0-9]+\.[0-9]+\.[0-9]+' || { echo "not a release version (X.Y.Z): $version" >&2; exit 1; }
mkdir -p "$out"

sed "s/BROWSERPIER_VERSION:-latest/BROWSERPIER_VERSION:-$version/" deploy/docker-compose.yml >"$out/docker-compose.yml"
sed "s/^#BROWSERPIER_VERSION=latest/#BROWSERPIER_VERSION=$version/" deploy/.env.example >"$out/env.example"
cp deploy/Caddyfile "$out/Caddyfile"
grep -q "BROWSERPIER_VERSION:-$version}" "$out/docker-compose.yml" || { echo "compose file not pinned" >&2; exit 1; }
grep -q "^#BROWSERPIER_VERSION=$version\$" "$out/env.example" || { echo ".env example not pinned" >&2; exit 1; }

# This version's section of the changelog, without its heading.
awk -v v="$version" '
	/^## / { on = ($2 == v) ; next }
	on { print }
' CHANGELOG.md >"$out/notes.md"
grep -q . "$out/notes.md" || { echo "CHANGELOG.md has no section for $version" >&2; exit 1; }
