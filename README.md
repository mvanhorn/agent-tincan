# Agent Tincan

Let your AI agents call each other. Grok Bot can ask Muse to make a phone call, Muse can tell Grok Bot how it went, and Instinct can hand either of them work. Your laptop can be off.

## The point

Personal agents now live in different places: a cloud VM, a sandbox that pauses, a container that can only dial out through a proxy, a chat app in someone else's cloud, a terminal on your Mac. None of them can reach the others directly, and a plain webhook can't reach an agent that accepts no inbound connections.

Agent Tincan puts a tiny relay on your Tailscale network. Every agent dials out to it, so nothing needs an open port. The relay knows who sent each request because Tailscale tells it which machine the request came from. There are no keys to manage. Agents you join trust each other like teammates.

## How it works

1. One always-on machine runs `tincan relay`. It joins your tailnet as its own device.
2. You join each agent with a one-time code: `tincan invite muse` on your laptop or phone, then `tincan join <code>` on Muse.
3. Agents get tools, through MCP or the CLI: `ask`, `check_inbox`, `reply`, `get_reply`, `list_agents`, `cancel`, `claim`, `trace`.
4. `ask` queues the request. A listening agent gets it within about a second. An agent that isn't listening gets nudged the way it wakes best: a webhook, an email, a background command finishing, or a Claude Code channel. Otherwise the request waits for its next turn.
5. Replies wake the asker too. When an answer lands, the agent that asked is woken with the reply and the question it answers, so it can finish the job without waiting inline. If it leaves the reply unread, it is nudged again 5, 20 and 60 minutes later.
6. Requests and replies can carry attachments (images and small files, up to 8 per message and 10 MB each). They are stored on the relay, readable only by the agents on that message, and show up as images in the MCP tools; on the CLI use `--attach <path>` and `tincan attachment get <id>`.
7. Chains are tracked (Instinct to Muse to Grok Bot), loops are stopped with a hop limit and a cycle check, and every step lands in a tamper-evident log you can read with `tincan trace`.

ChatGPT can't join a tailnet, so the relay can also publish one OAuth-protected MCP endpoint through Tailscale Funnel for it.

## Onboarding

Once agents are joined, `tincan onboard` reads the live roster and writes the setup kit for you: a standing prompt for the Agent Tincan operator role, and for every agent, its join recipe and the exact text to paste into its standing instructions. Run `tincan onboard --offline` before anyone has joined to get the operator prompt and add-agent recipes on their own.

A single machine can run more than one agent, for example Claude Code and Codex on the same Mac, or Hermes and OpenClaw on the same mini; each still joins under its own name with its own invite. If an agent's machine is rebuilt, it heals itself: run `tincan rejoin` on the new machine with the same name and the relay re-admits it, no new invite needed.

## Ask your history

Add a `history` agent on your Mac and any allowed agent can ask what you last asked ChatGPT, claude.ai, Codex or Claude Code, and get your prompt back with the actual image attached. Ask Agent Tincan "what did I last ask ChatGPT? send the image" and it passes the question to `history` and forwards the answer.

`history` is a small service (`tincan history serve`), not another model. It reads Codex and Claude Code from their local logs, and ChatGPT and claude.ai through the Tincan Chrome extension in your already logged-in Chrome: a one-click Chrome Web Store install once the listing is live (load it unpacked until then); no cookies or tokens ever leave the browser, and Chrome is never restarted. Only agents on its allowlist (`~/.config/tincan/history-allow.txt`) can ask, and every agent in the request's chain must be on it. `tincan onboard` prints the setup. Details in [docs/adapters/history.md](docs/adapters/history.md).

## Keeping it running

- `tincan agents` shows every agent, how it wakes, and when it last called the relay. A wait or listen loop that died shows up as a growing "last seen".
- `tincan upgrade` updates an agent in place. Run the relay with `--dist <dir>` holding the release binaries and each agent pulls the right one for its OS, checks its sha256, and swaps itself.
- The Agent Tincan operator bot checks relay health, presence, wakes and queues in the background, fixes what it can, and stays silent. It speaks only when you ask it something or another agent sends it a request.

## Quick start

See [docs/quickstart.md](docs/quickstart.md). Guides for specific agents are in [docs/adapters/](docs/adapters/): [Grok Bot](docs/adapters/grokbot.md), [e2b sandboxes](docs/adapters/e2b.md), [proxy-only sandboxes](docs/adapters/proxy-sandbox.md), [ChatGPT](docs/adapters/chatgpt.md), [Claude Code](docs/adapters/claude-code.md), [Hermes Agent](docs/adapters/hermes.md), [OpenClaw](docs/adapters/openclaw.md), [OpenAI Codex CLI](docs/adapters/codex.md), and the [history agent](docs/adapters/history.md).

## Trust model

Joined agents act on each other's requests as if you asked. Read [docs/trust-model.md](docs/trust-model.md) before joining an agent that reads untrusted content and holds powers like spending money.

## Build

```bash
make build   # static ./tincan, CGO_ENABLED=0
make test
```

MIT licensed.
