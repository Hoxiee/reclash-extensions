# openflux

ReClash extension for [OpenFlux](https://github.com/p1neappleXpress/OpenFlux) links (`openflux://v1/...`).
GPL-3.0-or-later, like upstream.

The wrapper decodes the link with upstream's `share` package and runs upstream's client Session (the carriers the
link lists, the gVisor `tunnel.TCPTunnel`) behind the SDK's SOCKS5 front. Every carrier socket goes through the
Core's egress: upstream's `netbind` is patched to take a dial function (`patches/`).

## Build

Requires Go 1.26.6 or later and network access to the Go module proxy for the first build.

```sh
./build.sh          # dist/openflux/0.1.0/{manifest.json,openflux-ext}, linux/amd64 by default (GOOS/GOARCH override)
./build.sh test     # go vet + go test with the patch
```

A plain `go build`, `go vet` or `go test` in this directory fails with `undefined: netbind.SetDial`: the
patch is applied only by `build.sh`, and an unpatched build would dial directly.

Install for a local Core by copying `dist/openflux/<version>/` to `<core home>/extensions/openflux/<version>/`.

## Behavior

- Supported carriers: direct, yandex, vyandex, boards, mailru, cupsonline, avito. Telemost (WebRTC sockets that no
  dialer covers), stream-mode (PHP exit) links and links without a secret are refused by `describe` and `run`.
- Names the front receives are resolved through the tunnel at `1.1.1.1:53`, never by the host.
- UDP ASSOCIATE is served: UDP rides the tunnel to the exit like TCP.
- Cookies the carriers collect are kept in `<dataDir>/cookies.json`.
- Checks a service asks for (captcha) cannot be shown; the node reports `degraded` with the carrier's name.
