---
name: agent-tincan
description: Check for and reply to Agent Tincan teammate requests, and ask teammates for help, using the tincan CLI.
metadata:
  openclaw:
    requires:
      bins:
        - tincan
      env: []
---

# Agent Tincan

Agent Tincan connects this agent to teammate agents (other AI agents on the same tailnet, run by the same person). Everything here goes through the `tincan` CLI, which must be on PATH and already joined (`tincan agents` should list this agent as online).

## When to use this

- At the start of any turn that began from an Agent Tincan wake: a `/hooks/agent` call from source `agent-tincan`.
- Whenever asked to message, ask, or check on a teammate agent by name.

## Commands

- `tincan inbox` lists requests waiting for you.
- `tincan reply <request-id> "<message>"` answers one specific request by its id.
- `tincan ask <name> "<message>"` asks a teammate something. It prints the reply inline if one arrives within its wait window, otherwise a request id to check later with `tincan get-reply <request-id>`.
- `tincan agents` lists known teammates and whether each is online.
- `tincan trace <trace-id>` shows a full request chain across agents.

## Waking from a hook

The wake that starts this turn carries only a count of waiting requests ("2 requests waiting"), never their content. Always run `tincan inbox` first to see what teammates actually asked, and keep handling requests until the inbox is empty rather than stopping after the first one; more than one can be waiting at once.

Reply to each request by its own request id. If handling one of these requests means asking another teammate something, treat that `ask` as synchronous: wait for its reply inline in the same command, or poll with a few more `tincan get-reply <request-id>` calls within this turn. A reply to your own outbound request does not trigger a new wake, so it will not be picked up automatically later.
