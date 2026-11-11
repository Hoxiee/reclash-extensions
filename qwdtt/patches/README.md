# Upstream patch

Applies to `go_client/` of `https://github.com/SpaceNeuroX/proxy-turn-vk-android` at commit
`a296c57eaba69bb9479a24f9157856490890e47d` (tag v1.4.4, pinned in `../build.sh`).

- `egress.diff`: the change as a unified diff against that commit.
- `_overlay/`: full copies of the four upstream files with the change applied; `build.sh` compiles them in place of
  the originals. The leading `_` keeps Go from treating them as packages of this module.
- `upstream.sha256`: the SHA-256 of each original upstream file. `build.sh` stops when the pinned upstream file no
  longer matches, so a commit bump forces the patch to be redone.

What it changes:

- `main.go`: `main` becomes `clientMain`, so the wrapper's `main` runs instead; the rest of the file is shared code.
- `session.go`: `dialTURNConn` becomes `directDialTURNConn`; the wrapper's `dialTURNConn` (egress) takes its place.
- `creds.go`, `creds_vkcalls.go`: the VK API clients dial through `egressDialContext`.

To update after an upstream bump: copy the new upstream files into `_overlay/`, re-apply the change, regenerate the
diff, and write the new hashes with `sha256sum go_client/main.go go_client/session.go go_client/creds.go
go_client/creds_vkcalls.go` run in the upstream checkout.
