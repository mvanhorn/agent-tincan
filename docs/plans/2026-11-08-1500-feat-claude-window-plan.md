---
title: "feat: open a new Claude Code window in cmux for each teammate ask"
type: feat
date: 2026-11-08
artifact_contract: ce-unified-plan/v1
product_contract_source: ce-plan-bootstrap
execution: code
---

# feat: open a new Claude Code window in cmux for each teammate ask

## Goal Capsule

- Objective: from any teammate (phone, web agent, another Claude), the owner can say "open Claude Code on my Mac and do X", and a new Claude Code window appears in their main terminal (cmux), already working on X, with the answer going back to whoever asked.
- Means: a built-in `tincan window` command group. It reuses the per-request listener's claim, renew and fail machinery, and opens each request in a new background cmux workspace. This is the same idea as the agentmail-to-claude-code daemon (write the task to a file, open the terminal, hand Claude a one-line pointer) with no email and no script to copy.
- Authority: this plan, then the repo's CONTRIBUTING.md and existing listen/worker conventions.
- Stop conditions: stop and report if cmux cannot open a workspace, read its screen, and type into it from a process started outside cmux.

## Product Contract

### Summary

Add `tincan window`: `open` starts a Claude Code window in cmux with a task, `serve` turns each ask to a dedicated agent into its own window, and `install` keeps `serve` running in the background under launchd.

### Problem Frame

Today the only ways to get a fresh, visible Claude Code session going on the Mac from elsewhere are the separate agentmail-to-claude-code email daemon and `examples/claude-code/cmux-wake.sh`. That script has to be copied by hand, types into a surface after guessing when Claude is ready, and only tells a generic new session to drain the inbox. The request is never tied to the window, so the asker cannot tell which window did the work, and nobody fails the request if the window dies. The owner wants tincan to do this by itself: ask, a window opens, the work is visible and can be taken over, and the answer comes back on the same request.

### Requirements

- R1. `tincan window open <task>` opens a new Claude Code window in cmux, running Claude with the task, and returns without waiting.
- R2. `tincan window serve` runs as its own tincan agent (for example `claude-window`) and opens one new window for each ask addressed to that agent. Pings are answered, and notifies and replies are left alone, as `listen --per-request` already does.
- R3. The window opened for an ask owns that one request. Its answer goes back on the same request id, from the agent the ask was sent to.
- R4. A request whose window is closed before it replies is failed with a clear message instead of waiting silently. A Claude that exits in a still-open window is covered by `--timeout`.
- R5. Windows open without taking focus.
- R6. Windows start Claude with the owner's normal permission settings, never `--dangerously-skip-permissions`.
- R7. An ask that a guest's ask led to (`TINCAN_FROM_GUEST=1`) never opens a window. It is failed with a message that says why.
- R8. A teammate never chooses the window's working directory. The window always opens in the directory the owner configured.
- R9. The terminal is a setting (`--terminal`), with `cmux` as the only value this change implements.
- R10. `tincan window install` writes a LaunchAgent that keeps `serve` running, and with `--start` also loads it, so nothing needs to be started by hand after setup.
- R11. The window's own tincan MCP server keeps running as the owner's normal Claude Code agent, so the window's other tincan tools work as in any other window.

### Success Criteria

- From a phone or web teammate, `ask claude-window "summarize the README of agent-tincan"` makes a new cmux workspace appear in the background with Claude working on it, and the asker gets the answer on the same request.
- Closing that workspace before Claude replies fails the request within seconds, with a message that says the window was closed.

### Scope Boundaries

- No email trigger. agentmail-to-claude-code stays a separate project.
- No terminals other than cmux. The `--terminal` setting leaves room for an AppleScript backend (Ghostty or Terminal.app) later.
- No teammate-chosen directories or project allowlist. Requests to the window agent with a `project` label are not routed specially.
- No reuse of an existing conversation. `examples/claude-code/session-route.py` covers that.
- No Windows or Linux support. `serve` refuses on non-macOS, like other darwin-only paths.

#### Deferred to Follow-Up Work

- A project allowlist (label to directory) so a teammate can pick one of the owner's repos by name.
- An AppleScript backend for Ghostty or Terminal.app.
- Retiring `examples/claude-code/cmux-wake.sh` once `tincan window` has been used for a while.

## Planning Contract

### Key Technical Decisions

