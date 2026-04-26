# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- `HashConsistent` balancer and `UpstreamConfig.HashConsistent(keyFunc)`
  option implementing ketama-style consistent hashing. Mirrors nginx's
  `hash <key> consistent`. Ring construction is byte-compatible with
  Cache::Memcached::Fast (160 virtual nodes per peer, chained
  `crc32(host \0 port + prev_hash_LE)`), so cslb shares the same ring as
  any standard ketama client configured against the same backends. On
  peer removal only ~1/N of keys remap instead of nearly all of them.

### Fixed

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

[Unreleased]: https://github.com/fovecifer/cslb/compare/v2.0.0...HEAD
[v2.0.0]: https://github.com/fovecifer/cslb/compare/v1.1.0...v2.0.0
[v1.1.0]: https://github.com/fovecifer/cslb/compare/v1.0.0...v1.1.0
[v1.0.0]: https://github.com/fovecifer/cslb/releases/tag/v1.0.0
