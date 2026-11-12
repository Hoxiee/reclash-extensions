#!/bin/sh
# Builds olcrtc-ext with the upstream patch in patches/ applied.
#
#   ./build.sh          dist/olcrtc/<version>/{manifest.json,olcrtc-ext} for GOOS/GOARCH (default linux/amd64)
#   ./build.sh test     go vet and go test with the patch
#   ./build.sh prepare  only set up build/
#
# go.mod replaces the upstream module with the pinned checkout in build/upstream, and build/overlay.json swaps in
# the files of patches/_overlay (patched originals and new files) at their upstream paths. Without the overlay the
# wrapper does not compile, on purpose: upstream alone would dial directly.
set -eu

cd "$(dirname "$0")"
ROOT=$PWD
VERSION=0.1.0
ID=olcrtc
EXE=olcrtc-ext
REPO=https://github.com/openlibrecommunity/olcrtc
COMMIT=f3ad8fb7c0d0fa981423f83269526fa94077c23d
TREE=41f9fe33ce20d3a3179661e5651e4948cebdc593

die() {
	echo "build.sh: $*" >&2
	exit 1
}

prepare() {
	case "$ROOT" in *'"'* | *'\'* | *' '*) die "the checkout path must not contain quotes, backslashes or spaces" ;; esac
	up=build/upstream
	if [ "$(git -C "$up" rev-parse HEAD 2>/dev/null || true)" != "$COMMIT" ]; then
		rm -rf "$up"
		mkdir -p "$up"
		(
			cd "$up"
			git init -q
			git fetch -q --depth 1 "$REPO" "$COMMIT"
			git -c advice.detachedHead=false checkout -q FETCH_HEAD
		) || die "cannot fetch $REPO at $COMMIT"
	fi
	[ "$(git -C "$up" rev-parse HEAD)" = "$COMMIT" ] || die "build/upstream is not at $COMMIT"
	[ "$(git -C "$up" rev-parse 'HEAD^{tree}')" = "$TREE" ] || die "build/upstream does not have tree $TREE"
	[ -z "$(git -C "$up" status --porcelain)" ] || die "build/upstream has local changes; delete it to refetch"

	while read -r sum path; do
		[ -f "patches/_overlay/$path" ] || die "patches/_overlay/$path is missing"
		actual=$(sha256sum "$up/$path" | cut -d' ' -f1)
		[ "$actual" = "$sum" ] ||
			die "upstream $path is not the file the patch was made for (sha256 $actual, expected $sum); rebase patches/ onto the new upstream"
	done <patches/upstream.sha256

	json='{"Replace":{'
	sep=
	for f in $(cd patches/_overlay && find . -type f -name '*.go' | sed 's|^\./||' | sort); do
		json="$json$sep\"$ROOT/$up/$f\":\"$ROOT/patches/_overlay/$f\""
		sep=,
	done
	printf '%s}}\n' "$json" >build/overlay.json
}

prepare
FLAGS="-overlay=$ROOT/build/overlay.json"
case "${1:-dist}" in
prepare) ;;
test)
	go vet $FLAGS .
	go test $FLAGS .
	;;
dist)
	export GOOS="${GOOS:-linux}" GOARCH="${GOARCH:-amd64}" CGO_ENABLED=0
	out="dist/$ID/$VERSION"
	rm -rf "$out"
	mkdir -p "$out"
	go build $FLAGS -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$out/$EXE" .
	sum=$(sha256sum "$out/$EXE" | cut -d' ' -f1)
	cat >"$out/manifest.json" <<EOM
{
  "id": "$ID",
  "version": "$VERSION",
  "protocol": [1],
  "schemes": ["olcrtc"],
  "executable": "$EXE",
  "sha256": "$sum"
}
EOM
	echo "$out ($GOOS/$GOARCH, sha256 $sum)"
	;;
*) die "unknown command $1" ;;
esac
