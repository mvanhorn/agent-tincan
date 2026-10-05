---
title: Web Agent Signed-Out Status - Plan
type: feat
date: 2026-10-04
artifact_contract: ce-unified-plan/v1
product_contract_source: ce-plan-bootstrap
execution: code
---

# Web Agent Signed-Out Status - Plan

## Goal Capsule

- Objective: a signed-out browser session becomes visible to the owner and prospective askers, including when dot-web's outbound reader is the only activity.
- Means: typed authentication observations, bounded idle probes, persisted relay recipient facts, and the existing roster, top, doctor and operator-notice surfaces.
- Authority: R-IDs and KTDs below; incidents are owner-reported, code evidence is from b983494.
- Stop conditions: no ambiguous sign-out diagnosis, credential export or Chrome restart. Unproven probes remain disabled.
- Execution profile: its own PR, independent of image input. Planning authorizes no implementation or shipping.

## Product Contract

### Summary

Keep process presence and site authentication separate. A polling web agent may remain online while its browser account needs attention. Record that distinction once, expose it wherever teammates are selected, and clear it after fresh authenticated success.

### Problem Frame

The owner reports that on 2026-10-03/04 an expired chatgpt.com session broke a council ask and an image request, while dot-web logged its sign-in failure 270 times. Roster, top and doctor still looked healthy. The code explains the gap: `internal/history/native.go` maps `not_logged_in` to `ErrNotLoggedIn`, but `internal/history/web.go` replies and logs locally, and `internal/history/dots_out.go` backs off and logs. `internal/relay/server.go` computes Online from polling, not browser authentication. `internal/cli/top.go` and `internal/cli/doctor.go` know unanswered wakes but have no browser-authentication fact.

### Requirements

Detection and recovery

- R1. All seven kinds report typed ErrNotLoggedIn from inbound reads/sends and dot outbound reads, teaching and replies. Rate limits, anti-bot checks, missing grants/conversations, paused dots and disconnected extensions remain distinct.
- R2. Fresh authenticated reads or confirmed sends clear the condition. Relay polls, tab closes, public file downloads and cached auth do not. Repeated failures preserve since.
- R3. Probe initially and every two idle minutes while Chrome is available; skip overlap and cooldowns. Never send anonymously, create conversations or restart Chrome. Sleep/disconnection delays detection.

Relay and presentation

- R4. Persist optional site, since, observation time and browser host across relay restarts. Reject cross-identity, arbitrary-site and stale reports; removal/replacement cannot inherit old health.
- R5. CLI/MCP rosters show signed_out="chatgpt.com since 2h"; top flags SIGNED-OUT even when online. Doctor names agent, site and host, says to sign in there in Chrome without restarting it, and preserves Copilot personal-account guidance.
- R6. Send target facts warn of known sign-out; CLI/MCP asks return pending promptly instead of waiting inline. Service authentication failure still produces a specific reply. Warnings do not cancel work or prove nothing was sent.
- R7. Fields are optional. New services on old relays keep serving, log reporting incompatibility once and retry transient failures with backoff.

Owner visibility

- R8. Reuse configured operator notices once per episode. Notification failure never hides health. Without a recipient, roster/top/doctor are the owner surfaces; no push-delivery guarantee.
- R9. Document facts, probe cost, recovery and upgrade limits in protocol and adapter docs.

### Key Decisions

- Keep authentication separate from Online and unanswered. Governs R1, R4, R5.
- Probe every two idle minutes, a proposed default. Copilot requires a background tab, not a cheap HTTP check. Governs R3.
- Preserve queue/hold semantics; known sign-out shortens the inline wait. Governs R6.
- Reuse approval.json's configured notify destination, documenting its broader purpose. Existing notifyApproval queues KindNotify to an agent, not directly to a human. Governs R8.

### Scope Boundaries