- KTD1. The window is a per-request worker. `serve` calls the existing `listenPerRequest` with an exec command that runs `tincan window open --request`. That process opens the window, then waits until the request closes or the window goes away. This reuses claim renewal, timeouts, the guest mark, `TINCAN_SESSION` and fail-on-exit without touching the listener loop. Rejected: a new listener loop, which would duplicate subtle claim and lease code (`keepWorkerClaim`, `postWorkerFailed`).
- KTD2. A dedicated agent identity, joined with its own config (default `~/.config/tincan/window.json`, the same pattern as `notes.json`). Rejected: running `serve` as the owner's `claude-code` agent, where `--per-request` would take asks meant for already-open windows that channels and socket wake deliver.
- KTD3. The window replies through the CLI under the worker's session, and that session is never exported into the window. The task file gives Claude the exact `TINCAN_CONFIG=… TINCAN_SESSION=… tincan reply <id> …` command. The window's own `tincan mcp` then runs as the normal `claude-code` agent (R11) and never takes new `claude-window` asks through check_inbox. Rejected: exporting `TINCAN_CONFIG` and `TINCAN_SESSION` into the window, which would make its MCP server run as the window agent and race the listener for new asks.
- KTD4. cmux `workspace create --name … --cwd <dir> --focus false`, then read the new workspace's screen to see what it started, then `send` one line and `send-key enter`. A probe on the owner's Mac showed that cmux's `--command` (and a layout surface's `command`) is typed into whatever the terminal starts rather than exec'd, and the owner's Ghostty `command` starts Claude Code, so the typed text lands in Claude's input box. So tincan detects Claude (banner plus the `❯` input mark, the same in every permission mode) or a settled shell, and types the pointer or a quoted `claude '<pointer>'` line. `--starts claude|shell` skips the guess.
- KTD5. Each request gets a private directory (0700) `window-runs/<request id>/` beside the window agent's config, holding `task.md` (0600). The path has no spaces, so the one-line pointer stays simple. The window counts as gone when `cmux read-screen` answers "not found"; tincan does not own the window's processes, so it does not track Claude's exit.
- KTD6. `serve` defaults `--timeout` to 12h and `--max-windows` to 3, because interactive windows run much longer than headless workers and an ask burst should not flood the screen.
- KTD7. `claude` and `cmux` are resolved to absolute paths when `serve`/`install` runs (`--claude`, `--cmux`, `CMUX_BIN`, then `/Applications/cmux.app/Contents/Resources/bin/cmux`, then PATH), because launchd's PATH does not include them.
- KTD8. The opener exits 0 once the request is terminal or needs_input. When the window asks a clarifying question, the listener's existing `workerAsked` path leaves the request alone, and the answered request starts a fresh window that sees the exchange.

### High-Level Technical Design

Directional sketch, not implementation:

```
teammate --ask--> relay --> tincan window serve (agent claude-window, launchd)
                               listenPerRequest(exec = "tincan window open --request ...")
                                 claim(session S) -> sh -c exec (env TINCAN_REQUEST_ID, TINCAN_SESSION=S, stdin body)
                                    window open --request:
                                      guest? -> reply failed, exit
                                      write window-runs/<id>/task.md
                                      cmux workspace create --cwd DIR --focus false
                                      read-screen until Claude's input box or a settled shell
                                      send pointer (Claude) or 'claude' 'pointer' (shell), send-key enter
                                      loop: status terminal/needs_input -> exit 0
                                            read-screen not found -> reply failed "window was closed", exit 1
                                 renew claim while running; on nonzero exit post failed (existing)
cmux workspace: Claude reads task.md and replies with the CLI command in it
```

### Assumptions

