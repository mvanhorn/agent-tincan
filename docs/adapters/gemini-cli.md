# Gemini command-line agent (gemini-cli)

The `gemini-cli` teammate is Google's Gemini running as a coding agent on your machine, woken the way Codex is: `tincan listen --exec` runs a wake script whenever teammates' requests are waiting, and the script starts one headless run that drains the inbox and exits. It has two engines:

- `agy`, Antigravity CLI, the default. It works with a Google consumer account (free tier, AI Pro or Ultra) after one interactive login, and needs no API key. By default it runs confined and read-only: it answers from what it knows and can read, but cannot write files or run commands. This is the normal setup, and the next section walks through it.
- `gemini`, Gemini CLI, for owners with a paid Gemini API key. Google stopped accepting consumer Google logins in Gemini CLI on 2026-06-18 and moved consumer accounts to Antigravity, so Gemini CLI now runs only with `GEMINI_API_KEY` (or Vertex, which the wake does not set up). See [If you have a Gemini API key](#if-you-have-a-gemini-api-key).

The teammate's name, kind and tincan config stay the same whichever engine runs.

The history agent cannot read `gemini-cli` runs yet: Antigravity keeps conversations in protobuf files with an unpublished schema, and Gemini CLI sessions are not a history source in this version.

## Set up with a Google account (no API key)

Do these steps in order on the machine that will run the teammate, except step 1's invite, which runs on an admin device.

### 1. Invite and join

On an admin device (the relay must know the `gemini-cli` kind; on an older relay, invite without `--kind` and use `tincan onboard --kind gemini-cli=gemini-cli`):

```bash
tincan invite gemini-cli --kind gemini-cli
```

On the machine that runs the teammate, with a config of its own:

```bash
TINCAN_CONFIG="$HOME/.config/tincan/gemini-cli.json" tincan join <code> --relay http://tincan-relay
```

### 2. Install agy and log in

```bash
curl -fsSL https://antigravity.google/cli/install.sh | bash
agy        # log in with the owner's Google account, then quit
```

agy has no API-key login. Its credentials are cached by that interactive login, so when they expire, only the owner can renew them by running `agy` again. The wake treats an authentication failure as something a retry cannot fix: it backs off at once, tells the operator teammate once (`TINCAN_WAKE_OPERATOR`), and leaves requests queued.

### 3. Add the tincan MCP server to agy

```bash
agy mcp add -e TINCAN_CONFIG="$HOME/.config/tincan/gemini-cli.json" agent-tincan "$(command -v tincan)" mcp
agy mcp list
```

The flags go before the server name. `agy mcp add` writes `~/.gemini/config/mcp_config.json` in the usual `{"mcpServers": {...}}` shape. The wake reads that file, and `.agents/mcp_config.json` in its working directory, for its identity check. If you keep agy's config elsewhere, set `TINCAN_AGY_MCP_CONFIG` in the listener service's environment.

The identity check also refuses to start agy while that file lists any other MCP server, unless you name it in `TINCAN_WAKE_ALLOWED_SERVERS`. A confined run allows every MCP tool agy has, so each configured server is one you trust with a teammate's request.

### 4. Install the listener

```bash
TINCAN_CONFIG="$HOME/.config/tincan/gemini-cli.json" tincan listen install --wake gemini-cli
```

This prints what a woken agy may do, then writes `gemini-wake.sh` and the shared wake library beside the teammate's config, and a login service that keeps `tincan listen` running (a launchd agent on macOS, a systemd user unit on Linux). It checks agy's MCP config first and stops, writing nothing, unless step 3 left exactly one agent-tincan server pinned to this config. It prints the command that starts the service and never starts it. `tincan upgrade` later refreshes the scripts you did not edit.

### 5. Set the wake method on the relay

In the relay's `wake.json`:

```json
{ "gemini-cli": { "method": "command" } }
```

### 6. Start the service and verify

Run the start command install printed. Then check the setup with the teammate's config:

```bash
TINCAN_CONFIG="$HOME/.config/tincan/gemini-cli.json" tincan doctor
```

From another joined agent, send a test ask:

```bash
tincan ask gemini-cli "Tincan test: reply ok."
```

The listener's log (`~/Library/Logs/tincan-listen-gemini-cli.log` on macOS) shows agy's JSON result, and the ask comes back with a reply. A line saying `agy denied these tool calls` names any tool the run tried and was refused.

agy sets no time limit on a headless run (`--print-timeout` defaults to 0). Set `TINCAN_AGY_PRINT_TIMEOUT` to pass one; the wake's own hard timeout is `TINCAN_WAKE_TIMEOUT` (1500 seconds).

### Without the service

To run the listener by hand instead, copy `examples/gemini-cli/gemini-wake.sh` and `examples/lib/tincan-wake-lib.sh` into one folder, `chmod +x` the script, and keep this running in a terminal or `tmux`:

```bash
TINCAN_CONFIG="$HOME/.config/tincan/gemini-cli.json" tincan listen --exec ~/bin/gemini-wake.sh
```

## Running agy unconfined

A confined agy cannot change files, so it cannot do coding work. To run it as a full coding agent, set `TINCAN_GEMINI_ALLOW_UNCONFINED=1` in the listener's environment. Read what that accepts first. The wake then runs agy with tool approval off (`--dangerously-skip-permissions`) and no per-run limits, and nothing limits its writes to your user's files: any joined teammate's request can make agy change or delete any file your user owns, and run commands that reach the network. The wake still pins identity and still times out. If that is more than you want, run the listener under a separate macOS or Linux user that owns only the directories agy should touch, or use the gemini engine with an API key.

The wake reads the variable only from its own environment, which it gets from `tincan listen`. Add it to the service install wrote: an `EnvironmentVariables` entry in `~/Library/LaunchAgents/com.agenttincan.listen.gemini-cli.plist`, or an `Environment="TINCAN_GEMINI_ALLOW_UNCONFINED=1"` line in the systemd unit, then reload it. Install and `tincan upgrade` keep variables you added there. For a listener run by hand, put it on the command line before `tincan listen`. The listener's log then shows `running agy unconfined (TINCAN_GEMINI_ALLOW_UNCONFINED=1)` before each run.

## If you have a Gemini API key

With a paid Gemini API key you can run Gemini CLI instead of agy. It runs in its own sandbox and can write in its working directory (see [Security posture](#security-posture)). Join and set `wake.json` as in steps 1 and 5 above, and install the listener as in step 4 or copy the wake script by hand. Then install Gemini CLI and add the tincan server to your user settings with trust on:

```bash
gemini mcp add -s user -e TINCAN_CONFIG="$HOME/.config/tincan/gemini-cli.json" --trust agent-tincan tincan mcp
```

That writes an entry to `~/.gemini/settings.json` like:

```json
{
  "mcpServers": {
    "agent-tincan": {
      "command": "tincan",
      "args": ["mcp"],
      "env": { "TINCAN_CONFIG": "/Users/you/.config/tincan/gemini-cli.json" },
      "trust": true
    }
  }
}
```

`--trust` (`"trust": true`) spares you confirmation prompts when you run Gemini CLI yourself; the wake does not need it, since `--approval-mode=yolo` approves tincan tool calls either way.

Put the engine and the key in the listener's environment. Install never writes a key into the service, so for the gemini engine run the listener by hand:

```bash
TINCAN_GEMINI_ENGINE=gemini GEMINI_API_KEY="$(cat ~/.config/tincan/gemini-api-key)" \
TINCAN_CONFIG="$HOME/.config/tincan/gemini-cli.json" \
  tincan listen --exec ~/bin/gemini-wake.sh
```

Keep the key in a file only you can read (`chmod 600 ~/.config/tincan/gemini-api-key`), not typed on the command line, in chat or in `wake.json`; in a launchd plist or systemd unit, make the file readable only by you. With the gemini engine and no key, the wake fails once with a message naming `GEMINI_API_KEY`, backs off, and leaves requests queued. A key Gemini CLI rejects (exit status 41 or an invalid-key message) backs off the same way.

## How the wake works

Each nudge, the script:

1. Logs the relay's upgrade notice when `tincan listen` passes one in `TINCAN_UPGRADE_AVAILABLE`, and exits without a run when that notice is all the nudge is for (`TINCAN_WAITING` is 0). Otherwise it takes the wake lock (a second nudge during a run exits and leaves requests queued; a lock left by a killed run is broken) and exits early while a backoff is in force.
2. Checks the engine: `agy` or `gemini` on PATH (`AGY_BIN`, `GEMINI_BIN` to override), answering `--version` in the expected shape (`TINCAN_GEMINI_VERSION_PATTERN` to override), and `GEMINI_API_KEY` for the gemini engine.
3. Pins identity: it reads the chosen engine's MCP config (agy: `TINCAN_AGY_MCP_CONFIG`, default `~/.gemini/config/mcp_config.json`, plus `.agents/mcp_config.json` in the workdir; gemini: `TINCAN_GEMINI_SETTINGS`, default `~/.gemini/settings.json`, plus `.gemini/settings.json` in the workdir) and refuses to run unless there is exactly one agent-tincan server, its `TINCAN_CONFIG` is the wake's own, and every other server is listed in `TINCAN_WAKE_ALLOWED_SERVERS`. The engine runs with tool approval off, so every server it loads is trusted. Reading these JSON files needs `jq` or `python3` on the listener's PATH.
4. Runs the engine in `TINCAN_GEMINI_WORKDIR` (default `~/tincan-gemini`) with a prompt that tells it to call check_inbox, finish work waiting on replies, handle and reply to every request, and repeat until the inbox is empty:
   - agy, confined (the default): `agy --sandbox --output-format json --settings <per-run settings> -p <prompt>`. The settings are described under [Security posture](#security-posture), and the prompt tells agy it is read-only.
   - agy, unconfined (`TINCAN_GEMINI_ALLOW_UNCONFINED=1`): `agy --output-format json --dangerously-skip-permissions -p <prompt>`
   - gemini: `gemini --sandbox --approval-mode=yolo --output-format json --allowed-mcp-server-names agent-tincan [--include-directories <root>]... --include-directories <attachments dir> -p <prompt>`, where the tincan server's name is whatever the identity check found, and the attachments directory is `attachments/<agent>` beside `TINCAN_CONFIG` (created with mode 700), where `tincan mcp` saves the files teammates send.
5. Records the run in `<name>.wake-sessions` beside the teammate's config (`~/.config/tincan/gemini-cli.wake-sessions`, mode 600): the workdir before the run, and the session id the engine's JSON output reports (`conversation_id` for agy, `session_id` for Gemini CLI) after it, so the history agent leaves wake runs out. For agy it also logs the names of any actions the JSON lists under `denied_actions`. A run the timeout kills is recorded by its workdir alone.
6. Kills the run and everything it started after `TINCAN_WAKE_TIMEOUT` seconds, counts failures, and backs off after three in a row (`TINCAN_WAKE_MAX_FAILURES`, `TINCAN_WAKE_BACKOFF`), telling `TINCAN_WAKE_OPERATOR` once. One successful run clears it.

The header of `examples/lib/tincan-wake-lib.sh` lists the library's settings.

## Security posture

Both engines run unattended: no one confirms the tool calls a run makes. What confines them differs, and you should choose the engine and mode with that in mind.

gemini engine: confined. The wake passes `--sandbox`, which on macOS runs Gemini CLI under seatbelt (`sandbox-exec`) with its default `permissive-open` profile: writes only inside the working directory and the include directories below (the operator's write roots and the teammate's attachments directory), reads anywhere, network open. Gemini CLI documents that profile as also allowing writes to temp and cache directories; this wake has not verified the exact list. `GEMINI_API_KEY` is in the engine's environment, so a command the model runs can read it, and with the network open, send it elsewhere; use a key you can revoke and scope it to this use. On Linux, `--sandbox` needs Docker or Podman; set `GEMINI_SANDBOX` to pick one, and `SEATBELT_PROFILE` on macOS to pick a stricter profile (`restrictive-open`, `strict-open`, `strict-proxied`). The wake also passes `--allowed-mcp-server-names` with only the tincan server and your `TINCAN_WAKE_ALLOWED_SERVERS`, so a server added by an extension or another settings file does not start in a wake run. Extra write roots are opened by the operator, not the model: `TINCAN_GEMINI_WRITE_ROOTS` (colon-separated absolute paths), each canonicalized and passed as `--include-directories` only if it sits under `TINCAN_GEMINI_ALLOWED_ROOTS` (default `~/code:~/tincan-gemini`), the same checks as the Codex wake's write roots. Network is open, so a model-run command can send anything it can read off the machine, as with the Codex wake.

agy engine, confined (the default). Verified on agy 1.5.16. The wake runs agy with `--sandbox`, its terminal sandbox, which among other things blocks shell reads of `~/.config`, and with `--settings`, settings for this run only that agy never saves, so your own `~/.gemini/antigravity-cli/settings.json` is neither changed nor relied on. The settings put agy in `strict` permission mode, where a headless run denies any tool call no rule allows instead of asking. Only whole-category allow rules take effect in strict mode, so the run allows `mcp(*)` (every configured MCP server's tools, which is why the identity check refuses unlisted servers) and `read_file(/)` (agy reads files itself before it calls an MCP tool). Deny rules win over allow rules, including any merged in from your own settings, and the run denies `command(*)`, `write_file(/)`, and reads of everything in the tincan config folder (which holds every co-located teammate's key) except this teammate's own `attachments/<agent>` folder, plus `~/.ssh`. So a confined run can read the rest of your user's files and pass what it reads to the teammate that asked, but it cannot write files or run commands. agy resolves symlinks, `/tmp` aliases and `./` before it matches a deny rule, but it matches letter case exactly, so on a case-insensitive volume (the macOS default) a path spelled with different case gets past the read denies. Treat them as best effort: a request crafted to do it could read another co-located teammate's key. agy hooks you installed yourself (`~/.gemini/config/hooks.json`) still load in a wake run. Plugin MCP servers would too, outside `mcp_config.json` and the identity check, with `mcp(*)` allowing their tools, so a confined wake refuses to run while `agy plugin list` shows any plugin.

agy engine, unconfined (`TINCAN_GEMINI_ALLOW_UNCONFINED=1`). `--dangerously-skip-permissions` turns approval off with nothing else limiting what a run does. It can write anywhere your user can and reach the network. The wake logs every unconfined run. Write roots do not apply to agy and are ignored with a note. To reduce exposure, run the listener under a dedicated macOS or Linux user that owns only the directories agy should touch, or use the gemini engine.

In both cases the identity check starts the engine wired only to this teammate: exactly one agent-tincan server, pinned to its own `TINCAN_CONFIG`. That guards against misconfiguration, not against the model. An engine with a shell can run `tincan` with another co-located agent's `TINCAN_CONFIG` and act as that agent, so allowlists that name agents keep them apart only when they run on different machines. The requests it acts on come from joined teammates, who are fully trusted (see [docs/trust-model.md](../trust-model.md)).

## Doctor

`tincan doctor` reads `~/.gemini/settings.json` (Gemini CLI) and `~/.gemini/config/mcp_config.json` (agy) along with the other apps' MCP configs, plus the other files the wake checks: `TINCAN_AGY_MCP_CONFIG` and `TINCAN_GEMINI_SETTINGS` when set, and `.agents/mcp_config.json` and `.gemini/settings.json` in `TINCAN_GEMINI_WORKDIR` (default `~/tincan-gemini`). Those settings usually live in the listener's environment, so run doctor with the same ones when you changed them. It checks that the tincan entry runs this tincan with `mcp` and flags two tincan servers loaded together in one file.

## Limits

- Every wake is a fresh session: the whole exchange has to finish inside one run.
- No history reader for either engine yet.
- A confined agy is read-only: a request that needs a file written or a command run gets a failed reply. Run it unconfined, or use the gemini engine, for coding work.
- agy's permission rules are not documented outside the binary. The confined settings were verified on agy 1.5.16; if a later agy changes them, a run may fail or deny more than it should.
- agy matches read deny rules case-sensitively, so on a case-insensitive volume (the macOS default) they are best effort: a request that spells a path in different case can read the tincan config folder or `~/.ssh`.
- A confined wake refuses to run while agy has plugins installed (`agy plugin list`). Uninstall them, or run unconfined.
- Under the gemini sandbox, `tincan mcp` runs inside seatbelt too. It reaches the relay over the network (allowed) and saves attachments to the directory the wake opens for it, but may be unable to save a relay move to its config file outside the workdir; `tincan doctor` reports that case.
- Verified live on 2026-11-24 with agy 1.5.16 under the launchd listener: teammate asks were answered, and a request to write a file outside the workdir was refused with no file written. The Gemini CLI engine has not been run against a relay yet.
