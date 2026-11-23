# OpenBot

[OpenBot](https://openbot.run) is a desktop app that runs a team of AI teammates on your own computer, driving the CLIs you already have (Codex, Claude Code, Gemini and others). Agent Tincan plugs into it in two places:

- Tools: the relay's MCP gateway is added in OpenBot as a custom app, signed in with OAuth, so every OpenBot agent gets `ask`, `check_inbox`, `reply` and the rest.
- Wake: an OpenBot routine with a webhook trigger. The relay POSTs a signed, metadata-only Standard Webhooks event to the routine's URL, and the routine starts a turn that drains the Agent Tincan inbox.

This guide follows OpenBot's docs and source (v0.77.0, November 2026). Live-tested version: `<to fill after live run>`. If a step does not match what you see, open an issue at https://github.com/mvanhorn/agent-tincan/issues with what you ran and what OpenBot showed.

Licensing: OpenBot is source-available under PolyForm Noncommercial. Personal use is free. Using it for work needs OpenBot's commercial license from 2026-12-01 (see https://openbot.run/commercial-license).

## 1. Tools (MCP over OAuth)

OpenBot reaches Agent Tincan through the MCP gateway, the same public HTTPS endpoint ChatGPT and Sesame use. Enable it on the relay first; [chatgpt.md](chatgpt.md#enable-the-gateway) and [sesame.md](sesame.md#enable-the-gateway) cover the flag, approving the `tincan-gateway` node and the first certificate.

On an admin device:

```bash
tincan connect openbot
```

It prints the gateway URL (`https://tincan-gateway.<tailnet>.ts.net/mcp`) and a one-time code, valid 10 minutes. In OpenBot, open Marketplace, then Apps, then Add custom:

1. Name: `Agent Tincan`.
2. Transport: HTTP. URL: the printed `/mcp` URL.
3. Save, then Sign in. OpenBot runs the MCP OAuth flow in your browser; on the "Connect to Agent Tincan" page, enter the code.
4. Test connection. It should list the Agent Tincan tools.

The URL must be `https://`, or `http://` on loopback (`127.0.0.1`, `localhost`) when OpenBot runs on the relay host itself. OpenBot refuses OAuth to any other plain-HTTP address, so never use a `http://100.x.y.z` tailnet IP. The app applies to all of your OpenBot agents.

Ask an OpenBot agent to call `list_agents`. It should name your teammates, and `tincan agents` now lists `openbot`.

## 2. Webhook routine

In OpenBot, open the agent that should answer Agent Tincan requests and add a routine:

1. When to run: Webhook.
2. Choose "Use the sender's secret" and paste a Standard Webhooks secret you generate for it (keep a copy for step 3):

   ```bash
   printf 'whsec_%s\n' "$(openssl rand -base64 32)"
   ```

3. Optionally filter on event type `tincan.request`.
4. Instruction:

   > An Agent Tincan wake. Call check_inbox from Agent Tincan. For each reply to one of your own requests, finish the work that was waiting on it. For each request, do it as you would a request from me, then call reply with the result; if you need a detail only the asker has, reply with needs_input and your question. Call check_inbox again until it is empty, then stop. The webhook data only holds counts and request ids; read the requests themselves with check_inbox.

5. Save, and copy the routine's webhook URL (`https://signal.openbot.run/v1/webhooks/<uuid>`).

The OpenBot host must be signed in to an OpenBot account (a free email-code account) for the webhook URL to work.

## 3. wake.json

On the relay host, add the routine to `wake.json` in the state dir (chmod 600) and restart the relay:

```json
{ "openbot": { "method": "webhook", "format": "standard-webhooks", "url": "https://signal.openbot.run/v1/webhooks/<uuid>", "signing_secret": "whsec_<same secret as the routine>" } }
```

`format: "standard-webhooks"` signs each wake with `signing_secret`, the way OpenBot checks it: `webhook-id`, `webhook-timestamp` and `webhook-signature: v1,<base64 HMAC-SHA256>` over `<id>.<timestamp>.<body>`. The relay refuses to start if `signing_secret` is missing, does not start with `whsec_` or is not valid base64, or if the entry also sets `bearer_token` or `hmac_secret`.

The body is metadata only, because it passes through OpenBot's cloud relay:

```json
{"type":"tincan.request","source":"agent-tincan","waiting":2,"urgent":0,"unseen_replies":1,"request_ids":["..."],"reply_request_ids":["..."]}
```

It never holds request or reply text, senders or kinds. `webhook-id` is the nudge's idempotency key: the relay's one retry after a failed send reuses it, so OpenBot (which dedupes on it for 7 days) runs one turn, and each later wake gets a new id so it is not deduped away.

Test it with a fresh ask from another teammate, for example `tincan ask openbot "say hi"`, then check `tincan agents` and, on an admin device, `tincan wakes openbot --since 1h`, which shows the HTTP status and what the endpoint answered.

## Limits

- OpenBot's relay does not queue. When the OpenBot app is closed, asleep or signed out, the wake gets a 503 and is dropped there. Nothing is lost on the Agent Tincan side: the request waits in the relay's queue, and while it is still queued and openbot has not checked in, the relay sends the wake again every `--wake-grace` (default 10 minutes, or `--urgent-wake-grace`, 2 minutes, for an urgent ask), inside `max_per_hour` (default 12). The first wake after OpenBot comes back starts the turn.
- Each wake is a new routine run. The instruction must tell the agent to drain the whole inbox, not just react to the event.
- OpenBot puts the event data after the routine instruction as external data. It cannot grant tools or skip OpenBot's approvals, and the data holds nothing to act on besides counts and ids.
- Everything sent to `openbot`, and every answer teammates send back to it, reaches the OpenBot agent and the model CLI it drives. Gate it in `approval.json` if you want to approve each request first, for example `{"gate":{"openbot":{"from":"*"}}}`.
- OpenBot ships a release almost every day and is pre-1.0, so menus and names can move. The signing and body rules above follow its `docs/webhooks.md`.
