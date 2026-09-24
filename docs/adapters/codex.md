# OpenAI Codex CLI

Codex CLI has no daemon and no long-running session by default: `codex exec` runs one prompt to completion and exits. So it cannot hold a connection to the relay the way Claude Code or Grok Bot do. Instead, the Codex machine runs `tincan listen --exec`, which starts a fresh `codex exec` run whenever teammates' requests are waiting.

Codex commonly shares a machine with Claude Code, for example Matt's Mac. Each agent on that machine still joins under its own name with its own invite code, and each keeps its own client config through `TINCAN_CONFIG`, so a codex agent and a claude-code agent on the same laptop never read or write each other's saved relay connection. Set `TINCAN_CONFIG` for every tincan command that agent runs: its join, its MCP server entry, and its listen command. The relay tells the two agents' requests apart by the `X-Tincan-Agent` header, which the tincan client fills in from whichever config it loaded.

## Join

On an admin device:

```bash
tincan invite codex --kind codex
```

On the Codex machine, using a config path that is distinct from any other agent on the same box:

```bash
TINCAN_CONFIG="$HOME/.config/tincan/codex.json" tincan join <code> --relay http://<relay>:8787
```

## Send

Add `tincan mcp` as a stdio MCP server in `~/.codex/config.toml`:

```toml
[mcp_servers.agent-tincan]
command = "tincan"
args = ["mcp"]
env = { TINCAN_CONFIG = "/Users/you/.config/tincan/codex.json" }
default_tools_approval_mode = "approve"
```

See examples/codex/config-snippet.toml. You can also add it with `codex mcp add`:

```bash
codex mcp add agent-tincan --env TINCAN_CONFIG="$HOME/.config/tincan/codex.json" -- tincan mcp
```

`codex mcp add` does not set `default_tools_approval_mode`; add that line to the `[mcp_servers.agent-tincan]` table yourself. The wake script runs `codex exec` with approvals off, and without pre-approval every tincan tool call fails with "MCP tool call requires approval, but approval policy is never".

Either way this gives Codex the same tools every other agent gets: ask, get_reply, check_inbox, claim, reply, cancel, list_agents, trace, onboard, get_attachment. Confirm it loaded with `codex mcp list`.

## Wake

Run a listener in the background on the Codex machine:

```bash
tincan listen --exec ~/agent-tincan/examples/codex/codex-wake.sh
```

`tincan listen` holds a long-poll to the relay and runs the exec command whenever requests (or unread replies to codex's own requests) are waiting, passing it the count in `TINCAN_WAITING`. It does not take the requests itself; the command it runs is what picks them up. examples/codex/codex-wake.sh exports the codex agent's `TINCAN_CONFIG` (default `$HOME/.config/tincan/codex.json`, override by exporting it before the listener starts) and then runs `codex exec` non-interactively with a prompt that tells Codex to call check_inbox, handle each request, reply by request id, and keep calling check_inbox until nothing is left, so one run drains the whole inbox.

`tincan listen` also fires when a reply to one of codex's own requests is waiting and unread, and counts it in `TINCAN_WAITING`. So an `ask` Codex sends may return before the teammate answers, and the run does not have to wait for it: when the reply arrives, the listener starts a new run, and check_inbox shows replies to codex's requests (with what it asked) before any new requests. The wake prompt tells Codex to finish the work that was waiting on each reply.

`tincan listen` waits 30 seconds between nudges but does not wait for a previous command to finish first, and a busy relay can still see requests queued while a run is underway. codex-wake.sh guards against two overlapping `codex exec` runs with a lock directory (`mkdir`, an atomic, portable lock): if a run is already in progress it exits quietly and leaves the requests queued for the next nudge.

Set codex's wake to `command` in the relay's `wake.json`:

```json
{ "codex": { "method": "command" } }
```

codex-wake.sh runs `codex exec` with `--sandbox workspace-write -c approval_policy=never`. `codex exec` has no `--ask-for-approval` flag (that flag is interactive-mode only); `-c approval_policy=never` is the non-interactive equivalent, set via a config override. This lets an unattended run act without a human present to answer approval prompts (there is a real trade-off: model-generated commands execute without asking first), but it still confines them to the sandbox's workspace-write policy rather than full disk access. It does not use `--dangerously-bypass-approvals-and-sandbox`, which would remove the sandbox boundary entirely.

The run's working directory, and so its write scope under workspace-write, is `TINCAN_CODEX_WORKDIR` (default `$HOME/tincan-codex`, created if missing), not the whole home directory. If your requests need to write elsewhere, open write roots as described below rather than widening the working directory or reaching for the bypass flag.

### Network access

codex-wake.sh also passes `-c sandbox_workspace_write.network_access=true`. Without it the workspace-write sandbox blocks outbound network, so `gh` and `git push` fail inside an unattended run and Codex cannot look at a PR or issue it is asked about. This is a deliberate trade-off: with network on, a model-generated command can send data off the machine (anything it can read, and reading is allowed everywhere under workspace-write), not only change files inside its write roots. It is on because reaching GitHub is part of the work codex is woken for. If your codex does not need the network, delete that line from your copy of the script.

### Finding prior work

The wake prompt tells Codex that, before touching any checkout for work that continues a prior thread (for example "the work you did yesterday"), it should run `tincan history codex --list 20 --all` to find that thread, when it was last updated, and its real working directory, and then work in that `cwd` instead of guessing a path. `--all` matters because earlier wake runs are themselves `codex exec` runs, which the history listing leaves out by default. The command needs `tincan` on the `PATH` the listener passes to the script.

### Write roots

Codex reads anywhere but writes only in `TINCAN_CODEX_WORKDIR` and the directories passed with `--add-dir`. The operator, not the model, chooses those extra directories, by exporting `TINCAN_CODEX_WRITE_ROOTS` before starting the listener:

```bash
TINCAN_CODEX_WRITE_ROOTS="$HOME/code/agent-tincan:$HOME/Documents/Codex/site" \
  tincan listen --exec ~/agent-tincan/examples/codex/codex-wake.sh
```

It is a colon-separated list of absolute paths to existing directories. For each one the script resolves the canonical path (following symlinks and `..`) and adds it as `--add-dir` only if that path is at or under an allowed root. The allowed roots default to `$HOME/Documents/Codex`, `$HOME/code` and `$HOME/tincan-codex`; override them with `TINCAN_CODEX_ALLOWED_ROOTS`, also colon-separated and also canonicalized. Anything else is skipped with a note on stderr: a relative path, a missing directory, a path outside every allowed root, a `../` path that climbs out of one, a sibling that merely shares a root's name prefix (`$HOME/code-old` is not under `$HOME/code`), and a symlink inside an allowed root that points outside it. The script resolves and checks these paths; nothing in the prompt or the model's output can add a write root.

## Limits

- No persistent session: Codex starts over on every wake, so the whole exchange, check_inbox, each reply, and any teammate asks it makes, has to finish inside that one `codex exec` run.
- No channel and no background wait: Codex has neither Claude Code's channel preview nor a runtime that can keep a background process alive across turns the way Muse's does, so `command` wake through `tincan listen --exec` is the only wake method that fits.
- Wake messages, including the `command` nudge, carry only the waiting count, never request content.
- Verified live on the owner's Mac: codex joined beside claude-code answers round trips, including slow replies woken through the listener.
