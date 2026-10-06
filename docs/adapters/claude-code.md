# Claude Code (in cmux or any terminal)

Claude Code joins as its own agent, for example `claude-code`, from the Mac it runs on. When the Mac is off, it shows as offline and requests to it wait in the queue.

## Join

On an admin device, `tincan invite claude-code --kind claude-code`. On the Mac:

```bash
tincan join <code> --relay http://tincan-relay
```

## Add the MCP server

```bash
claude mcp add --scope user agent-tincan -- tincan mcp --channel
```

`--scope user` registers the server for every project. The default scope is the directory the command runs in, so a session started anywhere else would have no tincan tools and no channel.

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

If channels are unavailable, run a listener that opens a new Claude Code session in cmux whenever requests are waiting. The script, `examples/claude-code/cmux-wake.sh`, lives in the agent-tincan repo (it is not in the release downloads); copy it to `~/bin` and `chmod +x` it first:

```bash
tincan listen --exec ~/bin/cmux-wake.sh
```

It uses the same cmux control-plane calls as agentmail-to-claude-code, so cmux's `automation.socketControlMode` must allow it (see that repo's `setup_cmux.py`). The listener does not take the requests; the new session picks them up with `check_inbox`. Set this agent's wake to `command` in the relay's `wake.json`.

## Optional operator-bound project/task sessions

When an operator wants to reuse a particular conversation rather than choose the
latest conversation in a directory, the POSIX Python 3 helper
[`examples/claude-code/session-route.py`](../../examples/claude-code/session-route.py)
resolves **canonical project directory + explicit task key** to a bound Claude
conversation UUID. This is a local controller building block, not an automatic
Tincan inbox router. It changes no relay protocol, MCP registration or permissions.

Create or choose the conversation with Claude's normal interface first. Obtain
its actual conversation UUID (for example, from `/status`), not a Tincan request
ID or the short address returned by cross-session discovery. Bind it once:

The registry's parent directory must already exist, be owned by you, have no
group/other permissions (normally `0700`), and have no symlinked path components.
Registry and lock files must be private `0600` regular files. The helper rejects
unsafe storage rather than changing existing permissions. Choose a dedicated
private registry directory if your existing Tincan config directory is shared.

```bash
python3 examples/claude-code/session-route.py \
  --registry "$HOME/.config/tincan/claude-session-routes.json" \
  --project /path/to/project --task design \
  --bind 00000000-0000-4000-8000-000000000001
```

Replace the example UUID with the real one. Running the same command without
`--bind` returns the existing route as JSON. Other projects and task keys have
independent routes. Missing routes and conflicting rebinds fail: the helper
never silently creates a conversation, chooses the newest one, or substitutes
a different conversation when the bound one is unavailable.

On macOS, adding `--desktop` uses Claude's official
`claude --desktop --resume <conversation UUID>` opener. It opens Desktop, not a
second model executor. The helper supplies the terminal required by that opener;
it does not click, type into a window, or inject into private session sockets.
Claude Code v2.1.285 or newer, Claude Desktop, and an eligible subscription login
are required; the official opener determines availability. An accepted open
command is not proof of live CLI/Desktop synchronization or task execution.
See [Claude's Desktop documentation](https://code.claude.com/docs/en/desktop#coming-from-the-cli).

The operator must decide whether opening is needed. If the destination is
already open, use the host's supported messaging/channel path instead of
opening it again. Never run a headless `--resume` writer concurrently with a
Desktop writer. This helper deliberately does not implement live-session
discovery, message forwarding, automatic creation, or a semantic decision model.

Registry paths, project paths, task keys and bind operations must come from a
trusted local operator/controller, **not from peer request text**. A route is
not new authority to access a project or approve an action. Keep the registry
private; no prompts, credentials or chat histories belong in it. The existing
native Tincan tools still fetch and reply to actual requests, and the receiving
session's permissions and approval boundaries still apply. The shared-inbox
first-claim behavior described above is unchanged.

Regression tests require no Claude process, account, relay or LLM calls:

```bash
python3 -m unittest discover -s examples/claude-code -p 'test_session_route.py'
```
