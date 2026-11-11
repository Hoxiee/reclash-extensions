# Upstream patch

Applies to `github.com/p1neappleXpress/OpenFlux` at commit `edcf77a1848a72370cfba9f346d3b897b61e7e7d`
(`v0.7.1-0.20261013095521-edcf77a1848a`, the version pinned in `../go.mod`).

- `netbind-setdial.diff`: the change as a unified diff against that commit, in the form to propose upstream.
- `_overlay/`: full copies of the three upstream files with the change applied; `build.sh` swaps them in with
  `go build -overlay`. The leading `_` keeps Go from treating them as packages of this module.
- `upstream.sha256`: the SHA-256 of each original upstream file. `build.sh` stops when the pinned upstream file no
  longer matches, so a version bump forces the patch to be redone.

What it changes:

- `netbind/netbind.go`: `SetDial(f)` makes every dial of the core go through `f` (here: the Core's SOCKS5 egress)
  and points `http.DefaultTransport`, `websocket.DefaultDialer` and `net.DefaultResolver` at it, as the Windows
  binding already does. `Dialer` and `Wrap` return `*BoundDialer` (a `*net.Dialer` whose `Dial`/`DialContext`
  follow `SetDial`) instead of `*net.Dialer`; every existing call site compiles unchanged.
- `transport/script/host.go`: `dialer()` returns `*netbind.BoundDialer`, the one call site whose type is spelled out.
- `transport/yandex/dial.go`: with `SetDial` active, names go to the redirect unresolved instead of through the
  host's resolver (`lookupIPAddr` is bound to the original `net.DefaultResolver` at init).

To update after an upstream bump: copy the new upstream files into `_overlay/`, re-apply the change, regenerate the
diff, and write the new hashes with `sha256sum netbind/netbind.go transport/script/host.go transport/yandex/dial.go`
run in the upstream module directory.
