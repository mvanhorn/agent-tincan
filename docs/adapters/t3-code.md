# T3 Code (`t3-code`)

T3 Code can join your Agent Tincan team as a two-way teammate (`tincan t3 serve`). It bridges the Agent Tincan relay and T3 Code's external `/mcp` server using T3 Code's tools and SEP-3415 MCP Events (`events/stream` with `events/poll` catch-up).

Verified live against T3 Code Nightly `0.1.13-nightly.20261125.5258`.

## What it does

1. Dispatched coding work: any teammate on your tailnet can ask `t3` to run a task. The bridge launches a real T3 Code thread (matching `--project <name>` or a `project: <name>` first line against `t3_project_list`, else `--default-project` or Scratch), uploads any Tincan attachments, posts a progress note with the thread link, and replies when `thread.run_settled` fires.
2. Mid-run questions vs permission prompts: when a dispatched thread emits `thread.needs_input` for an answerable user question (`t3_pending_request` `list`), the bridge relays it via Tincan `needs_input` and forwards the asker's `tincan answer` back via `t3_pending_request` `respond`. T3 permission and approval prompts are never approved over Agent Tincan; the bridge notes that owner approval is required directly in T3 Code and waits for `thread.input_resolved`.
3. Opt-in interactive thread babysitting: you (`tincan t3 watch <thread-url-or-id> --notify <agent>`) or a teammate (`tincan ask t3 "watch <thread-url-or-id>"`) can subscribe an existing interactive T3 thread to notify on `thread.run_settled`, `thread.needs_input`, and `pr.watch_wake`. If the thread already settled before the watch started, the bridge answers right away.
4. Outbound from every T3 provider with late-reply routing: `tincan t3 install` wires `agent-tincan` (`tincan mcp` pinned to `t3.json`) into T3 Code's configured providers (T3-isolated Antigravity profiles under `~/.t3/userdata/providers/antigravity/*/config/mcp_config.json`, Claude custom `homePath`s from `~/.t3/userdata/settings.json`, OpenCode `~/.config/opencode/opencode.json`, and Codex `~/.codex/config.toml`). Existing standalone `agent-tincan` entries (such as `claude-code` in `~/.claude.json`) are left untouched unless you pass `--override-shared`. When any T3 thread sends an ask (`T3CODE_THREAD_ID` in the environment) and stops waiting before the reply lands, `tincan t3 serve` delivers the reply back into that exact T3 thread via `t3_thread_send` (`mode: "auto"`).

## Setup

Upgrade the relay to v0.34.0 first so it accepts the `t3-code` kind.

1. Invite the `t3` teammate from an admin device:

```bash
tincan invite t3 --kind t3-code
```

2. Join on the machine running T3 Code, with its own config:

```bash
TINCAN_CONFIG=~/.config/tincan/t3.json tincan join <code> --relay http://tincan-relay
```

3. Authorize Agent Tincan against T3 Code's `/mcp` server:

```bash
tincan t3 auth --config ~/.config/tincan/t3.json
```

This runs OAuth 2.1 PKCE against T3 Code's local origin (discovered from `~/.t3/userdata/server-runtime.json`, or pass `--t3-url https://<host>` for a remote T3 host), opens T3 Code's approval page so you choose the runtime access ceiling, verifies `events/list`, and saves the bearer token at mode `0600` in `~/.config/tincan/t3-auth.json`. For headless setups with an existing `0600` bearer token file, pass `--token-file <path>`.

4. Wire T3's providers and install the background service:

```bash
tincan t3 install --config ~/.config/tincan/t3.json --start
```

On macOS this writes `~/Library/LaunchAgents/com.agenttincan.t3.plist` (logging to `~/Library/Logs/tincan-t3.log`) and loads it. On Linux it writes `~/.config/systemd/user/tincan-t3.service`. Pass `--wire-providers=false` to skip editing provider MCP configs, or `--override-shared` to repoint shared `~/.claude.json` / `~/.codex/config.toml` entries to `t3.json`.

5. Check everything with:

```bash
tincan t3 doctor --config ~/.config/tincan/t3.json
```

6. On the relay host, set `t3` to `{"method": "wait"}` in `wake.json` (no secret fields).

## Directives

Teammates can address `t3` with plain prompts (launches a new T3 thread) or deterministic first-line directives:

- `project: <name>` followed by a newline and prompt: launches a new thread in the matching T3 project (matched by ID, title, or workspace folder basename). `--project <name>` on `tincan ask` works the same way.
- `watch <thread-url-or-id> [note]`: opts an existing T3 thread into notifications on settle, input, or PR watch wake. Accepts raw thread IDs, `https://app.t3.codes/threads/<env>/<id>`, or `t3-thread://` links.
- `unwatch <thread-url-or-id>`: removes your watches on that thread.
- `status <thread-url-or-id>`: reads the thread's current status, pending request count, and latest assistant message.
- `thread: <thread-url-or-id>` (followed by message) or `send <thread-url-or-id>: <message>`: sends a follow-up message into an existing T3 thread and replies when that turn settles.

## Shell commands

- `tincan t3 watch <thread-url-or-id> --notify <agent> [--events settled,needs_input,pr_watch] [--note "..."] [--once]`
- `tincan t3 unwatch <thread-url-or-id> [--notify <agent>]`
- `tincan t3 doctor [--json]`

## Trust and limits

- Credentials: `~/.config/tincan/t3-auth.json` is stored `0600` in a `0700` directory and is never logged, printed by `doctor`, or included in error text. Outbound HTTP to T3 Code refuses redirects and requires `https://` for any non-loopback `--t3-url`.
- Permission prompts: `tincan t3 serve` never approves T3 tool or permission prompts over Agent Tincan.
- Guests: requests marked `FromGuest` are declined on `watch`, `unwatch`, `status`, and `thread:`/`send` directives with zero T3 reads. Ordinary guest asks launch isolated Scratch threads with Tincan's guest framing.
- Laptop sleep: if T3 Code runs on a laptop that sleeps overnight, both T3 threads and `tincan t3 serve` pause until the machine wakes (saved SEP-3415 cursors catch up via `events/poll` on wake). For unattended overnight runs, keep the Mac awake or point `--t3-url` at an always-on T3 host.
- Token expiry: T3 Code MCP OAuth sessions live 30 days by default. `tincan t3 doctor` warns when expiry is within 72 hours; re-run `tincan t3 auth` to refresh.
