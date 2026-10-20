---
title: LocalAPI Client Socket Leak - Plan
type: fix
date: 2026-10-20
artifact_contract: ce-unified-plan/v1
product_contract_source: ce-plan-bootstrap
execution: code
---

# LocalAPI Client Socket Leak - Plan

## Goal Capsule

- Objective: A Mac running tincan agents keeps working network access no matter how long the relay stays unreachable. Tincan holds at most a handful of connections to the Tailscale app, not one more for every relay search.
- Means: reuse one `local.Client` per tailscaled socket in `localAPINetmap` instead of building a new one per call (KTD1).
- Authority: Product Contract requirements win on behavior. KTDs win on mechanism. The tailscale.com v1.102.4 source in the module cache wins over comments in this repo about how `local.Client` behaves.
- Stop conditions: stop and report if a reused `local.Client` stops reaching LocalAPI after the Tailscale app restarts on a new port, or if the existing per-socket tests (userspace tailscaled, `TS_SOCKET`) need their meaning changed to pass.
- Execution profile: one Go package, `internal/client`, plus tests. No relay, wire, or config change.
- Finishing: ce-work implements and verifies locally, then opens one PR against `main`.

---

## Product Contract

### Summary

`localAPINetmap` stops creating a fresh Tailscale LocalAPI client on every relay search. It keeps one client per socket for the life of the process, so each search reuses the same connection pool. A regression test proves that many searches against a fake LocalAPI open at most one connection.

### Problem Frame

On 2026-10-20 a Mac running 15 long-lived tincan processes (`tincan mcp`, `tincan mcp --channel`, `tincan listen`) had 16,260 ESTABLISHED connections to `127.0.0.1:57385`, the Tailscale macOS app's LocalAPI port. That is nearly the whole ephemeral range (49152-65535, 16,384 ports). Every other app on the machine then failed to connect intermittently with `EADDRNOTAVAIL` / `ECONNREFUSED` for about two days. Killing the tincan processes freed the ports once TIME_WAIT drained.

The cause is in `internal/client/discover.go`, `localAPINetmap`: each call builds `&local.Client{}` per socket. In tailscale.com v1.102.4, `local.Client.DoLocalRequest` lazily builds a private `http.Client` whose `http.Transport` has keep-alives on and no idle timeout. When `Status` returns, its connection goes back to that transport's idle pool. Nothing uses the client again, and the idle connection stays open for the life of the process.

`localAPINetmap` runs on every relay search (`relocate` -> `startFind` -> `follow`). A search starts only after a relay call fails as unreachable, and at most once per `findEvery` (30s) per relay client, so two leaked connections a minute per process is a ceiling, not a steady rate. How close a process gets to it depends on how often it calls the relay. The owner's relay at `100.73.62.112:8787` has been unreachable since at least 2026-10-13 (noted in `docs/plans/2026-10-13-1404-fix-dots-send-home-redirect-plan.md`), and processes started and exited over that week, so the leak piled up over days until the ports ran out.

### Requirements

- R1. Once relay searches finish, a process keeps at most one idle connection pool per tailscaled LocalAPI socket, no matter how many searches have run. Requests in flight at the same moment may each hold a connection while they run.
- R2. Each socket `tailscaledSockets` returns (the macOS/Windows default `""`, `TS_SOCKET`, the Linux default, every userspace socket) still gets its own client with its own `Socket` / `UseSocketOnly` settings.
- R3. After the Tailscale macOS app restarts on a new LocalAPI port, the next search still reaches it without restarting tincan.
- R4. Concurrent searches across sockets stay concurrent and race-free.

### Scope Boundaries

