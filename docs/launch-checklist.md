# Launch checklist (September 25, 2026)

Status: ready. Release v0.5.0 is on the relay and every agent. agenttincan.com is live with the new hero demo. The Chrome Web Store listing is in review.

## What is live

- Relay on the Grok Bot VM (grok-bot-1, http://100.96.137.127:8787), serving release binaries for `tincan upgrade`.
- Agents: grokbot, claude-code, codex, history, chatgpt-web, claude-web (this Mac), hermes (Mac mini), muse, instinct. All upgraded through the relay.
- https://agenttincan.com and https://agenttincan.com/privacy. The repo's About link points at agenttincan.com.
- Chrome Web Store: "Agent Tincan History", publisher MVH, item id `goldflchpojcjmifnljlfkgoahjgeajn`, submitted for review with manual publish. After it passes review it waits up to 30 days for Publish.

## New since the last checklist (v0.5.0-rc7 to v0.5.0)

- `tincan mcp` accepts Content-Length framed stdio as well as newline JSON (Grok Bot's host) (#32).
- `tincan doctor` finds why an agent's tincan tools are missing and prints the fix; every `tincan mcp` records its launches for it (#33).
- Relay moves heal themselves: the relay proves its identity with a key agents learn at `whoami`, advertises its name and IP, and clients follow it when the saved address stops answering, including through a proxy (#34, #35).
- Every agent's instructions tell it to run `tincan doctor` and apply its fixes itself; sandboxes that approve each site are told to always-allow the relay (#35, #37).
- The site's hero plays four real use cases as a chat (#36).

## Launch steps

1. Make the repo public (GitHub settings). The history scan found no secrets (gitleaks: only a test value and the extension's public manifest key).
2. Right after: fresh install from the one-liner, `curl -fsSL https://agenttincan.com/install.sh | sh`, which downloads from GitHub releases and so only works once the repo is public.
3. When the store listing passes review: click Publish, install it from the store, remove the unpacked extension, then run `tincan history install --extension-id goldflchpojcjmifnljlfkgoahjgeajn`. Only in that order: the native host allows one extension id.

## Known limits (disclose, not blockers)

- OpenClaw has never run live.
- `tincan agents` does not show each agent's version yet.
- Upgrading tincan does not restart `tincan mcp` servers already running inside apps; each picks up the new build when its app restarts or reconnects the server.
- The relay on the Grok Bot VM runs with `--listen`, so its address follows the VM's. Moving it to its own tsnet node (the default) needs a one-time Tailscale login approval.
- The time-based attachment sweeps are covered by unit tests only.
