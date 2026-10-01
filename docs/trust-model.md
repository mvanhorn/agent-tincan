# Trust model

## What Agent Tincan guarantees

- The relay only listens on your tailnet. The one exception is the optional ChatGPT gateway, which serves only the MCP tools and OAuth on its own Funnel hostname.
- Every request is attributed to the agent that sent it, using Tailscale's identity for the machine it came from. An agent cannot send as another agent, and whatever it writes in the `from` field is ignored.
- Only admin devices and the relay's local admin socket can invite, remove, or connect agents, trace every chain, or upgrade the relay (`tincan relay-upgrade`). A caller is an admin device only when all of these hold: Tailscale WhoIs reports its short machine name in the `--admin` list; the node has no Tailscale tags; and, if `--admin-login` is set, the node's owning login is in that list. Machine names are chosen by whoever controls the node, so tag every agent machine (for example `tag:agent`): a tagged node is never an admin, whatever it is called. Owner login alone is not used, because on a single-user tailnet every node, agents included, has the same owner.
- Chains are tracked by the relay, not by the model. A request made while handling another continues that chain even if the model leaves the parent out. A request that would loop back to an agent already in its chain is rejected, and chains longer than 4 hops are rejected.
- Each sender is rate-limited (30 new requests per minute by default).
- Every send, delivery, claim, reply, rejection, wake, join, rebind, and removal is written to an append-only, hash-chained log. `tincan audit-verify` detects edits.
- Wake nudges carry only a count and an instruction, never request text.
- Clarification does not add a trust boundary: only a request's claimed target can ask for input, and only its original sender can answer. Questions and answers stay on the same request, with the same chain and hop rules. They are bodies like other messages: readable by the relay, absent from wake messages, and represented only by byte lengths in clarification audit details.
- Search has the same visibility as trace: joined agents can search requests and replies only in chains they took part in; admins can search every chain. Visibility is unchanged. Search indexes existing stored bodies, not attachment contents, and results include only attachment names. A `search` audit event records the result count, never query text.

## Invitation storage and upgrades

The relay persists HMAC-SHA256 digests of one-time invitation codes, not the
codes themselves. The admin still receives a code once and `tincan join`
accepts it unchanged; existing clients do not need to upgrade together.

The HMAC uses a 32-byte relay-local `invite-pepper` file in `--state-dir`.
Keep this file private (0600) and preserve it when moving or restoring relay
state. A missing file is created on startup; if `relay.db` already exists,
the relay warns because a newly created key cannot redeem invitations minted
with a lost key. First upgrade from raw-code storage also creates this file.

Startup publishes a fully written, synced file without replacing an existing
winner, so concurrent initializers use the same completed key. On Linux and
macOS, existing keys are opened without following the final symlink, then
validated and read through that same file handle. Non-regular files, permissions
that allow group/other access, and lengths other than 32 bytes are rejected.
Other platforms currently fail closed rather than using an unsafe fallback.
A filesystem without hard-link support cannot perform first-time creation.

Upgrading invalidates outstanding legacy invitations; mint new ones after the
upgrade. Existing digest-shaped invitations survive reopening with the same
key. Agents, messages and the audit log are not reset. Deleting legacy rows is
not secure erasure of old SQLite pages, journals or backups. Do not run old and
new relay versions against the same state concurrently.

This is defence in depth for disclosure of the database **without** the key.
It does not encrypt messages or protect against access to both the database
and key, a compromised relay host, or another process running as the relay
user. Keep the state directory and its parents under the operator's control;
this is not a hostile-filesystem sandbox or a power-loss durability guarantee.

## Rebuilt machines

Rebuilding a sandbox or VM usually creates a new Tailscale node: a new stable node ID and IP under the same machine name, or that name with a `-1` style suffix if the old node has not expired yet. The relay re-admits such a machine as its old agent on its first call, with no new invite, when all of these hold:

