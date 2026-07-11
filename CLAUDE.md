# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Rules

- Always use English for all generated code, comments, commit messages, and documentation in this project.
- Never use `panic` in library code; return errors instead.
- Always update CHANGELOG.md when releasing a new version.
- Follow the **fail-fast** strategy: validate inputs and preconditions at the earliest possible point (constructors, option functions, public API boundaries) and return an error immediately rather than deferring failure to a later call. Never silently ignore errors (`_ = err`) — either handle them or propagate them to the caller.

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
- **`HashConsistent`** (`chash.go`) — ketama-compatible consistent hashing with 160 points per unit of peer weight.
- **`Random`** / **`RandomTwo`** (`random.go`) — weighted random and Power of Two Choices.

### Request flow in `Transport.RoundTrip`

1. Match request `scheme+host` against registered upstreams; pass through unchanged if no match.
2. Buffer request body via `prepareBody()` so it can be replayed on retries.
3. Loop: `Pick()` a peer → rewrite URL → send → call `shouldRetry()` → call `Done()`.
4. On retry, close the failed response body and loop. When no peers remain, forward to the underlying transport directly.

### Options pattern

Public configuration uses transport-level options plus an explicit nginx-style
hierarchy: `WithUpstreams(...)` with `Upstream(...)` and `Server(...)`.
All upstreams are normalized through the same registration path in
`NewTransport`, which also clones per-upstream `*http.Transport` instances for
upstreams that use `ProxySSLName`.

## 与另一个编码 Agent 协作

本机的同一个 tmux 会话中还运行着另一个交互式编码 Agent(Claude Code 或 Codex,
环境变量 `AGENT_NAME` 标识了你自己的身份)。你可以通过 `peer` 命令与它交互:

- `peer peek [行数]` — 查看对方终端最近的输出(默认 80 行)
- `peer tell "消息"` — 发送单行消息到对方输入框并回车,效果等同于用户直接对它说话
- 多行/含特殊字符的指令请走 stdin(经 tmux buffer + bracketed paste,无需任何转义):

  ```bash
  peer tell <<'EOF'
  这里是完整的多行指令,可包含 `反引号`、引号和换行
  EOF
  ```
- `peer wait [超时秒]` — 阻塞等待,直到对方屏幕输出稳定(视为它已完成当前任务)
- `peer esc` — 向对方发送 Escape,打断它正在进行的生成
- `peer status` — 查看双方身份与窗口状态

### 使用规则

1. **仅在用户明确要求时**才向对方发送指令(`peer tell` / `peer esc`);
   `peer peek` 用于查看状态,可以在用户询问对方进展时主动使用。
2. 典型流程:`peer tell "..."` → `peer wait` → `peer peek 120`,
   然后把对方回复的要点**转述给用户**,不要只说"已发送"。
3. 对方屏幕是 TUI 界面,`peek` 抓到的文本可能含边框、状态栏等噪音,自行过滤。
4. 不要与对方进入无人监督的自动循环对话;每轮交互都应源自用户的指令。
5. 如果对方处于权限确认/弹窗状态(peek 可以看出来),如实告知用户,由用户决定,
   不要替用户按下确认键。
