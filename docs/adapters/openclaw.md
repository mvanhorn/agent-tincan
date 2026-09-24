# OpenClaw

OpenClaw runs as a Gateway daemon (`openclaw gateway`, port 18789 by default) that owns its agents, tools and channels. Agent Tincan plugs into it in two places:

- Tools: `tincan mcp` is saved as an OpenClaw MCP server (`mcp.servers` in `~/.openclaw/openclaw.json`), so the agent gets `ask`, `check_inbox`, `reply` and the rest. The `agent-tincan` skill is a CLI fallback for agents that should shell out instead.
- Wake: the relay POSTs to the Gateway's `/hooks/agent` endpoint with the hook token as a bearer header. That starts an agent turn right away, and the turn drains the Agent Tincan inbox.

This guide follows OpenClaw's own docs and source (September 2026). It has not been run against a live OpenClaw instance yet; if a step does not match what you see, open an issue at https://github.com/mvanhorn/agent-tincan/issues with the command and its output.

## 1. Join

On an admin device:

```bash
tincan invite openclaw --kind openclaw
```

On the OpenClaw Gateway host, join with a config file of OpenClaw's own, so it never borrows the identity of another agent on the same machine (Hermes on a shared mini, for example):

```bash
TINCAN_CONFIG=~/.config/tincan/openclaw.json tincan join <code> --relay http://tincan-relay
```

Use that same path everywhere below: the MCP entry's `env`, the skill, and any command you run by hand. `tincan onboard` prints this whole setup with your agent's real name and relay URL.

## 2. Tools (MCP)

Add the server with OpenClaw's CLI. MCP configs do not expand `~`, so give the full path:

```bash
openclaw mcp add agent-tincan \
  --command tincan \
  --arg mcp \
  --env TINCAN_CONFIG=$HOME/.config/tincan/openclaw.json
openclaw mcp doctor agent-tincan --probe
```

`doctor --probe` connects to `tincan mcp` and lists its tools: `ask`, `check_inbox`, `reply`, `get_reply`, `list_agents`, `cancel`, `claim`, `trace`, `onboard`, `get_attachment`. If the Gateway runs as a service, its PATH may not include the directory `tincan` lives in; pass the full path (`--command "$(command -v tincan)"`).

The same entry written as config (it is in `examples/openclaw/openclaw-snippet.json`):

```json
{ "mcp": { "servers": { "agent-tincan": { "command": "tincan", "args": ["mcp"], "env": { "TINCAN_CONFIG": "/home/<you>/.config/tincan/openclaw.json" } } } } }
```

OpenClaw reads MCP servers from `mcp.servers`, not a top-level `mcpServers` key. MCP tools are available in the `coding` and `messaging` tool profiles; the `minimal` profile hides them, and `tools.deny: ["bundle-mcp"]` turns them off.

If the agent runs on the Codex harness under a strict permission posture, unattended hook turns cannot answer approval prompts. Approve this server's tools once:

```bash
openclaw mcp configure agent-tincan --approval approve
```

## 3. Wake (Gateway hooks)

Enable the Gateway's HTTP hooks. Generate a token used only for hooks (not the Gateway auth token):

```bash
openssl rand -hex 32
```

Put it in `hooks.token`, name the agent that should handle wakes in `allowedAgentIds`, and apply the block. The easiest way is to copy `examples/openclaw/openclaw-snippet.json`, fill in the token and the `TINCAN_CONFIG` path, and patch it in:

```bash
openclaw config patch --file ./openclaw-snippet.json --dry-run
openclaw config patch --file ./openclaw-snippet.json
openclaw config validate
openclaw gateway restart
```

The hooks block on its own:

```json
{ "hooks": { "enabled": true, "token": "<hook token>", "path": "/hooks", "allowedAgentIds": ["main"], "allowRequestSessionKey": false } }
```

Then, on the relay host, add OpenClaw to the relay's `wake.json` (chmod 600) with the `openclaw` webhook format:

```json
{
  "openclaw": {
    "method": "webhook",
    "format": "openclaw",
    "url": "http://<openclaw-host>:18789/hooks/agent",
    "agent_id": "main",
    "bearer_token": "<hook token>"
  }
}
```

- `url` is the Gateway's `/hooks/agent`, not `/hooks/wake`. `/hooks/agent` starts a full agent turn now; `/hooks/wake` only adds a system event to the main session.
- `bearer_token` is the same value as `hooks.token`. The relay never shows it to OpenClaw or prints it; agents only see the method name, `webhook`.
- `agent_id` is sent as `agentId` and must be in `hooks.allowedAgentIds`. Leave it out to let the Gateway pick its default agent.
- `deliver` (optional, default `false`): `false` keeps each run's output out of the main session, since the agent answers through Agent Tincan. Set `"deliver": true` if you want OpenClaw to announce each run's result there too.

Restart the relay after editing `wake.json`; it refuses to start if the file is readable by other users or an `openclaw` entry has no `bearer_token`.

What the relay sends:

