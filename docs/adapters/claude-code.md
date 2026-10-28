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

That gives the session the tools (`ask`, `check_inbox`, `reply`, `get_reply`, `list_agents`, `cancel`, `claim`, `trace`, `onboard`, `get_attachment`) and, with `--channel`, pushes teammate requests straight into the running session. Without `--channel`, it wakes an idle session through the session's own socket instead (below).

## Receive requests without typing (channels)

Channels are a Claude Code research preview. Custom channels load with the development flag:

```bash
claude --dangerously-load-development-channels server:agent-tincan
```

When teammate requests, or replies to this session's own requests, are waiting, the channel pushes a short notice such as `<channel source="agent-tincan" kind="request" count="1" from="instinct" request_ids="...">1 Agent Tincan item waiting from instinct. Call check_inbox to take it, then reply to each request.</channel>`. The notice never carries the items and never claims anything: Claude calls `check_inbox`, which claims the requests, then handles them and calls `reply`. `kind` is `request`, `reply`, or `mixed`.

Every open Claude Code session runs its own `tincan mcp --channel`. Each one names its own session to the relay (a fresh id per process, or `TINCAN_SESSION` when set), so a reply goes only to the window that asked: only that window gets the notice, and `check_inbox` in another window does not take it. A reply to a window that closed, or whose tincan server restarted (after `tincan upgrade` or `/mcp`, say), goes to the next window that checks once the old one has been quiet for 10 minutes; `tincan get <id>` or `tincan inbox` in a terminal reads it at any time. New teammate requests still go to the agent, so every session gets that notice. The first one to call `check_inbox` takes the requests; the others find an empty inbox. A session that was started without the channels flag, or is idle, simply drops the channel notice and the items stay queued for someone else. Each process announces an item once, and again after 10 quiet minutes if it is still waiting.

Notes:

- Channels only deliver while the session is open. Keep a Claude Code session running in cmux.
- If Claude hits a permission prompt while you are away, the session waits. Pre-approve the tincan tools with `/permissions` (allow `mcp__agent-tincan__*`).
- The tincan channel offers MCP revisions up to 2025-11-25 only, because Claude Code does not register a channel server that negotiates 2026-07-28.

## Receive requests without channels (socket wake)

Without `--channel`, `tincan mcp` wakes the session through Claude Code's own messaging socket, and no preview flag is needed:

```bash
claude mcp add --scope user agent-tincan -- tincan mcp
```

The server reads the socket path and token Claude Code gives it in its environment, and pushes only after checking that its parent process is the session registered for that socket in `~/.claude/sessions/<pid>.json` (under `$CLAUDE_CONFIG_DIR` when set). A `tincan` run from the session's Bash tool inherits the same variables but fails that check, so it does not push into the session. The check guards against that mistake, not against other programs the session runs, which inherit the token too. If the check fails at start, the server logs why and runs without a wake.

When teammate requests, or replies to this session's own requests, are waiting and the session is idle, the server writes one user message with the channel notice's text, such as `1 Agent Tincan item waiting from instinct. Call check_inbox to take it, then reply to each request.`, and that starts a turn. It carries only the count and senders; Claude calls `check_inbox` and `reply` as with a channel notice. A busy session is left alone until it is idle. Each message spends a turn, so:

- Each item is sent once, with no re-announce after 10 minutes. It still shows on the next `check_inbox`.
- A reply to an ask goes only to the window that sent it.
- A teammate request starts one turn per agent per machine, however many windows are open. The windows share a lock and a delivery record in `socket-wake/` beside the agent's config, and the first idle window takes the item.
- Upgrade notices are not sent this way; they show on the next tool call.

While socket wake runs, the session tells the relay so, and a teammate's pending ask says `claude-code is listening now; a session will be prompted when it is idle.` The roster's `wake` stays `channel`. After three failed writes in a row the server stops peeking, so the agent stops looking online, keeps its relay session live for reply routing, and checks the socket again after 5 minutes.

`--wake-socket=false` turns socket wake off. With `--channel` the channel runs and `--wake-socket` is ignored.

## Several sessions on one machine

Every Claude Code window on a machine runs as the same agent. A teammate cannot pick one window, and one window cannot ask another: an `ask` to the agent's own name fails with `send: an agent cannot send to itself`. To reach another window on the same machine, use Claude Code's own messaging between sessions where it has one. To make a window addressable on the relay, join it as an agent of its own, with its own invite and `TINCAN_CONFIG`.

## Show what each session is working on

Add `--share-title` to the server command to show each session's title on the roster:

```bash
claude mcp add --scope user agent-tincan -- tincan mcp --channel --share-title
```

Teammates then see `working_on="Fix flaky relay test"` on this agent's `list_agents` and `tincan agents` line: the session's custom title, else the title Claude Code wrote for it. Only title records are read, not your prompts, but a title can still say what you asked: Claude Code's generated titles summarize the conversation, and a hook that names sessions from their first prompt turns that prompt into the custom title. It works with or without `--channel`. Each server re-sends its title every 2 minutes and removes it when it exits; a window that stops without a clean exit drops off within 10 minutes. With several windows open the line shows the most recently refreshed title and a count of the others, such as `(+2)`. It is off by default, and every joined agent can read it, so leave it off if your session titles name things the team should not see.

## Fallback: open a session in cmux

If no session is open to wake, or socket wake is off, run a listener that opens a new Claude Code session in cmux whenever requests are waiting. The script, `examples/claude-code/cmux-wake.sh`, lives in the agent-tincan repo (it is not in the release downloads); copy it to `~/bin` and `chmod +x` it first:

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
ID or the short address returned by cross-session discovery.

The registry's parent directory must already exist, be owned by you, have no
group/other permissions (normally `0700`), and have no symlinked path components.
Registry and lock files must be private `0600` regular files. The helper rejects
unsafe storage rather than changing existing permissions. Choose a dedicated
private registry directory if your existing Tincan config directory is shared.

Bind it once:

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
Claude Desktop and an eligible subscription login are required; the official
opener determines availability and the minimum Claude Code version. An accepted open
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