- The relay-unreachable condition itself (this Mac off the tailnet, relay at `100.73.62.112` down) is an ops problem, not part of this PR.
- `findEvery` and the search cadence stay as they are. With the leak fixed, a 30s search is cheap.
- `identity.NewLocalResolverAt` and `tsnet` resolvers in `internal/cli/relay.go` build their client once at startup and are left alone.
- The audit found no other per-call `http.Transport` in long-running loops: `New`, `NewRelaySocket`, `wakeHTTPClient`, and the `wake` fallbacks each build once per client or process. Not changed.
- Considered and not built: evicting cached clients for sockets that disappear. A host has one to a few tailscaled sockets, so the cache cannot grow in a way that matters. Revisit only if socket paths start including per-run random names.
- Considered and not built: a socket-count self-check or log warning. A correct pool makes it unnecessary, and the regression test catches a reintroduction.

---

## Planning Contract

### Key Technical Decisions

- KTD1. Cache one `*local.Client` per socket string at package level, created lazily under a mutex. This bounds connections to one keep-alive pool per socket (R1, R2). It is safe across Tailscale restarts because `local.Client.defaultDialer` looks up the macOS port with `safesocket.LocalTCPPortAndToken` on every dial, and `DoLocalRequest` reads the token on every request (R3). A dead idle connection fails and `http.Transport` redials for the idempotent GET.
- KTD2. Rejected: giving each fresh client a `Transport` with `DisableKeepAlives`. `local.Client`'s dialer and auth are unexported, so tincan would have to copy them, and every search would still open and close one connection. `local.Client` also has no `CloseIdleConnections`, so closing the pool after each call is not possible without the same copying.
- KTD3. Build clients through a package-level constructor variable (for example a `newLocalClient(sock)` seam beside `localStatus`), so a test can point the client's exported `Dial` at a fake LocalAPI and set `OmitAuth`. The existing `localStatus` seam and its tests keep their signature and meaning.
- KTD4. Tests reset the cache through a test-only helper registered with `t.Cleanup`, matching how `swapLocalStatus` restores `goos`, `localStatus`, `procRoot`, and `userHome`.

### Assumptions

- tincan stays on tailscale.com v1.102.x, where `local.Client` keeps the lazy per-client `http.Client` described above. A future version that closes idle connections itself would make KTD1 unnecessary but still harmless.

---

## Implementation Units

### U1. Reuse one LocalAPI client per socket

- Goal: `localAPINetmap` reuses a cached `local.Client` per socket, so relay searches stop leaking LocalAPI connections.
- Requirements: R1, R2, R3, R4
- Dependencies: none
- Files:
  - `internal/client/discover.go`
  - `internal/client/discover_test.go`
- Approach:
  1. Add the per-socket client cache and constructor seam (KTD1, KTD3) next to `localStatus`, with a comment saying why the client must outlive the call: its private transport keeps idle connections open forever.
  2. In `localAPINetmap`, replace `&local.Client{}` with a cache lookup. Keep the `Socket` / `UseSocketOnly` assignment inside the constructor so every cached client is configured exactly as today.
  3. Leave the per-socket goroutines, the 5s deadline, and result merging unchanged.
  4. Add the cache-reset helper for tests (KTD4) and call it from `swapLocalStatus` and the other tests that replace `localStatus`, so no test sees a client cached by another.
