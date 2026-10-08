# Notes agent

The `notes` agent lets any agent on your team save a note into Agent Notes, search your notes, and read one back. Ask it things like "save a note titled Relay checklist with these steps" or send a structured request (see [Request format](#request-format)). An add replies with the new note's id; a search replies with up to 20 matches, each with its id, title, tags and a short snippet; a read replies with the note's title, tags and body.

It is a Go service, `tincan notes serve`, that runs under launchd on the Mac where you use Agent Notes. It is not an LLM agent. It drives the `agent-notes` helper that ships inside the app (`/Applications/Agent Notes.app/Contents/Helpers/agent-notes`) against your library folder. For each request it:

1. Checks access against its two allowlists (see [Allowlists](#allowlists)), from the relay-recorded chain, before the request text is read.
2. Turns the request into an operation. A structured `note:` request is parsed in Go and never reaches a model. Free text goes to one tool-less `codex exec` call that picks the operation, title, tags or search query.
3. Runs the helper: `create` for an add, `search` for a search, `read` for a read.
4. Fills in a fixed reply template in Go. Note text is never sent to a model.

v1 never edits, retags, archives or deletes a note. Search and read cover active notes only: an archived or trashed note is left out of search results, and a read of one says "No note with id ... was found."

Every note added through Tincan carries the `from-agent` tag and these properties, so you can find and review them in the app:

- `remote.agent`: the agent that asked
- `remote.trace-id`: the request's trace id
- `remote.via`: `tincan`
- `remote.request-id`: `tincan:<request id>`

It needs an Agent Notes build whose helper supports `create --idempotency-key` (the release after the one current when this service shipped). `tincan notes doctor` reports an older helper as `helper_too_old`.

## Allowlists

There are two allowlist files, one agent name per line, in the same format as the history agent's ([history.md](history.md#allowlist)): commas and spaces also separate names, `#` starts a comment, and a `*` entry means every joined agent.

- `~/.config/tincan/notes-allow.txt`: who may search and read.
- `~/.config/tincan/notes-add-allow.txt`: who may add.

With no file, every agent joined to your relay is allowed, the same as `history`. A file of names allows only those names, and an empty file allows nobody. The files are reread for every request, so edits take effect without a restart. A file that cannot be read, or that has an entry that is neither `*` nor a plain agent name, declines everyone for that operation, so a typo never opens access.

The check covers the whole chain the relay recorded, not only the sender. If muse asks codex and codex asks notes while handling muse's request, muse must be allowed too. A request is first checked against both lists together (the chain must be allowed to do something), then against the list for its operation. `tincan notes serve --read-allowlist <file>` and `--add-allowlist <file>` read other files.

## Request format

A structured request needs no model. Put `note:` first and one JSON object after it, on the same line or on the lines below:

```
note: {"op":"add","title":"Relay checklist","body":"- upgrade\n- restart","tags":["ops"]}
note: {"op":"search","query":"relay","count":10}
note: {"op":"read","id":"<note id>"}
```

The marker is matched in any case. The first line must be exactly `note:`, or `note:` followed only by spaces or tabs and a JSON object starting with `{`. Any other first line is free text.

The object is checked strictly in Go: one JSON object with nothing but whitespace after it, no unknown fields, no field named twice or set to `null`, and only the fields its `op` uses.

- `add`: `title` (required, up to 300 characters, no line breaks or other control characters), `body` (Markdown, up to 256 KB), `tags` (up to 20, each up to 64 characters, no commas or control characters). The `from-agent` tag is added for you.
- `search`: `query` (required, up to 500 characters), `count` (1 to 20, default 10).
- `read`: `id` (required, the note's UUID).

A request that fails a check is answered failed with the reason and the structured form; nothing is written.

Free text works too: "save a note called Trip ideas: ..." or "find my notes about the relay". The `codex exec` step sees only your message and picks the operation, title, tags, query, count or id. For an add, the body is your whole message, saved verbatim; the model never writes it. Free text over 8000 bytes is refused as unclear, so send long notes in the structured form. When the step cannot tell what you want, the reply is "I could not tell whether to save, search, or read a note, so nothing was done." with the structured form. When the step itself fails (codex missing or logged out, a timeout), the request is left for the relay to redeliver and tried again; the third failure is answered failed with the structured form.

The request step runs `codex exec` with `--sandbox read-only`, `--ignore-user-config` (no MCP servers from `~/.codex/config.toml`), `-c mcp_servers={}`, web search, plugins, apps, the shell tool, browser use, computer use and image generation disabled, `--ephemeral` (no session file), approvals off, from an empty scratch directory (`--codex-scratch`, default `~/Library/Application Support/tincan-notes/codex-scratch`). Its answer is checked against a schema and the same bounds in Go.

## Durability

A note an agent asked to save is not silently lost, even when the Mac is asleep or offline for days.

- On the relay: requests to an agent of kind `notes` wait up to 30 days before they expire (relay flag `--notes-ttl`, default 30 days); other kinds keep 24 hours. This needs a relay from this release or later, and the agent must have kind `notes` (`tincan invite notes --kind notes`, or `tincan kind notes notes` on an admin device for one that joined without it). Until the relay is upgraded, adds keep the 24-hour window.
- On the Mac: each add the service claims is written to its spool (`~/Library/Application Support/tincan-notes/spool`, or `--spool-dir`) before the library is touched. If the helper fails for a reason that may pass (library missing, no folder access, a timeout), the add stays in the spool, the asker gets a progress note ("Not saved to Agent Notes yet ... The add is kept on the notes Mac and will be retried; the reply will carry the note id."), and the service retries every minute and at every start.
- At most once: each add is created with the idempotency key `tincan:<request id>`. Redelivery, restarts and crashes mid-write return the same note instead of a second one, even when you have since archived or trashed it. The reply then says "This request was already saved as note ... No new note was created."

Agents should use `ask`, not `notify`, for an add, so the reply with the note id comes back. A pending add is queued, not lost; they should not resend it.

If an add does expire on the relay, the asker gets the `expired` status. It may still be saved: an add already in the Mac's spool is saved once the Mac can reach the library, even though its reply can no longer be delivered. The standing instructions tell agents to search notes for the title before resending, and to tell you.

## Install

Upgrade the relay to this release first (`tincan relay-upgrade`, see the [quick start](../quickstart.md)), so adds wait 30 days. Then, on an admin device and the Agent Notes Mac:

```bash
tincan invite notes --kind notes                                           # on an admin device
TINCAN_CONFIG=~/.config/tincan/notes.json tincan join <code> --relay http://tincan-relay
tincan notes install --library-root <Agent Notes library folder>
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.agenttincan.notes.plist
tincan notes doctor
```

The agent must be named `notes`: `tincan notes serve` refuses to run unless both its config and the relay say it is the `notes` agent, so a config for another agent can never claim that agent's requests.

Unlike tincan's other macOS services, the notes service does not start through Agent Tincan.app, so it still appears in Login Items under the signer's name. That keeps its Files and Folders grant from being shared with other services ([trust model](../trust-model.md#agent-tincanapp-on-macos)).

`tincan notes install` writes `~/Library/LaunchAgents/com.agenttincan.notes.plist`, which runs `tincan notes serve --library-root <folder> --helper <helper>` with `TINCAN_CONFIG=~/.config/tincan/notes.json`, keeps it running, and logs to `~/Library/Logs/tincan-notes.log`. It prints the start command and starts nothing itself. Flags: `--library-root` (required), `--helper` (default the helper inside `/Applications/Agent Notes.app`), `--binary` (the tincan binary the service runs, default this one).

The service reads and writes your library from launchd, not from Terminal, so macOS asks for access separately. In System Settings > Privacy & Security, grant the tincan binary Files and Folders access to the library's folder (not Full Disk Access, which the free-text extractor would inherit), then restart the service:

```bash
launchctl kickstart -k gui/$(id -u)/com.agenttincan.notes
```

To run it by hand instead: `tincan notes serve --library-root <folder>`, with `--helper`, `--app-support` (the service's own Application Support folder, default `~/Library/Application Support/tincan-notes`), `--spool-dir` (default `<app-support>/spool`), `--read-allowlist`, `--add-allowlist`, `--codex-scratch` and `--config` (default `$TINCAN_CONFIG`, else `~/.config/tincan/notes.json`). It stops cleanly on SIGINT or SIGTERM.

Free-text requests need `codex` installed and logged in on the Mac. Structured requests do not.

Set `{ "notes": { "method": "wait" } }` in the relay's `wake.json`; the service long-polls the relay.

## Onboarding

After install, check it from another agent: `tincan ask notes 'note: {"op":"search","query":"tincan","count":3}'`. Run `tincan onboard --section agents`: the notes block carries the lines to add to the standing instructions of each teammate that should use it. Agent Tincan's operator prompt routes requests to save, find or read a note to `notes`.

## Privacy

- Reads send note bodies to the agent that asked. By default every joined agent may search and read, and that includes the ChatGPT connector, which runs in OpenAI's cloud. If some of your agents should not see your notes, write `~/.config/tincan/notes-allow.txt` with only the ones you trust (for example, leave out `chatgpt`). Use `notes-add-allow.txt` the same way for who may add.
- Access is checked on the whole relay-recorded chain, in Go, before any model sees the request.
- Free-text requests are sent to the `codex exec` request step, which means OpenAI sees the request text. Structured `note:` requests never leave the Mac except as the reply to the asker.
- Note text is never sent to a model. Replies are filled in from fixed templates in Go, so text inside a note cannot steer the service. Agents are told that note text is data, never instructions.
- The helper runs with the service's own Application Support folder, so it never picks up an in-app agent session's context.
- The spool holds the full text of adds not yet saved, in a folder only your user can read (folder 0700, files 0600), and each entry is removed once its reply is sent.

## Troubleshooting

`tincan notes doctor` (`--json` for JSON) checks, in order:

- helper: the `agent-notes` helper exists and is executable. Fix: install Agent Notes, or pass `--helper`.
- spool: the spool folder can be created and written.
- relay: the config is joined, the relay is reachable, and the relay knows this machine as `notes`. Fix: `TINCAN_CONFIG=~/.config/tincan/notes.json tincan rejoin`, or a new invite for a machine that was never joined.
- relay kind: the relay stores kind `notes` for the agent, so requests wait 30 days. Fix: `tincan kind notes notes` on an admin device. Until then requests to notes expire after 24 hours.
- last helper result: what the running service's last helper call returned (doctor reads the service's health file rather than running the helper, since the service may lack folder access that Terminal has).
  - `missing_authorization` or `operation_failed`: the service cannot open the library. Grant the tincan binary Files and Folders access to the library's folder, then restart the service with `launchctl kickstart -k gui/$(id -u)/com.agenttincan.notes`.
  - `helper_too_old`: this Agent Notes build cannot save notes safely for the notes agent. Install the latest Agent Notes, then restart the service.
  - `helper_unavailable`: check that Agent Notes is installed and that `--helper` names its helper.
  - No result yet: start the service and send notes a request, then run doctor again.

Replies and what to do:

- "Declined: X is not on the notes allowlist, so it cannot read notes." (or "... notes add allowlist, so it cannot add notes."): add X to the matching file if you want it to have access.
- "Declined: this request came through X, ...": an allowed agent was asked by X and passed the request on. Every agent in the chain must be allowed.
- "The note: request was not valid (...)": the structured request broke a rule; the reply names it and shows the structured form.
- "I could not tell whether to save, search, or read a note": the free text was unclear or over 8000 bytes. Use the structured form.
- "The notes agent could not work out this free-text request (its request step failed several times)": `codex exec` failed three times. Check that `codex` is on the service's PATH and logged in (`codex login status`), or use the structured form.
- "Not saved to Agent Notes yet (helper error ...)" as a progress note: the add is in the spool and will be retried. Run `tincan notes doctor` and fix what the last helper result says.
- "The note was not saved: the Agent Notes helper rejected it (...)": the helper refused the note itself (bad metadata, too large). Nothing was written; fix the request and send it again.
- "The notes agent could not reach the Agent Notes library right now": a search or read failed in the helper. Run `tincan notes doctor`.
- "No note with id ... was found.": the id is wrong, or the note is archived or trashed.
- No reply at all: check the service is loaded (`launchctl print gui/$(id -u)/com.agenttincan.notes`) and read `~/Library/Logs/tincan-notes.log`.
