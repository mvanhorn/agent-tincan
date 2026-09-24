# Claude Code (in cmux or any terminal)

Claude Code joins as its own agent, for example `claude-code`, from the Mac it runs on. When the Mac is off, it shows as offline and requests to it wait in the queue.

## Join

On an admin device, `tincan invite claude-code`. On the Mac:

```bash
tincan join <code> --relay http://tincan-relay
```

## Add the MCP server

```bash
claude mcp add agent-tincan -- tincan mcp --channel
```

That gives the session the tools (`ask`, `check_inbox`, `reply`, `get_reply`, `list_agents`, `cancel`, `claim`, `trace`, `onboard`, `get_attachment`) and, with `--channel`, pushes teammate requests straight into the running session.

## Receive requests without typing (channels)

Channels are a Claude Code research preview. Custom channels load with the development flag:

```bash
claude --dangerously-load-development-channels server:agent-tincan
```

When teammate requests, or replies to this session's own requests, are waiting, the channel pushes a short notice such as `<channel source="agent-tincan" kind="request" count="1" from="instinct" request_ids="...">1 Agent Tincan item waiting from instinct. Call check_inbox to take it, then reply to each request.</channel>`. The notice never carries the items and never claims anything: Claude calls `check_inbox`, which claims the requests, then handles them and calls `reply`. `kind` is `request`, `reply`, or `mixed`.

Every open Claude Code session runs its own `tincan mcp --channel`, so every session gets the notice. The first one to call `check_inbox` takes the items; the others find an empty inbox. A session that was started without channels, or is idle, simply drops the notice and the items stay queued for someone else. Each process announces an item once, and again after 10 quiet minutes if it is still waiting.

Notes:

- Channels only deliver while the session is open. Keep a Claude Code session running in cmux.
- If Claude hits a permission prompt while you are away, the session waits. Pre-approve the tincan tools with `/permissions` (allow `mcp__agent-tincan__*`).
- The tincan channel offers MCP revisions up to 2025-11-25 only, because Claude Code does not register a channel server that negotiates 2026-07-28.

## Fallback: open a session in cmux

If channels are unavailable, run a listener that opens a new Claude Code session in cmux whenever requests are waiting:

```bash
tincan listen --exec ~/agent-tincan/examples/claude-code/cmux-wake.sh
```

It uses the same cmux control-plane calls as agentmail-to-claude-code, so cmux's `automation.socketControlMode` must allow it (see that repo's `setup_cmux.py`). The listener does not take the requests; the new session picks them up with `check_inbox`. Set this agent's wake to `command` in the relay's `wake.json`.
