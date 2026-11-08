# gotify-server.sh is sourced by pin-gotify.sh and check-plugin-abi.sh. It
# downloads go.mod and GO_VERSION of one Gotify server tag into a cache, because
# both files are fixed once a tag is released and the build matrix asks for the
# same few versions over and over.

# GOTIFY_SERVER_CACHE holds one directory per server version, each with the
# server's go.mod and GO_VERSION. Override it to share the cache in CI.
GOTIFY_SERVER_CACHE="${GOTIFY_SERVER_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/gotify-commander/server}"

# GOTIFY_SERVER_RAW is where the tagged files are read from.
GOTIFY_SERVER_RAW="https://raw.githubusercontent.com/gotify/server"

# fetch_gotify_server <version> makes sure go.mod and GO_VERSION of server tag
# v<version> are in the cache and prints the directory that holds them. Each
# file is downloaded to a temporary name first, so a failed download never
# leaves a half file in the cache for the next run to trust.
fetch_gotify_server() {
	local version="$1"
	local dir="$GOTIFY_SERVER_CACHE/$version"
	local file

	mkdir -p "$dir"

	for file in go.mod GO_VERSION; do
		if [ -s "$dir/$file" ]; then
			continue
		fi

		if ! curl -fsSL "$GOTIFY_SERVER_RAW/v$version/$file" -o "$dir/$file.part"; then
			rm -f "$dir/$file.part"
			echo "error: cannot download $file of gotify/server v$version" >&2
			return 1
		fi

		mv "$dir/$file.part" "$dir/$file"
	done

	# A wrong tag can still answer with something; make sure it is the server.
	if ! grep -q '^module github.com/gotify/server' "$dir/go.mod"; then
		echo "error: $dir/go.mod is not the gotify/server go.mod" >&2
		return 1
	fi

	echo "$dir"
}

# gomod_requires <go.mod> prints "path version" for every require line of a
# go.mod, both in require blocks and single-line requires. Plain awk keeps the
# scripts free of jq, which is not on every machine that builds the plugin.
gomod_requires() {
	awk '
		/^require[ \t]*\(/ { inblock = 1; next }
		inblock && /^\)/ { inblock = 0; next }
		inblock && NF >= 2 && $1 !~ /^\/\// { print $1, $2; next }
		/^require[ \t]+[^( \t]/ { print $2, $3 }
	' "$1"
}

# version_le <a> <b> succeeds when dotted version a is lower than or equal to
# b, comparing each part as a number (1.9 < 1.10). sort -V is not portable to
# the BSD sort of macOS, hence the awk.
version_le() {
	awk -v a="$1" -v b="$2" 'BEGIN {
		na = split(a, x, ".")
		nb = split(b, y, ".")
		n = na > nb ? na : nb
		for (i = 1; i <= n; i++) {
			if (x[i] + 0 < y[i] + 0) exit 0
			if (x[i] + 0 > y[i] + 0) exit 1
		}
		exit 0
	}'
}
