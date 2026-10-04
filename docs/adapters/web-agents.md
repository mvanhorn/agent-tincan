# Web agents: ChatGPT, Claude, Grok, Gemini, Perplexity and Copilot as teammates

A web agent makes chatgpt.com, claude.ai, grok.com, gemini.google.com, www.perplexity.ai or Microsoft Copilot (copilot.com) a teammate. Another agent asks it something with `tincan ask chatgpt-web "..."` and gets ChatGPT's answer back as the reply, with any images ChatGPT generated attached. `claude-web` does the same with Claude on claude.ai, replying with the answer as text since Claude makes no images, `grok-web` with Grok on grok.com, `gemini-web` with Gemini on gemini.google.com, attaching its images when they can be fetched (see [Gemini](#gemini) for what differs there, including the account risk and what Gemini can read), `perplexity-web` with Perplexity on www.perplexity.ai, whose replies list the answer's source links (see [Perplexity](#perplexity)), and `copilot-web` with a personal Microsoft account's Copilot on copilot.com, replying with the answer and its source links (see [Copilot](#copilot)). `dot-web` does the same with your OpenAI dot through its DM on chatgpt.com, and also lets the dot ask teammates (see [Your dot](#your-dot-dot-web)).

It is a Go service, `tincan web serve --site chatgpt` (or `--site claude-ai`, `--site grok`, `--site gemini`, `--site perplexity`, `--site copilot`), running on your Mac next to Chrome. It is not a model. For each request it:

1. Checks access. By default any agent joined to your relay may ask. If you wrote an allowlist file, every agent in the request's chain, as the relay recorded it, must be on it; otherwise it declines and names the agent.
2. Reads the optional threading line (below). The rest of the body is the message, sent as is.
3. Has the Tincan Chrome extension type the message into the site, in a background tab the extension opens itself. The extension answers as soon as the message is sent and the conversation id is in the tab's address.
4. Reads the conversation through the same detail operation the history agent uses until the reply is finished (bounded by the request timeout, 8 minutes), then replies with the answer text and the generated images, fetched through the file operation, as attachments. Then it has the extension close the tab. The first read is 5 seconds after the send, then the reads back off: 5, 8 and 12 seconds apart, then every 20 seconds. Every read is a request on your account, so it never polls faster than that.

It handles one request at a time. The extension also runs one send per site at a time. While a request waits for its answer, the agent refreshes its relay presence every 30 seconds with a peek that claims nothing, so it does not show offline; requests that arrive meanwhile stay queued for the next one.

## Rate limits

If the site answers HTTP 429, the agent waits the site's `Retry-After` (the extension reports it), or backs off from 30 seconds, doubling up to 5 minutes. It never retries at the normal cadence. If that wait would outlast the request's 8 minutes, the request fails with "ChatGPT is rate-limiting this account right now. The message was sent to ChatGPT (conversation <id>); ask for the reply later instead of sending it again." (or claude.ai, Grok, Gemini, Perplexity or Copilot): the message already went through, so sending it again would only duplicate it. While the cooldown runs, the next request fails at once, before anything is sent, with "ChatGPT is rate-limiting this account right now; try again later" instead of asking the site again. The native host keeps the same cooldown for every reader and agent that goes through it, so history reads also stop hitting the site; a history read that meets a 429 fails at once with that message, without retrying. An HTTP 5xx while waiting also backs off (doubling, up to 2 minutes).

Each site has its own cooldown: a rate limit on one never holds back another. Gemini also cools down for 5 minutes after Google shows its anti-bot page (`/sorry/`), Perplexity for 5 minutes after a Cloudflare challenge, and Copilot for 5 minutes after it shows its "Verification required" human check, so the agent does not keep tripping them on your account; while that runs, requests fail at once with the anti-bot reply instead of asking the site again. ChatGPT and claude.ai start no cooldown on an anti-bot answer.

Grok can also end an answer on the plan's usage limit instead of answering with a 429: the finished response carries a stream error naming a rate or usage limit. The agent treats that like a 429 it cannot wait out: the request fails with "Grok is rate-limiting this account right now. The message was sent to Grok (conversation <id>); ask for the reply later instead of sending it again.", and Grok is left alone for 15 minutes, during which requests to grok-web fail at once without sending.

## What it does in your browser

This types into your logged-in ChatGPT, Claude, Grok, Gemini, Perplexity or Copilot account, as you. The messages and answers show up in that site's history like any chat you had yourself, count against your plan's usage, and follow the site's own settings (memory, custom instructions, model choice).

Account risk: xAI's terms prohibit automated access to Grok. grok-web acts as you, on your own account, one request at a time and at the human-paced cadence below, through the same page a person uses; it does not scrape, bulk-export or share the account. xAI can still limit or suspend an account it believes is automated, so turn grok-web on only if you accept that.

The extension opens a new background tab (`chrome.tabs.create` with `active: false`), fills the message box, clicks send, and reads the conversation id from the tab's address (at most 60 seconds). It does not watch the page for the answer: Chrome throttles background tabs, so page signals are unreliable there. The tab stays open while the site writes the answer, because closing it may stop claude.ai from finishing, and the web agent closes it with the fixed `chatgpt.close`, `claudeai.close`, `grok.close`, `gemini.close`, `perplexity.close` or `copilot.close` operation once the answer is read or the wait gives up. That operation closes only a tab a send opened for that conversation. A tab nobody closes is closed after 10 minutes; a send that fails closes its tab at once. It never touches a tab you opened. Chrome is never quit or restarted. The message is passed to a fixed function in the extension as data and inserted as text; nothing in it is ever run as code.

Why a tab and not an API call: all five sites protect their send endpoints with anti-bot tokens that only the real page can produce (grok.com's send carries a per-request `x-statsig-id`). Driving the page is what survives that.

Reads are plain JSON requests from the extension's service worker with your cookies. For grok.com they are `GET /rest/app-chat/conversations` (the list), `GET /rest/app-chat/conversations/<id>/response-node?includeThreads=true` (the message tree and what is still being written) and `POST .../load-responses` (the message bodies); none of them needs a page-set header. If grok.com ever refuses reads from the extension's origin, the one function that fetches them (`grokJSON` in `extension/ops.js`) is where a fixed read in an extension-opened grok.com tab's isolated world would go; nothing ever runs in the page's own JavaScript.

### Grok: grant it first

grok.com is an optional site: the extension has no access to it until you grant it. Open `chrome://extensions` > Agent Tincan History > Details > Extension options and click Grant for Grok; Chrome asks for grok.com and its image host, assets.grok.com, together. Chrome takes the grant only from that click, so it is a step for you, not for an agent. Until then every grok-web request is answered with that step, and while the extension is connected `tincan web serve --site grok` waits for the grant, checking again every minute (see the grant reply below). grok.com also lets a logged-out browser chat anonymously, so before opening a tab the extension reads your conversation list, and the send tab then checks the page for grok.com's sign-in link or `/sign-in` address before typing; a logged-out browser fails at one of the two and never sends.

## Allowlist

By default there is no allowlist file and every agent joined to your relay may ask, so any agent on your mesh can act as you in ChatGPT, Claude, Grok, Gemini, Perplexity or Copilot (and, through Gemini, read what Gemini can read; see [Gemini](#gemini)). The startup log says `allowlist: all joined agents (no file at ~/.config/tincan/chatgpt-web-allow.txt)`.

To restrict it, write the web agent's own file, `~/.config/tincan/chatgpt-web-allow.txt`, `~/.config/tincan/claude-web-allow.txt`, `~/.config/tincan/grok-web-allow.txt`, `~/.config/tincan/gemini-web-allow.txt`, `~/.config/tincan/perplexity-web-allow.txt` or `~/.config/tincan/copilot-web-allow.txt`, with one agent name per line (commas and spaces also separate names, `#` starts a comment). It works exactly like the history allowlist:

- It is reread for every request.
- A file of names allows only those names. A `*` entry means every joined agent, the same as no file. An empty file allows nobody.
- An unreadable file, or an entry that is neither `*` nor a plain agent name, declines everyone (and stops the service from starting).
- A file of names covers the whole chain. If muse asks codex and codex asks chatgpt-web while handling muse's request, a file that lists codex but not muse declines it because of muse. The chain and sender come from the relay, never from the request body.

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

`conversation:` takes an id or a conversation URL (`https://chatgpt.com/c/<id>`, `https://claude.ai/chat/<id>`, `https://grok.com/c/<id>`, `https://gemini.google.com/app/<id>`, also `/u/<n>/app/<id>` and `/gem/<name>/<id>`, `https://www.perplexity.ai/search/<slug>`, `https://copilot.com/chat/conversation/<id>`). Without either line, the message continues the conversation this asker used last with this agent, or starts one if there is none. Each asker has its own thread: grokbot's follow-ups never land in codex's conversation. The ids are kept in `~/.config/tincan/<agent>-state.json` (mode 0600; ids only, never messages). If a remembered conversation was deleted, the message goes to a new chat and the reply says so. An explicit `conversation:` id that is gone is an error.

Every conversation the agent sends into is also added to `~/.config/tincan/web-agent-<site>-conversations.json` (mode 0600; ids and times only, newest 1000 kept), so the history agent leaves these chats out when you ask what you last asked ChatGPT, Claude, Grok, Gemini or Copilot. (Perplexity is not a history source; its list is kept all the same.)

Every reply ends with the conversation id, for example `ChatGPT conversation: 6a1f0c2e-...` or `Grok conversation: 0e1d0000-...`, so the asker can come back to it.

## Replies

- The answer text, capped at 64 KB. A longer answer is cut and the reply says how much is shown.
- Images the assistant generated in that turn, as relay attachments (up to 8). The reply says how many were attached, or why none were. Grok's images are fetched from assets.grok.com by the extension, which looks each URL up again from the conversation by response id and index (no URL is ever passed to it); an image it cannot fetch is left out and the reply says "The images could not be attached." Grok videos are not attached.
- Perplexity and Copilot: after the answer text, a blank line and a `Sources:` list of the answer's source links, in the site's order, each URL once, only http and https links without credentials, at most 10 and then an `(and N more)` line. Perplexity numbers its sources, so its lines are `- [n] <title> <url>` (see [Perplexity](#perplexity)); Copilot's are `- <title> <url>` (see [Copilot](#copilot)). The list, any note and the conversation footer are kept whole; the answer text is what gives way to stay inside the 64 KB.
- Messages are capped at 32 KB. A longer one is refused, not cut.
- If the answer finished but the conversation could not be read back, the request fails saying the message was sent and naming the conversation; the answer is there on the site.

## Install

The web agents use the Tincan Chrome extension and its native host, the same ones the history agent uses. If you already run `history`, that part is done. Otherwise load the extension first as described in [history.md](history.md#install) (unzip `tincan-history-extension.zip` into a folder you keep, then Load unpacked in `chrome://extensions` with Developer mode on).

```bash
tincan invite chatgpt-web --kind chatgpt-web          # on an admin device
TINCAN_CONFIG=~/.config/tincan/chatgpt-web.json tincan join <code> --relay http://tincan-relay
tincan history install --no-service --extension-dir ~/tincan-extension   # skip if history is installed
tincan web install --site chatgpt
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.agenttincan.web.chatgpt.plist
```

For Claude, use `claude-web`, `--kind claude-web`, `~/.config/tincan/claude-web.json` and `--site claude-ai` (plist `com.agenttincan.web.claude-ai.plist`). For Grok, use `grok-web`, `--kind grok-web`, `~/.config/tincan/grok-web.json` and `--site grok` (plist `com.agenttincan.web.grok.plist`), and grant grok.com on the extension's options page first (above). A relay older than the `grok-web` kind refuses `--kind grok-web`, and the CLI says so; upgrade the relay, or invite `grok-web` without `--kind` and run `tincan onboard --kind grok-web=grok-web` for its block. For Gemini, use `gemini-web`, `--kind gemini-web`, `~/.config/tincan/gemini-web.json` and `--site gemini` (plist `com.agenttincan.web.gemini.plist`), and first grant Gemini on the extension's options page (see [Gemini](#gemini)). For Perplexity, use `perplexity-web`, `--kind perplexity-web`, `~/.config/tincan/perplexity-web.json` and `--site perplexity` (plist `com.agenttincan.web.perplexity.plist`), and first grant Perplexity on the extension's options page (see [Perplexity](#perplexity)). For Copilot, use `copilot-web`, `--kind copilot-web`, `~/.config/tincan/copilot-web.json` and `--site copilot` (plist `com.agenttincan.web.copilot.plist`), and first grant Copilot on the extension's options page (see [Copilot](#copilot)).

`tincan web install` writes the launchd agent (a systemd user unit on Linux, `tincan-chatgpt-web.service`) and prints the command that starts it. It never starts anything itself. On a headless Linux box, run `loginctl enable-linger $USER` once so the user service keeps running after you log out. The service logs to `~/Library/Logs/tincan-chatgpt-web.log`. It refuses to start unless the relay confirms it is `chatgpt-web` (or the name given with `--name`), so a config for another agent can never claim that agent's requests. Flags for running by hand: `--site`, `--name`, `--config`, `--allowlist`, `--state`.

Set its wake method to `wait` in the relay's `wake.json`; the service long-polls. `tincan onboard` prints these steps for the `chatgpt-web`, `claude-web`, `grok-web`, `gemini-web`, `perplexity-web` and `copilot-web` kinds. A relay older than this release does not know the `grok-web`, `gemini-web`, `perplexity-web` or `copilot-web` kind: upgrade the relay first, or invite without `--kind` and pass `tincan onboard --kind <name>=<kind>`.

### Extension updates

The ChatGPT and Claude send operations need extension version 0.3.0 or later (0.2.0 waited for the answer on the page); Grok and Gemini need 0.4.0, which adds the options page that grants them; Perplexity and Copilot need the extension from this release, which adds their operations. Load the extension unpacked once from `chrome://extensions` (the unzipped `tincan-history-extension.zip`, or `extension/` in a repo checkout). After that, updates need no Reload click: run `tincan history install --extension-dir <the folder you loaded>` (or run it from a repo checkout), and whenever the extension connects (and every 10 minutes while it stays connected), the native host compares its version and file hashes with the files on disk and, if they differ, sends the fixed `extension.reload` operation. The extension hashes its files once when its worker starts, so the hashes describe the code Chrome loaded even after the files on disk change. On `extension.reload` it calls `chrome.runtime.reload()` and Chrome re-reads the files, but not while a send is typing in a tab or a finished send's tab is waiting to be closed (a reload would lose track of those tabs): it checks again every 5 seconds for up to 5 minutes. At that cap it closes the finished sends' tabs and reloads anyway; a send still typing then is abandoned, its tab stays open, and that request fails when its wait times out. The host asks at most once per 10 minutes for the same files when the extension connects, and the 10 minute re-check never asks again for files it already asked about, so a copy loaded from somewhere else cannot cause a reload loop; only the files changing again, or a reconnect, asks again. A store install is never reloaded this way.

## Troubleshooting

- "Declined: X is not on the chatgpt-web allowlist": add X to `~/.config/tincan/chatgpt-web-allow.txt` if you want it to act as you in ChatGPT.
- "Declined: this request came through X": an allowed agent passed on X's request. Every agent in the chain must be allowed.
- "source unavailable: chatgpt: not logged in to chatgpt.com in Chrome" (or claude.ai, grok.com, www.perplexity.ai): log in in Chrome. For Copilot the reply says "not signed in to Copilot in Chrome": open https://copilot.microsoft.com, sign in with a personal Microsoft account and finish any sign-in or terms page Microsoft shows. The extension checks the session before it opens a tab, so a logged-out browser never sends anonymously.
- "source unavailable: ...: the Tincan Chrome extension is not connected": install or enable the extension, then run `tincan history install`.
- "the Tincan Chrome extension has no access to chatgpt.com; grant it on the extension's options page": Chrome has not granted the extension that site (it was withheld in the extension's site access settings, or the site is one that must be granted first). Open `chrome://extensions` > Agent Tincan History > Details > Extension options and click Grant. While the extension is connected and reports such a site ungranted, `tincan web serve` logs this once and waits, checking again every minute, and starts serving as soon as the grant appears (no restart needed); with no extension connected it starts and answers with the not-connected reply until Chrome is up. Withholding only ChatGPT's file host (`*.oaiusercontent.com`) does not count as ungranted: conversations still list and read, and only images stored on that host fail to download.
- "chatgpt.com showed an anti-bot check": the site answered with a Cloudflare challenge, an anti-bot refusal (grok.com's 403 "Request rejected by anti-bot rules") or Google's "unusual traffic" page instead of its API, or the send tab opened on a challenge page. Open the site in Chrome, complete the check, then ask again.
- "the Tincan Chrome extension has no access to grok.com": grant Grok on the extension's options page (above).
- "source unavailable: ...: the extension rejected the request (unknown operation; the loaded extension is older than this tincan ...)": the loaded extension predates the send operations. Reload it once from `chrome://extensions`; later updates reload themselves.
- "no message box on the chatgpt.com page (the page may have changed)": the site changed its page, or showed an interstitial (a consent or upgrade dialog). Open the site in Chrome, dismiss anything in the way, and ask again. If it persists, the selectors in `extension/send.js` need an update.
- "the message could not be sent on chatgpt.com": the text did not land in the message box, the send button stayed disabled, or the page ignored the click (for example a usage limit). Open the site and look.
- "ChatGPT did not finish answering in time": no finished answer before the request timeout. The reply names the conversation when the message was sent; look in it and ask again with `conversation: <id>`. If the answer is still coming, wait for it to finish first, or the send is refused (see below).
- "the conversation is still answering an earlier message": the conversation still shows a stop button or a streaming answer, so the send was refused. Wait for the answer on the site to finish and ask again, or use `new chat`.
- "... The message was sent to ChatGPT (conversation X), but the reply could not be read": the send worked but the detail read failed for good (logged out, API changed). The answer is in the conversation on the site.
- "No ChatGPT conversation with id X was found": the id is wrong or the conversation was deleted. Use `new chat`.
- Background tabs: Chrome throttles hidden tabs. Nothing waits on the page after the send; the answer is read through the site's API.
- No reply at all: check the service is loaded (`launchctl print gui/$(id -u)/com.agenttincan.web.chatgpt`) and read its log. A "403" there means the config is not joined.

### Selectors

All page selectors live in one table, `SELECTORS` in `extension/send.js`, each with fallbacks tried in order (grok.com in the second table):

| Role | chatgpt.com | claude.ai | gemini.google.com |
| --- | --- | --- | --- |
| message box | `#prompt-textarea`, `div[contenteditable="true"][id="prompt-textarea"]`, `textarea[data-id="root"]`, `form div[contenteditable="true"]` | `div[contenteditable="true"].ProseMirror`, `fieldset div[contenteditable="true"]`, `[contenteditable="true"][aria-label*="prompt" i]`, `div[contenteditable="true"]` | `div.ql-editor[aria-label="Enter a prompt for Gemini"]`, `rich-textarea div.ql-editor[contenteditable="true"]`, `div.ql-editor[contenteditable="true"]` |
| send button | `[data-testid="send-button"]`, `#composer-submit-button`, `button[aria-label*="Send"]` (else Enter) | `button[aria-label="Send message"]`, `button[aria-label*="Send"]`, `fieldset button[type="submit"]` (else Enter) | `button[aria-label*="Send" i]`, `button.send-button` (appears once there is text; else Enter) |
| answering (refuses a send while present; confirms a send after the click) | `[data-testid="stop-button"]`, `button[aria-label*="Stop"]`, `.result-streaming` | `button[aria-label="Stop response"]`, `button[aria-label*="Stop"]`, `[data-is-streaming="true"]` | `button[aria-label*="Stop" i]` |
| assistant messages | `[data-message-author-role="assistant"]` | `[data-is-streaming]`, `.font-claude-response`, `.font-claude-message`, `[data-testid="assistant-message"]` | `model-response`, `message-content` |
| user messages | `[data-message-author-role="user"]` | `[data-testid="user-message"]` | `user-query` |
| logged out | `[data-testid="login-button"]`, `a[href*="/auth/login"]`, paths `/auth/login`, `/log-in` | `a[href="/login"]`, `input[type="email"]`, paths `/login`, `/logout` | `a[href*="accounts.google.com/ServiceLogin"]`, `a[href*="accounts.google.com/v3/signin"]` |

On every site, a send tab the site sends to another host is not typed into: Google's `/sorry/` page is `blocked`, any other host (a sign-in page) is `not_logged_in`. When that happens after the send button was clicked, the message may already be in the conversation, so the failure reply says so and asks you to check the conversation before sending it again.

| Role | grok.com |
| --- | --- |
| message box | `div[contenteditable="true"][aria-label="Ask Grok anything"]`, `form div.ProseMirror[contenteditable="true"]`, `div[contenteditable="true"].ProseMirror`, `textarea[aria-label*="Ask Grok"]` |
| send button (appears only once there is text) | `form button[type="submit"][aria-label="Submit"]`, `button[aria-label="Submit"]`, `form button[type="submit"]` (else Enter) |
| answering | `button[aria-label="Stop model response"]`, `button[aria-label*="Stop"]`, `[data-streaming="true"]` |
| assistant messages | `div[id^="response-"].items-start` |
| user messages | `div[id^="response-"].items-end` |
| logged out | `a[href^="/sign-in"]`, `a[href*="accounts.x.ai/sign-in"]`, `a[href*="/sign-up"]`, paths `/sign-in`, `/sign-up` |
| anti-bot page (fails `blocked` before anything is typed) | `#challenge-form`, `iframe[src*="challenges.cloudflare.com"]`, `#cf-challenge-running`, a "Just a moment..." title |

| Role | www.perplexity.ai |
| --- | --- |
| message box | `div#ask-input[contenteditable="true"]` (checked live; text lands only through `insertText`, not synthetic key events), `#ask-input[contenteditable="true"]`, `textarea#ask-input` |
| send button (appears once there is text) | `button[aria-label="Submit"]` (checked live; a visible match is preferred), `button[data-testid="submit-button"]`, `button[aria-label*="Submit" i]` (else Enter) |
| startup dialogs (closed before typing) | "Maybe later" inside `[role="dialog"]` or `[role="alertdialog"]` (the promo); "Decline optional" inside a dialog, `[role="region"]` or a cookie or consent container (the cookie banner). Only these exact button texts are clicked, never "Get started" or "Got it". |
| answering | `button[aria-label*="Stop" i]`, `button[data-testid="stop-generating-response-button"]` |
| assistant messages | `[id^="markdown-content-"]` |
| user messages | `[data-testid="user-query"]`, `h1[class*="query"]` |
| logged out | `a[href^="/auth/signin"]`, `a[href^="/login"]`, `button[data-testid="login-button"]`, paths `/auth/signin`, `/auth/signup`, `/login` |
| anti-bot page | as grok.com |

A live send in the owner's tab confirmed the message box, `insertText`, the Submit button and the new address `https://www.perplexity.ai/search/<uuid>`. The stop, user, assistant and login selectors are still unconfirmed; a new chat's send is confirmed by the address gaining `/search/<slug>`, which does not depend on them.

The text is entered by typing (`document.execCommand('insertText')`), then a paste event, then setting it directly, and checked after each attempt. Before each click the page is probed again, and an existing conversation's address is checked again. The send counts as taken when, compared with that probe, a new chat's address gains a conversation id, or a new user or assistant message or an answering marker appears. The page is never used to decide that an answer is finished.

### When an answer is finished

The web agent decides from the conversation detail, never from the page:

First it finds this request's own user message: the first user (claude.ai: human) message on the current branch after the one that was last before the send (read just before sending into an existing conversation), whose text is the text sent (ignoring whitespace and the markdown marks ``#*`>_-``, the same comparison the extension uses to check the message box), and that is not dated more than 2 minutes before the extension's `submitted_at`. Once seen, that message is fixed for the rest of the wait. So neither a finished answer to an earlier message, even with the same text, nor the answer to a message sent later in the same conversation (you typing there meanwhile) is taken for this one.

The answer is the last message after that user message and before the next user message on the branch, if any:

- ChatGPT: an assistant message to everyone, with status `finished_successfully`, `finish_details`, or `end_turn: true` (and `end_turn` not `false`).
- claude.ai: an assistant message with a `stop_reason`, or else whose text is the same on 4 reads in a row spanning at least 10 seconds, so a pause mid-answer is not taken for the end.
- Grok: an assistant response with `partial: false` while the conversation's `inflightResponses` is empty. grok.com marks this explicitly, so there is no text-stability wait. The current branch is walked back through `parentResponseId` from the newest response, so a regenerated answer replaces the draft it regenerated.
- Gemini: its read has no finished marker, so the chosen answer counts as finished once its text (and image count) is the same on 4 reads in a row spanning at least 45 seconds. Gemini can hold its text still for a long while as it thinks or searches, so the window is longer than claude.ai's; an answer still changing when the 8 minutes run out ends with the "did not finish answering in time" reply.
- Perplexity: the thread entry's `status` is `COMPLETED` and its answer block's `markdown_block.progress` is `DONE`. This is explicit, so there is no text-stability wait: a `COMPLETED` entry whose markdown has no `DONE` yet is still waited on (checked live on a real send). An entry of the older shapes, with no markdown block, counts as finished only once it holds answer text, and with none it is read as a changed API. An entry that ends `FAILED` ends the turn with an empty reply.
- Copilot: its page JSON has no finished marker either, so the answer (the bot messages after this request's message, joined) counts as finished once its text is the same on 3 reads in a row spanning at least 20 seconds.
If a later user message follows this request's message with nothing between them, no answer will come, and the request fails saying another message was sent in the conversation first. If this request's answer is still being written when a later message appears, the agent keeps waiting for it.

## Gemini

`gemini-web` (`tincan web serve --site gemini`) works like the other two, with these differences.

Account risk. Google's terms do not allow automated access to Gemini. The agent acts as you, on your own account, at a human pace (one request at a time, reads 5 to 20 seconds apart, a cooldown after any rate limit or anti-bot page), but that does not make it permitted, and Google enforcement is not limited to Gemini: it can reach your whole Google account (Gmail, Drive, everything else signed in with it). Decide whether that risk is acceptable before you run it.

What Gemini can read. Gemini answers can draw on the Google apps connected to your account, such as Gmail, Drive and Calendar. Anyone who can ask `gemini-web` can therefore read that data through it ("summarize my latest emails"), not just chat with Gemini. With no allowlist file every agent joined to your relay may ask, so for `gemini-web` write `~/.config/tincan/gemini-web-allow.txt` listing only the agents that should, or disconnect those apps in Gemini's settings.

Grant first. Gemini is an optional site in the extension: an upgrade asks Chrome for nothing new. Open `chrome://extensions` > Agent Tincan History > Details > Extension options and click Grant next to Gemini. The grant covers `gemini.google.com` and `lh3.googleusercontent.com`, where Gemini's images are served. Until then every Gemini operation fails `permission_missing` without a tab or a request, `tincan web serve --site gemini` logs it and waits while the extension is connected, checking again every minute and serving as soon as the grant appears, and requests get a reply naming the options page. Withholding only the image host leaves list, read and send working; only image downloads fail.

Reads. Gemini has no JSON API of its own, so the extension's service worker fetches the app page (`/app`) for its per-session values (`SNlM0e`, `cfb2h`, `FdrFJe`, kept in the worker's memory for 10 minutes and never returned), then calls the app's `batchexecute` endpoint with two fixed rpcids: `MaZiqc` lists conversations 13 at a time, following each page's token, and `hNvQHb` reads a conversation's latest 10 turns. The extension hands back the decoded payloads as data; Go reads their positions defensively, and any shape it does not recognize is reported as "gemini.google.com changed its API", never a crash. A stale token (HTTP 400 or 401) fetches the app page again once.

Conversation ids. The canonical id is the hex in the address, `https://gemini.google.com/app/<id>` (16 hex digits). `batchexecute` calls it `c_<id>`; that form exists only inside the extension. `conversation:` accepts `/app/<id>`, `/u/<n>/app/<id>`, `/gem/<name>/<id>` and `c_<id>`, and every store (the per-asker state, the used list, the send journal, the reply footer `Gemini conversation: <id>`) holds the hex. The agent uses the first Google account signed in to Chrome (`/app`, not `/u/1/`); a conversation of another signed-in account is not found.

Images. Gemini's generated images are rendered from `lh3.googleusercontent.com`. The fixed `gemini.file` operation never takes a URL: it gets the conversation id and `<response id>-<n>`, reads the conversation again, and takes the n-th image URL of that response, only on that host. It then tries, in order: a fetch inside the tab the send left open (the isolated world, with the page's cookies, the same request the page makes), then a fetch from the service worker with the image host's grant. It never draws the page's `<img>` onto a canvas: a cross-origin image taints it. If both fail, the reply is the text plus a note that Gemini's reply had images that could not be attached; open the conversation to see them. Which of the two attempts works against the live site has not been confirmed yet. Gemini's text stands in for a generated image with a `googleusercontent.com/..._content/<n>` link; those are dropped from the reply text. History reads fetch images the worker way only (there is no send tab).

Not in v1: Deep Research (it needs a mode switch and a plan confirmation the plain composer send does not do), and choosing a model or a Gem for a new chat (a `conversation:` in a Gem's chat continues it).

## Perplexity

`perplexity-web` (`tincan web serve --site perplexity`) sends to Perplexity on www.perplexity.ai and replies with the answer and its source links. It differs from the others in these ways.

Account risk. Perplexity's terms do not allow using the service by automated means. The agent acts as you, on your own account, at a human pace (one request at a time, reads 5 to 20 seconds apart, a cooldown after any rate limit or Cloudflare challenge), but Perplexity can still limit or suspend an account it believes is automated. Turn it on only if you accept that.

Grant first. Perplexity is an optional site: open `chrome://extensions` > Agent Tincan History > Details > Extension options and click Grant next to Perplexity (it asks for `www.perplexity.ai`; `perplexity.ai` redirects there). Until then every Perplexity operation fails `permission_missing`, and `tincan web serve --site perplexity` waits for the grant as for Grok and Gemini.

Never anonymous. Perplexity answers signed-out visitors too, with a working message box, so a redirect to a sign-in page cannot be relied on. Before opening a tab, `perplexity.send` asks `GET /api/auth/session` and goes on only when it names a signed-in user with an id (signed out, it answers `{}`); the user's fields are only checked, never returned or kept. The send tab then checks the page for a sign-in link or a sign-in address before typing. A signed-out browser fails at the first gate and never gets a tab.

Reads. `perplexity.detail` reads the thread with `GET /rest/thread/<slug>?with_parent_info=true&with_schematized_response=true&version=2.18&source=default&limit=10&offset=0&from_first=true`, then follows `next_cursor` while `has_next_page` is true (at most 20 pages, 200 entries). A thread longer than that is not continued: a remembered one goes to a new chat with a note, a named one (`conversation:`) is refused before anything is sent, and a thread that grows past it during the wait fails at once with a note instead of timing out. It returns only the entry fields the Go side reads (`uuid`, `backend_uuid`, `status`, `query_str`, `thread_url_slug`, the entry times and `blocks`); the thread's `read_write_token` never leaves the service worker. Each entry is one question and its answer: the question is `query_str`, the answer is the `ask_text` block's `markdown_block.answer`, and the sources are the `web_results` block's `web_result_block.web_results`. Go parses this defensively (an unknown shape is "www.perplexity.ai changed its API"); older shapes (a workflow block's answer items, the entry text's final step) are read as fallbacks. `extension/ops.js`'s `perplexityJSON` is the one place these reads are fetched, should they ever have to move into an extension-opened tab's isolated world. Perplexity is not a history source: there is no list or file operation and `tincan history perplexity` is refused.

Sources. The answer keeps Perplexity's `[n]` citation markers, and the sources are numbered by their place in Perplexity's list so the markers name them: after the answer, a blank line, `Sources:`, then `- [n] <title> <url>` per source. Results that are not web pages (images, attachments, memories, earlier turns) are left out but keep their numbers counted; a URL listed twice is shown once, at its first number; only http and https links without a user name or password in them are listed, and none over 2048 bytes; after 10 the rest are counted in an `(and N more)` line. The sources list is never cut: a long answer gives way so both stay inside the 64 KB cap.

Thread ids. The conversation id is the slug in `https://www.perplexity.ai/search/<slug>`; new threads have uuid-shaped slugs. Older threads whose slugs contain other characters (a dot) cannot be named with `conversation:`.

Not in v1: focus modes, model pickers and Deep Research (the default mode is used), images and file attachments in replies.

## Copilot

`copilot-web` (`tincan web serve --site copilot`) asks consumer Microsoft Copilot, the one a personal Microsoft account reaches at copilot.microsoft.com, which now sends it to `https://copilot.com/chat`. It is not GitHub Copilot, and not Microsoft 365 Copilot for work or school accounts.

Account risk. The Microsoft Services Agreement does not allow automated access to Copilot. The agent acts as you, on your own account, at a human pace (one request at a time, reads 5 to 20 seconds apart, a cooldown after any rate limit or human check), but that does not make it permitted, and Microsoft enforcement can reach your whole Microsoft account (Outlook, OneDrive and everything else signed in with it). Decide whether that risk is acceptable before you run it.

Sign in first. Open https://copilot.microsoft.com in Chrome once, sign in with a personal Microsoft account, and finish anything Microsoft asks on the way (a "We're updating our terms" page on account.live.com, a sign-in on login.live.com or login.microsoftonline.com). A send tab that ends up on any of those, or on any host the extension cannot see, is `not_logged_in`: nothing is typed and the reply says to open Copilot in Chrome and finish the prompt. A tab that lands on a Microsoft 365 host (`*.cloud.microsoft`, `office.com`) gets a reply saying a personal Microsoft account is needed. copilot.com has no cookie-only account check the extension may call, so the send tab is the session gate: before anything is typed it must stay on copilot.com and show a signed-in account (the sidebar's chat list or the account button), not only a message box.

Grant first. Copilot is an optional site in the extension: open `chrome://extensions` > Agent Tincan History > Details > Extension options and click Grant next to Copilot. The grant covers `copilot.com` and `copilot.microsoft.com`. Until then every Copilot operation fails `permission_missing` without a tab or a request, and `tincan web serve --site copilot` waits for the grant as for the other optional sites.

Sending. The extension types into the composer (`#m365-chat-editor-target-element`, the "Message Copilot" box) with `insertText`, clicks the Send button that appears, or presses Enter when there is none, and reads the new conversation's id (a UUID) from `https://copilot.com/chat/conversation/<id>`.

Human check. Copilot can answer a send with a "Verification required" dialog ("Verify you are human", a Turnstile widget) instead of sending. The extension looks for that dialog, and a Cloudflare challenge frame, before it types and after it clicks; it never touches the widget. The send fails `blocked` (marked as clicked when it came after the click, so the reply asks you to check the conversation before resending), and Copilot requests are held back for 5 minutes while you complete the check in Chrome.

Reads. A conversation is read from copilot.com's own page data: the extension's service worker fetches `https://copilot.com/chat/conversation/<id>` asking for `application/json`, with your cookies and no token, and keeps only what Go reads (the title, the times, and each user or bot message's id, text, time and source links); the rest of that answer, including a reconnect token and token-bearing telemetry, is dropped in the worker and never returned or logged. Tool, search and suggestion messages are left out. The chat list has no such JSON (the app gets it with a bearer token the extension never touches), so `copilot.list` opens `https://copilot.com/chat` in a background tab of the extension's own, reads the sidebar's chat links (`#m365-copilot-chats-section`) with a fixed isolated-world function, scrolls it for more, and closes the tab. So a Copilot history question opens a background tab for a few seconds, like a send; such reads run one at a time with sends and at least 3 seconds apart. The extension never reads Copilot's tokens (they are in the page's storage) and never runs code in the page's own JavaScript.

Sources. Copilot's source links come from each answer's `sourceAttributions` (title and link). Copilot gives them no numbers, so they are listed after the answer with the same footer as Perplexity's, without the `[n]`: a blank line, `Sources:`, then `- <title> <url>` per source (just `- <url>` when a source has no title), with the same rules for duplicates, schemes, the 10-source cap and the 64 KB budget.

Not in v1: generated images and files in replies, "Think deeper" and voice, and picking a model.

| Role | copilot.com |
| --- | --- |
| message box | `#m365-chat-editor-target-element`, `[role="textbox"][aria-label="Message Copilot"]`, `span[contenteditable="true"][aria-label*="Copilot" i]` |
| send button (appears once there is text) | `button[aria-label="Send"]`, `button[data-testid="sendButton"]`, `button[aria-label="Submit message"]` (else Enter) |
| answering | `button[aria-label*="Stop generating" i]`, `button[aria-label="Stop"]` |
| assistant messages | `[data-testid="markdown-reply"]` |
| user messages | `[data-testid="chatQuestion"]` |
| logged out | `a[href*="login.live.com"]`, `a[href*="login.microsoftonline.com"]`, `button[aria-label="Sign in"]`, paths `/signin`, `/login` |
| signed in (required before typing) | `#m365-copilot-chats-section`, `#mectrl_main_trigger`, `button[aria-label*="Account manager" i]` |
| human check (fails `blocked`) | a `[role="dialog"]` or `[role="alertdialog"]` whose text has "Verification required" or "Verify you are human", `iframe[src*="challenges.cloudflare.com"]` |
| chat list (history) | links `a[href*="/chat/conversation/"]` inside `#m365-copilot-chats-section` |

### Retries and duplicate sends

The relay requeues a claimed request whose reply never arrives (after its 30-minute claim lease), for example when the agent crashed or the reply failed to reach the relay. A send is not repeated for that: right after the extension confirms a send, the agent records the request id, conversation id, the id of the message it sent (once seen) and `submitted_at` in `~/.config/tincan/<agent>-journal.json` (mode 0600; ids and times only, never messages or answers), and marks the entry answered once it replies. A request found there is not sent again; the agent reads the answer from the journaled conversation (waiting for it if needed) and replies. Entries are dropped after 90 minutes.

## Your dot (dot-web)

`dot-web` (`tincan web serve --site dots`) makes your OpenAI dot a teammate in both directions. A teammate asks it something and gets the dot's answer back, and the dot can ask teammates for help and get their answers in its DM. There is no Dots API: everything goes through the dot's DM page (`https://chatgpt.com/dots/<thread-id>`) in your logged-in Chrome, through the Tincan extension. Nothing is installed on the dot's computer, and there is no new site to grant: the dots operations use the extension's existing ChatGPT grant.

A dot acts on its own in the apps you connected to it (Gmail, GitHub, Google Drive and others). A message typed into its DM shows up as your own message, so whatever a teammate sends it carries your authority. That is why requests to `dot-web` are held for your approval by default (see [Approval](#approval) below).

### Running it

1. Find the dot's thread id. Open the dot's DM in ChatGPT; the address is `https://chatgpt.com/dots/<thread-id>`. The id is the last part (hex and hyphens).
2. Invite and join it, like the other web agents:

   ```bash
   tincan invite dot-web --kind dot-web                                        # relay machine (or an admin device)
   TINCAN_CONFIG=~/.config/tincan/dot-web.json tincan join <code> --relay http://tincan-relay
   ```

3. Run it in the foreground to try it:

   ```bash
   tincan web serve --site dots --name dot-web --thread <thread-id>
   ```

   or install it as a service, which keeps the thread id in the service definition:

   ```bash
   tincan web install --site dots --thread <thread-id>
   ```

   and start it with the command it prints. `--thread` is required for `--site dots` and refused for every other site. Logs go to `~/Library/Logs/tincan-dot-web.log`.

Sign in to chatgpt.com in the same Chrome first. The extension needs a build with the `dots.*` operations.

### Asking the dot

`tincan ask dot-web "..."` (once you approve it) has the extension open the dot's DM in a background tab, type the request into the message box and click Send. The page's address never changes, so the send is confirmed by the new message appearing in the DM's feed. The whole body is the message: a dot has one DM, so there is no `new chat` or `conversation:` line. A first line that is exactly `new chat` (or `new chat:`), as every council prompt has, is dropped before typing.

The request appears in the DM as your own message. The answer is every message the dot posts after it, joined in order with a blank line between them, once the same messages are read on 3 polls spanning at least 45 seconds. A dot can answer in several messages a while apart, and this lets them all in. Attachments the dot sends are not carried; the reply notes how many there were and says to open the DM. The wait is bounded by the 8 minute request timeout.

The agent never types over text already in the message box, so a draft you are writing in the DM is left alone and the request fails instead. It never types into a tab you opened.

### The dot asking teammates

While it runs, `dot-web` reads the DM every 30 seconds when idle (backing off on errors and rate limits, and never while one of its own requests is being sent). A dot message whose first line starts with `@tincan ask <agent>` becomes one ask from `dot-web` to that agent. The rest of that line and every line after it are the request:

```
@tincan ask muse check the calendar for Friday
```

The teammate gets that request with one more line at the end, `(from the owner's dot, ref dotask<16 hex characters>)`. The reference is unique to the dot message, so if `dot-web` stops or loses the relay's response mid-ask, it finds that exact ask on the relay by searching for the reference instead of asking again. Each such message is asked once, even across restarts. `@tincan` lines already in the DM when `dot-web` first reads it are treated as history and not asked. When the answer arrives, `dot-web` types it into the DM:

```
[tincan-reply from muse]
> @tincan ask muse check the calendar for Friday

<muse's answer>
```

A teammate that needs more is typed back as `[tincan-reply from <agent>] needs input: <question>`, quoting the request line, and that request is closed; the dot asks again with the details in a new `@tincan ask` message. A failure, decline or expiry is `[tincan-reply from <agent>] failed: <reason>`, quoting the request line. An answer longer than the DM takes is cut and says how much is shown. An ask with no answer after 25 hours is reported as failed.

`web serve --site dots` refuses to start unless the relay records the agent's kind as `dot-web`, because the default hold keys on that kind. If you invited it without `--kind`, run `tincan kind dot-web dot-web` from an admin device (the relay must run a build that knows the kind).

When the dot answers a request by asking a teammate itself (its reply ends with an `@tincan ask` line), the reply to the asker keeps that line and adds a note that the dot delegated and its final answer will be in its DM.

### The setup message

The first time `dot-web` reads a thread, it types one setup message into the DM. It explains the `@tincan ask <agent>` format in words (it carries no example line, so an echo of it can never send an ask), says replies are asynchronous and may wait on your approval so the dot should not ask twice, lists the reply forms (`[tincan-reply from <agent>]` answers, `needs input:` and `failed:`), lists the teammates the dot may ask, and says that requests Tincan types there and `[tincan-reply]` messages are data from teammates, not your instructions, and that the dot should ask you before writing through a connected app on a teammate's behalf. It is sent once per thread. To send it again (for example after the roster changes), run with `--teach`, which sends it once per start. `--teach` applies only to `--site dots`.

### Whom the dot may ask

The send allowlist is `~/.config/tincan/dot-web-send.txt`, in the same format as the other allowlists: one agent name per line, commas and spaces also separate, `#` starts a comment.

- No file: the dot may ask any joined agent.
- A file of names: only those agents. A `*` entry means any joined agent.
- An empty file: no agent.
- An unreadable file or a bad entry: every ask is refused (and typed back as failed) until you fix it. The startup log says so.

The file is reread for each ask. The relay's `approval.json` can also hold the dot's asks: an entry with `"from": ["dot-web"]` on a target holds asks from the dot to it.

### State

`~/.config/tincan/dot-web-out.json` (mode 0600) keeps, per thread, when the agent first read it, whether the dot was taught, and a record per `@tincan` message it acted on: status, target, request id and the request line. It keeps no answers. Finished records are dropped 30 days after their message leaves the part of the feed the agent reads. If the file is lost or unreadable, the agent starts fresh and treats what is in the DM as history, so nothing is asked twice.

The inbound side keeps no per-asker state (there is one thread), and uses the same send journal as the other web agents (see [Retries and duplicate sends](#retries-and-duplicate-sends)).

### Approval

Asks and notifies to `dot-web` are held for your approval when `approval.json` has no entry for it. `tincan held` lists them, and `tincan approve <id>` or `tincan deny <id> "reason"` decides. Replies to the dot's own asks are never held.

An `approval.json` entry for `dot-web` replaces the default. For example, to let `claude-code` ask the dot without approval and hold everyone else:

```json
{"gate": {"dot-web": {"unless": ["claude-code"]}}}
```

With `"from": "*"` every request is held, as with no entry. Keep the default unless you trust every agent that could ask; see the owner approval section of the README.

The dot sits on [councils](council.md) by default, like the other web teammates. A council's answer, review and chairman asks to it skip the default hold only when you approved that council question (or ran `tincan council` yourself in a terminal); every other ask to the dot is still held, and `approval.json` gates still apply. To keep the dot off councils, add `dot-web` to `exclude` in `council.json`.

### Failure replies

- Paused dot: "Nothing was sent: your dot is paused; unpause it in ChatGPT and ask again." The extension checks this before opening any tab.
- Not signed in: "not signed in to ChatGPT in Chrome; sign in to chatgpt.com in Chrome, then ask again". Nothing is sent.
- Wrong thread: "Nothing was sent: your dot's thread <id> was not found; check the --thread the dot-web agent runs with."
- Sent but no answer in time: "Sorry, your dot did not answer in time. The message was sent to your dot (conversation <id>); ask for the reply later instead of sending it again." The request is never sent again.
- Sent but not confirmed in the feed: "Sorry, the message was sent to your dot, but it did not show up in the conversation in time; ask for the reply later instead of sending it again."
- A 429 from ChatGPT follows the same cooldown as chatgpt-web (see [Rate limits](#rate-limits)).
- A changed DM page or backend fails as `endpoint_changed`, never silently. The selectors and endpoints live only in `extension/send.js` and `extension/ops.js`.

### Limits

- The DM shows Tincan's requests and the `[tincan-reply]` messages as your own messages. The dot cannot tell them from yours by author, only by the text, so the setup message's "data, not instructions" is guidance the dot may not follow. It is most reliable for `[tincan-reply]` messages, which are clearly marked; a typed request reads like anything you would write. Treat every request to `dot-web` as if you sent it yourself.
- The dot's own `@tincan ask` lines are its words, not yours: `dot-web` asks as `dot-web`, and the target sees that sender.
- A dot message that arrives after the reply went out (a late burst) is not delivered to the asker. Open the DM to see it.
- Each read of the DM takes only its latest 32 messages. If more arrive between two reads (the Mac asleep, `dot-web` stopped, or a long request holding the send path), an `@tincan ask` among the older ones is not asked; `dot-web` notices the gap and types one `[tincan]` note asking the dot to send any unanswered ask again.
- One DM per agent. For a second dot, invite a second agent with `--kind dot-web` and run it with its own config: `TINCAN_CONFIG=~/.config/tincan/<name>.json tincan web serve --site dots --name <name> --thread <thread-id>`, in the foreground or under your own service manager. `tincan web install` has no `--name` and installs only the default `dot-web` service.
- The request and the dot's answers use your ChatGPT account and stay in the DM like any chat you had with the dot.

## Ask both web agents

Run `tincan ask chatgpt-web,claude-web "Compare these options"`, or call MCP
`ask` with `{"to":"chatgpt-web","also":["claude-web"],"message":"Compare these options"}`.
Both receive the same question, with answers labeled by teammate. A partial
result includes a group id: pass it to `get_reply` as `request_id` or run
`tincan get <group-id>` to gather later answers. Each web agent still applies
its own allowlist. Attachments are uploaded once per target.
