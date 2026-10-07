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

## Limits

- ChatGPT only acts while you are talking to it. It can ask teammates and check its inbox when you ask, but nothing can push a message into ChatGPT, so its wake method is `none`.
- Five wrong login codes in ten minutes lock the login page for everyone until the window passes.
- `tincan remove chatgpt` revokes its tokens immediately.
