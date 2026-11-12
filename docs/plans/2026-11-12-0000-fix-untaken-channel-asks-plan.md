# Fix: asks to a channel-woken agent sit untaken and nobody says so

Date: 2026-11-12
Status: plan

## Problem

An email-woken agent sent three asks to `claude-code` (wake `channel`, kind `claude-code`). They sat queued for over four hours. Many Claude Code sessions run `tincan mcp --channel`, so the relay showed `claude-code` online, and often busy, because some session reported it was mid-turn. No session ever took the asks.

A channel notice only reaches a Claude Code session that is idle and was started with channels enabled. SDK and desktop sessions never get one, and the channel loop answers pings itself, so every health signal looked fine. The asker read "busy" as "working on it", told the owner the asks would be picked up soon, and then guessed at an order that did not exist.

Three gaps let this happen:

1. `TellAskers` (`internal/relay/wakenotice.go`) only covers agents the relay wakes (webhook, email). For an agent that wakes itself (channel, wait, command, none), nothing ever tells the asker that its ask is still untaken.
2. `computeStatus` (`internal/relay/status.go`) lets a host-reported busy hide a stalled queue. The only queue rule for an online agent is `staleQueue` (1h), and until then the roster says busy.
3. A Claude Code session without channels never looks at its inbox unless the model happens to call `check_inbox`.

## Decisions

- A new relay note kind, `untaken`, beside `wake`, `stale_claim` and `expired`. It goes through the existing `noteAndTell` path, so it is stored once per request in `relay_notes`, shown as `relay_note` on the asker's get, and sent as a notify from `relay`.
- The check runs from the relay's existing sweep (`Server.Sweep`, every `SweepEvery`), not a new goroutine. It reads every candidate in one query.
- It covers agents whose wake method is not relay-woken and not `schedule` (schedule agents already have overdue checks). A request is a candidate when it is a live queued ask the target never claimed (so not one requeued after a lapsed claim, which has a stale-claim note, nor one resumed after a clarification), from a joined agent other than the target, with no `wake` or `untaken` note yet, and has waited past `noticeGrace(urgent)` since the later of when it last entered the queue (`queued_at`: sent or approved) and its `deliver_after`.
- The note says plainly that the agent has not taken the request, that it is still queued, that nothing is working on it yet, and that showing busy or online does not mean it is working on this one. For `channel`, one more sentence says Claude Code sessions only see it while idle and started with channels. Then the usual `suggest` teammate line.
- A guest asker gets the same generic text `TellAskers` gives a guest, with no wake details.
- Status: for the same agent-side wakes, an agent with no live claim whose oldest never-claimed queued ask has waited past the notice grace, timed the same way as the notice, is `stuck` with reason `N queued, untaken 25m`, ahead of busy and idle. `statusFacts` carries the grace and a per-agent untaken count and oldest wait (`Store.UntakenStats`). The 1h `staleQueue` rule stays for everyone else, and the `quietUntil` guard still applies. Clients print `status_reason` as given, so no client change is needed for the new reason.
- Client: a new `tincan hook claude-code` command for Claude Code's `SessionStart` and `UserPromptSubmit` hooks. It peeks once (no hold, never claims, the same `Peek` the channel loop uses) under a 3s timeout, and prints one line when teammate requests or replies wait. It always exits 0; errors go to stderr only.
- To print the oldest age, `envelope.Pending` gains an optional `created_at`. Older relays leave it out and the hook drops the age.

## Units

### U1. Relay untaken notice
- Files: `internal/store/wakes.go` (`NoteUntaken`, `UntakenAsks`), `internal/relay/wakenotice.go` (`tellUntaken`), `internal/relay/server.go` (call from `Sweep`).
- Tests: `internal/relay/untaken_notice_test.go`.

### U2. Status
- Files: `internal/relay/status.go`, `internal/relay/server.go` (fill the grace into `statusFacts`).
- Docs: `docs/protocol.md` and `README.md` list the new reason.
- Tests: new cases in `internal/relay/status_test.go`.

### U3. Claude Code hook
- Files: `internal/cli/hook.go`, `internal/cli/root.go`, `internal/envelope/envelope.go` and `internal/store/store.go` (`Pending.CreatedAt`), `internal/onboard/templates/agent.tmpl`, `README.md`, `docs/adapters/claude-code.md`, `site/agents.txt`, `docs/protocol.md`.
- Tests: `internal/cli/hook_test.go`, onboard test for the new setup line.

## Test scenarios

- A channel agent with a queued ask past grace: the asker gets one notice, and a second sweep sends none.
- Before grace: no notice. An urgent ask uses the urgent grace.
- A held ask, a self-ask, an ask resumed after a clarification, and an ask that already has a `wake` note: no notice. An approved ask waits a full grace from its approval.
- A relay-woken (webhook) or schedule agent: no untaken notice.
- A guest asker: generic text, no wake method or channel details.
- Status: a channel agent that is host-busy with a queued ask past grace flips to `stuck` with `1 queued, untaken 15m`; inside grace it stays busy; a live claim keeps it busy; an ask approved after a long hold is not stuck at once; quietUntil still hides it.
- Hook: prints nothing when nothing waits; prints the line with the count and oldest age when requests wait; exits 0 with output on stderr only when the relay errors or is unreachable.

## Risks

- A busy single-threaded agent working a long claim will leave later asks queued past grace, and their askers get a note. The note is still true: nothing is working on that request yet. Status keeps showing busy in that case, since there is a live claim.
- The hook's peek is sessionless, so it counts replies meant for another window. That can tell a window to call `check_inbox` and find nothing; it costs one tool call.