- All seven web kinds, custom names and omitted join kinds are covered.
- Considered and not built: new email, desktop, Slack or phone channels. Never infer an operator by name.
- Considered and not built: general health framework, automatic login, cookie export, account switching, Chrome restart or failed-ask replay.
- Considered and not built: propagating one ChatGPT failure to all dot agents. Shared grants do not establish shared host/session identity.
- Image input belongs to its independent plan.

### Sources

- Detection: `internal/history/web.go`, `internal/history/web_poll.go`, `internal/history/dots_out.go`, `internal/history/native.go`, `internal/history/sites.go`, the seven site readers, `extension/ops.js`, `extension/send.js`, `extension/background.js`, `internal/cli/web.go`.
- Health pattern: `internal/store/wakes.go`, `internal/store/store.go`, `internal/relay/server.go`, `internal/relay/unanswered_test.go`, `internal/envelope/envelope.go`, `internal/client/relay.go`, `internal/client/render.go`, `internal/cli/agent.go`, `internal/cli/top.go`, `internal/cli/doctor.go`, `internal/mcpserver/server.go`.
- Identity/notices: `internal/identity/directory.go`, `internal/policy/approval.go`, `internal/relay/approval_test.go`; documentation: `docs/adapters/web-agents.md`, `docs/protocol.md`; commands: `Makefile`, `extension/package.json`.

## Planning Contract

### Key Technical Decisions

- KTD1. Add PUT /v1/agents/self/web-status through existing agent authentication. Accept known site, explicit state and observation timestamp only; derive host from directory NodeName. Permit kindless agents but reject conflicting web kinds. Reject stale observations and timestamps outside a bounded clock-skew allowance. Governs R1, R4, R7.
- KTD2. Store latest observation per agent/join identity, site, observed_at and nullable signed_out_since. Relay time starts an episode. Keep clear observations as ordering tombstones. Follow wakes migration/deletion patterns and invalidate on rebind. Add optional signed_out_site, signed_out_since, web_status_observed_at and web_host to envelope.Target, inherited by AgentInfo. Missing means unknown, not healthy. Governs R2, R4, R7.
- KTD3. Observe errors.Is(err, ErrNotLoggedIn) before conversion/swallowing in anchorFor, waitReply, resume and dot paths. Keep attribution local to WebAgent; public or cached read success must pass a fresh session check before clearing. Serialize observations under its mutex; a bounded reporter sends the latest snapshot in order and retries without replacing real replies. Log transitions, not each failed read. Governs R1, R2, R7.
- KTD4. Add argument-free fixed <site>.session operations. Reuse fresh chatgptAuth, uncached claudeOrgId, grokSession, geminiAuth(true), perplexitySession and chatgptAuth for dots. Copilot uses its signed-in gate in a temporary owned tab, then closes it. Return only success or typed failure; public detail reads cannot prove authentication. Governs R1 to R3.
- KTD5. Use a cancellable idle loop with web clock, TryLock and cooldown checks. Recent authenticated traffic replaces a probe. Bound HTTP probes to 30 seconds; use the existing tab-read timeout for Copilot. Dot watcher backoff cannot suppress the two-minute auth probe except during cooldown. Old extensions retain traffic-based detection with one upgrade notice. Governs R3, R7.
- KTD6. Compose health with schedule/wake facts rather than overwriting Target. Include health on held sends without releasing content. client.asked returns promptly on known sign-out; GET stays unchanged. Add a compatible send-result API for notify paths because Relay.Send discards Target. Share hints across CLI/MCP/groups. Governs R5 to R7.
- KTD7. Expose a safely reloaded approval-policy notify accessor; absent/invalid config sends no notice. Persist pending notification with the transition; enqueue and mark delivered atomically. Retry queue failures with backoff, cancel obsolete pending notices on recovery, and never recursively notify failure. Include only agent/site/since/host/recovery metadata. Governs R8.

### Assumptions

