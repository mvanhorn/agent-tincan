# Feat: move an untaken ask to the owner's fallback agent

Date: 2026-11-12
Status: plan
Builds on: 2026-11-12-0000-fix-untaken-channel-asks-plan.md

## Problem

The untaken notice tells an asker that `claude-code` has not taken its ask. That is honest, but the ask still sits. The owner already runs a separate agent, usually `claude-window`, whose `tincan window serve` opens a new Claude Code window for each ask and answers on the same request id. The claude-window plan (KTD2) keeps it a separate identity from `claude-code` on purpose, so nothing ever moves a stuck `claude-code` ask there. The owner has to notice the note and re-ask by hand.

## Decisions

- An agent can have one fallback: another joined agent. The owner sets it from an admin device or the relay's admin socket with `tincan fallback set <agent> <fallback>` and clears it with `tincan fallback clear <agent>`. Off by default, so a relay where no one has set one behaves exactly as before.
- Storage follows the good-at line: a nullable `fallback` column on `agents`, added by an in-place migration, written only by `SetAgentFallback`, carried over by `PutAgent` (rejoin, rebind) and dropped from every agent that names an agent when that agent is removed. The route is `PUT /v1/agents/{name}/fallback` with `{"fallback": "<name>"}` (`""` clears), registered in `adminRoutes`, checked with `isAdmin` in the handler and in `Directory.SetFallback`, and audited as `fallback`.
- Setting is refused when either name is not a joined agent, when they are the same agent, when either is a guest, and when the fallback is a phone, a council, `dot-web`, `duet-web`, or the history, notes or pass services. Those either never answer, wait for the owner's approval anyway, or parse a fixed form a normal ask would not fit.
- The roster carries `fallback` on each teammate's entry, and `tincan agents` and `list_agents` print `fallback=<name>`. A guest's roster is built field by field and does not get it.
- The move runs in the sweep's existing untaken check (`tellUntaken`), on the same candidates (`UntakenAsks`: live queued asks the target never claimed, never resumed, not held, not a self-ask, past `noticeGrace` from the later of `queued_at` and `deliver_after`, with no wake or untaken note yet). So it covers the same agents: those that wake themselves (channel, wait, command, none). For each candidate whose target A has a fallback B, the relay tries the move before writing the untaken note:
  - B must be on the roster, online (polled recently), not A and not the asker.
  - The ask must never have moved before (`moved_from` empty). That is the one-hop rule: no A to B to C chains, and no ping-pong between two agents that name each other.
  - A grouped ask (a pass seed, a group fan-out) and a room mention stay with A: the group names its targets, and a mention's reply posts to the room as the mentioned member.
  - Guest rules. A guest's own ask moves only when the guest's list names B and the guest was invited with `--no-hold`: a fresh send from that guest to B would not be held, while a guest without `--no-hold` had A approved by the owner, not B. A teammate's marked ask (sent while working on a guest's ask) moves only when it is `guest_approved`, which is when a fresh send of it would pass the guest hold.
  - Approval. The move runs the policy's `CheckMove` for B: loop, hop, `approval.json` and the default kind holds. A move that would be held is refused, and the ask stays with A with the plain untaken note. Holding the moved ask for approval was the other option; it was rejected because it turns a request that was already deliverable into one the owner must act on, and the asker would wait longer than if it had stayed queued for A. The owner can gate B in `approval.json` and get exactly that refusal.
  - When any of these fails, the ask gets the plain untaken note, as without a fallback, and the relay audits `reroute_skipped` with the reason. The untaken note marks the request, so it is not tried again on a later sweep.
