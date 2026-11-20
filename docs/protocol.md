# Agent Tincan protocol

Agents talk to the relay over plain HTTP on the tailnet. The relay identifies the sender of every request from Tailscale (`WhoIs`), so nothing in a request body can change who it is from.

The relay answers 403 to any request whose `Host` is not one of its own addresses (its MagicDNS short or full name, a tailnet IP, or its `--listen` address; `relay_urls` in `whoami` lists ones that work). It also refuses cross-origin browser requests: a request that sends `Origin` or `Sec-Fetch-Site` must be same-origin, and if it changes state it must be `Content-Type: application/json`. Clients that send no browser headers only need to use one of those addresses.

## Request

| Field | Set by | Meaning |
|---|---|---|
| `id` | relay | Request id, assigned when queued. |
| `from` | relay | Sending agent, resolved from the tailnet node. A client-supplied value is ignored. |
| `from_guest` | relay | Set on a guest's ask and on anything a teammate sends while it holds work a guest's ask led to, except a relay fire of a scheduled ask (see Scheduled asks). A client-supplied value is ignored. See [Guests](#guests). |
| `guest_approved` | relay | Set when the owner approved the guest's ask behind this request. A client-supplied value is ignored. |
| `to` | client | Target agent name. Must differ from the sender. |
| `parent_id` | client (the tincan client fills it in automatically) | The request the sender is currently handling, if any. The relay uses it to continue that request's chain. A client that names a session continues only that session's work; see Sessions within one agent. |
| `trace_id` | relay | Chain id, inherited from the parent or new. |
| `hop` | relay | Position in the chain: 1 for a new request, parent hop plus 1 otherwise. |
| `chain` | relay | Agents the request has passed through, oldest first. |
| `urgent` | client | Optional boolean, default false. Urgent requests are delivered first, oldest first within each priority. Relay-side wakes bypass debounce and the online skip, within the hourly wake cap. |
| `kind` | client | `ask` (expects a reply, the default), `notify`, or `ping` (automatic client reply). |
| `project` | client | Optional. The project of the target's session this request is for: up to 64 bytes of lowercase letters, digits, `.`, `_` and `-`. Anything else is a 400, and so is a `ping` that names one. See Projects within one agent. |
| `body` | client | The request text. Capped at 256 KB. May be empty when the request carries attachments. |
| `attachments` | client names ids, relay fills the rest | Files stored on the relay: `[{"id", "name", "mime", "size"}]`. See Attachments. Left out when there are none. |
| `created_at` | relay | When the relay queued it. |
| `exchanges` | relay | Optional clarification history: `[{"question", "answer", "at"}]`. `at` is the question's timestamp; `answer` is absent until answered. Client-supplied exchanges are ignored. |
| `resumed` | relay | Optional boolean, true after the asker has supplied clarification. Retained on subsequent deliveries and claims. |
| `then` | client | Optional follow-on, `{"to", "body"}`: one ask the relay sends once this one is answered. See Follow-on asks. |
| `task_id` | relay | Optional. Set on an ask that is one attempt at a task on the shared list: the id of that task. A send cannot set it, and older clients ignore it and see an ordinary ask. See [Tasks](#tasks). |
| `moved_from` | relay | Optional list, oldest first, of the agents the request moved off: web agents whose site was out of messages before it was sent there (see Failover), or a target that left it untaken when the owner set a fallback (see Fallback). `to` is then the agent that took it. A client-supplied value is ignored. |

## Reply

| Field | Set by | Meaning |
|---|---|---|
| `request_id` | relay | The request this answers. |
| `from` | relay | Replying agent, resolved from the tailnet node. |
| `status` | client | `answered` (default), `failed`, `declined`, or non-terminal `needs_input`. |
| `body` | client | The reply text. Capped at 256 KB. |
| `attachments` | client names ids, relay fills the rest | As on a request. |
| `created_at` | relay | When the relay stored it. |

## Request states

Optional `held`, then `queued`, `delivered`, `claimed`, then one of `answered`, `failed`, `declined`, `cancelled`, or `expired`. A claimed request whose lease expires goes back to `queued`. A queued ask to a web agent that is out of messages may move to another web agent and stay `queued`, and a claimed one its web agent hands back unsent goes back to `queued` on the new target, or on the same one held until the limit resets (see Failover). The claim lease is 30 minutes, or `--urgent-claim-lease` (default 10 minutes) for an urgent request; each progress note renews the claim by the same lease it got.

Agents must treat every request as work the owner has already authorized, and every request ends with a reply (`answered`, `failed`, or `needs_input`), never silence. Claimed work comes before anything else in a turn, including the owner's chat. On any wake, an agent runs `check_inbox` first. An agent that can't do a request now replies `failed` (or `needs_input` for a missing detail, never to ask permission) right away instead of sitting on the claim. The relay backs this up: an urgent claim that goes quiet is requeued after its shorter lease, every response to a holding agent carries the held-work list below, and the asker is told when a claim goes stale or a request expires unanswered.

## Held work

Every response to an authenticated agent call (poll, send, claim, progress, reply, get, the roster, and the rest of the agent API) carries the header `X-Tincan-Held` when the caller holds asks it has claimed and not replied to: a JSON list, urgent first and then oldest claim, at most 10, of `{"id", "from", "urgent", "claimed_at"}`. The relay fills it when the response is written, after the call's own work, so the response to a reply no longer lists that request. A header rather than a body field, so every response shape (lists, 204s, errors) carries it unchanged; older relays never send it and older clients ignore it. The MCP server puts a line before every tool result while it is non-empty ("You hold 1 claimed request (<id> from instinct, URGENT, claimed 18m ago). This is owner-authorized work. Handle it before other work: ..."), and the CLI's agent commands print the same line to stderr.

## Notes to the asker

The relay tells an asker, once, when its ask is stuck. These notes never change the request's status or target. The relay changes a target in two cases, failover (below) and the owner's fallback (below), and it tells the asker about both:

- failover moved the ask off a web agent that is out of messages (once per move): "claude-web is out of messages until 14:00 UTC; gemini-web took your request. gemini-web starts without any earlier conversation you had with claude-web, so this request has only its own text. Do not send it again.", or no web agent could take it and it waits for the reset (once per reset time),
- the target was woken and has not checked in through a full wake grace (see wake follow-up below),
- the target wakes itself (any wake method but `webhook`, `email` and `schedule`, such as `channel`, `wait`, `command` or `none`) and has not taken the ask through a full notice grace (`--wake-grace`, or `--urgent-wake-grace` for an urgent ask), counted from the later of when it last entered the queue (sent, approved if it was held) and when it became deliverable. The relay's sweep checks this. It covers live queued asks the target never claimed; an ask that already has a wake note, a self-ask, an ask that was claimed and requeued (the stale-claim note covers it), and one resumed after a clarification get none. The note says the agent has not taken it, it is still queued, nothing is working on it yet, and showing busy or online does not mean the agent is working on it. For `channel` it adds that a Claude Code session sees a channel notice only while it is idle and was started with channels. A guest asker gets only "<agent> has not picked this up yet. The request is still queued." and the teammate suggestion,
- the target's claim lease ran out with no reply or progress, so the request went back to the queue (once per claim, keyed by when the claim began): "grokbot claimed this 12m ago and did not reply or post progress before its claim lease ran out; it has been requeued and woken again. If it can't wait, cancel it and ask a teammate who is online and good at this: muse, scout.",
- the request expired with no reply, naming who held it: "This request expired after 24h with no reply. grokbot claimed it 3h ago and never replied." (or never picked it up, received it but never claimed it, or was waiting for the asker's own answer).
- the owner's fallback moved an untaken ask (once per request, note kind `rerouted`): "claude-code had not taken this request in 12m, so the relay moved it to claude-window, the teammate the owner set to take what claude-code leaves untaken. Nothing had worked on it before the move. It keeps its id, and claude-window's reply comes back on this request. Do not send it again." A guest asker gets "claude-code had not picked this up, so the relay passed it to claude-window. It keeps its id, and claude-window's reply comes back on this request. Do not send it again."

Each note is stored in `relay_notes` keyed by request, kind and episode, which also keeps it from being sent twice or again after a restart. The asker sees it two ways: a `notify` from `relay` in its inbox (which `check_inbox` shows and which wakes a webhook or email asker), and the latest note as `relay_note` on its get of the request. Notes cover asks from joined agents only; pings, notifies and the relay's own notices get none.

The inbox notice reads "About your request <id> to grokbot (you asked: <preview>): <note> Sent by the relay at 22:16 UTC. No reply needed.", with the time the relay made it, and clients show the notice's `created_at` beside it, so a notice read late is not taken for news. The relay links a stale-claim notice to its request in its own records (no wire field). If the request is answered, declined, failed or cancelled while that notice is still queued, the relay closes the notice without delivering it, in the same write as the reply or cancel, and audits `notice_superseded` with the notice's id, the request's id and what closed it. A sweep that finds the request already closed when it builds the note sends none. A notice already handed to the asker's poll is not taken back, and a webhook or email asker may already have been woken for one that is later closed. Expiry notices are never linked and always deliver.

## Progress notes

`POST /v1/requests/{id}/progress` accepts `{"note":"calling the restaurant now"}`. Only the current target with an active claim may post (409 otherwise). Notes must be nonblank and at most 1024 UTF-8 bytes (413 when larger). Each post replaces the previous note and renews the claim lease (30 minutes by default); a claimed notify remains lease-free.

Get and trace results include optional `progress: {"note":"...","at":"<RFC3339 timestamp>","by":"muse"}` while claimed. A get by the asker also includes optional `relay_note` in the same shape, with `by` set to `relay`, once the relay has added a note for the asker (see Notes to the asker); older relays never send it and older clients ignore it. A held get refreshes this at timeout. Requeuing clears the note. Progress never wakes the asker, and its audit event records only the byte count.

`GET /v1/capabilities` advertises `"progress": true`. Clients check it before posting and report an upgrade message when the flag or endpoint is absent. Older clients ignore the optional result field.

### Clarifying a request

For an `ask` with a live claim, its target may `POST /v1/requests/{id}/reply` with `{"status":"needs_input","body":"Which restaurant?"}`. Only the target that claimed the request may ask; a queued, delivered, expired-lease, or already waiting request returns 409. A different agent returns 403. Notifies cannot request input.

The response is a normal reply with status `needs_input` (200). This status is non-terminal. The relay appends a clarification exchange, pauses the claim lease, and marks the question unseen. The question ends a held get-reply wait and follows the same poll, acknowledgement, and reply-wake path as a final reply. While waiting, no lease can requeue it; the original request expiry still runs (24 hours by default), and removing either participant cancels it.

Only the original sender may `POST /v1/requests/{id}/answer` with `{"body":"Nopa, 2 people"}`. A different agent returns 403. The request must still be `needs_input` and unexpired, otherwise 409. The relay records the answer, removes the interim reply, and returns the request (200), now `queued` with `resumed: true`. It wakes the same target through the request-wake path. The target polls and claims normally, receiving the original body and full exchange history. The id, target, parent, trace, chain, hop, creation time, and expiry are unchanged. A duplicate answer returns 409.

Each question and answer must contain non-whitespace text and is capped at 16,384 UTF-8 bytes (400 for empty or oversized input). Clarifications carry text only; attachments on a `needs_input` reply return 400. At most three question/answer rounds are allowed per request; a fourth question returns 409, leaving the claim available for a final reply. A final reply uses the existing reply statuses and limits.

Get-reply, unseen replies, and trace steps include an optional top-level `exchanges` list as well as the history on their `request`. On expiry or cancellation, history is retained but the question is no longer returned as a live reply or counted as unseen. Audit events `needs_input` and `answered_input` record only question or answer byte lengths in their detail, never the text. Wake messages contain counts and instructions only.

Relays advertise `"needs_input": true` in `GET /v1/capabilities`. New clients refuse to send `needs_input` or an answer when this flag is missing or false (including a 404 capabilities endpoint), with an instruction to upgrade the relay. Existing ordinary replies work against older relays. An older asker can read the question as the reply body with an unfamiliar `needs_input` status; upgrade that client to answer using `tincan answer <id> "text"` or MCP `answer`. Older handlers never produce the new status. All new fields are optional.

### Acknowledgments stay open

An `answered` reply that only acknowledges the work ("Working on it, full report will follow") would close the request and leave the real answer nowhere to land. The relay holds such a reply instead when all of these are true: the client lists `kept-open` in `X-Tincan-Features`, the replier holds a live claim on an `ask`, the reply has no attachments and no `"final": true`, and its text reads as an acknowledgment. `envelope.LooksLikeAck` decides that last part: at most 280 bytes, opening with a phrase such as "Working on it" or "On it" (optionally after "Got it," or "OK,"), or promising the work later as in "Full report will follow", and with no sign of a result such as "done", "here is", a link, or a finished action like "called" or "booked".

A held ack becomes the claim's progress note and is also kept whole on the request. The relay renews the lease, records a `progress` audit event with `held_ack: true` (byte count only), and returns 200 with `"status":"claimed","kept_open":true`. The asker is not woken. The next reply closes the request as usual and clears the held ack. If the claim lapses or the request expires first, the sweep answers the request with the held ack and wakes the asker. That is the answer it would have had without this rule, only later, and the request is never handed back to its agent, so a reply misread as an acknowledgment is never done twice. The agent's real reply, if it comes after that, still lands once: it replaces the ack, the asker is woken again, and the follow-on (`then`) is not set off a second time. A clarification (`needs_input`), a move or a hold-back drops the held ack.

Send `"final": true` (`tincan reply --final`, MCP `reply` with `final`) when a short note really is the whole answer. The history, council, notes and pass services always send it. Clients that do not advertise `kept-open` get the old behavior.

## Replies the asker has not seen

A reply starts unseen by the agent that sent the request. It counts as seen once that agent reads it through `GET /v1/requests/{id}` (get_reply, or an inline ask wait), or once the agent acknowledges it after a poll. No poll marks a reply seen on its own, so a reply lost on the way (a dropped connection, a client crash, a response the client could not read) comes back on the next poll.

`GET /v1/poll` holds until requests for the caller, or unseen replies to its own requests that it asked for, are waiting, and returns both: `{"requests": [...], "replies": [...]}`, where each reply is a request with its status and reply (the same shape as get_reply). The `replies` field is additive; it is left out when there are none.

| Query | Effect on unseen replies |
|---|---|
| (no `replies` param) | Left out, and they do not end the hold, the same as `replies=none`. Clients that predate replies send this and decode only `requests`, so they must not be handed replies. |
| `replies=take` | Returned, left unseen until the client acknowledges them (below). check_inbox and `tincan inbox` use this. |
| `replies=keep` | Returned, left unseen, never acknowledged. `tincan wait` uses this to end the wait and print a count. |
| `replies=none` | Left out, and they do not end the hold. |
| `peek=1` | Nothing is taken. The response is `{"waiting": <total>, "queued": <requests>}`. When requests are queued it also carries `"pending": [{"id": "...", "from": "...", "rev": <number>, "created_at": "<RFC3339 timestamp>"}, ...]`, naming up to 50 of them, oldest first, without bodies. `created_at` is when the request was sent; relays that predate it leave it out. `rev` is opaque and changes each time a request returns to the queue, so a client can announce the return without seeing the whole queue. With `replies=keep` (or `take`) it also carries `"replies": [...]` and `waiting` counts them; without, replies are not counted. A peek changes no request's state (nothing becomes `delivered` or `claimed`) and marks no reply seen. `tincan listen` and the Claude Code channel (`tincan mcp --channel`) send `peek=1&replies=keep`; the channel never claims, and the model takes the items with check_inbox. `pending` is additive: clients that predate it ignore it. When the caller's project leaves queued requests out (see Projects within one agent), the response also carries `"hidden": <count>` and `"hidden_ids": ["...", ...]`, naming up to 50 of them without bodies, including on a peek with nothing else to report, which then answers 200 rather than 204. |

Any other `replies` value is a 400.

A relay that is shutting down ends every held poll and get-reply wait at once with the answer its deadline would give (204, the upgrade-only 200, or the request's current state), and answers later ones without holding. Clients treat that as an ordinary empty poll and poll again.

`POST /v1/replies/ack` with `{"ids": ["<request id>", ...]}` marks those replies seen and returns 204. Ids that are not the caller's own requests, or that have no reply yet, are ignored, so an agent can only acknowledge its own replies. At most 500 ids per call. `tincan inbox` acknowledges after it prints the replies, and check_inbox after it builds its result; if the ack fails, the replies simply show again next time.

Replies include an additive integer `generation`, a persistent per-request counter that advances for each clarification question or final reply, even within the same millisecond (pre-upgrade replies start at generation 0). Clients acknowledge the generation they displayed with `{"ids": [], "acks": [{"id": "<request id>", "generation": 2}]}`. An `acks` entry marks a reply seen only if its generation still matches; stale entries are ignored. Plain `ids` remain supported with their original behavior (acknowledging the current reply regardless of generation). The two lists may be combined, with a total limit of 500 entries; clients should put each reply in only one list. CLI and MCP inbox clients use generation acknowledgements.

One poll delivers at most 20 requests, oldest first, and stops adding requests once their bodies and clarification exchanges pass 1 MiB (it always delivers at least one); the rest come with the next poll. One poll returns at most 50 unseen replies, oldest first, and stops adding replies once their request and reply bodies pass 1 MiB (it always returns at least one), so a response stays well under the client's 4 MiB read limit. Bodies are not cut. When replies were left out, the response carries `"replies_remaining": <n>`; they come with a later poll once this batch is acknowledged.

When a reply lands, the relay also tells the waker, which nudges a webhook or email asker if the reply is still unseen after the reply grace period (`tincan relay --reply-grace`, default 60s). The nudge carries only counts. If the replies are still unseen after that nudge, the waker checks again 5, 20 and 60 minutes after each previous nudge and nudges each time some remain, within the agent's hourly wake cap, stopping as soon as they are read. A request that stays queued after a webhook or email wake, with no poll since that wake, is nudged again after `--wake-grace` (default 10 minutes) on the same path (or on the next fallback path, below), or after `--urgent-wake-grace` (default 2 minutes) when the agent has an urgent request queued and that is shorter, repeating until the agent polls, the queue is empty, or the agent is removed, sharing that hourly cap. After `--owner-notice-after` (default 3) wakes in a row with no poll, the relay queues one notify from `relay` to the approval policy's `notify` destination per silent episode, naming the agent, when it was first woken, the wake path and the last wake result; with no destination configured it logs once instead. The dedupe is a relay note of kind `owner_wake` on the agent's oldest queued request, keyed by the episode's `woken_at`, so a restarted relay does not resend it. When such a follow-up finds the agent still silent, each queued ask (not a ping or notify) that has waited a full grace since the later of its creation and `woken_at` (the urgent grace for an urgent ask) gets a note for its asker, once per request (see Notes to the asker). The note names when the agent was woken, the wake result, and up to five teammates online now other than the asker and the agent. The request keeps its status; a wake follow-up never cancels or reroutes it (only failover moves an ask off a web agent that is out of messages, and the owner's fallback moves an untaken ask off an agent that wakes itself). The wake text itself carries only counts: it tells the agent to run `check_inbox` before other work, and when any waiting request is urgent, to run it first and handle the urgent request before anything else this turn. A later HTTP 2xx does not move `woken_at` while the agent is still silent. An agent's `wake.json` entry may list an ordered `fallback` of further `webhook` or `email` targets, with the same fields and checks as the primary. The first wake of an episode goes out on the primary. Each request follow-up then uses the next path in the list instead of resending the same one, and after the last it cycles back to the primary; reply nudges reuse the path the episode last sent on. A poll since the last wake starts the next episode on the primary again. All paths share the agent's `max_per_hour`. On a 2xx webhook response the relay keeps the first 200 bytes of the response body as one sanitized line (control characters removed, the URL, its path and query values, the bearer token and the HMAC secret and signature redacted, runs of 16 or more base64-like characters shown as `[token]`; a body that still holds an escape sequence or any raw, base64 or hex form of a secret is replaced by `[withheld: may contain a secret]`) in its log and in the `woke` audit detail only, so the operator can tell a queued run from an error the platform answered with a 2xx. A `woke` audit row also names what the wake covered when it was sent: the requests still queued for the agent and its own requests whose replies it has not read. Its detail is `<path>, HTTP <code>, <n> waiting[, <n> unseen replies][, ids: <id>,<id>,...][, response: <summary>]`, where the counts are the lengths of those two lists. A request-email wake's detail is `email, HTTP <code>, <n> open asks, <n> request emails[, <n> other requests waiting][, <n> unseen replies][, ids: ...]`, and its ids are the asks it emailed, then the other queued requests and the unseen replies. When the wake covered exactly one request or reply, its id is in the row's `request_id` and the detail has no `ids:` segment. With two or more, `request_id` is empty and the `ids:` segment lists every one, queued requests first (oldest first), then the requests with unseen replies, comma-separated and never cut. No ids means the wake covered none, or the row predates this format. The grace and follow-up timers live in memory, so a relay that restarts schedules a fresh reply nudge for every webhook or email agent that still holds unseen replies, and a request nudge (counted when it fires, after the usual debounce) for every one that still has queued requests.

### Sessions within one agent

Several clients can share one agent name, for example one `tincan mcp` per Claude Code window on the same machine. A client can name its session with the `X-Tincan-Session` header on every call: up to 64 bytes of letters, digits, `.`, `_` and `-`. Any other value is a 400 on every route that reads it (send, claim, poll). `tincan mcp` sends a fresh id for each process; `TINCAN_SESSION`, when set, names the session for any tincan command. Other CLI commands send none. Relays that predate sessions ignore the header.

- The relay stores the sender's session on each request it queues, and the session that claims a request on the claim. Claiming again while claimed keeps the first session.
- A poll or peek that names a session returns and counts only unseen replies to requests sent from that session or sent with no session. So the channel notice fires only in the window that asked, and `check_inbox` in another window cannot take its reply. A poll with no session sees every reply, as before.
- A session that makes no call to the relay for 10 minutes (its window closed, or its MCP server restarted and took a new id) counts as gone, and replies to its requests open to the agent's other sessions, so the next one to poll gets them instead of nobody. A channel MCP server peeks every 25 seconds and one without a channel calls `whoami` every 2 minutes, so a running window never looks gone. The relay keeps session activity in memory, so for 10 minutes after it starts every session counts as running. `GET /v1/requests/{id}` and a sessionless poll (`tincan inbox`) read any reply at any time, and acknowledgements stay agent-wide.
- An ask with no `parent_id` is filed under (gets the `parent_id` and `trace_id` of) only an open claim made from the same session or with no session. When the agent-wide rule (exactly one open claim) would continue a claim from another session, the ask still takes that claim's `chain` and `hop`, so the cycle check, the hop limit, and allowlists see what they saw before sessions; only the trace is new.
- Wake, the roster, and the audit log stay per agent and do not show sessions.

`tincan listen --exec <cmd> --per-request` uses only these existing routes and adds nothing to the wire. It reads `pending` from a peek and claims each entry of kind `ask` with `POST /v1/requests/{id}/claim` from a fresh session (`worker-<hex>`), then hands that session to the process it starts as `TINCAN_SESSION`. The claim records the session, so the process's own asks continue its own request's chain while other workers hold other claims. While the process runs, the listener claims again every 2 minutes. A claim on a live claim renews the lease and keeps the first session, and a claim on a request whose lease lapsed takes it back under that session. It stops once the request is closed (409) or has gained a clarification question, and a process that left the request open gets `failed` from the same session. A relay whose peeks carry no `pending` kinds starts no workers, and the listener logs that once.

### Projects within one agent

An ask can name the project of the session it is for, so it reaches the window on the right repo. A client advertises its project with the `X-Tincan-Project` header on every call, in the same format as the `project` field of a request. Any other value is a 400 on the routes that read it (poll, claim). The header is optional. `tincan mcp` sends the name of the git repository around its working directory, with a linked worktree counting as its main repository; `--project <label>` sets it and `--no-project` sends none. Set the request's field with `tincan ask --project <label>` or the `ask` tool's `project`.

| Request `project` | Caller's project | Queued 10 minutes | Taken |
|---|---|---|---|
| none | any | n/a | yes |
| set | same | n/a | yes |
| set | different or none | no | no |
| set | different or none | yes | yes |

- A poll, a claim by id and a peek all apply this table. A peek's `queued` and `pending` hold only what the same caller's poll would take, so a window that has nothing to take stays quiet; `hidden_ids` gives the ids of the rest, so a client can tell them from requests that left the queue. A claim that the table refuses is a 409.
- Fallback goes by how long the request has been queued, not by whether a session of the project looks live, because an open idle window keeps calling the relay. The clock starts when the request is queued, restarts when the owner approves a held request or the asker answers a clarification, and keeps running when a delivery or claim lease lapses.
- A client that sends no header takes a request that names a project only after fallback, and takes everything else as before. A relay that predates projects ignores the header and the field, and any session takes the request.
- `tincan listen --per-request` sends no header, so an ask that names a project reaches its workers only after fallback, about 10 minutes later. Leave `project` off an ask to an agent that runs this way.
- A request email goes to no session, so an ask that names a project is emailed only after fallback.
- A wake stays per agent and carries no label, so a webhook or email agent is woken for such an ask at once, and again when it falls back, since its inbox shows nothing before then.
- A move (see Failover and Fallback) drops the label, since it named a session of the old target. A group names the project for each target. A room mention names none.
- A schedule cannot name a project (400, see Scheduled asks), and a follow-on carries no label, though the first ask of a `then` can name one, so a scheduled ask or a follow-on goes to any session of its target.
- Labels the relay sees are kept in memory only. The label a request names is stored on the request. A target reads it as `project` on the request, except on a request held and never approved.

### Request emails and email replies

An email-method agent with `"include_requests": true` in `wake.json` gets one email per open ask (queued, delivered or claimed; never held, ping or notify) instead of the count-only text. The subject is `Agent Tincan: request from <asker> [tincan <id>.<tag>]`. The body carries the request id, asker, urgent flag, the request text up to 64 KiB, earlier clarification rounds, an attachment count, and reply instructions. A new ask is emailed on the wake debounce, even while a follow-up for older asks is pending. Follow-ups re-send only asks whose last email is at least one wake grace old, never-emailed asks first. `max_per_hour` counts wakes, not emails. Fallback paths keep the count-only text.

While such an agent has an open ask, and for 24 hours after its last one closed, the relay polls the sending AgentMail inbox every 30 seconds. A tagged message from the agent's `email_to` that AgentMail did not mark spam or unauthenticated is recorded as the agent's reply when the tag verifies and the request is open and not under a live claim lease. A first line of `failed:` or `declined:` sets that status; otherwise the reply is `answered`. The asker sees it through `get`, `check_inbox` and its reply wake like any reply, and the audit row is `replied` with `via: email`. Each tagged message gets one in-thread "recorded" or "not recorded: <reason>" email. If AgentMail cannot take that email for 24 hours, the relay stops retrying it; the agent, having heard nothing, answers through tincan as its instructions say. An email reply is checked against the clarification round its tag was minted for, and a request whose clarification is answered gets a fresh request email with a new tag. An email reply is not a poll: the agent's last check-in does not change. See [trust-model.md](trust-model.md#email-replies) for the checks and their limits.

## Search

`GET /v1/search?q=<text>&limit=<n>` searches stored request and reply bodies. The relay resolves the caller from Tailscale just as for trace: agents see matches in chains where they sent or received a request at any step; admins see all chains. Search does not mark replies seen or change request state.

`q` must be nonblank and at most 4096 bytes. Words are quoted as literal FTS terms, all of which must match (case-insensitively); punctuation is ignored, and operators such as `OR` have no special meaning. Punctuation-only queries return no matches. `limit` defaults to 20 and must be 1 to 50; invalid input returns 400.

The response is `{"results": [{"request_id", "trace_id", "from", "to", "status", "created_at", "snippet", "reply_snippet", "question_snippet", "attachment_names"}]}`, newest request first (in the order the relay stored them). `snippet` and `reply_snippet` are optional short excerpts of the request and final reply bodies, respectively, with matches in square brackets; each is present only when its column matched. `question_snippet` is an optional excerpt of the request's latest clarification question (a `needs_input` reply), whether it is still waiting, answered, or left behind by expiry or cancellation; a question is an exchange on the request and never appears as `reply_snippet`. Older clients ignore it. An agent’s search starts from its own chains: it walks the requests in chains the agent took part in, newest first, 5,000 at a time, one short query per batch, until the limit is filled or its history is exhausted. Requests it cannot see are never examined, and older visible matches are still found. An admin search covers the whole index and considers at most the newest 2,000 matches (by request rowid, which follows insertion order), so very old matches for common terms may be omitted there. `attachment_names` is optional and contains names from both the request and reply; neither names nor file contents are searched. No matches returns `{"results": []}`.

The relay advertises `"search": true` in `/v1/capabilities`. Older relays without the route return 404; the client reports that the relay needs upgrading. The migration creates the index and triggers. On open, existing bodies are indexed in batches of at most 500 requests per transaction, recording a high-water rowid atomically with each batch. Backfill errors are logged and do not fail startup; search returns what is indexed so far, and backfill resumes on reopen. New requests and replies are indexed in the same transaction that stores them. Successful searches write a `search` audit event with only `{"count": <returned result count>}` in its detail, never the query or snippets.

The MCP `search` tool accepts `query` and an optional `limit` and returns the same results array as `tincan search <text> --json`.

## Attachments

Requests and replies can carry images and small files. The file goes to the relay first, and the message names it by id.

`GET /v1/capabilities` says what the relay supports: `{"attachments": true, "max_attachment_bytes": 10485760, "max_attachments": 8, "search": true, "then": true}`. A relay that predates attachments answers 404 there, and it would also drop an `attachments` field without a word, since it decodes sends and replies with unknown fields ignored. So the tincan client checks this first and refuses to send attachments to a relay that does not report `"attachments": true`. A relay reports false when it has no place to keep files.

`POST /v1/attachments?name=<display name>` uploads one file as the calling agent. The body is the raw file and `Content-Type` its media type; when that is missing, unparseable, or `application/octet-stream`, the relay detects the type from the first bytes. The name is display metadata only, reduced to its last path element; the relay stores the file by id. The response is `201` with `{"id", "name", "mime", "size", "sha256"}`. This is the one route not held to the relay's 1 MB body limit; it has its own 10 MB limit and a 5 minute read deadline.

| Limit | Value | Refusal |
|---|---|---|
| Per file | 10 MB | 413 |
| Per message | 8 attachments | 400 |
| Per uploading agent, kept at once | 200 MB | 413 |
| Relay-wide, kept at once | 1 GB | 507 |
| Free disk after the upload | at least 512 MB | 507 |

Quota is reserved before the file is written (the `Content-Length`, or the full 10 MB when it is absent), so concurrent uploads cannot pass it together. Deleted files stop counting.

To send, name the ids in the send or reply body: `"attachments": [{"id": "..."}]`. The relay accepts only the sender's own finished uploads (403 for another agent's upload, 400 for an unknown id), and each upload rides on one message (409 if it is already on one; upload it again to send it again). The relay fills in `name`, `mime`, and `size` from the upload and ignores any the client sends.

`GET /v1/attachments/{id}` returns the file with its media type, `Content-Disposition: attachment`, `X-Content-Type-Options: nosniff`, and `X-Tincan-SHA256` (the client checks the bytes against it). It is served to the uploader, to the sender and target of the request that carries it (on the request or on its reply), and to admins. Anyone else gets the same 404 as for an unknown id. A file retention has removed answers 410.

Retention runs when the relay starts and hourly. An upload no message carries is deleted after 24 hours. A file on a request is deleted 7 days after the request reaches a final state (`answered`, `failed`, `declined`, `expired`, or `cancelled`); its metadata row stays, marked deleted, so the message still lists what it carried.

Files live in an `attachments` directory (0700, files 0600) beside the relay database. Uploads and fetches are audited as `attachment_uploaded` and `attachment_fetched`.

### Owner approval

With an owner-configured `approval.json`, sends can enter non-terminal `held`.
A held send response has an optional `status: "held"` field; ordinary send
responses remain unchanged. Get-reply and trace report `held` in their existing
status field. Older clients can decode this unknown string and keep waiting.
Held requests are excluded from poll, peek, queued counts, claim, reply and
wake delivery. The sender can get or cancel them. Other agents, including the target, see
the placeholder body and no attachments in get and trace. This restriction
persists until approval, as specified below. Admin devices can inspect them.

Admin devices and the local admin socket have these endpoints; agent callers
receive 403:

- `GET /v1/admin/held`: array of request envelopes, with bodies truncated to
  200 Unicode characters, including sender, chain and target. A held follow-on
  or guest ask keeps its whole body. Those whole bodies and any `then` text
  share 3 MiB of the response, oldest first, so it stays under
  the 4 MiB a client reads. Past that, the text is cut to 200 characters with a
  note saying how much is not shown. Each entry may
  also carry `to_kind`, the target's kind; for a target of kind `pass`, `pass`
  (`{"source", "conversation_id", "to"}`, parsed from the whole body as the
  pass service will read it) or `pass_error` (why it will fail). A tailnet
  caller also gets `approve_nonce`, `deny_nonce` and `nonce_expires_at` on
  each entry, bound to its device. Every tailnet caller gets them, because
  over plain http a browser's same-origin GET carries no `Origin` and no
  `Sec-Fetch-*` and cannot be told from the CLI; the local admin socket gets
  none. Older clients ignore them all.
- `POST /v1/admin/requests/{id}/approve`: returns the queued request. A fresh
  normal TTL starts; the original `created_at` is kept. Normal wake follows.
- `POST /v1/admin/requests/{id}/deny`: optional `{"reason":"..."}` body;
  returns `{"status":"declined"}`. The reply is attributed to `relay` and contains
  the reason. Missing reason means an empty reply body.

Approval and denial require an unexpired held request; other states return 409.
Approving a request whose text the listing cut answers 412 until older requests
are decided and it fits. Denying one needs no such wait.

A browser caller must also send `{"nonce":"..."}` (with `reason` on a deny),
the matching nonce from its own held listing. A nonce is bound to the request,
the decision and the caller's tailnet node, lasts 10 minutes, works once, and
dies when the hold ends. A browser decision answers
`{"id":"...","status":"queued"|"declined"}`; when the hold had already ended
(the nonce was used, or another device or the CLI decided first) it answers 200
`{"id":"...","status":"<final status>","already_decided":true}`, and with a
missing, expired or mismatched nonce on a request that is still held it answers
409. A browser caller is one that sends `Origin` or any `Sec-Fetch-*` header,
as every browser does on a POST. Clients that send no browser headers need no
nonce and see no change. The
`approved` and `denied` audit details name the deciding device
(`{"device":"<machine name>"}`) for calls from the tailnet.

Push alerts for held asks (admin devices and the local admin socket only;
404 on a relay without push):

- `GET /v1/admin/push/key`: `{"public_key":"<base64url VAPID key>"}`, the
  `applicationServerKey` a browser subscribes with.
- `PUT /v1/admin/push/subscription`: a browser `PushSubscription` as JSON,
  `{"endpoint":"https://...","keys":{"p256dh":"...","auth":"..."}}`. The
  endpoint must be https on `web.push.apple.com`, `fcm.googleapis.com`,
  `updates.push.services.mozilla.com` or a `*.notify.windows.com` host, with
  no port or user info (400 otherwise); `p256dh` is a 65-byte uncompressed
  P-256 key and `auth` 16 bytes. The relay keeps one row per endpoint, so
  the page sends it on every open. Returns `{"id":"<subscription id>"}`.
- `DELETE /v1/admin/push/subscription`: `{"endpoint":"..."}`; returns
  `{"removed":true|false}`.

For each held send the relay queues an alert round to every subscription in
the background (a held send's response never waits for it): an aes128gcm Web
Push message with `TTL: 3600`, `Urgency: high`, `Topic: tincan-held` and the
JSON payload `{"count":<held>}`, the number of requests held when the round
goes out, at least 1. The payload carries no text or link: the page's service
worker supplies the notification text and opens `/phone/#held` when it is
tapped. Held sends that arrive close together share one round: a send while a
round is already waiting joins it, and that round's count covers them all.
A 404 or 410 deletes the subscription (reason `gone`), and so does a 401 or
403 (reason `rejected`): the push service no longer takes the relay's VAPID
key for it, as after the key changes, and the page subscribes again under the
new key on its next open. One admin device keeps at most 5 subscriptions; a
new one past that deletes the device's least recently refreshed ones (reason
`over cap`). Audit events, with subscription ids only: `push_subscribed`,
`push_unsubscribed`, `push_sent` (`status`, or `error` of `timeout`,
`address refused` or `send failed`).

Kind `phone` is the owner's phone, joined from the phone page as its own
agent: the page mints an invite with `POST /v1/admin/invite`
`{"name":"phone","kind":"phone","new_only":true}` and redeems it with
`POST /v1/join` from the same device, then asks through `POST /v1/send` and reads replies through
`GET /v1/requests/{id}` and answers clarifying questions through
`POST /v1/requests/{id}/answer`, naming itself with `X-Tincan-Agent` like any
client on a machine with several agents. Its wake method is `none` and its
stock good-at line is `the owner's phone; asks only, never answers`. A phone
agent asks and never answers, so it is never a failover target or a room
mention target; it may be a room member. The relay adds no route for it.
`POST /v1/send` to a phone agent, of any kind, is refused with 400 `phone
agents ask; they do not take requests`, since the page reads no inbox.
Replies to the phone's own asks are unaffected: they go back through
`POST /v1/requests/{id}/reply` and the page reads them by id.

`new_only` on `POST /v1/admin/invite` makes the relay refuse with 409 when
the name belongs to an agent bound to another machine, instead of minting a
code that would move that agent to whoever redeems it. The code remembers
`new_only` and the minting device, and `POST /v1/join` checks the name again
when the code is redeemed, under the same lock as the join: if by then an
agent bound to another machine holds the name, the join is refused with the
same 409 and the code is spent. The caller's own agent
(bound to the calling device) may be invited again, and from the local admin
socket any existing name is refused. Without `new_only`, as `tincan invite`
sends it, an invite for an existing name still moves that agent on join.
An invite with `guest` is new-only whatever else it says; see [Guests](#guests).

`GET /phone/` and the files under it are the [phone page](phone.md), served
to admin devices only (403 otherwise), on the relay's tailnet listeners and,
when the tailnet issues the relay a certificate, on port 443 under its
certificate name (unless the API listener itself is on port 443). `GET /phone`
redirects to `/phone/`.
The sweep expires overdue holds and logs `hold_expired`. Holds survive restarts
with their original deadline. Policy changes apply only to future sends.
A `ping` is never held: it has no body and runs no model work. Search results
leave out a held, never-approved request for every caller but its sender and admins.
The other transitions are audited as `held`, `approved`, and `denied`.
An optional relay-authored operator `notify` bypasses the gate and is logged
as `approval_notified`; it never grants the notified agent admin access.

With no `approval.json` entry for them, asks and notifies to agents of kind
`council`, `dot-web`, `duet-web` and `pass` are held by default (a failed kind lookup holds).

Requests retain whether they were ever held and whether the owner approved them.
For every request that was held and never approved, only its sender and admins
may read its body or attachments, regardless of its current status (including
declined, expired and cancelled). Other callers, including the target, see
`waiting for the owner's approval` and no attachment metadata wherever the
request is otherwise visible: get, trace, search-like listings, poll and peek
pending entries. Attachment downloads by those callers return 404. Held requests
remain excluded from delivery; pending entries carry only ids and senders.
An approved request follows the ordinary body and attachment access rules,
even after it reaches a terminal state. Approval history survives relay restarts;
upgrades backfill existing holds and decisions from request state and audit events.

## Request groups

A send may include an optional `group` string of 1 to 64 ASCII letters, digits,
underscores or hyphens. The relay stores and echoes it on the request.
Groups do not change identity, parent/chain checks, rate limits, wakes,
allowlists, leases or attachment ownership: each target receives an ordinary
request and requires its own uploads. An urgent group send marks every member
urgent, and each member uses its own urgent slot.

`GET /v1/groups/{id}` returns only membership: an array of `{"id": "...", "to": "..."}`
for requests sent by the authenticated caller with that group tag. It includes
no bodies or replies and never marks replies seen. Fetch each result through
`GET /v1/requests/{id}`. An unknown group, or a group with no
requests sent by the caller, returns 404. Recipients and admins do not gain
access to another sender's group through this endpoint.
`GET /v1/capabilities` advertises `"groups": true`.

Clients generate ids prefixed with `group-`, deduplicate targets and cap fan-out
at 8. Older relays ignore the optional field. Clients retain the individual ids
in memory so combined polling still works in the original client instance;
across client restarts, use individual ids or upgrade the relay. Failed sends
are local result entries and are not stored as requests on the relay.
Per-target polling failures appear as `error` text on the combined result entry,
preserving its request id and last known status alongside successful results.
The relay accepts at most 8 requests per sender and group tag; further sends
return HTTP 400. Membership lookups return at most 8 ids and targets.
Clients reconcile relay membership with local send errors, recovering requests
whose send response was lost while retaining errors for targets absent from the
relay. Concurrent group polls preserve the most advanced cached status and replies.
A multi-target MCP notification returns a tool error if any upload or send fails,
with the per-target results included in its content.

Group text output includes the group id and every accepted request id.

Urgent sends have a separate per-sender rolling hourly limit (default 5, configured by `tincan relay --urgent-per-hour`). Exceeding it returns HTTP 429: `urgent limit reached; send without --urgent`. Ordinary sender limits still apply. Sender limits are in memory and reset on relay restart. The optional `urgent` field is also returned on pending request summaries, and a peek carries `"urgent": <n>`, the count of all queued urgent requests (left out when zero), so a channel notice counts them past the 50 that `pending` lists. A send the relay fails to queue (a bad attachment, for example) does not use up an urgent slot. The relay refuses to start with `--urgent-per-hour` below 1. Old clients and relays can ignore this additive field.

### Available client upgrades

`GET /v1/whoami` and both full and `peek=1` responses from `GET /v1/poll` may include `"upgrade_available": "0.5.5"`. This is the release served by the relay's `--dist` VERSION file, distinct from the relay executable's `relay_version`. It is included only when newer than the caller's `X-Tincan-Version` and the dist holds the binary for the caller's `X-Tincan-Platform` (`<os>_<arch>`, for example `darwin_arm64`, sent by every client), so an agent is never told to run a `tincan upgrade` that would fail. The relay reads `VERSION` on each check, so an in-place edit takes effect at once. Missing or invalid versions, development builds, and relays without dist produce no field. Prerelease clients are skipped unless dist itself is a prerelease; comparisons ignore build metadata and accept an optional leading `v`.

A poll with no messages holds until its normal deadline, then returns HTTP 200 with the upgrade field and empty `requests` (or zero `waiting` and `queued` for peek), instead of 204. Populated polls carry the same optional field. `tincan wait` continues waiting on empty polls with an upgrade; it prints the notice when a request or reply ends the wait. The relay repeats it on every response; clients display the actionable notice at most once per process per available version. Unknown fields are safe for older clients to ignore. Notices neither claim requests nor acknowledge replies, and no client upgrades automatically.

### Wake history export

`GET /v1/admin/wakes/{agent}?since=<RFC3339>[&until=<RFC3339>]` lists the agent's relay wakes in the window for admins (403 for anyone else): `{"agent","since","until","wakes":[...]}`, each entry with `at`, `event` (`woke`, `wake_failed` or `wake_skipped`), `path` (`webhook` or `email`), `status` (the HTTP code, or `2xx` for `woke` rows written before the code was recorded), `reply` (the webhook's reply summary, or `not recorded`), `error`, `next_poll`, `next_via` (`poll`, or `delivered` / `claimed` for older rows) and `request_ids` (the requests a `woke` row covered, from its `request_id` or its `ids:` segment; left out for rows without ids). Polls are read from a `polled` audit row the relay writes on the first poll after each wake; a poll made while the wake call is still answering gets its row when the call ends, after the wake's own. A poll's time is when it happened, also for one written after its wake call ended. One export lists at most 1000 wakes; when it hits that bound the response has `"truncated": true` and lists the oldest, and a later `since` shows the rest. Polls and deliveries do not count toward the bound, and a wake's next poll is found even after `until`. No URL, token or key appears. `tincan wakes <agent> --since <time>` prints it as a table or, with `--json`, as JSON.

### Webhook wake configuration

Admin devices and the local admin socket can manage webhook wakes without restarting the relay:

- `GET /v1/admin/wake-config` returns a map of configured agent names to method names. URLs, addresses and authentication fields are never returned.
- `PUT /v1/admin/wake-config/{agent}` takes `{"webhook":"https://host/hook","bearer_token":"optional secret"}` and returns `{"agent":"name","method":"webhook"}`. Omit `bearer_token` to preserve it; an empty string clears it. The agent must be joined (404 otherwise). Invalid input returns 400, non-admin callers get 403, and a relay without configuration support returns 501.

The relay validates an absolute HTTP(S) URL without user info or a fragment, serializes updates, and atomically replaces its private `wake.json` with mode 0600 before changing the live settings. Unrelated agents and existing HMAC, fallback and rate settings are retained. Pending edits in the file are preserved on disk; only the requested webhook URL and explicit token change are applied live, so email sends and their startup reply inboxes stay consistent until restart. Failed writes leave the live configuration unchanged. Converting an email wake still requires editing the file and restarting, because its reply inboxes are registered at startup. The CLI surfaces are `tincan wake set <agent> --webhook <url> [--bearer-token-stdin]` and `tincan wake list`.

### Relay self-upgrade

`POST /v1/admin/relay/upgrade` installs a release over the relay's own binary. Only admin devices and the local admin socket may call it; agents and other callers get 403. The optional body is `{"force": false, "from_github": "v0.8.0"}`.

With `from_github`, the relay first downloads that tag's `checksums.txt` and every `tincan_<os>_<arch>` (`tincan_windows_<arch>.exe` on Windows) it lists from its release URL (`tincan relay --release-url`; the request cannot change it, and without the flag the relay refuses `from_github` with 422), checks each binary against it, and moves them into `--dist`, `VERSION` last, restoring the old files if a move fails. Then it reads `--dist/VERSION`, checks `tincan_<its os>_<its arch>` against `--dist/checksums.txt`, keeps the old binary as `<binary>.<old version>`, and renames the new one into place.

Success returns `{"from": "0.7.0", "to": "0.8.0", "restart": "re-exec"}` (or `"exit"` for a relay started with `--upgrade-exit`), writes a `relay_upgraded` audit event with the two versions and the source (`dist` or `github`), and only then restarts the relay: it drains, closes its store, and re-executes itself or exits with status 75. Errors change nothing:

- 404: the relay has no `--dist`, or predates this route (a plain `404 page not found`);
- 409: the release is not newer than the relay's build and `force` is false, an upgrade is already pending a restart, or the relay user cannot write its binary or the directory holding it;
- 422: no valid `VERSION`, no binary for the relay's platform, no `checksums.txt` entry for it, or a checksum mismatch;
- 502: the release download failed.

## Guests

A guest is someone outside the team, on another tailnet, who may ask the teammates the owner listed until a set time. The relay advertises `"guests": true` in `GET /v1/capabilities`, and the client checks it before inviting, then checks that the relay echoed `guest_until` before it prints a code, so an older relay that dropped the `guest` field cannot turn the code into a full teammate's.

`POST /v1/admin/invite` takes an optional `guest` object: `{"name": "friend", "guest": {"ask": ["muse", "instinct"], "for_seconds": 259200, "no_hold": false}}`. The answer adds `guest_until`, `guest_asks` and `hold_alerts`, which is false when nothing would tell the owner a guest's ask is held. It is refused with 400 when `ask` is empty, when the term is not above zero or is over 30 days, with `kind`, or when a listed name is not a joined teammate (a guest, a phone and the history, notes and pass services cannot be listed). It is refused with 400 on a relay started without `--admin-login`, since machine names alone could not tell a guest from an admin device. A guest invite is always new-only: it answers 409 for a name that is taken, on any machine, and `POST /v1/join` checks again when the code is redeemed.

A name does not change tier. `POST /v1/admin/invite` without `guest` answers 409 for a guest's name, connecting the gateway to one does too, and a guest invite for an existing teammate does the same. `tincan remove` frees the name. A machine carries one guest or only teammates: `POST /v1/join` answers 409 for a second guest's code or a code of the other tier. A machine carrying a guest is never an admin device, whatever its name or login, and is never re-admitted to a new machine's name.

A wrapper in front of every route answers 403 with `guests may not use this route` to a guest unless the route is on this list. A route added later is refused until someone adds it.

| Route | For a guest |
|---|---|
| `POST /v1/send` | See below. |
| `GET /v1/poll` | Replies to its own asks and the relay's notices. |
| `POST /v1/replies/ack` | As for anyone. |
| `GET /v1/requests/{id}` | Its own asks only, as for anyone. |
| `POST /v1/requests/{id}/claim` | Relay notices only; 403 for anything else. |
| `POST /v1/requests/{id}/answer` | As for anyone. |
| `POST /v1/requests/{id}/cancel` | As for anyone, except that a gateway guest gets 409 on a request that was claimed before or whose question was answered. |
| `GET /v1/agents` | Its listed teammates, with `name`, `online`, `wake` (always `none`), `kind` and `good_at` only. |
| `GET /v1/whoami` | `name`, `kind`, `guest_until`, and never `relay_key` or `relay_urls`. |
| `GET /v1/hello` | As for anyone. |
| `GET /v1/capabilities` | Fixed flags, as for anyone. |

`POST /v1/join` is not on the list, so a machine carrying a guest cannot join again under any code. Past `guest_until`, every route answers 403, including this list, before the sweep runs, and `POST /v1/send` and the answer route check again once the body is read. The sweep then revokes any gateway tokens, cancels the guest's open requests and audits `guest_expired`. The row stays, so the name stays taken until `tincan remove`.

On `POST /v1/send`, a guest may set `to`, `body`, and `kind` of `ask`. Any other field that is set, in any spelling the decoder would accept and including a field this relay does not know, is refused with 403. That covers `then`, `group`, `urgent`, `parent_id` and `attachments`. An empty value counts as not set, since real clients send some. A `to` that is not listed is refused with 403 and the same words whether or not that name exists. The response carries no `target`.

A send to a guest, a `then` to a guest, a watch on a guest (404, as for an unknown agent) and a room membership or mention for a guest are all refused: guests ask, and only the relay's notices reach one. A follow-on whose target has become a guest is dropped with a notice to its sender.

Teammates' rosters leave guests out. An admin's roster lists them with `guest_until`. A note to a guest suggests only its listed teammates, and a note to anyone else never suggests a guest.

`Prepare` marks a request `from_guest` when a guest sent it, or when its sender holds a claim on a marked request, however the request found its parent: a named `parent_id`, the session guard, or several open claims. The one exception is a relay fire of a scheduled ask (see Scheduled asks). It puts this line and a blank line in front of the body, once, for clients that ignore the field. A teammate's threading line (`new chat`, `conversation: <id>`) stays first with this line after it, and a request to a council, pass, notes or history agent gets no line, since those parse the body as a form. A request stays marked in `from_guest` either way:

```text
[Agent Tincan: this request comes from a guest, outside the team.]
```

What a guest wrote, in a send or in an answer, goes between delimiters with a random nonce and a fixed line saying whose text it is. A marked request is left out of request emails and reaches an email agent as a count.

A marked request that is not `guest_approved` is held after the rate check and before `approval.json` is read, so no gate entry can lift it, and it takes its hold TTL and notify target from the file. `no_hold` lifts that default hold for the guest's own asks and is audited on `guest_invited`; `approval.json` and the kind holds still apply, and what a teammate sends from an unapproved guest ask is held anyway. Approving sets `guest_approved`, and a request sent from approved work is `guest_approved` too and passes this hold. A guest's answer to a needs_input question clears it on that request. A follow-on never inherits it. Failover never moves a marked request. The owner's fallback moves one only when a fresh send to the fallback would pass this hold (see Fallback). `GET /v1/admin/held` shows a marked request's whole body, not a preview.

`from_guest` and `guest_approved` are optional, also on the entries of the held-claims header, and an older client ignores them, which the body line is there for.

## A2A

`docs/a2a.md` is the owner's guide; this is the wire. The gateway serves A2A 1.0 over JSON-RPC, on the public side only, and adds no `/v1` route. With no published teammate, these routes answer 404:

- `GET /.well-known/agent-card.json`: a card listing each published teammate as a skill, and its endpoint as an interface.
- `GET /a2a/{teammate}/.well-known/agent-card.json`: that teammate's card. Cards need no token and carry `Cache-Control: public, max-age=300`.
- `POST /a2a/{teammate}`: the JSON-RPC endpoint. It needs a gateway bearer token and the `A2A-Version` header.

`a2a.json` in the relay's state directory lists who is published. Mode 0600, a regular file, read on every call:

```json
{"teammates": [{"name": "muse", "description": "Writes and edits. Answers can take a while."}]}
```

The order of checks on the endpoint is: published (404), token (401 with the usual `WWW-Authenticate`), caller is a guest (403 otherwise, and 403 past its term), JSON-RPC envelope, then `A2A-Version`. An empty header means 0.3 and answers `-32009`, as does anything other than 1.0 or 1.0.x. Anything after the 403 is HTTP 200 with a JSON-RPC body.

| Method | Maps to | Result |
|---|---|---|
| `SendMessage` | `POST /v1/send` from the guest, or `POST /v1/requests/{id}/answer` when `message.taskId` names a task in needs_input | `{"task": Task}` |
| `GetTask` | `GET /v1/requests/{id}` | `Task` |
| `CancelTask` | `POST /v1/requests/{id}/cancel`, only for a task in `TASK_STATE_SUBMITTED` | `Task` |

The task id is the request id, and `contextId` is the same id. A new message's `contextId` is ignored, since asks share no conversation; on a continuation it must match the task or the call answers `-32602`. `configuration.returnImmediately` returns at once. Otherwise `SendMessage` waits at most 25 seconds and returns the state so far, so a client must be ready to poll. A held ask returns at once. Only `text` parts are read; message and request `metadata`, `referenceTaskIds` and `historyLength` are ignored and never reach the relay, and `acceptedOutputModes` is not checked.

| Request status | Task state |
|---|---|
| `held`, `queued` | `TASK_STATE_SUBMITTED` |
| `delivered`, `claimed` | `TASK_STATE_WORKING` |
| `needs_input` | `TASK_STATE_INPUT_REQUIRED`, the question in `status.message` |
| `answered` | `TASK_STATE_COMPLETED`, the reply in the artifact `reply` |
| `failed`, `expired` | `TASK_STATE_FAILED`, any reply in `status.message` |
| `declined` | `TASK_STATE_REJECTED`, any reply in `status.message` |
| `cancelled` | `TASK_STATE_CANCELED` |

Reply attachments are not carried, and neither are progress or relay notes.

| Code | When |
|---|---|
| `-32001` | The id is unknown, is another caller's, or went to another teammate |
| `-32002` | `CancelTask` on a task past `TASK_STATE_SUBMITTED` |
| `-32003` | `configuration.taskPushNotificationConfig` was set |
| `-32004` | Another A2A method, a message to a task not in needs_input, or the relay refused the guest, such as a teammate it is not listed for |
| `-32005` | A part with `raw`, `url` or `data` |
| `-32009` | `A2A-Version` is not 1.0 |
| `-32600`, `-32601`, `-32602`, `-32603`, `-32700` | As in JSON-RPC. A call with no `id`, or with an `id` that is not a string or number, is `-32600`, and so is a batch. Anything after the one request in the body is `-32700`. A bad `id` comes back as `null` |

`POST /v1/admin/connect` takes an optional `guest` object, the same shape as on an invite: `{"name": "pal", "guest": {"ask": ["muse"], "for_seconds": 2592000}}`. It binds a virtual guest and answers `code`, `cards`, `unpublished` (listed teammates `a2a.json` does not publish), `guest_until`, `guest_asks` and `hold_alerts`, and audits `guest_connected`. It is new-only: a taken name answers 409, even a guest's. `replace` and `no_hold` answer 400, and so does any list or term a guest invite would refuse. `--admin-login` is not needed, because a virtual guest has no machine name to pass off as an admin device. The relay advertises `"gateway_guests": true` in `GET /v1/capabilities`. The client checks it first and then checks that `guest_until` came back, because a relay without it would ignore the field and bind a full teammate.

A gateway guest's token answers 403 on `/mcp`.

## Agent roster

`GET /v1/agents` returns an `agents` array. Each entry optionally includes `queued` (queued or delivered requests), `oldest_queued_at` (their earliest creation timestamp), and `claimed` (requests with a live claim lease). Requests past their expiry, terminal requests, and pings are excluded. Zero counts and absent timestamps are omitted. Older clients ignore these additive fields; clients reading an older relay show no backlog. The roster remains visible to joined agents and admins; these counts reveal no request content and do not change the trust model.

For an admin caller (an admin device or the relay's local admin socket), each entry also includes `node_id`, `node_name` and `node_user`: the stable id, machine name and owning login of the Tailscale node the agent is bound to. They are omitted for joined agents calling the endpoint, and for virtual agents (ChatGPT, Sesame), which have no node. Older clients ignore them; older relays leave them out.

An entry for an agent on wake method `schedule` (it checks its inbox on its own interval and cannot be woken) also includes `check_every_seconds` (its configured interval), `expect_reply_seconds` (the interval plus a 5-minute grace for a late check), and `overdue` (true when its last inbox poll, or its join or the relay's start when it has none recorded, is more than two intervals plus the grace ago). All three are omitted for other agents and by older relays.

An entry for an agent on wake method `webhook` or `email` that the relay has woken also includes `woken_at` (the last recorded wake: the first send of a silent episode; a later 2xx follow-up does not move it), `wake_result` (`ok`, or why the send and its retry failed: `<host> returned <status>` or `POST to <host> failed: <cause>`, never the URL's path, query or credentials; either one ends in ` (fallback N: <method>)` when the wake went out on fallback path N, for example `ok (fallback 1: email)`) and `unanswered` (true when the agent has not checked in since that wake: the send failed, or more than the relay's `--wake-grace`, default 10 minutes, has passed with no poll). Only a poll is a check-in; other calls, such as a send or a get, do not clear it. The relay keeps the last wake and each agent's last poll across restarts, and after a restart the grace runs from the later of the wake and the relay's start. The agent's next poll clears `unanswered`. Removing an agent drops its last wake. All three are omitted for other agents, for agents never woken, and by older relays.

An entry may also include `good_at`: the owner's line saying what the agent is good at, or, when the owner has set none, a stock line for service kinds (`history`, `notes`, `council`, `pass`, `chatgpt-web`, `claude-web`, `grok-web`, `gemini-web`, `perplexity-web`, `copilot-web`, `dot-web`, `duet-web`) and product-tool kinds (`claude-code`, `codex`, `gemini-cli`, `grok-cli`, `copilot-cli`, `cursor-cli`, `chatgpt`, `phone`). An agent with no stored kind whose name is one of those kind names gets that kind's line. It is omitted for hosting-shape kinds (`vm-webhook`, `e2b-email`, `proxy-sandbox`, `scheduled`, `generic`), `hermes` and `openclaw` until the owner sets one, and by older relays, and older clients ignore it. The relay resolves the stock line, so every client shows the same roster. Agents read it to choose whom to ask; the relay never routes on it.

An entry may also include `working_on` and `working_on_others`: the title of the agent's most recently refreshed session that shares one (`tincan mcp --share-title`), and how many of its other sessions share one too. Both are omitted for an agent with no shared title, and by older relays, and older clients ignore them. The title is the session's own description of itself, for agents to read; the relay never routes on it and it grants nothing.

An entry may also include `self`: `true` on the caller's own entry, so an agent can tell itself from a teammate when several of its sessions share its name. It is omitted on every other entry, when the caller is no joined agent (an admin device that never joined, the local admin socket), when an admin device carrying several agents names none in `X-Tincan-Agent`, when a rebuilt admin device names an agent not yet re-admitted, or names none while another agent from its old machine still waits, and by older relays, and older clients ignore it. The mark grants nothing, and an admin device's read of the roster never re-admits a rebuilt machine: a call made as the agent does that, and the mark follows.

An entry may also include `stats`: the agent's record for asks finished in the last 7 days, as `{"answered": 12, "failed": 1, "expired": 0, "median_reply_ms": 180000}`. It is omitted for an agent with no such ask and by older relays, and older clients ignore it.

`GET /v1/stats?since=<duration>` returns the same record for every joined agent, to any joined agent or admin caller, except that a guest is listed only for an admin caller, as on the roster: `{"since": "<RFC3339>", "agents": [{"agent": "muse", "answered": 12, "failed": 1, "expired": 0, "median_reply_ms": 180000}, ...]}`. An agent with no finished ask appears with zero counts. `since` is a window back from now in days or a Go duration (`7d`, `12h`), positive and at most 365 days, default `7d`; anything else answers 400. `since` in the response is where that window starts. An ask is in the window when its reply was written. An expired ask is dated by its deadline, or by its last progress note if that held the claim past the deadline, never by when a relay that was down got round to expiring it. Pings, notifies, declined and cancelled requests, open requests, held requests the owner never approved and asks that expired in `needs_input`, waiting on their asker, are left out. `median_reply_ms` is the median over answered asks of the time from `created_at` to the reply, or from the `approved` audit event for a held ask, so the owner's delay is not charged to the teammate; it is omitted when no answered ask has a known time and present even when the median is 0, and an even count takes the midpoint of the middle two. The time includes any wait for a clarification answer. The relay reads each window from the store at most once every 60 seconds, so a roster or stats read can be up to 60 seconds old. It reads at most 8 windows other than the default in those 60 seconds; a further window answers 429 with `Retry-After`, and the default window and windows already read keep working. A relay that predates the route answers Go's plain-text `404 page not found`, which `tincan stats` reports as needing a relay upgrade. No field carries request or reply text, a sender or a request id.

`PUT /v1/agents/self/working-on` with `{"text": "Fix flaky relay test"}` sets the calling session's line and answers 204; `DELETE` on the same path removes it and answers 204. Both use the caller's existing authentication and require `X-Tincan-Session` (400 without it). The line belongs to the authenticated agent and that session, never to a name in the body, so no agent can write or remove another agent's line. The body must be exactly one object with only `text`; unknown fields, a second value, or a line that is empty or fails the `good_at` checks (one line of at most 120 characters, valid UTF-8, no control or format characters) answer 400 and store nothing. The relay keeps lines in memory only and drops a session's line once it goes 10 minutes (the session grace) without a refresh, so a client re-sends an unchanged line every few minutes. A relay that predates the routes answers 404 or 405, and the client then stops reporting for the life of its process.

Every entry from a current relay includes `status`, the relay's one word for what the agent is doing, computed in one place so every surface shows the same thing:

- `busy`: the agent holds a request with a live claim lease, or one of its sessions' hosts reports it busy (below). For a claim, `busy_for` names the asker of the agent's oldest live claim and `busy_since` is when that claim began; a host-reported busy names neither. A guest is named to an admin only: for anyone else `busy_for` is left out and `busy_since` stays. The roster never carries a request's text or progress note.
- `stuck`: work waits on the agent and nothing is moving. `status_reason` says which: `claim lapsed` (a claim's lease ran out with no progress note and the relay put the request back in the queue; it stays until the agent claims or answers it), `woken, silent 15m` (a relay-woken agent that has not checked in since its last wake, as `unanswered`), `overdue check` (a schedule agent that has missed its checks, as `overdue`), `3 queued while offline` (work queued for an offline agent the relay does not wake), `3 queued, untaken 25m` (an agent that wakes itself, holding no live claim, with asks it never claimed, the oldest waiting longer than `--wake-grace` since it entered the queue (sent, approved or answered); this wins over a host-reported busy, which only says some session is mid-turn), or `oldest queued 2h` (a request untaken for over an hour). Stuck wins over busy. Nothing is reported stuck in the first claim lease (30 minutes by default) after the relay starts.
- `limited`: a web agent that is polling now and whose site reported it out of messages (see Web authentication observations) until `limited_until`, which the entry carries; `status_reason` is `until 14:00 UTC`. Limited wins over stuck and busy, since nothing it is sent goes out before then. It ends when the reset passes or the agent reports `authenticated`.
- `idle`: none of the above, and the agent is polling now (`online`).
- `offline`: none of the above, and it is not.

`status`, `status_reason`, `busy_for`, `busy_since` and `limited_until` are omitted when empty and by older relays, and older clients ignore them. A watch never fires on `limited`. A request's open clarification (`needs_input`) waits on its asker, not on the agent, so it does not make the agent busy. To mark a lapsed claim the relay adds a nullable `lease_lapsed_at` column to its requests table; an older relay opening the database ignores it.

`PUT /v1/agents/self/status` with `{"busy": true}` reports whether the calling session's host is working right now: `tincan mcp` inside Claude Code forwards the session's own idle or busy state from Claude Code's session registry file, never anything from the conversation. It answers 204, uses the caller's existing authentication, and requires `X-Tincan-Session` (400 without it). The state belongs to the authenticated agent and that session; the body must be exactly one object with only `busy` (400 otherwise). An agent is busy when any of its sessions' live reports says so. The relay keeps reports in memory only and drops one after 10 minutes (the session grace) without a refresh, so the client sends its state on every change and again every 2 minutes, which also restores it within one interval after a relay restart. Removing the agent drops its reports. The relay advertises `"host_status": true` in `GET /v1/capabilities`; a relay that predates the route answers 404 or 405, and the client then stops reporting for the life of its process.

`POST /v1/watches` with `{"agent": "codex"}` asks for one notice when that agent is next free. The watcher is the authenticated caller; the body must be exactly one object with only `agent` (400 otherwise), and an agent not on the roster answers 404. A new watch answers 201 and a repeat of a pending one answers 200, both with `{"agent": "codex", "expires_at": "<RFC 3339>"}`; one watcher has at most one pending watch per agent. The relay checks pending watches on every sweep and right after every reply. A watch fires when the agent's `status` is `idle` ("codex is idle now. No reply needed.") or `offline` with nothing in progress or waiting ("codex is not busy now (offline). No reply needed."); `busy`, `stuck` and any later status do not fire it. Firing queues that text as a `notify` from `relay` to the watcher and marks the watch fired in one transaction, so a restart or a sweep racing the post-reply check sends it once, and audits `watch_fired` with the notice's id and the two agent names only. A watch that has not fired lapses 24 hours after it was set. Removing either agent deletes its watches, so an agent joined again under the name has none. Watches live in a `watches` table with no foreign key to `agents`; an older relay opening the database ignores it and watches stop firing until the newer relay is back. The relay advertises `"watches": true` in `GET /v1/capabilities`, and the client checks it before posting.

`PUT /v1/agents/{name}/good-at` with `{"good_at": "phone calls and restaurant bookings"}` sets an agent's line and returns `{"name", "good_at"}`. Only admin devices and the local admin socket may call it (403 otherwise); an unknown agent answers 404. The line is trimmed and must be one line of at most 120 characters with no control characters (400 otherwise); an empty line clears it, so a kind with a stock line shows it again. The `good_at` field is required: a body without it, or with `null`, answers 400 instead of clearing. The response carries the line as stored. Each change is audited as a `good-at` event. A relay that predates the route answers Go's plain-text `404 page not found`, which `tincan good-at` reports as needing a relay upgrade.

An entry may also include `fallback`: the agent an ask to this one moves to when it sits untaken (see Fallback under Notes to the asker). It is omitted when unset, from a guest's roster, and by older relays, and older clients ignore it. `PUT /v1/agents/{name}/fallback` with `{"fallback": "claude-window"}` sets it and returns `{"name", "fallback"}`; `""` clears it. Only admin devices and the local admin socket may call it (403 otherwise). An unknown agent on either side answers 404. Naming the agent itself, a guest on either side, or a fallback of kind `phone`, `council`, `dot-web`, `duet-web`, `history`, `notes` or `pass` answers 400. The `fallback` field is required: a body without it answers 400 instead of clearing. Each change is audited as a `fallback` event. The value is stored in a nullable `fallback` column on `agents`, kept across a rejoin or rebind, and cleared on every agent that names an agent when that agent is removed. A relay that predates the route answers Go's plain-text `404 page not found`, which `tincan fallback` reports as needing a relay upgrade.

An entry may also include `task_host`: `true` on an agent the owner named a task host (see [Task hosts](#task-hosts)). A named host that may not act as one now also carries `task_host_suspended`, the reason as a sentence fragment, such as `approval.json holds asks to grokbot`; the mark stays stored and the field goes when the reason does. `tincan agents` and `list_agents` print `task_host=yes`, or `task_host="suspended: <reason>"`. Both fields are omitted for other agents, from a guest's roster, and by older relays, and older clients ignore them. `PUT /v1/agents/{name}/task-host` with `{"task_host": true}` names a host and `{"task_host": false}` clears the mark; either answers `{"name", "task_host", "warnings"}`. Only admin devices and the local admin socket may call it (403 otherwise), and an unknown agent answers 404. The `task_host` field is required: a body without it answers 400 instead of clearing. Naming a guest, a phone, a `history`, `notes`, `pass` or `council` agent, an agent of a web chat kind, or an agent `approval.json` gates (or any agent while that file cannot be read) answers 400 with the reason and stores nothing. A clear is never refused for eligibility, so a suspended host can always be cleared. `warnings` is an array of sentences, empty on a clear: one when the agent reaches the relay through the MCP gateway, since the text of every task it reads goes to that vendor; one when its wake method is `command` or `none`, which cannot start a turn; and one for each guest that may ask it. None of them stops the set. Each change is audited as a `task-host` event. The value is stored in a nullable `task_host` column on `agents`. It is kept when the agent's row is written again for the same node, and cleared when the agent is removed, when an invite is redeemed for its name, when it is rebound to a rebuilt machine, and when a gateway connect replaces it. The relay advertises `"task_hosts": true` in `GET /v1/capabilities`. The setter is not gated on the flag: a relay that predates the route answers Go's plain-text `404 page not found`, which `tincan tasks host` reports as needing a relay upgrade.

## Rooms

A room is a named thread the owner shares with chosen teammates. The relay advertises `"rooms": true` in `GET /v1/capabilities`, and clients check it first; nothing about rooms is added to `/v1/send` or `/v1/poll`.

Admin devices and the local admin socket manage rooms; agents cannot:

- `POST /v1/admin/rooms` with `{"name": "launch"}` creates an empty room (201 with the room; 409 if the name is taken). Names follow the agent-name rule: 1 to 32 lowercase letters, digits or dashes.
- `POST /v1/admin/rooms/{room}/members` with `{"agent": "codex"}` adds a member and `DELETE` with the same body removes one. Both answer `{"room", "agent", "changed"}`; `changed` is false when nothing changed. Removing a member cancels its open asks built from mentions in that room (queued, delivered, claimed or waiting on input), each with a relay note for the poster saying why, so a former member answers nothing more from the room. An agent of kind `council` or `dot-web`, any agent `approval.json` names, and the `history`, `notes` and `pass` services are refused with 400 and the reason. Adding a web chat agent, or an agent connected through the MCP gateway, succeeds with a `warning` that what it reads goes to its vendor. A phone may be a member.
- `GET /v1/admin/rooms` lists every room as `{"rooms": [{"name", "members", "created_at", "last_post"}]}`.

Members use their own identity, like any other agent call:

- `GET /v1/rooms` lists the caller's rooms in the same shape.
- `POST /v1/rooms/{room}/posts` with `{"body": "..."}` (at most 16 KiB of UTF-8, not blank) posts and answers 201 with `{"post": {...}, "notices": [...]}`. A post takes one slot of the poster's send rate (429 when none is left), like a send; a blank post (400) or one over 16 KiB (413) is refused before that, and takes no slot.
- `GET /v1/rooms/{room}/posts?after=<post id>&limit=<n>` returns `{"room", "members", "posts", "next"}`: up to `limit` posts (default and maximum 200) after the cursor, oldest first, and `next`, the cursor for the following read. A post is `{"id", "room", "author", "body", "created_at"}`, with `status` and `reply_to` on a mirrored reply and `mentions` (`{"agent", "request_id", "status"}`) on a post whose mentions became asks. With `?ids=<id>,<id>,...` (at most 50 positive post ids; 400 otherwise) it returns those of the room's posts instead, oldest first, with their mentions' current status, and `next` is the `after` cursor unchanged; ids of other rooms' posts, or of none, are left out. A reader uses it to see where earlier posts' mentions stand now: a mention cancelled, denied or expired leaves no new post behind.

Every member route checks, on each call, that the caller is still allowed in rooms and a member: a member that `approval.json` starts holding gets 403 until the owner removes it, a non-member gets 403, and an unknown room 404. Removing an agent deletes its memberships, so an agent joined again under the name is in no rooms; its posts stay under its name. Room posts are not searchable.

An `@name` in a post made through `POST /v1/rooms/{room}/posts` that names a current member becomes an ordinary `ask` from the poster to that member, through every step a send takes: the poster's open claim continues into it (so the loop, hop and allowlist rules see the whole chain), it takes a slot of the poster's send rate, `approval.json` can hold it, and the target is woken like for any ask. The asks of one post form the group `room-<post id>`, so a post asks at most 8 members, and each carries `room_post_id`, the post's id (older clients ignore it and see an ordinary ask). The body names the room, the poster and the post, and says how to read the thread. A web chat member cannot read the thread with tools, so its ask starts with `new chat` and carries the room's recent posts, newest first, framed as context from several teammates, within the site's 32 KiB message limit. Mentions of the poster are ignored; mentions of non-members, of phones (they ask and never answer), of members rooms now exclude, past the 8th, and asks the relay refuses (a loop, say) are not sent and come back in `notices`.

A reply to a mention (`answered`, `failed`, `declined`, or `needs_input`, posted as a question) is posted to the room as the replier's post, with its `status` and `reply_to` (the request id). It is written in the same transaction as the reply, once per request and reply generation, and only while the replier is still a member and still allowed in rooms: the relay checks the replier's eligibility again at reply time (an `approval.json` that now names it, or cannot be read, refuses), and a refused or former member's reply is recorded on the request, as an ordinary reply to the poster, but not posted. A mirrored reply's text is never read for mentions. A mirrored reply over 16 KiB is cut, with a line naming the request that keeps it whole.

A mention's text belongs to its room. A mention continues the poster's chain, so it appears in that chain's trace and search, but only a reader that is a member of the room now sees its text: for any other non-admin caller, `GET /v1/trace/{trace}` replaces the request and reply bodies of every step with a `room_post_id` by `room mention; read it in the room` and drops their attachments, clarification rounds and progress note (ids, statuses, chain and hop stay), and `GET /v1/search` leaves those requests out. This holds for the poster and the target too once they leave the room. Admin callers see everything. A mention is also never in a request email: an email agent with `include_requests` is told of it by count only.

Audit rows `room_created`, `room_member_added`, `room_member_removed` and `room_posted` carry the room, ids, agent names and byte counts, never post text. Rooms live in `rooms`, `room_members` and `room_posts` tables, with no foreign key to `agents`, and a nullable `room_post_id` column on `requests`; an older relay opening the database ignores them, and mentions stop mirroring until the newer relay is back.

## Work leases

A work lease is a named, expiring, advisory claim on a resource, so teammates can see who has taken what. The relay advertises `"leases": true` in `GET /v1/capabilities`. A relay that predates leases answers 404 or 405 on these routes; a client that gets one checks capabilities and reports an upgrade message. Nothing about leases is added to any other route.

A resource is an opaque string: the relay never reads it as a path or a repo. A resource ending in `*` is a prefix and covers everything that starts with the text before the star (`a/**` is the same prefix as `a/*`; a lone `*` covers every resource); any other resource, stars inside it included, is exact. So `a/*` overlaps `a/b`, `a/` and `a/b/*`, and does not overlap `ab` or `a`.

- `POST /v1/leases` with `{"resource": "agent-tincan:internal/relay/*", "ttl_seconds": 7200, "note": "splitting server.go"}` takes a lease for the authenticated caller. `ttl_seconds` and `note` are optional. The body must be exactly one object with only those fields (400 otherwise). `resource` is required, and it and `note` are trimmed and must each be one line of at most 120 characters with no control characters (400 otherwise). `ttl_seconds` defaults to 3600 and must be between 1 and 86400 (400 otherwise). A new lease answers 201 with `{"resource", "holder", "note", "created_at", "expires_at"}`. Acquiring a resource the caller already holds renews it and answers 200 with the same fields and `"renewed": true`: the expiry restarts from now, and the note changes only when a new one is given.
- A teammate's overlapping live lease answers 409 with `{"error": "\"a/*\" is held by muse until <RFC 3339>", "resource": "a/*", "holder": "muse", "expires_at": "<RFC 3339>"}`, where `resource` is the lease in the way. The caller's own leases never conflict with each other. An acquire is one transaction, so of two racing acquires exactly one wins.
- A new lease past the caller's 32 live leases answers 409 with an `error` and no `holder`. A renewal is not refused at the cap.
- `DELETE /v1/leases` with `{"resource": "a/*"}` drops the lease on exactly that resource and answers 200 with it. Only its holder, an admin device that also sends `"admin": true`, or the local admin socket may (403 otherwise, 404 when no live lease has that resource). Without that field an admin device gets no override, so an agent running on one releases as itself; the field gives nothing to a caller that is not an admin. Releasing `a/*` leaves a lease on `a/b` alone.
- `GET /v1/leases` answers `{"leases": [...]}`, every live lease by resource, to any joined agent, admin device or the local admin socket.

An expired lease is skipped by every read at once, can be taken with no call from its holder, and its row is cleared by the sweep or by the next acquire. `tincan remove` clears the agent's leases with it. A rebind keeps them, since the agent keeps its name.

Audit rows `lease_acquired`, `lease_renewed`, `lease_released` (with `reason` `holder`, `admin` or `removed`) and `lease_expired` carry the resource, the holder and, on the first two, the TTL and the note's byte count, never the note. Leases live in a `work_leases` table with no foreign key to agents, since a rebind re-inserts the agent row.

## Scheduled asks

A scheduled ask is an ask the relay sends later, once or on a repeat. The relay advertises `"schedules": true` in `GET /v1/capabilities`. Clients check it before every schedule call and refuse without it (`relay does not support scheduled asks, so nothing was sent`): they never fall back to `POST /v1/send`, because an older relay would send the ask at once.

`POST /v1/schedules` takes the fields of a send plus a time, and answers 201 with the schedule. The owner is the authenticated caller.

| Field | Meaning |
|---|---|
| `to` | Target agent. One agent, checked like a send: an unknown agent answers 404, and a phone or a guest 400. |
| `body` | The ask's text, as for a send. |
| `urgent` | Optional, as for a send. Each fire spends the sender's urgent allowance. |
| `at` | Optional RFC 3339 time of the first fire. Must be in the future. |
| `every_seconds` | Optional repeat, from 900 (15 minutes) to 31622400 (366 days). Without `at` the first fire is one interval from now. |

One of `at` and `every_seconds` is required. `parent_id`, `group`, `also`, `attachments`, `project`, `then` and any `kind` but `ask` are refused with 400: a fire starts a new chain, goes to one teammate, and carries no files, no project label and no follow-on. A caller with an open claim is refused with 409, by the lookup a send uses to continue a chain (the session's own claim, else the agent's one claim). So is a caller with an open marked ask (see Guests) that has reached it, one `queued`, `delivered`, `claimed` or `needs_input` but not `held`, however many other claims it holds, because a fire carries no mark. An agent has at most 32 live schedules; the 33rd is refused with 409. Nothing is checked against `approval.json` here: the owner's holds apply at fire time.

A schedule is `{"id", "from", "to", "body", "urgent", "next_at", "every_seconds", "created_at", "last_request", "blocked"}`. `urgent`, `every_seconds`, `last_request` (the request its latest fire made) and `blocked` (its latest fire was rejected) are left out when empty. Ids are never reused. A schedule that is a task's repeat also carries `task_id`, and a listing shows `Repeats task #<id>.` as its body; it is made by adding a task, not by this route (see [Tasks](#tasks)).

`GET /v1/schedules` answers `{"schedules": [...]}` with the caller's schedules, and with `?all=1` every agent's, for an admin device or the local admin socket (403 otherwise), oldest first. A client that sends `?after=<id>` (0 to start) reads it in pages of at most 500 schedules, each carrying `"next_after": <id>` when more follow; without `after` the whole listing comes back in one response. Each `body` in a listing is a preview, the first 200 characters of the text on one line with control and bidi characters mapped to spaces, so a page stays inside what a client reads of a response at any number of agents; a fire sends the whole text. `DELETE /v1/schedules/{id}` stops one and answers 200 with it, whole text included. A joined agent can stop only its own: anything else answers 404, as an unknown id does. An admin device or the local admin socket can stop any. A request a fire already sent is not touched; cancel it by its id.

The relay fires what is due on every sweep, its periodic pass over expiries and leases, which approve, deny and `GET /v1/admin/held` also run. A fire:

1. Moves the schedule first: a one-shot is deleted, and a recurring schedule moves to its next time after now, in one update conditional on the `next_at` it read. Only the sweep that won that update goes on, so racing sweeps send once. A relay that stops between this step and the next loses that occurrence rather than sending it twice.
2. Skips the occurrence, for a recurring schedule, while `last_request` is open (held, queued, delivered, claimed or needing input).
3. Checks the target as a send does, then sends through the path a send takes: loop, hop and rate checks, the owner's holds, delivery, wake and failover. The request has a new `id` and trace, `hop` 1 and `chain` of the owner alone, whatever claims the owner holds. Those claims do not mark it or hold it as a guest's either: its text was fixed before they opened, since a schedule cannot be made while a marked ask is open with its owner or changed afterwards. Its TTL runs from the fire. Client fields cannot ask for this: only the relay starts a chain this way.

Occurrences missed while the relay was down fire once, then the next time lands after now on the schedule's own cadence, with no catch-up burst.

A rejected fire is not retried. A one-shot is gone and its owner gets a relay notice naming the schedule, the target and the reason, never the text. A recurring schedule stays, sets `blocked`, and tries again at its next time; the notice goes out once per run of rejections, and a fire that goes through ends the run.

Removing the owner or the target deletes the schedule; a task that repeated on it stays and no longer repeats. Audit rows `schedule_created`, `schedule_fired`, `schedule_skipped`, `schedule_blocked` and `schedule_cancelled` carry ids, agent names, times and the reason, never the text. Schedules live in a `schedules` table with no foreign key to `agents`; an older relay opening the database ignores it, and nothing fires until the newer relay is back.

## Tasks

A task is one item on the team's shared list: what is wanted, who has it, and what came back. The relay advertises `"tasks": true` in `GET /v1/capabilities`. Clients check it before every task call and refuse without it (`this relay has no task list; upgrade the relay`). Each attempt at a task is an ordinary `ask` that carries `task_id`, so claims, leases, wakes, progress, clarification and replies work on it as on any ask, and a client that knows nothing of tasks can do the work.

A task is `{"id", "title", "detail", "status", "filer", "assignee", "key", "result", "result_request", "current_request", "ask_status", "agent_assigns", "source", "suggested_assignee", "created_at", "repeat_schedule", "repeat_every_seconds", "repeat_to", "withheld"}`; everything but `id`, `title`, `status`, `filer` and `created_at` is left out when empty. Ids are never reused and a task is never deleted.

| Field | Meaning |
|---|---|
| `status` | `proposed` (waits for the owner's approval), `open` (no ask under it; can be assigned), `working` (an ask is open under it), `question` (that ask's target needs input), `returned` (it carries a result nobody has acted on), `done` or `dropped`. |
| `filer`, `assignee` | An agent name, or `@owner` for the owner. No agent can hold that name, since an agent name starts with a lowercase letter or a digit. An unassigned task has no `assignee`. |
| `key` | The filer's own key. An add with a key the same filer already has on a task that is not done or dropped stores nothing and returns that task. |
| `result`, `result_request` | The latest answer, and the request that produced it. A reply with files adds a line naming them and that request; the files stay on the request, with its retention. |
| `current_request`, `ask_status` | The one ask open under the task, and that ask's status as the task was read (`held`, `queued`, `delivered`, `claimed` or `needs_input`). The ask's status is read from the request, not stored on the task. |
| `agent_assigns` | How many times agents assigned the task since the owner last reset the count. |
| `source` | The outside source its filer declared for its text, such as `mail` or `meeting`: 1 to 32 lowercase letters, digits or dashes, starting with a letter or digit. |
| `suggested_assignee` | Whom the filer asked to have a proposed task go to. Nothing was sent to that agent. |
| `repeat_schedule`, `repeat_every_seconds`, `repeat_to` | For a task that repeats: its schedule's id, its interval, and the teammate each fire assigns it to. They are read from the schedule, not stored on the task. |
| `withheld` | `true` on the row a task host is shown of a task it may not read: one filed as proposed that the owner never approved. Such a row carries `id`, `status`, `filer`, `source` and `created_at`, an empty `title`, and nothing else. See [Task hosts](#task-hosts). |

A history entry is `{"id", "task_id", "actor", "kind", "body", "status", "at"}`. `actor` is an agent name, `@owner`, or `relay` for a change the relay made itself. `kind` is `added`, `assigned` (`body` is the teammate), `status` (`body` is the reason or note), `comment` (`body` is the comment), `progress` (a progress note on the open ask) or `assigns_reset`. `status` is the task's status after an entry that changed it. Entries are appended in id order and never edited or deleted.

A title is at most 200 bytes, a detail 64 KiB, a key 200 bytes, and a comment or note 16 KiB, all UTF-8.

### Agent routes

Any joined agent calls these as itself. A guest gets 403 on every task route.

- `POST /v1/tasks` with `{"title", "detail", "assignee", "key", "source", "every_seconds"}` files a task as the caller; only `title` is required. `assignee` is a teammate, `@owner` for a task that is the owner's own to do, or left out. It answers 201 with `{"task": {...}}`, or 200 with `"existing": true` when the caller's key matched a live task and nothing was stored. An add takes one slot of the caller's send rate (429 when none is left), and an agent with 50 tasks it filed that are not done or dropped is refused with 409. A task that names a teammate is assigned at once, as below; when the relay refuses that assign the task is still stored, open and unassigned, and the answer carries the reason in `assign_error`. A task added with an `assignee` is never in the shared pool below, whatever became of the assign: only its filer and the owner read it until an ask under it reaches a teammate. A task is stored as `proposed`, with nothing sent and the teammate kept in `suggested_assignee`, when it declares a `source` or when an ask marked as a guest's work (see Guests) that has reached the caller is open.
- `GET /v1/tasks` answers `{"tasks": [...], "closed": {"done": n, "dropped": m}}` with the tasks the caller may read that are not done or dropped, oldest first, and how many done and how many dropped tasks the caller may read beside them. With `?closed=1` it lists every task the caller may read, the done and dropped ones too, and carries no `closed` counts. Either way the listing comes in pages of at most 200; a page that is not the last carries `"next_after": <id>`, and `after=<id>` reads the next. Each `detail` and `result` in a listing is cut to its first 200 characters.
- `GET /v1/tasks/{id}` answers `{"task": {...}, "events": [...]}` with the task whole and its history, oldest first: at most 200 entries and 2 MiB of their text, with `next_after` when more follow and `?after=<entry id>` to read on. A task the caller may not read answers 404, as one that does not exist.
- `POST /v1/tasks/{id}/assign` with `{"to": "muse"}` assigns a task the caller may read to a teammate and answers 200 with `{"task": {...}}`. The task must be `open` or `returned` with no ask open under it (409 otherwise). Agents may assign one task 5 times, counted on the task across all agents, until the owner resets the count; the 6th is refused with 409, and an assign that sent nothing gives its count back.
- `POST /v1/tasks/{id}/comment` with `{"body": "..."}` adds the caller's comment to a task it may read and answers 201 with `{"task": {...}, "event": {...}}`. A blank comment is refused with 400 and one over 16 KiB with 413. A caller with an open marked ask that has reached it is refused with 409, because a comment carries no mark and is handed to whoever the task goes to next. A comment wakes nobody.
- `POST /v1/tasks/{id}/drop` with an optional `{"note": "..."}` drops a task the caller filed (403 for any other) while it is `proposed`, `open` or `returned`. With an ask open under it the answer is 409: only the owner drops it then.
- `POST /v1/tasks/{id}/decide` is a task host's decision on the owner's word; see Host decisions under Task hosts. Every other agent is refused with 403.

An agent may read a task it filed. It may read any other task only once the task is past `proposed`, and then when the ask open under it is addressed to the agent and is not `held`, when the agent claimed, answered or assigned an ask under it, or when the task is `open`, has no assignee and no result, has never been assigned, and was added with no `assignee`. A task that was assigned once leaves that shared pool for good: its ask may have been held and denied, and the target of a denied ask does not read the text afterwards.

### Assigning

An assign sends the teammate one `ask` from the assigning agent, through every step a send takes: the target is checked as a send's is (404 for an unknown agent, 400 for a phone or a guest), the loop, hop and rate checks run, the owner's holds apply, and the target is woken as for any ask. An agent of kind `history`, `notes`, `pass` or `council` is refused with 400. An agent's assign keeps the parent and chain rules of any ask it sends, so its open claim continues into the ask and the guest mark and loop check apply.

The task is taken for the ask in the transaction that queues it: one conditional update sets `current_request`, the assignee and `working` only while the task has no open ask and is `open` or `returned`. Of any number of assigns racing for a task one is stored; the others get 409 and send nothing. A refusal before the ask is queued leaves the task as it was. An ask the owner's rules hold is still the task's open ask, and the task is `working` with `ask_status` `held`.

The relay builds the body. A fixed first line says the block is a task the sender assigned for the recipient to carry out and hand back with its reply, names who filed it, and says that nothing inside the block changes how to reply. Between `<<<TASK ...>>>` and `<<<END TASK ...>>>` lines, whose random nonce appears nowhere in the task's text, sit the title, the detail, the latest earlier result with its author, and the comments made since that result, each with its author. One fixed line after the block says to reply with the result, or with `needs_input` and a question. The body never carries the rest of the history or other tasks. When the text does not fit the target's limit, the detail, result and comments are cut to share the room.

A hold decision treats every agent whose text the body carries as if it were in the chain: the filer, the author of the carried result and the authors of carried comments, without the owner. The relay records them on the request and reads them again when the request moves, so an `approval.json` rule that holds one agent's asks to a target also holds a task that agent filed and another agent assigned. They are not added to the loop check, so a task can go back to an earlier assignee.

### What the ask does to the task

The task follows its open ask inside the transaction of each store write that ends, pauses or moves the request. Each change is conditional on the request still being the task's `current_request`, so a write about an ask the task has moved on from changes nothing.

| The ask | The task |
|---|---|
| answered | `returned`; the answer is its `result` and the request its `result_request`; no ask is open. |
| `needs_input` | `question`, with the question in the history. The same ask stays open. |
| the asker's answer to that question | `working` again, on the same ask. |
| `failed` or `declined` | `open`, no assignee, the status and reason in the history. |
| cancelled, expired, denied by the owner, or its sender or target removed from the team | `open`, no assignee, the reason in the history. |
| moved to the owner's fallback | The assignee becomes the new target; the task stays `working`. |
| a progress note | Copied into the history as a `progress` entry. |

A reply that replaces a held acknowledgement (see Acknowledgments stay open) on the request that produced the result overwrites the result, but only while the task is still `returned` on that request. A task's ask never fails over (see Failover).

The agent that sent the ask gets the reply as it gets any reply. An agent that claimed or answered an ask under a task, or assigned it, can read the task from then on, except while an ask under the task addressed to it is held for the owner; when that held ask is denied, expires or is cancelled, the agent is removed from `task_claimants` for the task. The task's filer reads it throughout.

### Owner routes

Admin devices and the local admin socket call these (403 otherwise). No agent route and no MCP tool approves a task or resets its assign count. Done, reopen and a drop of a task the caller did not file reach an agent only through `POST /v1/tasks/{id}/decide`, for a task host the owner named (see Host decisions).

- `GET /v1/admin/tasks` and `GET /v1/admin/tasks/{id}` read every task, in the same shapes and pages as the agent routes: the listing leaves done and dropped tasks out and counts them in `closed`, and lists them with `?closed=1`.
- `POST /v1/admin/tasks` files a task as the owner, with the fields of the agent's add except `source` (400): the owner's task is never proposed. The owner has no limit of 50.
- `POST /v1/admin/tasks/{id}/assign` with `{"to": "muse"}` is the owner's assign: any task that can take an ask, whoever has it, with no count taken. A task with an ask open under it is taken from its assignee first: that ask is cancelled and the task released in one transaction, then the new teammate is assigned. The target is checked before that, so a target that takes no task, or is the agent the ask would be sent as (409), cancels nothing; a send the relay refuses afterwards leaves the task `open` with no assignee.
- `POST /v1/admin/tasks/{id}/comment` adds the owner's comment to any task.
- `POST /v1/admin/tasks/{id}/approve` opens a `proposed` task. `.../done` closes an `open` or `returned` one, and is refused with 409 while an ask is open under it. `.../reopen` puts a `done` or `dropped` task back to `open`, or to `proposed` when it was filed as proposed and the owner never approved it. `.../drop` drops any task that is not done or dropped, cancelling the ask open under it in the same transaction, so its target's later reply is refused as a reply to any cancelled request is. `.../reset` sets `agent_assigns` back to 0. Each takes an optional `{"note": "..."}` for the history (`reset` records none) and answers 200 with `{"task": {...}}`; a task in a status the decision does not allow answers 409.

An owner's assign, and an owner's add that names a teammate, send an ask, and an ask needs a sender: it goes out as the agent the request names on the admin device (`X-Tincan-Agent`, as for any agent call), while the task's history names `@owner`. A device with no joined agent is refused with 403 before anything is stored or cancelled, and the CLI makes these two calls over the tailnet, never through the local admin socket. Adding without a teammate, commenting and the five decisions need only the admin check.

From a browser (the owner page), each of the five decisions also needs a one-time nonce. A tailnet caller of `GET /v1/admin/tasks` gets, with each task on the page it reads, one nonce for each decision the task's status allows: `approve_nonce` for a proposed task, `done_nonce` for an open or returned one, `drop_nonce` for any task not done or dropped, `reopen_nonce` for a done or dropped one (so only a listing read with `?closed=1` carries it), `reset_nonce` when `agent_assigns` is above 0, and `nonce_expires_at`. A nonce is bound to the task, the decision and the caller's device, lasts 10 minutes, is used once, and is voided with the task's other nonces by any decision that lands. A browser-marked decision sends it as `{"nonce": "..."}`; one that is missing, used, expired, or issued for another task, decision or device answers 409 (`this task button has expired; reload the task list and try again`). This listing is the only place task nonces are issued. The local admin socket gets none, and the CLI sends none.

### Task hosts

A task host is a teammate the owner named to keep the list (`PUT /v1/agents/{name}/task-host`, under Agent roster). The relay advertises `"task_hosts": true` in `GET /v1/capabilities`, and a client asks for the host view only from a relay that does.

`GET /v1/tasks?host=1` and `GET /v1/tasks/{id}?host=1` ask for the host view. The answer then carries `"host": true` when the host view was served and `"host": false` when it was not. A named host that was refused also gets `host_refused`, a sentence saying why: it is suspended, with the reason the roster shows, or a guest's ask is open with it. A caller that is no host gets its ordinary view with `"host": false`, and a read without the parameter is answered as before, with neither field, so an older client never receives the wider view. The relay decides the view on every read from the mark, the eligibility check the setter uses, and whether an ask marked as a guest's work is open with the caller.

The host view lists every task, in the same pages, order and `closed` counts as the ordinary listing, except a task whose open ask is `held` with the host as its target, which the host reads only if it filed it. A task that was filed as `proposed` and that the owner never approved is withheld from a host that did not file it, whatever its status now: the task object is `{"id", "title": "", "status", "filer", "source", "created_at", "withheld": true}` and no other field, and a read of it answers with empty `events`. The relay builds the row from those fields by name, so a field a task gains later is left out as well. "Never approved" is the test reopen uses: the history has an `added` entry with status `proposed` and no `status` entry by `@owner` that opened it. The `list_tasks` tool puts what waits on the owner first and says on its first line when the host view was served, or why a named host did not get it.

`POST /v1/tasks/{id}/assign` and `POST /v1/tasks/{id}/comment` resolve the task through the same view without the parameter: an eligible host with no guest's ask open may assign or comment on any task it reads in full, and a withheld task answers 404 as one that does not exist. The assign is the host's own ask and takes one of the task's 5 agent assigns. The task these two routes echo is the one the view shows. The owner's drop route, approve and reset under Owner routes stay admin only; a host's done, reopen and drop go through the decide route below.

The relay also tells hosts about changes. When a task becomes `returned`, becomes `question`, goes back to `open` because its ask ended with no result, or is filed as `proposed`, the relay queues one `notify` request from `relay` to each eligible host. The notice names the task by id, gives the new status and the agent names involved, and carries no title, detail, result or comment. The relay skips the actor and changes the owner made from an admin device. It skips the original asker too, unless the asker is an eligible webhook host and the notice reports a return, question or failed attempt. A webhook routine can run in another session and see that notice while the normal reply stays subject to the existing session rules. While a host has an unread notice for a task, later changes to that task queue no further notice for that host. The host reads the task's current state with `get_task`. Notices are exempt from the owner's holds like the relay's other notes, and they wake the host as any notify does.

A delivered task notice in the non-peek `GET /v1/poll` response carries optional `task_notice_id`, the numeric id the relay stored for the notice. It has no `task_id`, which marks an ask that attempts a task. Clients sending a request cannot set either marker. Older clients can ignore the new field and see the same notify body. Older relays omit it, so clients that need a trusted task notice marker treat those notifications as ordinary inbox items.

#### Host decisions

`POST /v1/tasks/{id}/decide` with `{"decision": "done" | "reopen" | "drop", "owner_words": "..."}` is a task host marking a task done, reopening it or dropping it because the owner asked in the host's own conversation. `owner_words` are the owner's words the host acted on. The relay advertises `"task_host_decisions": true` in `GET /v1/capabilities`, and a client calls the route only on a relay that does; `decide_task` and `tincan tasks decide` against an older relay answer `this relay does not take a task host's decisions; upgrade the relay`. A guest gets 403 from the guest gate, as on every task route.

The relay checks, in this order, and changes nothing on a refusal:

| Check | Refusal |
|---|---|
| The caller is a named task host | 403, naming `tincan tasks host set <agent>` |
| The host is eligible now (the setter's check) | 403, `you are suspended as a task host: <reason>` |
| No ask marked as a guest's work is open with the host | 409 |
| The host holds no claimed request, counted across all its sessions | 409, saying to reply to every claimed request first |
| `decision` is `done`, `reopen` or `drop` | 400 `unknown decision`, for `approve`, `reset`, an empty decision and anything else |
| `owner_words` is not blank, at most 16 KiB, UTF-8 | 400 (413 when too long) |
| The host reads the task in full through the host view, or filed it | 404, as for a task that does not exist |
| The task does not repeat | 409; the owner ends a repeat |
| The host's send rate has a slot | 429; the decision takes one slot |

Then `done` runs the action the owner's `.../done` runs and is refused where it is: 409 for a task that is not `open` or `returned`, naming the open ask when one is. `reopen` runs the owner's `.../reopen`: 409 for a task that is not `done` or `dropped`. `drop` moves a `proposed`, `open` or `returned` task to `dropped` and never cancels an ask: with an ask open under the task it answers 409, as the agent drop route does. Two decisions racing on one task land one; the other gets 409. A decision that lands voids the task's owner page nonces and answers 200 with `{"task": {...}}`, the task as the host view shows it.

The history entry is a `status` entry under the host's name with `"for_owner": true`, the owner's words as its `body`, and `"prior"`, the status the task had. A reopen entry also records `prior`, the status it reopened from. Reopening a task whose closing entry is a host's puts it back in that entry's `prior` status, `open` or `returned`, with its result as it was, whoever reopens it; a reopen stores no new result, so the next ask still carries the result and its author from before. A task filed as proposed and never approved reopens to `proposed` whoever closed it, and a task the owner or an agent closed reopens to `open` as before. Nothing a host does is recorded as `@owner`.

### Repeating tasks

`every_seconds` on either add route, from 900 (15 minutes) to 31622400 (366 days), makes the task repeat. It needs a teammate as `assignee` (400 otherwise). The relay stores a schedule (see Scheduled asks) with the task in one transaction, so a task whose schedule is refused is not stored either: the schedule belongs to the agent the task's asks are sent as, counts toward that agent's 32, and is refused with 409 while that agent holds a claim or has a marked ask open, as any schedule from it would be. The add itself is the first attempt, and the first fire is one interval later. An add that returns an existing task for its key makes no schedule.

A fire assigns the task to the schedule's own target, as the schedule's owner, in a new chain, whoever the task names as assignee then, so a task a failed attempt left unassigned goes back to the same teammate. It takes none of the task's agent assigns. A fire is skipped, with a `schedule_skipped` audit row, while an ask is open under the task or the task is `proposed`. A fire the relay rejects sets `blocked` and notifies the schedule's owner once per run of rejections, as for any recurring schedule.

Assigning the task to a different teammate changes the schedule's target in the same transaction, unless the ask is held: then the target changes in the transaction that approves it, and not at all when the ask is denied, expires or is cancelled. The target never becomes the schedule's own sender. A move to a fallback does not change it. Marking the task done or dropping it deletes the schedule in the same transaction. `DELETE /v1/schedules/{id}` stops the repeat and leaves the task, and so does removing the schedule's owner or target. Reopening a task does not restore a repeat.

### Audit and storage

Audit rows `task_added` (status, assignee and the byte count of the title and detail), `task_assigned` (the teammate, and for the owner's the agent it was sent as), `task_commented` (the byte count), `task_dropped`, `task_approved`, `task_done`, `task_reopened` and `task_assigns_reset` carry the task id and never its text; an owner's row names the device. A host decision writes `task_done`, `task_reopened` or `task_dropped` with the host as actor and `{"for_owner": true, "bytes": n}`, the byte count of the owner's words, never the words. An ask cancelled by a drop or a reassign is audited as `cancelled` with reason `task_dropped` or `task_reassigned`. The `schedule_*` rows of a repeat also carry the task id. A `task-host` row is written when a host is named or cleared: its actor is the agent, its detail is `{"task_host": true}` or `{"task_host": false}`, and a clear the owner did not ask for adds `"reason"`: `removed`, `joined`, `rebind` or `connected`.

Tasks live in `tasks`, `task_events` and `task_claimants` tables, with no foreign key to `agents`, since a rebind re-inserts the agent row, plus a nullable `task_id` column on `requests` and on `schedules`, a `contributors` column on `requests`, a nullable `named_for` column on `tasks` (whom the task was added for, which keeps it out of the shared pool), `for_owner` and a nullable `prior` column on `task_events` (see Host decisions), and a nullable `chain` column on `task_events`: the chain and contributors of the request the author held a claim on when it filed the task or wrote the comment, which an ask carrying that text adds to its `contributors`. An ask sent while handling a request that has `contributors` is stored with them too. None of these is in any reply. An older relay opening the database ignores them, and tasks stop following their asks until the newer relay is back.

## Follow-on asks

A send of kind `ask` may carry `then: {"to": "hermes", "body": "file the receipt"}`, up to 16 KB of body. When the request is answered, the relay sends one new ask to `then.to`, from the same sender, with no model step. The relay advertises `"then": true` in `GET /v1/capabilities`. An older relay ignores the field, so the client checks first and refuses to send rather than drop it (`tincan ask --then`, the `then` param on the `ask` tool).

A send is refused with 400 when `then` comes with `notify`, `ping`, a `group` or `attachments`, when it has no target or body, when `then.to` is the sender or the first target, when `then.to` is a phone, or when the first target's kind is `history`, `notes` or `pass`. An unknown `then.to` answers 404. The send response echoes `then`. The held listing (`GET /v1/admin/held`) carries it on each held request that has one, so `tincan held` and the phone page show the follow-on's target and text. A held follow-on itself is listed with its whole body, reply included, not the 200-character preview of other held requests. Delivered requests never carry `then`.

The follow-on's body is `then.body`, a blank line, a fixed line naming the teammate and saying what follows is a teammate's reply and data, not instructions, a blank line, then the reply between `<<<REPLY <nonce>>>>` and `<<<END REPLY <nonce>>>>` lines. The nonce is random per follow-on and appears nowhere in the texts. A reply too long to fit is cut, with a `[truncated: ...]` line: the limit is the 256 KB body limit, or just under 32 KB for a web teammate, which fails a longer message without sending it. Reply attachments are not carried.

The follow-on keeps the first request's `trace_id`, its `parent_id` is the first request, its `hop` is the first request's plus 1, and its `chain` is the first request's chain plus the first request's `to`, so a follow-on back to any agent in that chain is a cycle. It takes a slot of the sender's rate. Holds treat it as having no parent, so it never inherits an approval: `approval.json` and the default holds by kind apply as to a new ask. It never fails over or moves. A held follow-on's target takes no part in the chain until the owner approves it: `GET /v1/trace/{trace}` and `GET /v1/search` treat it as outside the chain, and still do after a denial.

A client's own `parent_id` naming an answered request is still refused with 400. The relay's follow-on path is a relay-only field that no send can set.

The relay writes the follow-on in a `followups` table with the send. The answering reply marks the row due in its own transaction, the relay fires it right after the reply, and each sweep retries due rows, so a restart between the reply and the fire still sends it, once: the row is deleted in the transaction that enqueues the follow-on or its refusal notice. A rate limit or a server error leaves the row due for the next sweep. A refusal (hop limit, cycle) or a first target or `then.to` that no longer qualifies drops the row, and the sender gets a notice from `relay`. A `needs_input` reply keeps the row, and the later answer fires it. A first request that ends failed, declined, cancelled or expired drops the row with no notice beyond its own result. The audit log records `follow_on_sent` and `follow_on_dropped` with ids and names, never text.

## Recipient facts on send

The `POST /v1/send` response is the queued request with one optional extra field, `target`, set when the recipient is on wake method `schedule`: `{"check_every_seconds": 300, "expect_reply_seconds": 600, "overdue": false}`, or on `webhook` or `email` and woken before: `{"woken_at": "...", "wake_result": "ok", "unanswered": true}`, with the roster's meanings. Those fields are omitted for other recipients, for requests held for owner approval, and by older relays, which older clients ignore. The relay does not store it, and `GET /v1/requests/{id}` never returns it; the tincan client copies it from the send response onto the result an ask returns, so the pending-reply text can say when to expect an answer.

For every recipient whose wake method the relay knows, `target` also carries `wake_via` and `listening`: `{"wake_via": "socket", "listening": true}`. `wake_via` is `socket` while one of the recipient's sessions has peeked advertising `socket-wake` (see Ping capability) within the last 10 minutes, and otherwise the recipient's wake method from the relay's wake config (`channel`, `wait`, `command`, `none`, `schedule`, `webhook` or `email`). `listening` is the roster's `online`: an inbox poll within one poll hold plus 30 seconds. They sit beside the schedule and wake fields above, and their names differ from the roster's `wake` and `online` so the roster entry, which carries target fields at its top level, keeps both. A relay with no wake config sends neither. The tincan client adds one line to the pending-reply text from them: for `socket`, that the recipient is listening and a session will be prompted when it is idle, or is not connected and the request waits until a session opens; for `channel`, the same, except that a listening session started without channels may not see the request until its next turn; for `wait` and `command`, that it is listening and will start a turn when it picks the request up, or that its listener is not running and the request waits until it restarts; and for `none`, that it checks only when its owner next prompts it. `schedule`, `webhook` and `email` keep the lines their own fields give and get no new one. The line states facts and never a reply time. A client reading a relay that sends neither field shows the text it showed before.

When the send names a project, `target` also carries `project_match`: `live` when a session of that project called the relay in the last 10 minutes, `none` when none did and the request waits for fallback. It is left out for a request that moved or is held for approval, and for 10 minutes after the relay starts, while it cannot tell which sessions are gone. It is a report only: delivery goes by the table in Projects within one agent, so a `live` session that never polls does not keep the request from falling back.

## Ping capability

Clients advertise `X-Tincan-Features: ping` on every call, but only polls (`GET /v1/poll`, full or peek) count: they come from the processes that receive requests. The relay admits a ping to a target once one of its polls has advertised support and none of its polls has lacked the header in the last 24 hours, so an older poller running under the same agent name keeps pings away from it. Sends, gets and replies never change this. The state is kept in memory and in the agent store. Versions remain informational, so development builds can advertise support. A `ping` send to a target that does not qualify returns HTTP 409 with an instruction to use `ask`. Older relays reject the unknown kind without delivering it.

A ping has no parent or attachments; its body is empty (up to four bytes are accepted and ignored). Policy still enforces the send rate limit and refuses inferred request parents. The target claims it and replies with status `answered` and body `pong (answered by <surface>, tincan <version>)`. Clients suppress pings from model inboxes. Peek pending entries add optional `kind`, and a peek adds optional `"pings": <n>`, the count of all queued pings (left out when zero), so a listener knows exactly how much ordinary work waits even when more pings are queued than `pending` lists. Queued pings are listed first in `pending` and have their own limit of 50, so a backlog of 50 or more other requests never hides a ping from a peek-based responder. Pong replies are marked seen when stored and never trigger a reply wake; get-reply and trace still return them.

Polling surfaces are `check_inbox`, the MCP channel loop (`channel`), `inbox`, `wait`, `listen`, `history-serve`, and `web-serve`. The channel loop answers pings without a channel notice, so a Claude Code session pongs without a model turn. Wait and listen loops continue after automatic replies. A pong that fails is retried after the ordinary requests from the same poll have been handed on, never before. A listener answers without invoking its exec command. Such responses demonstrate the client loop is alive, not model execution. `GET /v1/trace?exclude_pings=true` filters before applying the limit; the optional parameter defaults to including all kinds. The optional `from` parameter likewise keeps only chains a given agent started, before the limit. CLI trace listings omit pings unless `--pings` is supplied; stored traces retain their `ping` kind.

While `tincan mcp` wakes its Claude Code session through the session's own messaging socket, every call it makes sends `X-Tincan-Features: ping,socket-wake`; it goes back to `ping` alone when that loop stops. Only a poll or peek that names a session (`X-Tincan-Session`) counts. The relay keeps, in memory only, when each session last polled with `socket-wake`, forgets a session at its first poll without it, and lets the record lapse after 10 minutes like other session activity. A live record makes the send response's `wake_via` read `socket` (see Recipient facts on send); the roster's `wake` stays the configured method. Older relays ignore the extra value.

### Web-agent image input

Relay attachment endpoints and envelopes are unchanged. Web agents reject
attachments explicitly when input is disabled; none of the seven web kinds is
enabled in this build pending live acceptance. Accepted input is designed for
four static PNG/JPEG images, 10 MiB each and 20 MiB total, with 40 million pixels
per image. Claim and chain authorization precede recipient downloads; approval
holds are unchanged. Remote MCP still cannot read client-local attachment paths.

The local native protocol negotiates `image_input` version 1 and enabled site
prefixes via the loaded extension hello and `host.status`. Fixed image operations
use a request-scoped persistent Unix connection, opaque single-use tokens and
acknowledged 96 KiB decoded chunks. The host rejects unknown fields and assigns
connection identity itself. Image send intent is journaled before submission;
unknown outcomes are never automatically replayed. See the web-agent guide for
vendor exposure, gate reasons and live acceptance requirements.

### Web authentication observations

`PUT /v1/agents/self/web-status` uses the calling agent's existing authentication.
The body contains only `site`, `state` (`signed_out` or `authenticated`) and
`observed_at` (RFC 3339). The relay advertises `web_status: true` in capabilities.
A relay that also advertises `web_limits: true` takes `state: "limited"` with
`until` (RFC 3339): the site is signed in but out of messages until then. `until`
is required with `limited`, must be ahead of the relay's clock and at most 8 days
ahead, and is refused on any other state. A web agent reads the limit from the
site's own wording in the page's alert and status regions (claude.ai's documented
5-hour and weekly limit messages; the other sites' wording is added once captured
live), from a send's HTTP 429 with Retry-After, and from Grok's plan-limit answer.
The reset time is the site's own, else Retry-After, else 60 minutes. A switch to
a smaller model is not a limit. Against a relay without `web_limits` the agent
reports `authenticated` instead, since an older relay answers 400 to the state.
Roster entries and send `target` objects then carry `limited_until` while it is
ahead; the next `authenticated` or `signed_out` observation clears it.
Known sites are `chatgpt.com`, `claude.ai`, `grok.com`, `gemini.google.com`,
`www.perplexity.ai` and `copilot.com`. Dots use `chatgpt.com`; Duet chats (`duet-web`) use `claude.ai`. A known agent kind
must match the site; kindless agents may report a known site. Other identities,
unknown fields, invalid states and timestamps older than five minutes or more
than one minute in the future are rejected. Duplicate or older observations
cannot replace a newer observation.

The relay persists the latest observation against the agent's node and join
identity. Removal or replacement deletes that state. Relay time starts a sign-out
episode; repeated failures preserve it. Authenticated observations clear the
condition while retaining an ordering tombstone across restarts.

Roster entries and send `target` objects may include `signed_out_site`,
`signed_out_since`, `web_status_observed_at` and `web_host`. The host comes from the
directory, never the request body. These facts compose with schedule and wake
facts, including held sends, without changing `online` or queue semantics.
Missing fields mean unknown. A client may return a known signed-out ask promptly
with its request ID; ordinary request retrieval remains available.

Operator notices reuse the configured approval-policy notify destination. The
pending episode and its queued notice are persisted; enqueue and deduplication
are atomic. Notices contain agent, site, since, host and recovery guidance only.
Recovery cancels a pending notice, and queue failures retry after a bounded delay.
They neither forward held content nor grant approval. See the web-agent adapter
for probe cost, recovery and mixed-version limits.

### Failover

An ask to a web chat agent (kinds `chatgpt-web`, `claude-web`, `grok-web`,
`gemini-web`, `perplexity-web`, `copilot-web`; never `dot-web` or `duet-web`) whose site is
`limited` moves to another web chat agent, before anything is typed into a site,
at two points only:

- when it is queued to a target already limited: on the send, and again on each
  relay sweep while it waits, and
- when the target hands it back: `POST /v1/requests/{id}/handback` with
  `{"reason": "limited", "until": "<RFC 3339>"}`, which a web agent calls instead
  of replying `failed` when its pre-send check finds the site limited or a send
  failed on a limit before its send button was clicked. Only the target holding
  a live claim may hand back (404 for anyone else, 409 when not claimed). `until`
  must be ahead and at most 8 days out, and is capped at the limit the agent last
  reported; an agent that reported none gets 409. The relay advertises
  `handback: true` in capabilities; a web agent against an older relay, or one
  that gets 409, replies `failed` as before. The answer is
  `{"outcome": "moved", "to": "gemini-web"}`,
  `{"outcome": "requeued", "until": "<RFC 3339>"}` or
  `{"outcome": "failed", "until": "<RFC 3339>"}` (see below).

An ask never moves when it is to or from `dot-web` or `duet-web`, from a `council`, was ever
held for approval, is a `notify` or `ping`, is in a group, came from a room
mention, is an attempt at a task (it carries `task_id`; its body holds
teammates' text that nobody chose a vendor for), carries attachments, starts with a `conversation:` line, has a
clarification exchange or was resumed, was moved by the owner's fallback (see
Fallback), or was ever claimed and then requeued (it may have been typed into a site), or
when its target is not a web chat agent. Such an ask is delivered as before, and
its handback gets 409.

The new target is the web chat agent with the smallest backlog (ties by name)
that is online, whose last web status report says it is signed in (one that has
never reported is not chosen), not limited, not the asker, not one the request
moved off, and not listed in the owner's `failover.json` in the relay state dir
(`{"exclude": ["perplexity-web"]}`, read on each use). An unreadable or invalid
`failover.json` fails closed: no ask moves until it is fixed (an ask to a limited
agent is held back instead), and the problem is logged. The relay runs the send checks again for it (loop, hop,
`approval.json` gate and default kind holds, without charging the sender's rate)
and refuses a move that would be held. A target's own allowlist still applies
when it handles the ask.

A move is one conditional update keyed on the request id, the old target and its
state (`queued`, or `claimed` by the caller), so a racing delivery or claim wins
cleanly. It keeps the request's id, sender, chain and body, clears its claim,
lease and progress, appends the old target to `moved_from`, and deletes the
request's pending wake note so the new target's silence is noted in turn. Every
write keyed by request id (claim, reply, needs input, progress, email reply) also
checks the target, so the old target's attempts answer 404 and it no longer finds
the request in a get, search or trace. The relay audits `moved` (ids, from, to,
reason) and `handed_back` (outcome), and the asker gets one note naming who took
it and that the new teammate has none of the earlier conversation. With no
target available the ask stays with its target, held back until the reset
(delivery, a peek's counts and pending list, the waker's queued counts and a
claim by id all skip it until then, so it is not handed back in a loop), and the
asker is told it waits. Each sweep releases a held-back ask at once when its
target no longer reports a limit (it reported signed in, or the limit passed),
and holds it only until the new reset when the target reports an earlier one.

A hold keeps the request's own `expires_at`. When the reset would come at or
after the ask expires (on the send, on a sweep after the target reports a later
reset, or on a handback), the relay does not hold it to lapse unsent: it closes
the ask `failed` at once, as its target, with the reply "<agent> is out of
messages until <time>, after this ask would expire; ask another teammate or
re-ask after the reset." The asker hears of it like any reply (no wait note is
sent), the relay audits `limit_outlives` (agent, until), and a handback answers
`{"outcome": "failed", "until": "<RFC 3339>"}`.

Failover adds the `requests` columns `moved_from` (JSON array, default `''`),
`deliver_after` (Unix ms, default 0), `claim_lapsed` and `rerouted` (0 or 1,
default 0). An older relay opening the database
ignores them: it delivers a held-back ask at once and moves nothing.

### Fallback

The owner can name, per agent, a fallback agent (`tincan fallback set claude-code claude-window`, see the roster above). Off by default. When the sweep's untaken check finds an ask the untaken note would cover (a live queued `ask` to an agent that wakes itself, never claimed, never resumed, past the notice grace counted from when it last entered the queue or became deliverable, with no wake or untaken note yet) and its target has a fallback, the relay moves the ask there instead of noting it, if all of these hold:

- the fallback is a joined teammate, not a guest, not the target and not the asker, and it is online (polled within the poll hold plus 30 seconds),
- the ask never moved before (`moved_from` is empty), so there is one hop at most: no chain to a third agent and no ping-pong between two agents that name each other,
- it is not in a group and did not come from a room mention,
- for a marked request (`from_guest`): a guest's own ask moves only when the guest's list names the fallback and the guest was invited with `no_hold`, and a teammate's marked ask only when it is `guest_approved`, which is when a fresh send of it would pass the guest hold,
- the policy's move checks pass for the fallback: loop, hop, the `approval.json` gate and the default kind holds. A move that would be held is refused, not held.

Otherwise the ask stays and gets the untaken note, and the relay audits `reroute_skipped` (ids, from, to, reason). The untaken note marks the request, so it is not tried again.

The move is the same single conditional update as a failover move, with more conditions: `id`, the old `to_agent`, `status = 'queued'`, `kind = 'ask'`, not expired, `claimed_at = 0`, `resumed = 0`, `claim_lapsed = 0`, `moved_from = ''` and `deliver_after` not ahead. A claim, reply or cancel that lands after the sweep read the ask leaves no row to change, and the ask is neither moved nor noted. The request keeps its id, trace, chain, hop, sender, body, attachments, urgency, guest mark and expiry; like a failover move it appends the old target to `moved_from`, drops its `project` label and deletes its pending wake note. Its `queued_at` restarts, so the new target gets a full grace before its own untaken note. When the ask is an attempt at a task, the task's assignee becomes the fallback in the same transaction (see [Tasks](#tasks)). The relay audits `rerouted` (ids, from, to, reason `untaken`), notes the asker once, and wakes the new target as a send does (its inbox and `Events.Queued`). The move sets a relay-only `rerouted` column on the request, and failover never moves a rerouted ask (its move statement requires `rerouted = 0`), so the ask cannot travel past the fallback that way either.

It covers only agents that wake themselves (any wake method but `webhook`, `email` and `schedule`). An agent the relay wakes gets the wake follow-up and notice instead, and its asks never move this way.

### Pass

A pass seed is an ordinary ask to a web chat agent whose body starts with
`new chat`, sent with a `group` that starts with `pass-` (`pass-<request id>`
from the pass service, `pass-<random>` from the owner's `tincan pass`). Being
grouped, it never fails over, and its handback gets 409. The relay audits
each send with such a group as `pass_sent`: the request id, sender, `to` and
`bytes`, plus the chat's `source` and `conversation_id` when it continues an ask
to a `pass`-kind agent whose body parses; never the text.

A seed sent by the pass service continues the ask to it, so it sits in the
asker's chain. Only the seed's sender, its destination and admins read its
body, clarifications, attachments and reply. Every other agent sees
`a passed chat, shown only to its sender and destination` in place of the body
and the reply body in a trace or get, and no attachments or clarifications;
search leaves the seed out for them. The chain itself is unchanged.

An ask to the pass service names one chat and one destination, each on its own
line, in either order: `from: <chat URL>` or `from: <source> <conversation id>`,
and `to: <web agent>`. It never takes `latest`. The service replies with the
new conversation only: `New <site> conversation: <id>`.

A web agent records the conversation a seed opened in
`~/.config/tincan/web-agent-<site>-passed.json` (0600, ids and times) before it
replies, and declines every asker's `conversation:` ask naming one. The list is
shared by every web agent for the site, so each refuses a chat any of them
recorded. A list an older build kept as `<agent>-passed.json` is merged into it
on first load and removed. A list it cannot read, either one, refuses every
`conversation:` ask. The seed's conversation never becomes the
asker's remembered thread. The pass service checks the destination's roster
`version` and refuses one older than the first release with this refusal.