- Patterns to follow: the `localStatus`, `lookupLocalNetmap`, `goos`, and `procRoot` test seams in `internal/client/discover.go`, and the save-and-restore style of `swapLocalStatus` in `internal/client/discover_test.go`.
- Execution note: on `main` the client is built inline, so a test cannot reach it yet. First add only the constructor seam (KTD3), still called once per call so behavior is unchanged. Write the connection-count test against it and watch it record 50 connections, then add the cache (KTD1) and watch the same test record 1.
- Test scenarios:
  - Happy path: with `goos = "darwin"`, `TS_SOCKET` empty, and the constructor seam returning a client whose `Dial` reaches an `httptest` server serving `/localapi/v0/status` with a small `ipnstate.Status` JSON body, 50 sequential `localAPINetmap` calls all return the expected IPv4s and the fake server accepts exactly 1 connection, counted with `ConnState` `StateNew` or a counting dialer.
  - Regression proof: the same test against the pre-change code accepts 50 connections and leaves them open. This is the test-first failure in the execution note.
  - Per-socket identity: on `linux` with two userspace sockets (the setup in the existing userspace-socket tests near the `localAPINetmap` tests), each socket's client carries its own `Socket` with `UseSocketOnly` true, and repeated calls return the same client per socket and a different one across sockets.
  - Concurrency: 20 goroutines calling `localAPINetmap` at once against the fake server under `go test -race` report no race, and the server accepts no more than the number of calls in flight together, then drops back to reuse.
  - Dead idle connection: when the fake server is closed and a new one started at a different address, with the counting `Dial` reading the current address at dial time, the next `localAPINetmap` call succeeds on the cached client. This proves the cache does not pin a dead connection or address. It does not exercise `defaultDialer`'s port lookup, so R3 against the real app is proven by the live restart gate in the Verification Contract.
  - Existing coverage: `TestLocalAPINetmapFailsFastWithMissingTSSocket`, `TestLocalAPINetmapFailsFastWithoutLinuxSocket`, `TestLocalAPINetmapOnDarwinDoesNotNeedSocketFile`, and `TestRelayMovedFollowsLocalAPIAddresses` pass unchanged.
- Verification: the new tests pass, the full `internal/client` package passes under `-race`, and a rebuilt `tincan mcp` left running on a Mac with an unreachable relay holds a flat count of connections to the Tailscale LocalAPI port across several `findEvery` cycles.

---

## Verification Contract

| Gate | Command | Proves |
|---|---|---|
| Go tests with race detector | `go test -race ./...` (what CI runs) | U1 scenarios, no regressions |
| Go vet | `go vet ./...` | build hygiene |
| Lint | `make lint` | repo lint rules |
| Live soak on macOS | build `tincan`, run one `tincan mcp` with the relay unreachable for 3+ minutes, sample `lsof -nP -iTCP -a -p <pid>` once a minute | R1 holds against the real Tailscale app: connection count to the LocalAPI port stays at 0-1 instead of rising about 2 per minute |
| Live Tailscale restart on macOS | during the soak, quit and reopen the Tailscale app so LocalAPI comes back on a new port, then wait one `findEvery` cycle | R3: the same tincan process reaches the new port (a fresh connection appears to the new LocalAPI port, no LocalAPI errors in the search log) without a restart |

## Definition of Done

- U1's test scenarios exist and pass under the Verification Contract commands, and the connection-count test was seen recording one connection per call with the seam alone, before the cache landed.
- `localAPINetmap` no longer constructs a `local.Client` per call.
- The live soak keeps the LocalAPI connection count at 0-1 across the run, and the live Tailscale restart gate passes.
- No leftover probes, debug logging, or abandoned-approach code in the diff.
- The PR body states the symptom (machine-wide port exhaustion), the root cause, and the soak result, and ticks the template's human-review box.

---

## Appendix

Evidence from 2026-10-20 on the affected Mac:

| Observation | Value |
|---|---|
| Ephemeral port range | 49152-65535 (16,384) |
| ESTABLISHED TCP sockets | 32,624 (both ends local) |
| tincan-owned sockets to 127.0.0.1:57385 | 16,260 across 15 processes, 494-1,756 each |
| Listener on 57385 | `io.tailscale.ipn` (Tailscale macOS app LocalAPI) |
| Raw connect error | `connectx ... failed: Can't assign requested address` |
| After killing tincan | 16,284 TIME_WAIT, cleared after about 25s; connectivity restored |

Source reference: `tailscale.com@v1.102.4/client/local/local.go`, `DoLocalRequest` (lazy `tsClient` with `&http.Transport{DialContext: lc.dialer()}`) and `defaultDialer` (per-dial `safesocket.LocalTCPPortAndToken`).
