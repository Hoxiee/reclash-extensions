# qwdtt

ReClash extension for qWDTT links (`qwdtt://config?...`) from
[proxy-turn-vk-android](https://github.com/SpaceNeuroX/proxy-turn-vk-android). GPL-3.0-or-later, like upstream.

qWDTT carries WireGuard over DTLS through VK Calls TURN relays. The wrapper runs upstream's Go client
(`go_client/`, pinned to `a296c57e`, tag v1.4.4): anonymous VK Calls credentials, the TURN worker groups and the
dispatcher. The WireGuard config the server hands out drives a userspace WireGuard netstack behind the SDK's SOCKS5
front. Every upstream socket goes through the Core's egress (`patches/`).

## Build

Requires Go 1.26.6 or later, git, and network access to GitHub and the Go module proxy for the first build.

```sh
./build.sh          # dist/qwdtt/0.1.0/{manifest.json,qwdtt-ext}, linux/amd64 by default (GOOS/GOARCH override)
./build.sh test     # go vet + go test with the upstream files compiled in
```

Upstream's client is a `package main` outside any fetchable module. `build.sh` checks the pinned commit out into
`build/upstream` and compiles its `go_client` files into this package with `go build -overlay`, the patched ones
from `patches/_overlay`. A plain `go build` here fails with undefined upstream symbols.

Install for a local Core by copying `dist/qwdtt/<version>/` to `<core home>/extensions/qwdtt/<version>/`.

## Sockets

| Upstream socket | Upstream code | Here |
| --- | --- | --- |
| VK API and captcha HTTPS (tls-client, Chrome profile) | `creds.go`, `creds_vkcalls.go`, `captcha_v2*.go` | egress TCP by host name (`WithDialContext`, patch) |
| TURN over UDP / TCP | `session.go` `dialTURNConn` | egress UDP / TCP, replaced by `egress.go` |
| Name lookups (server, VK hosts in pion) | `net.DefaultResolver`, `setupGlobalResolver` | Yandex DoH (`77.88.8.8:443`) through the egress TCP |
| Local WireGuard UDP loopback | `main.go` `listenUDP`, WireGuard `NewDefaultBind` | in-memory pipe (`wgpipe.go`), no socket |
| SOCKS5 listener, TUN fd, raw mode | `socks5.go`, `tun_fd.go`, `raw_diag.go` | not used |
| Proxied flows | userspace WireGuard | netstack inside the tunnel; names resolve at the server's DNS |

## Behavior

- UDP ASSOCIATE is served: UDP rides the WireGuard tunnel like TCP.
- Worker count follows upstream: `workers` rounded down to groups of 9 (at least one, at most 108); group *n* uses
  the *n*-th call hash.
- The device id sent with `GETCONF` is derived from `/etc/machine-id` and the password, so every run on one machine
  is one device to the server. The server binds a password to the first devices that use it (one by default).
- Raw-IP links (`mode=raw`) and links without a password, peer or hashes are refused.
- A captcha the automatic solver cannot pass needs a WebView this process does not have: the node reports
  `degraded`, and when every worker has given up it stops with the reason.
- Upstream keeps its VK profile files (`vk_profile.json`, `captcha_browser_fp`) in `<dataDir>`.
