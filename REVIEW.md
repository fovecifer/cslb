# Code Review — cslb v3 (commit b3d99e4)

Review date: 2026-09-25. Scope: the whole repository (`*.go`, README, CHANGELOG,
CI workflow). No production code was modified. Severity labels follow the
requested scale: **严重 (Critical)**, **一般 (Major)**, **建议 (Suggestion)**.

Reproduction tests for the confirmed findings live in
[`review_findings_test.go`](./review_findings_test.go). They are behind the
`review` build tag so the regular suite and CI stay green; they are expected to
**fail** until the findings are fixed:

```bash
go test -tags review -run '^TestReview' -v ./...
```

## 1. Automated checks

| Check | Result |
|-------|--------|
| `go build ./...` (Go 1.24.7) | OK |
| `go vet ./...` | OK, no findings |
| `go vet -tags review ./...` | OK |
| `go test -race -count=1 ./...` | OK (`ok github.com/fovecifer/cslb/v3 2.174s`) |
| `gofmt -l .` | clean |
| `go test -tags review -run '^TestReview'` | 5 tests FAIL, as intended (see below) |

The existing suite is extensive (fuzz, state machine, nginx golden tests,
protocol integration), and the tooling found nothing. All findings below come
from reading the code and are backed by the reproduction tests where marked.

## 2. Design understanding

`cslb` is an `http.RoundTripper` that maps a `scheme+host` to an upstream and
reproduces nginx's upstream model in three layers: `Balancer`
(init_upstream) → `Picker` (init_peer) → `Pick`/`Done` (get/free). Every
algorithm embeds `RRPicker` and falls back to it, as nginx's modules do.
`Transport.RoundTrip` makes the body replayable when a retry is possible,
loops Pick → rewrite → send → classify, and keeps retry decisions and peer
health accounting separate. It also adds a "neutral release" for outcomes that
say nothing about the upstream, such as caller cancellation or RoundTripper
contract violations.

The algorithm layer is careful and closely follows nginx. The significant
problems are in `transport.go`, in how request-body lifetime is handled.

## 3. Findings summary

| ID | Severity | Location | Title | Status |
|----|----------|----------|-------|--------|
| S1 | 严重 | `transport.go:772-774`, `1076-1077`, `1123-1129`, `1141-1143`, `1188-1192` | Request body closed/deleted when `RoundTrip` returns, while the transport may still be sending it | **Confirmed** (2 tests) |
| S2 | 严重 (推测 / speculative) | `transport.go:1123-1129`, `1182-1187` | Seekable/spilled body shares one file offset across attempts; a previous attempt's writer can corrupt the retry's body | Speculative, not reproduced |
| G1 | 一般 | `transport.go:944-946` | Transport error that is not retried is reported to the balancer as a **success** | **Confirmed** |
| G2 | 一般 | `transport.go:539`, `hash.go:86`, `iphash.go:102`, `chash.go:144/159`, `leastconn.go:80` | No upper bound on `Weight`: divide-by-zero panic in `RoundTrip`, unbounded allocation, integer overflow | **Confirmed** (panic) |
| G3 | 一般 | `transport.go:554-575` | Backend addresses are barely validated; bad addresses surface per request and penalize peer health | **Confirmed** |
| G4 | 一般 | `transport.go:36-45`, `307-312` | `Transport` does not implement `CloseIdleConnections`; cloned per-upstream transports can never be drained | By inspection |
| A1 | 建议 | `transport.go:753-757` | Upstream matching is exact on `URL.Host` (case and default port) and silently falls through to real DNS | By inspection |
| A2 | 建议 | `balancer.go:162-168`, `roundrobin.go:212-281` | `Done` is not idempotent; double `Done` drives `conns` negative and bypasses `max_conns` | By inspection |
| A3 | 建议 | `balancer.go:42-69`, `balancer.go:103-124` | Exported mutable `Peer` fields and `BuildGroups` re-initializing shared peers invite data races | By inspection |
| A4 | 建议 | `transport.go:1132-1161` | Multi-peer non-POST streaming bodies are fully buffered before the first byte is sent (breaks full-duplex) | Documented behaviour, by inspection |
| A5 | 建议 | `transport.go:1047-1067` | The hash key function sees the original request, including an unconsumed body | By inspection |

---

## 4. Details

### S1 — 严重: request body is closed when `RoundTrip` returns, not when the transport is done with it

**Where**

- `transport.go:772-774`: `defer cleanup()` runs on every return from `RoundTrip`.
- Streaming path, `transport.go:1141-1143`: the caller's body is wrapped in
  `io.NopCloser`, and the real body is closed by `cleanup`.
- Spill path, `transport.go:1188-1192`: `cleanup` closes **and deletes** the temp file.
- Seekable path, `transport.go:1123-1129`: `cleanup` closes the caller's seeker
  (for example an `*os.File`).

