# cslb — Client Side Load Balance

[![Go Reference](https://pkg.go.dev/badge/github.com/fovecifer/cslb.svg)](https://pkg.go.dev/github.com/fovecifer/cslb)

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
- **Options pattern** — clean three-level configuration: Transport, Upstream, Backend
- **Zero dependencies** — only the Go standard library

## Install

```
go get github.com/fovecifer/cslb
```

## Quick Start

```go
package main

import (
    "fmt"
    "net/http"
    "time"

    "github.com/fovecifer/cslb"
)

func main() {
    transport := cslb.NewTransport(
        cslb.WithUpstream("http://api.example.com",
            cslb.Backend("http://10.0.0.1:8080", cslb.Weight(5)),
            cslb.Backend("http://10.0.0.2:8080", cslb.Weight(3)),
            cslb.Backend("http://10.0.0.3:8080", cslb.AsBackup()),
            cslb.UseLeastConn(),
        ),
        cslb.WithTimeout(5*time.Second),
    )

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

## Algorithms

| Algorithm | Constructor | Description |
|-----------|-------------|-------------|
| Round-Robin | `UseRoundRobin()` | Smooth weighted round-robin (default) |
| Least-Conn | `UseLeastConn()` | Routes to the peer with fewest active connections |
| IP-Hash | `UseIPHash()` | Consistent hashing by client IP (uses /24 subnet for IPv4) |
| Key-Hash | `UseHash(keyFunc)` | CRC32-based hash on an arbitrary request key |
| Random | `UseRandom()` | Weighted random selection |
| Random-Two | `UseRandomTwo()` | Power of Two Choices — picks the less loaded of two random peers |

## Configuration

### Transport Options

| Option | Description |
|--------|-------------|
| `WithUpstream(pattern, opts...)` | Register an upstream group matched by scheme+host |
| `WithRoundTripper(rt)` | Use a custom `http.RoundTripper` as the underlying transport |
| `WithTimeout(d)` | Per-attempt request timeout |
| `WithConnectTimeout(d)` | TCP connect timeout (ignored with custom RoundTripper) |
| `WithMaxBodyBuffer(size)` | Max body size buffered in memory for retries (default 32MB) |
| `WithNextUpstream(cond)` | Custom retry condition function |
| `WithNextUpstreamCodes(codes...)` | Retry on specific HTTP status codes |

### Backend Options

| Option | Description |
|--------|-------------|
| `Weight(w)` | Selection weight (default 1) |
| `MaxFails(n)` | Failures before temporarily disabling (default 1) |
| `FailTimeout(d)` | Duration a failed backend stays disabled (default 10s) |
| `MaxConns(n)` | Max concurrent connections (0 = unlimited) |
| `AsBackup()` | Only used when all primary backends are unavailable |
| `AsDown()` | Initially marked as down |

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
