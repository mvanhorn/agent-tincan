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

Either way this gives Codex the same tools every other agent gets: ask, get_reply, check_inbox, claim, reply, cancel, list_agents, trace. Confirm it loaded with `codex mcp list`.

## Wake

Run a listener in the background on the Codex machine:

```bash
tincan listen --exec ~/agent-tincan/examples/codex/codex-wake.sh
```

`tincan listen` holds a long-poll to the relay and runs the exec command whenever requests are waiting, passing it the count in `TINCAN_WAITING`. It does not take the requests itself; the command it runs is what picks them up. examples/codex/codex-wake.sh exports the codex agent's `TINCAN_CONFIG` (default `$HOME/.config/tincan/codex.json`, override by exporting it before the listener starts) and then runs `codex exec` non-interactively with a prompt that tells Codex to call check_inbox, handle each request, reply by request id, and keep calling check_inbox until nothing is left, so one run drains the whole inbox.

Because the relay only wakes an agent for requests addressed to it, never for replies to that agent's own outbound asks, the wake prompt also tells Codex to treat any `ask` it sends as synchronous: wait for the inline reply, or poll `get_reply`, in the same run, rather than expect a second wake for the answer.

`tincan listen` waits 30 seconds between nudges but does not wait for a previous command to finish first, and a busy relay can still see requests queued while a run is underway. codex-wake.sh guards against two overlapping `codex exec` runs with a lock directory (`mkdir`, an atomic, portable lock): if a run is already in progress it exits quietly and leaves the requests queued for the next nudge.

Set codex's wake to `command` in the relay's `wake.json`:

```json
{ "codex": { "method": "command" } }
```

codex-wake.sh runs `codex exec` with `--sandbox workspace-write -c approval_policy=never`. `codex exec` has no `--ask-for-approval` flag (that flag is interactive-mode only); `-c approval_policy=never` is the non-interactive equivalent, set via a config override. This lets an unattended run act without a human present to answer approval prompts (there is a real trade-off: model-generated commands execute without asking first), but it still confines them to the sandbox's workspace-write policy rather than full disk access. It does not use `--dangerously-bypass-approvals-and-sandbox`, which would remove the sandbox boundary entirely.

The run's working directory, and so its write scope under workspace-write, is `TINCAN_CODEX_WORKDIR` (default `$HOME/tincan-codex`, created if missing), not the whole home directory. If your requests need to write elsewhere, add `--add-dir` to the script rather than widening the working directory or reaching for the bypass flag.

## Limits

- No persistent session: Codex starts over on every wake, so the whole exchange, check_inbox, each reply, and any teammate asks it makes, has to finish inside that one `codex exec` run.
- No channel and no background wait: Codex has neither Claude Code's channel preview nor a runtime that can keep a background process alive across turns the way Muse's does, so `command` wake through `tincan listen --exec` is the only wake method that fits.
- Wake messages, including the `command` nudge, carry only the waiting count, never request content.
- Live verification (codex joined beside claude-code, answering a round trip) has not been run yet; it is part of the rollout step for this plan, not this adapter's docs.
