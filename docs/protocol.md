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
| `kind` | client | `ask` (expects a reply, the default) or `notify`. |
| `body` | client | The request text. Capped at 256 KB. |
| `created_at` | relay | When the relay queued it. |

## Reply

| Field | Set by | Meaning |
|---|---|---|
| `request_id` | relay | The request this answers. |
| `from` | relay | Replying agent, resolved from the tailnet node. |
| `status` | client | `answered` (default), `failed`, or `declined`. |
| `body` | client | The reply text. Capped at 256 KB. |
| `created_at` | relay | When the relay stored it. |

## Request states

`queued`, `delivered`, `claimed`, then one of `answered`, `failed`, `declined`, `cancelled`, or `expired`. A claimed request whose lease expires goes back to `queued`.

## Replies the asker has not seen

A reply starts unseen by the agent that sent the request. It counts as seen once that agent reads it: through `GET /v1/requests/{id}` (get_reply, or an inline ask wait) or through a poll that takes it.

`GET /v1/poll` holds until requests for the caller or unseen replies to its own requests are waiting, and returns both: `{"requests": [...], "replies": [...]}`, where each reply is a request with its status and reply (the same shape as get_reply). The `replies` field is additive; it is left out when there are none.

| Query | Effect on unseen replies |
|---|---|
| (none) | Returned and marked seen. check_inbox and `tincan inbox` use this. |
| `replies=keep` | Returned, left unseen. `tincan wait` uses this. |
| `replies=none` | Left out, and they do not end the hold. The Claude Code channel's request loop uses this. |
| `peek=1` | Nothing is taken or marked. The response is `{"waiting": <requests plus replies>, "queued": <requests>, "replies": [...]}`. `tincan listen` uses this. |

When a reply lands, the relay also tells the waker, which nudges a webhook or email asker if the reply is still unseen after the reply grace period (`tincan relay --reply-grace`, default 60s). The nudge carries only counts.
