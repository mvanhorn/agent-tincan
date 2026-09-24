# Launch checklist (morning of September 24, 2026)

Status: ready to launch. Release v0.5.0-rc6 is live on the relay, this Mac and the Mac mini. agenttincan.com is live. Every launch test passed on the final build except Muse's attachment test, which needs Muse upgraded (step 1 below). The release stays an rc until you are happy with that; promoting it is one tag.

## What is live

- Relay on the Grok Bot VM: v0.5.0-rc6, serving release binaries for `tincan upgrade`.
- This Mac: claude-code, codex (listener with the updated wake script), history, chatgpt-web, claude-web, all on v0.5.0-rc6. The Tincan Chrome extension (0.3.2 after its self-reload) is connected.
- Mac mini: hermes on v0.5.0-rc6, gateway restarted.
- https://agenttincan.com and https://agenttincan.com/privacy (Vercel project agenttincan). No install or repo links while the repo is private.

## Tests run on the final build (v0.5.0-rc6)

| Area | Result |
|---|---|
| claude-web and chatgpt-web answer (FIG) | pass, 13 s each |
| chatgpt-web image generation (blue square) | pass, 47 s, PNG attached |
| history: last ChatGPT prompt with its image | pass, byte-identical image |
| Grok Bot demo (Grok Bot asks history, gets the image) | pass; image arrives through `tincan attachment get` until Grok Bot's connector refreshes (step 2) |
| Hermes receives an attachment | pass, uses get_attachment |
| No new ChatGPT rate limits during testing | pass (requests spaced 2 minutes apart) |

Earlier tonight, also passed: allowlist live (grokbot allowed; muse declined directly and via codex), Codex unattended wake (found its prior thread and folder via tincan history, ran gh), long answers with end markers, Claude thinking pauses, conversation threading, send journal, fresh-customer install walked from the docs in isolation (relay, invites, joins, attachments, reply wakes, history install, the production extension in a throwaway Chrome, Linux systemd install in a container).

## Fixed tonight

- PR #18: fresh-install gaps (install docs for binaries and the extension, owner-neutral text, command output on stdout, invite prints the real relay URL, `tincan agents` on an admin device, macOS socket path, Codex recipe uses the wake script, service PATH, Linux units, relay --listen validation, a new invite retires older unused codes, `make dist`), and the web agents' rate-limit handling (see incidents).
- PR #17, #19: the site, the privacy policy page, and the Chrome Web Store package (`make store`).
- PR #20: extension icons (required by the store).

## Incidents to know about

- ChatGPT rate limit: around 12:27 AM the chatgpt-web agent's 2-second polling made chatgpt.com rate-limit your account (HTTP 429) for a while. I stopped the service within minutes. The fix (PR #18) polls gently (5 s, 8 s, 12 s, then 20 s), honors Retry-After, backs off from 30 s to 5 minutes, and shares a cooldown so nothing hammers the site again. Retests afterwards produced no rate limits. If ChatGPT felt slow for you overnight, that was why.
- Keychain prompt: during the fresh-install test a throwaway Chrome briefly showed a macOS keychain dialog on your screen; it closed with that Chrome. Your real Chrome, its profile and your files were never touched.

## Your steps this morning

1. Muse: tell Muse yourself "run tincan upgrade and restart your tincan wait loop". Its safety filter declines maintenance requests that come from other agents. Until then Muse (0.4.0-rc3) cannot receive attachments.
2. Grok Bot: refresh or reconnect its tincan MCP connector in the Grok Bot app. The binary exposes get_attachment, but the platform caches the old tool list; until refreshed, Grok Bot fetches images through the CLI (works, just not inline).
3. Chrome Web Store (optional for launch): follow docs/chrome-web-store.md. Build with `make store` (last build sha256 cb637f7d1a2e4d3769d1ffeafe4d20841383558002e5979703b905b1bc35d7bf; it changes on each rebuild). Review usually takes days, so launch uses the unpacked install. Do not switch the native host to the store id until the store build is installed.
4. Done: every joined agent, hermes included, may use history, chatgpt-web and claude-web by default (no allowlist files). To restrict one later, write its ~/.config/tincan/<agent>-allow.txt with the names allowed.
5. When you are ready to open the repo: make it public, then add install links to agenttincan.com (site/index.html) and redeploy with `cd site && vercel deploy --prod`.
6. Promote the release when happy: tag v0.5.0 on main, `make dist`, create the GitHub release with the dist files, and ask Grok Bot to update the relay the usual way.

## Known limits (disclose, not blockers)

- OpenClaw has never run live.
- The time-based attachment sweeps (24 hours for orphans, 7 days after a request finishes) are covered by unit tests only.
- ChatGPT image turns are verified against real image generation; other tool-using turns (web search, code) are unverified live.
- `tincan agents` does not show each agent's version yet, so an out-of-date agent is not flagged automatically.