- One active service owns each agent identity; this does not solve competing pollers.
- Directory NodeName labels the machine running the local native socket and Chrome. Missing legacy names say "the agent's registered machine".
- Copilot's unfinished personal-account sign-in shares the sign-in recovery action; blocked and disconnected errors do not.

## Implementation Units

### U1. Persist authenticated self-reports and recipient facts

- **Goal:** establish a bounded, restart-safe relay contract independent of presentation.
- **Requirements:** R4, R7; KTD1, KTD2, KTD6.
- **Dependencies:** none.
- **Files:** `internal/envelope/envelope.go`, `internal/client/relay.go`, `internal/client/attachments.go` (capabilities), `internal/relay/server.go`, `internal/relay/attachments.go` (capability response), `internal/store/store.go`; new `internal/store/web_status.go`, `internal/store/web_status_test.go`, `internal/relay/web_status_test.go`.
- **Approach:** add optional capability and fields, self-only route, validated observations and migration. Batch roster reads outside the relay mutex. Compose the facts for every wake method, including wait, without overwriting existing Target fields. Clean up on removal, replacement and rebind. Add a small client reporting method with unsupported-relay detection.
- **Patterns to follow:** wakes migration and restart tests in `internal/store/wakes.go`; identity selection and handleAgents in `internal/relay/server.go`.
- **Test scenarios:**
  - First failure sets since; repeat failures retain it; fresh success clears it; an older or duplicate report cannot reverse the result.
  - Restart retains signed-out and clear observations; removing and rejoining the same name does not inherit them.
  - Another identity, invalid site/state, conflicting kind, unexpected fields and implausible timestamps are rejected.
  - Wait, schedule and webhook agents preserve their other target facts; held sends expose health without held content.
  - Older JSON has no condition; older clients ignore the new fields; unsupported route is distinguished from transient errors.
- **Verification:** focused store, relay and client tests, then the contract gates below.

### U2. Observe traffic and probe idle authentication

- **Goal:** discover and clear sign-out on every web path without sending a message.
- **Requirements:** R1 to R3, R7; KTD3 to KTD5.
- **Dependencies:** U1.
- **Files:** `internal/history/web.go`, `internal/history/web_poll.go`, `internal/history/dots_out.go`, `internal/history/native.go`, `internal/history/sites.go`, `internal/cli/web.go`, `extension/ops.js`, `extension/send.js`; new `internal/history/web_status.go`, `internal/history/web_status_test.go`; existing native, dots and extension operation/send tests.
- **Approach:** wire fresh operation observations into all inbound and outbound paths, then add fixed session operations and the idle loop. Preserve clicked-send uncertainty. Coalesce reporting outside site locks, stop it on cancellation and use bounded detached reporting when a request context expires. Back off reporting errors and suppress duplicate sign-out logs. Host status and file operations are not authenticated recovery evidence.
- **Patterns to follow:** ErrNotLoggedIn mapping in internal/history/native.go; dotTick's TryLock, web clock and cooldown; fixed SPEC validation and owned-tab lifecycle in the extension.
- **Test scenarios:**
  - Every site's typed signed-out failure reaches the relay; all other error classes leave the previous state unchanged.
  - An anchor read that currently logs and continues still records sign-out; subsequent confirmed success clears it.
  - Dot outbound-only failure reports once, repeated ticks retain since, and a successful feed read clears it.
  - Idle startup and two-minute probes run with no inbox traffic, skip active sends/cooldowns, and stop on shutdown.
  - Cached Claude/Gemini state and public reads cannot falsely clear sign-out; Copilot's probe closes its owned tab.
  - Relay failure and an old extension do not break existing replies or create a tight retry loop.
- **Verification:** fake-clock service tests, extension mocks and native framing tests; live sign-out and sign-in checks for all seven kinds during implementation acceptance.

### U3. Surface warnings and notify the configured operator

