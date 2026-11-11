#!/bin/sh
# Builds qwdtt-ext with the upstream patch in patches/ applied.
#
#   ./build.sh          dist/qwdtt/<version>/{manifest.json,qwdtt-ext} for GOOS/GOARCH (default linux/amd64)
#   ./build.sh test     go vet and go test with the patch
#   ./build.sh prepare  only set up build/
#
# Upstream's client is a `package main` outside any fetchable module, so the pinned commit is checked out into
# build/upstream and build/overlay.json compiles its go_client files into this package as upstream_<name>.go,
# the patched ones from patches/_overlay. A plain `go build` without this fails to compile, on purpose.
set -eu

cd "$(dirname "$0")"
ROOT=$PWD
VERSION=0.1.0
ID=qwdtt
EXE=qwdtt-ext
REPO=https://github.com/SpaceNeuroX/proxy-turn-vk-android
COMMIT=a296c57eaba69bb9479a24f9157856490890e47d
# The client files compiled in; main.go is kept for the helpers it shares, its main renamed by the patch.
FILES="captcha_v2.go captcha_v2_slider.go creds.go creds_vkcalls.go dispatcher.go doh.go errhint.go group.go
listen.go main.go namegen.go obfs.go profiles.go protocol.go raw_diag.go session.go socks5.go socks_wg.go stats.go
tun_fd.go vk_account.go wrap.go"

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

	while read -r sum path; do
		[ -f "patches/_overlay/${path#go_client/}" ] || die "patches/_overlay/${path#go_client/} is missing"
		actual=$(sha256sum "$up/$path" | cut -d' ' -f1)
		[ "$actual" = "$sum" ] ||
			die "upstream $path is not the file the patch was made for (sha256 $actual, expected $sum); rebase patches/ onto the new upstream"
	done <patches/upstream.sha256

	json='{"Replace":{'
	sep=
	for f in $FILES; do
		src="$ROOT/$up/go_client/$f"
		[ -f "patches/_overlay/$f" ] && src="$ROOT/patches/_overlay/$f"
		[ -f "$src" ] || die "$src is missing"
		json="$json$sep\"$ROOT/upstream_$f\":\"$src\""
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
  "schemes": ["qwdtt"],
  "executable": "$EXE",
  "sha256": "$sum"
}
EOM
	echo "$out ($GOOS/$GOARCH, sha256 $sum)"
	;;
*) die "unknown command $1" ;;
esac
