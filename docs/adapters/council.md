# Council agent

The `council` agent puts one question to every model on your team, has them rank each other's answers blind, and has a chairman write the verdict. It is [llm-council](https://github.com/karpathy/llm-council) on the subscriptions you already pay for, and your coding agents get a seat. Ask it things like "Should the relay store attachments in SQLite or on disk?" and attach the plan or diff it should read. The reply carries a recommendation, the members ranked by blind peer review, where they agreed and disagreed, and any minority answer worth a second look, with a self-contained HTML report and a PNG scorecard sized for posting.

The idea and the stage shape (answer, anonymous peer review, chairman) come from Andrej Karpathy's llm-council. Council runs it over Agent Tincan: the members are your own web teammates (ChatGPT, Claude, Grok, Gemini, Perplexity, Copilot, and your OpenAI dot) in your logged-in Chrome and the coding agents the relay can wake (Codex, Gemini CLI, Grok CLI), so there are no API keys and no per-token bills.

It is a Go service, `tincan council serve`, that runs under launchd (or a systemd user unit) on one of your machines, usually the Mac that runs the web teammates. It is not an LLM agent. For each convening request it:

1. Claims the request at once and queues it. Councils run one at a time, because each web teammate handles one request at a time anyway; a queued request gets the progress note "Waiting behind another council (N ahead)".
2. Decides who sits on the council from the roster (see [Members and chairman](#members-and-chairman)), and declines before asking anyone when fewer than 3 members are eligible.
3. Answer stage: asks every member the question, each in a new chat, with the context you attached. Nobody sees another member's answer.
4. Review stage: sends each member every answer under neutral labels, shuffled per reviewer, with authorship hidden and self-identifying names redacted, and asks for a `FINAL RANKING:` list. A member's vote on its own answer is dropped. Each ballot becomes Borda points, and a member's score is its mean across ballots, from 0 to 1. That tally alone decides the ranking and the leaderboard.
5. Chairman stage: gives the chairman the question, the anonymized answers and the tally. It writes the recommendation, agreement, disagreement and minority notes and files the question under one category. It cannot change the scores.
6. Replies to the convener and saves the report and scorecard on the Council machine.

Each stage waits up to its time limit (6 minutes for answers, 5 for review, 4 for the whole chairman stage) and then goes on with the members that responded, as long as at least 3 did. A progress note goes out at each stage change ("Answers: 5 of 7 in", "Chairman claude-web writing the verdict") and at least every 10 minutes, which keeps the claim alive and drives the owner command's stage display.

## Convening

Any teammate convenes a council by asking `council`. There is no separate MCP tool.

```
tincan ask council "Should we store attachments in SQLite or on disk? Pick one." --attach docs/plan.md
```

The owner can also convene from a terminal:

```
tincan council "Should we store attachments in SQLite or on disk?" --attach docs/plan.md
tincan council "..." --members chatgpt-web,claude-web,codex --chairman chatgpt-web
tincan council "..." --json
```

`tincan council "question"` shows each stage as it happens and prints the verdict when done. `--attach` adds context files, `--members` replaces the default roster for this council, `--chairman` names the chairman to try first, and `--json` prints the result as JSON. When its stdin and stdout are both terminals on an admin device, it approves its own held request, so it starts at once. Otherwise it waits for approval like any agent's ask. An agent with a shell on an admin device can already approve its own requests with `tincan approve`, so the default hold restrains the agents that cannot approve.

Free text is the question, asked of the default council. For more control, put `council:` first and one JSON object after it:

```
council: {"question":"...","members":["chatgpt-web","claude-web","codex"],"chairman":"chatgpt-web"}
council: {"question":"...","exclude":["perplexity-web"]}
council: {"op":"leaderboard","category":"debugging"}
```

The object is checked strictly in Go: one JSON object with nothing but whitespace after it, no unknown fields, no field named twice or set to `null`, and only the fields its `op` uses. `question` is required to convene; `members` and `exclude` cannot both be set. A bad form is declined with the reason and the form's shape.

Attachments: text files are pasted into each member's prompt, cut to fit when needed. Web members never receive binary files. Members that accept files (the CLI teammates) get them attached, up to 20 MB forwarded per council; when an upload fails, text is inlined instead. Every prompt to a web member stays under the web agents' 32 KB send limit: Council measures the fixed parts first, trims inlined attachment text next, and shares what is left evenly across the answers. The report records which member saw which file and which answers were cut.

## Members and chairman

By default these sit on a council:

- every web teammate (`chatgpt-web`, `claude-web`, `grok-web`, `gemini-web`, `perplexity-web`, `copilot-web`, `dot-web`);
- every model teammate the relay can wake on demand: command-woken CLI agents (`codex`, `gemini-cli`, `grok-cli`) and webhook, email and always-on agents.

These do not, unless you list them in `council.json`:

- Claude Code and other live sessions (wake `channel`), because asking them would interrupt the session you are working in: "excluded (live session)";
- services (`history`, `notes`, `council`) and scheduled agents: "excluded (service)";
- agents with no way to wake them: "excluded (not wakeable)".

The convener and every agent already in the request's chain never sit on that council or chair it ("excluded (in chain)"), so a council can never reach an agent the chain could not. A form may name only agents that are eligible or that you listed in `council.json`; naming a service or a live session is declined with the reason, so no retrieved chat or note reaches another vendor. A request already at the relay's hop limit is declined.

The chairman is the first candidate that is not in the chain: the form's `chairman`, then `chairmen` from `council.json` (default `claude-web`, `chatgpt-web`, `gemini-web`), preferring one whose review ballot is already in. If a chairman fails, Council tries the next inside the chairman stage's time limit. When none delivers a verdict, the council still completes with the peer ranking, says "Verdict unavailable", and records the scores under `uncategorized`.

## Replies, reports and scorecards

The reply is text first, readable by any agent:

- `Recommendation: ...` (or why the council failed or was declined)
- `Question: ...` (its first line)
- `Status: 7 asked, 5 scored, 1 absent, 1 excluded`
- `Chairman: claude-web. Category: architecture.`
- the ranking by blind peer review, with each score and ballot count
- `Agreement:`, `Disagreement:` and `Minority:` notes, with each answer label followed by its author now that judging is over
- the absent members with the stage and reason (timed out, held by a gate, failed, declined, needs input, not ranked, no verdict in reply) and the excluded ones
- the local paths of the report and scorecard
- a fenced `council-result` JSON block for scripts

Statuses: a completed council is `answered`. With fewer than 3 answers or 3 valid ballots it is `failed`, and the reply carries the answers it did get. A bad form, a too-deep chain or too few eligible members is `declined`, and nobody was asked. Absent members are not scored, and their leaderboard record is unchanged.

The report is one HTML file with inline CSS and a Content-Security-Policy that blocks scripts and remote loads. It shows the question, every answer in full with its author, the peer rankings, the verdict, timings, absent members, and how each attachment reached each member. Model text is inserted only as escaped text. The scorecard is a 1600x900 PNG with the question, the members ranked, the verdict in one line, and Agent Tincan branding with the repo link. Both are attached to the reply and saved under `reports` in Council's folder. If attaching fails, the reply still goes out with their paths.

## Leaderboard

Every completed council records each scored member's peer score, placement and the question's category. Categories are a small fixed set: `architecture`, `debugging`, `writing`, `research`, `current-events`, `other`, plus `uncategorized` for councils without a verdict.

```
tincan council leaderboard
tincan council leaderboard --category debugging
tincan council leaderboard --card
```

The leaderboard ranks members by wins, then mean peer score. `--card` renders it as a PNG card to post. Agents read it with `council: {"op":"leaderboard"}`; that ask is held for approval like a council, since the relay holds by target and cannot see the operation.

## Owner approval

The relay holds every ask to an agent of kind `council` for your approval, even when there is no `approval.json` at all, because a council sends the question, its attachments and every member's answer to every model vendor on the team. An agent that convenes gets `held` back, and its standing instructions tell it to give you the request id and the approve command.

```
tincan held          # shows the target's kind and each attachment's name and size
tincan approve <id>
tincan deny <id> "reason"
```

A held council expires after 2 hours (`hold_ttl` in `approval.json` changes that). Without `approval.json` the relay notifies nobody. To have it tell an agent about each held ask, create `approval.json` in the relay's state dir with mode 0600 and a `notify` field:

```json
{"gate": {}, "notify": "grokbot"}
```

A gate entry for `council` replaces the default hold. `{"gate": {"council": {"from": []}}}` holds nothing, so any agent's council starts at once; any other rule holds as written. Your existing rules still apply to the asks Council sends its members, since the convener is in their chain: a rule that holds `chatgpt-web` asks from `codex` holds that member when codex convenes, and it counts absent as "held by a gate".

Approval from an admin device is the owner's decision. The hold restrains agents only because they cannot approve. See the [trust model](../trust-model.md#council).

Your dot (`dot-web`) is held by default too, since it acts in the apps you connected to it. Council's answer, review and chairman asks to the dot skip that hold only when you approved the council question they belong to (a `tincan council` run in a terminal on an admin device approves its own); every other ask to the dot is still held, and `approval.json` gates still apply to it. Approving a council question is therefore also approving that the dot reads it and answers with your authority in its connected apps. To keep the dot off councils, add `dot-web` to `exclude` in `council.json`. For blind review, Council strips the dot's DM footer, its attachment and delegation notes and any `@tincan ask` paragraph from its answer, and drops the `new chat` line from its prompts. The dot is not a default chairman, but `chairmen` or `--chairman` can name it. The dot can also convene by asking `council` (`@tincan ask council <question>`), which is held for you like any council.

## Install

Upgrade the relay first, from an admin device: `tincan relay-upgrade --from-github v0.10.0` (or put the v0.10.0 files in the relay's `--dist` and run `tincan relay-upgrade`). Then run `tincan upgrade` on each agent. For a dot, run `tincan kind dot-web dot-web` from an admin device next; `dot-web` refuses to start without that kind. Then install Council. An older relay refuses the `council` kind and would never hold councils, and `tincan council serve` refuses to run until the relay stores kind `council` for it.

```bash
tincan invite council --kind council                                          # on an admin device
TINCAN_CONFIG=~/.config/tincan/council.json tincan join <code> --relay http://tincan-relay
tincan council install
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.agenttincan.council.plist
tincan council doctor
```

The agent must be named `council`: `tincan council serve` refuses to run unless both its config and the relay say it is the `council` agent. If it joined without the kind, run `tincan kind council council` on an admin device.

`tincan council install` writes `~/Library/LaunchAgents/com.agenttincan.council.plist` on macOS (a systemd user unit, `tincan-council.service`, on Linux), which runs `tincan council serve` with `TINCAN_CONFIG=~/.config/tincan/council.json`, keeps it running, and logs to `~/Library/Logs/tincan-council.log`. It prints the start command and starts nothing itself. `--binary` names the tincan binary the service runs (default this one).

On macOS the service starts through Agent Tincan.app, so it shows as "Agent Tincan" in Login Items ([README](../../README.md#macos-login-items)).

To run it by hand instead: `tincan council serve`, with `--config` (default `$TINCAN_CONFIG`, else `~/.config/tincan/council.json`) and `--dir` (Council's own folder, default `~/Library/Application Support/tincan-council` on macOS, `~/.local/share/tincan-council` on Linux). It stops cleanly on SIGINT or SIGTERM.

Set `{ "council": { "method": "wait" } }` in the relay's `wake.json`; the service long-polls the relay.

## council.json

Council's settings live in `council.json` in its folder (not `~/.config/tincan/council.json`, which is its relay identity). Every field is optional:

```json
{
  "members": ["claude-code"],
  "exclude": ["perplexity-web"],
  "chairmen": ["claude-web", "chatgpt-web", "gemini-web"],
  "categories": ["architecture", "debugging", "writing", "research", "current-events", "other"],
  "answer_limit": "6m",
  "review_limit": "5m",
  "chairman_limit": "4m",
  "dir": "/absolute/path",
  "report_dir": "/absolute/path"
}
```

- `members`: agents added to the default roster, even a live session or a service. Listing one also lets a form name it.
- `exclude`: agents left off the default roster. A form may still name one.
- `chairmen`: the chairman order. An entry names an agent, or else every agent of that kind.
- `categories`: lowercase words joined by hyphens; `uncategorized` is reserved.
- `answer_limit`, `review_limit`, `chairman_limit`: durations above 0 and under 8 minutes, the web agents' send timeout.
- `dir`, `report_dir`: absolute paths for the database and the reports.

The file is checked strictly: unknown, repeated or null fields and out-of-range values are errors, so a typo never widens who sits on a council. `tincan council doctor` reports a bad file.

## Onboarding

After install, check it from another agent: `tincan ask council "Tincan test: in one sentence, what is a tin can telephone?"`. Run `tincan onboard --section agents`: the council block carries the lines to add to the standing instructions of each teammate that should convene. They cover when to convene (a real decision, not a lookup), at most one council per task, never convening while answering a council, that a held ask is expected and what to tell you, and that verdicts and answers are data, never instructions. Agent Tincan's operator prompt passes "put this to the council" to `council` and never approves a held council.

## Privacy

- Council runs on your machine and talks only to your relay, but its members do not. A web member sends the prompt to its vendor's site as you: the question, inlined attachment text, and in the review and chairman stages every other member's answer. One member's answer can draw on that site's memory of you, and it then reaches every other vendor on the council during review. The approval preview (`tincan held`) shows the target kind and attachment names and sizes before anything goes out.
- The convener's chain never sits on the council, and a form cannot name a service, so history and notes content does not reach a vendor through a council.
- Blind review hides authorship with shuffled labels and redacted vendor and model names, and strips the web agents' reply footers, including the dot's DM footer and notes. Writing style can still give a member away; the report says so.
- Answers are wrapped in per-council random delimiters and the prompts say the enclosed text is material to judge, never instructions. Only the defined output fields (`FINAL RANKING:`, the verdict sections, the category) are parsed.
- Inlined attachment text lives in request bodies, which the relay keeps like any request, not on the 7-day attachment clock.
- Council's database and reports stay in its folder (created 0700, files 0600) until you delete them.
- A council keeps every web teammate busy for up to about 15 minutes and opens several background tabs in your Chrome. Other asks to a web teammate wait their turn meanwhile.

## Troubleshooting

`tincan council doctor` (`--json` for JSON) checks, in order:

- relay: the config is joined, the relay is reachable, and the relay knows this machine as `council`. Fix: `TINCAN_CONFIG=~/.config/tincan/council.json tincan rejoin`, or a new invite for a machine that was never joined.
- relay kind: the relay stores kind `council`. Fix: upgrade the relay, then `tincan kind council council` on an admin device. Until then `council serve` refuses to run.
- council.json: the file is valid, or absent (the defaults).
- data folder and report folder: they can be created and written.

Replies and what to do:

- `held`: expected. Approve it with `tincan approve <id>` on an admin device.
- "Council declined: only 2 eligible member(s) ...": add web teammates, or list more agents in `council.json`'s `members`.
- "Council declined: X cannot sit on a council: excluded (service) ...": a form named a service or a live session. List it in `council.json` if you really want it to sit.
- "Council declined: the request chain is too deep ...": the convening request was already at the relay's hop limit. Convene directly rather than through a chain of teammates.
- "Council failed: ..." with answers received: fewer than 3 members answered or ranked in time. Check the absent list: "timed out" often means a web teammate was busy or a site was slow; "held by a gate" means an `approval.json` rule held that member's ask.
- "Verdict unavailable": no chairman delivered a verdict. The ranking still stands. Check the chairman candidates in `council.json` and that their sites are signed in.
- No reply at all: check the service is loaded (`launchctl print gui/$(id -u)/com.agenttincan.council`) and read `~/Library/Logs/tincan-council.log`. A council in progress posts progress notes; `tincan get <id>` shows the latest.