- The write is one conditional update (`Store.RerouteUntaken`), sharing `Store.Move`'s statement: `WHERE id = ? AND to_agent = A AND status = 'queued' AND expires_at > now AND claim_lapsed = 0` plus `claimed_at = 0 AND resumed = 0 AND kind = 'ask' AND moved_from = '' AND deliver_after <= now`. A claim, reply, cancel or earlier move that lands between the sweep's read and this write leaves no row to change, and the ask stays where it is with no note.
- What moves and what stays: the request keeps its id, trace, chain, hop, asker, body, attachments, urgency, guest mark and expiry, so B's reply goes back to the original asker on the same request. Like a failover move, it gets A appended to `moved_from`, its `project` label is dropped (it named a session of A; `tincan window` does not route on it), and any pending wake note is deleted. `queued_at` is set to now, so B gets a full grace of its own before B's untaken note can fire.
- After the write the relay audits `rerouted` (ids, from, to, reason, never text), notes the asker once with a new note kind `rerouted` (`store.NoteRerouted`), and wakes B the way a fresh send does: the inbox key, the request key, and `Events.Queued`, which wakes a webhook or email agent and is skipped by pollers. It does not run failover on the moved ask, and the move sets a `rerouted` column that failover's move statement and `failoverIneligible` both refuse, so a fallback that is a limited web chat agent cannot pass the ask on: one hop.
- The note says the ask moved from A to B because A had not taken it in Xm, that nothing had worked on it before, that it keeps its id so B's reply comes back on it, and not to send it again. A guest asker gets the same text without the wait time detail being tied to a wake method (it never names one).
- `docs/trust-model.md`, `docs/protocol.md` and `greptile.json` say that a request's target changes in two places, failover and this owner-set fallback, and list this path's checks.

## Units

### U1. Config
- Files: `internal/store/store.go` (column, migration, `SetAgentFallback`, `PutAgent`, `DeleteAgent`, `agentsWhere`), `internal/identity/directory.go` and `memstore.go` (`Agent.Fallback`, `Store.SetAgentFallback`, `Directory.SetFallback`), `internal/relay/server.go` (route, handler, roster field), `internal/client/relay.go` and `render.go` (`AgentInfo.Fallback`, `SetFallback`, `FallbackField`), `internal/cli/fallback.go`, `internal/cli/root.go`, `internal/cli/agent.go` and `internal/mcpserver/server.go` (roster lines).
- Tests: store persistence across reopen and removal, admin-only route, refusals, roster field.

### U2. Move
- Files: `internal/store/failover.go` (shared move statement, `RerouteUntaken`), `internal/store/wakes.go` (`NoteRerouted`), `internal/relay/fallback.go` (`rerouteUntaken`, refusal reasons, note), `internal/relay/wakenotice.go` (call from `tellUntaken`).
- Tests: `internal/relay/fallback_test.go`, store race test, guest cases in `internal/relay/guests_test.go`.

### U3. Docs
- `README.md` (claude-code and claude-window together), `docs/adapters/claude-code.md`, `docs/protocol.md`, `docs/trust-model.md`, `site/agents.txt`, onboarding text for kind `claude-code` that suggests `tincan fallback set <agent> <window agent>` when the roster has a window agent (another `claude-code` kind agent whose name has `window` in it).

## Test scenarios

- Moves after grace, exactly once: B receives the ask with the same id, trace and body; the asker gets one `rerouted` note; a later sweep does nothing more; the `rerouted` audit row is there.
- Not before grace. Not when no fallback is set (the plain untaken note instead).
- B offline, or B unknown: plain untaken note, no move. B equal to A is refused at set time.
- No second hop: with A to B and B to A set, an ask moved to B and left untaken there gets B's untaken note and stays with B.
- Race: an ask claimed after the read is not moved (store-level conditional update).
- Approval: a preparer whose `CheckMove` refuses B leaves the ask with A and gives the untaken note.
- Guests: a `--no-hold` guest listed for B moves; one not listed for B, or one without `--no-hold`, does not and gets the guest-safe untaken note.
- B claims and replies on the same id; the original asker reads the answer.
- Config: set, clear, persist across reopen, cleared when the fallback agent is removed; a non-admin caller gets 403; the roster shows `fallback`.
- Onboarding: a claude-code block names `tincan fallback set` only when a window agent is on the roster.

## Risks

- An owner who sets a fallback on an agent the relay wakes (webhook, email) sees no move: those agents get the wake notice path, not the untaken check. The docs say so.
- B is chosen by the owner, so its vendor and data handling are the owner's call; unlike failover, there is no vendor list to stay inside. The approval gate and guest rules still apply to B.
- A moved ask may show in A's history as gone: A can no longer read it in a get, search or trace, as with failover.
