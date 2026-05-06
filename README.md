# zap-http

HTTP request/response semantics over the [ZAP transport](https://github.com/zap-proto/spec).

[**zap-proto.io**](https://zap-proto.io) · [Spec](https://github.com/zap-proto/spec) · [Paper](https://github.com/zap-proto/papers/tree/main/transport-vs-jwt) · [Discord](https://zap-proto.io/discord)

Drop-in replacement for `net/http` server and client when both peers live in a trusted boundary (in-cluster service mesh, agent-to-tool, edge-to-edge). Existing handlers and `http.Client` code work unchanged — only the wire underneath changes.

## Why

| Property | `net/http` over TCP+TLS | `zap-http` |
|---|---|---|
| Confidentiality | TLS (classical curves) | X-Wing hybrid PQ (X25519 + ML-KEM-768) |
| Authentication | bearer / JWT at app layer | KEM keypair at transport layer |
| Wire encoding | text headers + chunked body | Cap'n Proto, zero-copy |
| Field access | parse → allocate → copy | pointer offset, O(1) |
| JWT mint per call | typical | not in the path |

## Install

```bash
go get github.com/zap-proto/http
```

Requires Go 1.23+.

## Server

```go
package main

import (
    "io"
    "net/http"

    zaphttp "github.com/zap-proto/http"
)

func main() {
    http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
        io.WriteString(w, "ok")
    })
    if err := zaphttp.ListenAndServe(":9999", nil); err != nil {
        panic(err)
    }
}
```

Same `http.Handler`, same `http.HandleFunc`, same default mux. The wire is ZAP-HTTP.

## Client

```go
package main

import (
    "io"
    "net/http"
    "os"

    zaphttp "github.com/zap-proto/http"
)

func main() {
    client := &http.Client{Transport: zaphttp.NewTransport("server:9999")}
    resp, err := client.Get("http://server/healthz")
    if err != nil { panic(err) }
    defer resp.Body.Close()
    io.Copy(os.Stdout, resp.Body)
}
```

`http.Client` machinery — timeouts, redirects, cookies — keeps working unchanged.

## Wire format

Each HTTP message is one Cap'n Proto frame (`schema/zap_http.capnp`). Today the wire layer is length-prefixed framing over TCP; the [paper](https://github.com/zap-proto/papers/tree/main/transport-vs-jwt) and the v0.2 release will swap that for the full ZAP transport with X-Wing PQ KEM handshake on connect.

```
zap-http v0.1 wire (transitional):
  +---------+-------------------+
  | u32 BE  |   capnp.Frame     |
  | length  |   (zap_http.capnp)|
  +---------+-------------------+

zap-http v0.2 wire:
  +-----------------------------+--------+--------+
  | ZAP transport (X-Wing KEM,  | AEAD   | capnp  |
  | mutual auth, multi-stream)  | header | Frame  |
  +-----------------------------+--------+--------+
```

The Cap'n Proto schema is the source of truth for what's on the wire. Marshal/unmarshal in any language follows from the schema; bindings:
- Go (this repo)
- Rust — [`zap-proto/rust`](https://github.com/zap-proto/rust) (consumer; sub-protocol bindings TBD)
- TypeScript — [`zap-proto/ts`](https://github.com/zap-proto/ts) (planned)

## What's in v0.1

| Feature | Status |
|---|---|
| Request method, target, headers, body | ✓ |
| Response status, reason, headers, body | ✓ |
| Multi-value headers preserved | ✓ |
| Trailers (read & write) | ✓ |
| `http.Client` / `http.Handler` compatibility | ✓ |
| Length-prefixed framing over TCP | ✓ |
| 64 MiB max frame size (configurable) | ✓ |
| Streaming bodies (chunked-style) | v0.2 |
| Connection pool / keep-alive on client | v0.2 |
| Real ZAP transport (PQ KEM handshake) | v0.2 |
| WebSocket-style upgrade | see [zap-proto/ws](https://github.com/zap-proto/ws) |

## Sub-protocol family

- [`zap-http`](https://github.com/zap-proto/http) — this repo
- [`zap-ws`](https://github.com/zap-proto/ws) — multi-stream pubsub, per-stream FEC
- [`zap-fix`](https://github.com/zap-proto/fix) — FIX 4.4/5.0 trading channel
- [`zap-rns`](https://github.com/zap-proto/rns) — KEM-bound service naming
- [`zap-mcp`](https://github.com/zap-proto/mcp) — Model Context Protocol over ZAP
- [`zap-acp`](https://github.com/zap-proto/acp) — Agent Communication Protocol
- [`zap-a2a`](https://github.com/zap-proto/a2a) — Google Agent2Agent over ZAP

By the [composability theorem](https://github.com/zap-proto/papers/tree/main/composability), every sub-protocol that embeds onto ZAP-base inherits its post-quantum confidentiality and mutual authentication automatically.

## Schema regeneration

```sh
make schema    # regenerates internal/capnp/zap_http.capnp.go
```

Requires `capnp` (Cap'n Proto compiler) and `capnpc-go`:

```sh
brew install capnp                                  # macOS
go install capnproto.org/go/capnp/v3/capnpc-go@latest
```

## License

MIT OR Apache-2.0