- **Goal:** expose the condition at selection, ask and owner attention points.
- **Requirements:** R5, R6, R8; KTD6, KTD7.
- **Dependencies:** U1, U2.
- **Files:** `internal/client/render.go`, `internal/client/relay.go`, `internal/client/attachments.go`, `internal/cli/agent.go`, `internal/cli/top.go`, `internal/cli/doctor.go`, `internal/mcpserver/server.go`, `internal/policy/approval.go`, `internal/relay/server.go`, `internal/store/web_status.go`; corresponding client, CLI, MCP, policy and relay tests.
- **Approach:** add shared signed-out note/field/hint helpers; top score comparable to UNANSWERED; doctor action naming site and browser host. Return known-health asks before their inline wait and retain request IDs and hold instructions. Preserve Target in notify and group paths through an additive API. Reuse operator notices with transactional deduplication and failure audit events. Do not imply an operator actually informed the human.
- **Patterns to follow:** UnansweredField, wakeHint, attention, unansweredCheck and notifyApproval.
- **Test scenarios:**
  - CLI/MCP rosters agree on quoted fields, elapsed time and omission after recovery.
  - An online signed-out agent is flagged and sorted for attention; doctor names the remote browser host and never recommends restarting Chrome.
  - Plain, attached, grouped, held and notify asks preserve target warnings without waiting or losing request IDs.
  - One episode creates one notice across repeated reports and restarts; recovery then a new failure permits another.
  - No configured recipient or a missing recipient leaves health visible; notification errors never loop or bypass approval for user content.
- **Verification:** renderer parity, CLI/MCP result tests, relay notice integration tests and race checks.

### U4. Document recovery and compatibility

- **Goal:** make the operator action and mixed-version limits explicit.
- **Requirements:** R9, R3, R7, R8.
- **Dependencies:** U1 to U3.
- **Files:** `docs/adapters/web-agents.md`, `docs/protocol.md`, `README.md`, `internal/cli/web.go`, `internal/mcpserver/server.go`.
- **Approach:** describe optional fields and route, distinction from Online, idle timing exceptions, Copilot tab cost, notification destination reuse, old-extension traffic-only behavior and signing back in without restarting Chrome.
- **Patterns to follow:** protocol's unanswered-wake and recipient-fact sections; web-agent troubleshooting.
- **Test scenarios:**
  - Documented recovery text matches doctor and service replies for ChatGPT/dots and Copilot.
  - Documentation never promises push notices with no configured operator or idle detection when Chrome is unavailable.
- **Verification:** prose and protocol review against final wire fixtures and the seven-site acceptance matrix.

## Verification Contract

Planning only: these are implementation gates, not commands executed for this document.

| Gate | Command or check | Proves |
|---|---|---|
| Suite | `go test ./...` | Service, persistence, CLI/MCP and compatibility scenarios |
| Race | `go test -race ./internal/history ./internal/relay ./internal/store ./internal/client` | Reporter, probe and roster concurrency |
| Vet | `go vet ./...` | Go static checks |
| Lint | `make lint` | No new findings; six pre-existing modernize issues are the supplied baseline, to be compared during implementation |
| Linux | `GOOS=linux go build ./...` | Relay and client portability |
| Extension | `make extension-test` | Fixed probe validation and tab lifecycle; Makefile runs Node's test runner |
| Live | Sign out, observe roster/top/doctor and an ask, then sign in for each kind; repeat dots with outbound activity only | Detection, recovery and actionable host guidance without Chrome restart |
| Mixed versions | New service with old relay/extension, old client with new relay | Optional fields and graceful reporting fallback |

## Definition of Done

- All seven kinds produce and clear the same persisted authentication fact without changing process presence.
- A known signed-out target is visible in CLI/MCP selection and ask results, top and doctor; held requests remain held.
- Idle detection and its exceptions are documented and verified; no probing sends a message or exports a credential.
- Configured operator notices are deduplicated and restart-safe; absent configuration is described honestly.
- Verification gates pass apart from the identified existing lint baseline. The PR remains independent of image input.
