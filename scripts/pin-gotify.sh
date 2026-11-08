#!/usr/bin/env bash
#
# pin-gotify.sh pins the plugin's go.mod to one Gotify server release.
#
# A Go plugin only loads when it was built with the same Go version and the
# same version of every module it shares with the server binary; otherwise the
# server refuses it with "plugin was built with a different version of
# package ...". This script takes the server's go.mod and GO_VERSION from the
# tag v<version>, caps every shared module of our go.mod to the server's
# version with gomod-cap (shipped by gotify/plugin-api), tidies, and checks
# that nothing drifted away again.
#
# It writes two one-line files at the repository root, read by the Makefile to
# pick the gotify/build image and to name the .so:
#
#   .gotify-version   the Gotify server version, e.g. 3.1.1
#   .go-version       the Go version that server release is built with, e.g. 1.26.0
#
# Usage: scripts/pin-gotify.sh <gotify-version>
#
# Any failed step stops the script with a non-zero exit and leaves the files
# as the failed step left them; `git checkout go.mod go.sum` undoes it.

set -euo pipefail

trap 'echo "pin-gotify: failed at line $LINENO" >&2' ERR

# The tags are v3.1.1; accept both spellings and keep the bare number.
version="${1:-}"
version="${version#v}"

if [ -z "$version" ]; then
	echo "usage: $0 <gotify-version>   (e.g. 3.1.1)" >&2
	exit 2
fi

script_dir="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"

# shellcheck source=scripts/gotify-server.sh
. "$script_dir/gotify-server.sh"

cd "$repo_root"

# Never let go switch toolchains behind our back: the versions written here
# must be resolved by the Go that is installed, and a missing one must fail.
export GOTOOLCHAIN=local

# The Makefile passes -mod=readonly to the Docker build; tidy needs to write.
unset GOFLAGS

server_dir="$(fetch_gotify_server "$version")"
server_mod="$server_dir/go.mod"
go_version="$(tr -d '[:space:]' < "$server_dir/GO_VERSION")"

if ! printf '%s' "$go_version" | grep -Eq '^[0-9]+\.[0-9]+(\.[0-9]+)?$'; then
	echo "error: GO_VERSION of gotify/server v$version is not a version: '$go_version'" >&2
	exit 1
fi

server_go="$(awk '$1 == "go" { print $2 }' "$server_mod")"

echo "pin-gotify: gotify/server v$version, Go $go_version, go directive $server_go"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# Build gomod-cap once, before go.mod changes, so later passes do not depend on
# go.mod and go.sum being consistent between an edit and the next tidy.
go build -o "$tmp/gomod-cap" github.com/gotify/plugin-api/cmd/gomod-cap

# Start from the server's go directive. Tidy raises it again if a plugin-only
# module needs more, but it never lowers it, so going back to an older server
# would otherwise keep a directive its Go cannot build.
go mod edit -go="$server_go" -toolchain=none

# gomod-cap only touches modules already listed in our go.mod, and tidy can
# bring in new ones or raise a capped one through another requirement. Repeat
# cap and tidy until the check is clean; three passes is plenty in practice.
capped=0

for pass in 1 2 3; do
	"$tmp/gomod-cap" -from "$server_mod" -to go.mod 2> "$tmp/cap.log"

	# gomod-cap logs the path on one line and the new version on the next.
	awk -v pass="$pass" '
		/Found common import path/ { path = $NF }
		/Changing require version to/ { print "  pass " pass ": " path " -> " $NF }
	' "$tmp/cap.log"

	go mod tidy
	go mod edit -toolchain=none

	if "$tmp/gomod-cap" -check -from "$server_mod" -to go.mod 2> "$tmp/check.log"; then
		capped=1
		break
	fi
done

if [ "$capped" -ne 1 ]; then
	echo "error: go.mod still differs from gotify/server v$version after 3 passes:" >&2
	grep 'incompatible' "$tmp/check.log" >&2 || cat "$tmp/check.log" >&2
	exit 1
fi

# The Docker build runs that Go with GOTOOLCHAIN=local, so it must be able to
# build a module with our go directive.
our_go="$(awk '$1 == "go" { print $2 }' go.mod)"

if ! version_le "$our_go" "$go_version"; then
	echo "error: go.mod says go $our_go, but gotify/server v$version is built with Go $go_version" >&2
	exit 1
fi

go mod verify > /dev/null

echo "$version" > .gotify-version
echo "$go_version" > .go-version

shared="$(grep -c 'Found common import path' "$tmp/check.log" || true)"

echo "pin-gotify: pinned to Gotify $version (Go $go_version, go $our_go), $shared modules shared with the server"
