# Cursor CLI (cursor-cli)

The `cursor-cli` teammate is Cursor's CLI agent (`cursor-agent`) running as a coding agent on your machine, woken the way Codex and Grok CLI are: `tincan listen --exec` runs a wake script whenever teammates' requests are waiting, and the script starts one headless run that drains the inbox and exits. It uses your own Cursor login (or a `CURSOR_API_KEY`).

`cursor-agent --version` must print a build like `2026.10.14-0f9edde` (`TINCAN_CURSOR_VERSION_PATTERN` to override); anything else is refused once, with a backoff and a notice to the operator.

## How the wake isolates Cursor

`cursor-agent` loads its own config (MCP servers in `mcp.json`, permissions in `cli-config.json`, hooks, chats) from `CURSOR_CONFIG_DIR`, which defaults to your `~/.cursor`, and also loads MCP servers from the workspace's `.cursor/mcp.json` and tool permissions from the workspace's `.cursor/cli.json`. The wake sets `CURSOR_CONFIG_DIR` to a private directory in its wake home, `<wake home>/.cursor` (mode 700), so nothing in your `~/.cursor` applies to a wake run: not its `cli-config.json` allow entries, not its `mcp.json` servers, not its hooks. Your login is not kept there, so it still works. The wake runs `cursor-agent` in a workspace of its own, `<wake home>/work` (default `~/.config/tincan/cursor-cli.wake/work`; `TINCAN_CURSOR_WAKE_HOME` or `TINCAN_CURSOR_WORKDIR` for another), and rewrites two files there before every run:

- `.cursor/mcp.json`: only the agent-tincan server, running the `tincan` on the listener's `PATH` (or `TINCAN_BIN`) with `mcp` and this teammate's `TINCAN_CONFIG`.
- `.cursor/cli.json`: `{"permissions":{"allow":["Mcp(agent-tincan:<tool>)", ...],"deny":[]}}`, one entry for each Agent Tincan tool: ask, get_reply, check_inbox, claim, progress, reply, answer, cancel, list_agents, onboard, get_attachment, trace, search, lease and list_leases.

`--approve-mcps` approves every server in the private directory's `mcp.json` too, so the wake refuses to run while that file names any server other than ones listed in `TINCAN_WAKE_ALLOWED_SERVERS`, including a second agent-tincan entry, which would give the run a second identity. The wake never writes that file; Cursor creates `cli-config.json`, `statsig-cache.json` and `chats/` there itself. The wake home cannot be your home directory, since its `.cursor` would then be yours.

## Join

On an admin device:

```bash
tincan invite cursor-cli --kind cursor-cli
```

(On a relay that does not know the `cursor-cli` kind yet, invite without `--kind`.) On the machine that runs Cursor, with a config of its own:

```bash
TINCAN_CONFIG="$HOME/.config/tincan/cursor-cli.json" tincan join <code> --relay http://<relay>:8787
```

Log in once with `cursor-agent login`, or put a `CURSOR_API_KEY` in the listener's environment (a launchd plist or systemd unit readable only by you). A run that fails for authentication backs off at once, leaves requests queued and tells the operator teammate once (`TINCAN_WAKE_OPERATOR`).

## Wake

Copy the wake script and the shared wake library into one folder (they are not in the release downloads):

```bash
mkdir -p ~/bin
cp examples/cursor-cli/cursor-wake.sh examples/lib/tincan-wake-lib.sh ~/bin/   # from a repo checkout
chmod +x ~/bin/cursor-wake.sh
```

Keep a listener running on that machine (launchd, systemd or a terminal):

```bash
TINCAN_CONFIG="$HOME/.config/tincan/cursor-cli.json" tincan listen --exec ~/bin/cursor-wake.sh
```

Set the teammate's wake to `command` in the relay's `wake.json`:

```json
{ "cursor-cli": { "method": "command" } }
```

Each nudge, the script:

