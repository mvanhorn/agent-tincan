---
title: Relay Service Shows the Maintainer's Name in Login Items - Plan
type: fix
date: 2026-11-03
artifact_contract: ce-unified-plan/v1
product_contract_source: ce-plan-bootstrap
execution: code
---

# Relay Service Shows the Maintainer's Name in Login Items - Plan

## Goal Capsule

- Objective: nobody running a tincan relay on a Mac sees "Software from 'Matthew Charles Van Horn' can run in the background" again. The relay gets an install command like every other tincan service, so its LaunchAgent starts through Agent Tincan.app from the first run, and relay plists written by hand under any label are found and moved onto the app.
- Means: `tincan relay install` (KTD1, KTD2) built on `history.InstallServiceDef`; `services refresh` and the doctor's login items check look past the `com.agenttincan.*` file names (KTD4); the quickstart and README point at the command.
- Authority: issue #209 and PR #144 win on behavior. KTDs win on mechanism.
- Stop conditions: stop and report if the relay's flags cannot be carried into a plist without changing what `tincan relay` does, or if widening refresh would touch a plist that does not run this tincan binary.
- Execution profile: `internal/cli` and `internal/history`, plus docs. No relay server, wire or config change.
- Finishing: ce-work implements and verifies locally, then opens one PR against `main`.

---

## Product Contract

### Summary

`tincan relay install [relay flags]` writes `com.agenttincan.relay` (a LaunchAgent on macOS, the `tincan-relay.service` user unit on Linux) that runs `tincan relay` with the flags given, through Agent Tincan.app on a release build. It starts nothing and prints the start command. `tincan services refresh`, `tincan upgrade` and `tincan doctor` now also find a LaunchAgent whose program is this tincan binary when its file is not named `com.agenttincan.*`, so a relay plist written by hand under its own label is moved onto the app too. The quickstart's "run it under your service manager" line becomes the install command.

### Problem Frame

PR #144 (merged 2026-10-09) made every tincan LaunchAgent start through a signed Agent Tincan.app so Login Items names it "Agent Tincan". That fix holds only for plists tincan writes or later refreshes:

- The `install` commands cover history, web, council, pass and relay-watch. The relay has none. The quickstart says only "Run it under your service manager (systemd, launchd) so it restarts", so relay operators write a plist by hand that runs the bare binary, and macOS names it after the signing certificate: the maintainer's legal name.
- `tincan upgrade` runs `services refresh --restart`, so a hand-made plist that already exists at upgrade time is fixed. One written after the last upgrade (Kyle's, on v0.20.1, on a headless Mac mini) stays bare until someone runs `tincan services refresh`, and nothing in the relay's path mentions it.
- `services refresh` and the doctor's check only look at `~/Library/LaunchAgents/com.agenttincan.*.plist`. A hand-made relay plist under any other label is invisible to both, so it stays bare even after an upgrade and the doctor reports "ok". That is the likely reason Trevin hit the same thing after #144 shipped.

### Requirements

- R1. `tincan relay install` accepts the same flags as `tincan relay`, validates them the same way, and writes a service that runs `tincan relay` with exactly the flags the operator passed.
- R2. On a release build on macOS, the written plist starts through Agent Tincan.app, as the other installs do. A development build, a `--binary` the launcher would refuse, or a failed app install fall back to the plain plist, which `tincan doctor` reports.
- R3. The command never writes `TS_AUTHKEY` into the service. When the relay has not joined the tailnet yet, it says how to join first.
- R4. When another LaunchAgent already runs `tincan relay`, install says so, so two relays never share a state dir unnoticed.
- R5. `tincan services refresh` (and so `tincan upgrade`) wraps a LaunchAgent whose program is this tincan binary whatever its file name. Plists that do not run this binary are neither listed nor touched.
- R6. `tincan doctor`'s login items check names such a plist when it still runs bare.
- R7. The quickstart and README name `tincan relay install`, and say what to run for a relay plist written before it existed.

### Scope Boundaries

- A relay run as a LaunchDaemon (`/Library/LaunchDaemons`) or as a separate macOS user is not covered. Agent Tincan.app lives in a user's `~/Applications`, and `services refresh` only manages the current user's LaunchAgents. The README keeps its advice about a separate OS user and says the install command writes a per-user service.
- The notes service stays unwrapped, as #144 decided.
- No warning from the relay process itself at startup. The doctor and the install command are where operators look.
- Considered and not built: re-rendering hand-made plists into `com.agenttincan.relay`. Refresh edits in place, keeping every key the operator chose, which is the #144 contract.

---

## Planning Contract

### Key Technical Decisions

- KTD1. The relay's flags move into one `addRelayFlags` used by both `relay` and `relay install`, with validation in `relayFlags.validate`. Install writes only the flags the operator changed (`Flags().Visit`), as `--name=value`, one per element for slice flags. Defaults then stay the relay's own, so a later release's new default reaches the service. The one exception is `--state-dir`, always written as the absolute path install resolved: it is the relay's identity, and a service manager's environment (systemd `--user` without a shell's `XDG_CONFIG_HOME`) could resolve the default elsewhere. Slice elements are CSV-quoted so they read back whole; arguments with a newline are refused.
- KTD2. `history.ServiceDef` gains `Args []string`, rendered at an `__ARGS__` placeholder: one escaped `<string>` per argument in the plist, one quoted, systemd-escaped word in `ExecStart` (with `$` doubled). This keeps variable-length arguments out of the fixed-placeholder path, which XML-escapes values and so cannot carry new array elements.
- KTD3. Label `com.agenttincan.relay`, unit `tincan-relay.service`, log `~/Library/Logs/tincan-relay.log`, `KeepAlive` true and `Restart=always`, so both the default re-exec and `--upgrade-exit` (status 75) recover.
- KTD4. `launchAgentPlists(home, exe)` returns every `com.agenttincan.*` plist plus any other plist in `~/Library/LaunchAgents` whose parsed program (through the launcher, if present) is `exe`. Refresh prints no line for plists it skips silently. Hidden files (refresh's own `.tincan-refresh-*` staging files), non-regular files, copies whose Label starts with `com.agenttincan.`, and a second plist for a Label already listed are left out. The notes exclusion also matches a job that runs `tincan notes`, whatever its label. The doctor treats an unreadable plist as a failure only under the `com.agenttincan.` prefix.
- KTD5. Join detection is a file check: with tsnet (no `--listen`), no `<state-dir>/tsnet/tailscaled.state` means the node has not joined. Install then prints a one-time terminal command with `TS_AUTHKEY` before the start command.

### Implementation Units

- U1. `internal/history/service.go`: `ServiceDef.Args` and its rendering in `installServiceDef`. Tests: darwin plist carries each argument as its own escaped element, after the fixed ones and behind the launcher; Linux `ExecStart` quotes and escapes each, `$` doubled.
- U2. `internal/cli/relay.go` + new `internal/cli/relayinstall.go`: `addRelayFlags`, `validate`, `relayInstallCmd`, templates, join hint, other-relay warning. Tests (linux-runnable, GOOS forced): args from changed flags only, slices split, validation errors, no `TS_AUTHKEY` in the file, join hint shown and hidden, unit path and start command.
- U3. `internal/cli/services.go`: KTD4 in refresh, `wrappedPlists` and `loginItemsCheck`. Tests (macOS, plutil): a hand-labelled plist running this tincan is wrapped and named by the doctor; one running another binary is left alone and unlisted; an unreadable foreign plist does not fail the doctor.
- U4. Docs: quickstart step 3, README relay and Login Items sections, `relay` long help.

### Verification Contract

- `go build ./...`, `go vet ./...`, `go test ./internal/cli/ ./internal/history/ ./internal/watch/` on Linux.
- The plutil-backed services tests skip on Linux; CI's `macos-latest` job runs them.
- `golangci-lint run` if available.

### Definition of Done

All of R1-R7 hold, tests above pass locally and in CI, and the PR body says what was not checked on a real Mac (Login Items naming itself).

### Risks

- Widening refresh means `tincan upgrade` now also restarts a hand-labelled relay LaunchAgent that runs the upgraded binary, as it already did for `com.agenttincan.*` ones. A brief relay restart during a client upgrade on the relay host is the expected cost.
- Reading every plist in `~/Library/LaunchAgents` costs one `plutil` exec each. Typical Macs have a few dozen.

### Sources

- Issue #209; PR #144 and `docs/plans/2026-10-08-1342-fix-macos-login-items-name-plan.md`.
- `internal/cli/services.go`, `internal/history/service.go`, `internal/watch/watch.go` (`relay-watch install`, the closest existing pattern).
