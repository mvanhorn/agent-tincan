# Agent Tincan protocol

Agents talk to the relay over plain HTTP on the tailnet. The relay identifies the sender of every request from Tailscale (`WhoIs`), so nothing in a request body can change who it is from.

## Request

| Field | Set by | Meaning |
|---|---|---|
| `id` | relay | Request id, assigned when queued. |
| `from` | relay | Sending agent, resolved from the tailnet node. A client-supplied value is ignored. |
| `to` | client | Target agent name. Must differ from the sender. |
| `parent_id` | client (the tincan client fills it in automatically) | The request the sender is currently handling, if any. The relay uses it to continue that request's chain. |
| `trace_id` | relay | Chain id, inherited from the parent or new. |
| `hop` | relay | Position in the chain: 1 for a new request, parent hop plus 1 otherwise. |
| `chain` | relay | Agents the request has passed through, oldest first. |
| `urgent` | client | Optional boolean, default false. Urgent requests are delivered first, oldest first within each priority. Relay-side wakes bypass debounce and the online skip, within the hourly wake cap. |
| `kind` | client | `ask` (expects a reply, the default), `notify`, or `ping` (automatic client reply). |
| `body` | client | The request text. Capped at 256 KB. May be empty when the request carries attachments. |
| `attachments` | client names ids, relay fills the rest | Files stored on the relay: `[{"id", "name", "mime", "size"}]`. See Attachments. Left out when there are none. |
| `created_at` | relay | When the relay queued it. |
| `exchanges` | relay | Optional clarification history: `[{"question", "answer", "at"}]`. `at` is the question's timestamp; `answer` is absent until answered. Client-supplied exchanges are ignored. |
| `resumed` | relay | Optional boolean, true after the asker has supplied clarification. Retained on subsequent deliveries and claims. |

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

Optional `held`, then `queued`, `delivered`, `claimed`, then one of `answered`, `failed`, `declined`, `cancelled`, or `expired`. A claimed request whose lease expires goes back to `queued`.

## Progress notes

`POST /v1/requests/{id}/progress` accepts `{"note":"calling the restaurant now"}`. Only the current target with an active claim may post (409 otherwise). Notes must be nonblank and at most 1024 UTF-8 bytes (413 when larger). Each post replaces the previous note and renews the claim lease (30 minutes by default); a claimed notify remains lease-free.

Get and trace results include optional `progress: {"note":"...","at":"<RFC3339 timestamp>","by":"muse"}` while claimed. A held get refreshes this at timeout. Requeuing clears the note. Progress never wakes the asker, and its audit event records only the byte count.

`GET /v1/capabilities` advertises `"progress": true`. Clients check it before posting and report an upgrade message when the flag or endpoint is absent. Older clients ignore the optional result field.

### Clarifying a request

For an `ask` with a live claim, its target may `POST /v1/requests/{id}/reply` with `{"status":"needs_input","body":"Which restaurant?"}`. Only the target that claimed the request may ask; a queued, delivered, expired-lease, or already waiting request returns 409. A different agent returns 403. Notifies cannot request input.

The response is a normal reply with status `needs_input` (200). This status is non-terminal. The relay appends a clarification exchange, pauses the claim lease, and marks the question unseen. The question ends a held get-reply wait and follows the same poll, acknowledgement, and reply-wake path as a final reply. While waiting, no lease can requeue it; the original request expiry still runs (24 hours by default), and removing either participant cancels it.

Only the original sender may `POST /v1/requests/{id}/answer` with `{"body":"Nopa, 2 people"}`. A different agent returns 403. The request must still be `needs_input` and unexpired, otherwise 409. The relay records the answer, removes the interim reply, and returns the request (200), now `queued` with `resumed: true`. It wakes the same target through the request-wake path. The target polls and claims normally, receiving the original body and full exchange history. The id, target, parent, trace, chain, hop, creation time, and expiry are unchanged. A duplicate answer returns 409.

