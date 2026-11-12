# naive

ReClash extension for NaiveProxy links (`naive+https://user:pass@host:port#name`, the URI v2rayN and NekoBox write).
MIT, like the SDK; the code is a reimplementation of the protocol, not a port.

The client mimics [NaiveProxy](https://github.com/klzgrad/naiveproxy): a TLS connection to the server with Chrome's
ClientHello and HTTP/2 settings (`bogdanfinn/tls-client` profile `Chrome_146`, `fhttp`, `utls`), one HTTP/2 CONNECT
stream per tunnel with `proxy-authorization: basic`, and naive's padding (`src/net/tools/naive/naive_protocol.h`,
`naive_padding_framer.cc`, `naive_proxy_delegate.cc`): the `padding` request header, `padding-type-request: 1, 0`
negotiated through `padding-type-reply`, `fastopen: 1` once the type is known, and for padding variant 1 the first 8
reads and writes of a tunnel framed as `[payload size, 2 bytes BE][padding size, 1 byte][payload][zeros]`.
The HTTP/2 receive windows are raised to naive's values.

Every socket, including the DNS lookup of the server name, goes through the Core's egress: the SDK's SOCKS5 dialer is
the only dialer in the code and the libraries are never given one of their own.

## Build

Requires Go 1.26.6 or later and network access to the Go module proxy for the first build.

```sh
./build.sh          # dist/naive/0.1.0/{manifest.json,naive-ext}, linux/amd64 by default (GOOS/GOARCH override)
./build.sh test     # go vet + go test
```

Install for a local Core by copying `dist/naive/<version>/` to `<core home>/extensions/naive/<version>/`.

## Behavior

- Link query parameters: `insecure-concurrency=N` (HTTP/2 connections tunnels are spread over, at most 8, default 1),
  `extra-headers=` (CRLF-separated `name: value`), `padding=true|false` (accepted and ignored: padding is always
  negotiated). `naive+quic` links, a path, and extra headers the client sets itself are refused.
- TCP only. NaiveProxy carries UDP only over QUIC, so the node reports `udp=false`.
- A server that answers CONNECT with an error is reported by status: 407 sets the node `degraded` with a
  credentials message; a refused CONNECT is told apart from an unreachable target in the SOCKS reply code.
- The server certificate is verified against the system roots; there is no option to skip it.

## Limits

- The ClientHello and HTTP/2 framing follow the pinned Chrome profile, not the Chromium that the current naive
  release was built from; a server that fingerprints exactly may tell them apart.
- No HTTP/3 (`naive+quic`), no `--proxy` chaining, no `--host-resolver-rules`.
- The binary links BSD-licensed libraries (`bogdanfinn/tls-client`, `fhttp`, `utls`); their licenses apply to them.
