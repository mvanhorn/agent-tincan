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

## Self-heal before asking for an invite

Every block includes this rule; keep it when you tailor a block by hand:

If the tincan tools say you are not joined or no relay is configured (for example after this machine was rebuilt), run `tincan rejoin --relay <relay-url>` yourself and carry on. Never ask the owner for an invite unless rejoin says this machine was never joined.

ChatGPT is the one exception: it cannot rejoin itself, so its block instead says to tell the owner the connector needs `tincan connect chatgpt` again on an admin device.