Each question and answer must contain non-whitespace text and is capped at 16,384 UTF-8 bytes (400 for empty or oversized input). Clarifications carry text only; attachments on a `needs_input` reply return 400. At most three question/answer rounds are allowed per request; a fourth question returns 409, leaving the claim available for a final reply. A final reply uses the existing reply statuses and limits.

Get-reply, unseen replies, and trace steps include an optional top-level `exchanges` list as well as the history on their `request`. On expiry or cancellation, history is retained but the question is no longer returned as a live reply or counted as unseen. Audit events `needs_input` and `answered_input` record only question or answer byte lengths in their detail, never the text. Wake messages contain counts and instructions only.

Relays advertise `"needs_input": true` in `GET /v1/capabilities`. New clients refuse to send `needs_input` or an answer when this flag is missing or false (including a 404 capabilities endpoint), with an instruction to upgrade the relay. Existing ordinary replies work against older relays. An older asker can read the question as the reply body with an unfamiliar `needs_input` status; upgrade that client to answer using `tincan answer <id> "text"` or MCP `answer`. Older handlers never produce the new status. All new fields are optional.

## Replies the asker has not seen

A reply starts unseen by the agent that sent the request. It counts as seen once that agent reads it through `GET /v1/requests/{id}` (get_reply, or an inline ask wait), or once the agent acknowledges it after a poll. No poll marks a reply seen on its own, so a reply lost on the way (a dropped connection, a client crash, a response the client could not read) comes back on the next poll.

`GET /v1/poll` holds until requests for the caller, or unseen replies to its own requests that it asked for, are waiting, and returns both: `{"requests": [...], "replies": [...]}`, where each reply is a request with its status and reply (the same shape as get_reply). The `replies` field is additive; it is left out when there are none.

| Query | Effect on unseen replies |
|---|---|
| (no `replies` param) | Left out, and they do not end the hold, the same as `replies=none`. Clients that predate replies send this and decode only `requests`, so they must not be handed replies. |
| `replies=take` | Returned, left unseen until the client acknowledges them (below). check_inbox and `tincan inbox` use this. |
| `replies=keep` | Returned, left unseen, never acknowledged. `tincan wait` uses this to end the wait and print a count. |
| `replies=none` | Left out, and they do not end the hold. |
| `peek=1` | Nothing is taken. The response is `{"waiting": <total>, "queued": <requests>}`. When requests are queued it also carries `"pending": [{"id": "...", "from": "..."}, ...]`, naming up to 50 of them, oldest first, without bodies. With `replies=keep` (or `take`) it also carries `"replies": [...]` and `waiting` counts them; without, replies are not counted. A peek changes no request's state (nothing becomes `delivered` or `claimed`) and marks no reply seen. `tincan listen` and the Claude Code channel (`tincan mcp --channel`) send `peek=1&replies=keep`; the channel never claims, and the model takes the items with check_inbox. `pending` is additive: clients that predate it ignore it. |

Any other `replies` value is a 400.

