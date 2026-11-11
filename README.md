# reclash-extensions

Protocol extensions for ReClash: separate executables that speak `reclash-ext/1` and give ReClash Core protocols
mihomo does not implement.

- `sdk/` — the Go SDK every extension is built on (MIT). `sdk/testdata/protocol.json` is a copy of
  `ReClash/core/extensions/testdata/protocol.json`; the SDK tests fail when the two differ.
- `openflux/` — the OpenFlux wrapper (GPL-3.0-or-later, like upstream). Build and test it with `openflux/build.sh`,
  see `openflux/README.md`.
- `qwdtt/` — the qWDTT (WireGuard over VK Calls TURN) wrapper (GPL-3.0-or-later, like upstream). Build and test it
  with `qwdtt/build.sh`, see `qwdtt/README.md`.

The protocol is described in `ReClash/project/_new/design/extensions/SPEC.md` §3.

## Writing an extension

Implement `sdk.Extension` (`Info`, `Describe`, `Start`) and call `sdk.Main` from `main`. `Start` gets an
`sdk.Runtime`: `Egress` is the only network access an extension may use (`DialContext` for tcp/udp,
`ListenPacket` for unconnected UDP, all through the Core's SOCKS5), `Log` writes redacted lines to stderr
(register decoded secrets with `Log.AddSecret`), `Status` and `Fatal` report to Core. The returned `sdk.Proxy`
(`DialTCP`, plus `DialUDP` for UDP) is served on the loopback SOCKS5 front with per-process credentials.
