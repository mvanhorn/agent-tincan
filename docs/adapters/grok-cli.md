# Grok command-line agent (grok-cli)

The `grok-cli` teammate is xAI's Grok Build CLI (`grok`) running as a coding agent on your machine, woken the way Codex is: `tincan listen --exec` runs a wake script whenever teammates' requests are waiting, and the script starts one headless run that drains the inbox and exits. It uses your own Grok account (a login, or an xAI API key).

It is a different teammate from `grok-web`, which types into grok.com in your browser ([web-agents.md](web-agents.md)), and from Grok Bot. The history agent reads both: "what did I ask Grok CLI" reads your Grok CLI sessions, "what did I ask Grok" your grok.com chats ([history.md](history.md#grok-cli)).

Only xAI's Grok Build works. `grok --version` must print `grok X.Y.Z (<commit>)`, for example `grok 1.0.40 (eb1a2256660d) [stable]`. The community `grok-cli` npm package installs a `grok` command too; the wake refuses it (it fails once, backs off, and tells the operator).

## Why a wake home

Grok Build loads MCP servers from its own `config.toml` and also from `~/.claude.json`, `~/.cursor/mcp.json`, a project's `.mcp.json` and Claude plugins. On a machine where you use Claude Code, that import includes Claude Code's own agent-tincan server, with Claude Code's identity, and every other server you have there. The wake runs Grok with tool approval off, so every server it loads is trusted. Turning the import off in `config.toml` (`[compat.claude] mcps = false`) did not remove those servers in testing.

So the wake runs Grok in a wake home of its own: `HOME` is the wake home and `GROK_HOME` is its `.grok` folder. With no `.claude.json`, Cursor config or plugins there, Grok finds only what the wake home's `.grok/config.toml` names: this teammate's tincan server. The default wake home is `<name>.wake` beside the teammate's tincan config, `~/.config/tincan/grok-cli.wake`; set `TINCAN_GROK_WAKE_HOME` for another. The wake refuses a wake home that is your own home.

Tools that read their config from `~` see the wake home during a run. `git` has no user name or email there and `gh` is not logged in unless you set them up in the wake home (or pass `GH_TOKEN` in the listener's environment, which the model can then read). The wake links `~/.config/tincan` in the wake home to your real `~/.config/tincan`, so the tincan commands in the standing instructions (`TINCAN_CONFIG=~/.config/tincan/grok-cli.json tincan ...`) reach the teammate's config.

## Join

On an admin device (the relay must know the `grok-cli` kind; on an older relay, invite without `--kind` and use `tincan onboard --kind grok-cli=grok-cli`):

```bash
tincan invite grok-cli --kind grok-cli
```

On the machine that runs Grok, with a config of its own:

```bash
TINCAN_CONFIG="$HOME/.config/tincan/grok-cli.json" tincan join <code> --relay http://tincan-relay
```

## Setup

Make the wake home and add the tincan server to its Grok config:

```bash
W="$HOME/.config/tincan/grok-cli.wake"
mkdir -p "$W/.grok"
HOME="$W" GROK_HOME="$W/.grok" grok mcp add agent-tincan \
  -e TINCAN_CONFIG="$HOME/.config/tincan/grok-cli.json" -- tincan mcp
```

(`$HOME` in the `-e` value is expanded by your shell before the command runs, so it is your real home.) That writes `~/.config/tincan/grok-cli.wake/.grok/config.toml`:

```toml
[mcp_servers.agent-tincan]
command = "tincan"
args = ["mcp"]

[mcp_servers.agent-tincan.env]
TINCAN_CONFIG = "/Users/you/.config/tincan/grok-cli.json"
```

MCP configs do not expand `~`, so `TINCAN_CONFIG` must be a full path; if the listener's `PATH` lacks `tincan`, give `command` its full path. Writing the file by hand works the same; keep `TINCAN_CONFIG` in double quotes on one line (the wake reads the entry as either an `[mcp_servers.agent-tincan.env]` table or `env = { TINCAN_CONFIG = "..." }`). Keep nothing else in that `config.toml` unless you list it in `TINCAN_WAKE_ALLOWED_SERVERS`.

Log in once, in the wake home (your normal `~/.grok` login is not used):

```bash
GROK_HOME="$HOME/.config/tincan/grok-cli.wake/.grok" grok login
```

Or put an xAI API key in the listener's environment as `XAI_API_KEY` (a launchd plist or systemd unit readable only by you), never in chat and never in wake.json. Without either, the wake fails once with a message naming `grok login`, backs off, and leaves requests queued. A run Grok fails for authentication (a login that expired and could not be refreshed, or a rejected key) backs off the same way at once and tells the operator teammate once (`TINCAN_WAKE_OPERATOR`).

Standing instructions go in `AGENTS.md` in the workdir (`~/.config/tincan/grok-cli.wake/work`), or in the prompt the listener passes.

## Wake

Copy the wake script and the shared wake library into one folder (they are not in the release downloads):

```bash
mkdir -p ~/bin
cp examples/grok-cli/grok-wake.sh examples/lib/tincan-wake-lib.sh ~/bin/   # from a repo checkout
chmod +x ~/bin/grok-wake.sh
```

Keep a listener running on that machine (launchd, systemd or a terminal):

```bash
TINCAN_CONFIG="$HOME/.config/tincan/grok-cli.json" tincan listen --exec ~/bin/grok-wake.sh
```

Set the teammate's wake to `command` in the relay's `wake.json`:

```json
{ "grok-cli": { "method": "command" } }
```

Each nudge, the script:

1. Logs the relay's upgrade notice when `tincan listen` passes one in `TINCAN_UPGRADE_AVAILABLE`, and exits without a run when that notice is all the nudge is for (`TINCAN_WAITING` is 0). Otherwise it takes the wake lock (a second nudge during a run exits and leaves requests queued; a lock left by a killed run is broken) and exits early while a backoff is in force.
2. Pins the binary (see [The grok binary](#the-grok-binary)) and checks that it is Grok Build by its `--version` (`TINCAN_GROK_VERSION_PATTERN` to override), run with the wake's environment. It also checks that the wake home is not your home, and that there is a login in the wake home or an `XAI_API_KEY`.
3. Refuses project MCP config: a `.grok/config.toml` or `.mcp.json` in the workdir or any directory above it, up to `/`. The wake home's own `.grok/config.toml` and your own Grok config (`~/.grok/config.toml`, which Grok reads as a user config only when `HOME` is your home) are the two exceptions.
4. Pins identity: it runs `grok inspect --json` from the workdir with the wake home as `HOME` and `GROK_HOME`, exactly as the run will, and refuses to run unless the listing holds exactly one agent-tincan server, its `TINCAN_CONFIG` (read from the `config.toml` entry the listing points at, since the listing does not show a server's env) is the wake's own, every other server is in `TINCAN_WAKE_ALLOWED_SERVERS`, there are no plugins or hooks, and Grok reports no project config layer. The command Grok says it runs for a server must be the command in that `config.toml` entry: a project config that overrides the entry is listed with the wake home's file as its source, so a mismatch is refused. Reading the listing needs `jq` or `python3` on the listener's PATH (`TINCAN_GROK_JSON_TOOL` picks one).
5. Writes the sandbox profile (below) to the wake home's `.grok/sandbox.toml`, replacing whatever is there.
6. Picks a new session id and records it, with the workdir, in `~/.config/tincan/grok-cli.wake-sessions` (mode 600) before the run, so the history agent leaves the run out even if the timeout kills it.
7. Runs the pinned binary, started in the workdir (`TINCAN_GROK_WORKDIR`, default `<wake home>/work`) as the listing was, with Grok's update check off (`GROK_DISABLE_AUTOUPDATER=1`):

   ```
   <pinned grok> -p <prompt> --output-format json --always-approve --sandbox tincan-wake --cwd <workdir> --session-id <id>
   ```

   The prompt tells Grok to call check_inbox, finish work waiting on replies, handle and reply to every request, and repeat until the inbox is empty.
8. Kills the run and everything it started after `TINCAN_WAKE_TIMEOUT` seconds, counts failures, and backs off after three in a row (`TINCAN_WAKE_MAX_FAILURES`, `TINCAN_WAKE_BACKOFF`), telling `TINCAN_WAKE_OPERATOR` once. One successful run clears it.

The wake keeps its lock, failure count and backoff marker in `<name>.wake-state` beside the teammate's config (`~/.config/tincan/grok-cli.wake-state`, mode 700), not under the temp directory, since the run can write the temp directories. `TINCAN_WAKE_STATE_DIR` overrides it.

The header of `examples/lib/tincan-wake-lib.sh` lists the library's settings, and the header of `examples/grok-cli/grok-wake.sh` the wake's own.

## The grok binary

The npm package (`@xai-official/grok`) puts a node script on your PATH (for example `/opt/homebrew/bin/grok`) that runs `$GROK_HOME/bin/grok`, and installs a native binary there when it is missing. Under the wake's `GROK_HOME`, that is a file inside the wake home, which a sandboxed run can write. So the wake never runs the bootstrap. Each wake it:

- resolves `grok` on PATH (or `GROK_BIN`) to one file, following symlinks;
- when that file is the node bootstrap, uses the native binary your own Grok home points at instead: `$GROK_HOME/bin/grok` from the listener's environment, else `~/.grok/bin/grok`, resolved to its versioned file (for example `~/.grok/bin/grok-1.0.40`). With no native binary there, it refuses and asks for `GROK_BIN`;
- refuses a binary under the wake home, the workdir, a write root or a temp directory the run can write (`$TMPDIR`, `/tmp`, `/var/tmp` and the macOS per-user temp root under `/var/folders`), where the run could replace it;
- removes the wake home's `.grok/bin`, if a run left one there;
- runs that one file for the version check, `grok inspect` and the run, all with the wake's `HOME` and `GROK_HOME`, so the file it vetted is the file that runs.

To pin a version by hand, set `GROK_BIN` to a native binary, for example `GROK_BIN=$HOME/.grok/bin/grok-1.0.40`. When your own Grok updates itself, `~/.grok/bin/grok` moves to the new version and the next wake vets and runs that one.

## Security posture

Grok runs unattended with tool approval off (`--always-approve`): model-generated commands and tool calls execute without anyone confirming them. What confines it:

- The sandbox. `tincan-wake` extends Grok's built-in `workspace` profile, which Grok enforces on its whole process with Seatbelt on macOS and Landlock on Linux. It reads anywhere your user can. It writes only in the workdir, temp directories (`/tmp`, `/var/tmp` and the macOS temp dirs), the wake home's `.grok` (where Grok keeps its `config.toml`, `sandbox.toml` and hook files write-protected, so a run cannot widen the next one; `.grok/bin` is not protected, which is why the wake never runs a grok from there, see [The grok binary](#the-grok-binary)) and the `read_write` directories the wake adds: the teammate's attachments folder (`attachments/<agent>` beside `TINCAN_CONFIG`, where `tincan mcp` saves the files teammates send) and the operator's write roots. Write roots are opened by the operator, not the model: `TINCAN_GROK_WRITE_ROOTS` (colon-separated absolute paths), each canonicalized and added only if it sits under `TINCAN_GROK_ALLOWED_ROOTS` (default `~/code:<workdir>`), the same checks as the Codex wake's write roots. A custom profile makes Grok refuse to start when it cannot apply it, where a built-in profile would warn and run unconfined. Network is open, as with the Codex wake, so a command the model runs can send anything it can read off the machine.
- The identity check starts Grok wired only to this teammate: exactly one agent-tincan server, pinned to its own `TINCAN_CONFIG`, and nothing else it did not vet. That guards against misconfiguration, not against the model. Grok can read anywhere and has a shell, so it can run `tincan` with another co-located agent's `TINCAN_CONFIG` and act as that agent, or read another agent's config. Allowlists that name agents keep them apart only when they run on different machines.
- Secrets. The wake home's Grok login (`.grok/auth.json`) and an `XAI_API_KEY` in the listener's environment are readable by the run.
- Account. The wake runs on your Grok account and uses its plan or your API credits; every wake is a Grok session like one you started yourself.

The requests it acts on come from joined teammates, who are fully trusted (see [docs/trust-model.md](../trust-model.md)).

## Doctor

`tincan doctor` reads `$GROK_HOME/config.toml` (`~/.grok/config.toml` by default). Run with the teammate's config, `TINCAN_CONFIG=~/.config/tincan/grok-cli.json tincan doctor`, it also reads the wake home's `~/.config/tincan/grok-cli.wake/.grok/config.toml`, and inside a wake run `GROK_HOME` already points there. For a wake home elsewhere, pass it: `tincan doctor --config <wake home>/.grok/config.toml`, or run doctor with `GROK_HOME=<wake home>/.grok`. It checks that the tincan entry runs this tincan with `mcp` and flags two tincan servers loaded together. It does not run `grok inspect`, so a server Grok imports from elsewhere shows up only as a wake refusal.

## History

Your own Grok CLI sessions (in `~/.grok`, or `$GROK_HOME`) are a history source, `grok-cli`: `tincan history grok-cli`, or ask the history agent "what did I last ask Grok CLI?". Wake runs live in the wake home, not in your Grok home, and are left out in any case by the recorded session ids, the workdir and the sandbox profile ([history.md](history.md#grok-cli)).

## Limits

- Every wake is a fresh session: the whole exchange has to finish inside one run.
- The run can write the temp directories, so the wake refuses a grok binary under `$TMPDIR`, `/tmp`, `/var/tmp` or the macOS per-user temp root, as it does one under the wake home, the workdir or a write root. Install Grok Build (or point `GROK_BIN`) outside them.
- `grok inspect` does not show a server's env, so the `TINCAN_CONFIG` check reads the `config.toml` entry the listing points at, one-line values only; an entry it cannot read is refused rather than trusted.
- Under the sandbox, `tincan mcp` runs inside it too. It reaches the relay over the network and saves attachments to the folder the wake opens for it, but cannot save a relay move to its config file outside the workdir; `tincan doctor` reports that case.
- Not yet verified live: that `--always-approve` lets headless Grok call the tincan MCP tools without a prompt, that Grok accepts the `tincan-wake` profile as written, and a full wake answering a teammate's ask. The binary check and the identity check were run against Grok Build 1.0.40's real `--version` and `grok inspect --json` output. Record the first live wake before relying on it.
