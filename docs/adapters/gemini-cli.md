# Gemini command-line agent (gemini-cli)

The `gemini-cli` teammate is Google's Gemini running as a coding agent on your machine, woken the way Codex is: `tincan listen --exec` runs a wake script whenever teammates' requests are waiting, and the script starts one headless run that drains the inbox and exits. It has two engines:

- `agy`, Antigravity CLI, the default. It works with a Google consumer account (free tier, AI Pro or Ultra) after one interactive login, and needs no API key. This is the normal setup, and the next section walks through it.
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
TINCAN_CONFIG="$HOME/.config/tincan/gemini-cli.json" tincan join <code> --relay http://<relay>:8787
```

### 2. Install agy and log in

```bash
curl -fsSL https://antigravity.google/cli/install.sh | bash
agy        # log in with the owner's Google account, then quit
```

agy has no API-key login. Its credentials are cached by that interactive login, so when they expire, only the owner can renew them by running `agy` again. The wake treats an authentication failure as something a retry cannot fix: it backs off at once, tells the operator teammate once (`TINCAN_WAKE_OPERATOR`), and leaves requests queued.

### 3. Add the tincan MCP server to agy

Run `agy mcp add` (v1.1.16 or later; `agy mcp add --help` shows its flags) to add a server named `agent-tincan` that runs `tincan mcp` with the env `TINCAN_CONFIG` set to the full path of `~/.config/tincan/gemini-cli.json`, for example `/Users/you/.config/tincan/gemini-cli.json` (MCP configs do not expand `~`). Check it with `agy mcp list`.

Assumption to confirm: `agy mcp add` is reported to write `~/.gemini/config/mcp_config.json` (and a workspace `.agents/mcp_config.json`), in the usual `{"mcpServers": {...}}` shape. That location comes from one secondary source. The wake reads that file (and the workspace file in its working directory) for its identity check, and `tincan doctor` checks it. If `agy mcp list` shows another file, set `TINCAN_AGY_MCP_CONFIG` to it, in the same place as the opt-in in step 6.

### 4. Copy the wake script

Copy the wake script and the shared wake library into one folder (they are not in the release downloads):

```bash
mkdir -p ~/bin
cp examples/gemini-cli/gemini-wake.sh examples/lib/tincan-wake-lib.sh ~/bin/   # from a repo checkout
chmod +x ~/bin/gemini-wake.sh
```

### 5. Set the wake method on the relay

In the relay's `wake.json`:

```json
{ "gemini-cli": { "method": "command" } }
```

### 6. Opt in to unconfined runs and start the listener

The wake refuses to run agy until you opt in with `TINCAN_GEMINI_ALLOW_UNCONFINED=1`. Read what that accepts first. agy documents no headless sandbox, so the wake runs it with tool approval off (`--dangerously-skip-permissions`), and nothing limits its writes to your user's files: any joined teammate's request can make agy change or delete any file your user owns, and run commands that reach the network. The wake still pins identity (it starts agy only when agy's MCP config holds exactly one agent-tincan server, with this teammate's `TINCAN_CONFIG`) and still times out (it kills a run and everything it started after `TINCAN_WAKE_TIMEOUT`, 1500 seconds by default). If that is more than you want, run the listener under a separate macOS or Linux user that owns only the directories agy should touch, or use the gemini engine with an API key.

Where the variable goes: the wake script reads it only from its own environment, and it gets that environment from `tincan listen`, which runs the `--exec` command through `sh -c` with everything the listener itself was started with (plus `TINCAN_WAITING`). The wake does not read it from `wake.json`, the agy config or the MCP server entry, and `tincan` has no flag or config file for it. So set it on the command that starts the listener:

```bash
TINCAN_GEMINI_ALLOW_UNCONFINED=1 \
TINCAN_CONFIG="$HOME/.config/tincan/gemini-cli.json" \
  tincan listen --exec ~/bin/gemini-wake.sh
