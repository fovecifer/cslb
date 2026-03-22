# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

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

[v1.1.0]: https://github.com/fovecifer/cslb/compare/v1.0.0...v1.1.0
[v1.0.0]: https://github.com/fovecifer/cslb/releases/tag/v1.0.0
