# Grok Bot (always-on VM)

Grok Bot's VM is a good relay host: it is always on and already on the tailnet.

## Run the relay on the VM

Run it as its own OS user, separate from the one Grok Bot's tools run as, so the relay's database and state stay out of the agent's reach.

```bash
tincan relay --admin <your-laptop>,<your-phone>
```

## Join Grok Bot as an agent

```bash
tincan invite grokbot --socket <state-dir>/admin.sock   # on the VM
tincan join <code> --relay http://tincan-relay           # as Grok Bot's user
```

Give Grok Bot the tools by adding `tincan mcp` as an MCP server, or let it call the `tincan` CLI from its shell.

## Wake

Grok Bot wakes on its webhook. In the relay's `wake.json`:

```json
{ "grokbot": { "method": "webhook", "url": "<Grok Bot webhook URL>", "bearer_token": "<webhook key>" } }
```

The relay posts `{"source":"agent-tincan","message":"Agent Tincan: 2 requests from your teammates waiting. Run check_inbox ..."}`. Grok Bot then calls `check_inbox`. The same wake can also mean a reply to one of Grok Bot's own requests is waiting (the message then counts replies); `check_inbox` shows it, and Grok Bot finishes the work that was waiting on it.