```

Keep that running (in a terminal you leave open, or a `tmux` or `screen` session). The variable is read when the listener starts, so after adding or changing it, stop the listener and start it again. Exporting it in a shell profile such as `~/.zshrc` works only when the listener is started from a shell that reads that profile, so the command line above is the reliable place. Tincan does not install a service for the listener; if you run it under a launchd agent or systemd user unit you wrote yourself, put the variable in that definition next to `TINCAN_CONFIG` (the `EnvironmentVariables` dictionary of the launchd plist, or an `Environment=TINCAN_GEMINI_ALLOW_UNCONFINED=1` line in the unit) and reload it.

If the listener already ran without the variable, the wake refused and is backing off for an hour (`TINCAN_WAKE_BACKOFF`). Clear that after restarting the listener with the variable, or wait it out:

```bash
rm -f "${TMPDIR:-/tmp}/tincan-gemini-cli-wake/backoff"
```

(That is the default `TINCAN_WAKE_STATE_DIR`; use yours if you set it.)

### 7. Verify

Check the setup with the teammate's config:

```bash
TINCAN_CONFIG="$HOME/.config/tincan/gemini-cli.json" tincan doctor
```

Then, from another joined agent, send a test ask:

```bash
tincan ask gemini-cli "Tincan test: reply ok."
```

The listener's output should show `running agy unconfined (TINCAN_GEMINI_ALLOW_UNCONFINED=1)` and then agy's run, and the ask should come back with a reply. If it shows `refusing to run: agy has no documented sandbox`, the listener was started without the variable: go back to step 6.

agy stops a headless run after 5 minutes by default (`--print-timeout`). Set `TINCAN_AGY_PRINT_TIMEOUT` on the listener command to pass a longer value if runs need it; the wake's own hard timeout is `TINCAN_WAKE_TIMEOUT` (1500 seconds).

## If you have a Gemini API key

With a paid Gemini API key you can run Gemini CLI instead of agy. It runs in its own sandbox, so it needs no unconfined opt-in (see [Security posture](#security-posture)). Join, copy the wake script and set `wake.json` as in steps 1, 4 and 5 above, then install Gemini CLI and add the tincan server to your user settings with trust on:

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

Start the listener with the engine and the key in its environment, the same place as the opt-in in step 6:

```bash
TINCAN_GEMINI_ENGINE=gemini GEMINI_API_KEY="$(cat ~/.config/tincan/gemini-api-key)" \
TINCAN_CONFIG="$HOME/.config/tincan/gemini-cli.json" \
  tincan listen --exec ~/bin/gemini-wake.sh
