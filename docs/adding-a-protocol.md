# Adding a protocol as an extension

This guide takes a protocol from "a subscription has links ReClash cannot import" to a node that carries traffic.
The contract itself (`reclash-ext/1`, the supervisor, packaging) is specified in
`ReClash/project/_new/design/extensions/SPEC.md`; this page is the working procedure. `openflux/`, `qwdtt/`,
`naive/` and `olcrtc/` are the reference wrappers, and every rule below is one of them already follows.

## 1. Decide whether it needs an extension

| The protocol is… | Do this |
|---|---|
| implemented by mihomo (even under another name, e.g. `mierus://` is mieru, `tt://` is trusttunnel) | a Dart link parser in `ReClash/lib/subscriptions/imports/subscription/subscription_links.dart`, no extension |
| missing from mihomo | an extension in this repository |
| a carrier for something mihomo has (TURN, a CDN, a messenger's calls) | still an extension: the carrier is the protocol |

An extension never shadows a native mihomo type: the Dart import consults the extension catalog only after every
native parser declined the line. Check `ReClash/core/mihomo/adapter/parser.go` and the `adapter/outbound` options of
the pinned mihomo before you start; a missing option on a native type is a parser loss (`onLoss`), not a new extension.

## 2. Study the upstream first

Write the answers down; they go into the wrapper's README.

- **Source and pin.** Repository, last commit you build from, tag if any, maintenance state (archived upstreams are
  marked `upstream-archived` in the README).
- **License.** It decides the wrapper's license (section 8). Check direct dependencies too.
- **Link format.** From a primary source: upstream docs, the upstream parser, or the client that generates links. Do
  not reverse-engineer from a subscription alone; note in the README what the format was verified against.
- **Socket inventory.** Every place the client touches the network: the server connection, HTTP APIs, STUN/TURN,
  ICE, DNS lookups, update checks, telemetry. Grep for `net.Dial`, `net.Listen`, `net.Resolver`,
  `http.DefaultClient`, `http.DefaultTransport`, `tls.Dial`, `ListenUDP`, `ListenPacket`, and library constructors that
  dial on their own (pion, utls clients, `tls-client.NewHttpClient`). This table is the core of the review; each row
  must end in "egress".
- **Shape.** TCP streams, datagrams, or an L3 tunnel (WireGuard and the like). L3 runs a userspace netstack inside
  the extension (`qwdtt/` uses gVisor netstack through wireguard-go), so Core always sees SOCKS5.

Non-Go upstreams: a Go reimplementation (as `naive/` does) is preferred over shipping a foreign binary, because a
foreign binary cannot be forced through the egress.

## 3. The one hard rule: everything through the egress

Core hands the extension an egress SOCKS5 endpoint, and `sdk.Runtime.Egress` is the extension's only network access:

- `Egress.DialContext(ctx, "tcp"|"udp", "host:port")` — a stream or a connected datagram socket;
- `Egress.ListenPacket(ctx)` — an unconnected UDP socket (UDP ASSOCIATE), for ICE, TURN, QUIC.

Why it is absolute: with TUN on, a direct socket from the extension loops back into ReClash; on Android it would
need `protect`; and only the egress applies the node's `dialer-proxy`, `interface-name` and routing mark. A direct dial
that "works on my machine" breaks every one of those.

How the reference wrappers get upstream onto the egress, in order of preference:

1. Pass a dialer into upstream: `tls-client`'s `WithDialContext`, pion's `SettingEngine` net, an `http.Transport`
   with `DialContext`. Wrap `Egress` in the interface the library wants (`qwdtt/egress.go`).
2. Patch the call site (section 4) when upstream offers no hook.
3. Re-implement the client when the upstream is unusable as a library (`naive/`).

**DNS.** Never resolve with the system resolver. With TUN on, Core answers fake-ip addresses, so a hostname resolved
outside the egress points nowhere. Either hand the hostname to the egress (SOCKS5 carries names; prefer this), or,
when upstream insists on an IP, resolve over DoH dialed through the egress (`qwdtt/egress.go` uses Yandex DoH at
`77.88.8.8`, reachable where foreign resolvers are blocked).

Before review, prove it: a test with a fake egress that counts dials and fails on any other socket (see
`naive/main_test.go`), and an empty grep for the calls listed in section 2 outside the egress helpers.

## 4. Patching upstream

When upstream is not a fetchable module or needs a change:

- `build.sh` fetches the pinned commit into `build/upstream` (shallow fetch of the exact SHA) and refuses to build if
  `HEAD` differs.
- Changed files live as full copies in `patches/_overlay/` (the `_` keeps Go from treating it as a package), the
  original files' SHA-256 in `patches/upstream.sha256`, and the change as a unified diff in `patches/egress.diff`.
  `build.sh` checks each hash, so an upstream bump forces the patch to be redone instead of silently applying to new
  code.
- `go build -overlay build/overlay.json` compiles upstream files into the wrapper package; a plain `go build` must
  fail, on purpose.
- Keep the patch minimal: rename an upstream `main`, swap one dial function, add one dialer option. `qwdtt/` is +5/−2
  lines over four files. `patches/README.md` lists every change and the update procedure.

## 5. Write the wrapper

Directory `<id>/`, where `<id>` matches `^[a-z0-9][a-z0-9._-]{0,63}$` and is the id everywhere (catalog, manifest,
install path, node `extension:` key).

```
<id>/
  main.go        sdk.Main(extension{}); version var set by build.sh
  link.go        link parsing, describe, secretsOf
  client.go      Start: config → client on the egress → sdk.Proxy
  main_test.go   synthetic values only
  build.sh       test | dist | prepare
  go.mod         module github.com/Hoxiee/reclash-extensions/<id>; replace …/sdk => ../sdk
  README.md  LICENSE  .gitignore (/build/ /dist/)
  patches/       only when upstream is patched
```

`sdk.Extension` has three methods:

- `Info()` — `{ID, Version}`.
- `Describe(link)` — name, server and UDP support for import. Pure: no network, no files. Return an error for links
  this extension cannot run (e.g. `naive+quic`), so import reports them instead of creating dead nodes.
- `Start(ctx, rt)` — parse `rt.Link`, call `rt.Log.AddSecret(...)` with every secret the link decodes to (passwords,
  keys, tokens, room ids) before logging anything, build the client on `rt.Egress`, and return an `sdk.Proxy`
  (`DialTCP(ctx, "host:port")`) or an `sdk.UDPProxy` (adds `DialUDP`). Only an `sdk.UDPProxy` gets UDP ASSOCIATE on
  the front; report `udp` truthfully.

Runtime signals:

- `rt.Status(sdk.StateDegraded, reason)` when the carrier fails but may recover (captcha, auth rejected, server
  closing sessions); `rt.Status(sdk.StateOK, "")` when it recovers. Repeats are dropped, so call it freely.
- `rt.Fatal(err)` only for what a restart cannot fix (a link that parses but cannot run). Core restarts crashed
  processes with backoff and gives up after five crashes in five minutes.
- `rt.DataDir` is per node and persistent: device ids, cached credentials. Nothing else is writable.
- Stdout belongs to the SDK. An upstream that prints to stdout must be redirected (`qwdtt/main.go` pipes it into the
  logger).

## 6. Tests

`main_test.go` uses synthetic values only: documentation addresses (`192.0.2.x`, `198.51.100.x`, `203.0.113.x`),
`u`/`p` credentials, dummy keys. Never a value from a real subscription. Cover:

- link parsing: a valid link per variant, and each rejection (missing credentials, bad port, unknown scheme or mode);
- `Describe` output, including the error for unsupported variants;
- `secretsOf`: every secret in the link is registered;
- protocol framing round trips (padding, headers, obfuscation layers) where the wrapper implements them;
- an end-to-end test against a local fake server through a fake egress, asserting that every connection went
  through the egress.

## 7. Build and the manifest

`./build.sh test` runs `go vet` and `go test` (with the overlay when there is one). `./build.sh` (dist) builds
`CGO_ENABLED=0` for `GOOS`/`GOARCH` (default `linux/amd64`) into `dist/<id>/<version>/` with:

```json
{
  "id": "<id>",
  "version": "0.1.0",
  "protocol": [1],
  "schemes": ["<scheme>"],
  "executable": "<id>-ext",
  "sha256": "<sha256 of the executable>"
}
```

Core installs from `<core home>/extensions/<id>/<version>/`, picks the highest installed version whose `protocol`
contains its own, and re-checks `sha256` before every start: rebuild means reinstall. `schemes` are lowercase and
may contain `+` (`naive+https`).

## 8. License

- Wrapper code that compiles upstream code in (overlay or vendored) takes the upstream's license; keep upstream's
  `LICENSE` in the directory (`openflux/`, `qwdtt/`: GPL-3.0-or-later).
- A reimplementation with no upstream code is MIT like the SDK (`naive/`), unless a dependency forbids it; check
  dependency compatibility in both directions (GPL-3.0 cannot link BSD-4-clause code, which is why `naive/` is MIT).
- Non-commercial or source-available upstreams (csqtt) stay out of this repository.

## 9. Live test with extcheck

`ReClash/core/cmd/extcheck` loads a mihomo YAML outside the app, runs the real supervisor and egress, and delay-tests
the nodes. Keep everything in a scratch directory, never in a repository, and delete files with real links afterwards.

```sh
mkdir -p "$S/home/extensions" && cp -r dist/<id> "$S/home/extensions/"
cat >"$S/nodes.yaml" <<'EOF'
proxies:
  - {name: n-1, type: extension, extension: <id>, link: "<real link>"}
EOF
cd ReClash/core && go run ./cmd/extcheck -home "$S/home" -config "$S/nodes.yaml" -rounds 3
```

`-home` must be a directory of its own, never the app's home. Run each node for several rounds: the first includes
the extension start. In reports, name nodes `n-1`, `n-2`, never by host. When a node fails, decide from the logs
whether the server or the wrapper is at fault (a session the server closes right after the handshake while another
node on the same code works is the server).

## 10. Register it in ReClash

Core needs no change: any `type: extension` node runs whatever is installed under its id. The app needs one catalog
entry so import recognises the scheme and the UI names the protocol:

- `ReClash/lib/subscriptions/imports/subscription/extension_catalog.dart`: add
  `ExtensionEntry(id: '<id>', name: '<Display name>', schemes: ['<scheme>'])`. `name` is what proxy cards and editors
  show as the node's type, and what unnamed nodes are numbered after (`<Display name> 1`). Import names a node after
  the link's `#` fragment; when the fragment is not a name (olcRTC keeps its key there), pass `nameOf`, or the node
  is named after a secret.
- `ReClash/test/subscriptions/share_link_compatibility_test.dart`, group `extension links`: one test that a line of
  the scheme imports as an extension node with the right `extension` and `link` (synthetic link).
- Run `flutter analyze --no-fatal-infos` and that test file. No website page changes yet; the site documents
  extensions once the Extensions page ships (SPEC phase 2).

## 11. Before committing

- [ ] Socket inventory: every row ends in the egress; grep is empty outside the egress helpers.
- [ ] `./build.sh test` and `./build.sh` green; manifest `sha256` equals `sha256sum dist/<id>/<version>/<id>-ext`.
- [ ] No real hosts, credentials, keys or room ids in any tracked file: grep the repository for the tokens of every
      tested link.
- [ ] README: what it is, upstream and pinned commit, license reasoning, the link format source, limits (captcha,
      unsupported modes, fingerprint drift, maturity).
- [ ] Root `README.md` lists the wrapper.
- [ ] Live results recorded (delay per node or the exact error with the diagnosis).
- [ ] ReClash catalog entry and its test, as a separate commit in ReClash.

Commits follow Conventional Commits: `feat(<id>): add the <Name> extension wrapper` here,
`feat(subscriptions): import <Name> links as extension nodes` in ReClash.

## Not yet covered

Phase 1 installs locally on desktop only. The signed catalog and downloads, the Extensions page, Android companion
APKs, and privileged Windows and macOS Cores are later phases of `SPEC.md`; a privileged Core refuses to start
extensions today.
