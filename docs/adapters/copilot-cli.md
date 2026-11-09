# GitHub Copilot CLI (copilot-cli)

The `copilot-cli` teammate is GitHub Copilot CLI (`copilot`) running as a coding agent on your machine, woken the way Codex and Grok CLI are: `tincan listen --exec` runs a wake script whenever teammates' requests are waiting, and the script starts one headless run that drains the inbox and exits. It uses your own GitHub Copilot subscription.

`copilot --version` must print `GitHub Copilot CLI X.Y.Z` (`TINCAN_COPILOT_VERSION_PATTERN` to override); anything else is refused once, with a backoff and a notice to the operator.

## Why a wake home

Copilot CLI keeps its config, MCP servers (`mcp-config.json`), plugins, hooks, trusted folders and sessions in `~/.copilot`, and honors `COPILOT_HOME` to put them elsewhere. The wake runs Copilot with `COPILOT_HOME=<wake home>/.copilot`, so none of your own MCP servers, plugins or hooks load in a wake run. The default wake home is `<name>.wake` beside the teammate's tincan config, `~/.config/tincan/copilot-cli.wake`; set `TINCAN_COPILOT_WAKE_HOME` for another. The wake refuses a wake home that is your own home.

`HOME` stays your home, so `git`, `gh` and `tincan` see your normal config.

## Join

On an admin device:

```bash
tincan invite copilot-cli --kind copilot-cli
```

(On a relay that does not know the `copilot-cli` kind yet, invite without `--kind`.) On the machine that runs Copilot, with a config of its own:

```bash
TINCAN_CONFIG="$HOME/.config/tincan/copilot-cli.json" tincan join <code> --relay http://<relay>:8787
```

## Setup

There is no MCP setup: the wake writes the agent-tincan server itself before every run, to `mcp-config.json` in its state directory, running the `tincan` on the listener's `PATH` (or `TINCAN_BIN`) with `mcp` and this teammate's `TINCAN_CONFIG`, and hands it to Copilot with `--additional-mcp-config`. Keep the wake home's own `.copilot/mcp-config.json` empty, or list what you add there in `TINCAN_WAKE_ALLOWED_SERVERS`.

Log in once for the wake home, since your normal `~/.copilot` login may not carry over:

```bash
COPILOT_HOME="$HOME/.config/tincan/copilot-cli.wake/.copilot" copilot login
```

Or put a token in the listener's environment as `COPILOT_GITHUB_TOKEN` (a launchd plist or systemd unit readable only by you), never in chat and never in wake.json. A run that fails for authentication backs off at once, leaves requests queued and tells the operator teammate once (`TINCAN_WAKE_OPERATOR`).

## Wake

Copy the wake script and the shared wake library into one folder (they are not in the release downloads):

```bash
mkdir -p ~/bin
cp examples/copilot-cli/copilot-wake.sh examples/lib/tincan-wake-lib.sh ~/bin/   # from a repo checkout
chmod +x ~/bin/copilot-wake.sh
```

Keep a listener running on that machine (launchd, systemd or a terminal):

```bash
TINCAN_CONFIG="$HOME/.config/tincan/copilot-cli.json" tincan listen --exec ~/bin/copilot-wake.sh
```

Set the teammate's wake to `command` in the relay's `wake.json`:

```json
{ "copilot-cli": { "method": "command" } }
```

Each nudge, the script:

1. Passes on the relay's upgrade notice, takes the wake lock and exits early while a backoff is in force, as every command wake does.
2. Checks that `copilot --version` is GitHub Copilot CLI, run with the wake's `COPILOT_HOME`.
3. Writes the agent-tincan server file and pins identity: the wake home's `mcp-config.json` and the wake's file together must hold exactly one agent-tincan server, with this teammate's `TINCAN_CONFIG`, and nothing outside `TINCAN_WAKE_ALLOWED_SERVERS`. Reading them needs `jq` or `python3` (`TINCAN_COPILOT_JSON_TOOL` picks one).
4. Picks a new session id and records it, with the workdir, in `~/.config/tincan/copilot-cli.wake-sessions` (mode 600) before the run, so the history agent leaves the run out even if the timeout kills it.
5. Runs Copilot in the workdir (`TINCAN_COPILOT_WORKDIR`, default `<wake home>/work`), with `COPILOT_ALLOW_ALL` cleared from the environment:

   ```
   copilot -p <prompt> -s --disable-builtin-mcps --additional-mcp-config @<state>/mcp-config.json \
     --session-id <id> --allow-tool 'agent-tincan(ask)' --allow-tool 'agent-tincan(check_inbox)' ...
   ```

   with one `--allow-tool` for each Agent Tincan tool: ask, get_reply, check_inbox, claim, progress, reply, answer, cancel, list_agents, onboard, get_attachment, trace, search, lease and list_leases.
6. Kills the run and everything it started after `TINCAN_WAKE_TIMEOUT` seconds, counts failures and backs off after three in a row, telling `TINCAN_WAKE_OPERATOR` once.

The lock, failure count, backoff marker and the generated server file live in `<name>.wake-state` beside the teammate's config (mode 700). `TINCAN_WAKE_STATE_DIR` overrides it.

## Security posture

Copilot's tool approval is an allowlist. The wake never passes `--allow-all-tools`, `--allow-all`, `--yolo` or `--allow-all-paths`, and clears `COPILOT_ALLOW_ALL`. In `-p` mode a tool call that is not allowed is denied, not prompted (the live check confirmed this for a sibling MCP tool), so a wake run is meant to use the Agent Tincan tools and read files in its workdir, and not edit files, run shell commands or fetch URLs; the prompt tells it to say so in its reply when a request needs those. That file writes and shell commands are denied the same way has not been checked live. The built-in GitHub MCP servers are off (`--disable-builtin-mcps`).

The identity check starts Copilot wired only to this teammate, which guards against misconfiguration. The requests it acts on come from joined teammates, who are fully trusted (see [docs/trust-model.md](../trust-model.md)). The wake runs on your Copilot subscription; every wake is a session like one you started yourself.

## Doctor

`tincan doctor` reads `$COPILOT_HOME/mcp-config.json` (`~/.copilot/mcp-config.json` by default), so an agent-tincan server you added for your own Copilot sessions is checked like any other app's. The wake's own server file is regenerated every run and is not read.

## Limits

- Every wake is a fresh session: the whole exchange has to finish inside one run.
- Wake runs are not given file writes or shell commands. Widening that (for example `--allow-tool write`) is not offered.

## Verified

Live check, 2026-10-15, macOS arm64, `@github/copilot` 1.0.98:

- Headless `copilot -p "<prompt>" -s` answered with the owner's existing login.
- `--disable-builtin-mcps` dropped the built-in GitHub MCP servers.
- `--additional-mcp-config @<file>` added a server from a JSON file shaped `{"mcpServers":{"<name>":{"type":"local","command":...,"args":[...],"tools":["*"]}}}`.
- `--allow-tool '<server>(<tool>)'` allowed exactly that MCP tool; a sibling tool on the same server was denied.
- Sessions are stored in `~/.copilot/session-state/<uuid>/` and `~/.copilot/session-store.db`.
- `copilot --help` documents `COPILOT_HOME` (config and state directory, default `$HOME/.copilot`) and `--session-id` (sets the UUID of a new session).

Not yet verified live: a run under a separate `COPILOT_HOME` (whether the owner's login carries over or needs `copilot login` there), and a full wake answering a teammate's ask. The script tests run against a fake `copilot`. Record the first live wake here before relying on it.
