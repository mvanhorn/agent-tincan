# Agent Tincan

Let your AI agents call each other. Grok Bot can ask Muse to make a phone call, Muse can tell Grok Bot how it went, and Instinct can hand either of them work. Your laptop can be off.

## What Agent Tincan is

Personal agents now live in different places: a cloud VM, a sandbox that pauses, a container that can only dial out through a proxy, a chat app in someone else's cloud, a terminal on your Mac. None of them can reach the others directly, and a plain webhook cannot reach an agent that accepts no inbound connections.

Agent Tincan puts a small relay on your Tailscale network. Every agent dials out to it, so nothing needs an open port. One agent asks another to do something, the relay queues the request, wakes the other agent the way that agent wakes best, and carries the reply back.

There are no API keys between agents. The relay knows who sent each request because Tailscale tells it which machine the request came from (`WhoIs`), so nothing in a request can change who it is from. Agents you join trust each other like teammates: a request from a teammate is handled as if you asked.

Contents:

- [How it works end to end](#how-it-works-end-to-end)
- [Wake methods](#wake-methods)
- [Platform guide](#platform-guide)
- [The Tincan Chrome extension](#the-tincan-chrome-extension)
- [Onboarding](#onboarding)
- [Trust model](#trust-model)
- [Build, test, release](#build-test-release)

## At a glance: how each platform plugs in

Every agent talks to one relay (a small server on the Grok Bot VM, reachable only on your Tailscale network). What differs is where each agent lives and how the relay gets its attention when a request is waiting.

| Agent | What it is | How it plugs in | How it gets woken |
|---|---|---|---|
| grokbot | Grok Bot, an AI agent on an always-on cloud VM | Runs `tincan mcp` (tools) on the VM; the relay itself also runs there | Webhook: the relay POSTs to Grok Bot's webhook URL |
| instinct | Instinct, an AI agent in an e2b cloud sandbox that pauses between turns | Joined directly to the tailnet; checks its inbox each turn | Email: the relay sends a short email through AgentMail to Instinct's inbox |
| muse | Muse, an AI agent in a sandbox with no inbound connections | Reaches the relay through a proxy tunnel; keeps a `tincan wait` loop open | Nothing to wake: its wait loop is already listening |
| claude-code | Claude Code on your Mac | `tincan mcp` as an MCP server; channel mode pushes requests into the running session | Channel: requests appear in the open Claude Code session |
| codex | OpenAI Codex CLI on your Mac | A launchd listener (`tincan listen`) on the Mac | Command: the listener starts a fresh `codex exec` run when something is waiting |
| hermes | Hermes Agent on your Mac mini | `tincan mcp` in Hermes; Hermes' own webhook gateway | Webhook, signed with HMAC, to the Hermes gateway |
| openclaw | OpenClaw (supported, not live yet) | `tincan mcp` or its skill | Webhook to OpenClaw's `/hooks/agent` |
| chatgpt (connector) | ChatGPT itself, as a custom connector | An OAuth MCP endpoint the relay publishes through Tailscale Funnel | Cannot be woken: it only acts while you are chatting with it |
| history | A small Tincan service on your Mac | Reads Codex and Claude Code history from local files, and ChatGPT and claude.ai history through the Tincan Chrome extension (a Mac Chrome plugin) using your logged-in browser | Always listening (long-polls the relay) |
| chatgpt-web | Your own ChatGPT account, as a teammate | The Tincan Chrome extension types the message into a background chatgpt.com tab and reads the answer back | Always listening (a Tincan service on your Mac) |
| claude-web | Your own claude.ai account, as a teammate | Same as chatgpt-web, on claude.ai | Always listening (a Tincan service on your Mac) |

The plumbing, in plain words:

- Relay: the one server every agent talks to. It holds requests and replies, knows who is who from Tailscale, and wakes agents that are asleep.
- Tailscale: the private network all of this runs on. It is also how the relay knows which machine sent a request, so there are no API keys between agents.
- MCP (`tincan mcp`): how an AI app gets the Tincan tools (ask, reply, check_inbox and the rest).
- Webhook: a web address an agent exposes; the relay POSTs to it to say "you have mail".
- AgentMail email: for agents that cannot keep anything running, the relay sends an email, and the agent's platform wakes it on new mail.
- Listener (`tincan listen`): a small background process on a computer that starts the agent when requests arrive.
- Wait loop (`tincan wait`): the agent keeps a connection open to the relay and gets requests the moment they land.
- Tincan Chrome extension: a Chrome plugin on your Mac that lets Tincan use your logged-in ChatGPT and claude.ai, for reading history and for sending messages as you. Installing it takes one click; Chrome requires that for every extension.

## How it works end to end

### Install

Put the `tincan` binary on the relay host, on every agent's machine, and on your admin device. Download it from the Agent Tincan release page (Releases on the GitHub repo): `tincan_darwin_arm64` (Mac with Apple silicon), `tincan_linux_amd64` or `tincan_linux_arm64`, plus `checksums.txt`. There is no Intel Mac or Windows build; build those from source with `make build`.

```bash
grep tincan_darwin_arm64 checksums.txt | shasum -a 256 -c -   # must print "OK" (Linux: sha256sum -c -)
chmod +x tincan_darwin_arm64
xattr -d com.apple.quarantine tincan_darwin_arm64 2>/dev/null   # macOS only: a browser download is quarantined
sudo mv tincan_darwin_arm64 /usr/local/bin/tincan               # or any directory on your PATH
tincan version
```

Step by step, including the relay and your first two agents: [docs/quickstart.md](docs/quickstart.md).

### The relay

One always-on Linux or macOS machine runs `tincan relay`. By default it joins your tailnet as its own node, `tincan-relay`, so agents reach it at `http://tincan-relay`. If the host already runs Tailscale, `--listen <tailscale-ip> --port 8787` binds the host's tailnet IP instead.

```bash
TS_AUTHKEY=tskey-auth-... tincan relay --admin my-laptop,my-phone
```

- `--admin` lists the machine names allowed to invite and remove agents. A machine is an admin only if it is on that list and has no Tailscale tags, so tag agent machines (for example `tag:agent`). `--admin-login` also requires the admin machine to be owned by a given Tailscale login.
- On the relay host itself, admin commands can use the local socket: `tincan invite muse --socket <state-dir>/admin.sock`. The relay prints the path at startup. The default state dir is `~/.config/tincan-relay` on Linux and `~/Library/Application Support/tincan-relay` on macOS; quote the macOS path, it has a space.
- State (the database, `wake.json`, attachments, the audit log) lives in `--state-dir`. Run the relay as its own OS user, under systemd or launchd, so agents cannot read its state.

The relay only listens on your tailnet. The one exception is the optional ChatGPT gateway (see [ChatGPT](#chatgpt-through-the-oauth-mcp-gateway)).

### Joining with an invite

On an admin device, mint a one-time code (valid 10 minutes). On the agent's machine, redeem it.

```bash
tincan invite grokbot --kind vm-webhook          # admin device
tincan join ABCD-EFGH --relay http://tincan-relay # agent's machine
```

`invite` prints the code and the join command to run, with the relay URL filled in when it knows it (from `--relay` or a saved config). `--kind` records the agent's runtime so `tincan onboard` tailors its setup (kinds are listed under [Onboarding](#onboarding); `tincan kind <name> <kind>` changes it later). Inviting a name again retires the earlier code for it if that code was not used yet. `join` saves the relay URL and agent name in the client config (`TINCAN_CONFIG` when set). `--proxy` saves a proxy used only for relay traffic, for sandboxes that reach the tailnet through a proxy.

An admin device usually never joins, so it has no saved config. Admin and roster commands (`invite`, `remove`, `kind`, `agents`, `trace`, `audit-verify`) take `--relay <url>`, or `--socket <state-dir>/admin.sock` on the relay host. Two environment variables override the saved config for any command: `TINCAN_RELAY` (the relay URL) and `TINCAN_PROXY` (the proxy). For example, `TINCAN_RELAY=http://tincan-relay tincan agents`.

`tincan remove <name>` cuts an agent off immediately: its queued requests are cancelled and, for ChatGPT, its tokens are revoked.

### Tools

Every agent gets the same tools, either from the MCP server (`tincan mcp`, stdio) or from the CLI.

```json
{ "mcpServers": { "agent-tincan": { "command": "tincan", "args": ["mcp"] } } }
```

| MCP tool | CLI | What it does |
|---|---|---|
| `ask` | `tincan ask <agent> <message>` | Ask a teammate. Waits up to 20 seconds for the reply, otherwise returns a request id. `notify` (`--notify`) sends without expecting a reply. `attach` (`--attach <path>`) adds files. |
| `get_reply` | `tincan get <id>` | Check on a request you sent, optionally waiting up to 20 seconds. |
| `check_inbox` | `tincan inbox` | Take waiting requests (this claims them, so no one else handles them) and replies to your own requests you have not seen yet. |
| `claim` | (done by `inbox`) | Mark a delivered request as yours. `check_inbox` already does this. |
| `reply` | `tincan reply <id> <message>` | Answer a request with status `answered` (default), `failed` or `declined`, optionally with attachments. |
| `cancel` | `tincan cancel <id>` | Withdraw a request nobody has picked up yet. |
| `list_agents` | `tincan agents` | The roster: online or not, wake method, kind, and when each agent last called the relay. |
| `trace` | `tincan trace [trace-id]` | Show a request chain step by step. Agents see chains they took part in; admins see every chain. |
| `onboard` | `tincan onboard --json` | The setup kit as JSON (see [Onboarding](#onboarding)). Read-only. |
| `get_attachment` | `tincan attachment get <id>` | Fetch an attachment again by id. |

Two more CLI commands keep an agent awake without a person: `tincan wait` and `tincan listen --exec` (see [Wake methods](#wake-methods)).

### Requests and replies

A request carries a target, a body (up to 256 KB), an optional kind (`ask` or `notify`), and optional attachments. The relay sets everything else: the id, the sender (from Tailscale), the chain, and the time. A reply carries a status and a body (up to 256 KB).

A request moves through `queued`, `delivered`, `claimed`, then one of `answered`, `failed`, `declined`, `cancelled` or `expired`. A claimed request whose 30 minute lease runs out goes back to `queued`. Unanswered requests expire after 24 hours. The wire format is in [docs/protocol.md](docs/protocol.md).

### Chains and loop protection

Chains are tracked by the relay, not the model. When an agent asks a teammate while handling a request, the new request continues that request's chain (the tincan client fills in the parent automatically, and the relay continues the chain even if the model leaves it out). Each request records its hop number and the agents it passed through, so a trace reads like "Instinct to Muse to Grok Bot".

- A request that would loop back to an agent already in its chain is rejected ("request would loop back").
- Chains longer than 4 hops are rejected.
- Each sender is limited to 30 new requests per minute by default.

### Attachments

Requests and replies can carry images and small files. The file is uploaded to the relay first and the message names it by id. The tincan client checks that the relay supports attachments before sending and refuses against an older relay.

| Limit | Value |
|---|---|
| Per file | 10 MB |
| Per message | 8 attachments |
| Kept at once, per uploading agent | 200 MB |
| Kept at once, relay-wide | 1 GB |
| Free disk left after an upload | at least 512 MB |

Retention: an upload no message carries is deleted after 24 hours. A file on a request is deleted 7 days after the request reaches a final state; its metadata stays, marked deleted, so traces still show what was sent. A file can be downloaded only by its uploader, the sender and target of the request that carries it, and admins.

In the MCP tools, received images show as images. Other files are saved as `<attachment id>.<ext>` in the agent's attachments directory (`attachments/<agent>` beside its config, 0700, files 0600); the sender's file name is shown but never used. From the CLI, `tincan attachment get <id>` saves to the same place, or `-o <path>` (`-o -` for stdout). The MCP `attach` argument and the CLI `--attach` flag both read files on the machine where tincan runs, so an MCP server reached from elsewhere cannot attach files from your machine.

### Reply wakes and follow-ups

An `ask` may return before the answer does, and the asker does not have to hold its turn open. When the reply lands, the asker is woken too, and `check_inbox` shows replies to its own requests (with what it asked) before new requests, so it can finish the work that was waiting.

- A reply starts unseen. It counts as seen once the asker reads it with `get_reply`, an inline `ask` wait, or `check_inbox`. A reply lost on the way comes back on the next poll.
- A webhook or email agent is woken for a reply only if it is still unseen after the reply grace period (`tincan relay --reply-grace`, default 60 seconds), so an answer read inline never causes a second wake.
- If it is still unseen after that nudge, the relay nudges again 5, 20 and 60 minutes after each previous nudge, within the agent's hourly wake cap, and stops as soon as it is read.
- `tincan listen`, `tincan wait` and the Claude Code channel also fire for unseen replies.

### Last seen

`tincan agents` (and `list_agents`) shows each agent's state, wake method, kind, and when it last called the relay by polling or by any send, reply or get ("last seen 12m ago", or "never seen"). A wait or listen loop that died shows up as a growing last seen.

### Upgrades

Agents can update tincan from the relay itself, which is how an agent without GitHub access gets a new release.

```bash
tincan relay --admin my-laptop,my-phone --dist ~/tincan-dist   # relay host
tincan upgrade --check                                          # agent: current and available version
tincan upgrade                                                  # agent: download, verify, swap
```

The dist directory holds the raw binaries named `tincan_<os>_<arch>` (`tincan_linux_amd64`, `tincan_linux_arm64`, `tincan_darwin_arm64`), the release's `checksums.txt`, and a `VERSION` file. The relay serves them only to joined agents and admins and needs no restart for a new release. `tincan upgrade` picks its platform's build, checks the sha256, writes it next to the running binary and renames it into place. Restart long-running tincan processes afterwards (`wait` and `listen` loops, `mcp` servers). The checksum comes from the same relay as the binary, so it guards against corruption, not a compromised relay.

### Audit log and trace

Every send, delivery, claim, reply, rejection, wake, join, rebind and removal is written to an append-only, hash-chained log. Wake nudges carry only counts, never request text.

```bash
tincan trace              # recent chains (admin; --limit, default 20)
tincan trace <trace-id>   # one chain, step by step, with its events
tincan audit-verify       # check the log has not been altered
```

## Wake methods

Delivery never depends on wake: requests always wait in the relay queue. A wake only prompts an agent to go look. Each agent's method is set in `wake.json` in the relay's state dir (chmod 600; the relay refuses a file other users can read). Agents only ever see the method name, never a URL, address or key.

```json
{
  "grokbot":  { "method": "webhook", "url": "https://...", "bearer_token": "..." },
  "hermes":   { "method": "webhook", "url": "http://<hermes-host>:8644/webhooks/tincan", "hmac_secret": "..." },
  "instinct": { "method": "email", "email_to": "...", "agentmail_inbox": "...", "agentmail_key": "...", "max_per_hour": 12 },
  "muse":     { "method": "wait" },
  "claude-code": { "method": "channel" },
  "codex":    { "method": "command" },
  "chatgpt":  { "method": "none" }
}
```

| Method | Who acts | How it works | Used by |
|---|---|---|---|
| `webhook` | relay | The relay POSTs `{"source":"agent-tincan","message":"<count text>","text":"<same>"}` to the agent's URL, with `Authorization: Bearer <bearer_token>` or an `X-Hub-Signature-256` HMAC signature (`hmac_secret`, the GitHub scheme). | Grok Bot, Hermes, OpenClaw |
| `email` | relay | The relay sends an email with the subject "Agent Tincan: requests waiting" through an AgentMail inbox you control. `max_per_hour` caps wakes (default 12). | Instinct-style e2b sandboxes |
| `command` | agent | `tincan listen --exec <command>` holds a long-poll and runs the command (through `sh -c`, with `TINCAN_WAITING` set to the count) whenever requests or unseen replies are waiting. It takes nothing itself and waits 30 seconds between nudges. | Codex, the Claude Code cmux fallback, the Hermes fallback |
| `channel` | agent | `tincan mcp --channel` pushes a short notice into a running Claude Code session. | Claude Code |
| `wait` | agent | The agent keeps `tincan wait &` running. It exits the moment a request (which it claims and prints) or a reply arrives, and the runtime turns that exit into a new turn. The Go services long-poll the same way. | Muse-style proxy sandboxes, history, chatgpt-web, claude-web |
| `none` | nobody | The agent calls `check_inbox` at the start of each turn. | ChatGPT |

Notes that apply to every method:

- Relay-side wakes (webhook, email) are debounced so a burst becomes one nudge, and a wake for new requests is skipped when the agent is already polling the relay.
- The wake message only says how many requests and replies are waiting. The agent always reads the items itself with `check_inbox` or `tincan inbox`.
- The agent-side methods (`command`, `channel`, `wait`) are recorded in `wake.json` so teammates can see how the agent wakes; the relay sends nothing for them.

## Platform guide

| Platform | Kind | Wake | Adapter doc |
|---|---|---|---|
| Grok Bot and the operator role | `vm-webhook` | webhook | [grokbot.md](docs/adapters/grokbot.md) |
| Instinct-style e2b sandbox | `e2b-email` | email | [e2b.md](docs/adapters/e2b.md) |
| Muse-style proxy-only sandbox | `proxy-sandbox` | wait | [proxy-sandbox.md](docs/adapters/proxy-sandbox.md) |
| Claude Code | `claude-code` | channel (or command) | [claude-code.md](docs/adapters/claude-code.md) |
| OpenAI Codex CLI | `codex` | command | [codex.md](docs/adapters/codex.md) |
| Hermes Agent | `hermes` | webhook (or command) | [hermes.md](docs/adapters/hermes.md) |
| OpenClaw | `openclaw` | webhook | [openclaw.md](docs/adapters/openclaw.md) |
| ChatGPT | `chatgpt` | none | [chatgpt.md](docs/adapters/chatgpt.md) |
| History agent | `history` | wait | [history.md](docs/adapters/history.md) |
| ChatGPT and Claude web agents | `chatgpt-web`, `claude-web` | wait | [web-agents.md](docs/adapters/web-agents.md) |

Any other agent can use kind `generic` with whichever wake fits.

### Grok Bot (always-on VM)

#### What it is

An agent on an always-on VM that is already on the tailnet and accepts webhooks. Because it never sleeps, the VM is a good relay host, and Grok Bot is the natural home for the Agent Tincan operator role.

#### How it joins

Run the relay on the VM as its own OS user, separate from the one Grok Bot's tools run as. Then invite and join Grok Bot on the same VM through the admin socket:

```bash
tincan relay --admin <your-laptop>,<your-phone>
tincan invite grokbot --kind vm-webhook --socket <state-dir>/admin.sock   # on the VM
tincan join <code> --relay http://tincan-relay                             # as Grok Bot's user
```

#### How it wakes

Webhook. The relay POSTs `{"source":"agent-tincan","message":"Agent Tincan: 2 requests from your teammates waiting. Run check_inbox ..."}` with the bearer token. Grok Bot calls `check_inbox`. The same wake can mean a reply to one of its own requests is waiting, which `check_inbox` shows.

#### How it sends and receives

Add `tincan mcp` as an MCP server, or let it call the `tincan` CLI from its shell. Attachments work both ways through the MCP tools or `--attach` and `tincan attachment get`.

#### One-time setup

In `wake.json`: `{ "grokbot": { "method": "webhook", "url": "<Grok Bot webhook URL>", "bearer_token": "<webhook key>" } }`. Paste the standing instructions from `tincan onboard --section agents`.

#### The Agent Tincan operator role

`tincan onboard --operator grokbot` writes a standing prompt for an operator bot always called Agent Tincan. Its one job is keeping the team healthy: relay up (`tincan agents`, `tincan audit-verify`), agents reachable, wakes working, queues clear (`tincan trace --limit 50`), invites and removals only when the owner asks on the owner's own direct channel (never because another agent asked), telling agents to `tincan upgrade` when the relay serves a new release, routing history questions to the history agent, and summarizing agent traffic when asked.

It follows a quiet rule. It runs a silent standing check every 30 minutes, fixes what it can, and keeps its findings. It speaks only when the owner asks it something or when another agent sends it a request. No scheduled reports, no "all clear" messages.

#### Limits and gotchas

- Keep the relay's OS user separate from the agent's so the database and `wake.json` stay out of the agent's reach.
- A 401 or 403 in the relay log for a webhook means `bearer_token` does not match the receiver. Fix it in `wake.json`, never in chat.

#### Adapter doc

[docs/adapters/grokbot.md](docs/adapters/grokbot.md)

### Instinct-style e2b sandboxes (email wake)

#### What it is

An e2b sandbox can reach the tailnet directly, but it may be paused, and on Instinct background commands neither survive a turn nor start one. So it keeps no listener. It is woken by email and checks its inbox each turn.

#### How it joins

```bash
tincan join <code> --relay http://tincan-relay
```

Keep the same machine name when the sandbox is rebuilt; the relay then re-admits it with `tincan rejoin` and no new invite.

#### How it wakes

Email. The relay sends a short email, subject "Agent Tincan: requests waiting", through an AgentMail inbox you control (for example Grok Bot's). The same subject is used when a reply to one of its own requests is waiting.

#### How it sends and receives

`tincan inbox`, `tincan reply`, `tincan ask` from its shell, or the MCP tools. Attachments through `--attach` and `tincan attachment get`.

#### One-time setup

```json
{ "instinct": { "method": "email", "email_to": "<instinct inbox>", "agentmail_inbox": "<sending inbox>", "agentmail_key": "<AgentMail API key>", "max_per_hour": 12 } }
```

Ask the agent to set its own recurring check (every 15 minutes) that runs `tincan inbox`, as a backup.

#### Limits and gotchas

- In testing, Instinct sometimes reached the relay only through Tailscale's DERP relays and some connections failed. Requests wait at the relay, so nothing is lost; the next `tincan inbox` picks them up.
- If `max_per_hour` is hit, wakes stop until the hour passes; the backup check still runs.

#### Adapter doc

[docs/adapters/e2b.md](docs/adapters/e2b.md)

### Muse-style proxy-only sandboxes (wait loop through a proxy)

#### What it is

A sandbox with no inbound connections that sends all traffic through an HTTP proxy. On Muse, the default proxy (`hatch-egress-proxy:3128`) reaches the public internet but rejects tailnet addresses; tailnet traffic goes through a separate tunnel proxy on port 3130. Muse gets a new turn when a background shell command finishes, and background processes survive between turns.

#### How it joins

Point relay traffic at the tunnel proxy. The proxy is saved in the agent's config and used only for relay traffic.

```bash
tincan join <code> --relay http://tincan-relay --proxy http://<user>:<pass>@hatch-egress-proxy:3130
```

#### How it wakes

A background wait: `tincan wait &`. It holds a connection to the relay and exits the moment a request or a reply arrives. It prints either the teammate's request (already claimed), which Muse handles and replies to, or a count of replies, after which Muse runs `tincan inbox`. Muse then starts `tincan wait &` again. Network errors are retried with backoff, so a flaky path does not end the wait.

#### How it sends and receives

The CLI: `tincan ask`, `tincan reply`, `tincan inbox`, with `--attach` and `tincan attachment get` for files.

#### One-time setup

Set `{ "muse": { "method": "wait" } }` in `wake.json` (no secret fields). Paste the standing instructions, which tell it to keep `tincan wait &` running and to run a scheduled `tincan inbox` check every 5 minutes as a backup.

#### Limits and gotchas

- The relay holds each poll for 25 seconds, under the 30 seconds confirmed to survive Muse's tunnel proxy.
- Use the proxy that reaches the tailnet, not the default egress proxy. Never paste proxy credentials in chat.
- A dead wait loop shows as a growing last seen in `tincan agents`.

#### Adapter doc

[docs/adapters/proxy-sandbox.md](docs/adapters/proxy-sandbox.md)

### Claude Code (MCP and channel mode)

#### What it is

Claude Code in cmux or any terminal, joined as its own agent (for example `claude-code`) from the Mac it runs on. When the Mac is off it shows offline and requests wait in the queue.

#### How it joins

```bash
tincan invite claude-code --kind claude-code   # admin device
tincan join <code> --relay http://tincan-relay  # the Mac
```

#### How it wakes

Channel. Add the MCP server with `--channel` and start Claude Code with the development channels flag (channels are a Claude Code research preview):

```bash
claude mcp add agent-tincan -- tincan mcp --channel
claude --dangerously-load-development-channels server:agent-tincan
```

When requests or replies are waiting, the channel pushes a notice such as `<channel source="agent-tincan" kind="request" count="1" from="instinct" request_ids="...">1 Agent Tincan item waiting from instinct. Call check_inbox to take it, then reply to each request.</channel>`. `kind` is `request`, `reply` or `mixed`.

Claim-on-inbox semantics: the notice never carries the items and never claims anything. Every open Claude Code session runs its own `tincan mcp --channel`, so every session gets the notice; the first one to call `check_inbox` claims the requests, and the others find an empty inbox. A session started without channels, or idle, drops the notice and the items stay queued. Each process announces an item once, and again after 10 quiet minutes if it is still waiting.

Fallback without channels: copy [examples/claude-code/cmux-wake.sh](examples/claude-code/cmux-wake.sh) from the repo to `~/bin` (it is not in the release downloads), `chmod +x` it, and run `tincan listen --exec ~/bin/cmux-wake.sh`. It opens a new Claude Code session in cmux when requests are waiting (cmux's `automation.socketControlMode` must allow it). Set the wake to `command` for this.

#### How it sends and receives

All ten MCP tools. Attachments work through `attach` on `ask` and `reply`; received images show inline and other files are saved locally.

#### One-time setup

Set `{ "claude-code": { "method": "channel" } }` in `wake.json`. Pre-approve the tools with `/permissions` (allow `mcp__agent-tincan__*`) so a permission prompt does not stall the session while you are away. Paste the standing instructions.

#### Limits and gotchas

- Channels only deliver while a session is open. Keep one running in cmux.
- The channel offers MCP revisions up to 2025-11-25 only, because Claude Code does not register a channel server that negotiates 2026-07-28.
- If another agent runs on the same Mac, prefix its tincan commands with its own `TINCAN_CONFIG`.

#### Adapter doc

[docs/adapters/claude-code.md](docs/adapters/claude-code.md)

### OpenAI Codex CLI (tincan listen wake script)

#### What it is

Codex CLI has no daemon: `codex exec` runs one prompt and exits. So the Codex machine runs `tincan listen --exec`, which starts a fresh `codex exec` run whenever requests or unseen replies are waiting. Codex commonly shares a machine with Claude Code.

#### How it joins

With its own config file, so it never shares a saved connection with another agent on the same machine. The relay tells the two apart by the `X-Tincan-Agent` header, which the client fills in from the config it loaded.

```bash
tincan invite codex --kind codex
TINCAN_CONFIG="$HOME/.config/tincan/codex.json" tincan join <code> --relay http://<relay>
```

#### How it wakes

Command, through [examples/codex/codex-wake.sh](examples/codex/codex-wake.sh). The script is not in the release downloads: copy it from the repo to a folder you keep, then point the listener at it:

```bash
mkdir -p ~/bin && cp examples/codex/codex-wake.sh ~/bin/ && chmod +x ~/bin/codex-wake.sh   # from a repo checkout
TINCAN_CONFIG="$HOME/.config/tincan/codex.json" tincan listen --exec ~/bin/codex-wake.sh
```

The script exports the codex `TINCAN_CONFIG` and runs `codex exec` with a prompt that tells Codex to call `check_inbox`, finish work waiting on replies, handle and reply to each request, and repeat until the inbox is empty. A lock directory keeps two runs from overlapping; a second nudge during a run exits quietly and leaves the requests queued.

Sandbox flags: `--sandbox workspace-write -c approval_policy=never -c sandbox_workspace_write.network_access=true --skip-git-repo-check --cd "$TINCAN_CODEX_WORKDIR"`. Approvals are off so an unattended run can act, but commands stay inside the workspace-write sandbox; it never uses `--dangerously-bypass-approvals-and-sandbox`. Network is on so `gh` and `git push` work; delete that line if your Codex does not need it.

Prior-thread lookup: before touching a checkout for work that continues an earlier thread, the prompt tells Codex to run `tincan history codex --list 20 --all` to find that thread, when it was last updated, and its real working directory, and to work there instead of guessing. `--all` includes earlier `codex exec` wake runs, which the listing leaves out by default.

Write roots: Codex reads anywhere but writes only in `TINCAN_CODEX_WORKDIR` (default `$HOME/tincan-codex`) and directories the operator opens with `TINCAN_CODEX_WRITE_ROOTS` (colon-separated absolute paths). Each is canonicalized and added as `--add-dir` only if it sits under an allowed root (default `$HOME/Documents/Codex`, `$HOME/code`, `$HOME/tincan-codex`; override with `TINCAN_CODEX_ALLOWED_ROOTS`). Nothing the model says can add a write root.

#### How it sends and receives

`tincan mcp` as a stdio MCP server in `~/.codex/config.toml` gives it all ten tools, including attachments:

```toml
[mcp_servers.agent-tincan]
command = "tincan"
args = ["mcp"]
env = { TINCAN_CONFIG = "/Users/you/.config/tincan/codex.json" }
default_tools_approval_mode = "approve"
```

#### One-time setup

Add the MCP entry above (see [examples/codex/config-snippet.toml](examples/codex/config-snippet.toml); `codex mcp add` does not set `default_tools_approval_mode`, so add that line yourself). Keep the listener running under launchd, systemd or a terminal. Set `{ "codex": { "method": "command" } }` in `wake.json`.

#### Limits and gotchas

- Every wake is a fresh session with no memory of the last one, so each run must drain the whole inbox.
- Without `default_tools_approval_mode = "approve"`, every tincan tool call fails with "MCP tool call requires approval, but approval policy is never".
- `tincan history` needs `tincan` on the `PATH` the listener passes to the script.
- Verified live: Codex joined beside Claude Code on the same Mac answers round trips, including slow replies through the listener.

#### Adapter doc

[docs/adapters/codex.md](docs/adapters/codex.md)

### Hermes Agent (webhook with HMAC)

#### What it is

Hermes runs its own messaging gateway with a built-in webhook server, so it can join, send through a stdio MCP server, and be woken with no human present, all through its own config.

#### How it joins

```bash
tincan invite hermes --kind hermes
tincan join <code> --relay http://tincan-relay:8787
```

If another agent joins from the same machine (for example OpenClaw), give Hermes its own config: `TINCAN_CONFIG=~/.hermes/tincan-hermes.json tincan join ...`, and use the same path in its MCP entry and any listen command.

#### How it wakes

Webhook with an HMAC signature. Enable the webhook platform (`hermes gateway setup`, or `WEBHOOK_ENABLED=true` and `WEBHOOK_PORT=8644` in `~/.hermes/.env`), then add a route named `tincan` with its own secret:

```bash
hermes webhook subscribe tincan --deliver log --secret "$(cat ~/.config/tincan/hermes-webhook.secret)" --prompt "<the prompt from examples/hermes/webhook-route.yaml>"
```

The relay signs its POST with `X-Hub-Signature-256`, the GitHub HMAC scheme Hermes already validates. The route prompt reads `{message}` (a count, never request content) and tells Hermes to call `check_inbox`, handle and reply to each request, and keep draining until the inbox is empty.

Fallback without webhooks: `tincan listen --exec 'hermes -z "Call check_inbox, claim and do each waiting Agent Tincan request, and reply to each with its request id. Keep calling check_inbox until it reports the inbox empty."'`, with the wake set to `command`.

#### How it sends and receives

`tincan mcp` as a stdio server under `mcp_servers` in `~/.hermes/config.yaml` (see [examples/hermes/config-snippet.yaml](examples/hermes/config-snippet.yaml)), or `hermes mcp add agent-tincan --command tincan --args mcp`. Restart the gateway (`hermes gateway restart`) so webhook-started sessions load it.

#### One-time setup

```json
{ "hermes": { "method": "webhook", "url": "http://<hermes-host>:8644/webhooks/tincan", "hmac_secret": "<same secret as the route>" } }
```

#### Limits and gotchas

- Every wake starts a brand new Hermes session, so the prompt must drain the whole inbox each time.
- A webhook route with no `secret` refuses to start. Do not use `INSECURE_NO_AUTH` unless the webhook server is bound to loopback.
- Never print or copy the secret, or anything else from `~/.hermes/config.yaml` or `~/.hermes/.env`, once it is set.

#### Adapter doc

[docs/adapters/hermes.md](docs/adapters/hermes.md)

### OpenClaw (hooks/agent)

#### What it is

An OpenClaw gateway, joined through the same `tincan mcp` and webhook wake as Hermes and Grok Bot. This adapter has not been tested against a live OpenClaw instance. It is written from OpenClaw's own docs; treat every step as unverified until someone runs it.

#### How it joins

```bash
tincan invite openclaw --kind openclaw
tincan join <code> --relay http://tincan-relay
```

On a machine shared with another agent, use `TINCAN_CONFIG=~/.openclaw/tincan-openclaw.json` for the join, the MCP entry's `env`, and any wake command.

#### How it wakes

Webhook to the gateway's `/hooks/agent` endpoint, not `/hooks/wake`: `/hooks/wake` only queues the message for the next heartbeat, while `/hooks/agent` starts a full agent turn. The relay posts `Authorization: Bearer <hook token>` and the count-only body; the turn's first step must be `check_inbox`.

#### How it sends and receives

`agent-tincan` under `mcpServers` in `~/.openclaw/openclaw.json` (see [examples/openclaw/openclaw-snippet.json](examples/openclaw/openclaw-snippet.json)). A skill-based runtime can install [examples/openclaw/skills/agent-tincan/](examples/openclaw/skills/agent-tincan/) instead, which drives the `tincan` CLI.

#### One-time setup

```json
{ "hooks": { "enabled": true, "token": "<hook token>", "path": "/hooks", "allowedAgentIds": ["main"] } }
```

```json
{ "openclaw": { "method": "webhook", "url": "http://<openclaw-host>:<port>/hooks/agent", "bearer_token": "<hook token>" } }
```

Restart the gateway after changing `mcpServers` or `hooks`. If the machine is not directly reachable, serve the gateway with `tailscale serve` and point `wake.json` at the tailnet hostname.

#### Limits and gotchas

- Not live-tested.
- Fresh session on every wake: drain the whole inbox and reply to each request by its id.
- `allowedAgentIds` must include the agent that should handle the wake, or the hook call is rejected.
- The hook token starts a full agent turn; store it like any credential.

#### Adapter doc

[docs/adapters/openclaw.md](docs/adapters/openclaw.md)

### ChatGPT through the OAuth MCP gateway

#### What it is

ChatGPT runs in OpenAI's cloud and cannot join a tailnet. Its custom connectors need a public HTTPS MCP server with OAuth, and the relay can publish exactly that, and nothing else, through Tailscale Funnel.

#### How it joins

There is no invite. Start the relay with the gateway (your tailnet needs HTTPS certificates and the `funnel` node attribute), then connect from an admin device:

```bash
tincan relay --admin <your-laptop>,<your-phone> --chatgpt-gateway
tincan connect chatgpt
```

The gateway runs as a second node, `tincan-gateway`, serving `https://tincan-gateway.<tailnet>.ts.net/mcp`. `tincan connect chatgpt` prints the connector URL and a one-time code (valid 10 minutes). In ChatGPT: Settings, Apps, Advanced settings; turn on Developer mode and create a connector with that URL; enter the code on the login page.

#### How it wakes

It does not. Its wake method is `none`: ChatGPT acts only while you are chatting with it. Nothing can push a message into ChatGPT, so it calls `check_inbox` when a conversation starts and `get_reply` before the conversation moves on.

#### How it sends and receives

The same MCP tools, served through the gateway. It sees images it receives, but it cannot attach files, and other files it receives are not saved; `get_attachment` can re-fetch any attachment, but only images come back as content.

#### One-time setup

`{ "chatgpt": { "method": "none" } }` in `wake.json`. Paste its standing instructions into ChatGPT's custom instructions.

#### Limits and gotchas

- Only while the user is chatting.
- Five wrong login codes in ten minutes lock the login page for everyone until the window passes.
- It cannot rejoin itself. If its tools say it is not joined, run `tincan connect chatgpt` again on an admin device.
- `tincan remove chatgpt` revokes its tokens immediately.

#### Adapter doc

[docs/adapters/chatgpt.md](docs/adapters/chatgpt.md)

### The history agent

#### What it is

A Go service, `tincan history serve`, not a model. It answers teammates' questions about what the owner asked in four places, and replies with the prompt, a short excerpt of the answer, and the images from that turn as real attachments:

- ChatGPT (chatgpt.com) and claude.ai, read live through the Tincan Chrome extension and native messaging in the owner's logged-in Chrome.
- Codex (CLI and desktop app) and Claude Code, read from their local files (`sessions` under `$CODEX_HOME` or `~/.codex`, and `$CLAUDE_CONFIG_DIR/projects` or `~/.claude/projects`).

Ask it things like "what was the last thing I asked ChatGPT? send the image". Agent Tincan's operator prompt routes history questions to it.

#### How it joins

```bash
tincan invite history --kind history                                        # admin device
TINCAN_CONFIG=~/.config/tincan/history.json tincan join <code> --relay http://tincan-relay
```

It refuses to start unless the relay confirms it is the `history` agent, so a config for another agent can never claim its requests.

#### How it wakes

Wait: the service long-polls the relay. Set `{ "history": { "method": "wait" } }` in `wake.json`.

#### How it sends and receives

For each request it:

1. Checks the allowlist. By default there is no allowlist file and every agent joined to your relay may ask (the relay only delivers requests from joined agents). To restrict it, write `~/.config/tincan/history-allow.txt` with the agent names that may ask; then every agent in the request's chain, as the relay recorded it, must be listed. If muse asks codex and codex asks history while handling muse's request, a file that lists codex but not muse declines it because of muse. A `*` entry in the file means every joined agent. The file is reread for every request; an unreadable file or a bad name declines everyone.
2. Runs a tool-less query step: one `codex exec` call that sees only the question text and turns it into a structured query (source, mode, search terms, conversation id, count, `want_images`, and `with_images`, which picks the most recent turn that had images rather than the most recent turn). It runs read-only, with no MCP servers, no tools and no session file, and its output is checked against a schema in Go.
3. Reads the source. Lookups cover the 50 most recent conversations per source, up to 30 days old.
4. Fills in a fixed reply template in Go and attaches up to 8 images. Retrieved chat content is never sent to a model, so text inside the owner's chats cannot steer the service.

The same readers are on the CLI: `tincan history <chatgpt|claude-ai|codex|claude-code>` with `--latest`, `--list N`, `--search`, `--id`, `--all`, `--json` and `--images-dir`.

#### One-time setup

Install the Tincan Chrome extension (see [below](#the-tincan-chrome-extension)), make sure `codex` is installed and logged in, then:

```bash
tincan history install --extension-dir ~/tincan-extension                          # the folder you loaded unpacked
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.agenttincan.history.plist   # macOS
systemctl --user daemon-reload && systemctl --user enable --now tincan-history.service  # Linux
```

`tincan history install` writes the Chrome native messaging host manifest and the service definition (a launchd agent on macOS logging to `~/Library/Logs/tincan-history.log`, or `~/.config/systemd/user/tincan-history.service` on Linux) and prints the start command. It starts nothing. Pass `--extension-dir <the folder you loaded>` (or run it from a repo checkout, where it finds `extension/`) so the native host can reload the unpacked extension when its files change. `--no-service` skips the service definition.

The service does not get your login shell's PATH. Its PATH starts with the directories where `codex` and `claude` were found when you ran `tincan history install`, then `~/.local/bin`, `~/.npm-global/bin` and `~/bin`, then the system directories. If you install or move `codex` later, run `tincan history install` again.

On a headless Linux box, run `loginctl enable-linger $USER` once so the user service keeps running after you log out.

#### Limits and gotchas

- It is the most sensitive agent on the mesh: by default every joined agent can read the owner's chat history. Write `~/.config/tincan/history-allow.txt` to narrow that to the agents you trust with it. The allowlist governs requests to the history agent, not local shell access; an agent with a shell on the owner's machine (such as the Codex wake) can read local Codex and Claude Code history directly.
- Live sources need Chrome running, the extension connected, and the owner logged in; otherwise the reply says the source is unavailable and local sources still work. Chrome is never quit or restarted.
- A query the step cannot place gets "Please ask a clearer question naming ChatGPT, claude.ai, Codex or Claude Code".

#### Adapter doc

[docs/adapters/history.md](docs/adapters/history.md)

### The ChatGPT and Claude web agents (chatgpt-web and claude-web)

#### What it is

A Go service, `tincan web serve --site chatgpt` (or `--site claude-ai`), that makes chatgpt.com or claude.ai a teammate. `tincan ask chatgpt-web "..."` comes back with ChatGPT's answer and any images it generated attached. It types into the owner's logged-in account, as the owner, so the chats show in the owner's ChatGPT or Claude history, count against the owner's plan, and follow the site's own memory, custom instructions and model choice.

#### How it joins

```bash
tincan invite chatgpt-web --kind chatgpt-web                                        # admin device
TINCAN_CONFIG=~/.config/tincan/chatgpt-web.json tincan join <code> --relay http://tincan-relay
```

For Claude use `claude-web`, `--kind claude-web` and `~/.config/tincan/claude-web.json`. Like history, it refuses to start unless the relay confirms its name.

#### How it wakes

Wait: the service long-polls. Set its method to `wait` in `wake.json`.

#### How it sends and receives

For each request, one at a time:

1. Checks the allowlist exactly like history: with no file, every joined agent may ask; `~/.config/tincan/chatgpt-web-allow.txt` or `claude-web-allow.txt` restricts it to the listed names, and every agent in the chain must be listed.
2. Reads the optional threading line. A first line `new chat` starts a new conversation; `conversation: <id>` (or a conversation URL) continues that one; otherwise it continues the conversation this asker used last with this agent. Each asker has its own thread. The ids live in `~/.config/tincan/<agent>-state.json` (0600, ids only). Every reply ends with the conversation id so the asker can come back.
3. Has the extension type the message into a background tab the extension opens itself (`active: false`). The extension fills the message box, clicks send, and returns once the conversation id is in the tab's address (at most 60 seconds). It never touches a tab the owner opened.
4. Decides completion from the conversation data, not the page: it reads the conversation through the same detail operation the history agent uses (first 5 seconds after the send, then 5, 8 and 12 seconds apart, then every 20 seconds), finds this request's own user message, and waits for the answer after it (ChatGPT: any message in the turn marked end of turn, which covers image turns whose last message is hidden; claude.ai: a `stop_reason`, or the same text on 4 reads spanning at least 10 seconds). The wait is bounded by the 8 minute request timeout. An HTTP 429 waits the site's `Retry-After` or backs off from 30 seconds up to 5 minutes, and a cooldown makes the next requests fail at once with "ChatGPT is rate-limiting this account right now; try again later" instead of hitting the site again. A rate limit that ends the wait after the send says the message was sent, names the conversation, and asks for the reply later instead of sending again. While it waits, the agent keeps its relay presence fresh without claiming new requests.
5. Replies with the answer text (up to 64 KB) and the generated images (up to 8) as attachments, then has the extension close the tab. A tab nobody closes is closed after 10 minutes.

Send journal: right after a send is confirmed, the agent records the request id, conversation id and send time in `~/.config/tincan/<agent>-journal.json` (0600, no message text). If the relay requeues the request after its 30 minute claim lease, the agent reads the answer from the journaled conversation instead of sending again. Entries are dropped after 90 minutes.

The web agents send the request text only; messages over 32 KB are refused, not cut.

#### One-time setup

```bash
tincan history install --no-service      # skip if history is already installed
tincan web install --site chatgpt
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.agenttincan.web.chatgpt.plist
```

For Claude: `tincan web install --site claude-ai` and `com.agenttincan.web.claude-ai.plist`. `tincan web install` writes the launchd agent (or a systemd user unit on Linux) and prints the start command; it starts nothing. Logs go to `~/Library/Logs/tincan-chatgpt-web.log` (or `tincan-claude-web.log`). On a headless Linux box, run `loginctl enable-linger $USER` once so the user service keeps running after you log out.

#### Limits and gotchas

- It acts as the owner, and by default any joined agent may ask it to. Write its allowlist file to restrict that. The answer can include what the site remembers about the owner, and it is untrusted model output: the web agents pass it back as is.
- The extension checks the site session before opening a tab, so a logged-out browser never sends anonymously.
- Both sites protect their send endpoints with anti-bot tokens only the real page can produce, which is why it drives a tab instead of calling an API. If a site changes its page, the selectors in `extension/send.js` need an update.
- The send operations need extension version 0.3.0 or later.

#### Adapter doc

[docs/adapters/web-agents.md](docs/adapters/web-agents.md)

## The Tincan Chrome extension

A Manifest V3 extension (in [extension/](extension/), named "Agent Tincan History" in its manifest) that lets the history agent read, and the web agents send to, ChatGPT and claude.ai through the owner's own logged-in Chrome.

What it can do:

- Run a fixed set of operations for its native host (`tincan history native-host`): list, detail and file reads for chatgpt.com and claude.ai, `chatgpt.send` and `claudeai.send`, `chatgpt.close` and `claudeai.close`, and `extension.reload`. Images come back as base64 in chunks of at most 384 KiB.
- Open, fill and close its own background tabs for sends.

What it cannot do:

- It accepts nothing outside that operation set and never runs code from a message or a page. A message is passed as data to a fixed function in an isolated content script and inserted as text.
- No cookie or token leaves the browser. The ChatGPT access token is read inside the extension's worker and stays there.
- It never scripts a tab the owner opened, and Chrome is never quit or restarted.
- Its permissions are limited to `nativeMessaging`, `alarms` and `scripting`, on chatgpt.com, `*.oaiusercontent.com` and claude.ai.

Install: once the Chrome Web Store listing is published, installing is one click through Chrome's standard permission dialog. Until the store listing is live, load it unpacked once:

1. Download `tincan-history-extension.zip` from the release page and unzip it into a folder you will keep, for example `~/tincan-extension` (Chrome loads it from there every time, so do not delete it). From a repo checkout, the `extension/` folder works the same way.
2. Open `chrome://extensions`, turn on Developer mode (top right), click Load unpacked, and pick that folder.
3. Run `tincan history install --extension-dir ~/tincan-extension` so Chrome can start the native host and the host can reload the extension when its files change.

The manifest carries a public key, so an unpacked load always gets the id `ciejooalclcpgpapboofdbbddphldhnh`; a store listing with its own id is passed with `tincan history install --extension-id <id>`.

Self-reload: after the first load, updates need no Reload click. When the extension connects, it sends the native host its version and the sha256 of each file (hashed when its worker started). If `tincan history install` was run with `--extension-dir` (or from a repo checkout) and the files on disk differ, the host sends `extension.reload` and the extension calls `chrome.runtime.reload()`. The host checks again every 10 minutes while the extension stays connected. It waits while a send has a tab open, checking every 5 seconds for up to 5 minutes. The host asks at most once per 10 minutes for the same files when the extension connects, and the re-check never asks again for files it already asked about. A store install is never reloaded this way.

Why an extension is required: ChatGPT and claude.ai offer no official API for reading your own chat history, so the reads go through the sites' own endpoints with your existing browser session, and sends need the real page. An extension is the one way to do that inside your logged-in Chrome without exporting cookies or tokens. Chrome requires a person to click to install any extension, so that click is the one human step in the setup.

## Onboarding

Once agents are joined, `tincan onboard` reads the live roster and writes the setup kit: a standing prompt for the Agent Tincan operator role, and for every agent its join line, the text to paste into its standing instructions, and its setup steps. It also prints add-agent recipes for every kind, a second-agent-on-one-machine recipe, and a relay-hosting recipe.

```bash
tincan onboard --operator grokbot
```

- `--operator <agent>` names the always-on agent that runs the operator prompt.
- `--section operator|agents|recipes` prints one part; `--json` prints the kit as structured data (the shape the `onboard` MCP tool returns).
- `--offline` skips the roster and makes no network call, so you can print the operator prompt and recipes before anyone has joined.
- `--owner` names the person the prompts refer to; `--kind name=kind` tailors one agent's block.
- It is read-only: it never mints invite codes or joins or removes agents. Its output never contains wake secrets. Re-run it after any roster or wake change and paste the fresh text over the old.

Kinds: `vm-webhook`, `e2b-email`, `proxy-sandbox`, `claude-code`, `chatgpt`, `hermes`, `openclaw`, `codex`, `history`, `chatgpt-web`, `claude-web`, `generic`. History and the web agents are services, so their blocks carry setup only, no standing instructions. The generic shape of an agent's instructions is in [docs/adapters/agent-instructions.md](docs/adapters/agent-instructions.md).

The operator prompt follows a quiet rule: the operator speaks only when the owner asks it something or when it is answering an agent. Its 30 minute standing check never messages the owner; findings wait until the owner asks.

Several agents per machine: each agent on one machine gets its own invite and its own client config through `TINCAN_CONFIG`, for example Claude Code and Codex on the same Mac, or Hermes and OpenClaw on the same mini. Set that `TINCAN_CONFIG` on every tincan command the agent runs (join, MCP entry, listen, rejoin). `tincan join` refuses to overwrite a config that already names a different agent unless you pass `--replace`.

Self-healing rejoin: if an agent's machine is rebuilt with the same machine name (or that name with a `-1` style suffix), run `tincan rejoin --relay http://tincan-relay` on the new machine. The relay re-admits it as the old agent, with its queued requests still waiting, when the new node is untagged, owned by the same Tailscale login, and the old node is offline or gone. Add `--proxy` for a proxy sandbox and `--name <agent>` when the machine ran several agents. Every standing instruction tells the agent to run rejoin itself and never ask for an invite unless rejoin says the machine was never joined. Tagged machines need a new invite, and `tincan relay --no-auto-rebind` turns this off. Each re-admission is audited as a `rebind` event.

## Trust model

Joined agents trust each other fully: a request from a joined agent is acted on as if you asked, with no per-request approval. Tailscale is the security boundary, the relay can read every request and reply, and only admin devices can invite, remove or connect agents. The real risk is an agent that reads untrusted content being tricked into asking a powerful teammate to do something harmful. Give high-power agents instructions about what to confirm with you, keep `tincan trace` handy, and use `tincan remove` to cut an agent off.

Read [docs/trust-model.md](docs/trust-model.md) before joining an agent that reads untrusted content alongside one that holds powers like spending money. It also covers attachments, the history agent, and the web agents.

## Build, test, release

```bash
make build            # static ./tincan, CGO_ENABLED=0
make test             # go test -race ./...
make vet              # go vet ./...
make extension-test   # node --test for the extension worker code (no dependencies)
make extension        # dist/tincan-history-extension.zip
make dist             # every release asset in dist/ (see below)
```

`make build` builds one binary, for the machine you run it on. CI runs `go vet` and `go test -race` on Linux and macOS, and checks the static builds for linux/amd64, linux/arm64 and darwin/arm64.

Releases are on the GitHub repo's release page. Each carries `tincan_darwin_arm64`, `tincan_linux_amd64`, `tincan_linux_arm64`, `checksums.txt` (sha256 of the three binaries) and `tincan-history-extension.zip`. The binaries and `checksums.txt` are what the relay's `--dist` directory takes (add a `VERSION` file).

Releases are cut by hand; CI does not publish them. From a clean checkout of the commit to release:

```bash
git tag v0.5.0 && git push origin v0.5.0
make dist        # static binaries for the three targets, checksums.txt and the extension zip, in dist/
gh release create v0.5.0 --prerelease --title v0.5.0 dist/tincan_* dist/checksums.txt dist/tincan-history-extension.zip
```

`make dist` stamps the version from `git describe`, so tag first.

Quick start: [docs/quickstart.md](docs/quickstart.md). Protocol: [docs/protocol.md](docs/protocol.md).

MIT licensed.
