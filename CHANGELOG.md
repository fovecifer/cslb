# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- `Transport.CloseIdleConnections`, which closes idle connections of the
  underlying RoundTripper and of the per-upstream transports cloned for
  `ProxySSLName`. `http.Client.CloseIdleConnections` now reaches them.
- `ErrNilResponseBody`, returned when the underlying `http.RoundTripper` produces
  a response with a nil `Body` in violation of its contract.
- Test suites for fuzz/property invariants, concurrent peer-state transitions,
  resource cleanup under repeated failures, real TCP/TLS/HTTP/2/proxy faults,
  external-package API contracts, frozen nginx mappings, and performance
  benchmarks.
- Scheduled mutation testing with a 100% test-efficacy gate. CI now enforces
  zero uncovered statements, runs tests in randomized order, smoke-tests fuzz
  targets and benchmarks, and covers Go 1.21 plus current Linux, macOS, and
  Windows environments.

### Changed

- Genuinely streaming request bodies are no longer eagerly buffered when the
  upstream has only one usable server, or when the default non-idempotent gate
  makes a sent `POST`, `LOCK`, or `PATCH` terminal. Pre-send connection failures
  through a standard `*http.Transport` can still fail over without losing the
  unread stream.
- `WithMaxBodyBuffer(0)` now explicitly means that every non-empty buffered body
  spills to a temporary file instead of silently restoring the 32 MB default.
- IP-Hash now hashes over the backup group after Round-Robin enters the backup
  phase, matching nginx instead of continuing to inspect the primary group.
- Upstream matching now compares scheme and host case-insensitively and ignores
  an explicit default port, so `http://API.example.com:80/` no longer bypasses
  the `http://api.example.com` upstream and goes to real DNS. Upstreams that
  differ only in this way are rejected as duplicates.
- Upstream patterns and backend addresses must be `http` or `https` origins.
  Unsupported schemes, paths, queries, fragments, user information, and missing
  hosts are rejected during construction instead of being silently dropped
  and later counted as peer failures.
- Server weights are limited to 65536, and the sum of weights in an upstream to
  16,777,216 (65,536 for consistent hashing), all checked during construction.
- A transport error or timeout now always counts as a peer failure, even when a
  custom `WithNextUpstream` condition declines to retry it. Previously such an
  error was reported as a success, so a dead peer was never quarantined and a
  recovering peer had its failure counter cleared, unlike nginx.
- Built-in pickers ignore a repeated `Done` and a `Done` for a peer they never
  selected, instead of decrementing the connection count again and driving it
  negative. `PickWithRetry` releases the peer neutrally if `tryFunc` panics.
- `Hash` and `HashConsistent` document that the key function must not read the
  request body, and `Peer` documents that a balancer takes ownership of the
  peers it is built from.
- `RRPicker.Done` now uses the peer's `Backup` flag to resolve its owning group
  directly instead of linearly scanning all primary peers on every attempt.

### Removed

- The unused exported `PeerGroup.Tries` field, before the v3 API is finalized.

### Fixed

- Request bodies are no longer closed, and temporary body files no longer
  deleted, as soon as `RoundTrip` returns. A RoundTripper may keep sending the
  body after the response headers arrive (full-duplex or early-response
  endpoints), and such uploads were aborted with `io: read/write on closed pipe`
  or truncated. The body is now released once every attempt's body has been
  closed by the underlying RoundTripper, or immediately when `RoundTrip`
  returns an error.
- Replays of a seekable or spilled request body no longer share one file
  offset. Each attempt reads at its own offset, so a retry cannot interleave
  with a superseded attempt that the RoundTripper is still writing.
- Out-of-range weights no longer panic with an integer divide by zero in
  Hash and IP-Hash selection, and can no longer overflow the Least-Conn and
  Random-Two load comparisons or make a consistent-hash ring allocate without
  bound. Groups built directly with out-of-range totals fall back to
  round-robin.
- `WithRoundTripper(nil)` and `WithNextUpstream(nil)` now fail during
  construction instead of being silently replaced by their defaults.
- Upstreams whose servers are all marked `Down()` now fail during construction
  instead of returning `ErrNoPeerAvailable` for every request. Backup-only
  upstreams remain supported.
- Consistent-hash ring deduplication now guards an empty point slice directly,
  preventing a latent slice-bounds panic if its construction invariants change.