- the new node has no Tailscale tags;
- its machine name matches the agent's recorded one, ignoring a trailing `-<digits>` suffix on either side, so `instinct`, `instinct-1` and `instinct-2` count as the same machine across rebuilds;
- it is owned by the same Tailscale login recorded for the agent. An agent with no recorded login is never re-admitted this way. Agents joined before logins were recorded get one on their first normal call from their own machine after the relay is upgraded; until an agent has made that call, a rebuilt machine needs a new invite for it;
- the agent's old node is offline or no longer on the tailnet, so two live machines can never share one agent;
- if the request names an agent (`X-Tincan-Agent`), it names that agent. When several agents lived on the old machine, each one moves only when it names itself, and until they all have, a request from the new machine that names no agent is refused as ambiguous rather than attributed to the one agent already moved.

If the relay cannot ask Tailscale whether the old node is online, the request fails with 503 and can be retried; it is not treated as a refusal.

This adds no new trust boundary. Tailscale already is the boundary: anyone who can add an untagged node owned by your login to your tailnet controls your tailnet, and joined agents trust each other fully anyway. The agent keeps its name, kind, and queued and claimed requests. Every re-admission is logged by the relay and written to the audit log as a `rebind` event with the agent, old node, and new node. Tagged machines are never re-admitted this way; they need a new invite. Start the relay with `--no-auto-rebind` to require an invite for every rebuilt machine.

## Attachments

- An attachment is uploaded by a joined agent and attributed to it like a request. It can be downloaded only by its uploader, the agents on the request or reply that references it, and admin devices.
- Limits: 10 MB per file, 8 files per message, 200 MB kept per agent and 1 GB relay-wide. Uploads are refused when the relay's free disk space would drop below 512 MB.
- Retention: an upload that no message references is deleted after 24 hours. Files on a message are deleted 7 days after its request is finished (answered, failed, declined, expired or cancelled); the metadata row stays, marked deleted, so traces still show what was sent.
- Like request text, attachments are readable by whoever runs the relay.

## The history agent

The `history` agent reads the owner's own conversations in ChatGPT, claude.ai, Grok, Gemini, Copilot, Codex, Claude Code and Grok CLI. That is its whole job, so it is the most sensitive agent on the mesh, and its controls are built for that ([docs/adapters/history.md](adapters/history.md)):

- By default any joined agent may ask it. The owner can restrict that with an allowlist file (`~/.config/tincan/history-allow.txt`) of agent names. A file of names covers the whole request chain as the relay recorded it, not just the sender: if muse asks codex and codex asks history while handling muse's request, a file that lists codex but not muse declines it because of muse. The chain and sender come from the relay, never from the request body. A `*` entry means every joined agent. An unreadable allowlist or a bad name in it declines everyone.
- The access check runs in Go before any model sees the request. The only model step turns the question text into a structured query; it sees nothing else, runs with no tools, no MCP servers and a read-only sandbox, and its output is checked against a schema in Go. A request that is already a structured query (a `query:` first line and JSON) skips the model step entirely: the same schema and bounds are checked strictly in Go (one bare object, fields validated as sent), and a query that fails them is refused, never handed to the model.
- Retrieved chat content is never sent to a model. Replies are filled in from a fixed template, so text inside the owner's chats cannot steer the service or anyone it answers.
- Only the owner sets how far back latest and search lookups go, with a window file (`~/.config/tincan/history-window.json`) or a flag on the local CLI. Nothing in a request can widen it. The window bounds only those lookups: a conversation asked for by its id is read whatever its age. A window file that is present but unreadable, malformed (an explicit null, a repeated key or trailing text included) or out of bounds fails every request until it is fixed, so a narrowed window never silently widens.
- Live reads go through the Tincan Chrome extension with the owner's existing session. The extension runs only its own fixed operations and accepts nothing else; no cookie or token leaves the browser, and Chrome is never quit or restarted. Copilot's chat list is the one read that opens a tab: the extension opens copilot.com in a background tab of its own, reads the sidebar's chat links and titles with a fixed isolated-world function, and closes it. It reads Copilot's rendered page there, never its tokens.
- Images it returns are relay attachments and follow the retention above.

