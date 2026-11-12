#!/bin/sh
# Builds naive-ext.
#
#   ./build.sh          dist/naive/<version>/{manifest.json,naive-ext} for GOOS/GOARCH (default linux/amd64)
#   ./build.sh test     go vet and go test
#
# Nothing is patched: the only dialing code is client.go's call to the SDK egress. The libraries (tls-client,
# fhttp, utls) are used for their Chrome ClientHello and HTTP/2 framing and are never handed a dialer of their own.
set -eu

cd "$(dirname "$0")"
VERSION=0.1.0
ID=naive
EXE=naive-ext

case "${1:-dist}" in
test)
	go vet ./...
	go test ./...
	;;
dist)
	export GOOS="${GOOS:-linux}" GOARCH="${GOARCH:-amd64}" CGO_ENABLED=0
	out="dist/$ID/$VERSION"
	rm -rf "$out"
	mkdir -p "$out"
	go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$out/$EXE" .
	sum=$(sha256sum "$out/$EXE" | cut -d' ' -f1)
	cat >"$out/manifest.json" <<EOT
{
  "id": "$ID",
  "version": "$VERSION",
  "protocol": [1],
  "schemes": ["naive+https"],
  "executable": "$EXE",
  "sha256": "$sum"
}
EOT
	echo "$out ($GOOS/$GOARCH, sha256 $sum)"
	;;
*)
	echo "build.sh: unknown command $1" >&2
	exit 1
	;;
esac
