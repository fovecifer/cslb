# cslb — Client Side Load Balance

[![Go Reference](https://pkg.go.dev/badge/github.com/fovecifer/cslb/v2.svg)](https://pkg.go.dev/github.com/fovecifer/cslb/v2)

`cslb` is a Go library that implements client-side load balancing as an
`http.RoundTripper`. It intercepts outgoing HTTP requests, selects a backend,
and rewrites the request URL before forwarding — fully transparent to the
caller.

Its selection algorithms, peer health accounting, and default failover policy
are modeled on **nginx** and covered by compatibility tests. The client-side
execution model has deliberate differences, documented below.

## Features

- **Drop-in replacement** — works as a standard `http.RoundTripper`
- **7 algorithms** — Round-Robin, Least-Conn, IP-Hash, Key-Hash, Consistent-Hash, Random, Random-Two (Power of Two Choices)
- **Nginx-style peer management** — weighted selection, effective weight degradation/recovery, max_fails, fail_timeout, max_conns, backup peers
- **Nginx-aligned failover defaults** — retries transport errors/timeouts; HTTP status retries are opt-in
- **Configurable retry conditions** — control which HTTP status codes trigger a retry (like nginx's `proxy_next_upstream`)
- **Non-idempotent safety gate** — sent `POST`, `LOCK`, and `PATCH` requests are not retried unless explicitly enabled
- **Request body replay** — supports `GetBody`, seekable bodies, and memory/temp-file buffering when retries are enabled
- **Per-attempt timeout** — each backend attempt gets its own timeout
- **Scheme rewriting** — HTTP frontend can route to HTTPS backends (or mixed)
- **Host header preservation** — original Host is preserved when rewriting
- **Nginx-style config hierarchy** — explicit `Upstream` and `Server` blocks
- **Zero dependencies** — only the Go standard library

## Install

```bash
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
handle the returned error. It rejects non-positive explicit weights, negative
connection/failure limits, negative timeouts or body-buffer sizes, and unknown
algorithm values.

## Runnable Examples

[example_test.go](./example_test.go) contains runnable examples for every
algorithm, default and status-based failover, backup servers, safe opt-in POST
replay, timeouts, mixed HTTP/HTTPS backends, body spill, custom transports,
`ProxySSLName`, and request-key hashing. Run them with:

```bash
go test -run '^Example' -v
```

## Algorithms

| Algorithm | Constructor | Description |
|-----------|-------------|-------------|
| Round-Robin | `.RoundRobin()` | Smooth weighted round-robin (default) |
| Least-Conn | `.LeastConn()` | Routes to the peer with fewest active connections |
| IP-Hash | `.IPHash()` | Consistent hashing by client IP when the request provides one |
| Key-Hash | `.Hash(keyFunc)` | CRC32-based hash on an arbitrary request key |
| Consistent-Hash | `.HashConsistent(keyFunc)` | Ketama-compatible consistent hash with minimal remapping when peers change |
| Random | `.Random()` | Weighted random selection |
| Random-Two | `.RandomTwo()` | Power of Two Choices — picks the less loaded of two random peers |

`.IPHash()` extracts the client address from `X-Real-IP`, `X-Forwarded-For`,
then `RemoteAddr` and uses the IPv4 /24 subnet behavior from nginx. In a normal
client-side `http.Client`, outgoing requests usually do not have those fields
unless the caller sets them explicitly; when no client IP is available, IP-Hash
falls back to Round-Robin. For ordinary client affinity, prefer
`.Hash(func(*http.Request) string)` and provide the key explicitly.

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
| `WithNonIdempotentRetries()` | Allow sent `POST`/`LOCK`/`PATCH` requests to retry; may duplicate side effects |

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

Upstream algorithm methods are `.RoundRobin()`, `.LeastConn()`, `.IPHash()`,
`.Hash(keyFunc)`, `.HashConsistent(keyFunc)`, `.Random()`, and `.RandomTwo()`.
Use `.ProxySSLName(name)` to override the TLS server name for an HTTPS
upstream; see [TLS server name](#tls-server-name) below.

### Server Options

| Option | Description |
|--------|-------------|
| `Weight(w)` | Selection weight (default 1) |
| `MaxFails(n)` | Failures before temporarily disabling (default 1; 0 disables suppression) |
| `FailTimeout(d)` | Duration a failed server stays disabled (default 10s; explicit 0 is preserved) |
| `MaxConns(n)` | Max concurrent connections (0 = unlimited) |
| `Backup()` | Only used when all primary backends are unavailable |
| `Down()` | Initially marked as down |

## Retry and Failure Semantics

The default policy matches nginx's `proxy_next_upstream error timeout`:

- An error returned by the underlying `RoundTripper`, including a connect or
  per-attempt timeout, is retried on the next available peer.
- An HTTP response is returned as-is. A `500`, `502`, or any other status does
  **not** trigger a retry unless configured.
- If the caller's request context is canceled or its overall deadline expires,
  cslb stops immediately and does not penalize the selected peer.

> **Migration note:** earlier cslb releases retried every 5xx response and
> replayed sent POST-like requests by default. Existing applications that rely
> on either behavior must add `WithNextUpstreamCodes` and, where replay is
> genuinely safe, `WithNonIdempotentRetries`.

HTTP status retries are opt-in:

```go
transport := cslb.NewTransport(
    // Equivalent to:
    // proxy_next_upstream error timeout http_502 http_503 http_504;
    cslb.WithNextUpstreamCodes(502, 503, 504),
    cslb.WithUpstreams(/* ... */),
)

// Full control with a custom function
cslb.WithNextUpstream(func(resp *http.Response, err error) bool {
    if err != nil {
        return true
    }
    return resp.StatusCode == 502 || resp.StatusCode == 503
})
```

`WithNextUpstreamCodes` always retains transport-error retries. A custom
`WithNextUpstream` condition has full control and should normally return true
when `err != nil`.

### POST, LOCK, and PATCH

Like nginx, cslb does not retry `POST`, `LOCK`, or `PATCH` after the request has
been sent. A connection failure before anything is written can still move to
the next peer. To opt in when the operation is safe to replay:

```go
transport := cslb.NewTransport(
    cslb.WithNextUpstreamCodes(502, 503, 504),
    cslb.WithNonIdempotentRetries(),
    cslb.WithUpstreams(/* ... */),
)
```

Enabling `WithNonIdempotentRetries` does not select retryable status codes; it
only removes the method safety gate. Use application-level idempotency keys or
another deduplication mechanism before enabling it.

With the standard `*http.Transport`, cslb tracks whether headers/request bytes
were written. A custom `RoundTripper` has no general request-sent contract, so
an error from an arbitrary custom implementation is conservatively treated as
having sent the request.

Retry selection and peer health are separate: configured `403` and `404`
responses can move to another peer but do not increment `max_fails` or reduce
effective weight, matching nginx.

## Request Body Buffering

Retryable bodies use the request's `GetBody` callback when available, then a
seekable-body fast path, then memory/temp-file buffering. `WithMaxBodyBuffer`
is the in-memory threshold, not a total request-size limit. Bodies above it
spill to a temporary file, and the temporary file currently has no library
level size cap. Applications accepting untrusted or very large uploads should
enforce their own request-size limit.

## TLS Server Name

For HTTPS backends addressed by IP or by a name different from the certificate
name, configure the upstream TLS name explicitly:

```go
cslb.Upstream("https://api.internal",
    cslb.Server("https://10.0.0.10:8443"),
).ProxySSLName("api.example.com")
```

`ProxySSLName` sets `tls.Config.ServerName` on a cloned per-upstream
`*http.Transport`, affecting both SNI and certificate verification. It is
equivalent to the combined intent of nginx's `proxy_ssl_server_name on` and
`proxy_ssl_name`. Construction fails when it is combined with a custom
RoundTripper that is not an `*http.Transport`.

## Nginx Compatibility Boundaries

- Smooth weighted Round-Robin, Least-Conn, IP-Hash, Hash, Consistent-Hash,
  Random, and Random-Two selection are ported from nginx and exercised by
  compatibility tests.
- A true single-server upstream ignores `max_fails` and `fail_timeout` and
  does not degrade effective weight. Adding a backup disables this single-peer
  fast path, so a one-primary/one-backup upstream can fail over normally.
- nginx rejects `backup` with `hash` (including consistent hash), `ip_hash`,
  and `random` (including Random-Two); cslb currently permits those
  combinations as a client-side extension.
- `.IPHash()` has no implicit nginx `$remote_addr` in an outgoing
  `http.Client`. It uses caller-provided `X-Real-IP`, `X-Forwarded-For`, or
  `RemoteAddr`, otherwise it falls back to Round-Robin.
- `WithTimeout` is a per-attempt context deadline, rather than a direct clone
  of one nginx proxy timeout directive.
- Dynamic upstream zones, runtime DNS reconfiguration, and `slow_start` are
  outside this library's current scope.

## Architecture

The design mirrors nginx's three-layer upstream callback architecture:

| Layer | nginx | cslb |
|-------|-------|------|
| 1. Init upstream | `init_upstream` | `Balancer` interface |
| 2. Init peer | `init_peer` | `NewPicker()` method |
| 3. Get / Free | `get_peer` / `free_peer` | `Pick()` / `Done()` methods |

## License

MIT
