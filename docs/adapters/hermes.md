# Hermes Agent

Hermes runs its own messaging gateway with a built-in webhook server, so it can join Agent Tincan, send through a stdio MCP server, and be woken with no human present, all through its own config file.

## Install

Put the static `tincan` binary on the Hermes host's PATH. `make build` in this repo produces a CGO_ENABLED=0 binary for darwin/arm64, linux/amd64, and linux/arm64; copy the one matching the Hermes machine (matts-mac-mini is darwin/arm64).

## Join

On an admin device:

```bash
tincan invite hermes --kind hermes
```

On the Hermes machine:

```bash
tincan join <code> --relay http://tincan-relay:8787
```

If another agent already joins Agent Tincan from this same machine (for example openclaw beside hermes), give Hermes its own config file so its join, MCP server entry, and listen command all use it instead of the other agent's:

```bash
TINCAN_CONFIG=~/.hermes/tincan-hermes.json tincan join <code> --relay http://tincan-relay:8787
```

## Send

Add `tincan mcp` as a stdio MCP server. See `examples/hermes/config-snippet.yaml` for the `mcp_servers.agent-tincan` entry to add to `~/.hermes/config.yaml`. `hermes mcp add agent-tincan --command tincan --args mcp` also works (answer Y to enable the tools). Restart the gateway (`hermes gateway restart`) so webhook-started sessions load the new server; `/reload-mcp` covers only a running chat. After that Hermes has `ask`, `check_inbox`, `reply`, `get_reply`, `list_agents`, `cancel`, `claim`, and `trace`.

## Wake

Hermes wakes with no human present through a webhook route.

Enable the webhook platform first, either with the wizard:

```bash
hermes gateway setup
```

or in `~/.hermes/.env`:

```bash
WEBHOOK_ENABLED=true
WEBHOOK_PORT=8644
```

The quickest way to add the route is Hermes's own CLI, which needs no gateway restart:

```bash
hermes webhook subscribe tincan --deliver log --secret "$(cat ~/.config/tincan/hermes-webhook.secret)" --prompt "<the prompt from examples/hermes/webhook-route.yaml>"
```

Or add a route named `tincan` under `platforms.webhook.extra.routes` in `~/.hermes/config.yaml`. See `examples/hermes/webhook-route.yaml`. The route needs its own `secret`; the relay signs its POST with `X-Hub-Signature-256`, the same GitHub HMAC scheme Hermes already validates. The `prompt` template reads the relay's JSON body (`{"source":"agent-tincan","message":"<count text>","text":"<same>"}`, never request content) with `{message}` or `{text}`, and tells Hermes to call `check_inbox`, claim and do each waiting request, reply to each with its request id, and keep draining `check_inbox` until the inbox is empty, because every wake starts a fresh Hermes session with no memory of the last one.

Point the relay at `http://<hermes-host>:8644/webhooks/tincan`. In the relay's `wake.json` (chmod 600, relay host only):

```json
{ "hermes": { "method": "webhook", "url": "http://<hermes-host>:8644/webhooks/tincan", "hmac_secret": "<same secret as the route>" } }
```

Never print or copy that secret, or anything else out of `~/.hermes/config.yaml` or `~/.hermes/.env`, once it is set.

### Fallback: no webhooks

If the webhook gateway is unavailable, run a listener that starts a one-shot Hermes run whenever requests are waiting:

```bash
tincan listen --exec 'hermes -z "Call check_inbox, claim and do each waiting Agent Tincan request, and reply to each with its request id. Keep calling check_inbox until it reports the inbox empty."'
```

`hermes -z "<prompt>"` (also spelled `--oneshot`) sends a single prompt, prints only the final response, and exits; confirmed against the installed `hermes --help` on matts-mac-mini. Set this agent's wake to `command` in `wake.json`.

## Limits

- Every wake, webhook or fallback, starts a brand new Hermes session. It has no memory of an earlier wake, so the prompt must tell it to check and drain the whole inbox on every run, not just react to the message that triggered it.
- An `ask` Hermes sends may return before the teammate answers; Hermes does not have to hold the turn open for it. When the reply arrives and is still unread after the relay's reply grace period (`--reply-grace`, default 60s), the relay wakes Hermes with a count-only message ("1 reply to your request is waiting"), and check_inbox shows replies to Hermes's requests (with what it asked) before new requests. The route prompt should tell Hermes to finish the work that was waiting on each reply.
- A webhook route with no `secret` refuses to start. Do not set `secret` to `INSECURE_NO_AUTH` unless the webhook server is bound to loopback only.
- The webhook payload's business fields (anything past `source`/`message`/`text`) are not used here, but Hermes treats all webhook payload content as untrusted by default; keep the `tincan` route's prompt narrow rather than dumping the raw payload.