**Problem.** The `http.RoundTripper` contract lets a transport keep reading
`Request.Body` after `RoundTrip` returns. The transport closes the body when it
has finished with it. `net/http` does this whenever the response headers arrive
before the upload completes. Typical cases are HTTP/2 bidirectional streams,
full-duplex HTTP/1 handlers, early `200`/`413` responses, and upload-progress
endpoints. cslb hides the transport's `Close` behind `NopCloser` and closes the
real body itself in a `defer`. As a result, it closes the body while
`http.Transport`'s write loop is still streaming it. The upload is aborted:
pipe writers get `io: read/write on closed pipe`, and the server sees a
truncated body or a reset.

This is a silent regression compared with using `*http.Transport` directly, and
it affects all three non-`GetBody` body paths. The in-memory path is not
affected because it hands out fresh `bytes.Reader`s.

**Proof** (`review_findings_test.go`):

- `TestReviewStreamingBodyClosedWhenRoundTripReturns`: a single backend, an
  `io.Pipe` body, and a server that flushes headers before it reads the body.
  The baseline subtest with a plain `*http.Transport` **passes**. The cslb
  subtest **fails** with `upload aborted after RoundTrip returned: io:
  read/write on closed pipe`.
- `TestReviewSpilledBodyClosedWhenRoundTripReturns`: two peers,
  `WithMaxBodyBuffer(0)`, and an 8 MiB `PUT`. The server receives nothing
  (`server received "", want 8388608 bytes`).

**Fix direction.** Tie cleanup to the *last* attempt's body lifetime instead of
`RoundTrip`'s return. For example, hand each attempt a body wrapper whose
`Close` is reference counted, and run `cleanup` when the final attempt's
transport closes it. Alternatively, run `cleanup` from the returned
response body's `Close` **and** from the attempt body's `Close`, whichever
comes last. Stop wrapping the streaming body in `NopCloser` for the final
attempt.

### S2 — 严重 (推测 / speculative): one shared file offset across attempts

**Where.** `transport.go:1123-1129` (seekable) and `1182-1187` (spill). Every
attempt's `GetBody` returns `io.NopCloser` over the **same** seeker and file
offset.

**Problem (hypothesis).** This follows from S1. After an attempt that got a
retryable status (such as a configured `502`) or a transport error such as the
per-attempt timeout, `http.Transport` may still be reading the previous
attempt's body. The next attempt seeks the same file to `startPos` and starts
reading. The two readers then interleave on one offset, so the retry can send a
body with missing or duplicated chunks. `Content-Length` would be wrong only if
the length changed, so the corruption could go undetected. The in-memory path is
immune.

**Status.** Not reproduced deterministically, because the race window depends
on socket buffering. It is marked **推测 (speculative)**. It is the same root
cause as S1, and the S1 fix should also give each attempt an independent
reader: `io.NewSectionReader` over the file instead of a shared `Seek`.

### G1 — 一般: a transport error that is not retried counts as a peer success

**Where.** `transport.go:944-946`

```go
if !t.shouldRetry(resp, err) {
    return attemptDecision{neutral: neutral}   // failed == false
}
```

**Problem.** When a custom `WithNextUpstream` condition returns `false` for a
transport error (the equivalent of `proxy_next_upstream off`), the peer is
released with `Done(peer, false)`. That is the **success** path, and it even
clears `fails` for a recovering peer (`roundrobin.go:275-277`). nginx calls
`peer.free(..., NGX_PEER_FAILED)` in `ngx_http_upstream_next` for every
error and timeout, whether or not `next_upstream` allows moving on. As a
result, a dead peer is never quarantined in this configuration and keeps its
share of traffic. This contradicts the README's statement that retry
selection and peer health are separate.

**Proof.** `TestReviewNonRetriedTransportErrorCountsAsSuccess` configures the
dead peer with `max_fails=1` and `fail_timeout=1h` and sends 6 requests. The
dead peer receives 3 of them instead of 1: `dead:1,live:1,dead:1,live:1,dead:1,live:1`.

**Fix direction.** When `err != nil` and the error is not a contract error or
caller cancellation, set `failed: true` even when `retry` is false.

### G2 — 一般: unbounded `Weight` causes a panic in `RoundTrip` and unbounded allocation

**Where.** Validation only rejects `Weight <= 0` (`transport.go:539`).

- `hash.go:86` / `iphash.go:102`: `p.hash % uint32(group.TotalWeight)`. When
  `TotalWeight` is a multiple of 2³², the modulus truncates to 0 and the code
  panics with **integer divide by zero** inside `RoundTrip`. When it is
  above 2³² but not a multiple, the distribution is silently skewed.
- `chash.go:144,159`: `Weight * 160` ring points. A weight in the millions or
  above makes `NewTransportE` allocate gigabytes or overflow `int`.
- `leastconn.go:80,83,100` and `random.go:158`: `conns * Weight` can overflow.

**Proof.** `TestReviewHugeWeightsPanicInsteadOfValidationError` configures two
servers with `Weight(1<<31)` and `.Hash(...)`. `NewTransportE` accepts them,
and `RoundTrip` panics with `runtime error: integer divide by zero`. The
project rule is to never panic in library code and to fail fast.

**Fix direction.** Cap the weight per server (for example `≤ 1<<20`, well above
any realistic value) and the total weight, and return a construction error.
This is cheap and removes all of the overflow sites at once.