A relay that is shutting down ends every held poll and get-reply wait at once with the answer its deadline would give (204, the upgrade-only 200, or the request's current state), and answers later ones without holding. Clients treat that as an ordinary empty poll and poll again.

`POST /v1/replies/ack` with `{"ids": ["<request id>", ...]}` marks those replies seen and returns 204. Ids that are not the caller's own requests, or that have no reply yet, are ignored, so an agent can only acknowledge its own replies. At most 500 ids per call. `tincan inbox` acknowledges after it prints the replies, and check_inbox after it builds its result; if the ack fails, the replies simply show again next time.

Replies include an additive integer `generation`, a persistent per-request counter that advances for each clarification question or final reply, even within the same millisecond (pre-upgrade replies start at generation 0). Clients acknowledge the generation they displayed with `{"ids": [], "acks": [{"id": "<request id>", "generation": 2}]}`. An `acks` entry marks a reply seen only if its generation still matches; stale entries are ignored. Plain `ids` remain supported with their original behavior (acknowledging the current reply regardless of generation). The two lists may be combined, with a total limit of 500 entries; clients should put each reply in only one list. CLI and MCP inbox clients use generation acknowledgements.

One poll delivers at most 20 requests, oldest first, and stops adding requests once their bodies and clarification exchanges pass 1 MiB (it always delivers at least one); the rest come with the next poll. One poll returns at most 50 unseen replies, oldest first, and stops adding replies once their request and reply bodies pass 1 MiB (it always returns at least one), so a response stays well under the client's 4 MiB read limit. Bodies are not cut. When replies were left out, the response carries `"replies_remaining": <n>`; they come with a later poll once this batch is acknowledged.

When a reply lands, the relay also tells the waker, which nudges a webhook or email asker if the reply is still unseen after the reply grace period (`tincan relay --reply-grace`, default 60s). The nudge carries only counts. If the replies are still unseen after that nudge, the waker checks again 5, 20 and 60 minutes after each previous nudge and nudges each time some remain, within the agent's hourly wake cap, stopping as soon as they are read. The grace and follow-up timers live in memory, so a relay that restarts schedules a fresh reply nudge for every webhook or email agent that still holds unseen replies, and a request nudge (counted when it fires, after the usual debounce) for every one that still has queued requests.

## Search

`GET /v1/search?q=<text>&limit=<n>` searches stored request and reply bodies. The relay resolves the caller from Tailscale just as for trace: agents see matches in chains where they sent or received a request at any step; admins see all chains. Search does not mark replies seen or change request state.

`q` must be nonblank and at most 4096 bytes. Words are quoted as literal FTS terms, all of which must match (case-insensitively); punctuation is ignored, and operators such as `OR` have no special meaning. Punctuation-only queries return no matches. `limit` defaults to 20 and must be 1 to 50; invalid input returns 400.

The response is `{"results": [{"request_id", "trace_id", "from", "to", "status", "created_at", "snippet", "reply_snippet", "question_snippet", "attachment_names"}]}`, newest request first (in the order the relay stored them). `snippet` and `reply_snippet` are optional short excerpts of the request and final reply bodies, respectively, with matches in square brackets; each is present only when its column matched. `question_snippet` is an optional excerpt of the request's latest clarification question (a `needs_input` reply), whether it is still waiting, answered, or left behind by expiry or cancellation; a question is an exchange on the request and never appears as `reply_snippet`. Older clients ignore it. An agent’s search starts from its own chains: it walks the requests in chains the agent took part in, newest first, 5,000 at a time, one short query per batch, until the limit is filled or its history is exhausted. Requests it cannot see are never examined, and older visible matches are still found. An admin search covers the whole index and considers at most the newest 2,000 matches (by request rowid, which follows insertion order), so very old matches for common terms may be omitted there. `attachment_names` is optional and contains names from both the request and reply; neither names nor file contents are searched. No matches returns `{"results": []}`.

The relay advertises `"search": true` in `/v1/capabilities`. Older relays without the route return 404; the client reports that the relay needs upgrading. The migration creates the index and triggers. On open, existing bodies are indexed in batches of at most 500 requests per transaction, recording a high-water rowid atomically with each batch. Backfill errors are logged and do not fail startup; search returns what is indexed so far, and backfill resumes on reopen. New requests and replies are indexed in the same transaction that stores them. Successful searches write a `search` audit event with only `{"count": <returned result count>}` in its detail, never the query or snippets.

The MCP `search` tool accepts `query` and an optional `limit` and returns the same results array as `tincan search <text> --json`.

## Attachments

Requests and replies can carry images and small files. The file goes to the relay first, and the message names it by id.

`GET /v1/capabilities` says what the relay supports: `{"attachments": true, "max_attachment_bytes": 10485760, "max_attachments": 8, "search": true}`. A relay that predates attachments answers 404 there, and it would also drop an `attachments` field without a word, since it decodes sends and replies with unknown fields ignored. So the tincan client checks this first and refuses to send attachments to a relay that does not report `"attachments": true`. A relay reports false when it has no place to keep files.

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
  200 Unicode characters, including sender, chain and target.
- `POST /v1/admin/requests/{id}/approve`: returns the queued request. A fresh
  normal TTL starts; the original `created_at` is kept. Normal wake follows.
- `POST /v1/admin/requests/{id}/deny`: optional `{"reason":"..."}` body;
  returns `{"status":"declined"}`. The reply is attributed to `relay` and contains
  the reason. Missing reason means an empty reply body.

Approval and denial require an unexpired held request; other states return 409.
The sweep expires overdue holds and logs `hold_expired`. Holds survive restarts
with their original deadline. Policy changes apply only to future sends.
A `ping` is never held: it has no body and runs no model work. Search results
leave out a held, never-approved request for every caller but its sender and admins.
The other transitions are audited as `held`, `approved`, and `denied`.
An optional relay-authored operator `notify` bypasses the gate and is logged
as `approval_notified`; it never grants the notified agent admin access.

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

### Relay self-upgrade

`POST /v1/admin/relay/upgrade` installs a release over the relay's own binary. Only admin devices and the local admin socket may call it; agents and other callers get 403. The optional body is `{"force": false, "from_github": "v0.8.0"}`.

With `from_github`, the relay first downloads that tag's `checksums.txt` and every `tincan_<os>_<arch>` it lists from its release URL (`tincan relay --release-url`; the request cannot change it, and without the flag the relay refuses `from_github` with 422), checks each binary against it, and moves them into `--dist`, `VERSION` last, restoring the old files if a move fails. Then it reads `--dist/VERSION`, checks `tincan_<its os>_<its arch>` against `--dist/checksums.txt`, keeps the old binary as `<binary>.<old version>`, and renames the new one into place.

Success returns `{"from": "0.7.0", "to": "0.8.0", "restart": "re-exec"}` (or `"exit"` for a relay started with `--upgrade-exit`), writes a `relay_upgraded` audit event with the two versions and the source (`dist` or `github`), and only then restarts the relay: it drains, closes its store, and re-executes itself or exits with status 75. Errors change nothing:

- 404: the relay has no `--dist`, or predates this route (a plain `404 page not found`);
- 409: the release is not newer than the relay's build and `force` is false, an upgrade is already pending a restart, or the relay user cannot write its binary or the directory holding it;
- 422: no valid `VERSION`, no binary for the relay's platform, no `checksums.txt` entry for it, or a checksum mismatch;
- 502: the release download failed.

## Agent roster

`GET /v1/agents` returns an `agents` array. Each entry optionally includes `queued` (queued or delivered requests), `oldest_queued_at` (their earliest creation timestamp), and `claimed` (requests with a live claim lease). Requests past their expiry, terminal requests, and pings are excluded. Zero counts and absent timestamps are omitted. Older clients ignore these additive fields; clients reading an older relay show no backlog. The roster remains visible to joined agents and admins; these counts reveal no request content and do not change the trust model.

An entry for an agent on wake method `schedule` (it checks its inbox on its own interval and cannot be woken) also includes `check_every_seconds` (its configured interval), `expect_reply_seconds` (the interval plus a 5-minute grace for a late check), and `overdue` (true when its last inbox poll, or its join or the relay's start when it has none recorded, is more than two intervals plus the grace ago). All three are omitted for other agents and by older relays.

An entry for an agent on wake method `webhook` or `email` that the relay has woken also includes `woken_at` (when the relay last sent it a wake), `wake_result` (`ok`, or why the send and its retry failed: `<host> returned <status>` or `POST to <host> failed: <cause>`, never the URL's path, query or credentials) and `unanswered` (true when the agent has not checked in since that wake: the send failed, or more than the relay's `--wake-grace`, default 10 minutes, has passed with no poll). Only a poll is a check-in; other calls, such as a send or a get, do not clear it. The relay keeps the last wake and each agent's last poll across restarts, and after a restart the grace runs from the later of the wake and the relay's start. The agent's next poll clears `unanswered`. Removing an agent drops its last wake. All three are omitted for other agents, for agents never woken, and by older relays.

An entry may also include `good_at`: the owner's line saying what the agent is good at, or, when the owner has set none, a stock line for service kinds (`history`, `notes`, `council`, `chatgpt-web`, `claude-web`, `grok-web`, `gemini-web`, `perplexity-web`, `copilot-web`, `dot-web`) and product-tool kinds (`claude-code`, `codex`, `gemini-cli`, `grok-cli`, `chatgpt`). An agent with no stored kind whose name is one of those kind names gets that kind's line. It is omitted for hosting-shape kinds (`vm-webhook`, `e2b-email`, `proxy-sandbox`, `scheduled`, `generic`), `hermes` and `openclaw` until the owner sets one, and by older relays, and older clients ignore it. The relay resolves the stock line, so every client shows the same roster. Agents read it to choose whom to ask; the relay never routes on it.

`PUT /v1/agents/{name}/good-at` with `{"good_at": "phone calls and restaurant bookings"}` sets an agent's line and returns `{"name", "good_at"}`. Only admin devices and the local admin socket may call it (403 otherwise); an unknown agent answers 404. The line is trimmed and must be one line of at most 120 characters with no control characters (400 otherwise); an empty line clears it, so a kind with a stock line shows it again. The `good_at` field is required: a body without it, or with `null`, answers 400 instead of clearing. The response carries the line as stored. Each change is audited as a `good-at` event. A relay that predates the route answers Go's plain-text `404 page not found`, which `tincan good-at` reports as needing a relay upgrade.

## Recipient facts on send

The `POST /v1/send` response is the queued request with one optional extra field, `target`, set when the recipient is on wake method `schedule`: `{"check_every_seconds": 300, "expect_reply_seconds": 600, "overdue": false}`, or on `webhook` or `email` and woken before: `{"woken_at": "...", "wake_result": "ok", "unanswered": true}`, with the roster's meanings. It is omitted for other recipients, for requests held for owner approval, and by older relays, which older clients ignore. The relay does not store it, and `GET /v1/requests/{id}` never returns it; the tincan client copies it from the send response onto the result an ask returns, so the pending-reply text can say when to expect an answer.

## Ping capability

Clients advertise `X-Tincan-Features: ping` on every call, but only polls (`GET /v1/poll`, full or peek) count: they come from the processes that receive requests. The relay admits a ping to a target once one of its polls has advertised support and none of its polls has lacked the header in the last 24 hours, so an older poller running under the same agent name keeps pings away from it. Sends, gets and replies never change this. The state is kept in memory and in the agent store. Versions remain informational, so development builds can advertise support. A `ping` send to a target that does not qualify returns HTTP 409 with an instruction to use `ask`. Older relays reject the unknown kind without delivering it.

A ping has no parent or attachments; its body is empty (up to four bytes are accepted and ignored). Policy still enforces the send rate limit and refuses inferred request parents. The target claims it and replies with status `answered` and body `pong (answered by <surface>, tincan <version>)`. Clients suppress pings from model inboxes. Peek pending entries add optional `kind`, and a peek adds optional `"pings": <n>`, the count of all queued pings (left out when zero), so a listener knows exactly how much ordinary work waits even when more pings are queued than `pending` lists. Queued pings are listed first in `pending` and have their own limit of 50, so a backlog of 50 or more other requests never hides a ping from a peek-based responder. Pong replies are marked seen when stored and never trigger a reply wake; get-reply and trace still return them.

Polling surfaces are `check_inbox`, the MCP channel loop (`channel`), `inbox`, `wait`, `listen`, `history-serve`, and `web-serve`. The channel loop answers pings without a channel notice, so a Claude Code session pongs without a model turn. Wait and listen loops continue after automatic replies. A pong that fails is retried after the ordinary requests from the same poll have been handed on, never before. A listener answers without invoking its exec command. Such responses demonstrate the client loop is alive, not model execution. `GET /v1/trace?exclude_pings=true` filters before applying the limit; the optional parameter defaults to including all kinds. CLI trace listings omit pings unless `--pings` is supplied; stored traces retain their `ping` kind.
