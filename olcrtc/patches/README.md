# Upstream patch

Applies to `https://github.com/openlibrecommunity/olcrtc` at commit `f3ad8fb7c0d0fa981423f83269526fa94077c23d`
(pinned in `../build.sh`).

- `egress.diff`: the change as a unified diff against that commit.
- `_overlay/`: full copies of the changed and added upstream files; `build.sh` compiles them in place of the
  originals. The leading `_` keeps Go from treating them as packages of this module.
- `upstream.sha256`: the SHA-256 of each original upstream file the overlay replaces. `build.sh` stops when the
  pinned upstream file no longer matches, so a commit bump forces the patch to be redone.

What it changes:

- `internal/protect/egress.go` (new): the process-wide `Egress` and pion's `transport.Net` over it (UDP listen and
  dial, TCP dial, one placeholder interface, no listeners).
- `internal/protect/protect.go`: the HTTP transport, WebSocket dialer, `DialContext` and resolver use the egress
  when one is set.
- `internal/engine/pion.go`, `internal/engine/pion_egress.go` (new): with an egress, pion gets that Net, IPv4 UDP
  candidates only, the egress as the ICE proxy dialer for TURN, and no mDNS.
- `internal/client/dial.go` (new): `RunDialer` joins the room like `Run` but hands out a stream dialer instead of
  opening a SOCKS listener; `ErrConfig` marks key and device-id failures.
- `pkg/olcrtc/client/egress.go` (new): exports `SetEgress`, `SetVerbose`, `RunDialer`, `ErrConfig`,
  `ErrClientStopped`.

To update after an upstream bump: copy the new upstream files into `_overlay/`, re-apply the change, regenerate the
diff, and write the new hashes with `sha256sum internal/protect/protect.go internal/engine/pion.go` run in the
upstream checkout.
