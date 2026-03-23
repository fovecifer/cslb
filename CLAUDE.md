# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Rules

- Always use English for all generated code, comments, commit messages, and documentation in this project.
- Never use `panic` in library code; return errors instead.
- Always update CHANGELOG.md when releasing a new version.

## Commands

```bash
# Run all tests
go test ./...

# Run a single test
go test -run TestRR_SmoothWeightedDistribution ./...

# Run tests with race detector
go test -race ./...

# Run tests with verbose output
go test -v ./...
```

There is no build step — this is a library with no binary. There is no linter configuration in the repo, but `go vet ./...` applies.

## Architecture

`cslb` is a zero-dependency Go library that implements client-side load balancing as an `http.RoundTripper`. The design mirrors nginx's three-layer upstream callback architecture:

| Layer | nginx | cslb |
|-------|-------|------|
| 1 — per-upstream state | `init_upstream` | `Balancer` interface (`balancer.go`) |
| 2 — per-request state | `init_peer` | `Picker` interface / `NewPicker()` |
| 3 — select / report | `get_peer` / `free_peer` | `Pick()` / `Done()` |

### Key types

- **`Peer`** (`balancer.go`) — a single backend server with weight, failure tracking (`fails`, `effectiveWeight`), and connection counts. Mutable state is protected by `PeerGroup.mu`.
- **`PeerGroup`** (`balancer.go`) — a list of peers sharing a mutex; split into primary and backup groups via `BuildGroups()`.
- **`Balancer`** interface — one per upstream group, creates `Picker`s on demand.
- **`Picker`** interface — per-request state; `Pick()` returns next peer, `Done()` reports success/failure.
- **`Transport`** (`transport.go`) — implements `http.RoundTripper`. Matches requests by `scheme+host`, rewrites URL to selected backend, retries on failure. Buffers request bodies for replay (memory up to `maxBodyBuffer`, then spills to a temp file).

### Algorithm structure

All algorithms wrap `RoundRobin` as a base layer, matching nginx's decorator pattern:

- **`RoundRobin`** (`roundrobin.go`) — smooth weighted round-robin. `RRPicker` is embedded as the first field in all other per-request pickers.
- **`LeastConn`** (`leastconn.go`) — routes to the peer with fewest active connections.
- **`IPHash`** (`iphash.go`) — consistent hashing by client IP (uses /24 subnet for IPv4). `NewPickerForIP()` takes a pre-extracted `net.IP`.
- **`Hash`** (`hash.go`) — CRC32-based hash on an arbitrary key. `NewPickerForKey()` takes the key string.
- **`Random`** / **`RandomTwo`** (`random.go`) — weighted random and Power of Two Choices.

### Request flow in `Transport.RoundTrip`

1. Match request `scheme+host` against registered upstreams; pass through unchanged if no match.
2. Buffer request body via `prepareBody()` so it can be replayed on retries.
3. Loop: `Pick()` a peer → rewrite URL → send → call `shouldRetry()` → call `Done()`.
4. On retry, close the failed response body and loop. When no peers remain, forward to the underlying transport directly.

### Options pattern

Three-level functional options: `TransportOption` → `UpstreamOption` → `BackendOption`. All are applied in `NewTransport`, which also clones per-upstream `*http.Transport` instances for upstreams that use `ProxySSLName`.