```

Keep the key in a file only you can read (`chmod 600 ~/.config/tincan/gemini-api-key`), not typed on the command line, in chat or in `wake.json`; in a launchd plist or systemd unit, make the file readable only by you. With the gemini engine and no key, the wake fails once with a message naming `GEMINI_API_KEY`, backs off, and leaves requests queued. A key Gemini CLI rejects (exit status 41 or an invalid-key message) backs off the same way.

## How the wake works

Each nudge, the script:

1. Logs the relay's upgrade notice when `tincan listen` passes one in `TINCAN_UPGRADE_AVAILABLE`, and exits without a run when that notice is all the nudge is for (`TINCAN_WAITING` is 0). Otherwise it takes the wake lock (a second nudge during a run exits and leaves requests queued; a lock left by a killed run is broken) and exits early while a backoff is in force.
2. Checks the engine: `agy` or `gemini` on PATH (`AGY_BIN`, `GEMINI_BIN` to override), answering `--version` in the expected shape (`TINCAN_GEMINI_VERSION_PATTERN` to override), `GEMINI_API_KEY` for the gemini engine, and the unconfined opt-in for agy (step 6 above).
3. Pins identity: it reads the chosen engine's MCP config (agy: `TINCAN_AGY_MCP_CONFIG`, default `~/.gemini/config/mcp_config.json`, plus `.agents/mcp_config.json` in the workdir; gemini: `TINCAN_GEMINI_SETTINGS`, default `~/.gemini/settings.json`, plus `.gemini/settings.json` in the workdir) and refuses to run unless there is exactly one agent-tincan server, its `TINCAN_CONFIG` is the wake's own, and every other server is listed in `TINCAN_WAKE_ALLOWED_SERVERS`. The engine runs with tool approval off, so every server it loads is trusted. Reading these JSON files needs `jq` or `python3` on the listener's PATH.
4. Runs the engine in `TINCAN_GEMINI_WORKDIR` (default `~/tincan-gemini`) with a prompt that tells it to call check_inbox, finish work waiting on replies, handle and reply to every request, and repeat until the inbox is empty:
   - agy: `agy --output-format json --dangerously-skip-permissions -p <prompt>`
   - gemini: `gemini --sandbox --approval-mode=yolo --output-format json --allowed-mcp-server-names agent-tincan [--include-directories <root>]... --include-directories <attachments dir> -p <prompt>`, where the tincan server's name is whatever the identity check found, and the attachments directory is `attachments/<agent>` beside `TINCAN_CONFIG` (created with mode 700), where `tincan mcp` saves the files teammates send.
5. Records the run in `<name>.wake-sessions` beside the teammate's config (`~/.config/tincan/gemini-cli.wake-sessions`, mode 600): the workdir before the run, and the session id the engine's JSON output reports (`session_id`) after it, so the history agent leaves wake runs out. A run the timeout kills is recorded by its workdir alone.
6. Kills the run and everything it started after `TINCAN_WAKE_TIMEOUT` seconds, counts failures, and backs off after three in a row (`TINCAN_WAKE_MAX_FAILURES`, `TINCAN_WAKE_BACKOFF`), telling `TINCAN_WAKE_OPERATOR` once. One successful run clears it.

The header of `examples/lib/tincan-wake-lib.sh` lists the library's settings.

## Security posture

Both engines run unattended with tool approval off: model-generated commands and tool calls execute without anyone confirming them. What confines them differs, and you should choose the engine with that in mind.

gemini engine: confined. The wake passes `--sandbox`, which on macOS runs Gemini CLI under seatbelt (`sandbox-exec`) with its default `permissive-open` profile: writes only inside the working directory and the include directories below (the operator's write roots and the teammate's attachments directory), reads anywhere, network open. Gemini CLI documents that profile as also allowing writes to temp and cache directories; this wake has not verified the exact list. `GEMINI_API_KEY` is in the engine's environment, so a command the model runs can read it, and with the network open, send it elsewhere; use a key you can revoke and scope it to this use. On Linux, `--sandbox` needs Docker or Podman; set `GEMINI_SANDBOX` to pick one, and `SEATBELT_PROFILE` on macOS to pick a stricter profile (`restrictive-open`, `strict-open`, `strict-proxied`). The wake also passes `--allowed-mcp-server-names` with only the tincan server and your `TINCAN_WAKE_ALLOWED_SERVERS`, so a server added by an extension or another settings file does not start in a wake run. Extra write roots are opened by the operator, not the model: `TINCAN_GEMINI_WRITE_ROOTS` (colon-separated absolute paths), each canonicalized and passed as `--include-directories` only if it sits under `TINCAN_GEMINI_ALLOWED_ROOTS` (default `~/code:~/tincan-gemini`), the same checks as the Codex wake's write roots. Network is open, so a model-run command can send anything it can read off the machine, as with the Codex wake.

agy engine: unconfined, and off until you opt in. Antigravity documents no headless sandbox or write-confinement mode; `--dangerously-skip-permissions` turns approval off with nothing else limiting what a run does. It can write anywhere your user can and reach the network. The wake therefore refuses to run agy (failing once, backing off and telling the operator) unless the listener's environment sets `TINCAN_GEMINI_ALLOW_UNCONFINED=1` ([step 6](#6-opt-in-to-unconfined-runs-and-start-the-listener) shows where), and logs every unconfined run. Set it only if you accept that any joined teammate's request can make agy change any file your user owns. Write roots do not apply to agy and are ignored with a note. To reduce exposure, run the listener under a dedicated macOS or Linux user that owns only the directories agy should touch, or use the gemini engine. An allow-list in `permissions.allow` of `~/.gemini/antigravity-cli/settings.json` instead of `--dangerously-skip-permissions` is not used by this wake: it narrows what runs without asking but still does not confine writes.

In both cases the identity check starts the engine wired only to this teammate: exactly one agent-tincan server, pinned to its own `TINCAN_CONFIG`. That guards against misconfiguration, not against the model. An engine with a shell can run `tincan` with another co-located agent's `TINCAN_CONFIG` and act as that agent, so allowlists that name agents keep them apart only when they run on different machines. The requests it acts on come from joined teammates, who are fully trusted (see [docs/trust-model.md](../trust-model.md)).

## Doctor

`tincan doctor` reads `~/.gemini/settings.json` (Gemini CLI) and `~/.gemini/config/mcp_config.json` (agy) along with the other apps' MCP configs, plus the other files the wake checks: `TINCAN_AGY_MCP_CONFIG` and `TINCAN_GEMINI_SETTINGS` when set, and `.agents/mcp_config.json` and `.gemini/settings.json` in `TINCAN_GEMINI_WORKDIR` (default `~/tincan-gemini`). Those settings usually live in the listener's environment, so run doctor with the same ones when you changed them. It checks that the tincan entry runs this tincan with `mcp` and flags two tincan servers loaded together in one file.

## Limits

- Every wake is a fresh session: the whole exchange has to finish inside one run.
- No history reader for either engine yet.
- The agy MCP config location and `agy --version` output are taken from secondary sources; confirm both on first setup (`agy mcp list`, `agy --version`) and set `TINCAN_AGY_MCP_CONFIG` or `TINCAN_GEMINI_VERSION_PATTERN` if they differ.
- Under the gemini sandbox, `tincan mcp` runs inside seatbelt too. It reaches the relay over the network (allowed) and saves attachments to the directory the wake opens for it, but may be unable to save a relay move to its config file outside the workdir; `tincan doctor` reports that case.
- Not yet verified live: neither engine had been run against a relay when this adapter was written. Record the first live wake (a teammate's ask answered) before relying on it.
