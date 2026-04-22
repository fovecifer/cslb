# cslb — Client Side Load Balance

[![Go Reference](https://pkg.go.dev/badge/github.com/fovecifer/cslb/v2.svg)](https://pkg.go.dev/github.com/fovecifer/cslb/v2)

`cslb` is a Go library that implements client-side load balancing as an `http.RoundTripper`.
It intercepts outgoing HTTP requests, selects a backend using a load balancing algorithm,
and rewrites the request URL before forwarding — fully transparent to the caller.

All algorithms faithfully follow **nginx**'s upstream implementation.

## Features

- **Drop-in replacement** — works as a standard `http.RoundTripper`
- **6 algorithms** — Round-Robin, Least-Conn, IP-Hash, Key-Hash, Random, Random-Two (Power of Two Choices)
- **Nginx-style peer management** — weighted selection, effective weight degradation/recovery, max_fails, fail_timeout, max_conns, backup peers
- **Automatic failover** — retries failed requests on the next available backend
- **Configurable retry conditions** — control which HTTP status codes trigger a retry (like nginx's `proxy_next_upstream`)
- **Request body replay** — buffers request bodies for safe retry (supports GetBody, seekable bodies, memory/file spill)
- **Per-attempt timeout** — each backend attempt gets its own timeout
- **Scheme rewriting** — HTTP frontend can route to HTTPS backends (or mixed)
- **Host header preservation** — original Host is preserved when rewriting
- **Nginx-style config hierarchy** — explicit `Upstream` and `Server` blocks
- **Zero dependencies** — only the Go standard library

## Install

```
go get github.com/fovecifer/cslb/v2
```

## Quick Start

```go
package main

import (
    "fmt"
    "net/http"
    "time"

    "github.com/fovecifer/cslb/v2"
)

func main() {
    transport, err := cslb.NewTransportE(
        cslb.WithUpstreams(
            cslb.Upstream("http://api.example.com",
                cslb.Server("http://10.0.0.1:8080", cslb.Weight(5)),
                cslb.Server("http://10.0.0.2:8080", cslb.Weight(3)),
                cslb.Server("http://10.0.0.3:8080", cslb.Backup()),
            ).LeastConn(),
        ),
        cslb.WithTimeout(5*time.Second),
    )
    if err != nil {
        panic(err)
    }

    client := &http.Client{Transport: transport}

    // Requests to http://api.example.com are load-balanced across backends.
    // All other requests pass through unchanged.
    resp, err := client.Get("http://api.example.com/v1/users")
    if err != nil {
        panic(err)
    }
    defer resp.Body.Close()
    fmt.Println(resp.Status)
}
```

`WithUpstreams(...)` is the primary configuration API. It cleanly separates
upstream-level and server-level configuration, similar to nginx's
`upstream { server ...; }` blocks.
For fail-fast validation during construction, use `NewTransportE(...)` and
handle the returned error.

## Algorithms

| Algorithm | Constructor | Description |
|-----------|-------------|-------------|
| Round-Robin | `.RoundRobin()` | Smooth weighted round-robin (default) |
| Least-Conn | `.LeastConn()` | Routes to the peer with fewest active connections |
| IP-Hash | `.IPHash()` | Consistent hashing by client IP (uses /24 subnet for IPv4) |
| Key-Hash | `.Hash(keyFunc)` | CRC32-based hash on an arbitrary request key |
| Random | `.Random()` | Weighted random selection |
| Random-Two | `.RandomTwo()` | Power of Two Choices — picks the less loaded of two random peers |

## Configuration

### Transport Options

| Option | Description |
|--------|-------------|
| `WithUpstreams(upstream...)` | Register explicit nginx-style upstream blocks |
| `WithRoundTripper(rt)` | Use a custom `http.RoundTripper` as the underlying transport |
| `WithTimeout(d)` | Per-attempt request timeout |
| `WithConnectTimeout(d)` | TCP connect timeout (ignored with custom RoundTripper) |
| `WithMaxBodyBuffer(size)` | Max body size buffered in memory for retries (default 32MB) |
| `WithNextUpstream(cond)` | Custom retry condition function |
| `WithNextUpstreamCodes(codes...)` | Retry on specific HTTP status codes |

### Upstream / Server API

```go
cslb.WithUpstreams(
    cslb.Upstream("http://api.example.com",
        cslb.Server("http://10.0.0.1:8080", cslb.Weight(5)),
        cslb.Server("http://10.0.0.2:8080", cslb.Weight(3)),
        cslb.Server("http://10.0.0.3:8080", cslb.Backup()),
    ).LeastConn(),
)
```

`Upstream(...)` groups `Server(...)` entries together so algorithm selection,
hashing, and SNI overrides live at the upstream level instead of being mixed
into the same variadic option list as individual backends.

### Server Options

| Option | Description |
|--------|-------------|
| `Weight(w)` | Selection weight (default 1) |
| `MaxFails(n)` | Failures before temporarily disabling (default 1) |
| `FailTimeout(d)` | Duration a failed server stays disabled (default 10s) |
| `MaxConns(n)` | Max concurrent connections (0 = unlimited) |
| `Backup()` | Only used when all primary backends are unavailable |
| `Down()` | Initially marked as down |

## Retry Conditions

By default, requests are retried on connection errors or any 5xx response. Customize with:

```go
// Retry only on specific status codes (connection errors always retry)
cslb.WithNextUpstreamCodes(502, 503, 504)

// Full control with a custom function
cslb.WithNextUpstream(func(resp *http.Response, err error) bool {
    if err != nil {
        return true
    }
    return resp.StatusCode == 502 || resp.StatusCode == 503
})
```

## Architecture

The design mirrors nginx's three-layer upstream callback architecture:

| Layer | nginx | cslb |
|-------|-------|------|
| 1. Init upstream | `init_upstream` | `Balancer` interface |
| 2. Init peer | `init_peer` | `NewPicker()` method |
| 3. Get / Free | `get_peer` / `free_peer` | `Pick()` / `Done()` methods |

## License

MIT
