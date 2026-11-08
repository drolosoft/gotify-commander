#!/usr/bin/env bash
#
# check-plugin-abi.sh checks that a built plugin can load on one Gotify release.
#
# Gotify refuses a plugin whose Go version, or the version of any module it
# shares with the server, differs from the server binary's. This script reads
# the build info of the .so with `go version -m`, compares it with the
# server's GO_VERSION and go.mod for that release (downloaded once into the
# cache of scripts/gotify-server.sh), and prints every difference.
#
# Usage: scripts/check-plugin-abi.sh <file.so> <gotify-version>
#
# Exit 0 with one OK line when everything matches, 1 with the differences
# otherwise, 2 on bad usage.

set -euo pipefail

if [ "$#" -ne 2 ]; then
	echo "usage: $0 <file.so> <gotify-version>" >&2
	exit 2
fi

plugin="$1"
version="${2#v}"

if [ ! -f "$plugin" ]; then
	echo "error: $plugin does not exist" >&2
	exit 2
fi

script_dir="$(cd "$(dirname "$0")" && pwd)"

# shellcheck source=scripts/gotify-server.sh
. "$script_dir/gotify-server.sh"

server_dir="$(fetch_gotify_server "$version")"
server_go="go$(tr -d '[:space:]' < "$server_dir/GO_VERSION")"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# go version -m reads the build info of any Go binary, a Linux .so on a Mac
# included; it fails on a file that was not built by Go.
if ! go version -m "$plugin" > "$tmp/buildinfo"; then
	echo "error: cannot read Go build info from $plugin" >&2
	exit 1
fi

# The first line is "<file>: go1.26.0".
plugin_go="$(head -n 1 "$tmp/buildinfo" | awk '{ print $NF }')"
plugin_arch="$(awk '$1 == "build" && $2 ~ /^GOARCH=/ { sub(/^GOARCH=/, "", $2); print $2 }' "$tmp/buildinfo")"
plugin_mode="$(awk '$1 == "build" && $2 ~ /^-buildmode=/ { sub(/^-buildmode=/, "", $2); print $2 }' "$tmp/buildinfo")"

# A dep line is "dep <path> <version> <sum>". A replaced module is followed by
# "=> <path> <version> <sum>"; the replacement is what got linked, so its
# version wins.
awk '
	$1 == "dep" { path = $2; version[path] = $3; order[++n] = path }
	$1 == "=>" { version[path] = $3 }
	END { for (i = 1; i <= n; i++) print order[i], version[order[i]] }
' "$tmp/buildinfo" > "$tmp/plugin.deps"

gomod_requires "$server_dir/go.mod" > "$tmp/server.deps"

failed=0

if [ "$plugin_mode" != "plugin" ]; then
	echo "error: $plugin was built with -buildmode=${plugin_mode:-exe}, not plugin" >&2
	failed=1
fi

if [ "$plugin_go" != "$server_go" ]; then
	echo "error: Go version differs: plugin $plugin_go, Gotify $version $server_go" >&2
	failed=1
fi

# Every module linked into both binaries must be at the same version.
awk '
	NR == FNR { server[$1] = $2; next }
	($1 in server) {
		shared++
		if (server[$1] != $2) {
			if (!header) {
				printf "%-45s %-14s %s\n", "module", "plugin", "server"
				header = 1
			}
			printf "%-45s %-14s %s\n", $1, $2, server[$1]
		}
	}
	END { print shared + 0 > "/dev/stderr" }
' "$tmp/server.deps" "$tmp/plugin.deps" > "$tmp/diff" 2> "$tmp/shared"

if [ -s "$tmp/diff" ]; then
	echo "error: module versions differ from Gotify $version:" >&2
	cat "$tmp/diff" >&2
	failed=1
fi

if [ "$failed" -ne 0 ]; then
	echo "FAIL $plugin cannot load on Gotify $version" >&2
	exit 1
fi

echo "OK $plugin: $plugin_go linux/$plugin_arch, $(cat "$tmp/shared") shared modules match Gotify $version"