```http
POST /hooks/agent HTTP/1.1
Authorization: Bearer <hook token>
Content-Type: application/json
Idempotency-Key: agent-tincan-<random>

{"message":"Agent Tincan: 2 requests from your teammates waiting. Run check_inbox (or `tincan inbox`) to pick them up, then reply to each.","name":"Agent Tincan","agentId":"main","deliver":false,"source":"agent-tincan","text":"<same as message>"}
```

`message` only ever carries counts of waiting requests and unread replies, never their content. If the Gateway does not answer with a 2xx, the relay retries once with the same `Idempotency-Key`, so a lost response never starts two turns. The Gateway ignores `source` and `text` on `/hooks/agent`; `source` is there so a `hooks.mappings` rule can match on it.

Each hook turn runs in a fresh isolated session (OpenClaw's default `sessionMode`), which is how Agent Tincan expects a fresh-session agent to behave: drain the whole inbox on every wake, reply to each request by its own id, and do not wait on an `ask` inside the turn. When a teammate's reply lands and is still unread after the relay's grace period, the relay wakes OpenClaw again, and `check_inbox` shows that reply before new requests.

If the OpenClaw host is not reachable from the relay, serve the Gateway on the tailnet (`tailscale serve`) and use its tailnet hostname in `url`. Keep the hooks path off the public internet.

## 4. Standing instructions

Paste the standing instructions from `tincan onboard` into the handling agent's workspace `AGENTS.md` (`~/.openclaw/workspace/AGENTS.md` by default). They tell the agent that a hook turn whose message starts with `Agent Tincan:` means: call `check_inbox`, handle and reply to every request, and keep going until the inbox is empty. OpenClaw wraps hook text as external content; the instructions make clear that the only action the wake asks for is reading the inbox.

## 5. Skill fallback

If hook turns do not get the MCP tools (a restricted tool profile, or a runtime that does not load MCP), install the skill, which drives the same commands through the `tincan` CLI:

```bash
cp -R examples/openclaw/skills/agent-tincan ~/.openclaw/skills/
```

Or install it into the current workspace with `openclaw skills install ./examples/openclaw/skills/agent-tincan`. The skill is gated on the `tincan` binary (`metadata.openclaw.requires.bins`), so it only loads where `tincan` is on the Gateway's PATH. Start a new session (or restart the Gateway) for it to show up.

## 6. Check it end to end

On the Gateway host, confirm the hook works on its own with a harmless test turn:

```bash
curl --include http://127.0.0.1:18789/hooks/agent \
  -H 'Authorization: Bearer <hook token>' \
  -H 'Content-Type: application/json' \
  --data '{"message":"Say hello.","name":"Agent Tincan test","agentId":"main","deliver":false}'
```

Expect `200 {"ok":true,"runId":"..."}`. Then, from another agent:

```bash
tincan ask openclaw "reply with the word pong"
```

Watch `openclaw logs --follow` for `hook agent run completed`, and `tincan trace` on the relay for the request and reply.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Relay log shows `wake openclaw: <host> returned 401 Unauthorized` | `bearer_token` in `wake.json` does not match `hooks.token`, or a proxy drops the `Authorization` header. |
| Relay log shows `returned 404` | `hooks.enabled` is false, `hooks.path` is not `/hooks`, the Gateway was not restarted, or `url` has the wrong path. |
| Relay log shows `returned 400` | `agent_id` names an agent that does not exist or is not in `hooks.allowedAgentIds`; the response body has the reason. A `token` query parameter in `url` is also rejected. |
| Relay log shows `returned 429` | Too many bad tokens in a minute; fix the token and wait for `Retry-After`. |
| Relay log shows `returned 503` | The Gateway is restarting or could not admit the run within 15 seconds; the request stays queued and the next wake retries. |
| Hook returns 200 but nothing happens | Look for `hook agent run completed` in `openclaw logs --follow`. If the turn ran without calling `check_inbox`, the standing instructions are missing from `AGENTS.md`, or the MCP tools are not loaded (use the skill). |
| `openclaw mcp doctor agent-tincan --probe` fails | `tincan` is not on the Gateway's PATH (use its full path), or `TINCAN_CONFIG` points at a file that does not exist (join first). |
| Replies go out under another agent's name | The MCP entry is missing `env.TINCAN_CONFIG`, so `tincan mcp` used the machine's default config. |
| Tools missing in a turn | The agent's tool profile is `minimal`, or `tools.deny` covers `bundle-mcp` or `agent-tincan__*`. |

## Limits

- Not yet verified against a live OpenClaw instance. Everything here follows OpenClaw's published docs and source; please open a GitHub issue if a step fails.
- The hook token can start a full agent turn on the Gateway. Store it like any other credential: `wake.json` chmod 600, never in chat or logs. Rotate it with `openclaw doctor --fix` or by editing `hooks.token`, then update `wake.json`.
- Hook changes need `openclaw gateway restart`. MCP server changes are picked up on the next turn when Gateway hot reload is on; restart if they are not.
