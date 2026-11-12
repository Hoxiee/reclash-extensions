# olcrtc

ReClash extension for olcRTC links (`olcrtc://<provider>?<transport>[<params>]@<room>#<key>[$<name>]`) from
[olcrtc](https://github.com/openlibrecommunity/olcrtc). WTFPL, like upstream (`LICENSE` is upstream's).

olcRTC carries an encrypted smux session through a video-call room (Yandex Telemost, WB Stream or a Jitsi server)
to an olcrtc server in the same room. The wrapper runs upstream's client library (`pkg/olcrtc/client`, pinned to
`f3ad8fb7`) without its SOCKS listener: each CONNECT the SDK's SOCKS5 front accepts becomes one stream of that
session. Every upstream socket goes through the Core's egress (`patches/`).

Maturity: upstream is archived; the pinned commit is its last state and no fixes are expected.

## Build

Requires Go 1.26.6 or later, git, and network access to GitHub and the Go module proxy for the first build.

```sh
./build.sh          # dist/olcrtc/0.1.0/{manifest.json,olcrtc-ext}, linux/amd64 by default (GOOS/GOARCH override)
./build.sh test     # go vet + go test with the patched upstream compiled in
```

`build.sh` checks the pinned commit out into `build/upstream`, which `go.mod` replaces the module with, and compiles
the patched files from `patches/_overlay` with `go build -overlay`.

Install for a local Core by copying `dist/olcrtc/<version>/` to `<core home>/extensions/olcrtc/<version>/`.

## Links

Upstream's URI convention (`docs/uri.md`). Providers: `telemost`, `wbstream`, `jitsi` (the room is `host/room` or
`https://host/room`). Transports: `datachannel`, `vp8channel` (`vp8-fps`, `vp8-batch`), `seichannel` (`fps`,
`batch`, `frag`, `ack-ms`), `videochannel` (`video-w`, `video-h`, `video-fps`, `video-codec` `qrcode`|`tile`,
`video-qr-size`, `video-qr-recovery`, `video-tile-module`, `video-tile-rs`). Missing values take upstream's mobile
defaults; unknown keys are ignored. The key is 32 bytes in hex. The key and the room are redacted from the log.

## Sockets

| Upstream socket | Upstream code | Here |
| --- | --- | --- |
| Provider API and signalling (HTTPS, WebSocket) | `protect.NewHTTPClient`, `NewWebSocketDialer`, `DialContext` | egress TCP by host name (patch) |
| ICE host candidates, STUN | pion `SettingEngine` net | egress UDP (`ListenPacket`), IPv4 only (patch) |
| TURN over UDP / TCP / TLS | pion ICE proxy dialer, `Net.Dial` | egress UDP / TCP (patch) |
| Name lookups (provider, TURN hosts) | `protect` resolver, `net.DefaultResolver` | Yandex DoH (`77.88.8.8:443`) through the egress TCP |
| HTTP / WebSocket defaults of third-party libraries | `http.DefaultTransport`, `websocket.DefaultDialer` | egress TCP (`egress.go`) |
| mDNS, ICE-TCP listeners, SOCKS5 listener | pion, `internal/client` | not used |
| Proxied flows | smux streams over the room | resolved and dialed by the server |

## Behavior

- TCP only: the server opens TCP streams and has no UDP relay, so the node is described without UDP.
- ICE runs over IPv4 UDP through the egress. Host candidates carry a placeholder address (`192.0.2.1`); the peer
  sees the egress's real address as peer-reflexive or through TURN. There is no ICE over TCP.
- While the room is joining or rejoining the node reports `degraded` with the reason; a failed call is rejoined
  with a backoff of 2 s up to 1 min. Only an invalid configuration (key, device id) stops the extension.
- Liveness follows upstream's mobile defaults: a ping every 10 s, 4 missed 15 s answers end the call.
- WB Stream with `datachannel` does not work: the guest flow cannot publish data (upstream `docs/about.md`); use
  `vp8channel`, `seichannel` or `videochannel`.
- The device id upstream sends to the provider is kept in `<dataDir>/device-id`.
- A `vp8channel` room serves one client at a time: a second client in the same room is taken for a server restart
  and both sessions break.

## Live status

Tested 2026-11-12 on two subscription nodes; neither carried traffic. A Telemost room joined over the egress (ICE,
TURN and DoH all through it) and opened the session, but control pings took 7–28 s and every delay test timed out
or ended in EOF, at 30, 60 and 120 fps alike; upstream's own client fails the same handshake from the same host. A
WB Stream room answered `guests cannot create rooms`: the room in the link does not exist.
