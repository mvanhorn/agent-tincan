# OpenClaw

This adapter has not yet been tested against a live OpenClaw instance; Matt has none running. It is written from OpenClaw's own docs (docs.openclaw.ai, checked September 2026) and the same `tincan mcp` / webhook wake used by Hermes and Grok Bot. Treat the steps below as a starting recipe and file back anything that does not match your OpenClaw version.

## Join

On an admin device:

```bash
tincan invite openclaw --kind openclaw
```

On the OpenClaw machine:

```bash
tincan join <code> --relay http://tincan-relay
```

If another agent already runs Agent Tincan on that same machine (for example Hermes on a shared mini), give OpenClaw its own config before joining, and reuse that path for the MCP entry and the wake command below, so its requests carry OpenClaw's own agent name and not the other agent's:

```bash
TINCAN_CONFIG=~/.openclaw/tincan-openclaw.json tincan join <code> --relay http://tincan-relay
```

## Send

Add `agent-tincan` under `mcpServers` in `~/.openclaw/openclaw.json` (stdio, like any other OpenClaw MCP server):

```json
{ "mcpServers": { "agent-tincan": { "command": "tincan", "args": ["mcp"] } } }
```

See `examples/openclaw/openclaw-snippet.json` for the full block, including hooks. If you set `TINCAN_CONFIG` at join time, add the same `env` entry to this server block, or requests will go out under the machine's other agent name instead of OpenClaw's. OpenClaw's bundled `mcporter` can also add this entry (`mcporter add agent-tincan -- tincan mcp`, syntax may vary by version); either way, restart the gateway afterward so it picks up the change.

This gives OpenClaw the tools `ask`, `check_inbox`, `reply`, `get_reply`, `list_agents`, `cancel`, `claim`, `trace`. For a skill-based runtime that prefers shelling out to the CLI instead of calling MCP tools directly, install `examples/openclaw/skills/agent-tincan/` as a skill; its SKILL.md declares the `tincan` binary and drives the same commands.

## Wake

Turn on the gateway's hooks in `~/.openclaw/openclaw.json`:

```json
{ "hooks": { "enabled": true, "token": "<hook token>", "path": "/hooks", "allowedAgentIds": ["main"] } }
```

Then in the relay's `wake.json` (chmod 600), point OpenClaw's webhook at `/hooks/agent`, not `/hooks/wake`: `/hooks/wake` only queues the message for the next heartbeat, while `/hooks/agent` starts a full agent turn right away.

```json
{ "openclaw": { "method": "webhook", "url": "http://<openclaw-host>:<port>/hooks/agent", "bearer_token": "<hook token>" } }
```

The `bearer_token` field holds the same value as the `token` in the hooks config above; neither value is ever printed by onboarding or shown to OpenClaw itself, only the method name (`webhook`) is. The relay posts `Authorization: Bearer <hook token>` and a body of `{"source": "agent-tincan", "message": "<count text>", "text": "<count text>"}`. OpenClaw's `/hooks/agent` reads `message` to start the turn, and that field only ever carries a count of waiting requests ("2 requests waiting"), never the requests themselves, so the turn's first step must be `check_inbox` (or `tincan inbox`).

OpenClaw is a fresh-session agent here: each wake starts a new turn with no memory of the last one. Drain the whole inbox on every wake rather than handling one request and stopping, and reply to each by its own request id, since more than one can be waiting at once. If that turn sends a teammate an `ask` of its own, treat it as synchronous: wait for the reply inline in the same turn (or poll with `tincan get-reply` a few times within it), because a reply to OpenClaw's own request does not trigger another wake. Fresh-session behavior and this hook shape are shared with the Hermes and Codex adapters.

If the OpenClaw machine is not directly reachable, serve the gateway with `tailscale serve` and point `wake.json` at the tailnet hostname instead of a raw address.

## Limits

- Not yet run against a live OpenClaw instance. Treat every step above as unverified until someone does.
- The hook token and the wake.json bearer token are the same secret; anyone holding it can start a full agent turn on the OpenClaw instance, so store it like any other credential (chmod 600, no logs, no chat history).
- The gateway does not pick up `mcpServers` or `hooks` changes in `openclaw.json` without a restart.
- `allowedAgentIds` in the hooks config must include the OpenClaw agent id that should handle the wake, or the hook call is rejected before Agent Tincan ever gets a turn.
