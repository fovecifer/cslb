# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

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
