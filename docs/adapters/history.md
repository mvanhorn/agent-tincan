# History agent

The `history` agent answers teammates' questions about what the owner (you) asked in eight places: ChatGPT (chatgpt.com), claude.ai, Grok (grok.com), Gemini (gemini.google.com), Microsoft Copilot (copilot.com, a personal Microsoft account), Codex (CLI and desktop app), Claude Code and Grok CLI (xAI's Grok Build, the `grok` command). Ask it things like "what was the last thing I asked ChatGPT? send the image" or "find my recent Codex thread about the relay" (your name in place of "I" works too) and it replies with the prompt, a short excerpt of the answer, and the images from that turn as real attachments.

It is a Go service, `tincan history serve`, that runs on your Mac under launchd (or a systemd user unit on Linux), outside any Codex sandbox. It is not an LLM agent. For each request it:

1. Checks access. By default any agent joined to your relay may ask. If you wrote an allowlist file, every agent in the request's chain, as the relay recorded it, must be on it; otherwise it declines and names the agent.
2. Turns the question into a structured query (source, mode, search terms, conversation id, count, whether images are wanted, and whether to pick the most recent turn that had images) with one tool-less `codex exec` call that sees only the question text. A request that is already a structured query (see [Structured queries](#structured-queries)) skips this step and never reaches a model.
3. Reads the source: Codex, Claude Code and Grok CLI from their local logs, ChatGPT, claude.ai, Grok, Gemini and Copilot live through the Tincan Chrome extension in your logged-in Chrome. Grok is read only once you grant the extension grok.com on its options page (see [Install](#install)), and Gemini and Copilot only once they are granted there too (see [web-agents.md](web-agents.md#gemini) for how Gemini is read and [web-agents.md](web-agents.md#copilot) for Copilot). Reading Copilot's chat list opens copilot.com in a background tab of the extension's own for a few seconds, like a send, because the list has no data the extension may fetch; each conversation is then read from copilot.com's page data with your cookies. The extension never reads Copilot's tokens.
4. Fills in a fixed reply template and attaches the images.

By default, lookups cover the 50 most recent conversations per source, up to 30 days old. You can change that window (see [Window](#window)).

It answers about what you typed, not what your agents typed. Codex `codex exec` runs, Claude Code SDK runs and grok-cli wake runs are left out, and so are the ChatGPT, claude.ai, Grok, Gemini and Copilot conversations that the chatgpt-web, claude-web, grok-web, gemini-web and copilot-web agents sent messages into (they list them in `~/.config/tincan/web-agent-<site>-conversations.json`, ids only). `tincan history <source> --all` includes them, marked as automated.

## Grok CLI

Grok CLI sessions are read from your own Grok home, `$GROK_HOME` or `~/.grok`: each session is a folder `sessions/<url-encoded working directory>/<session id>/` holding `chat_history.jsonl` (the messages) and `summary.json` (times, title, working directory), and `sessions/<url-encoded working directory>/prompt_history.jsonl` gives the time each prompt was sent. Your prompts are the user messages Grok did not add itself: its system reminders, project instructions and the environment block it opens each session with are skipped, and so are tool calls and tool output. The reply is the last thing Grok said in that turn. Files and folders the reader does not recognize are ignored. If the folder is missing or empty, the reply is "No Grok CLI history was found on this machine" (on the command line, `no Grok CLI history found in ~/.grok/sessions`).

Say "Grok CLI" to ask about these: "what did I last ask Grok CLI?" reads Grok CLI sessions, and "what did I last ask Grok?" reads your grok.com chats.

The grok-cli teammate's wake ([grok-cli.md](grok-cli.md)) runs Grok with a Grok home of its own, so its sessions are not in yours at all. In case one is (a wake set up with your own Grok home, for example), a session counts as a wake run, left out unless `--all`, when any of these holds:

- its id is in a wake's recorded list, `~/.config/tincan/<name>.wake-sessions` (the wake writes each session id there before the run, so a run the timeout killed is covered);
- its working directory is a wake's working directory (the list's `workdir` line) or inside a wake home (`~/.config/tincan/<name>.wake`);
- it ran with the wake's sandbox profile (`tincan-wake`), or with a wake home's `.grok` as its Grok home.

The reader looks for wake lists and homes only in `~/.config/tincan`, so keep a grok-cli teammate's tincan config there.

## Allowlist

By default there is no allowlist file and every agent joined to your relay may ask. The relay only delivers requests from agents that joined it, so "every agent" means every agent on your tailnet mesh. The startup log says `allowlist: all joined agents (no file at ~/.config/tincan/history-allow.txt)`.

To restrict it, write `~/.config/tincan/history-allow.txt` with one agent name per line (commas and spaces also separate names, `#` starts a comment):

```
# agents that may read the owner's conversation history
grokbot
claude-code
codex
```

A file of names allows only those names. A `*` entry means every joined agent, the same as having no file. An empty file allows nobody. The file is reread for every request, so edits take effect without a restart. A file that cannot be read, or that has an entry that is neither `*` nor a plain agent name, makes the service decline everyone (at startup it refuses to start), so a typo never opens access.

With a file of names, the check covers the whole chain, not only the sender. If muse asks codex and codex asks history while handling muse's request, the chain is muse, codex and the request is declined because of muse. The chain and sender come from the relay, never from the request body, so a body that says "I am grokbot" changes nothing.

## Window

Latest and search lookups read back through a window: by default the 50 most recent conversations per source, none older than 30 days. Only you, the owner, set it. Nothing in a request can change it: a question like "in the last 90 days" still uses your window.

For the history service, write `~/.config/tincan/history-window.json`:

```json
{"days": 90, "max": 100}
```

Both fields are optional; a missing one keeps its default. `days` is 1 to 3650 and `max` is 1 to 200. ChatGPT, claude.ai, Grok, Gemini and Copilot read at most 100 conversations whatever `max` says, because that is all the extension lists. Copilot's list is its sidebar, which has no dates: it is taken in the sidebar's order (newest first), each conversation's own time comes from reading it, and the day window stops the scan at the first conversation read that is older than it. A Copilot list (`tincan history copilot --list`) shows no dates. Grok's list comes in pages, and the extension reads at most 5 pages per list, so if grok.com returns short pages a Grok list can hold fewer conversations than asked for. The file is reread for every request, right after the allowlist check, so edits take effect without a restart. `tincan history serve --window-file <path>` reads another file. The startup log describes the window, for example `window: the last 50 conversations, up to 30 days (default, no file at ~/.config/tincan/history-window.json)`.

With no file, the service uses the default window. A file that is present but cannot be read, is not that JSON shape (unknown fields included), or has a value out of bounds fails every history request with "The history agent could not use its window file (history-window.json)" until you fix it, and the log says why. It never falls back to the default, so a window you narrowed never silently widens.

On the command line, `tincan history <source>` takes the same bounds as flags: `--days N` (applies to `--list` too) and `--max N`.

When the window cut an answer short, the answer says so. A search that found fewer matches than it wanted, or a latest lookup that could not be sure it saw the newest prompt, gets one extra line in the reply, such as "Only ChatGPT conversations from the last 30 days were searched, so older ones may be missing." (or "Only the last 50 ... conversations were searched" when the count ran out). On the command line the same note goes to stderr, in text and `--json` modes; `--json` output stays a bare array. A complete answer has no note.

## Structured queries

A caller that already knows its query can send it directly and skip the `codex exec` step. Put `query:` alone on the first line and one JSON object after it:

```
query:
{"source": "chatgpt", "mode": "search", "terms": ["sourdough"], "count": 3}
```

The JSON may also start on the marker line: `query: {"source": "codex", "mode": "latest"}`. The marker is matched in any case, but the first line must be exactly `query:`, or `query:` followed only by spaces or tabs and a JSON object starting with `{`. Any other first line, such as `Query: what did I ask ChatGPT`, is a normal question and goes through the query step as before.

The object takes only these fields, the same schema the query step produces:

- `source`: `chatgpt`, `claude-ai`, `grok`, `gemini`, `copilot`, `codex`, `claude-code` or `grok-cli` (required). `grok` is your own Grok chats on grok.com; `grok-cli` is your Grok CLI sessions on this machine. `gemini` is the Gemini app and website (gemini.google.com), not the Gemini CLI. `copilot` is Microsoft Copilot with a personal Microsoft account (copilot.microsoft.com, now copilot.com), not GitHub Copilot.
- `mode`: `latest`, `search` or `conversation` (required).
- `terms`: the words to search for, up to 8 of up to 100 bytes each, none blank (only with `search`).
- `conversation_id`: the conversation to read (only with `conversation`).
- `count`: 0 to 20; 0 means the default.
- `want_images`: attach the images from the answer.
- `with_images`: pick the most recent turn that had images (implies `want_images`).

Structured queries never reach a model. The allowlist check still runs first, exactly as for a question, and the JSON is then checked in Go: the request must be at most 4000 bytes and be one bare JSON object with nothing but whitespace after it (no code fence, no trailing text), have no other fields (so it cannot carry a window) and stay in bounds. Fields are checked as sent, never trimmed or dropped: `terms` or `conversation_id` on a mode that does not use them is refused, and so is a field named twice or set to `null` (leave a field out to use its default). A query that fails any check gets the failed reply "The structured query was not valid" (or "The structured query is too long") with the list of accepted fields; it is never passed to the query step instead. A valid one is read and answered with the same fixed reply templates as a question.

## Install

The only human step is installing the Tincan Chrome extension from the Chrome Web Store with its standard permission dialog (it asks for chatgpt.com, claude.ai and native messaging). Grok is optional and asks nothing at install: to let history read your Grok chats, open `chrome://extensions` > Agent Tincan History > Details > Extension options and click Grant for Grok (grok.com and its image host, assets.grok.com). Until then a Grok question is answered with that step. Gemini is optional the same way: click Grant for Gemini there, which also grants its image host. So is Copilot: click Grant for Copilot (copilot.com and copilot.microsoft.com). Until the store listing is live, load the extension unpacked:

1. Download `tincan-history-extension.zip` from the release page and unzip it into a folder you will keep, for example `~/tincan-extension`. Chrome loads it from there every time, so do not delete it. (From a repo checkout, `extension/` works the same way; see `extension/README.md`.)
2. Open `chrome://extensions`, turn on Developer mode, click Load unpacked, and pick that folder.

When the store listing goes live, add it from the store: tincan accepts the store build right away and prefers it over the unpacked one, so there is nothing to reinstall. Remove the unpacked copy whenever you like.

Then, on the Mac:

```bash
tincan invite history --kind history                               # on an admin device
TINCAN_CONFIG=~/.config/tincan/history.json tincan join <code> --relay http://tincan-relay
tincan history install --extension-dir ~/tincan-extension          # the folder you loaded
```

`tincan history install` writes three things and starts nothing:

- the native messaging host manifest, so Chrome can start `tincan history native-host` for the extension
- on macOS, `~/Library/LaunchAgents/com.agenttincan.history.plist` (from `examples/history/com.agenttincan.history.plist`, with the real `tincan` path and your home directory filled in), logging to `~/Library/Logs/tincan-history.log`
- on Linux, `~/.config/systemd/user/tincan-history.service`

It prints the command that starts the service. On macOS:

```bash
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.agenttincan.history.plist
```

On macOS the service starts through Agent Tincan.app, so it shows as "Agent Tincan" in Login Items ([README](../../README.md#macos-login-items)).

On Linux:

```bash
systemctl --user daemon-reload && systemctl --user enable --now tincan-history.service
```

On a headless Linux box, also run `loginctl enable-linger $USER` once; without it, systemd stops user services when you log out.

With `--extension-dir <the folder you loaded>` (or run from a repo checkout, where it finds `extension/`), install also tells the native host where the unpacked extension lives; the host then reloads the extension whenever those files change, so updates need no Reload click in `chrome://extensions` after the first load (see [web-agents.md](web-agents.md#extension-updates)).

Pass `--no-service` to skip the service definition. To run it by hand instead: `tincan history serve` (flags: `--config`, default `$TINCAN_CONFIG` or `~/.config/tincan/history.json`; `--allowlist`; `--window-file`, default `~/.config/tincan/history-window.json`; `--codex`, the codex binary for the query step). It stops cleanly on SIGINT or SIGTERM, finishing the request it is on. It refuses to start unless the relay confirms it is the `history` agent, so a config for another agent (say `$TINCAN_CONFIG` pointing at `codex.json`) can never claim that agent's requests.

The service needs `codex` logged in on the Mac for the query step. A service does not get your login shell's PATH, so install writes one: the directories where `codex` and `claude` were found at install time first, then `~/.local/bin`, `~/.npm-global/bin` and `~/bin`, then the system directories (`/opt/homebrew/bin`, `/usr/local/bin`, `/usr/bin`, `/bin` on macOS; no Homebrew path on Linux). If you install or move `codex` later, run `tincan history install` again.

## Onboarding

After install, check it from another agent: `tincan ask history "what was the last thing I asked Codex?"`. Agent Tincan's operator prompt routes history questions to `history`.

## Privacy

- The service reads your chats. That is its whole job. By default every joined agent may ask it, so any agent on your relay can read your conversation history. If some of your agents should not, write the allowlist file and list only the ones you trust. The allowlist governs requests to the history agent, not local shell access: an agent with a shell on your machine (such as the Codex wake, which runs `tincan history codex`, or the grok-cli wake) can read local Codex, Claude Code and Grok CLI history directly.
- With an allowlist file, access is checked on the whole relay-recorded chain, in Go, before any LLM sees the request.
- The LLM step sees only the question text, and a structured query skips it entirely. It runs as `codex exec --sandbox read-only` with `--ignore-user-config` (so no MCP servers from `~/.codex/config.toml`, including agent-tincan), `-c mcp_servers={}`, plugins, apps, the shell tool, browser use, computer use, image generation and web search disabled, `--ephemeral` (no session file), approvals off, from an empty scratch directory under `~/.config/tincan/history-scratch` that the Codex, Claude Code and Grok CLI readers never report. Its output is checked against the schema and bounds in Go before anything is read.
- Retrieved chat content is never sent to an LLM. Replies are filled in from a fixed template in Go, so text inside your chats cannot steer the service.
- Images are written to a private per-request temporary directory (0700, files 0600), uploaded to the relay, and the directory is removed after the reply, including on errors. On the relay they follow its attachment retention.
- Chrome is never quit or restarted. Live reads use the extension's fixed read operations with your existing session; no cookie or token leaves the browser.

## Troubleshooting

Replies and what to do:

- "Declined: X is not on the history allowlist": add X to `~/.config/tincan/history-allow.txt` if you want it to have access.
- "Declined: this request came through X, which is not on the history allowlist": an allowed agent was asked by X and passed the question on. Ask X's owner, or add X.
- "Declined: the history agent could not read its allowlist": fix the file's permissions or the bad name in it. The log says which.
- "Please ask a clearer question naming ChatGPT, claude.ai, Grok, Gemini, Copilot, Codex, Claude Code or Grok CLI": the query step could not tell what was asked, or asked for something out of bounds. Rephrase, for example "what was the last thing I asked ChatGPT? send the image".
- "The structured query was not valid" or "The structured query is too long": the request started with a `query:` line but its JSON was not one object with only the accepted fields in bounds, or the request was over 4000 bytes. The reply lists the fields; see [Structured queries](#structured-queries).
- "its query step failed": `codex exec` did not run. Check that `codex` is on the service's `PATH` and logged in (`codex login status`), then see `~/Library/Logs/tincan-history.log`.
- "No Codex history was found on this machine" (on the CLI, "no Codex history found in ~/.codex"; the same for Claude Code and Grok CLI): that tool has not been used on this machine yet, or keeps its history elsewhere (`CODEX_HOME`, `CLAUDE_CONFIG_DIR`, `GROK_HOME`; the service reads them from its own environment).
- "source unavailable: chatgpt: Chrome is not running": start Chrome. Local sources still work.
- "source unavailable: ...: the Tincan Chrome extension is not connected": install or enable the extension, then run `tincan history install` again.
- "source unavailable: ...: not logged in to chatgpt.com in Chrome" (or claude.ai, grok.com, gemini.google.com): log in in Chrome. Gemini is read with the first Google account signed in to Chrome.
- "source unavailable: grok: the Tincan Chrome extension has no access to grok.com": grant Grok on the extension's options page (see [Install](#install)).
- "source unavailable: gemini: the Tincan Chrome extension has no access to gemini.google.com": grant Gemini on the extension's options page (`chrome://extensions` > Agent Tincan History > Details > Extension options).
- "source unavailable: gemini: gemini.google.com showed an anti-bot check": Google showed its "unusual traffic" page. Open gemini.google.com in Chrome and complete it; Gemini reads are held back for 5 minutes after one.
- "source unavailable: copilot: not signed in to Copilot in Chrome": open https://copilot.microsoft.com in Chrome, sign in with a personal Microsoft account and finish any sign-in or terms page Microsoft shows. A work or school account is not supported.
- "source unavailable: copilot: the Tincan Chrome extension has no access to copilot.com": grant Copilot on the extension's options page.
- "source unavailable: ...: chatgpt.com changed its API": the site changed its internal endpoints; the extension needs an update.
- "source unavailable: ...: Chrome did not answer in time": Chrome was busy or asleep; ask again.
- "The images could not be attached: this relay does not support attachments": upgrade the relay. The text answer is still correct.
- "No matching ... conversation in the recent window": nothing inside the window the reply names matched. Try other words, or widen the window (see [Window](#window)).
- "The history agent could not use its window file": `~/.config/tincan/history-window.json` is unreadable, not `{"days": N, "max": N}`, or out of bounds. Fix or delete it. The log says which.
- "... were searched, so older ones may be missing": the window stopped the lookup before it was complete. Widen the window if the conversation is older.
- No reply at all: check the service is loaded (`launchctl print gui/$(id -u)/com.agenttincan.history`) and read `~/Library/Logs/tincan-history.log`. A "403" there means the config is not joined as an agent.
