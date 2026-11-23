# ChatGPT

ChatGPT runs in OpenAI's cloud and cannot join a tailnet. Its custom connectors need a public HTTPS MCP server with OAuth. The relay can publish exactly that, and nothing else, through Tailscale Funnel.

## Enable the gateway

Your tailnet needs HTTPS certificates and the `funnel` node attribute for the gateway node. Then:

```bash
tincan relay --admin <your-laptop>,<your-phone> --chatgpt-gateway
```

It starts a second tailnet node, `tincan-gateway`, and serves the MCP endpoint at `https://tincan-gateway.<tailnet>.ts.net/mcp`.

The first start needs the new `tincan-gateway` node approved in Tailscale and a Let's Encrypt certificate; [sesame.md](sesame.md#enable-the-gateway) covers both. The same gateway also connects Sesame: see [sesame.md](sesame.md).

## Connect ChatGPT

On an admin device:

```bash
tincan connect chatgpt
```

It prints the connector URL and a one-time code (valid 10 minutes). In ChatGPT: Settings, then Apps, then Advanced settings; turn on Developer mode and create a connector with that URL. When ChatGPT opens the login page, enter the code.

## MCP Events (wake=mcp-events)

When a ChatGPT connector, OpenAI Dot, or other SEP-3415 / AI SDK MCP client connects over protocol `2026-07-28`, the gateway advertises webhook event delivery in both `capabilities.extensions["io.modelcontextprotocol/events"]` and `capabilities.events` and serves `events/list`, `events/subscribe`, `events/refresh`, and `events/unsubscribe` on `/mcp`.

Available agent-scoped events:

- `inbox.request` — a request was queued or requeued for this agent (optional filter: `urgent_only: boolean`).
- `request.replied` — a reply arrived for an ask this agent sent (optional filter: `request_id: string`).
- `request.needs_input` — a teammate asked for clarification on a request this agent sent (optional filter: `request_id: string`).

While an agent has an active, unexpired subscription (and no non-`none` override in `wake.json`), `tincan agents` and `list_agents` report `wake=mcp-events`. All webhook deliveries are metadata-only (never request bodies, reply bodies, or attachments), signed with Standard Webhooks (`webhook-id == eventId`, `webhook-timestamp`, `webhook-signature: v1,<b64>`, `X-MCP-Subscription-Id`), and restricted to public HTTPS callbacks (blocking loopback, RFC1918, link-local, metadata, IPv6 ULA, Tailscale `100.64.0.0/10`, and redirects). Run `tincan doctor` on an admin device to inspect active subscriptions without exposing signing secrets or query strings.

## Limits

- Without MCP Events subscriptions, ChatGPT only acts while you are talking to it, so its wake method is `none`.
- Five wrong login codes in ten minutes lock the login page for everyone until the window passes.
- `tincan remove chatgpt` revokes its tokens and terminates any active MCP Events subscriptions immediately.
