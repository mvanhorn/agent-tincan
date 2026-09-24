# Web agents: ChatGPT and Claude as teammates

A web agent makes chatgpt.com or claude.ai a teammate. Another agent asks it something with `tincan ask chatgpt-web "..."` and gets ChatGPT's answer back as the reply, with any images ChatGPT generated attached. `claude-web` does the same with Claude on claude.ai.

It is a Go service, `tincan web serve --site chatgpt` (or `--site claude-ai`), running on Matt's Mac next to Chrome. It is not a model. For each request it:

1. Checks access. Every agent in the request's chain, as the relay recorded it, must be on the allowlist. Otherwise it declines and names the agent.
2. Reads the optional threading line (below). The rest of the body is the message, sent as is.
3. Has the Tincan Chrome extension type the message into the site, in a background tab the extension opens itself. The extension answers as soon as the message is sent and the conversation id is in the tab's address.
4. Reads the conversation every 2 seconds through the same detail operation the history agent uses until the reply is finished (bounded by the request timeout, 8 minutes), then replies with the answer text and the generated images, fetched through the file operation, as attachments. Then it has the extension close the tab.

It handles one request at a time. The extension also runs one send per site at a time.

## What it does in your browser

This types into your logged-in ChatGPT or Claude account, as you. The messages and answers show up in your ChatGPT or Claude history like any chat you had yourself, count against your plan's usage, and follow the site's own settings (memory, custom instructions, model choice).

The extension opens a new background tab (`chrome.tabs.create` with `active: false`), fills the message box, clicks send, and reads the conversation id from the tab's address (at most 60 seconds). It does not watch the page for the answer: Chrome throttles background tabs, so page signals are unreliable there. The tab stays open while the site writes the answer, because closing it may stop claude.ai from finishing, and the web agent closes it with the fixed `chatgpt.close` or `claudeai.close` operation once the answer is read or the wait gives up. That operation closes only a tab a send opened for that conversation. A tab nobody closes is closed after 10 minutes; a send that fails closes its tab at once. It never touches a tab you opened. Chrome is never quit or restarted. The message is passed to a fixed function in the extension as data and inserted as text; nothing in it is ever run as code.

Why a tab and not an API call: both sites protect their send endpoints with anti-bot tokens that only the real page can produce. Driving the page is what survives that.

## Allowlist

By default grokbot, claude-code and codex may ask. Each web agent has its own file, `~/.config/tincan/chatgpt-web-allow.txt` or `~/.config/tincan/claude-web-allow.txt`, one agent name per line (commas and spaces also separate names, `#` starts a comment). It works exactly like the history allowlist:

- It is reread for every request.
- A missing file means the default list. An unreadable file, or a name that is not a plain agent name, declines everyone (and stops the service from starting).
- It covers the whole chain. If muse asks codex and codex asks chatgpt-web while handling muse's request, the request is declined because of muse. The chain and sender come from the relay, never from the request body.

## Threading

The first line of a request can pick the conversation:

```
new chat
What are three names for a fox mascot?
```

```
conversation: 6a1f0c2e-1111-4a2b-9c3d-000000000001
Make the second one shorter.
```

`conversation:` takes an id or a conversation URL (`https://chatgpt.com/c/<id>`, `https://claude.ai/chat/<id>`). Without either line, the message continues the conversation this asker used last with this agent, or starts one if there is none. Each asker has its own thread: grokbot's follow-ups never land in codex's conversation. The ids are kept in `~/.config/tincan/<agent>-state.json` (mode 0600; ids only, never messages). If a remembered conversation was deleted, the message goes to a new chat and the reply says so. An explicit `conversation:` id that is gone is an error.

Every reply ends with the conversation id, for example `ChatGPT conversation: 6a1f0c2e-...`, so the asker can come back to it.

## Replies

- The answer text, capped at 64 KB. A longer answer is cut and the reply says how much is shown.
- Images the assistant generated in that turn, as relay attachments (up to 8). The reply says how many were attached, or why none were.
- Messages are capped at 32 KB. A longer one is refused, not cut.
- If the answer finished but the conversation could not be read back, the reply carries the text as the page showed it and says images are not included.

## Install

The web agents use the Tincan Chrome extension and its native host, the same ones the history agent uses. If you already run `history`, that part is done.

```bash
tincan invite chatgpt-web --kind chatgpt-web          # on an admin device
TINCAN_CONFIG=~/.config/tincan/chatgpt-web.json tincan join <code> --relay http://tincan-relay
tincan history install --no-service                   # skip if history is installed
tincan web install --site chatgpt
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.agenttincan.web.chatgpt.plist
```

For Claude, use `claude-web`, `--kind claude-web`, `~/.config/tincan/claude-web.json` and `--site claude-ai` (plist `com.agenttincan.web.claude-ai.plist`).

`tincan web install` writes the launchd agent (a systemd user unit on Linux, `tincan-chatgpt-web.service`) and prints the command that starts it. It never starts anything itself. The service logs to `~/Library/Logs/tincan-chatgpt-web.log`. It refuses to start unless the relay confirms it is `chatgpt-web` (or the name given with `--name`), so a config for another agent can never claim that agent's requests. Flags for running by hand: `--site`, `--name`, `--config`, `--allowlist`, `--state`.