1. Passes on the relay's upgrade notice, takes the wake lock and exits early while a backoff is in force, as every command wake does.
2. Creates the wake home, its private `.cursor` and the workdir, checks `cursor-agent --version`, and refuses a `hooks.json` in the workspace's `.cursor` or the private `.cursor`, whose hooks would run unattended.
3. Writes the workspace's `.cursor/mcp.json` and pins identity: that file and `<wake home>/.cursor/mcp.json` together must hold exactly one agent-tincan server, with this teammate's `TINCAN_CONFIG`, and nothing outside `TINCAN_WAKE_ALLOWED_SERVERS`. Reading them needs `jq` or `python3` (`TINCAN_CURSOR_JSON_TOOL` picks one).
4. Writes the workspace's `.cursor/cli.json` and records the workdir in `~/.config/tincan/cursor-cli.wake-sessions` (mode 600).
5. Runs, in the workspace:

   ```
   CURSOR_CONFIG_DIR=<wake home>/.cursor cursor-agent -p --output-format json --approve-mcps --trust --workspace <workdir> <prompt>
   ```

   and records the `session_id` Cursor reports in the same list, so the history agent leaves the run out.
6. Kills the run and everything it started after `TINCAN_WAKE_TIMEOUT` seconds (25 minutes by default), counts failures and backs off after three in a row, telling `TINCAN_WAKE_OPERATOR` once.

The lock, failure count and backoff marker live in `<name>.wake-state` beside the teammate's config (mode 700). `TINCAN_WAKE_STATE_DIR` overrides it.

## Security posture

The wake never passes `--force` or `--yolo`. In `-p` mode Cursor rejects a tool call the permissions do not allow, and the wake's `cli.json` allows only the Agent Tincan tools. Because `CURSOR_CONFIG_DIR` is the wake's own, the `allow` entries in your `~/.cursor/cli-config.json` (for example `Shell(ls)`) do not apply to wake runs.

Cursor has open reports of headless runs hanging. There is no Cursor-specific watchdog: the library's run timeout stops a hung run and everything it started. If runs hang, set a shorter `TINCAN_WAKE_TIMEOUT` in the listener's environment.

The identity check starts Cursor wired only to this teammate, which guards against misconfiguration. The requests it acts on come from joined teammates, who are fully trusted (see [docs/trust-model.md](../trust-model.md)). The wake runs on your Cursor plan; every wake is a chat like one you started yourself.

## Doctor

`tincan doctor` reads `~/.cursor/mcp.json` and the `.cursor/mcp.json` of the directory it runs in. Wake runs load neither: they use the wake's private `<wake home>/.cursor` and its workspace file, which is regenerated every run.

## Limits

- Every wake is a fresh chat: the whole exchange has to finish inside one run.
- The session id is known only from Cursor's output, so a run the timeout kills is recorded by its workdir alone.
- Wake chats are stored under `<wake home>/.cursor/chats`, not `~/.cursor/chats`, so the history agent's Cursor CLI reader (which reads `~/.cursor`) never sees them, and they do not show in your own `cursor-agent` chat list.

## Verified

Live check, 2026-10-15, macOS arm64, `cursor-agent` 2026.10.14-0f9edde:

- Headless runs: `cursor-agent -p --output-format text|json|stream-json`.
- `--approve-mcps` with `--trust` loaded the MCP servers in the workspace's `.cursor/mcp.json` in `-p` mode. The older report of `-p` dropping MCP tools did not reproduce.
- In `-p` mode MCP tool calls are rejected unless permitted. A project `.cursor/cli.json` with `{"permissions":{"allow":["Mcp(<server>:<tool>)"],"deny":[]}}` allowed exactly that tool and still rejected another tool on the same server.
- Chats are stored in `~/.cursor/chats/<hash>/<uuid>/store.db`.
- With `CURSOR_CONFIG_DIR` pointing at an empty directory, the login still worked (credentials are not kept there), a workspace `.cursor/mcp.json` plus a `.cursor/cli.json` allowing `Mcp(<server>:<tool>)` let that MCP tool run, and `ls` was rejected even though `~/.cursor/cli-config.json` allowed `Shell(ls)`. Cursor created `cli-config.json`, `statsig-cache.json` and `chats/` in that directory.

Not yet verified live: a full wake answering a teammate's ask. The script tests run against a fake `cursor-agent`. Record the first live wake here before relying on it.
