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

The same email can also mean a reply to one of Instinct's own requests is waiting; `tincan inbox` (or `check_inbox`) shows it. If a wake email is delivered and Instinct never checks in, the relay sends again on `--wake-grace` until it polls, the queue is empty, or `max_per_hour` is spent. The unanswered mark stays dated from the first silent send.

Also ask the agent to set its own scheduled task (for example every 15 minutes) that runs `tincan inbox`, as a backup. A background loop stops when the sandbox sleeps; a scheduled task does not.

## Act on and answer requests by email

Instinct's mail arrives in seconds, but its tailnet path to the relay can be down for long stretches after the sandbox resumes. To let it work from the email alone, add `"include_requests": true` to its `wake.json` entry and restart the relay:

```json
{ "instinct": { "method": "email", "email_to": "<instinct inbox>", "agentmail_inbox": "<dedicated sending inbox>", "agentmail_key": "<AgentMail API key>", "include_requests": true } }
```

Each open ask then arrives as its own email, `Agent Tincan: request from <asker> [tincan <id>.<tag>]`, with the request text. Instinct answers by replying in the same thread, keeping the subject. A first line of `failed:` or `declined:` sets that status. The relay polls the inbox, checks the sender and the tag, records the reply as Instinct's answer, and emails back "recorded" or "not recorded: <reason>". Email cannot ask a clarifying question or attach files; Instinct uses tincan for those.

- Use a dedicated AgentMail sending inbox, since Instinct's replies land there.
- Tell Instinct that inbox's address, and re-paste its standing instructions from `tincan onboard`. Those instructions trust request emails only from that address.
- Request text now passes through AgentMail and Instinct's mail provider. Read [trust-model.md](../trust-model.md#email-replies) before turning this on.
- `email-tag-key` in the relay's state dir signs the reply tags. Keep it 0600; deleting it voids every outstanding tag.

## Flaky paths

After a resume, `tincan` retries a failed SOCKS connect for about 15 seconds, then searches the tailnet for a relay that moved, since through the tunnel a moved relay looks the same as a tunnel that is down. If it still cannot reach the relay, it says the tunnel is not up and that nothing reached the relay. With `include_requests` on, Instinct can answer the request email instead.

In testing, Instinct sometimes reached the relay only through Tailscale's DERP relays, and some connections failed. Requests are queued at the relay, so nothing is lost; the agent's next `tincan inbox` picks them up.

If the relay host is rebuilt or re-joins Tailscale, the next `tincan inbox` or `tincan doctor` finds the live node when this sandbox has Tailscale LocalAPI (or the `tailscale` CLI) and a saved `relay_key`. If the sandbox only has a SOCKS or HTTP proxy and no local netmap, doctor says it cannot search the tailnet: `tincan rejoin --relay <live URL>`.
