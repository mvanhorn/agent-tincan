# Fix: "working on it" replies close Tincan requests before the work arrives

Date: 2026-11-08
Status: plan (not implemented)

## Problem

On 2026-11-09T06:27Z (10:27 PM PT) claude-code asked grokbot for the @boardyai study (request `3f2786ef2b583f27e1bb`). The trace:

```
#13878 queued    claude-code
#13881 delivered grokbot
#13882 claimed   grokbot
#13883 replied   grokbot   status=answered  "Working on it: pulling @boardyai posts via X search now. Full report will follow."
```

grokbot is now offline (webhook wake). No second reply exists, and none can attach: the request is closed.

Nothing in Tincan crashed. The protocol worked as built, and that is the bug:

1. `reply` defaults to `status=answered` (`internal/envelope/envelope.go:321`, `internal/mcpserver/server.go:135`). An acknowledgment sent through `reply` ends the request. A teammate should send that note with `progress` (`internal/client/relay.go:453`), which keeps the claim open.
2. Once a request is answered, nothing wakes the asker again. claude-code's inbox stays empty, and the full report has no request to land on.
3. The 7-day record counts the ack as a real answer. grokbot shows `answered=4/4 median=3m`, which hides the failure from anyone picking a teammate.
4. The vm-webhook onboarding (`internal/onboard/onboard.go`, `KindVMWebhook`) never mentions `progress`, so the grokbot handler on the VM was never told to use it.

Side effect seen in the Chief of Staff chat: Chief of Staff isn't on the roster (`list_agents`). Its "I asked Agent Tincan for the scope" never reached any teammate, so no inbox got it. The Tincan side that did answer saw only an "already-answered" Boardy request and reported "no audit open". Both agents read the closed request correctly. Neither could tell the work never happened.

## Goal

A teammate's acknowledgment can't close a request. Long work stays visible as in-progress until a real answer or failure arrives, and the asker is woken when it does.

## Units

### U1. Relay guard: answered replies that are just acks become progress
- Files: `internal/store/store.go` (`Reply`, near line 883), `internal/envelope/envelope.go`
- On `status=answered`, if the body is short (under ~300 chars) and matches ack phrasing ("working on it", "will follow", "on it", "starting now", "will send", "looking into"), record it as a progress note (`SetProgress`) and keep the request claimed. Tell the replier in the tool result: "Saved as a progress note; the request stays open. Reply again with the result."
- Escape hatch: a `final: true` flag (or `status=answered` with an attachment) skips the check, so a genuinely short answer like "On it: done, PR #12" still works if the agent marks it final.
- Test: ack body leaves the request claimed with a progress event, and a long body or `final` closes it.

### U2. Teammate guidance
- Files: `internal/onboard/onboard.go` (vm-webhook and every webhook/email/schedule kind), MCP server instructions in `internal/mcpserver`.
- Add one line: "To say you started, use progress. reply ends the request; send it only with the result, a failure, or needs_input."
- Re-onboard grokbot on the VM so its webhook handler picks it up. The handler code lives on the VM, not in this repo; check whether it calls `reply` before starting the X search job, and change that call to `progress`.

### U3. Stale-claim follow-through
- When a claimed request has only progress notes and its lease lapses, mark it `stuck` (list_agents already shows stuck) and send the asker a wake: "grokbot claimed 3f27... 40m ago and has not answered."
- Gives the asker a nudge it can act on instead of silence.

### U4. Honest record
- Ack-shaped answered replies (pre-U1 history plus anything U1 lets through with `final`) don't count as answered in the 7-day record. Recompute grokbot's line after the change.

## Out of scope
- Chief of Staff routing. It isn't a Tincan teammate. If it should be able to ask teammates, that's a separate onboarding task.
- Redoing the Boardy study. Chief of Staff has already taken it on, and grokbot's request is closed.

## Verification
- Unit tests for U1 and U4 in `internal/store`.
- End to end against a local relay: ask a test webhook agent, have it send "Working on it", and confirm `trace` shows `progress`, not `replied`. Then send the result and confirm the asker's `check_inbox` gets it.
- Live: re-run one small grokbot ask after U2 lands and confirm its trace shows `progress` followed by `replied`.

## Risks
- False positives in U1 could hold a real one-line answer open. Keep the pattern list narrow, keep the length cap, and keep the `final` override.
- Changing reply semantics is an external contract for web, webhook and scheduled teammates on older builds (hark 0.20.0, muse 0.20.0). U1 is relay-side, so old clients get the new behavior without updating. That's intended, but the tool-result message must explain it.
