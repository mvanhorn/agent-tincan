# Quick start

You need a Tailscale tailnet, one always-on Linux or macOS machine for the relay, and the `tincan` binary on each agent's machine.

## 1. Start the relay

On the always-on machine:

```bash
TS_AUTHKEY=tskey-auth-... tincan relay --admin my-laptop,my-phone
```

- It joins your tailnet as `tincan-relay`, so agents reach it at `http://tincan-relay`.
- `--admin` lists the machine names (as shown by `tailscale status`) allowed to invite and remove agents. On the relay host you can always use the local admin socket instead: `tincan invite muse --socket ~/.config/tincan-relay/admin.sock` (the path is printed at startup).
- A machine is an admin only if its name is in `--admin` and it has no Tailscale tags. Tag your agent machines (for example `tag:agent`, via `tailscale up --advertise-tags=tag:agent` or an auth key with that tag) so they can never be admins, even if one is renamed to match an admin machine.
- `--admin-login you@example.com` additionally requires an admin machine to be owned by that Tailscale login. It narrows admin rights on shared tailnets, but on a single-user tailnet every node has the same owner, so the machine list and tags still do the real work.
- If the host already runs tailscaled and you would rather not add a node, use `--listen 100.x.y.z --port 8787` with the host's tailnet IP.

Run it under your service manager (systemd, launchd) so it restarts.

## 2. Join two agents

On an admin device:

```bash
tincan invite grokbot --relay http://tincan-relay
```

On the agent's machine:

```bash
tincan join ABCD-EFGH --relay http://tincan-relay
```

Repeat for the second agent. Check with `tincan agents`.

## Rebuilt machines

If an agent's machine is rebuilt (a sandbox recreated from scratch, a VM reimaged), keep the same machine name. On the new machine run:

```bash
tincan rejoin --relay http://tincan-relay
```

Add `--proxy <url>` if the agent reaches the relay through a proxy, and `--name <agent>` if the machine ran several agents. The relay re-admits the new Tailscale node as the old agent when it is untagged, owned by the same login, and the old node is offline or gone, then `rejoin` saves the config. Queued requests are still waiting. Only a machine that was never joined needs an invite. Tagged machines are not re-admitted this way, and `tincan relay --no-auto-rebind` turns it off. See `docs/trust-model.md`.

## 3. Talk

From one agent:

```bash
tincan ask muse "what's on my calendar tomorrow?"
```

On the other:

```bash
tincan inbox
tincan reply <request-id> "Dentist at 3pm"
```

Or add the MCP server so the model gets the tools directly:

```json
{ "mcpServers": { "agent-tincan": { "command": "tincan", "args": ["mcp"] } } }
```

## 4. Make agents wake on their own

Each agent needs a way to notice requests when it isn't mid-conversation. Pick per agent (details in `docs/adapters/`):

| Agent can... | Use | Set up |
|---|---|---|
| receive a webhook | `webhook` | relay `wake.json` |
| receive email | `email` | relay `wake.json` (sent through AgentMail) |
| get a new turn when a background command ends | `wait` | agent runs `tincan wait &` |
| run Claude Code | `channel` | `tincan mcp --channel` |
| run a command on a live machine | `command` | agent runs `tincan listen --exec ...` |
| none of these | `none` | agent calls `check_inbox` at the start of each turn |

Relay-side wake settings live in `wake.json` in the relay state dir (chmod 600):

```json
{
  "grokbot":  { "method": "webhook", "url": "https://...", "bearer_token": "..." },
  "instinct": { "method": "email", "email_to": "agent@example.com", "agentmail_inbox": "bot@agentmail.to", "agentmail_key": "..." },
  "muse":     { "method": "wait" },
  "claude-code": { "method": "channel" },
  "chatgpt":  { "method": "none" }
}
```

Agents only ever see the method name, never the URL, address, or key.

## 5. Generate your team's prompts

Once agents are on the roster, `tincan onboard` builds the setup kit from it: a standing prompt for the Agent Tincan operator role, and for every agent, its join recipe and the exact text to paste into its standing instructions.

```bash
tincan onboard --operator grokbot
```

- `--operator <agent>` names the agent that runs the Agent Tincan operator prompt (an always-on agent such as Grok Bot). Without it, the prompt uses a neutral operator-host line and tells you to pass it.
- `--section agents` (or `operator`, `recipes`) prints just one part; `--json` prints the same kit as structured data, the shape the MCP tool `onboard` also returns.
- `--offline` skips the roster and makes no network call, so you can print the operator prompt and add-agent recipes before anyone has joined.
- Onboarding is read-only: it never mints invite codes or joins or removes agents. Run `tincan invite <name> --kind <kind>` yourself when the kit tells you to.
- Re-run it after any roster or wake change, and paste the fresh output over the old instructions.

## 6. See what happened

```bash
tincan trace            # recent chains (admin)
tincan trace <trace-id> # one chain, step by step
tincan audit-verify     # check the log has not been altered
```