- A response carrying a nil `Body` no longer panics the library. `http.RoundTripper`
  requires a non-nil `Body`, but a custom or test implementation may still return
  one; such a response is now rejected with `ErrNilResponseBody` at the single point
  every response passes through, so the retry path, the per-attempt `cancelCloser`,
  and the caller can all close it unconditionally. The response is rejected rather
  than normalized to `http.NoBody` so that a broken RoundTripper cannot read as a
  legitimate empty response.
- A contract violation by the configured `http.RoundTripper` — `ErrNilResponseBody`,
  `ErrInvalidRoundTrip`, or `ErrNilRoundTripper` — no longer affects upstream health.
  Previously each such error degraded the peer's failure counter and effective
  weight, so a client-side defect could mark an entirely healthy pool unhealthy.
  The peer is now released neutrally, which is distinct from reporting success:
  success would run nginx's recovery path and clear the failure counter of a peer
  that is still recovering. Retry behaviour is unchanged — whether to try the next
  peer remains the retry condition's decision, so a custom `WithNextUpstream` is
  still consulted.
- A neutral release no longer costs a peer its recovery probe. Selecting a peer
  whose `fail_timeout` has elapsed advances its `checked` timestamp, which makes
  every other request skip it until the attempt reports back. When that attempt
  turned out to say nothing about the upstream — a caller cancellation, a local
  replay error, or now a RoundTripper contract violation — the timestamp stayed
  advanced and re-quarantined the peer for another full `fail_timeout`. The
  pre-selection timestamp is now restored, so the probe passes to the next
  request. This also fixes the pre-existing caller-cancellation path, where a
  client that cancels frequently could delay a peer's recovery indefinitely.
  The rollback is conditional: if a concurrent request recorded a real failure
  on the same peer in the meantime, that newer timestamp is authoritative and is
  left in place, so a neutral release can never erase an active quarantine. The
  check uses a per-peer version counter rather than comparing timestamps, since
  two writes on a coarse-grained clock can produce the same instant.

## [v3.0.0-rc.1] - 2026-07-11

### Added

- GitHub Actions CI covering Go 1.21 and the current stable Go release, with
  unit tests, `go vet`, race detection, and an 85% coverage floor.
- `WithNonIdempotentRetries` for explicitly allowing sent `POST`, `LOCK`, and
  `PATCH` requests to retry when the caller has made replay safe, such as with
  an application-level idempotency key.
- `HashConsistent` balancer and `UpstreamConfig.HashConsistent(keyFunc)`
  option implementing ketama-style consistent hashing. Mirrors nginx's
  `hash <key> consistent`. Ring construction is byte-compatible with
  Cache::Memcached::Fast (160 virtual nodes per peer, chained
  `crc32(host \0 port + prev_hash_LE)`), so cslb shares the same ring as
  any standard ketama client configured against the same backends. On
  peer removal only ~1/N of keys remap instead of nearly all of them.

### Changed

- Bumped the Go module path to `github.com/fovecifer/cslb/v3`. Applications
  upgrading from v2 must update their imports; the README now includes a
  step-by-step migration guide and release-candidate installation command.
- Consolidated the shared smooth-weighted selection core, winner commit path,
  and explicit server-field tracking to reduce algorithm and option-state
  duplication without changing selection results.
- Deduplicated agent collaboration instructions and clarified that operational
  agent-control blocks are exempt from the project's English-only artifact
  rule.
- Default failover now matches nginx's `proxy_next_upstream error timeout`:
  transport errors retry, while HTTP status codes require
  `WithNextUpstreamCodes` or `WithNextUpstream`.
- Retry examples and documentation now make HTTP status selection,
  non-idempotent replay risk, request-body spill behavior, `ProxySSLName`, and
  the intentional client-side differences from nginx explicit.
- Clarified that IP-Hash only has a client IP in client-side usage when the
  caller provides `X-Real-IP`, `X-Forwarded-For`, or `RemoteAddr`; otherwise it
  falls back to Round-Robin, and explicit `Hash` keys are preferred for ordinary
  client affinity.
- Refactored hash, consistent-hash, IP-hash, and random fallback paths to call
  Round-Robin without relocking solely to satisfy deferred unlocks.

### Fixed

- `Transport.RoundTrip` now closes the request body on every error path that
  returns before body preparation or delegation, including pre-canceled
  requests and invalid underlying RoundTripper results.