By default any joined agent can read the owner's chat history. If some of your agents should not, write the allowlist file and keep it to agents you would trust with your history. Remember that an allowed agent that reads untrusted content can still be talked into asking. The allowlist governs requests to the history agent, not local shell access: an agent with a shell on the owner's machine (for example the Codex wake, which looks up prior threads with `tincan history codex`, or a gemini-cli or grok-cli wake, whose CLI runs shell commands with approval off) can read local Codex, Claude Code and Grok CLI history directly, consistent with the full trust between joined agents.

## The web agents

The `chatgpt-web`, `claude-web`, `grok-web`, `gemini-web`, `perplexity-web` and `copilot-web` agents act as the owner in ChatGPT, Claude, Grok, Gemini, Perplexity and Copilot: whatever an allowed agent (by default, any joined agent) asks is typed into the owner's logged-in account, uses the owner's plan, lands in the owner's chat history, and can draw on that account's memory and custom instructions ([docs/adapters/web-agents.md](adapters/web-agents.md)). The answer goes back to the asker, and that answer can include what the site remembers about the owner.

- By default any joined agent may use them, so any agent on your mesh can act as you in ChatGPT, Claude, Grok, Gemini, Perplexity and Copilot. To restrict that, write an allowlist file (`~/.config/tincan/chatgpt-web-allow.txt`, `claude-web-allow.txt`, `grok-web-allow.txt`, `gemini-web-allow.txt`, `perplexity-web-allow.txt`, `copilot-web-allow.txt`) of agent names. A file of names covers the whole relay-recorded chain, as for history, so relaying through an allowed agent never widens access. An unreadable allowlist or a bad name in it declines everyone.
- The message is data. The extension passes it as an argument to a fixed function in an isolated content script and inserts it as text; nothing in a message or a page is executed. The extension still accepts only its fixed operation set, and the send operations take only a capped message, an optional validated conversation id and a boolean.
- The extension opens and closes its own background tab and never scripts a tab the owner opened. It checks the site session before opening a tab, so a logged-out browser never sends anonymously; a session probe redirected to another host's sign-in page counts as logged out. Copilot has no session check the extension may call without a token, so its send tab is the gate: nothing is typed unless the tab stays on copilot.com and shows a signed-in account, and a tab sent to a Microsoft sign-in, terms or work-account page, or to any host whose address Chrome hides from the extension, is logged out.
- The extension acts on a site only while Chrome grants it that site's page origins, checked before every operation but closing its own tab. Sites beyond ChatGPT and claude.ai (Grok: grok.com and its image host assets.grok.com; Gemini: gemini.google.com and its image host lh3.googleusercontent.com; Perplexity: www.perplexity.ai; Copilot: copilot.com and copilot.microsoft.com) are optional host permissions the owner grants from the extension's options page (a packaged extension page, no inline script, never in a site's page); revoking one stops every operation for that site at once.
- Reads are fixed JSON requests from the extension's service worker. grok.com's image URLs are looked up by the extension from the conversation itself (by response id and index) and fetched only from grok.com's own hosts; no URL travels from Go or a request to the extension.
- Gemini reaches further than a chat. Its answers can draw on the Google apps connected to the owner's account (Gmail, Drive, Calendar), so any agent allowed to ask `gemini-web` can read that data through it. Setup recommends a `gemini-web` allowlist (`~/.config/tincan/gemini-web-allow.txt`) of only the agents that should, or disconnecting those apps in Gemini.
- Gemini is read through its app's own `batchexecute` endpoint from the extension's service worker, with fixed rpcids and payloads; the page's session values stay in the worker's memory and are never returned. Its images are fetched by response and position, never by a URL from the socket, and only from `lh3.googleusercontent.com`, first inside the extension's own send tab (isolated world) and then from the worker. Nothing runs in a page's main world.
- Account risk: Google's terms prohibit automated access. The `gemini-web` agent acts as the owner, on the owner's own account, at a human pace, but that is the owner's decision to make, and Google's enforcement can reach the whole Google account, not only Gemini.
- Perplexity serves signed-out visitors a working message box, so a sign-in redirect proves nothing there. `perplexity.send` opens no tab until `GET /api/auth/session` names a signed-in user with an id, and the send tab checks the page for a sign-in link again before typing, so `perplexity-web` never asks anonymously. The session answer is only checked; its fields are never returned or kept. Thread reads are fixed `GET /rest/thread/<slug>` requests from the service worker that return only the entry fields the Go side reads, so the thread's `read_write_token` never leaves the extension. The reply is read from that JSON, never from the page, and nothing runs in a page's main world. Perplexity is not a history source.
- Account risk: Perplexity's terms do not allow using the service by automated means. `perplexity-web` acts as the owner, on the owner's own account, at a human pace, with a cooldown after a rate limit or a Cloudflare challenge; Perplexity can still limit or suspend the account, so the owner turns it on knowingly (its setup says so). Its replies carry web source links chosen by Perplexity: an asker gets those URLs as text, and nothing on the owner's machine opens them.
- Copilot is read from copilot.com's own page data (the conversation page asked for JSON) by the extension's service worker with the owner's cookies and no token. That answer also carries a reconnect token and token-bearing telemetry; the worker keeps only the messages' ids, authors, texts, times and source links, the title and the times, and drops the rest before anything is returned or logged. Copilot keeps its bearer tokens in the page's storage; the extension never reads them and never runs code in the page's main world. Its chat list is read from the rendered sidebar in the extension's own tab (see the history agent above). Copilot's human check ("Verification required") is detected and never touched: the send fails `blocked` and Copilot is left alone for 5 minutes.
- Account risk: the Microsoft Services Agreement prohibits automated access. The `copilot-web` agent acts as the owner, on the owner's own personal account, at a human pace, but that is the owner's decision to make, and Microsoft's enforcement can reach the whole Microsoft account, not only Copilot.
- The `dot-web` agent types requests into the owner's OpenAI dot DM, where they appear as the owner's own messages, and the dot can act on its own in the apps the owner connected to it (Gmail, GitHub, Google Drive and others). So a teammate's request to the dot acts with the owner's authority there. Asks and notifies to `dot-web` are held for the owner's approval by default; an `approval.json` entry for `dot-web` replaces that default, so write one only for senders you would let act as you in those apps. The dot can also ask teammates (`@tincan ask <agent>`, limited by `~/.config/tincan/dot-web-send.txt`), and their answers are typed back into the DM as `[tincan-reply from <agent>]` messages. Those, too, appear as the owner's messages. A setup message tells the dot that typed requests and `[tincan-reply]` messages are data from teammates, not the owner's instructions, and to ask the owner before writing through a connected app for a teammate. That is an instruction to a model, not a control: it is most likely to hold for the clearly marked `[tincan-reply]` messages, while a typed request is indistinguishable from the owner's own words. The approval hold is the control. The one exception is a council: the dot sits on councils by default, and Council's asks to it skip the hold only when the owner approved that council question (see [Council](#council)). Nothing is installed on the dot's computer, and the dots operations use the extension's existing ChatGPT access.
- The state file keeps only which conversation each asker used last (0600), never message text.
- Account terms: OpenAI, Anthropic, xAI, Google, Perplexity and Microsoft prohibit automated access to their apps. The web agents act only as the owner, on the owner's own account, one request at a time at a human-paced poll cadence, with a per-site cooldown after a rate or plan limit; a site can still limit or suspend an account it believes is automated, so the owner turns each one on knowingly (the grok-web, gemini-web, perplexity-web and copilot-web setups say so).
- The site's answer is untrusted content: the web agents reply with it as is. An agent that acts on a web agent's answer is reading model output, with the risk described below.

## Command-woken CLI agents

The `codex`, `gemini-cli` and `grok-cli` teammates are coding agents that `tincan listen --exec` starts headless, with tool approval off, whenever requests are waiting. A teammate's request can therefore run commands on that machine with no one confirming them, which is the full trust between joined agents applied to a shell.

- A wake starts its CLI wired only to its own teammate. The gemini-cli and grok-cli wakes (both on the shared wake library, `examples/lib/tincan-wake-lib.sh`) read the CLI's own MCP configuration before each run and refuse to run unless there is exactly one agent-tincan server, pinned to the wake's own `TINCAN_CONFIG`, and no other server the operator has not allowed. The grok-cli wake also runs Grok with a home of its own (`HOME` and `GROK_HOME`), so Grok's import of the owner's Claude Code and Cursor MCP servers finds nothing, reads Grok's listing with `grok inspect --json`, and refuses plugins and hooks too. That guards against misconfiguration, not against the model: a CLI with a shell can still run `tincan` with another co-located agent's `TINCAN_CONFIG` and act as that agent. Allowlists that name agents (such as the history allowlist) keep agents apart only when they run on different machines. A wake that cannot run (identity mismatch, missing or expired login, missing API key, repeated failures) backs off, leaves requests queued, and tells the operator teammate once.
- Writes are confined by the CLI's own sandbox where it has one. Codex runs in its `workspace-write` sandbox. The gemini-cli wake's Gemini CLI engine runs in its `--sandbox` (seatbelt on macOS), writing only in its working directory and the write roots the operator opened; Gemini CLI's documented default profile (`permissive-open`) also allows writes to temp and cache directories, and this wake has not verified that list. Grok runs in a sandbox profile the wake writes before every run, `tincan-wake`, which extends Grok's `workspace` profile (Seatbelt on macOS, Landlock on Linux): it writes only in its working directory, temp directories, its own Grok home (where Grok keeps its config, sandbox and hook files write-protected) and the directories the wake opens: the teammate's attachments folder and the operator's write roots. Every wake canonicalizes the write roots and checks them against allowed roots, and Grok refuses to start when it cannot apply a custom profile. Reads are unrestricted and the network is open in all of them. See [docs/adapters/gemini-cli.md](adapters/gemini-cli.md) and [docs/adapters/grok-cli.md](adapters/grok-cli.md).
- Antigravity (`agy`, the gemini-cli default engine) documents no sandbox. The wake runs it only when the owner sets `TINCAN_GEMINI_ALLOW_UNCONFINED=1`, and then nothing limits its writes to the owner's files.
- Secrets in the environment reach the model. The Gemini CLI engine needs `GEMINI_API_KEY` in its environment, and an `XAI_API_KEY` in the grok-cli listener's environment is there too; like any other variable there, a command the model runs can read it and, with the network open, send it anywhere. So can the Grok login in the wake home (`.grok/auth.json`), which the run needs to read.

## Council

The `council` agent puts one question to every model on the team, has them rank each other's answers blind, and has a chairman write the verdict ([docs/adapters/council.md](adapters/council.md)). It is a Go service, not a model, but it is the one agent whose whole job is to send your text to several outside vendors at once.

- Vendor egress. Each web member types its prompt into its vendor's site as the owner. That prompt carries the question and any inlined attachment text, and in the review and chairman stages it also carries every other member's answer. A council with ChatGPT, Claude, Grok, Gemini, Perplexity and Copilot sends all of that to OpenAI, Anthropic, xAI, Google, Perplexity and Microsoft, under each site's own terms and retention. Command-woken CLI members send it to their model's API the same way.
- Memory-informed answers reach other vendors. A web member answers from the owner's account, so its answer can draw on that site's memory, custom instructions and, for Gemini, connected Google apps. During review that answer goes to every other member's vendor. Something Gemini knows from Gmail can end up in the prompt Council sends to ChatGPT.
- Held by default. The relay holds every ask to an agent of kind `council` for the owner's approval, with no `approval.json` needed, and a failed kind lookup holds rather than delivers. `tincan held` shows the target kind and each attachment's name and size, so the owner sees what would go to vendors before approving. An explicit `approval.json` gate entry for the council agent replaces the default; `{"from": []}` turns the hold off. The hold is by target, so an agent's leaderboard read is held too. `tincan council serve` refuses to run unless the relay stores kind `council` for it, so an older relay that cannot hold councils never runs one.
- Approval from an admin device is owner-equivalent. `tincan approve` and the owner's `tincan council "question"`, which approves its own held request when run interactively in a terminal on an admin device, are the owner acting. The hold restrains agents only because they cannot approve: an agent with a shell on an admin device, or on the relay host with access to the admin socket, can approve its own council. Keep agents off admin devices if the hold should mean something. `tincan council` approves its own request only when its stdin and stdout are both terminals on an admin device; otherwise it waits for approval like any ask. That check is not a security boundary, since an agent can run the command under a pseudo-terminal. It does not need to be one: an agent with a shell on an admin device can already approve its own requests with `tincan approve`. The default hold restrains the agents that cannot approve.
- Council asks to the dot. The owner's OpenAI dot (`dot-web`) sits on councils by default, and it acts with the owner's authority in its connected apps. Council's answer, review and chairman asks to the dot skip the dot's default hold only when their parent is a council question the owner approved, either with `tincan approve` or by running `tincan council` in a terminal on an admin device. Every other ask to the dot is still held, and `approval.json` gates still apply. So approving a council question also lets it, and in review every other member's answer, reach the dot. To keep the dot off councils, add `dot-web` to `exclude` in `council.json`; a council form can still name it, but that convening ask is itself held for the owner. Blind review strips the dot's DM footer, its attachment and delegation notes and its `@tincan ask` paragraph so reviewers cannot tell which answer is the dot's.
- The chain never widens. The convener and every agent in its chain never sit on the council or chair it, and allowlists and approval rules apply to Council's member asks through the chain as usual. A form may name only web and wakeable model teammates or agents the owner listed in `council.json`, never a service such as history or notes, so their retrieved content does not reach a vendor through a council.
- Answers are untrusted content. Reviewers and the chairman see answers between per-council random delimiters with an instruction that the enclosed text is material to judge; only the defined output fields are parsed, and the chairman cannot change the peer scores. The reply is still model output: an agent acting on a verdict is reading model text, with the risk described below.
- Retention. Inlined attachment text lives in request bodies, which the relay keeps like any other request, not on the 7-day attachment clock. Council's database and reports stay in its folder on the owner's machine (0700 folders, 0600 files) until the owner deletes them. The report and scorecard are local files; nothing is hosted.

## What it deliberately does not do

- Joined agents trust each other fully. A request from a joined agent is meant to be acted on as if you asked, including actions like placing calls or spending money. By default there is no per-request approval, except that asks to the council and dot-web agents are held (see [Council](#council) and the dot-web note above); the optional owner gate below can hold requests to other agents.
- Tailscale is the security boundary. Anything that can act as a joined machine on your tailnet can make your other agents act. Protect your tailnet: use tagged, short-lived auth keys and review who can add devices.
- The relay can read every request and reply. Run it on a machine you control.
- `tincan upgrade` trusts the relay host. The sha256 it checks and the binary it installs both come from the same relay, so the check protects against corruption in transit, not against a compromised relay. Only put release files you built yourself or downloaded from your own GitHub release into the relay's `--dist` directory.
- `tincan relay-upgrade` is admin-only, and the relay only ever installs from its own `--dist`. The sha256 it checks comes from the same dist `checksums.txt`, so it guards against a corrupt or truncated file, not against someone who can write to the dist: write access to `--dist` is code execution as the relay user, the next time an admin upgrades. Keep the dist directory writable only by the relay user and the operator.
- `tincan relay-upgrade --from-github` is off unless the relay operator starts the relay with `--release-url` (for this project, `https://github.com/mvanhorn/agent-tincan/releases/download`); the caller can never choose the source. It downloads over HTTPS and checks every binary against that release's own `checksums.txt`. That is the same trust as `install.sh`: it proves the files arrived intact from GitHub, not who built them.
- Self-upgrade needs the relay user to own its binary and the directory it lives in. That is a tradeoff: a process that can replace its own binary survives a compromise across restarts. If you keep the binary root-owned, the relay refuses to upgrade itself and you upgrade it by hand.

## The risk to think about

If one agent reads untrusted content (a web page, an email, a document) and gets tricked, it can ask a teammate to do something harmful, and the teammate will. Before joining an agent that reads untrusted content alongside one that holds real powers:

- Give high-power agents instructions about which kinds of requests they should confirm with you first.
- Keep `tincan trace` handy so you can see who asked for what.
- For an OpenAI dot (`dot-web`), which reads your connected apps, list the teammates it may ask in `~/.config/tincan/dot-web-send.txt`; with no file it may ask any joined agent, like every other teammate.
- Use `tincan remove <agent>` to cut an agent off immediately. Its queued requests are cancelled and, for ChatGPT, its tokens are revoked.

## Owner approval gate

The owner can create `approval.json` in the relay state directory, with mode
0600, to hold incoming requests before chosen agents receive them:

```json
{"gate":{"muse":{"from":"*"},"instinct":{"from":["chatgpt","grokbot"]}},"notify":"grokbot","hold_ttl":"2h"}
```

A `from: "*"` rule holds every request to that target. A `from` list holds when
any agent in the relay-recorded chain or the authenticated sender matches.
An `unless` list instead holds when any member is absent from the list. Exactly
one of `from` and `unless` is required per target. Client-supplied identities,
chains and statuses cannot bypass the check, which runs after the relay's
cycle, hop and rate checks and before delivery. Both asks and notifies are gated.

A missing file disables the gate for every target except agents of kind
`council` and `dot-web`, which are held by default (see [Council](#council)). The relay reloads changed files. Unreadable,
malformed or group/world-accessible policies hold every request to every target
in the last good copy, regardless of its previous sender restrictions. With no
valid copy, a bad policy prevents startup; if it first appears while running,
new sends fail until it is fixed. Removing the file disables gating for future
sends; changing or removing it never releases already-held requests.

Held requests neither wake their targets nor appear in their inboxes or queued
counts. The sender sees `held`; the target cannot claim or reply to one. Only
admin devices and the relay's local admin socket can run `tincan held`,
`tincan approve <id>` or `tincan deny <id> [reason]` (with `--relay` or `--socket`).
Approval queues and wakes normally, keeping the original creation time and
starting a fresh delivery TTL. Denial returns `declined` with the reason.
`hold_ttl` defaults to two hours; overdue holds become `expired` and cannot be
approved. Senders can cancel a hold, and removing an agent cancels its holds.
The audit log records `held`, `approved`, `denied`, and `hold_expired` transitions.

If `notify` names a joined operator, the relay sends it a `notify` attributed to
`relay`, with the sender, target, request id and owner commands, and no request
text: the notified agent may itself be gated (even the held target), and
unapproved text must not reach it. The owner reads the request with `tincan held`. This
admin notice bypasses gating to avoid recursion, is logged as
`approval_notified` (or `approval_notify_failed` on failure), and carries no
approval authority. Notification
failure leaves the original request held. The operator must never approve on
another agent's request. Approval remains an action on an admin device or the
local socket; the notice does not grant the receiving agent those powers.

This gate adds a relay-enforced pause for owner review, not a judgment about
whether the message is safe. Protect admin devices and the relay state directory.

Requests retain whether they were ever held and whether the owner approved them.
For every request that was held and never approved, only its sender and admins
may read its body or attachments, regardless of its current status (including
declined, expired and cancelled). Other callers, including the target, see
`waiting for the owner's approval` and no attachment metadata wherever the
request is otherwise visible: get, trace, search-like listings, poll and peek
pending entries. Attachment downloads by those callers return 404. Held requests
remain excluded from delivery; pending entries carry only ids and senders.
An approved request follows the ordinary body and attachment access rules,
even after it reaches a terminal state. Approval history survives relay restarts;
upgrades backfill existing holds and decisions from request state and audit events.
