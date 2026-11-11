#!/bin/sh
# Builds openflux-ext with the upstream patch in patches/ applied.
#
#   ./build.sh          dist/openflux/<version>/{manifest.json,openflux-ext} for GOOS/GOARCH (default linux/amd64)
#   ./build.sh test     go vet and go test with the patch
#   ./build.sh prepare  only set up build/
#
# Go refuses -overlay for files inside the module cache, so the pinned upstream module is copied, untouched, from
# the cache (after `go mod verify`) to build/upstream; build/go.mod is go.mod plus a replace pointing at that copy,
# and build/overlay.json swaps the patched files in. A plain `go build` without this fails to compile, on purpose:
# unpatched, the carriers would dial directly.
set -eu

cd "$(dirname "$0")"
ROOT=$PWD
VERSION=0.1.0
ID=openflux
EXE=openflux-ext
UPSTREAM=github.com/p1neappleXpress/OpenFlux

die() {
	echo "build.sh: $*" >&2
	exit 1
}

prepare() {
	case "$ROOT" in *'"'* | *'\'* | *' '*) die "the checkout path must not contain quotes, backslashes or spaces" ;; esac
	go mod download "$UPSTREAM"
	go mod verify >/dev/null || die "go mod verify failed: the module cache does not match go.sum"
	src=$(go list -m -f '{{.Dir}}' "$UPSTREAM")
	[ -n "$src" ] && [ -d "$src" ] || die "cannot locate $UPSTREAM in the module cache"

	rm -rf build
	mkdir -p build/upstream
	cp -R "$src/." build/upstream/
	chmod -R u+w build/upstream

	json='{"Replace":{'
	sep=
	while read -r sum path; do
		[ -f "patches/_overlay/$path" ] || die "patches/_overlay/$path is missing"
		actual=$(sha256sum "build/upstream/$path" | cut -d' ' -f1)
		[ "$actual" = "$sum" ] ||
			die "upstream $path is not the file the patch was made for (sha256 $actual, expected $sum); rebase patches/ onto the new upstream"
		json="$json$sep\"$ROOT/build/upstream/$path\":\"$ROOT/patches/_overlay/$path\""
		sep=,
	done <patches/upstream.sha256
	printf '%s}}\n' "$json" >build/overlay.json

	cp go.sum build/go.sum
	{
		sed "s#=> \.\./sdk#=> $ROOT/../sdk#" go.mod
		printf '\nreplace %s => %s/build/upstream\n' "$UPSTREAM" "$ROOT"
	} >build/go.mod
}

prepare
FLAGS="-modfile=$ROOT/build/go.mod -overlay=$ROOT/build/overlay.json"
case "${1:-dist}" in
prepare) ;;
test)
	# shellcheck disable=SC2086
	go vet $FLAGS ./...
	# shellcheck disable=SC2086
	go test $FLAGS ./...
	;;
dist)
	export GOOS="${GOOS:-linux}" GOARCH="${GOARCH:-amd64}" CGO_ENABLED=0
	out="dist/$ID/$VERSION"
	rm -rf "$out"
	mkdir -p "$out"
	# shellcheck disable=SC2086
	go build $FLAGS -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$out/$EXE" .
	sum=$(sha256sum "$out/$EXE" | cut -d' ' -f1)
	cat >"$out/manifest.json" <<EOF
{
  "id": "$ID",
  "version": "$VERSION",
  "protocol": [1],
  "schemes": ["openflux"],
  "executable": "$EXE",
  "sha256": "$sum"
}
EOF
	echo "$out ($GOOS/$GOARCH, sha256 $sum)"
	;;
*) die "unknown command $1" ;;
esac
