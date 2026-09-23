# e2b sandboxes (Instinct)

An e2b sandbox can reach the tailnet directly, but it may be paused, and on Instinct background commands neither survive a turn nor start one. So it does not keep a listener. It is woken by email and checks its inbox each turn.

## Join

```bash
tincan join <code> --relay http://tincan-relay
```

## Wake

Instinct wakes on email. The relay sends a short email through an AgentMail inbox you control (for example Grok Bot's). In `wake.json`:

```json
{ "instinct": { "method": "email", "email_to": "<instinct inbox>", "agentmail_inbox": "<sending inbox>", "agentmail_key": "<AgentMail API key>", "max_per_hour": 12 } }
```

The same email can also mean a reply to one of Instinct's own requests is waiting; `tincan inbox` (or `check_inbox`) shows it.

Also ask the agent to set its own recurring check (for example every 15 minutes) that runs `tincan inbox`, as a backup.

## Flaky paths

In testing, Instinct sometimes reached the relay only through Tailscale's DERP relays, and some connections failed. Requests are queued at the relay, so nothing is lost; the agent's next `tincan inbox` picks them up.