- A new cmux workspace takes its environment from cmux, not from the caller, so the worker's `TINCAN_*` variables do not reach the window. (Probed: a variable set on the `cmux` call did not reach the workspace.)
- cmux `automation.socketControlMode` allows a process started outside cmux to connect (it is `allowAll` on the owner's machine). `doctor`-style errors from `open` name `setup_cmux.py` and the setting when cmux refuses the connection.

## Implementation Units

### U1. Terminal backend: cmux

- Goal: open a background cmux workspace, read its screen, and type a line into it.
- Requirements: R1, R5, R6, R9, R11.
- Files: `internal/cli/window_cmux.go`, tests in `internal/cli/window_test.go`.
- Approach: a `windowTerminal` interface (open, screen, typeLine) with one cmux implementation calling the cmux CLI by absolute path with `CMUX_QUIET=1`. Start detection and quoting live in `window.go`.
- Test scenarios: the exact cmux argv for create, send, send-key and read-screen; "not found" maps to window gone; an access-denied output names `socketControlMode`; a multi-line line is refused; an unknown `--terminal` errors; start detection sees Claude in any permission mode, a settled shell, a slow Claude under `--starts claude`, and an empty or closed window; the shell line runs `claude` with the prompt through `sh` and never contains `dangerously`.

### U2. `tincan window open` (local and `--request` modes)

- Goal: the command itself. The local mode opens and returns. The `--request` mode refuses guests, writes reply instructions, opens, and waits.
- Requirements: R1, R3, R4, R7, R8, KTD3, KTD5, KTD8.
- Dependencies: U1.
- Files: `internal/cli/window.go`, `internal/cli/window_test.go`, register in `internal/cli/root.go`.
- Approach: `--request` reads `TINCAN_REQUEST_ID`, `TINCAN_SESSION`, `TINCAN_FROM`, `TINCAN_FROM_GUEST` and `TINCAN_CONFIG` from the environment and the body from stdin. It connects with `connect()` (which honors `TINCAN_SESSION`) for status polls, progress, and failed replies. `task.md` holds the sender, the body, and the exact reply and progress commands. The wait loop polls `Get(id, 0)` and the window's screen every 2s. Local mode types a short one-line task as it is, and puts a long or multi-line one in a file.
- Test scenarios (fake terminal; testrelay with a claim under a worker session): the window's reply under the worker session ends the wait and the answer reaches the asker; closing the window fails the request with "closed before it replied"; needs_input ends the wait; a guest request posts the refusal and opens nothing; a failed open fails the request with the cause; a long local task goes in a 0700 run dir.

### U3. `tincan window serve`

- Goal: long-running listener that opens a window per ask.
- Requirements: R2, R4, R7, KTD1, KTD6, KTD7.
- Dependencies: U2.
- Files: `internal/cli/window.go`, `internal/cli/window_test.go`.
- Approach: validate the platform (darwin), resolve the absolute `claude`, `cmux` and `--dir` paths, build the exec string by quoting `os.Executable()` and the flags, then call `listenPerRequest` with `workerOptions{maxWorkers, timeout}` and the agent's config path.
- Test scenarios: the exec string round-trips paths with spaces through `sh -c` to the exact argv. The listener loop itself is covered by the existing per-request tests.

### U4. `tincan window install`

- Goal: write the LaunchAgent `com.agenttincan.window`, which runs `serve` with `TINCAN_CONFIG=~/.config/tincan/window.json` and the current PATH, and optionally load it.
- Requirements: R10.
- Dependencies: U3.
- Files: `internal/cli/window_service.go`, tests in `internal/cli/window_test.go`.
- Approach: mirror `internal/notes/service.go`'s template and XML escaping. Write to `~/Library/LaunchAgents`. Without `--start`, print the bootstrap command. With `--start`, boot out any loaded copy and bootstrap through the existing `launchctl` helper. If the window agent is not joined yet, print the `TINCAN_CONFIG=… tincan join` line, as notes install does. The plist's program is this tincan binary, so `services refresh` picks it up.
- Test scenarios: the plist contains the XML-escaped binary, dir, PATH and config path, the timeout, and the log path, and passes `plutil -lint`.

### U5. Docs

- Goal: the owner and other users can set it up from the docs.
- Requirements: all.
- Files: `docs/adapters/claude-code.md` (new section "Open a new window for each ask", with the fallback section pointing to it), `README.md` (one short mention in the Claude Code area), `docs/trust-model.md` (a bullet under Per-request workers), `site/agents.txt` (one line in the Claude Code setup step).
- Approach: invite plus join for `claude-window`, `tincan window install --start`, the example ask, the safety notes (normal permissions, guest refusal, fixed directory), and the cmux socket setting.
- Test scenarios: none (docs).

## Verification Contract

- `go build ./...`, `go vet ./...`, `go test ./internal/cli/ -run 'Window|Listen'`, then `go test ./...`.
- `golangci-lint run` if available (repo has `.golangci.yml`).
- Manual smoke on the owner's Mac: `tincan window open "say hi and exit"` opens a background cmux workspace running Claude, then close that workspace.

## Definition of Done

- U1 to U4 tests pass, the full test suite passes, and lint is clean.
- The manual smoke shows a background cmux workspace running Claude on the task.
- The docs describe setup end to end, and a PR to mvanhorn/agent-tincan is open.

## Sources & Research

- `internal/cli/listen.go`, `internal/cli/listen_worker.go`: per-request claim, renew, fail and env contract.
- `internal/cli/agent.go` `connect()`: honors `TINCAN_SESSION`.
- `internal/notes/service.go`, `internal/cli/notes.go`: LaunchAgent template and install pattern.
- `examples/claude-code/cmux-wake.sh`, `docs/adapters/claude-code.md`: the current fallback.
- github.com/mvanhorn/agentmail-to-claude-code `cc-daemon.py`: the prompt-file plus one-line-pointer design and the cmux socket access requirement.
- `cmux new-workspace --help` (installed cmux): `--command`, `--cwd`, `--env`, `--focus` (default false).
