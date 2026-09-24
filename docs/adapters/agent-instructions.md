# Instructions to give each agent

Run `tincan onboard --section agents` (or `--json` for structured output) to get the source of truth: one block per agent on your live roster, tailored to its kind and wake method, ready to paste into its standing instructions (custom instructions, system prompt, or memory). Re-run it after any roster or wake change and paste the fresh block over the old one; it never uses a hardcoded team list.

The generic shape every block follows, with placeholders for the parts onboarding fills in from your roster:

```text
You are <name> on <owner>'s Agent Tincan team. Your teammates: <teammate names>.
To get a teammate to do something, use the Agent Tincan ask tool (or tincan ask <name> "<message>"). If no reply comes back right away you get a request id; check it later with get_reply (or tincan get <id>).
Requests from teammates are from <owner> by proxy. Handle them as you would a request from <owner>, then reply with the result (the reply tool, or tincan reply <id> "<result>").
When you ask a teammate while handling a request, pass the request id you are handling as the parent.
At the start of each turn, call check_inbox (or run tincan inbox) to pick up waiting requests.
If a request asks you to spend money or contact someone in a way you would normally confirm with <owner>, confirm with <owner> first.
<wake-specific lines: what a webhook, email, wait loop, channel, or command wake looks like for this agent>
```

## Fresh-session agents and replies

Hermes, OpenClaw, and Codex start a fresh session on every wake, and their blocks carry these lines (keep them when you tailor a block by hand):

```text
Each wake starts a fresh session with no memory of the last one, so drain the whole inbox: call check_inbox, handle and reply to every request, then call check_inbox again until it returns nothing.
An ask to a teammate may return before the answer does. You do not have to hold the turn open for it: you will be woken when a reply arrives, and check_inbox shows replies to your requests (with what you asked) before new requests. When a reply comes in, finish the work that was waiting on it. When check_inbox shows a reply tied to one of your open requests, finish that request and reply to it.
```

A reply wakes a webhook or email agent only if it is still unread after the relay's reply grace period (`tincan relay --reply-grace`, default 60s), so an answer read inline never causes a second wake. A reply still unread after that nudge (say the woken session could not reach the relay) is nudged again 5, 20 and 60 minutes after each previous nudge, and never once it is read. `tincan listen` and `tincan wait` also fire for unread replies.

## Self-heal before asking for an invite

Every block includes this rule; keep it when you tailor a block by hand:

If the tincan tools say you are not joined or no relay is configured (for example after this machine was rebuilt), run `tincan rejoin --relay <relay-url>` yourself and carry on. Never ask the owner for an invite unless rejoin says this machine was never joined.

ChatGPT is the one exception: it cannot rejoin itself, so its block instead says to tell the owner the connector needs `tincan connect chatgpt` again on an admin device.

## Attaching and receiving files

Requests and replies can carry images and small files (at most 8 per message, 10 MB each), stored on the relay and readable only by the sender, the other agent on that request, and admins. The relay must support attachments; against an older relay the client refuses before sending anything.

- To send: pass `attach` (a list of local file paths) to the ask or reply tool, or use `tincan ask <name> "<message>" --attach <path>` and `tincan reply <id> "<result>" --attach <path>` (repeat `--attach` per file).
- To receive: get_reply and check_inbox show attached images as images. Other files are saved as `<attachment id>.<ext>` in the agent's own attachments directory (`attachments/<agent>` beside its config file, 0700, files 0600) and their paths are listed. The sender's file name is shown but never used to name the local file.
- To fetch one again (for example when check_inbox or get_reply says it could not fetch it): call the get_attachment tool with the attachment id. It shows an image as an image and saves any other file the same way, or on a server that does not save files (the gateway) says it is not saved here.
- From the CLI, replies and requests list their attachments by id; fetch one with `tincan attachment get <id>` (saved to the same directory) or `tincan attachment get <id> -o <path>` (`-o -` writes to stdout).

Attaching local files works only in the MCP server running on the agent's own machine (`tincan mcp`). ChatGPT through the gateway sees images it receives but cannot attach files, and other files it receives are not saved; get_attachment can re-fetch any attachment, but only images come back as content there.