Set its wake method to `wait` in the relay's `wake.json`; the service long-polls. `tincan onboard` prints these steps for the `chatgpt-web` and `claude-web` kinds.

### Extension updates

The send operations need extension version 0.3.0 or later (0.2.0 waited for the answer on the page). Load `extension/` unpacked once from `chrome://extensions`. After that, updates need no Reload click: run `tincan history install` from the repo checkout (or pass `--extension-dir <path to extension/>`), and whenever the extension connects, the native host compares its version and file hashes with the files on disk and, if they differ, sends the fixed `extension.reload` operation. The extension then calls `chrome.runtime.reload()` and Chrome re-reads the files. The host asks at most once per 10 minutes for the same files, so a copy loaded from somewhere else cannot cause a reload loop. A store install is never reloaded this way.

## Troubleshooting

- "Declined: X is not on the chatgpt-web allowlist": add X to `~/.config/tincan/chatgpt-web-allow.txt` if Matt wants it to act as him in ChatGPT.
- "Declined: this request came through X": an allowed agent passed on X's request. Every agent in the chain must be allowed.
- "source unavailable: chatgpt: not logged in to chatgpt.com in Chrome" (or claude.ai): log in in Chrome. The extension checks the session before it opens a tab, so a logged-out browser never sends anonymously.
- "source unavailable: ...: the Tincan Chrome extension is not connected": install or enable the extension, then run `tincan history install`.
- "source unavailable: ...: the extension rejected the request (unknown operation; the loaded extension is older than this tincan ...)": the loaded extension predates the send operations. Reload it once from `chrome://extensions`; later updates reload themselves.
- "no message box on the chatgpt.com page (the page may have changed)": the site changed its page, or showed an interstitial (a consent or upgrade dialog). Open the site in Chrome, dismiss anything in the way, and ask again. If it persists, the selectors in `extension/send.js` need an update.
- "the message could not be sent on chatgpt.com": the text did not land in the message box, the send button stayed disabled, or the page ignored the click (for example a usage limit). Open the site and look.
- "ChatGPT did not finish answering in time": no finished answer before the request timeout. The reply names the conversation when the message was sent; look in it and ask again with `conversation: <id>`.
- "... The message was sent to ChatGPT (conversation X), but the reply could not be read": the send worked but the detail read failed for good (logged out, API changed). The answer is in the conversation on the site.
- "No ChatGPT conversation with id X was found": the id is wrong or the conversation was deleted. Use `new chat`.
- Background tabs: Chrome throttles hidden tabs. Nothing waits on the page after the send; the answer is read through the site's API.
- No reply at all: check the service is loaded (`launchctl print gui/$(id -u)/com.agenttincan.web.chatgpt`) and read its log. A "403" there means the config is not joined.

### Selectors

All page selectors live in one table, `SELECTORS` in `extension/send.js`, each with fallbacks tried in order:

| Role | chatgpt.com | claude.ai |
| --- | --- | --- |
| message box | `#prompt-textarea`, `div[contenteditable="true"][id="prompt-textarea"]`, `textarea[data-id="root"]`, `form div[contenteditable="true"]` | `div[contenteditable="true"].ProseMirror`, `fieldset div[contenteditable="true"]`, `[contenteditable="true"][aria-label*="prompt" i]`, `div[contenteditable="true"]` |
| send button | `[data-testid="send-button"]`, `#composer-submit-button`, `button[aria-label*="Send"]` (else Enter) | `button[aria-label="Send message"]`, `button[aria-label*="Send"]`, `fieldset button[type="submit"]` (else Enter) |
| answering (confirms the send only) | `[data-testid="stop-button"]`, `button[aria-label*="Stop"]`, `.result-streaming` | `button[aria-label="Stop response"]`, `button[aria-label*="Stop"]`, `[data-is-streaming="true"]` |
| assistant messages | `[data-message-author-role="assistant"]` | `[data-is-streaming]`, `.font-claude-response`, `.font-claude-message`, `[data-testid="assistant-message"]` |
| user messages | `[data-message-author-role="user"]` | `[data-testid="user-message"]` |
| logged out | `[data-testid="login-button"]`, `a[href*="/auth/login"]`, paths `/auth/login`, `/log-in` | `a[href="/login"]`, `input[type="email"]`, paths `/login`, `/logout` |

The text is entered by typing (`document.execCommand('insertText')`), then a paste event, then setting it directly, and checked after each attempt. The send counts as taken when a new chat's address gains a conversation id, or a new user or assistant message or an answering marker appears. The page is never used to decide that an answer is finished.

### When an answer is finished

The web agent decides from the conversation detail, never from the page:

- ChatGPT: the last message on the `current_node` branch is an assistant message to everyone, after this request's user message, with status `finished_successfully`, `finish_details`, or `end_turn: true` (and `end_turn` not `false`).
- claude.ai: the last message on the current branch is from the assistant, after this request's human message, and has a `stop_reason`, or else its text is the same on two reads in a row.

"This request's user message" is the last user message on the branch, as long as it is not the one that was last before the send (read just before sending into an existing conversation) and is not dated more than 2 minutes before the extension's `submitted_at`. So a finished answer to an earlier message, even with the same text, is never taken for the new one.
