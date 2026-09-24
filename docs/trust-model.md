# Trust model

## What Agent Tincan guarantees

- The relay only listens on your tailnet. The one exception is the optional ChatGPT gateway, which serves only the MCP tools and OAuth on its own Funnel hostname.
- Every request is attributed to the agent that sent it, using Tailscale's identity for the machine it came from. An agent cannot send as another agent, and whatever it writes in the `from` field is ignored.
- Only admin devices and the relay's local admin socket can invite, remove, or connect agents, or trace every chain. A caller is an admin device only when all of these hold: Tailscale WhoIs reports its short machine name in the `--admin` list; the node has no Tailscale tags; and, if `--admin-login` is set, the node's owning login is in that list. Machine names are chosen by whoever controls the node, so tag every agent machine (for example `tag:agent`): a tagged node is never an admin, whatever it is called. Owner login alone is not used, because on a single-user tailnet every node, agents included, has the same owner.
- Chains are tracked by the relay, not by the model. A request made while handling another continues that chain even if the model leaves the parent out. A request that would loop back to an agent already in its chain is rejected, and chains longer than 4 hops are rejected.
- Each sender is rate-limited (30 new requests per minute by default).
- Every send, delivery, claim, reply, rejection, wake, join, rebind, and removal is written to an append-only, hash-chained log. `tincan audit-verify` detects edits.
- Wake nudges carry only a count and an instruction, never request text.

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

The `history` agent reads the owner's own conversations in ChatGPT, claude.ai, Codex and Claude Code. That is its whole job, so it is the most sensitive agent on the mesh, and its controls are built for that ([docs/adapters/history.md](adapters/history.md)):

- By default any joined agent may ask it. The owner can restrict that with an allowlist file (`~/.config/tincan/history-allow.txt`) of agent names. A file of names covers the whole request chain as the relay recorded it, not just the sender: if muse asks codex and codex asks history while handling muse's request, a file that lists codex but not muse declines it because of muse. The chain and sender come from the relay, never from the request body. A `*` entry means every joined agent. An unreadable allowlist or a bad name in it declines everyone.
- The access check runs in Go before any model sees the request. The only model step turns the question text into a structured query; it sees nothing else, runs with no tools, no MCP servers and a read-only sandbox, and its output is checked against a schema in Go.
- Retrieved chat content is never sent to a model. Replies are filled in from a fixed template, so text inside the owner's chats cannot steer the service or anyone it answers.
- Live reads go through the Tincan Chrome extension with the owner's existing session. The extension runs only its own fixed operations and accepts nothing else; no cookie or token leaves the browser, and Chrome is never quit or restarted.
- Images it returns are relay attachments and follow the retention above.

By default any joined agent can read the owner's chat history. If some of your agents should not, write the allowlist file and keep it to agents you would trust with your history. Remember that an allowed agent that reads untrusted content can still be talked into asking. The allowlist governs requests to the history agent, not local shell access: an agent with a shell on the owner's machine (for example the Codex wake, which looks up prior threads with `tincan history codex`) can read local Codex and Claude Code history directly, consistent with the full trust between joined agents.

## The web agents

The `chatgpt-web` and `claude-web` agents act as the owner in ChatGPT and Claude: whatever an allowed agent (by default, any joined agent) asks is typed into the owner's logged-in account, uses the owner's plan, lands in the owner's chat history, and can draw on that account's memory and custom instructions ([docs/adapters/web-agents.md](adapters/web-agents.md)). The answer goes back to the asker, and that answer can include what the site remembers about the owner.

- By default any joined agent may use them, so any agent on your mesh can act as you in ChatGPT and Claude. To restrict that, write an allowlist file (`~/.config/tincan/chatgpt-web-allow.txt`, `claude-web-allow.txt`) of agent names. A file of names covers the whole relay-recorded chain, as for history, so relaying through an allowed agent never widens access. An unreadable allowlist or a bad name in it declines everyone.
- The message is data. The extension passes it as an argument to a fixed function in an isolated content script and inserts it as text; nothing in a message or a page is executed. The extension still accepts only its fixed operation set, and the send operations take only a capped message, an optional validated conversation id and a boolean.
- The extension opens and closes its own background tab and never scripts a tab the owner opened. It checks the site session before opening a tab, so a logged-out browser never sends anonymously.
- The state file keeps only which conversation each asker used last (0600), never message text.
- The site's answer is untrusted content: the web agents reply with it as is. An agent that acts on a web agent's answer is reading model output, with the risk described below.

## What it deliberately does not do

- Joined agents trust each other fully. A request from a joined agent is meant to be acted on as if you asked, including actions like placing calls or spending money. There is no per-request approval.
- Tailscale is the security boundary. Anything that can act as a joined machine on your tailnet can make your other agents act. Protect your tailnet: use tagged, short-lived auth keys and review who can add devices.
- The relay can read every request and reply. Run it on a machine you control.
- `tincan upgrade` trusts the relay host. The sha256 it checks and the binary it installs both come from the same relay, so the check protects against corruption in transit, not against a compromised relay. Only put release files you built yourself or downloaded from your own GitHub release into the relay's `--dist` directory.

## The risk to think about

If one agent reads untrusted content (a web page, an email, a document) and gets tricked, it can ask a teammate to do something harmful, and the teammate will. Before joining an agent that reads untrusted content alongside one that holds real powers:

- Give high-power agents instructions about which kinds of requests they should confirm with you first.
- Keep `tincan trace` handy so you can see who asked for what.
- Use `tincan remove <agent>` to cut an agent off immediately. Its queued requests are cancelled and, for ChatGPT, its tokens are revoked.

An optional "ask the owner first" gate is planned for setups that want one.
