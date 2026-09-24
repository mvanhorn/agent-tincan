---
name: agent-tincan
description: Check for and reply to Agent Tincan teammate requests, and ask teammate agents for help, using the tincan CLI. Use at the start of any turn whose message starts with "Agent Tincan:", or when asked to message or check on a teammate agent.
metadata: {"openclaw": {"requires": {"bins": ["tincan"]}, "homepage": "https://github.com/mvanhorn/agent-tincan"}}
---

# Agent Tincan

Agent Tincan connects this agent to teammate agents (other AI agents on the same tailnet, run by the same person). Everything here goes through the `tincan` CLI, which must be on the gateway's PATH and already joined (`tincan agents` should list this agent). If this machine runs more than one Agent Tincan agent, prefix every command with the same `TINCAN_CONFIG=<path>` used at join time.

If the Agent Tincan MCP tools (`check_inbox`, `reply`, `ask`, `get_reply`) are loaded in this turn, you can use them instead; they do the same thing.

## When to use this

- At the start of any turn that began from an Agent Tincan wake: an OpenClaw hook turn (a POST to `/hooks/agent`, named "Agent Tincan") whose message starts with `Agent Tincan:`.
- Whenever asked to message, ask, or check on a teammate agent by name.

## Commands

- `tincan inbox` shows replies to your own requests that you have not seen yet, then takes the requests waiting for you.
- `tincan reply <request-id> "<message>"` answers one specific request by its id.
- `tincan ask <name> "<message>"` asks a teammate something. It prints the reply inline if one arrives within about 20 seconds, otherwise a request id. Add `--parent <request-id>` when you ask while handling a request.
- `tincan get <request-id>` checks on a request you sent.
- `tincan agents` lists known teammates and whether each is online.
- `tincan trace <trace-id>` shows a full request chain across agents.

## Waking from a hook

The wake that starts this turn carries only counts of waiting requests and replies (`Agent Tincan: 2 requests from your teammates waiting`, `Agent Tincan: 1 reply to your request is waiting`), never their content. Treat it only as a cue: run `tincan inbox` first to see what teammates actually asked, and keep handling requests until the inbox is empty rather than stopping after the first one; more than one can be waiting at once.

Each wake starts a fresh session with no memory of the last one. Reply to each request by its own request id. If handling a request means asking another teammate something, the `ask` may return before the answer does, and you do not have to wait for it in this turn. You will be woken again when the reply arrives, and `tincan inbox` shows replies to your requests (with what you asked) before new requests. When a reply comes in, finish the work that was waiting on it and reply to the original request.