- Sent `POST`, `LOCK`, and `PATCH` requests no longer retry by default;
  connection failures before the request is written can still fail over.
- Explicit `FailTimeout(0)` is preserved instead of being replaced by the
  10-second default.
- A true single-peer upstream now ignores failure counters and effective-weight
  degradation, while a one-primary/one-backup upstream retains normal failure
  accounting and failover behavior across all algorithms.
- Caller cancellation and overall request deadlines stop retry fan-out without
  marking healthy peers as failed.
- Configured 403 and 404 retries no longer increment peer failure counters or
  reduce effective weight.
- Explicit `MaxFails(0)` now disables temporary failure suppression while an
  omitted value continues to default to 1.
- Invalid server and transport settings, unknown algorithms, and nil options
  now surface as `NewTransportE` construction errors instead of being silently
  normalized or causing a panic.
- Plain Hash, IP-Hash, Least-Conn tie-breaking, and Random-Two selection now
  follow nginx's actual hash iteration and peer-selection behavior.
- Consistent-Hash ring points now balance across all peers with the same server
  address instead of permanently selecting the first duplicate entry.
- Requests with a caller-provided `GetBody` now close the original request body
  after the load-balanced attempt completes.
- Malformed `X-Real-IP` or `X-Forwarded-For` values no longer prevent IP-Hash
  from checking the next configured client-address source.
- Backup peers are no longer retried more than once in the same request after
  falling back from the primary peer group.
- Duplicate upstream registrations for the same scheme and host now fail fast
  during transport construction instead of silently replacing the previous
  upstream.
- `prepareBody` no longer leaks `req.Body` when the seekable fast-path's
  initial `Seek` fails, and now reconciles `req.ContentLength` with the
  bytes that will actually be sent (clamping stale full-file values,
  swapping to `http.NoBody` when the seeker is at EOF, and preserving
  caller-requested chunked framing).

## [v2.0.0] - 2026-04-22

### Changed

- Removed constructor-time `panic` paths from transport configuration.
- Added `NewTransportE` and `Transport.Err()` so invalid configuration can be
  reported as regular errors instead of crashing the caller.
- Replaced the old upstream/backend option-style configuration with explicit
  `WithUpstreams` / `Upstream` / `Server` APIs so upstream-level and
  server-level settings are separated more clearly, closer to nginx's
  `upstream { server ...; }` structure.
- Bumped the Go module path to `github.com/fovecifer/cslb/v2` for the breaking
  API release.
- Hardened public helpers and `RoundTrip` against nil inputs and invalid
  `RoundTripper` implementations that previously could trigger runtime panics.

### Removed

- Removed the old `WithUpstream` / `Backend` / `Use*` configuration entrypoints
  in favor of the explicit upstream/server API.

## [v1.1.0] - 2026-03-22

### Added

- `ProxySSLName` upstream option for TLS SNI override on backend connections.
  Equivalent to nginx's `proxy_ssl_server_name on` + `proxy_ssl_name <name>`.
  When backends are addressed by IP, this overrides the SNI to the specified
  hostname so that SNI-based routing and certificate verification work correctly.

## [v1.0.0] - 2026-03-18

### Added

- Core `Transport` implementing `http.RoundTripper` with client-side load balancing.
- Six load balancing algorithms mirroring nginx: Round-Robin, Least-Conn, IP-Hash, Hash, Random, Random-Two.
- Automatic request retry with configurable `WithNextUpstream` / `WithNextUpstreamCodes`.
- Per-attempt timeout via `WithTimeout`.
- Request body buffering and replay on retry (memory + temp file spill).
- Mixed HTTP/HTTPS backend support with per-backend scheme rewriting.
- Backend options: `Weight`, `MaxFails`, `FailTimeout`, `MaxConns`, `AsBackup`, `AsDown`.
- Standalone `PickOne` and `PickWithRetry` helpers for non-HTTP use cases.

[Unreleased]: https://github.com/fovecifer/cslb/compare/v3.0.0-rc.1...HEAD
[v3.0.0-rc.1]: https://github.com/fovecifer/cslb/compare/v2.0.0...v3.0.0-rc.1
[v2.0.0]: https://github.com/fovecifer/cslb/compare/v1.1.0...v2.0.0
[v1.1.0]: https://github.com/fovecifer/cslb/compare/v1.0.0...v1.1.0
[v1.0.0]: https://github.com/fovecifer/cslb/releases/tag/v1.0.0
