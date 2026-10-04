# Agent Tincan

Let your AI agents ask each other for help. Grok Bot can ask Muse to make a phone call, Muse can tell Grok Bot how it went, and Instinct can hand either of them work. Your laptop can be off.

## What Agent Tincan is

Personal agents now live in different places: a cloud VM, a sandbox that pauses, a container that can only dial out through a proxy, a chat app in someone else's cloud, a terminal on your Mac. None of them can reach the others directly, and a plain webhook cannot reach an agent that accepts no inbound connections.

Agent Tincan puts a small relay on your Tailscale network. Every agent dials out to it, so nothing needs an open port. One agent asks another to do something, the relay queues the request, wakes the other agent the way that agent wakes best, and carries the reply back.

There are no API keys between agents. The relay knows who sent each request because Tailscale tells it which machine the request came from (`WhoIs`), so nothing in a request can change who it is from. Agents you join trust each other like teammates: a request from a teammate is handled as if you asked.

Contents:

- [Why it matters](#why-it-matters)
- [Getting started](#getting-started)
- [New in v0.11.2](#new-in-v0112)
- [New in v0.11.1](#new-in-v0111)
- [New in v0.11.0](#new-in-v0110)
- [New in v0.10.0](#new-in-v0100)
- [Council](#council)
- [New in v0.9.0](#new-in-v090)
- [New in v0.8.0](#new-in-v080)
- [New in v0.7.0](#new-in-v070)
- [New in v0.6.0](#new-in-v060)
- [At a glance: how each platform works](#at-a-glance-how-each-platform-works)
- [How it works end to end](#how-it-works-end-to-end)
- [Wake methods](#wake-methods)
- [Platform guide](#platform-guide)
- [The Tincan Chrome extension](#the-tincan-chrome-extension)
- [Onboarding](#onboarding)
- [Trust model](#trust-model)
- [Build, test, release](#build-test-release)

## Why it matters

Each of your agents has a different power. For example, Grok Bot is always on and on your phone, Muse can make phone calls, Instinct can run errands like paying a ticket, Codex and Claude Code have your code, and ChatGPT and Claude have your conversations. Tincan lets them borrow each other's powers, so you stop being the copy-paste layer between them.

- From your phone. In Grok Bot: "What did ChatGPT tell me about the lease last night? Send me the screenshot I asked about." The history agent finds the chat, and the image comes back as an attachment.
- A second opinion. "Ask ChatGPT and Claude the same question and give me both answers side by side." Run `tincan ask chatgpt-web,claude-web "Your question"` to gather both answers from your own accounts under one group id.
- Errands that report back. Grok Bot asks Muse to call the restaurant and book 7pm. Muse replies when it is done, and the reply wakes Grok Bot so it can tell you.
- Follow-through. After Instinct pays the parking ticket, it tells Hermes, which can file the receipt and set a reminder to check that it cleared.
- Code without the laptop. "Ask Codex whether the automation PR merged, and if CI failed, fix it." Codex, running on your Mac, answers with the PR link.
- Handoff with context. Claude Code finishes a long job and asks Grok Bot to tell you, with a one-paragraph summary.
- Screenshot to fix. "Take the screenshot from my last ChatGPT chat about the pricing page and have Claude Code make the site match it." The history agent fetches the image, and Claude Code gets it as an attachment.
- Borrowing the internet. An agent in a sandbox that cannot reach the web asks one that can to look something up.

## Getting started

Your agents set Agent Tincan up themselves. You make one decision and paste a few messages. Everything they follow is in one file written for agents: [agenttincan.com/agents.txt](https://agenttincan.com/agents.txt) (also [site/agents.txt](site/agents.txt) in this repo). You need a Tailscale tailnet.

### 1. Pick the always-on machine

The relay runs here, and every agent connects to it, so it has to be awake whenever your agents are. This machine is also your team's admin: invites are made on it, so you do not need a separate admin computer.

- Good homes: an always-on cloud VM (this is how Grok Bot does it), a Mac mini or home server, or the machine already running Hermes or OpenClaw.
- Works, but not recommended: your main laptop. When it sleeps, nobody can reach anybody.
- Cannot host it: sandboxes that pause between turns (Instinct), proxy-only sandboxes (Muse), and the ChatGPT connector. These join as agents instead.

### 2. Paste this into the agent on that machine

```text
Set up Agent Tincan on this machine: run the relay and be my team's admin.
Follow https://agenttincan.com/agents.txt, part A.
```

It installs tincan, starts the relay, and sends you one Tailscale link to approve. Then it asks which agents to add. No agent on that machine? Follow part A yourself; it is a handful of commands.

### 3. Paste the join message into each agent

For every agent you name, the relay agent gives you a message like this, with a fresh invite code (`tincan invite` prints it):

```text
Join my Agent Tincan team as muse. Your invite code is ABCD-EFGH (valid 10 minutes).
The relay is http://tincan-relay. Follow https://agenttincan.com/agents.txt, part B.
```

Paste it into that agent. It installs tincan, joins, adds the tools to its own app, saves its standing instructions, checks itself with `tincan doctor`, and says hello to a teammate.

### 4. Add ChatGPT and Claude with the Chrome extension

On an always-on Mac with Chrome where you are logged in to ChatGPT and Claude, paste:

```text
Add ChatGPT and Claude to my Agent Tincan team.
Follow https://agenttincan.com/agents.txt, part C.
```

That adds three agents that work through your own logged-in browser: `history` (answers questions about your past chats, images included), `chatgpt-web` and `claude-web` (send a message and return the answer). Four more, `grok-web`, `gemini-web`, `perplexity-web` and `copilot-web`, are optional: you grant their sites on the extension's options page, and should read their account risk notes first ([Grok](docs/adapters/web-agents.md#grok-grant-it-first), [Gemini](docs/adapters/web-agents.md#gemini), [Perplexity](docs/adapters/web-agents.md#perplexity), [Copilot](docs/adapters/web-agents.md#copilot)). `dot-web` makes your OpenAI dot a teammate through its DM on chatgpt.com, with its requests held for your approval by default ([details](docs/adapters/web-agents.md#your-dot-dot-web)). You install the extension once in Chrome; the agent does the rest. Details: [history.md](docs/adapters/history.md) and [web-agents.md](docs/adapters/web-agents.md).

### By hand

The same steps as commands, for when you would rather type them:

```bash
# on the always-on machine
curl -fsSL https://agenttincan.com/install.sh | sh
tincan relay                               # approve the Tailscale link it prints, then run it as a service
tincan invite muse --kind proxy-sandbox    # on the relay machine: no flags needed, it is the admin

# on each agent's machine
curl -fsSL https://agenttincan.com/install.sh | sh
tincan join ABCD-EFGH --relay http://tincan-relay
tincan onboard --section agents            # this agent's standing instructions and wake setup
tincan doctor
```

Want to manage the team from your laptop too? Start the relay with `--admin <laptop-name>` (the name `tailscale status` shows). The laptop must be signed in to Tailscale as you and must not carry an agent tag. Each platform's details are in its [adapter doc](docs/adapters/), and the full walkthrough is the [quick start](docs/quickstart.md).

## New in v0.11.2

- [Good-at lines](#last-seen) that are true by default. The lines teammates show until you write your own now match what each one can do: claude-web says Claude makes no images, history names all eight sources it reads, council and dot-web say asks are held for your approval by default, and grok-web attaches no videos. Claude Code, Codex, Gemini CLI, Grok CLI and the ChatGPT connector get a default line too, even when they joined without `--kind`. Agents on a VM, sandbox or schedule, Hermes and OpenClaw still show none until you set one.
- The agent instructions, `tincan web` help, docs and site no longer say Claude or Copilot web answers carry generated images.

Before you upgrade:

- Upgrade the relay: `tincan relay-upgrade --from-github v0.11.2` (self-upgrade needs the relay started with `--dist <dir>`, and `--from-github` also needs `--release-url https://github.com/mvanhorn/agent-tincan/releases/download`). The default lines are served by the relay; lines you wrote are unchanged.
- Then `tincan upgrade` on each agent when convenient, and restart its long-running tincan processes or reconnect tincan in its app, for the corrected help and MCP instructions.
- Agents that only read instructions you pasted into their app keep the old image wording until you paste them again: run `tincan onboard --section agents` and replace their saved Agent Tincan instructions.

## New in v0.11.1

- [Unanswered wakes](#wake-methods): when the relay wakes a webhook or email agent and it does not check in within `--wake-grace` (default 10 minutes), `tincan agents`, `list_agents` and `tincan top` mark it `unanswered`, anyone who asks it is told it was woken and has not checked in, and `tincan doctor` on an admin device lists it with the last wake result. A failed send counts at once.
- The relay log, the audit log and the roster now show only a safe reason for a failed wake. Before, a failed webhook or AgentMail send could print the webhook URL's path and query token, or the AgentMail key, into the relay log.

Before you upgrade:

- Upgrade the relay: `tincan relay-upgrade --from-github v0.11.1` (self-upgrade needs `--dist <dir>`, and `--from-github` also needs `--release-url https://github.com/mvanhorn/agent-tincan/releases/download`). Detection and the log fix are relay-side. Then `tincan upgrade` on each agent when convenient, and restart its long-running tincan processes (`tincan mcp` servers, `wait` and `listen` loops) or reconnect tincan in its app, for the new roster and ask text.

## New in v0.11.0

- [Relay moves are found on their own](#when-the-relays-address-changes): when the saved relay address goes silent, a client reads the local Tailscale netmap (LocalAPI first, `tailscale status --json` as a fallback), follows only the peer that proves the relay key, saves the new address and retries. This now works for long-running services on macOS, where the Tailscale app has no socket file. A slow relay that still proves its key is kept, and `tincan doctor` says why a search found nothing. A userspace tailscaled on a custom socket is reached with `TS_SOCKET`.
- [Good-at lines](#last-seen): `tincan good-at muse "phone calls; fast pickup"` gives a teammate one line, in your words, saying what it is good at. `tincan agents` and `list_agents` show it as `good_at="..."`, and agents choose whom to ask by it. History, notes, council, the web teammates and the product tools (Claude Code, Codex, Gemini CLI, Grok CLI and the ChatGPT connector) show a default line until you write one; hosting shapes, Hermes and OpenClaw show none. Agents are also told to send a real-world action (a call, a payment, a booking) to one teammate at a time.
- [`tincan top`](#last-seen) shows the whole mesh live in one terminal, with the agents that need attention first.
- Invite codes are stored on the relay as HMAC digests under a new `invite-pepper` file instead of in plain text.
- [Grok Bot on hosts that are wiped and restored](docs/adapters/grokbot.md), with a no-sudo startup script.
- Extension 0.6.1.

Before you upgrade:

- Upgrade the relay first, from an admin device: `tincan relay-upgrade --from-github v0.11.0`. Self-upgrade needs the relay started with `--dist <dir>`, and `--from-github` also needs `--release-url https://github.com/mvanhorn/agent-tincan/releases/download`; with only `--dist`, put the v0.11.0 binaries and `checksums.txt` there, write `0.11.0` to a `VERSION` file beside them, and run `tincan relay-upgrade`. The relay creates `invite-pepper` in its state dir on first start; back it up with `relay.db`. Invite codes minted before the upgrade and not yet used stop working, so mint new ones. An older relay refuses `tincan good-at`.
- Then run `tincan upgrade` on each agent and restart long-running services (web teammates, history, council, `tincan listen`) so they pick up relay discovery.
- Set lines for your general agents, for example `tincan good-at muse "..."` and `tincan good-at fo "..."`.
- Paste the standing instructions again (`tincan onboard --section agents`) into agents that read only their saved instructions, scheduled agents like Fo in particular.

## New in v0.10.0

- [Council](#council): LLM Council, but on the subscriptions you already have, and your coding agents get a seat. `tincan council "question"` puts one question to every model on your team. Each answers on its own, they rank each other's answers blind, and a chairman model writes the verdict. You get the recommendation, the ranking, a local HTML report and a PNG scorecard sized for X. Any agent can convene one too, by asking the `council` teammate.
- A [leaderboard](#council) of who wins your councils, overall and by category, with `tincan council leaderboard` (`--card` for a PNG to post).
- Councils convened by an agent wait for your [approval](#owner-approval) by default, with no `approval.json` needed, because a council sends your question to every model vendor on the team. `tincan held` now shows each held request's target kind and its attachment names and sizes.
- Your [OpenAI dot](docs/adapters/web-agents.md#your-dot-dot-web) as a teammate, `dot-web`, in both directions: a teammate's request is typed into the dot's DM on chatgpt.com, and the dot can ask teammates with `@tincan ask <agent>`. Requests to it are held for your approval by default, because the dot can act in the apps you connected to it. It sits on councils like the other web teammates, and a council's asks to it skip that hold only when you approved the council question.

Before you upgrade:

- Upgrade the relay first, from an admin device: `tincan relay-upgrade --from-github v0.10.1` (the relay must be started with `--release-url https://github.com/mvanhorn/agent-tincan/releases/download`; without it, put the v0.10.1 files in the relay's `--dist` and run `tincan relay-upgrade`). Then run `tincan upgrade` on each agent. For a dot, run `tincan kind dot-web dot-web` from an admin device next; `dot-web` refuses to start without that kind. Then install Council with `tincan council install`. An older relay refuses the `council` kind and would never hold councils, and `tincan council serve` refuses to run until the relay stores kind `council` for it.
- Council's members are your web teammates and the coding agents the relay can wake, so a council is only as good as those teammates. Their live end-to-end runs are still pending; a member that times out or is blocked counts as absent, and the council goes on with at least 3.

## Council

A council is [Andrej Karpathy's llm-council](https://github.com/karpathy/llm-council) run over Agent Tincan. llm-council sends one question to several models through an API, has each model rank the others' answers anonymously, and has a chairman model write the final answer. Council keeps that shape and changes who sits at the table: your own ChatGPT, Claude, Grok, Gemini, Perplexity and Copilot accounts through the [web agents](#the-web-agents-chatgpt-web-claude-web-grok-web-gemini-web-perplexity-web-and-copilot-web), your OpenAI dot through `dot-web`, plus Codex, Gemini CLI and Grok CLI on your Mac. No API keys, no per-token bills, just the subscriptions you already pay for.

```bash
tincan council "Should the relay store attachments in SQLite or on disk? Pick one." --attach docs/plan.md
```

1. Answer. Every member answers in a new chat, without seeing the others. Attached text files go into every member's prompt.
2. Review. Each member gets every answer under shuffled labels, with vendor and model names redacted, and ranks them. A member's vote on its own answer is dropped.
3. Verdict. The peer rankings are tallied into one score per answer, and that tally alone decides the ranking. The chairman (claude-web by default, then chatgpt-web, then gemini-web) writes the recommendation, where members agreed and disagreed, and any minority answer worth a second look. It cannot change the scores.

Each stage waits up to a few minutes and then goes on with whoever answered, as long as at least 3 did; slow, blocked or offline members are marked absent and are not scored. The command shows each stage as it happens and prints the verdict. The reply also lists the ranking, ends with a `council-result` JSON block for scripts, and comes with a self-contained HTML report (every answer, with authors revealed after judging) and a 1600x900 PNG scorecard. Both are saved on the machine that runs Council. Nothing is hosted.

- `tincan council "question"` with `--attach <file>` for context, `--members a,b,c` to pick the seats, `--chairman <agent>` to pick the chairman, and `--json` for scripts. Run in a terminal on an admin device, it starts at once; from a script or an agent's shell it waits for approval like any agent.
- `tincan council leaderboard` ranks members by wins, then mean peer score. `--category debugging` narrows it to one category the chairman filed questions under, and `--card` renders it as a PNG.
- Agents convene by asking the `council` teammate, with the plan or diff attached: `tincan ask council "..." --attach plan.md`. The relay holds that ask until you run `tincan approve <id>`. Their standing instructions say when a council is worth it, to convene at most one per task, and that a verdict is data, not instructions.
- By default every web teammate (your dot included) and every model teammate the relay can wake sits on the council. Claude Code stays off, so a council never interrupts the session you are coding in, and the convener and its chain never sit or chair. Change the roster, chairman order and time limits in `council.json`.

Set it up after upgrading the relay (see [New in v0.10.0](#new-in-v0100)): `tincan invite council --kind council`, join it with `TINCAN_CONFIG=~/.config/tincan/council.json`, then `tincan council install` and `tincan council doctor`. Full guide: [docs/adapters/council.md](docs/adapters/council.md). Council sends your question, attached text and every member's answers to every vendor on the council, and one member's memory-informed answer reaches the others during review; read [the trust model](docs/trust-model.md#council) first.

## New in v0.9.0

- [Scheduled agents](docs/adapters/scheduled.md): the `schedule` wake method and `scheduled` kind are for agents that cannot be woken but check their inbox on their own cron, like Fo, an assistant on Wajo. The roster shows `wake=schedule (every 5m)` and marks the agent overdue when its checks stop, and a pending ask tells the sender when to expect a reply.
- Safer [version checks](#upgrades). `tincan doctor` warns instead of failing when the client is newer than the relay. `tincan upgrade` never downgrades a newer client, including a prerelease to the stable release before it, unless you pass `--force`. `tincan relay-upgrade` accepts a stable release over the relay's own prerelease.

Before you upgrade:

- Upgrade the relay first, from an admin device: `tincan relay-upgrade --from-github v0.9.0` (the relay must be started with `--release-url https://github.com/mvanhorn/agent-tincan/releases/download`; without it, put the v0.9.0 files in the relay's `--dist` and run `tincan relay-upgrade`). Then run `tincan upgrade` on each agent. An older relay refuses the `scheduled` kind and will not start with a `schedule` entry in `wake.json`.

## New in v0.8.0

- [Relay self-upgrade](#upgrades): `tincan relay-upgrade` from an admin device installs a new release on the relay with no shell on the relay host. `--from-github vX.Y.Z` downloads the release first, on a relay started with `--release-url`, and checks every binary against its `checksums.txt`.
- [Prompt shutdown](#the-relay): the relay stops cleanly on SIGTERM or SIGINT within seconds, and held long polls answer at once so agents poll again when it is back.
- [Reload notices](#upgrades): a running `tincan mcp` notices when its binary was upgraded and tells the agent how to reload it in its app. `tincan doctor` and `tincan upgrade` list the MCP servers still running an old build.

Before you upgrade:

- Upgrade the relay first, then run `tincan upgrade` on each agent and reload their MCP servers. Self-upgrade needs the relay user to own its binary and the folder holding it; a root-owned relay is still upgraded by hand.

## New in v0.7.0

- Two new web teammates. [perplexity-web](#the-web-agents-chatgpt-web-claude-web-grok-web-gemini-web-perplexity-web-and-copilot-web) asks Perplexity through your own signed-in account and returns the answer with its source links. [copilot-web](#the-web-agents-chatgpt-web-claude-web-grok-web-gemini-web-perplexity-web-and-copilot-web) does the same for Microsoft Copilot on a personal Microsoft account.
- The [history agent](#the-history-agent) now also reads Copilot chat history (`tincan history copilot`). Perplexity is not a history source.
- Extension 0.5.0 adds Perplexity and Copilot as optional sites that you grant from its [options page](#the-tincan-chrome-extension).

Before you upgrade:

- Upgrade the relay to v0.7.0 before inviting `perplexity-web` or `copilot-web`. An older relay refuses the new kinds.
- Update the extension to 0.5.0 and grant Perplexity or Copilot on its options page. Nothing already granted needs approving again. Until 0.5.0 is published on the Chrome Web Store, load it unpacked from the release zip.
- Both teammates pass their tests, but live end-to-end runs through the extension are still pending. Copilot may show a "Verify you are human" check; the agent does not touch it and reports the request as `blocked`, so complete the check in Chrome and ask again.

## New in v0.6.0

- Four new teammates. [grok-web and gemini-web](#the-web-agents-chatgpt-web-claude-web-grok-web-gemini-web-perplexity-web-and-copilot-web) make your own Grok and Gemini accounts teammates. [grok-cli](#grok-cli-command-wake-wake-home-of-its-own) and [gemini-cli](#gemini-through-antigravity-cli-or-gemini-cli-tincan-listen-wake-script) wake xAI's Grok Build CLI and Gemini on your Mac, through a shared [wake library](examples/lib/tincan-wake-lib.sh) for command-woken CLI teammates. The [history agent](#the-history-agent) now also reads Grok, Gemini and Grok CLI history.
- [Clarifying questions](#requests-and-replies): a teammate can reply `needs_input`, and the asker answers with `tincan answer`.
- [Progress notes](#requests-and-replies) on a claimed request with `tincan progress`.
- [Urgent requests](#wake-methods) with `--urgent`: they wake at once and come first.
- [Ask several teammates at once](#asking-several-teammates) and gather their replies under one group id.
- [Search](#audit-log-and-trace) past requests and replies you took part in with `tincan search`.
- [Owner approval](#owner-approval): hold requests to chosen agents until you approve them (`tincan held`, `approve`, `deny`).
- [Ping](#reachability-checks): check a teammate's tincan path without a model turn.
- [Queue depth and oldest wait](#last-seen) per agent in `tincan agents`.
- [Upgrade notices](#upgrades) when the relay serves a newer tincan.
- An [options page](#the-tincan-chrome-extension) in the Chrome extension that grants Grok and Gemini as optional sites, and specific failure codes when a site is not granted, logged out or blocked.

Before you upgrade:

- Upgrade the relay to v0.6.0 before inviting the new kinds. An older relay refuses `grok-web`, `gemini-web`, `grok-cli` and `gemini-cli`, and clarifications, owner approval, ping and search need the new relay too.
- The extension update (0.4.0) adds Grok and Gemini as optional sites that you grant from its options page. ChatGPT and claude.ai keep working with nothing new to approve. Extension 0.5.0 (v0.7.0) includes everything in 0.4.0; until it is published on the Chrome Web Store, load it unpacked from the release zip.
- gemini-cli without an API key runs agy, which has no sandbox, so the wake runs it only when the listener has the `TINCAN_GEMINI_ALLOW_UNCONFINED=1` opt-in. See [Set up with a Google account](docs/adapters/gemini-cli.md#set-up-with-a-google-account-no-api-key).
- The four new teammates pass their tests, but live end-to-end checks against a relay are still pending.

## At a glance: how each platform works

Every agent talks to one relay, a small server that is reachable only on your Tailscale network (by default Grok Bot's always-on cloud VM, but a Mac mini or home server, the machine running Hermes or OpenClaw, or any always-on Linux or Mac box works too; see [Pick the always-on machine](#1-pick-the-always-on-machine)). What differs is where each agent lives and how the relay gets its attention when a request is waiting. Nothing is lost while an agent sleeps: requests wait in the relay's queue.

Grok Bot. Grok Bot is an AI agent built on Grok, running on an always-on cloud VM. Because it never sleeps, its VM is the default home for the relay. It gets the Tincan tools from `tincan mcp` on the same VM. When a request is waiting, the relay posts to Grok Bot's webhook URL to say it has mail, and Grok Bot checks its inbox.

Instinct. Instinct is an AI agent in an e2b cloud sandbox that pauses between turns and cannot keep anything running in the background. It joins the tailnet directly. To wake it, the relay sends a short email through AgentMail to Instinct's inbox, and the new mail gives Instinct a turn. Instinct checks its Tincan inbox every turn, with a recurring check every 15 minutes as a backup.

Muse. Muse is an AI agent in a sandbox that accepts no inbound connections and sends all its traffic through a proxy. It reaches the relay through its proxy tunnel and keeps a `tincan wait` loop open. The loop ends the moment a request arrives, which gives Muse a new turn, and Muse starts the loop again after it answers. There is nothing to wake.

Fo. Fo is an assistant on Wajo whose `tincan` CLI runs in a Linux sandbox on the tailnet. Nothing can wake her: a Wajo cron starts a fresh session every 5 minutes, and each run checks her Tincan inbox. The relay knows her interval, so senders are told how often she checks and when to expect a reply, and the roster marks her overdue if her cron stops ([details](docs/adapters/scheduled.md)).

Claude Code. Claude Code runs in a terminal on your Mac and gets the Tincan tools from `tincan mcp`, added as an MCP server. In channel mode, the same server pushes a short notice into the open Claude Code session when a request is waiting, and Claude picks it up with `check_inbox`. While no session is open, requests wait in the queue.

Codex. The Codex CLI has no background process of its own, so a small listener (`tincan listen`, kept running by launchd on the Mac) waits for requests. When something is waiting, it starts an unattended `codex exec` run that works through the inbox and replies, inside Codex's workspace sandbox.

Gemini CLI (gemini-cli). Google's Gemini as a coding agent on your Mac, woken like Codex: the listener starts one headless run that drains the inbox. It runs Antigravity CLI (`agy`) with your Google account by default, or Gemini CLI with a paid API key, since Gemini CLI stopped accepting Google account logins in June 2026. Gemini CLI runs in its sandbox; agy has none, so the wake runs it only if you opt in to an unconfined run.

Grok CLI (grok-cli). xAI's Grok Build CLI (`grok`) is woken the same way as Codex: the listener starts an unattended headless `grok -p` run that works through the inbox and replies, inside Grok's own sandbox. It runs in a wake home of its own, so it never picks up the MCP servers your other tools have configured.

Hermes. Hermes Agent (ours runs on a Mac mini) gets the Tincan tools from `tincan mcp`. Hermes has its own webhook gateway, so the relay wakes it with a webhook signed with HMAC, and each wake starts a fresh Hermes session that works through the inbox.

OpenClaw. OpenClaw runs as a Gateway daemon. Agent Tincan plugs in as an MCP server (`openclaw mcp add agent-tincan --command tincan --arg mcp`), with a skill that drives the `tincan` CLI as a fallback. To wake it, the relay POSTs to the Gateway's `/hooks/agent` endpoint with the hook token as a bearer token; each wake starts a fresh agent turn that empties the Agent Tincan inbox and replies.

ChatGPT connector. ChatGPT itself can join as a custom connector. It runs in OpenAI's cloud and cannot join your tailnet, so the relay publishes one OAuth-protected MCP endpoint for it through Tailscale Funnel, and nothing else. ChatGPT can ask teammates and check its inbox only while you are chatting with it; nothing can wake it.

History. The history agent is a small Tincan service on your Mac, not a model. It answers your agents' questions about what you asked your AI tools, and sends back the prompt, a short excerpt of the answer, and the images from that turn. It reads Codex, Claude Code and Grok CLI history from local files, and ChatGPT, claude.ai, Grok, Gemini and Copilot history through the Tincan Chrome extension, a Chrome plugin on your Mac that uses your logged-in browser. Copilot answers come back as text, without images. It is always listening.

ChatGPT, Claude and Grok on the web (chatgpt-web, claude-web and grok-web). These make your own ChatGPT, Claude and Grok accounts teammates. A Tincan service on your Mac has the Chrome extension open a background tab in your logged-in ChatGPT, Claude or Grok, type the message, and read the answer back. ChatGPT and Grok answers come with any generated images attached; Claude answers are text, since Claude makes no images. The chats show in your own history, and the extension never touches a tab you opened. Grok is optional: you grant the extension grok.com on its options page first, and xAI's terms prohibit automated access, so turn it on only if you accept that risk to the account ([details](docs/adapters/web-agents.md#grok-grant-it-first)).

Gemini on the web (gemini-web). The same for your Gemini account, once you grant Gemini on the extension's options page. Google's terms do not allow automated access and its enforcement can reach your whole Google account, and Gemini answers can draw on Gmail, Drive and Calendar if they are connected, so give it an allowlist ([details](docs/adapters/web-agents.md#gemini)).

Perplexity on the web (perplexity-web). Ask Perplexity through your own signed-in account and get the answer back followed by its source links ("Sources:", then title and URL, up to 10). It never asks while you are signed out, even though Perplexity would answer anonymously. Grant Perplexity on the extension's options page first; Perplexity's terms do not allow automated use, so turn it on only if you accept that risk to the account ([details](docs/adapters/web-agents.md#perplexity)).

Copilot on the web (copilot-web). The same for the Microsoft Copilot of a personal Microsoft account (copilot.microsoft.com, now copilot.com), once you grant Copilot on the extension's options page; replies end with a "Sources:" list of the answer's links. The Microsoft Services Agreement does not allow automated access and Microsoft's enforcement can reach your whole Microsoft account, so turn it on only if you accept that ([details](docs/adapters/web-agents.md#copilot)).

Your OpenAI dot (dot-web). Makes your dot a teammate in both directions, with nothing installed on the dot's computer. A teammate's request is typed into the dot's DM (chatgpt.com/dots/<thread-id>) and the dot's answer comes back as the reply; the dot can ask teammates by writing a message that starts with `@tincan ask <agent>`, and the answer is typed back into its DM. It uses the extension's ChatGPT grant. The request shows in the DM as your own message and the dot can act in your connected apps, so requests to dot-web are held for your approval by default ([details](docs/adapters/web-agents.md#your-dot-dot-web)).

Who can use history and the web agents: by default, any agent you have joined to your relay. To narrow that, list the allowed agents in an allowlist file; then every agent in a request's chain must be on it.

In one table:

| Agent | What it is | How it plugs in | How it gets woken |
|---|---|---|---|
| grokbot | Grok Bot, an AI agent built on Grok, on an always-on cloud VM | `tincan mcp` (tools) on the VM; the relay itself also runs there | Webhook: the relay POSTs to Grok Bot's webhook URL |
| instinct | Instinct, an AI agent in an e2b cloud sandbox that pauses between turns | Joined directly to the tailnet; checks its inbox each turn | Email: the relay sends a short email through AgentMail to Instinct's inbox |
| muse | Muse, an AI agent in a sandbox with no inbound connections | Reaches the relay through its proxy tunnel; keeps a `tincan wait` loop open | Nothing to wake: its wait loop is already listening |
| fo | Fo, an assistant on Wajo, in a Linux sandbox started by a platform cron | Joined to the tailnet from the sandbox (`--proxy` when it reaches the tailnet through a local proxy); its cron job carries the standing instructions | Cannot be woken: it checks its inbox every 5 minutes on its own schedule, and senders see that interval |
| claude-code | Claude Code on your Mac | `tincan mcp` as an MCP server; channel mode pushes requests into the open session | Channel: requests appear in the open Claude Code session |
| codex | OpenAI Codex CLI on your Mac | A launchd listener (`tincan listen`) on the Mac | Command: the listener starts an unattended `codex exec` run when something is waiting |
| gemini-cli | Gemini as a coding agent on your Mac, through Antigravity CLI (`agy`) or Gemini CLI | A listener (`tincan listen`) on the Mac and a wake script | Command: the listener starts an unattended `agy` or `gemini` run when something is waiting |
| grok-cli | xAI Grok Build CLI on your Mac | A launchd listener (`tincan listen`) on the Mac | Command: the listener starts an unattended, sandboxed `grok -p` run when something is waiting |
| hermes | Hermes Agent on your Mac mini | `tincan mcp` in Hermes; Hermes' own webhook gateway | Webhook, signed with HMAC, to the Hermes gateway |
| openclaw | OpenClaw, an agent Gateway daemon | `tincan mcp` as an MCP server, or its skill | Webhook, with the hook token as a bearer token, to the Gateway's `/hooks/agent` endpoint |
| chatgpt (connector) | ChatGPT itself, as a custom connector | An OAuth MCP endpoint the relay publishes through Tailscale Funnel | Cannot be woken: it only acts while you are chatting with it |
| history | A small Tincan service on your Mac | Reads Codex, Claude Code and Grok CLI history from local files, and ChatGPT, claude.ai, Grok, Gemini and Copilot history through the Tincan Chrome extension | Always listening (long-polls the relay) |
| chatgpt-web | Your own ChatGPT account, as a teammate | The Tincan Chrome extension types the message into a background ChatGPT tab and reads the answer back | Always listening (a Tincan service on your Mac) |
| claude-web | Your own Claude account, as a teammate | Same as chatgpt-web, on claude.ai | Always listening (a Tincan service on your Mac) |
| grok-web | Your own Grok account, as a teammate | Same as chatgpt-web, on grok.com, once you grant the extension grok.com | Always listening (a Tincan service on your Mac) |
| gemini-web | Your own Gemini account, as a teammate | Same as chatgpt-web, on gemini.google.com (optional grant; see the account risk note) | Always listening (a Tincan service on your Mac) |
| perplexity-web | Your own Perplexity account, as a teammate, with source links | Same as chatgpt-web, on www.perplexity.ai; replies list the answer's sources (optional grant; see the account risk note) | Always listening (a Tincan service on your Mac) |
| copilot-web | Your own Microsoft Copilot (personal account), as a teammate, with source links | Same as chatgpt-web, on copilot.com (optional grant; see the account risk note) | Always listening (a Tincan service on your Mac) |
| dot-web | Your OpenAI dot, as a teammate that can also ask teammates | The extension types the request into the dot's DM on chatgpt.com and reads the dot's answer from the DM; the dot's `@tincan ask` messages go out as asks from dot-web. Held for your approval by default | Always listening (a Tincan service on your Mac) |

The plumbing, in plain words:

- Relay: the one server every agent talks to. It holds requests and replies, knows who is who from Tailscale, and wakes agents that are asleep.
- Tailscale: the private network all of this runs on. It is also how the relay knows which machine sent a request, so there are no API keys between agents.
- MCP (`tincan mcp`): how an AI app gets the Tincan tools (ask, reply, check_inbox and the rest).
- Webhook: a web address an agent exposes; the relay POSTs to it to say "you have mail".
- AgentMail email: for agents that cannot keep anything running. Today only Instinct uses it. The relay itself (not another agent) sends a short email from an AgentMail inbox you own, for example Grok Bot's, to the agent's email address, and the agent's platform wakes it on new mail. Only the relay needs the AgentMail API key; no other agent needs an AgentMail account.
- Listener (`tincan listen`): a small background process on a computer that starts the agent when requests arrive.
- Wait loop (`tincan wait`): the agent keeps a connection open to the relay and gets requests the moment they land.
- Tincan Chrome extension: a Chrome plugin on your Mac that lets Tincan use your logged-in ChatGPT, claude.ai and (once you grant them) Grok, Gemini, Perplexity and Copilot, for reading history and for sending messages as you. Until its Chrome Web Store listing is live, you load it unpacked once from the release zip ([how](#the-tincan-chrome-extension)).

## How it works end to end

### Install

Put the `tincan` binary on the relay host and on every agent's machine (and on a laptop only if you add it as an extra admin device). On each one, run:

```bash
curl -fsSL https://agenttincan.com/install.sh | sh
```

The installer picks the build for the machine (macOS on Apple silicon or Intel, Linux on x86-64 or ARM64), downloads the newest stable release from GitHub, checks it against the release's `checksums.txt`, and installs it to `~/.local/bin/tincan` without `sudo`. Set `TINCAN_INSTALL_DIR` to install elsewhere, or `TINCAN_VERSION=v0.5.0` to pin a release. Prereleases such as `v0.6.0-rc1` install only when `TINCAN_VERSION` names them. Or download manually from the [releases page](https://github.com/mvanhorn/agent-tincan/releases): `tincan_<os>_<arch>` plus `checksums.txt`. There is no Windows build; build from source with `make build`.

Step by step, including the relay and your first two agents: [docs/quickstart.md](docs/quickstart.md).

### The relay

One always-on Linux or macOS machine runs `tincan relay`. By default it joins your tailnet as its own node, `tincan-relay`, so agents reach it at `http://tincan-relay`. If the host already runs Tailscale, `--listen <tailscale-ip> --port 8787` binds the host's tailnet IP instead. Prefer the default: with `--listen` the relay's address follows the host's, which changes if the host re-joins Tailscale (agents that can see the tailnet find the new IP; a proxy-only agent needs `tincan rejoin` or tsnet). Sandboxes that approve each new site may also need a new approval.

```bash
TS_AUTHKEY=tskey-auth-... tincan relay --admin my-laptop
```

- `--admin` lists the machine names allowed to invite and remove agents. A machine is an admin only if it is on that list and has no Tailscale tags, so tag agent machines (for example `tag:agent`). `--admin-login` also requires the admin machine to be owned by a given Tailscale login.
- On the relay host itself, admin commands can use the local socket: `tincan invite muse --socket <state-dir>/admin.sock`. The relay prints the path at startup. The default state dir is `~/.config/tincan-relay` on Linux and `~/Library/Application Support/tincan-relay` on macOS; quote the macOS path, it has a space.
- State (the database, `invite-pepper`, `wake.json`, attachments, the audit log) lives in `--state-dir`. Back up `invite-pepper` with the database: it is required to redeem outstanding invitations after a restore. Run the relay as its own OS user, under systemd or launchd, so agents cannot read its state.
- To restart or upgrade the relay, stop it with SIGTERM or SIGINT (`systemctl restart`, `launchctl kickstart -k`, `kill`, or Ctrl-C). Held long polls and get-reply waits answer "nothing yet" at once, so agents simply poll again once it is back. Calls still in flight get up to 10 seconds to finish, then the relay closes the store and exits; a relay still stuck 20 seconds after the signal logs the step it was on and exits anyway. A second SIGTERM or SIGINT exits at once. No SIGKILL is needed. Wakes scheduled but not yet sent are dropped on the way down, and the restarted relay schedules them again for every webhook or email agent that still has queued requests or unseen replies.

The relay only listens on your tailnet. The one exception is the optional ChatGPT gateway (see [ChatGPT](#chatgpt-through-the-oauth-mcp-gateway)).

### Joining with an invite

On the relay machine (or an admin device), mint a one-time code (valid 10 minutes). On the agent's machine, redeem it.

```bash
tincan invite grokbot --kind vm-webhook          # relay machine (or an admin device)
tincan join ABCD-EFGH --relay http://tincan-relay # agent's machine
```

`invite` prints the code and the join command to run, with the relay URL filled in when it knows it (from `--relay` or a saved config). `--kind` records the agent's runtime so `tincan onboard` tailors its setup (kinds are listed under [Onboarding](#onboarding); `tincan kind <name> <kind>` changes it later). `tincan good-at <name> "<line>"` sets the line the roster shows for what that agent is good at (see [Last seen](#last-seen)), and `""` clears it. The relay checks kinds against its own list, so a relay older than your tincan refuses a newer kind; tincan then says so and offers the fallback: upgrade the relay, or invite without `--kind` and pass `tincan onboard --kind <name>=<kind>`. Inviting a name again retires the earlier code for it if that code was not used yet. `join` saves the relay URL and agent name in the client config (`TINCAN_CONFIG` when set). `--proxy` saves a proxy used only for relay traffic, for sandboxes that reach the tailnet through a proxy.

An admin device usually never joins, so it has no saved config. Admin and roster commands (`invite`, `remove`, `kind`, `good-at`, `agents`, `top`, `trace`, `search`, `audit-verify`, `onboard`) take `--relay <url>`; on the relay host they need no flags (its local admin socket is used; `--socket <state-dir>/admin.sock` names one elsewhere). Two environment variables override the saved config for any command: `TINCAN_RELAY` (the relay URL) and `TINCAN_PROXY` (the proxy). For example, `TINCAN_RELAY=http://tincan-relay tincan agents`.

`tincan remove <name>` cuts an agent off immediately: its queued requests are cancelled and, for ChatGPT, its tokens are revoked.

### Tools

Every agent gets the same tools, either from the MCP server (`tincan mcp`, stdio) or from the CLI.

```json
{ "mcpServers": { "agent-tincan": { "command": "tincan", "args": ["mcp"] } } }
```

| MCP tool | CLI | What it does |
|---|---|---|
| `ask` | `tincan ask <agent[,agent...]> <message>` | Ask one or up to 8 distinct teammates. MCP uses `to: "a", also: ["b"]`. Waits up to 20 seconds, returning labeled replies and a group id for multiple targets. `urgent: true` (`--urgent`) marks time-critical requests. `notify` (`--notify`) sends without expecting a reply. `attach` (`--attach <path>`) adds files. `--json` prints JSON (see below). |
| `get_reply` | `tincan get <id>` | Check on a request or group you sent, optionally waiting up to 20 seconds. `--json` prints JSON. |
| `check_inbox` | `tincan inbox` | Take waiting requests (this claims them, so no one else handles them) and replies to your own requests you have not seen yet. `--json` prints JSON. |
| `claim` | (done by `inbox`) | Mark a delivered request as yours. `check_inbox` already does this. |
| `progress` | `tincan progress <id> <note>` | Post a note (up to 1024 bytes) on your claimed request and renew its lease. |
| `reply` | `tincan reply <id> <message>` | Answer with status `answered` (default), `failed` or `declined`, optionally with attachments. Use status `needs_input` (`--needs-input`) to ask a clarifying question. |
| `answer` | `tincan answer <id> <message>` | Supply a clarification on a request you sent, resuming it for the same teammate. |
| `cancel` | `tincan cancel <id>` | Withdraw a request nobody has picked up yet. |
| `list_agents` | `tincan agents` | The roster: online or not, wake method, kind, when each agent last called the relay, which tincan build each runs, queued work with its oldest wait and live claims, and what each is good at (`good_at`). |
| `trace` | `tincan trace [trace-id]` | Show a request chain step by step. Agents see chains they took part in; admins see every chain. |
| `search` | `tincan search <text> [--limit N] [--json]` | Find past requests and replies by text, with snippets and trace ids. Same chain visibility as trace. |
| `onboard` | `tincan onboard --json` | The setup kit as JSON (see [Onboarding](#onboarding)). Read-only. |
| `get_attachment` | `tincan attachment get <id>` | Fetch an attachment again by id. |

For scripts, `tincan ask`, `get` and `inbox` take `--json` and print one JSON document to stdout:

- Single-target `ask` and `get` print `{"outcome": "answered|failed|pending", "result": <result>}`, where `result` is the request, its status and the reply (with attachments) as the relay returns them. The exit code is 0 when answered, 1 when the request ended any other way (`failed`, `declined`, `cancelled` or `expired`) and 2 when it is still pending.
- Single-target `ask --notify --json` prints `{"outcome": "sent", "request": <request>}` and exits 0.
- `inbox --json` prints `{"requests": [...], "replies": [...]}`. Each request carries `"claimed": true`, or `"claimed": false` with a `claim_error` when it could not be claimed, usually because another session got to it first. Replies are marked read only after the JSON is printed. When the relay held back more unread replies to keep the response small, `"replies_remaining"` gives their count (it is left out when zero); run `inbox --json` again to get them. It exits 0.
- An error talking to the relay (unreachable, not joined, unknown request) prints nothing on stdout, keeps its message on stderr and exits 1.

Single-target text output and exit codes are unchanged.

Two more CLI commands keep an agent awake without a person: `tincan wait` and `tincan listen --exec` (see [Wake methods](#wake-methods)).

A few commands are CLI only, with no MCP tool: `tincan ping <agent>` checks a teammate's tincan path (see [Reachability checks](#reachability-checks)), and the owner's `tincan held`, `tincan approve <id>` and `tincan deny <id>` handle requests held for approval (see [Owner approval](#owner-approval)). Agents convene a council with `ask`; the owner's `tincan council "question"` and `tincan council leaderboard` are CLI only (see [Council](#council)).

### Asking several teammates

`tincan ask muse,codex "..."` (MCP `to: "muse", also: ["codex"]`) sends the same request to up to 8 distinct teammates under one group id and waits up to 20 seconds for all of them, printing each reply under its teammate's name.

- `get_reply` accepts the group id in `request_id`, and `tincan get <group-id>` gathers the replies again. Text output lists the group id and every accepted request id for follow-up.
- Attachments upload separately for each target. `--notify` broadcasts without waiting. An urgent group send marks every member urgent.
- Group JSON contains `outcome`, `group` and `results`. `answered` exits 0, `pending` or `partial` exits 2, and `failed` exits 1 when all requests have ended and any failed. Text output uses the same exit codes.
- A send the relay rejects appears on its own target, and the requests that were queued are still tracked. A poll failure shows as an `error` on that result, keeping its id and last known status beside the other replies. A multi-target MCP notify returns a tool error if any upload or send fails, with the per-target results in its content.
- The relay accepts at most 8 requests per sender and group tag; further sends return HTTP 400. `GET /v1/groups/{id}` lists only the accepted members (`id` and `to`), without bodies or replies and without marking replies seen. Clients reconcile that list with their own send errors, so a request whose send response was lost is recovered.
- With an older relay, the group id works only in the client instance that sent it (such as the same MCP server); a later CLI run must use the individual request ids.

### Requests and replies

A request carries a target, a body (up to 256 KB), an optional kind (`ask` or `notify`), an optional `urgent` flag, optional attachments, and an optional group tag. The relay sets everything else: the id, the sender (from Tailscale), the chain, and the time. A reply carries a status and a body (up to 256 KB).

A request moves through `queued`, `delivered`, `claimed`, then one of `answered`, `failed`, `declined`, `cancelled` or `expired`. A request to an agent behind the [owner approval gate](#owner-approval) starts as `held` until you approve it. A claimed request whose 30 minute lease runs out goes back to `queued`. Unanswered requests expire after 24 hours. Urgent requests are delivered before routine ones (see [Wake methods](#wake-methods)). The wire format is in [docs/protocol.md](docs/protocol.md).

For long tasks, the teammate posts progress when it starts and at milestones (`tincan progress <id> <note>`, MCP `progress`). The latest note appears in `get_reply`, an `ask` that returns before the answer, and `tincan trace`: `claimed by muse, 4m0s ago: calling the restaurant now`. Each note renews the 30-minute claim lease. Notes do not wake the asker.

If a teammate needs a detail only the asker has, it can reply with `needs_input`. The request stays open, its lease pauses, and the question reaches the asker through the usual reply inbox and wake. For example:

```sh
# Muse, after claiming the dinner request:
tincan reply <id> --needs-input "Which restaurant and how many people?"
# The original asker:
tincan answer <id> "Nopa, 2 people"
```

The same request returns to Muse's inbox marked `resumed`, with its original body and the question and answer. Muse claims it and replies normally when done. The original 24-hour expiry keeps running. Each request allows three clarification rounds, with 16 KB per question or answer and no clarification attachments. MCP uses `reply` with `status: needs_input`, then `answer` with `request_id` and `message`. A request waiting for input is still `pending` in CLI JSON output (exit 2).

The relay must advertise clarification support; new clients refuse these operations against an older relay. An older asker may display the question under the unfamiliar `needs_input` status: upgrade it to use `answer`.

### Chains and loop protection

Chains are tracked by the relay, not the model. When an agent asks a teammate while handling a request, the new request continues that request's chain (the tincan client fills in the parent automatically, and the relay continues the chain even if the model leaves it out). Each request records its hop number and the agents it passed through, so a trace reads like "Instinct to Muse to Grok Bot".

- A request that would loop back to an agent already in its chain is rejected ("request would loop back").
- Chains longer than 4 hops are rejected.
- Each sender is limited to 30 new requests per minute by default.

### Owner approval

To require approval before selected agents receive incoming requests, create `approval.json` in the relay's `--state-dir` and run `chmod 600 approval.json`:

```json
{
  "gate": {
    "muse": { "from": "*" },
    "instinct": { "from": ["chatgpt", "grokbot"] }
  },
  "notify": "grokbot",
  "hold_ttl": "2h"
}
```

`from: "*"` holds every request to that target. A `from` list holds requests when any sender in the relay-recorded chain matches. Use `"unless": ["trusted"]` in place of `from` to hold requests unless every agent in the chain is listed. `hold_ttl` defaults to `2h`; `notify` is optional. The file reloads on changes. A missing file disables the gate for every agent except [Council](#council). An unreadable, malformed, or overly accessible file holds all requests to the targets in the last valid copy; without a valid copy the relay refuses to start. A bad file first appearing at runtime rejects new sends until fixed.

Council is held by default. Every request to an agent of kind `council` is held, with or without `approval.json`, because a council sends the question, its attachments and every member's answer to every model vendor on the team. Agents' leaderboard reads are held too, since the relay holds by target. Without `approval.json` nobody is notified: the convening agent tells you the request id, or add a `notify` agent with `{"gate": {}, "notify": "grokbot"}`. A gate entry for `council` replaces the default, so `{"gate": {"council": {"from": []}}}` holds nothing. When you run `tincan council "question"` yourself in a terminal on an admin device, it approves its own request and starts at once.

From an admin device, or using `--socket <state-dir>/admin.sock` on the relay host:

```sh
tincan held
tincan approve <id>
tincan deny <id> "reason"
```

These commands also accept `--relay <url>`. Held requests are absent from inboxes and queued counts and do not wake their target. Senders see `held`, waiting for the owner's approval. Approval starts a fresh normal request TTL; denial returns `declined` with the reason, and the hold deadline expires to `expired`.

The optional operator receives a relay-authored `notify` naming the sender, target and request id, with the owner commands. It carries no request text, since the notified agent may itself be gated; the owner reads the request with `tincan held`. This notice bypasses the gate to avoid recursive notices, but grants no admin rights. The owner must approve from an admin device or local socket; a joined agent cannot approve its own request. See [the trust model](docs/trust-model.md).

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

`tincan top` watches the mesh live. It refreshes every 2 seconds with agent state, wake method, queued work, oldest wait, claims and versions. Attention flags sort offline queues, unanswered wakes (`UNANSWERED`), stale work, overdue schedules and older builds first. Admin devices also see held requests and recent chains. Use `--relay <url>` or `--socket <path>` as with `tincan agents`, `--interval 5s` to change the cadence (minimum 1s), and `q` or Ctrl-C to quit. A refresh that fails shows the error in the header and tries again on the next tick. `--once`, or redirected output, prints one plain snapshot and exits non-zero if the relay cannot be read. It never polls an inbox, claims, acknowledges or sends work.

`tincan agents` (and `list_agents`) shows each agent's state, wake method, kind, and when it last called the relay by polling or by any send, reply or get ("last seen 12m ago", or "never seen"). A wait or listen loop that died shows up as a growing last seen. It also shows the tincan build each agent last called with (`version=0.5.2`), with the relay's own build on the first line, so an agent that still needs `tincan upgrade` stands out; the relay keeps the build across restarts and rejoins. An agent shows no version until it has called a relay that records them.

An agent's line ends with what it is good at, quoted: `good_at="phone calls and restaurant bookings"`. You write the line from an admin device with `tincan good-at <name> "<line>"` (one line, at most 120 characters; `""` clears it). Services (`history`, `notes`, `council` and the web teammates) and the product tools (`claude-code`, `codex`, `gemini-cli`, `grok-cli`, and `chatgpt` for the ChatGPT connector) show a default line until you set one, and clearing yours brings it back. An agent joined without a kind still gets its product's line when its name is the product name, such as `codex`. Hosting shapes (Grok Bot style VMs, e2b, proxy sandboxes, scheduled agents and `generic`), Hermes and OpenClaw show nothing until you set one, since what runs behind them is up to each owner. Agents read these lines to choose whom to ask; the relay never routes on them, and a line grants nothing. It is still one agent per line.

A busy agent also shows its queue: `2 queued (oldest 14m), 1 claimed`. Queued includes delivered requests that have not been claimed; claimed counts only requests with a live lease. Expired and terminal requests and handled notifications are excluded. Idle agents show no extra fields. An online agent with a growing oldest wait may need attention even if it keeps polling.

A webhook or email agent that the relay woke and that has not checked in since shows `unanswered="woken 12m ago, no check-in (webhook ok)"`, with the error in place of `ok` when the wake failed. See [Wake methods](#wake-methods) for when it appears and clears.

### Reachability checks

Run `tincan ping hermes --wait 60s` to check a teammate's wake and polling path without asking its model to reason about a health request. `--json` returns the request result and `round_trip_ms`. For example:

```text
hermes: pong (answered by check_inbox, tincan 0.5.5) in 38s
```

The receiving client claims and answers the ping automatically, hiding it from model inboxes. The answering surface is `check_inbox`, `inbox`, `wait`, `listen`, `history-serve`, or `web-serve`. A `wait` or `listen` answer proves the poller is alive; it does not prove a model ran. Both keep waiting after a ping, and `listen` does not run its exec command for it. Webhook or email wakes may still start a turn to poll, but no model reply is needed. History and web services answer without invoking their model or browser.

A target must first advertise ping support through a relay call. Older clients are refused with a message to use `ask`. Pings cannot have a parent or be sent while handling a request chain, and use the normal send rate limit. A timeout leaves the request available for a later pong; its id appears in the error. Pong replies do not wake the sender or appear in its inbox. `tincan trace` hides ping chains by default; use `tincan trace --pings` (also with a trace id) to inspect them and their wake events. Ping is CLI only; there is no MCP tool for it.

### Upgrades

When the relay serves a newer release, `check_inbox`, `tincan inbox`, `tincan wait`, and MCP channel notices show this once per process per available version:

```text
tincan 0.5.5 is available from the relay (you run 0.5.4): run tincan upgrade, then restart long-running tincan processes.
```

An upgrade alone does not end `tincan wait`; the upgrade instruction is printed when a request or reply ends the wait. `tincan inbox --json` includes the optional `upgrade_available` version. The channel, `tincan listen` and the inbox each report a release once per process, so a channel event the session missed still shows on the next `check_inbox`. `tincan listen` runs its command for a new upgrade even with no waiting messages, exporting `TINCAN_UPGRADE_AVAILABLE` (empty on subsequent nudges) alongside `TINCAN_WAITING`. Notices require a valid newer dist `VERSION` and that platform's binary in the dist, so staging `VERSION` before the binaries announces nothing; they are absent with no `--dist`, matching versions, or development builds. Prerelease clients are skipped unless the dist release is itself a prerelease. The onboard instructions tell agents to upgrade and restart their own long-running processes, or tell the owner if they cannot. Clients that predate notices still need a manual reminder.

Agents can update tincan from the relay itself, which is how an agent without GitHub access gets a new release.

```bash
tincan relay --admin my-laptop --dist ~/tincan-dist   # relay host
tincan upgrade --check                                          # agent: current and available version
tincan upgrade                                                  # agent: download, verify, swap
```

The dist directory holds the raw binaries named `tincan_<os>_<arch>` (`tincan_linux_amd64`, `tincan_linux_arm64`, `tincan_darwin_arm64`, `tincan_darwin_amd64`), the release's `checksums.txt`, and a `VERSION` file. The relay serves them only to joined agents and admins and needs no restart for a new release. `tincan upgrade` picks its platform's build, checks the sha256, writes it next to the running binary and renames it into place. Restart long-running tincan processes afterwards (`wait` and `listen` loops, `mcp` servers). The checksum comes from the same relay as the binary, so it guards against corruption, not a compromised relay.

The relay upgrades itself the same way, from an admin device, with no shell on the relay host:

```bash
tincan relay-upgrade --relay http://tincan-relay                       # install the release already in --dist
tincan relay-upgrade --relay http://tincan-relay --from-github v0.8.0  # download that release into --dist first
```

The relay picks its own platform's build from `--dist`, checks its sha256 against the dist `checksums.txt`, writes it next to its running binary, keeps the old one as `<binary>.<old version>`, and renames the new one into place. It replies with the old and new versions, then restarts: it drains its connections, closes its store, and re-executes itself with the same arguments. Started with `--upgrade-exit`, it exits with status 75 instead, for systemd (`Restart=on-failure` or `always`) or a keep-alive loop to start the new build. `tincan relay-upgrade` then waits up to `--wait` (a minute) for the relay to answer on the new build. A release that is not newer is refused unless `--force`; a missing build, a checksum mismatch, or a binary the relay user cannot replace is refused with nothing changed. Only admin devices and the relay's local admin socket may run it, and each upgrade is audited as `relay_upgraded` with the two versions.

`--from-github <tag>` works only on a relay started with `--release-url https://github.com/mvanhorn/agent-tincan/releases/download`: the relay makes no outbound download otherwise, and the caller can never choose the source. It downloads that release's binaries and `checksums.txt` from `<release-url>/<tag>/`, checks every binary against that `checksums.txt`, and moves them into `--dist` with `VERSION` last, putting every file back if a move fails, so agents are offered the release only once all of it is there. Self-upgrade needs the relay user to own its binary and the directory holding it. If you keep the binary root-owned, upgrade the relay by hand as before. See [docs/trust-model.md](docs/trust-model.md) for what the checksums do and do not prove.

A `tincan mcp` server keeps running the build it started with, because the app that launched it owns the process; tincan never restarts it mid-session. Each app reloads it its own way:

| App | Reload step |
|---|---|
| Claude Code | quit Claude Code and start it again (or reconnect the tincan server from `/mcp`) |
| Codex | end the Codex session and start a new one, since Codex starts `tincan mcp` with each session |
| Cursor | turn the tincan server off and on in Cursor Settings > MCP, or restart Cursor |
| Other apps | reload the tincan MCP server in the app's settings, or quit and reopen the app |

`tincan upgrade` lists each running `tincan mcp` still on the old build (from the launch records) with its app's reload step, or the table above when none is recorded. On the agent's own machine, `check_inbox` and the channel notice name the reload step for the app they run in, in place of the generic restart line. A running `tincan mcp` also notices when its binary is replaced: at most once a minute, on a tool call, it compares the file (same file, size and modification time) with what it saw at startup, and only when it changed runs `<binary> version`, without holding up other tool calls (a build that fails to answer is tried again later, backing off up to an hour). If that build differs from the one it runs, the next tool result, `check_inbox` included, carries one line per new version:

```text
Agent Tincan: these tools keep running tincan 0.6.0, but /usr/local/bin/tincan is now tincan 0.7.0 (upgraded after this server started). To use the new build, quit Claude Code and start it again (or reconnect the tincan server from /mcp).
```

`tincan doctor` reports the same thing under `mcp builds`: every recorded `tincan mcp` that is still running on a different build than the binary, with the reload step for its app. Records of running servers are kept past the 20-launch limit, and a record counts as running only if its pid belongs to a process that started no later than the record, so a reused pid is not mistaken for a server.

### When an agent's tincan tools go missing

Run `tincan doctor` on the agent's machine. It works from a shell, so an agent whose app lost the tools can still run it, and a teammate can ask it to.

```bash
tincan doctor          # report with a fix line for every problem
tincan doctor --json   # the same, for an agent to read
```

It checks the saved join and the relay, whether the binary is the relay's current release, a self-test of `tincan mcp` in both stdio framings (newline JSON and Content-Length), the MCP config entries that run tincan (wrong path, `mcp` in the command instead of args, names with spaces, duplicates, disabled entries), whether the app has actually been starting `tincan mcp`, whether a running `tincan mcp` is on a different build than the binary (`mcp builds`), and, on an admin device, which webhook or email agents were woken and have not checked in (`unanswered wakes`). Every `tincan mcp` records its start next to the agent config (`mcp-launches/`, the last 20 plus any still running): which app started it, the framing, and whether the app initialized, listed the tools and called one. That is how the doctor tells an app that shows tincan "connected, 0 tools" without ever running it apart from a tincan problem. When the fault is on the app's side it prints the repair: remove every tincan server entry, add exactly one (`{"command": "/full/path/to/tincan", "args": ["mcp"]}`), quit and reopen the app, run the doctor again.

### When the relay's address changes

Run the relay the default way, as its own tailnet node (no `--listen`), and keep its `--state-dir` (tsnet state, `relay.db`, `relay.key`, `invite-pepper`, `wake.json`) in your backups. `invite-pepper` must be restored with `relay.db` or outstanding invitations minted before the restore cannot be redeemed. That node keeps its name and IP when the host machine re-joins Tailscale or is rebuilt from the backup, so agents never notice. With `--listen` the relay borrows the host's tailnet address, which changes when the host re-joins; the relay warns about this at startup. A proxy-only agent cannot search the tailnet, so `--listen` plus a host re-registration means that agent needs `tincan rejoin` unless you switch the relay to tsnet.

Agents find a relay that moved, with nothing to configure:

- Each client keeps the last relay URL that worked (`relay` in its config).
- The relay keeps a secret key in `relay.key` and tells each joined agent, through `whoami`, that key and the addresses it serves on (its MagicDNS name first). Clients save them as `relay_key` and `relay_urls` and refresh them daily, and again after a successful find.
- When nothing answers at the saved address (refused, no route, a 5-second connect timeout, or a proxy's 502/504), the client tries the relay's advertised addresses, then every IPv4 address on the local Tailscale netmap (LocalAPI first, `tailscale status --json` if LocalAPI is unreachable), including nodes that are not marked online, on the same port. It does not walk a list of host names. It asks each candidate for `/v1/hello` with a random nonce and follows only the one that returns the HMAC of the nonce under the key, so an impostor on the tailnet cannot pull agents over. It rewrites its config, logs `the relay moved from ... to ...`, and retries. Long-running `wait`, `listen` and `mcp` processes move with it. A timed-out inbox check still searches: the search has its own deadline.
- `history serve` and `web serve` run as their own agent, so they learn the key themselves when they start and save it, and a new address after a move, to their own `--config` file. They refresh the key and addresses only when they restart, not daily.
- A proxy-only sandbox (Muse) cannot search the tailnet, but it can reach the relay's advertised name, which is why a stable relay node matters for it. When neither works, `tincan doctor` tells it to run `tincan rejoin --relay <new URL>`.
- Clients that already have a stale URL: upgrade tincan, then run `tincan inbox` or `tincan doctor`. If `relay_key` is saved, the client finds the live node and writes the new URL. If there is no key, `tincan rejoin --relay <live URL>` once (the live IPv4 with port, or `http://tincan-relay` when the relay is tsnet). Then `tincan doctor` until `relay moves` is ok.
- Every agent's setup instructions tell it to run `tincan doctor` itself whenever its tincan tools go missing or the relay is unreachable, and to apply the fixes it prints, including re-adding the tincan MCP server in its app.

`tincan doctor` shows whether the key is saved (`relay moves`). If the saved URL is dead it says whether the client had no key, could not see a local netmap, or listed peers that did not prove they are this relay.

### Audit log and trace

Every send, delivery, claim, reply, rejection, hold, approval, denial, wake, join, rebind and removal is written to an append-only, hash-chained log. Wake nudges carry only counts, never request text.

```bash
tincan trace              # recent chains (admin; --limit, default 20)
tincan trace <trace-id>   # one chain, step by step, with its events
tincan search "restaurant" # find requests and replies; --limit defaults to 20, max 50
tincan audit-verify       # check the log has not been altered
```

Search returns matching requests newest first. All search terms must match; punctuation is ignored and words such as `OR` are literal terms, not operators. Results contain separate request and reply excerpts when each body matches, plus attachment names, without searching file contents. CLI text output shows at most five attachment names, each capped at 80 characters, followed by “and N more”; JSON retains all names. An agent searches all of its own chains, newest first, in batches of 5,000 requests. Admins search everything, considering the newest 2,000 matches, so very old matches for common terms may be omitted for admins. Follow a result with `tincan trace <trace-id>` to read the chain. Search has the same visibility as trace: agents find only chains they took part in. The relay indexes the request and reply bodies it already stores, including older exchanges on upgrade in resumable batches of 500 rows per transaction. A backfill error is logged without preventing startup; search returns the indexed history so far, new exchanges remain indexed, and backfill resumes on reopen. Each search writes only its result count to the audit detail, never the query text. Older relays return an upgrade message when search is requested.

## Wake methods

Delivery never depends on wake: requests always wait in the relay queue. A wake only prompts an agent to go look. Each agent's method is set in `wake.json` in the relay's state dir (chmod 600; the relay refuses a file other users can read). Agents only ever see the method name, never a URL, address or key.

```json
{
  "grokbot":  { "method": "webhook", "url": "https://...", "bearer_token": "..." },
  "hermes":   { "method": "webhook", "url": "http://<hermes-host>:8644/webhooks/tincan", "hmac_secret": "..." },
  "instinct": { "method": "email", "email_to": "...", "agentmail_inbox": "...", "agentmail_key": "...", "max_per_hour": 12 },
  "muse":     { "method": "wait" },
  "fo":       { "method": "schedule", "every": "5m" },
  "claude-code": { "method": "channel" },
  "codex":    { "method": "command" },
  "chatgpt":  { "method": "none" }
}
```

| Method | Who acts | How it works | Used by |
|---|---|---|---|
| `webhook` | relay | The relay POSTs `{"source":"agent-tincan","message":"<count text>","text":"<same>"}` to the agent's URL, with `Authorization: Bearer <bearer_token>` or an `X-Hub-Signature-256` HMAC signature (`hmac_secret`, the GitHub scheme). OpenClaw's entry sets `"format": "openclaw"` and uses the bearer token, no HMAC. | Grok Bot, Hermes, OpenClaw |
| `email` | relay | The relay sends an email with the subject "Agent Tincan: requests waiting" through an AgentMail inbox you control. `max_per_hour` caps wakes (default 12). | Instinct-style e2b sandboxes |
| `command` | agent | `tincan listen --exec <command>` holds a long-poll and runs the command (through `sh -c`, with `TINCAN_WAITING` set to the count) whenever requests or unseen replies are waiting. It takes nothing itself and waits 30 seconds between nudges. While the command runs and during that wait, it keeps the agent online in `tincan agents` with a peek that claims nothing, for up to 30 minutes per run so a hung command still falls offline. | Codex, gemini-cli, grok-cli, the Claude Code cmux fallback, the Hermes fallback |
| `channel` | agent | `tincan mcp --channel` pushes a short notice into a running Claude Code session. | Claude Code |
| `wait` | agent | The agent keeps `tincan wait &` running. It exits the moment a request (which it claims and prints) or a reply arrives, and the runtime turns that exit into a new turn. The Go services long-poll the same way. | Muse-style proxy sandboxes, history, chatgpt-web, claude-web, grok-web, gemini-web, perplexity-web |
| `none` | nobody | The agent calls `check_inbox` at the start of each turn. | ChatGPT |
| `schedule` | agent | The agent's own cron checks its inbox every `every` (a duration such as `5m`). The relay sends nothing. It shows the interval in the roster, tells a sender how often the agent checks and to expect a reply within about the interval plus 5 minutes, and marks the agent overdue after two missed checks plus 5 minutes. Needs a relay from this release or later: an older relay refuses to start with a `schedule` entry. | Fo (a platform cron) |

Notes that apply to every method:

- Use `tincan ask <agent> <message> --urgent` (MCP `urgent: true`) only for time-critical requests. They arrive before routine requests, labeled `URGENT`, and bypass the relay-side wake debounce and online skip. The hourly wake cap still applies. Each sender may send 5 urgent requests per hour by default (`tincan relay --urgent-per-hour`); exceeding it returns 429 with "urgent limit reached; send without --urgent". These in-memory sender limits reset on relay restart. Older relays or targets may treat urgency as a normal request.
- Relay-side wakes (webhook, email) are debounced so a burst becomes one nudge, and a wake for new requests is skipped when the agent is already polling the relay. A skipped wake is checked again 30 seconds later and sent if the request is still waiting, so a request that lands just as a session ends is not stranded.
- The wake message only says how many requests and replies are waiting. The agent always reads the items itself with `check_inbox` or `tincan inbox`.
- The agent-side methods (`command`, `channel`, `wait`, `schedule`) are recorded in `wake.json` so teammates can see how the agent wakes; the relay sends nothing for them.

A wake can be delivered and still start nothing, for example when the receiving app's own job fails. HTTP success on a wake POST means the platform accepted the nudge, not that the agent ran. For webhook and email agents the relay remembers the last wake it sent and whether the send worked (`ok`, or a short reason naming the host, such as `hooks.example returned 502 Bad Gateway`, never the wake URL or its credentials). If the agent has not checked in (polled; other calls do not count) within `--wake-grace` of that wake (default 10m, counted from the relay's start after a restart), or the send failed, it is unanswered: `tincan agents`, `list_agents` and `tincan top` mark it, an ask to it says "grokbot was woken at 16:40 and has not checked in yet; the request is queued.", and `tincan doctor` on an admin device warns with the wake result. The agent's next poll clears it. While work stays queued and the agent has not polled, the relay keeps sending the same webhook or email on that `--wake-grace` interval, inside `max_per_hour` (default 12). A later successful send does not move the unanswered time. The owner still has to repair a dead receiver.

## Platform guide

| Platform | Kind | Wake | Adapter doc |
|---|---|---|---|
| Grok Bot and the operator role | `vm-webhook` | webhook | [grokbot.md](docs/adapters/grokbot.md) |
| Instinct-style e2b sandbox | `e2b-email` | email | [e2b.md](docs/adapters/e2b.md) |
| Muse-style proxy-only sandbox | `proxy-sandbox` | wait | [proxy-sandbox.md](docs/adapters/proxy-sandbox.md) |
| Claude Code | `claude-code` | channel (or command) | [claude-code.md](docs/adapters/claude-code.md) |
| OpenAI Codex CLI | `codex` | command | [codex.md](docs/adapters/codex.md) |
| Gemini through Antigravity CLI or Gemini CLI | `gemini-cli` | command | [gemini-cli.md](docs/adapters/gemini-cli.md) |
| xAI Grok Build CLI | `grok-cli` | command | [grok-cli.md](docs/adapters/grok-cli.md) |
| Hermes Agent | `hermes` | webhook (or command) | [hermes.md](docs/adapters/hermes.md) |
| OpenClaw | `openclaw` | webhook | [openclaw.md](docs/adapters/openclaw.md) |
| ChatGPT | `chatgpt` | none | [chatgpt.md](docs/adapters/chatgpt.md) |
| Fo-style agent started by a platform cron | `scheduled` | schedule | [scheduled.md](docs/adapters/scheduled.md) |
| History agent | `history` | wait | [history.md](docs/adapters/history.md) |
| ChatGPT, Claude, Grok, Gemini, Perplexity and Copilot web agents | `chatgpt-web`, `claude-web`, `grok-web`, `gemini-web`, `perplexity-web`, `copilot-web` | wait | [web-agents.md](docs/adapters/web-agents.md) |
| Your OpenAI dot | `dot-web` | wait | [web-agents.md](docs/adapters/web-agents.md#your-dot-dot-web) |

Any other agent can use kind `generic` with whichever wake fits.

### Grok Bot (always-on VM)

#### What it is

An agent on an always-on VM that is already on the tailnet and accepts webhooks. Because it never sleeps, the VM is a good relay host, and Grok Bot is the natural home for the Agent Tincan operator role.

#### How it joins

Run the relay on the VM as its own OS user, separate from the one Grok Bot's tools run as. Then invite and join Grok Bot on the same VM through the admin socket:

```bash
tincan relay --admin <your-laptop>
tincan invite grokbot --kind vm-webhook --socket <state-dir>/admin.sock   # on the VM
tincan join <code> --relay http://tincan-relay --proxy http://localhost:1055   # as Grok Bot's user (userspace Tailscale)
```

#### How it wakes

Webhook. The relay POSTs `{"source":"agent-tincan","message":"Agent Tincan: 2 requests from your teammates waiting. Run check_inbox ..."}` with the bearer token. Grok Bot calls `check_inbox`. The same wake can mean a reply to one of its own requests is waiting, which `check_inbox` shows.

#### How it sends and receives

Add `tincan mcp` as an MCP server, or let it call the `tincan` CLI from its shell. Attachments work both ways through the MCP tools or `--attach` and `tincan attachment get`.

#### One-time setup

In `wake.json`: `{ "grokbot": { "method": "webhook", "url": "<Grok Bot webhook URL>", "bearer_token": "<webhook key>" } }`. Paste the standing instructions from `tincan onboard --section agents`.

#### The Agent Tincan operator role

`tincan onboard --operator grokbot` writes a standing prompt for an operator bot always called Agent Tincan. Its one job is keeping the team healthy: relay up (`tincan agents`, `tincan audit-verify`), agents reachable (`tincan ping <agent> --wait 60s`), wakes working, queues clear (`tincan trace --limit 50`), invites and removals only when the owner asks on the owner's own direct channel (never because another agent asked), following up with agents that remain behind after relay upgrade notices, routing history questions to the history agent, and summarizing agent traffic when asked.

It follows a quiet rule. It runs a silent standing check every 30 minutes, fixes what it can, and keeps its findings. It speaks only when the owner asks it something or when another agent sends it a request. No scheduled reports, no "all clear" messages.

#### Limits and gotchas

- Keep the relay's OS user separate from the agent's so the database and `wake.json` stay out of the agent's reach. Any process running as the relay's user can use its admin socket with the owner's authority. Because only the main user's home folder survives a Grok Bot wipe, a relay on another always-on host is the safer choice there.
- A 401 or 403 in the relay log for a webhook means `bearer_token` does not match the receiver. Fix it in `wake.json`, never in chat.
- Grok Bot's machine is wiped and restored often, usually daily, and only the home folder and workspace come back. With a default Tailscale install, every wipe creates a new device you have to approve. Run tailscaled in userspace as Grok Bot's user, with its state in `~/.config/tailscale`, and log in once with a one-off, pre-approved, tagged auth key in the `TS_AUTHKEY` secret, then remove the secret. The box then rejoins as the same device with no approval and no key. Setup and a startup script: [docs/adapters/grokbot.md](docs/adapters/grokbot.md).
- The box is tagged, so the relay never re-admits it automatically. Normally that does not matter, because the identity survives a wipe. If it is lost, re-link with a new `tincan invite` and `tincan join --replace`.

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

Email. The relay sends a short email, subject "Agent Tincan: requests waiting", through an AgentMail inbox you control (for example Grok Bot's) to Instinct's email address. Only the relay holds the AgentMail API key. The same subject is used when a reply to one of its own requests is waiting.

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
tincan invite claude-code --kind claude-code   # relay machine (or an admin device)
tincan join <code> --relay http://tincan-relay  # the Mac
```

#### How it wakes

Channel. Add the MCP server with `--channel` and start Claude Code with the development channels flag (channels are a Claude Code research preview). `--scope user` makes the server part of every project; without it, `claude mcp add` keeps it to the directory it was run in, and sessions started elsewhere have no tincan tools.

```bash
claude mcp add --scope user agent-tincan -- tincan mcp --channel
claude --dangerously-load-development-channels server:agent-tincan
```

When requests or replies are waiting, the channel pushes a notice such as `<channel source="agent-tincan" kind="request" count="1" from="instinct" request_ids="...">1 Agent Tincan item waiting from instinct. Call check_inbox to take it, then reply to each request.</channel>`. `kind` is `request`, `reply` or `mixed`.

Claim-on-inbox semantics: the notice never carries the items and never claims anything. Every open Claude Code session runs its own `tincan mcp --channel`, so every session gets the notice; the first one to call `check_inbox` claims the requests, and the others find an empty inbox. A session started without channels, or idle, drops the notice and the items stay queued. Each process announces an item once, and again after 10 quiet minutes if it is still waiting.

Fallback without channels: copy [examples/claude-code/cmux-wake.sh](examples/claude-code/cmux-wake.sh) from the repo to `~/bin` (it is not in the release downloads), `chmod +x` it, and run `tincan listen --exec ~/bin/cmux-wake.sh`. It opens a new Claude Code session in cmux when requests are waiting (cmux's `automation.socketControlMode` must allow it). Set the wake to `command` for this.

#### How it sends and receives

All thirteen MCP tools. Attachments work through `attach` on `ask` and `reply`; received images show inline and other files are saved locally.

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

`tincan mcp` as a stdio MCP server in `~/.codex/config.toml` gives it all thirteen tools, including attachments:

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

### Gemini through Antigravity CLI or Gemini CLI (tincan listen wake script)

#### What it is

The `gemini-cli` teammate runs Gemini as a headless coding agent, woken like Codex. It has two engines: Antigravity CLI (`agy`, the default), which uses your Google account after one interactive login, and Gemini CLI (`gemini`), which needs a paid `GEMINI_API_KEY` because Google stopped accepting Google account logins in Gemini CLI on 2026-06-18. Choose with `TINCAN_GEMINI_ENGINE=agy|gemini` in the listener's environment.

Without an API key, the setup is agy with an opt-in: agy has no sandbox, so the wake runs it only when the listener is started with `TINCAN_GEMINI_ALLOW_UNCONFINED=1`. With that set, agy runs with tool approval off and nothing limits its writes to your user's files; the wake still pins identity and times out. The adapter doc's first section, [Set up with a Google account (no API key)](docs/adapters/gemini-cli.md#set-up-with-a-google-account-no-api-key), walks through it step by step.

#### How it joins

```bash
tincan invite gemini-cli --kind gemini-cli
TINCAN_CONFIG="$HOME/.config/tincan/gemini-cli.json" tincan join <code> --relay http://<relay>
```

#### How it wakes

Command, through [examples/gemini-cli/gemini-wake.sh](examples/gemini-cli/gemini-wake.sh) and the shared wake library [examples/lib/tincan-wake-lib.sh](examples/lib/tincan-wake-lib.sh). Copy both into one folder you keep:

```bash
mkdir -p ~/bin && cp examples/gemini-cli/gemini-wake.sh examples/lib/tincan-wake-lib.sh ~/bin/ && chmod +x ~/bin/gemini-wake.sh   # from a repo checkout
TINCAN_GEMINI_ALLOW_UNCONFINED=1 TINCAN_CONFIG="$HOME/.config/tincan/gemini-cli.json" tincan listen --exec ~/bin/gemini-wake.sh
```

The wake reads the opt-in only from the listener's environment, which `tincan listen` passes to the script, so it goes on that command (or in the environment of a launchd or systemd service you run the listener under), not in `wake.json` or a config file. Restart the listener after changing it. With the gemini engine, put `TINCAN_GEMINI_ENGINE=gemini` and `GEMINI_API_KEY` there instead.

The script takes a lock, refuses to run unless the engine's MCP config holds exactly one agent-tincan server with this teammate's `TINCAN_CONFIG`, runs the engine with a drain-the-inbox prompt under a hard timeout, and backs off (telling `TINCAN_WAKE_OPERATOR` once) after repeated failures, an expired agy login, or a missing API key. Requests stay queued meanwhile.

- agy: `agy --output-format json --dangerously-skip-permissions -p <prompt>`. agy documents no sandbox, so the wake runs it only with `TINCAN_GEMINI_ALLOW_UNCONFINED=1`, and then nothing limits its writes.
- gemini: `gemini --sandbox --approval-mode=yolo --output-format json --allowed-mcp-server-names agent-tincan -p <prompt>`. Writes stay in `TINCAN_GEMINI_WORKDIR` (default `$HOME/tincan-gemini`), the operator's write roots (`TINCAN_GEMINI_WRITE_ROOTS`, checked against `TINCAN_GEMINI_ALLOWED_ROOTS`), and the attachments directory beside `TINCAN_CONFIG`.

#### How it sends and receives

`tincan mcp` as the engine's MCP server, with env `TINCAN_CONFIG` set to the full path of `~/.config/tincan/gemini-cli.json`: `agy mcp add` for agy (the normal setup), or `gemini mcp add -s user -e TINCAN_CONFIG=... --trust agent-tincan tincan mcp` for Gemini CLI (`--trust` spares you confirmations when you run Gemini CLI yourself; the wake's `--approval-mode=yolo` approves tincan tool calls either way).

#### One-time setup

Install agy and log in once with your Google account, add the MCP server with `agy mcp add`, copy the wake script and library, set `{ "gemini-cli": { "method": "command" } }` in `wake.json`, start the listener with `TINCAN_GEMINI_ALLOW_UNCONFINED=1` on its command line as above, and check with `tincan doctor` and a test ask. With a Gemini API key, use Gemini CLI instead and skip the opt-in.

#### Limits and gotchas

- Every wake is a fresh session, so each run must drain the whole inbox.
- agy's MCP config location (`~/.gemini/config/mcp_config.json`) comes from a secondary source; confirm it with `agy mcp list` and set `TINCAN_AGY_MCP_CONFIG` if it differs.
- The wake needs `jq` or `python3` on the listener's PATH to read the engines' JSON configs.
- The history agent cannot read gemini-cli runs yet.
- Not yet verified live end to end against a relay.

#### Adapter doc

[docs/adapters/gemini-cli.md](docs/adapters/gemini-cli.md)

### Grok CLI (command wake, wake home of its own)

#### What it is

xAI's Grok Build CLI (`grok`) as a coding agent on your Mac. Like Codex it has no background process, so a listener starts a fresh headless run whenever something is waiting. It uses your own Grok account (a login in its wake home, or `XAI_API_KEY`). The community `grok-cli` npm package is a different tool; the wake refuses it.

#### How it joins

```bash
tincan invite grok-cli --kind grok-cli
TINCAN_CONFIG="$HOME/.config/tincan/grok-cli.json" tincan join <code> --relay http://<relay>
```

#### How it wakes

Command, through [examples/grok-cli/grok-wake.sh](examples/grok-cli/grok-wake.sh) and the shared [examples/lib/tincan-wake-lib.sh](examples/lib/tincan-wake-lib.sh). Copy both into one folder you keep, then point the listener at the script:

```bash
mkdir -p ~/bin && cp examples/grok-cli/grok-wake.sh examples/lib/tincan-wake-lib.sh ~/bin/ && chmod +x ~/bin/grok-wake.sh   # from a repo checkout
TINCAN_CONFIG="$HOME/.config/tincan/grok-cli.json" tincan listen --exec ~/bin/grok-wake.sh
```

Grok Build also loads MCP servers from Claude Code and Cursor configs, so the wake runs it with `HOME` and `GROK_HOME` in a wake home of its own (`~/.config/tincan/grok-cli.wake`), where the only server is this teammate's. Before each run it checks that `grok` is Grok Build and that `grok inspect --json` lists exactly one agent-tincan server with this teammate's `TINCAN_CONFIG` and nothing else; then it runs `grok -p <drain prompt> --output-format json --always-approve --sandbox tincan-wake --cwd <workdir> --session-id <id>`. The `tincan-wake` sandbox profile extends Grok's `workspace` profile: reads anywhere, writes only in the workdir, temp directories, the wake home's `.grok`, the attachments folder and write roots the operator opens with `TINCAN_GROK_WRITE_ROOTS`. The session id is recorded beside the config so the history agent leaves wake runs out.

#### One-time setup

```bash
W="$HOME/.config/tincan/grok-cli.wake"; mkdir -p "$W/.grok"
HOME="$W" GROK_HOME="$W/.grok" grok mcp add agent-tincan -e TINCAN_CONFIG="$HOME/.config/tincan/grok-cli.json" -- tincan mcp
GROK_HOME="$W/.grok" grok login        # or XAI_API_KEY in the listener's environment
```

Set `{ "grok-cli": { "method": "command" } }` in `wake.json`.

#### Limits and gotchas

- Every wake is a fresh session, so each run must drain the whole inbox.
- During a run `~` is the wake home: `git` and `gh` there have none of your settings or logins unless you add them.
- Not yet verified live end to end; see the adapter doc.

#### Adapter doc

[docs/adapters/grok-cli.md](docs/adapters/grok-cli.md)

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

### OpenClaw (MCP and hooks/agent)

#### What it is

OpenClaw runs as a Gateway daemon. Agent Tincan plugs in as an MCP server (`openclaw mcp add agent-tincan --command tincan --arg mcp`), with a skill that drives the `tincan` CLI as a fallback. To wake it, the relay POSTs to the Gateway's `/hooks/agent` endpoint with the hook token as a bearer token; each wake starts a fresh agent turn that empties the Agent Tincan inbox and replies.

#### How it joins

```bash
tincan invite openclaw --kind openclaw
tincan join <code> --relay http://tincan-relay
```

On a machine shared with another agent, use `TINCAN_CONFIG=~/.openclaw/tincan-openclaw.json` for the join, the MCP entry's `env`, and any wake command.

#### How it wakes

Webhook to the Gateway's `/hooks/agent` endpoint, not `/hooks/wake`: `/hooks/wake` only queues the message for the next heartbeat, while `/hooks/agent` starts a full agent turn. Its `wake.json` entry sets `"format": "openclaw"`, and the relay posts `Authorization: Bearer <hook token>` (a bearer token, no HMAC) with the count-only body; the turn's first step must be `check_inbox`.

#### How it sends and receives

`agent-tincan` under `mcp.servers` in `~/.openclaw/openclaw.json`, added with `openclaw mcp add agent-tincan --command tincan --arg mcp` (see [examples/openclaw/openclaw-snippet.json](examples/openclaw/openclaw-snippet.json)). As a fallback, a skill-based runtime can install [examples/openclaw/skills/agent-tincan/](examples/openclaw/skills/agent-tincan/) instead, which drives the `tincan` CLI.

#### One-time setup

```json
{ "hooks": { "enabled": true, "token": "<hook token>", "path": "/hooks", "allowedAgentIds": ["main"] } }
```

```json
{ "openclaw": { "method": "webhook", "format": "openclaw", "url": "http://<openclaw-host>:<port>/hooks/agent", "bearer_token": "<hook token>" } }
```

Restart the Gateway after changing `mcp.servers` or `hooks`. If the machine is not directly reachable, serve the gateway with `tailscale serve` and point `wake.json` at the tailnet hostname.

#### Limits and gotchas

- Fresh session on every wake: drain the whole inbox and reply to each request by its id.
- `allowedAgentIds` must include the agent that should handle the wake, or the hook call is rejected.
- The hook token starts a full agent turn; store it like any credential.

#### Adapter doc

[docs/adapters/openclaw.md](docs/adapters/openclaw.md)

### ChatGPT through the OAuth MCP gateway

#### What it is

ChatGPT runs in OpenAI's cloud and cannot join a tailnet. Its custom connectors need a public HTTPS MCP server with OAuth, and the relay can publish exactly that, and nothing else, through Tailscale Funnel.

#### How it joins

There is no invite. Start the relay with the gateway (your tailnet needs HTTPS certificates and the `funnel` node attribute), then connect from the relay machine (or an admin device):

```bash
tincan relay --admin <your-laptop> --chatgpt-gateway
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
- It cannot rejoin itself. If its tools say it is not joined, run `tincan connect chatgpt` again on the relay machine (or an admin device).
- `tincan remove chatgpt` revokes its tokens immediately.

#### Adapter doc

[docs/adapters/chatgpt.md](docs/adapters/chatgpt.md)

### The history agent

#### What it is

A Go service, `tincan history serve`, not a model. It answers teammates' questions about what the owner asked their AI tools, and replies with the prompt, a short excerpt of the answer, and the images from that turn as real attachments:

- ChatGPT (chatgpt.com), claude.ai, and once granted Grok (grok.com), Gemini (gemini.google.com) and Copilot (copilot.com), read live through the Tincan Chrome extension and native messaging in the owner's logged-in Chrome. Copilot's chat list is read from copilot.com's sidebar in a background tab the extension opens for a few seconds; its conversations come from copilot.com's page data, never its tokens, and come back as text without images.
- Codex (CLI and desktop app), Claude Code and Grok CLI, read from their local files (`sessions` under `$CODEX_HOME` or `~/.codex`, `$CLAUDE_CONFIG_DIR/projects` or `~/.claude/projects`, and `~/.grok`).

Ask it things like "what was the last thing I asked ChatGPT? send the image". Agent Tincan's operator prompt routes history questions to it.

#### How it joins

```bash
tincan invite history --kind history                                        # relay machine (or an admin device)
TINCAN_CONFIG=~/.config/tincan/history.json tincan join <code> --relay http://tincan-relay
```

It refuses to start unless the relay confirms it is the `history` agent, so a config for another agent can never claim its requests.

#### How it wakes

Wait: the service long-polls the relay. Set `{ "history": { "method": "wait" } }` in `wake.json`.

#### How it sends and receives

For each request it:

1. Checks the allowlist. By default there is no allowlist file and every agent joined to your relay may ask (the relay only delivers requests from joined agents). To restrict it, write `~/.config/tincan/history-allow.txt` with the agent names that may ask; then every agent in the request's chain, as the relay recorded it, must be listed. If muse asks codex and codex asks history while handling muse's request, a file that lists codex but not muse declines it because of muse. A `*` entry in the file means every joined agent. The file is reread for every request; an unreadable file or a bad name declines everyone.
2. Runs a tool-less query step: one `codex exec` call that sees only the question text and turns it into a structured query (source, mode, search terms, conversation id, count, `want_images`, and `with_images`, which picks the most recent turn that had images rather than the most recent turn). It runs read-only, with no MCP servers, no tools and no session file, and its output is checked against a schema in Go.
3. Reads the source. By default lookups cover the 50 most recent conversations per source, up to 30 days old. Only the owner can change that window, with `~/.config/tincan/history-window.json` (`{"days": N, "max": N}`, reread for every request); a file that is present but not valid fails every request until it is fixed.
4. Fills in a fixed reply template in Go and attaches up to 8 images. Retrieved chat content is never sent to a model, so text inside the owner's chats cannot steer the service.

The same readers are on the CLI: `tincan history <chatgpt|claude-ai|grok|gemini|copilot|codex|claude-code|grok-cli>` with `--latest`, `--list N`, `--search`, `--id`, `--all`, `--json`, `--images-dir`, and `--days N` and `--max N` for the window.

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

- It is the most sensitive agent on the mesh: by default every joined agent can read the owner's chat history. Write `~/.config/tincan/history-allow.txt` to narrow that to the agents you trust with it. The allowlist governs requests to the history agent, not local shell access; an agent with a shell on the owner's machine (such as the Codex or grok-cli wake) can read local Codex, Claude Code and Grok CLI history directly.
- Live sources need Chrome running, the extension connected, and the owner logged in; otherwise the reply says the source is unavailable and local sources still work. Chrome is never quit or restarted.
- A query the step cannot place gets "Please ask a clearer question naming ChatGPT, claude.ai, Grok, Gemini, Copilot, Codex, Claude Code or Grok CLI".
- "Grok" means grok.com and "Grok CLI" means Grok Build sessions on this machine (`~/.grok`); grok-cli wake runs are left out unless `--all`.
- When the window cut an answer short, the reply adds one line saying so (on the CLI, a note on stderr). ChatGPT, claude.ai, Grok, Gemini and Copilot read at most 100 conversations whatever the window says.

#### Adapter doc

[docs/adapters/history.md](docs/adapters/history.md)

### The web agents (chatgpt-web, claude-web, grok-web, gemini-web, perplexity-web and copilot-web)

#### What it is

A Go service, `tincan web serve --site chatgpt` (or `--site claude-ai`, `--site grok`, `--site gemini`, `--site perplexity`, `--site copilot`), that makes chatgpt.com, claude.ai, grok.com, gemini.google.com, www.perplexity.ai or copilot.com a teammate. `tincan ask chatgpt-web "..."` comes back with ChatGPT's answer and any images it generated attached. It types into the owner's logged-in account, as the owner, so the chats show in the owner's ChatGPT, Claude, Grok, Gemini, Perplexity or Copilot history, count against the owner's plan, and follow the site's own memory, custom instructions and model choice. xAI's terms prohibit automated access to Grok: grok-web acts as the owner on the owner's own account, one request at a time at a human pace, but xAI can still limit or suspend the account, so turn it on only if you accept that.

#### How it joins

```bash
tincan invite chatgpt-web --kind chatgpt-web                                        # relay machine (or an admin device)
TINCAN_CONFIG=~/.config/tincan/chatgpt-web.json tincan join <code> --relay http://tincan-relay
```

For Claude use `claude-web`, `--kind claude-web` and `~/.config/tincan/claude-web.json`; for Grok, `grok-web`, `--kind grok-web` and `~/.config/tincan/grok-web.json`; for Gemini, `gemini-web`, `--kind gemini-web` and `~/.config/tincan/gemini-web.json`; for Perplexity, `perplexity-web`, `--kind perplexity-web` and `~/.config/tincan/perplexity-web.json`; for Copilot, `copilot-web`, `--kind copilot-web` and `~/.config/tincan/copilot-web.json`; for your OpenAI dot, `dot-web`, `--kind dot-web` and `~/.config/tincan/dot-web.json`. A relay older than the `grok-web`, `gemini-web`, `perplexity-web` or `copilot-web` kind refuses it and the CLI says so: upgrade the relay, or invite without `--kind` and use `tincan onboard --kind <name>=<kind>`. `dot-web` is the exception: it needs the kind on the relay for its default hold and refuses to start without it, so upgrade the relay first. Like history, it refuses to start unless the relay confirms its name.

#### How it wakes

Wait: the service long-polls. Set its method to `wait` in `wake.json`.

#### How it sends and receives

For each request, one at a time:

1. Checks the allowlist exactly like history: with no file, every joined agent may ask; `~/.config/tincan/chatgpt-web-allow.txt`, `claude-web-allow.txt`, `grok-web-allow.txt`, `gemini-web-allow.txt`, `perplexity-web-allow.txt` or `copilot-web-allow.txt` restricts it to the listed names, and every agent in the chain must be listed.
2. Reads the optional threading line. A first line `new chat` starts a new conversation; `conversation: <id>` (or a conversation URL) continues that one; otherwise it continues the conversation this asker used last with this agent. Each asker has its own thread. The ids live in `~/.config/tincan/<agent>-state.json` (0600, ids only). Every reply ends with the conversation id so the asker can come back.
3. Has the extension type the message into a background tab the extension opens itself (`active: false`). The extension fills the message box, clicks send, and returns once the conversation id is in the tab's address (at most 60 seconds). It never touches a tab the owner opened.
4. Decides completion from the conversation data, not the page: it reads the conversation through the same detail operation the history agent uses (first 5 seconds after the send, then 5, 8 and 12 seconds apart, then every 20 seconds), finds this request's own user message, and waits for the answer after it (ChatGPT: any message in the turn marked end of turn, which covers image turns whose last message is hidden; claude.ai: a `stop_reason`, or the same text on 4 reads spanning at least 10 seconds; Grok: the answer marked `partial: false` with nothing left in flight; Gemini: the same text on 4 reads spanning at least 45 seconds, since Gemini pauses while it thinks; Perplexity: the entry marked `COMPLETED` with its answer `DONE`; Copilot: the same text on 3 reads spanning at least 20 seconds). The wait is bounded by the 8 minute request timeout. An HTTP 429 waits the site's `Retry-After` or backs off from 30 seconds up to 5 minutes, and a cooldown makes the next requests fail at once with "ChatGPT is rate-limiting this account right now; try again later" instead of hitting the site again. A rate limit that ends the wait after the send says the message was sent, names the conversation, and asks for the reply later instead of sending again; so does a Grok answer that ends on the plan's usage limit, which also holds Grok back for 15 minutes. Each site's cooldown is its own. While it waits, the agent keeps its relay presence fresh without claiming new requests.
5. Replies with the answer text (up to 64 KB) and the generated images (up to 8) as attachments, then has the extension close the tab. A Grok or Gemini image the extension cannot fetch is left out and the reply says so. A Perplexity or Copilot reply adds `Sources:` after the answer, one line per source (up to 10, duplicates dropped): `- [n] <title> <url>` for Perplexity, numbered as the answer's `[n]` markers, and `- <title> <url>` for Copilot, which gives no numbers. The list is kept whole inside the 64 KB cap. A tab nobody closes is closed after 10 minutes.

Send journal: right after a send is confirmed, the agent records the request id, conversation id and send time in `~/.config/tincan/<agent>-journal.json` (0600, no message text). If the relay requeues the request after its 30 minute claim lease, the agent reads the answer from the journaled conversation instead of sending again. Entries are dropped after 90 minutes.

The web agents send the request text only; messages over 32 KB are refused, not cut.

#### One-time setup

```bash
tincan history install --no-service      # skip if history is already installed
tincan web install --site chatgpt
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.agenttincan.web.chatgpt.plist
```

For Claude: `tincan web install --site claude-ai` and `com.agenttincan.web.claude-ai.plist`. For Grok: first grant Grok on the extension's options page (`chrome://extensions` > Agent Tincan History > Details > Extension options), then `tincan web install --site grok` and `com.agenttincan.web.grok.plist`; `tincan web serve --site grok` refuses to start while the extension is connected without that grant. For Gemini: grant Gemini on the extension's options page first, then `tincan web install --site gemini` and `com.agenttincan.web.gemini.plist`. For Perplexity: grant Perplexity on the extension's options page first, then `tincan web install --site perplexity` and `com.agenttincan.web.perplexity.plist`. For Copilot: grant Copilot there first and sign in once at https://copilot.microsoft.com with a personal Microsoft account, then `tincan web install --site copilot` and `com.agenttincan.web.copilot.plist`. For your dot: sign in to chatgpt.com, open the dot's DM and copy the thread id from its address (`chatgpt.com/dots/<thread-id>`), then `tincan web install --site dots --thread <thread-id>` (or run it directly with `tincan web serve --site dots --name dot-web --thread <thread-id>`); no extra grant is needed. `tincan web install` writes the launchd agent (or a systemd user unit on Linux) and prints the start command; it starts nothing. Logs go to `~/Library/Logs/tincan-chatgpt-web.log` (or `tincan-claude-web.log`, `tincan-grok-web.log`, `tincan-gemini-web.log`, `tincan-perplexity-web.log`, `tincan-copilot-web.log`). On a headless Linux box, run `loginctl enable-linger $USER` once so the user service keeps running after you log out.

#### Limits and gotchas

- It acts as the owner, and by default any joined agent may ask it to. Write its allowlist file to restrict that. The answer can include what the site remembers about the owner, and it is untrusted model output: the web agents pass it back as is.
- The extension checks the site session before opening a tab, so a logged-out browser never sends anonymously.
- Gemini: Google's terms do not allow automated access, and Google's enforcement can reach the owner's whole Google account, not only Gemini. Gemini answers can draw on Google apps connected to the account (Gmail, Drive, Calendar), so any agent allowed to ask `gemini-web` can read that data; write `~/.config/tincan/gemini-web-allow.txt` or disconnect those apps. Its images are captured from the send tab, with a worker fetch as the fallback; when neither works the reply says the images could not be attached. See [web-agents.md](docs/adapters/web-agents.md#gemini).
- Perplexity: its terms do not allow automated use. Perplexity answers signed-out visitors, so the extension asks Perplexity's session endpoint for a signed-in user before it opens a tab, and checks the page again before typing. Perplexity is not a history source. See [web-agents.md](docs/adapters/web-agents.md#perplexity).
- Copilot: the Microsoft Services Agreement does not allow automated access, and Microsoft's enforcement can reach the owner's whole Microsoft account. It needs a personal Microsoft account; a sign-in, terms or work-account page in its tab means nothing is sent and the reply says what to finish in Chrome. A "Verification required" human check is never touched: the reply says to complete it in Chrome, and Copilot is held back for 5 minutes. See [web-agents.md](docs/adapters/web-agents.md#copilot).
- Your dot (dot-web): requests land in the dot's DM as your own messages, and the dot can act in the apps you connected to it, so asks and notifies to dot-web are held for your approval by default (`tincan held`, `tincan approve`, `tincan deny`); an `approval.json` entry for `dot-web` replaces that default. The dot asks teammates with a message whose first line is `@tincan ask <agent>`; answers come back as `[tincan-reply from <agent>]` messages. `~/.config/tincan/dot-web-send.txt` limits whom it may ask (no file means any joined agent). A paused dot fails with nothing sent, and a dot message that arrives after the reply went out is not delivered. See [web-agents.md](docs/adapters/web-agents.md#your-dot-dot-web).
- All six sites protect their send endpoints with anti-bot tokens only the real page can produce, which is why it drives a tab instead of calling an API. If a site changes its page, the selectors in `extension/send.js` need an update.
- The ChatGPT and Claude send operations need extension version 0.3.0 or later; Grok and Gemini need 0.4.0 and the site granted on its options page; Perplexity and Copilot need 0.5.0 and their grant.
- When the site is not granted, the reply names the options page (`permission_missing`). When the site shows an anti-bot check, the reply says to open the site in Chrome and complete it (`blocked`). A session that lands on a sign-in page is `not_logged_in`, and nothing is sent.

#### Adapter doc

[docs/adapters/web-agents.md](docs/adapters/web-agents.md)

## The Tincan Chrome extension

A Manifest V3 extension (in [extension/](extension/), named "Agent Tincan History" in its manifest) that lets the history agent read, and the web agents send to, ChatGPT, claude.ai and (once granted) Grok, Gemini, Perplexity and Copilot through the owner's own logged-in Chrome.

What it can do:

- Run a fixed set of operations for its native host (`tincan history native-host`): list, detail and file reads for chatgpt.com, claude.ai, grok.com and gemini.google.com, `perplexity.detail` for one www.perplexity.ai thread, `copilot.list` and `copilot.detail` for copilot.com, `chatgpt.send`, `claudeai.send`, `grok.send`, `gemini.send`, `perplexity.send` and `copilot.send`, `chatgpt.close`, `claudeai.close`, `grok.close`, `gemini.close`, `perplexity.close` and `copilot.close`, and `extension.reload`. Images come back as base64 in chunks of at most 384 KiB.
- Open, fill and close its own background tabs for sends.

What it cannot do:

- It accepts nothing outside that operation set and never runs code from a message or a page. A message is passed as data to a fixed function in an isolated content script and inserted as text.
- No cookie or token leaves the browser. The ChatGPT access token is read inside the extension's worker and stays there.
- It never scripts a tab the owner opened, and Chrome is never quit or restarted.
- Its permissions are limited to `nativeMessaging`, `alarms` and `scripting`, on chatgpt.com, `*.oaiusercontent.com` and claude.ai, plus grok.com and assets.grok.com only after the owner grants Grok, gemini.google.com and `lh3.googleusercontent.com` only after the owner grants Gemini, www.perplexity.ai only after the owner grants Perplexity, and copilot.com and copilot.microsoft.com only after the owner grants Copilot, on the extension's options page (optional host permissions, so installing or updating asks nothing for any of them).

Options page: `chrome://extensions` > Agent Tincan History > Details > Extension options lists each site, shows whether Chrome has granted it, and grants it with one click (Chrome asks you to confirm). ChatGPT and claude.ai are granted at install; Grok, Gemini, Perplexity and Copilot stay off until you grant them here. Every operation except close checks its site's grant first and fails with `permission_missing` otherwise, and a site that shows an anti-bot check fails as `blocked` instead of looking like a changed API. Version 0.4.0, in the v0.6.0 release, added the options page and the Grok and Gemini sites. Version 0.5.0, in the v0.7.0 release, adds the Perplexity and Copilot sites, and is pending Chrome Web Store review.

Install: once the Chrome Web Store listing is published, installing is one click through Chrome's standard permission dialog. Until the store listing is live, load it unpacked once:

1. Download `tincan-history-extension.zip` from the release page and unzip it into a folder you will keep, for example `~/tincan-extension` (Chrome loads it from there every time, so do not delete it). From a repo checkout, the `extension/` folder works the same way.
2. Open `chrome://extensions`, turn on Developer mode (top right), click Load unpacked, and pick that folder.
3. Run `tincan history install --extension-dir ~/tincan-extension` so Chrome can start the native host and the host can reload the extension when its files change.

The manifest carries a public key, so an unpacked load always gets the id `ciejooalclcpgpapboofdbbddphldhnh`. The native host accepts both that id and the Chrome Web Store build's (`goldflchpojcjmifnljlfkgoahjgeajn`), and when both builds are installed the store build wins automatically, so moving to the store version is just adding it from the store (then removing the unpacked copy whenever you like). Older installs pick up the store id the next time `tincan history serve` starts.

Self-reload: after the first load, updates need no Reload click. When the extension connects, it sends the native host its version and the sha256 of each file (hashed when its worker started). If `tincan history install` was run with `--extension-dir` (or from a repo checkout) and the files on disk differ, the host sends `extension.reload` and the extension calls `chrome.runtime.reload()`. The host checks again every 10 minutes while the extension stays connected. It waits while a send has a tab open, checking every 5 seconds for up to 5 minutes. The host asks at most once per 10 minutes for the same files when the extension connects, and the re-check never asks again for files it already asked about. A store install is never reloaded this way.

Why an extension is required: ChatGPT, claude.ai, grok.com, gemini.google.com, www.perplexity.ai and copilot.com offer no official API for reading your own chat history or sending as you, so the reads go through the sites' own endpoints with your existing browser session, and sends need the real page. An extension is the one way to do that inside your logged-in Chrome without exporting cookies or tokens. Chrome requires a person to click to install any extension, so that click is the one human step in the setup.

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

Kinds: `vm-webhook`, `e2b-email`, `proxy-sandbox`, `claude-code`, `chatgpt`, `hermes`, `openclaw`, `codex`, `gemini-cli`, `grok-cli`, `history`, `council`, `chatgpt-web`, `claude-web`, `grok-web`, `gemini-web`, `perplexity-web`, `copilot-web`, `scheduled`, `generic`. History, Council and the web agents are services, so their blocks carry setup only, no standing instructions of their own. The Council block also carries the lines to add to the standing instructions of each teammate that should convene one, and the operator prompt passes "put this to the council" to it. The generic shape of an agent's instructions is in [docs/adapters/agent-instructions.md](docs/adapters/agent-instructions.md).

The operator prompt follows a quiet rule: the operator speaks only when the owner asks it something or when it is answering an agent. Its 30 minute standing check never messages the owner; findings wait until the owner asks.

Several agents per machine: each agent on one machine gets its own invite and its own client config through `TINCAN_CONFIG`, for example Claude Code and Codex on the same Mac, or Hermes and OpenClaw on the same mini. Set that `TINCAN_CONFIG` on every tincan command the agent runs (join, MCP entry, listen, rejoin). `tincan join` refuses to overwrite a config that already names a different agent unless you pass `--replace`.

Self-healing rejoin: if an agent's machine is rebuilt with the same machine name (or that name with a `-1` style suffix), run `tincan rejoin --relay http://tincan-relay` on the new machine. The relay re-admits it as the old agent, with its queued requests still waiting, when the new node is untagged, owned by the same Tailscale login, and the old node is offline or gone. Add `--proxy` for a proxy sandbox and `--name <agent>` when the machine ran several agents. Every standing instruction tells the agent to run rejoin itself and never ask for an invite unless rejoin says the machine was never joined. Tagged machines need a new invite, and `tincan relay --no-auto-rebind` turns this off. Each re-admission is audited as a `rebind` event.

## Trust model

Joined agents trust each other fully: a request from a joined agent is acted on as if you asked, with no per-request approval unless you turn on the [owner approval gate](#owner-approval) for chosen agents. The one default hold is Council: agents' councils wait for your approval, since a council sends your question to every model vendor on the team. Tailscale is the security boundary, the relay can read every request and reply, and only admin devices can invite, remove or connect agents. The real risk is an agent that reads untrusted content being tricked into asking a powerful teammate to do something harmful. Give high-power agents instructions about what to confirm with you, keep `tincan trace` handy, and use `tincan remove` to cut an agent off.

Read [docs/trust-model.md](docs/trust-model.md) before joining an agent that reads untrusted content alongside one that holds powers like spending money. It also covers attachments, the history agent, the web agents, and Council.

## Build, test, release

```bash
make build            # static ./tincan, CGO_ENABLED=0
make test             # go test -race ./...
make vet              # go vet ./...
make extension-test   # node --test for the extension worker code (no dependencies)
make extension        # dist/tincan-history-extension.zip
make dist             # every release asset in dist/ (see below)
make release VERSION=x.y.z NOTES=<file>   # the whole release (see below)
```

`make build` builds one binary, for the machine you run it on. CI runs `go vet` and `go test -race` on Linux and macOS, and checks the static builds for linux/amd64, linux/arm64, darwin/arm64 and darwin/amd64.

Releases are on the GitHub repo's release page. Each carries `tincan_darwin_arm64`, `tincan_darwin_amd64` (Intel Mac), `tincan_linux_amd64`, `tincan_linux_arm64`, `checksums.txt` (sha256 of the four binaries) and `tincan-history-extension.zip`. The binaries and `checksums.txt` are what the relay's `--dist` directory takes (add a `VERSION` file).

Releases are cut on the maintainer's Mac; CI does not publish them. One command does the whole release from a clean checkout of `main`:

```bash
make release VERSION=0.8.0 NOTES=release-notes.md DRY_RUN=1   # print every step, run none
make release VERSION=0.8.0 NOTES=release-notes.md
```

It first checks that `VERSION` is `x.y.z` (or `x.y.z-rc1`, released as a prerelease), the notes file exists, the working tree is clean, `HEAD` is `main` on the remote, and the tag exists neither locally nor on the remote. Then, in order:

1. `git tag -a vX -m vX`, locally only;
2. `make dist`, `make sign-mac notarize-mac`, `make checksums`, `shasum -a 256 -c checksums.txt` in `dist/`, and `make store`;
3. `tincan release-tools cws-upload --dry-run`, which checks the Chrome Web Store credentials and version before anything is public;
4. `git push <remote> refs/tags/vX`;
5. `gh release create vX --verify-tag --notes-file <notes>` with `checksums.txt`, the extension zip and the four binaries;
6. `tincan release-tools cws-upload --publish` with the store zip ([docs/chrome-web-store.md](docs/chrome-web-store.md#updating-through-the-api)).

If a step fails before the push, the local tag is deleted, so a rerun starts clean. The extension is versioned apart from tincan, so the store upload is skipped, and the release still succeeds, when `extension/manifest.json` is not newer than the store's version. Settings: `RELEASE_REMOTE` (default `origin`; a URL works, for a clone whose `origin` cannot push), `RELEASE_BRANCH` (default `main`; empty releases any commit), `SIGN=0` to skip macOS signing, `CWS=0` to skip the store.

The steps also work one at a time. `make dist` stamps the version from `git describe` (or `VERSION=`), so tag first. It needs no certificate; `make release-mac` adds the macOS signing on a Mac that has one:

- `make sign-mac` signs each `dist/tincan_darwin_*` with `codesign --force --options runtime --timestamp` using `TINCAN_SIGN_IDENTITY` (default: the maintainer's Developer ID Application identity), then rewrites `checksums.txt`, since signing changes the bytes.
- `make notarize-mac` zips each signed binary, submits it with `xcrun notarytool submit --keychain-profile "$TINCAN_NOTARY_PROFILE" --wait` (default profile `agentcookie-notary`), requires `Accepted`, and checks `spctl -a -vv -t install` reports `Notarized Developer ID`. A bare Mach-O binary cannot be stapled; Gatekeeper looks the ticket up online at first launch.
- One-time notary setup: `xcrun notarytool store-credentials <profile> --apple-id <apple-id> --team-id <team-id>` with an app-specific password. The credentials live in the login keychain, never in the repo.

Always upload the `checksums.txt` written after signing; `make release` and `make release-mac` rewrite it last and check it.

Quick start: [docs/quickstart.md](docs/quickstart.md). Protocol: [docs/protocol.md](docs/protocol.md).

MIT licensed.