### G3 — 一般: backend addresses are barely validated

**Where.** `transport.go:554-575`. An address without `://` is used verbatim
as `URL.Host`. An address with `://` is only checked for a non-empty scheme and
host.

**Problem.** `ftp://…`, `http://host:port/api` (the path is silently dropped),
`10.0.0.1:8080/api` (the whole string becomes the Host), and userinfo are all
accepted. The mistake only shows up per request as a transport error, which
`decideAttempt` counts as a **peer failure** (degrading `effectiveWeight` and
tripping `max_fails`). A configuration error therefore looks like an unhealthy
backend. The same applies to upstream patterns with a path
(`transport.go:631`), which is silently ignored.

**Proof.** `TestReviewInvalidBackendAddressesAccepted` fails for all four
addresses.

**Fix direction.** Allow only the `http` and `https` schemes. Reject a non-empty
path (other than `/`), a query, a fragment, and userinfo. Validate
`host[:port]` with `net.SplitHostPort` or `url.Parse("//"+addr)`.

### G4 — 一般: no `CloseIdleConnections`

**Where.** `Transport` (`transport.go:36-45`). Per-upstream clones are created
at `transport.go:307-312`.

**Problem.** `http.Client.CloseIdleConnections()` only forwards to transports
that implement `CloseIdleConnections()`. cslb doesn't implement it, so idle
connections of the default transport are unreachable. The cloned per-upstream
`*http.Transport`s created for `ProxySSLName` are private and cannot be drained
at all, even though they hold keep-alive connections and goroutines. This
matters for long-lived processes that recreate clients, and in tests
(`goleak`).

**Fix direction.** Implement `CloseIdleConnections()` so it forwards to
`t.transport` and to every `ups.transport` whose type implements it.

### A1 — 建议: exact `URL.Host` matching

`transport.go:753-757` looks up `{scheme, URL.Host}` literally.
`http://API.example.com/` and `http://api.example.com:80/` do not match
`Upstream("http://api.example.com")`. They pass through **unbalanced to real
DNS**, which can send traffic to the wrong place without any error. Consider
normalizing the host to lowercase and removing the default port at
registration and lookup. At minimum, document that matching is exact.

### A2 — 建议: `Done` is not idempotent

`PickResult.Done` documents "must be called exactly once" (`balancer.go:162`),
but nothing enforces it. A second `Done`, or a `Done` for a peer this picker
never selected, decrements `conns` again (`roundrobin.go:280`). This makes
`conns` negative, silently disables `max_conns`, and skews LeastConn/RandomTwo
toward that peer. `PickWithRetry` also leaks `conns` if `tryFunc` panics.
Consider having `RRPicker` track outstanding peers (it already has `tried`)
and ignore unmatched `Done` calls.

### A3 — 建议: exported mutable `Peer` fields and shared-peer re-initialization

`Peer.Down`, `Weight`, `MaxFails` and the other exported fields are read under
`PeerGroup.mu` during selection, but callers can write them without any lock.
The public API has no locked setter, so a runtime "mark down" is a data race.
Separately, `BuildGroups` calls `p.init()` on caller-owned peers
(`balancer.go:111`). Passing the same `[]*Peer` to two balancers resets and
then shares mutable state under two different mutexes. Consider copying the
peers in `BuildGroups`, or documenting ownership transfer, and adding a
`SetDown(bool)` that takes the group lock.

### A4 — 建议: full buffering before the first byte for multi-peer idempotent requests

With two or more usable peers and a non-POST method, `prepareBody` reads the
whole body, up to EOF, into memory or a temp file **before** the first attempt
(`transport.go:1146-1161`). A streaming `PUT` (a pipe fed by a producer that
waits for the response) deadlocks, and large uploads get the first-byte latency
of a full local copy. The CHANGELOG documents this. Consider an opt-out, such as
"no body replay" per upstream or per request context, or replay only when
`GetBody` is present.

### A5 — 建议: hash key function sees the original request

`newPicker` calls `ups.hashFunc(baseReq)` (`transport.go:1059,1063`) before
`prepareBody`. A key function that reads the body, for example to hash a JSON
field, consumes the stream that is about to be sent. Document that the key
function must not read `Body`, or pass it a request whose body has been made
replayable.

## 5. Areas reviewed with no findings

- Smooth weighted round-robin, the effective-weight decay/recovery, and the
  `checked`/`checkedSeq` probe rollback for neutral releases
  (`roundrobin.go`) are correct and faithful to nginx.
- The LeastConn two-pass tie-break is equivalent to nginx's scan from `best`.
- Ketama ring construction and deduplication (`chash.go`), and CRC32 rehash
  prefixing (`hash.go`), match nginx and Cache::Memcached.
- The retry loop correctly closes superseded response bodies, keeps the
  per-attempt `cancelCloser` on the returned response, and does not penalize
  peers on caller cancellation. The non-idempotent gate matches nginx for both
  error and status retries.
- The `Transport` is immutable after construction, and the race detector is
  clean across the existing concurrency tests.
