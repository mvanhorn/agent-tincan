# Agent Tincan History extension

Manifest V3 extension that lets the `history` agent read ChatGPT, claude.ai,
Grok, Gemini and Copilot conversations, and the `chatgpt-web`, `claude-web`,
`grok-web`, `gemini-web`, `perplexity-web` and `copilot-web` agents send to them (and to Perplexity), and the `dot-web` agent use the owner's OpenAI dot DM, through the user's own logged-in Chrome. Its service worker runs a fixed set of
operations (`ops.js`) for the native host `com.agenttincan.history`
(`tincan history native-host`) and returns JSON, with images as base64 in
chunks of at most 384 KiB. It accepts nothing else and never runs code from a
message or a page. The ChatGPT access token is read from `/api/auth/session`
inside the worker and never leaves it.

The send operations (`chatgpt.send`, `claudeai.send`, `grok.send`, `gemini.send`, `perplexity.send`, `copilot.send`, in `send.js`) open a
background tab of their own, fill the message box through fixed page functions
injected with `chrome.scripting` (message as an argument, isolated world),
click send, and return once the conversation id is in the tab's address. They
never watch the page for the answer: the Go side reads the conversation until
the answer is finished, then calls `chatgpt.close`, `claudeai.close`, `grok.close`, `gemini.close`, `perplexity.close` or `copilot.close`, which
close only the tab a send left open for that conversation (any such tab is
closed after 10 minutes regardless). A send tab the site moves to another
host is never typed into: Google's `/sorry/` page is `blocked`, any other host
(a sign-in page) `not_logged_in`.
All page selectors are in the `SELECTORS` table in `send.js`; see
docs/adapters/web-agents.md.

## Grok (grok.com)

The `grok.*` operations, all in the service worker with the owner's cookies
and no page-set headers:

- `grok.list {count}`: `GET /rest/app-chat/conversations?pageSize=N`, following
  `nextPageToken` (at most 5 pages) until `count` conversations are in; the
  result is `{conversations: [...]}`.
- `grok.detail {id}`: `GET .../conversations/<id>/response-node?includeThreads=true`
  (the message tree and `inflightResponses`), then `POST .../load-responses`
  with `{"responseIds": [...]}` for the message bodies; the result is
  `{conversationId, responseNodes, inflightResponses, responses}`.
- `grok.file {file_id, conversation_id}`: `file_id` is
  `<response id>_<index>`. The worker reads that response again with
  `load-responses`, takes `generatedImageUrls[index]`, and fetches it only if
  it resolves to `assets.grok.com` (or `grok.com`) over https; no URL ever
  comes from the request.
- `grok.send`: first `GET /rest/app-chat/conversations?pageSize=1` (a
  logged-out grok.com would chat anonymously, so no session means no tab),
  then the composer send in a background tab of `https://grok.com/` or
  `https://grok.com/c/<id>`. The tab fails `blocked` if it opens on an
  anti-bot challenge.
- `grok.close {conversation_id}`: closes only the tab a send left open.

Every grok.com read goes through one function, `grokJSON` in `ops.js`. If
grok.com starts refusing reads from the extension's origin, that function is
the one to replace with a fixed read in an extension-opened grok.com tab's
isolated world (as `send.js` injects its page functions); nothing runs in the
page's main world.

## Site access and the options page

`SITE_ACCESS` in `ops.js` lists each site's origins, keyed by its op prefix,
and among them its page origins (`pageOrigins`). Before any operation except
close, the worker asks `chrome.permissions.contains` for the site's page
origins and fails with `permission_missing` (no tab, no fetch) when they are
not granted. The other origins (ChatGPT's `*.oaiusercontent.com` file host)
are not checked up front: withholding one leaves list and read working, and
only a file fetch that reaches it fails, as any fetch to an ungranted host
does. Close runs
regardless, so a grant revoked while a reply is read still lets the tab close.
ChatGPT and claude.ai are required `host_permissions`, as before, so an
upgrade asks for nothing new; the owner can still withhold them in Chrome's
site access settings. Sites added later go under `optional_host_permissions`
(Grok: `https://grok.com/*` and `https://assets.grok.com/*`, granted together;
Gemini: `https://gemini.google.com/*` and `https://lh3.googleusercontent.com/*`,
its image host, granted together; Perplexity: `https://www.perplexity.ai/*`;
Copilot: `https://copilot.com/*` and `https://copilot.microsoft.com/*`, where
it starts, granted together) and are granted from the options page: `options.html` and `options.js`, opened from `chrome://extensions` >
Agent Tincan History > Details > Extension options. It lists every site in
`SITE_ACCESS`, shows whether all its origins are granted (a site with only
its page origins granted shows as granted with images and files needing
file access, and a "Grant file access" button), and its Grant button calls
`chrome.permissions.request` for all of the site's origins straight from the
click (Chrome allows the
request only during a user gesture). The page is built with `textContent`, has
no inline script, and never runs in a site's page.

The hello lists the granted sites (`granted`, op prefixes), and the worker
says hello again on `chrome.permissions.onAdded` and `onRemoved`, so the host
learns a grant without a reconnect. Only the newest hello started is posted,
so one built before a later change and finishing after it is dropped rather
than overwriting the newer grant list the host keeps. The host treats a hello without `granted`
(an older extension) as ChatGPT and claude.ai only. `tincan web serve` asks
the host (`host.status`, answered by the host itself) at startup. When the
extension is connected and reports its site ungranted, it logs that once
(naming the options page) and waits, asking again every minute, until the
grant appears or the extension goes away; it does not exit, so the service
manager never restarts it in a loop.

## Gemini

| Operation | Arguments | What it does |
| --- | --- | --- |
| `gemini.list` | `count` | Reads `MaZiqc` 13 conversations a page, passing each page's token, until it has `count` or a page carries no next-page token (at most 10 pages). An error row on any page is `endpoint_changed`, never a shorter list. Returns `{pages: [<inner payload>, ...]}`. |
| `gemini.detail` | `id` | Reads `hNvQHb` for `c_<id>` (latest 10 turns) and returns the inner payload. No payload is `not_found`. |
| `gemini.file` | `file_id` (`<response id>-<n>`), `conversation_id` | Reads the conversation again, takes image `n` of that response (only `https://lh3.googleusercontent.com/` URLs, found the way `geminiImageURLs` walks a response), then asks the sender to fetch it inside the tab the send left open (`pageFetchImage`, isolated world, the page's cookies) and falls back to a worker fetch with the image host's grant. Never draws an `<img>` onto a canvas. |
| `gemini.send` | `message`, `conversation_id?`, `new_chat?` | Fetches the app page fresh (logged out or `/sorry/` opens no tab), then types into the Quill composer (`div.ql-editor`) of `https://gemini.google.com/app` or `/app/<id>` in a background tab. Returns the hex id from the tab's address. |
| `gemini.close` | `conversation_id` | Closes the tab a Gemini send left open. |

Reads have no Gemini JSON API: the worker fetches `https://gemini.google.com/app`
for `SNlM0e` (the `at` token), `cfb2h` (`bl`) and `FdrFJe` (`f.sid`), keeps
them in memory for 10 minutes (never returned; a send or a 400/401 fetches
them again), and POSTs `f.req` and `at` to
`/_/BardChatUi/data/batchexecute?rpcids=<rpcid>&source-path=/app&bl=...&rt=c`.
`parseBatchexecute` takes the `wrb.fr` entry for the rpcid out of the
length-prefixed answer and decodes its inner JSON; anything else is
`endpoint_changed`. Payload positions are read in Go
(`internal/history/gemini.go`). Conversation ids cross the socket as the
URL's hex (`/app/<id>`); the `c_` prefix is added only here. If a live check
shows the worker's requests are refused, the fetch can move into an
extension-opened tab's isolated world: `geminiRPC` is the one place it
happens.

## Perplexity

Perplexity fronts the `perplexity-web` agent only: it has no list or file
operation and is not a history source.

| Operation | Arguments | What it does |
| --- | --- | --- |
| `perplexity.detail` | `id` (the thread slug) | Reads `GET /rest/thread/<slug>?with_parent_info=true&with_schematized_response=true&version=2.18&source=default&limit=10&offset=0&from_first=true`, then follows `next_cursor` (as `cursor`) while `has_next_page` is true, at most 20 pages. Returns `{slug, entries}` with only the fields in `PERPLEXITY_ENTRY_FIELDS` of each entry (never its `read_write_token`), and `more: true` when it stopped at the page cap. A 404 is `not_found`; an answer without an `entries` list is `endpoint_changed`. |
| `perplexity.send` | `message`, `conversation_id?`, `new_chat?` | Asks `GET /api/auth/session` first and opens no tab unless it names a signed-in `user` with an `id` (signed out it answers `{}`; the user's fields are only checked). Then types into `#ask-input` on `https://www.perplexity.ai/` or `/search/<slug>` in a background tab, checking the page for a sign-in link or address before typing. Before typing it closes a startup promo dialog (its "Maybe later" button only) and the cookie banner ("Decline optional" only), with `pageDismiss`; no other button is clicked. The text goes in with `insertText` and is sent with the visible `Submit` button, and the slug is returned from the tab's address. |
| `perplexity.close` | `conversation_id` | Closes the tab a Perplexity send left open. |

`perplexityJSON` is the one place Perplexity's JSON is fetched (from the
worker, with the owner's cookies); if Perplexity ever refuses the worker's
reads, a fixed read in an extension-opened tab's isolated world goes there.
The entry shape, the send button and the page's other selectors are to be
confirmed in a live round trip.

## Copilot (copilot.com)

| Operation | Arguments | What it does |
| --- | --- | --- |
| `copilot.list` | `count` | Opens `https://copilot.com/chat` in a background tab of its own, waits for a signed-in page, reads the sidebar's chat links (`#m365-copilot-chats-section a[href*="/chat/conversation/"]`: the UUID from the href, the title from `aria-label` or the text) with the fixed isolated-world function `pageCopilotList`, scrolls the last link into view for more until it has `count`, nothing more loads, or 10 rounds (then `more: true`), and closes the tab. A signed-in page with no chat list after 10 seconds is an empty list. Returns `{conversations: [{id, title}], more?}`. |
| `copilot.detail` | `id` (a lowercase UUID) | `GET https://copilot.com/chat/conversation/<id>` from the worker with `accept: application/json` and the owner's cookies (no token). Keeps only `store.rawConversationResponse`'s `conversationId`, `chatName`, `createTimeUtc`, `updateTimeUtc` and each user or bot message without a `messageType`: its `messageId`, `author`, `text`, `createdAt` and `sourceAttributions` as `{title: providerDisplayName, url: seeMoreUrl}` (http and https only, each URL once, at most 50). Everything else in the answer, including `reconnectToken` and the telemetry that carries token-like strings, is dropped here (`copilotConversation`). |
| `copilot.send` | `message`, `conversation_id?`, `new_chat?` | Opens `https://copilot.com/chat` or `/chat/conversation/<id>` in a background tab, waits for the composer and a signed-in mark, types with `insertText`, clicks Send (or presses Enter), and returns the UUID from the tab's address. |
| `copilot.close` | `conversation_id` | Closes the tab a Copilot send left open. |

copilot.com has no account check the worker may call without a token, so the
send and list tabs are the session gate: the page must stay on copilot.com and
show a signed-in mark (the sidebar's chat list or the account button) before
anything is typed or read. A tab that moves to a Microsoft sign-in or terms
page (login.live.com, login.microsoftonline.com, account.live.com), or to any
host whose address Chrome hides from the extension, is `not_logged_in`; one
on a Microsoft 365 host is `not_logged_in` with "work or school account".
Copilot's "Verification required" human-check dialog, or a Cloudflare
challenge frame, is `blocked` before typing and after the click; the widget
is never touched. Tab reads run one at a time with the site's sends, at most
75 seconds each and at least 3 seconds apart. `copilotJSON` is the one place
copilot.com's JSON is fetched, if it ever has to move into an
extension-opened tab's isolated world.

## OpenAI dots (chatgpt.com/dots)

The `dots.*` operations front the `dot-web` agent. They run under the
existing ChatGPT grant (`SITE_ALIASES` maps `dots` to `chatgpt`), so there is
no new site to grant and the hello does not list `dots` separately. Reads use
the same bearer `chatgptAuth` reads, which never leaves the worker.

| Operation | Arguments | What it does |
| --- | --- | --- |
| `dots.detail` | `id` (the dot's thread id) | `GET /backend-api/tbo/by-thread/<thread>` for the room and `is_paused`, `GET /backend-api/messaging/rooms/<room>` for its creator (the owner), and `GET .../rooms/<room>/messages?limit=32` for the latest messages (the API refuses more than 32). Returns `{thread, room, owner, paused, items: [{id, at, from, text, attachments}]}`, oldest first. An unexpected shape is `endpoint_changed`. |
| `dots.send` | `message`, `conversation_id` (the thread id; required, no new chat) | Refuses a paused dot with `paused` before any tab opens. Otherwise opens `https://chatgpt.com/dots/<thread>` in a background tab, types into the ProseMirror composer (`div[role="textbox"][aria-label="Message"]`) and clicks `button[aria-label="Send"]`. A composer that already holds text is left alone (`send_failed`). The address never changes, so the send is confirmed by polling the feed for a new owner message with the same text, and returns its id. |
| `dots.close` | `conversation_id` | Closes the tab a dots send left open. |

## Failure codes

Besides `not_logged_in`, `not_found`, `rate_limited` and the rest, the fetch
check reports anti-bot pages as `blocked`: a Cloudflare challenge (the
`cf-mitigated` header, or a "Just a moment..." page), a 403 JSON refusal that
names anti-bot rules or a captcha, and a redirect to Google's `/sorry/`
interstitial. A plain 401 stays `not_logged_in`. A session probe (ChatGPT's
`/api/auth/session`, claude.ai's organizations, Perplexity's
`/api/auth/session`) that was redirected to
another host is a sign-in page, so it is `not_logged_in` and no send follows.

On connect the worker sends the host a hello with its version, its granted
sites and the sha256 of each file, hashed once when the worker started (so it describes the code
Chrome loaded). When `tincan history install --extension-dir` (or a run from
the repo checkout) told the host where the unpacked files are, and they
differ, the host sends `extension.reload` and the worker calls
`chrome.runtime.reload()`, so updates need no Reload click after the first
load. The host compares again every 10 minutes while the worker stays
connected, so an update that lands mid-session is picked up too. The reload waits while a send has a tab open (checking every 5 seconds,
up to 5 minutes; at that cap it closes finished sends' tabs and reloads
anyway).

## Extension id

`manifest.json` carries a `key`: an RSA-2048 public key, base64 DER
SubjectPublicKeyInfo. Chrome derives the id from it (sha256 of the DER bytes,
first 16 bytes as hex, digits 0-f mapped to letters a-p), so an unpacked load
always gets `ciejooalclcpgpapboofdbbddphldhnh` (`history.DefaultExtensionID`,
checked by `TestExtensionIDFromManifestKey`). Only the public key is in the
repo; loading unpacked does not need the private key.

To rotate the key: `openssl genrsa -out key.pem 2048`, then
`openssl rsa -in key.pem -pubout -outform DER | base64` into `key`, update
`DefaultExtensionID`, and keep `key.pem` out of the repo. A Chrome Web Store
listing may assign its own id; pass it with
`tincan history install --extension-id <id>`.

## Develop

- `make extension-test` runs the worker tests (`node --test`, no dependencies).
- `make extension` writes `dist/tincan-history-extension.zip`.
- Load unpacked from `chrome://extensions`, then run `tincan history install`
  so Chrome can start the native host.

## Image input transport (gated)

Version 0.6.2 advertises `image_input: {version: 1, sites: []}` in the loaded
worker hello. The compiled per-site gate is off for every site in both Go and
the extension. No input site is enabled until live acceptance is recorded in
`docs/adapters/web-agents.md`. Output downloads retain their separate operations.
The fixed ChatGPT candidate operations registered in `OPS` are
`chatgpt.input_begin`, `chatgpt.input_chunk`, `chatgpt.input_abort` and
`chatgpt.send_images`. They accept a dedicated
`input` payload of metadata, opaque token, sequence and base64 bytes; no URLs,
paths, selectors, scripts or browser credentials. The host assigns connection
identity and rejects service-supplied identities. The worker also binds site and
connection, verifies hashes and consumes tokens once.

Begin reserves file metadata, chunk supplies ordered bytes, abort cancels the
transfer, and send_images consumes the token with the usual send arguments
(message capped at 32 KiB of UTF-8, optional validated conversation id and boolean).
Files must be nonempty PNG/JPEG. Go validates decoded pixels and MIME agreement,
rejects animated PNG and caps each image at 40 million pixels. Names are nonempty
UTF-8, at most 255 bytes, without control characters or path separators. The worker
checks complete byte counts and SHA-256 checksums before sending.

Chunks carry at most 96 KiB decoded. Limits are one to four files, 10 MiB/file, 20 MiB/ask,
four concurrent reservations and 40 MiB reserved decoded bytes, including queued
sends. Pending transfers expire after two idle minutes or ten total minutes.
Failure or disconnect aborts the connection's transfers; abort cannot delete
vendor uploads. Disconnect aborts unconsumed uploads, invalidates queued work and clears worker
unconsumed buffers on host reconnect. Cancelled active sends retain their
reservation until their cleanup finishes, preventing reconnects from bypassing
the memory bound. Reload waits for transfers only until the existing
five-minute deadline, then clears them. Bytes never persist in the worker.

Release packaging still uses `make extension` and `make store`. Extension changes
bump the manifest version; the store release target skips versions no newer than
the published version. This change requests no additional browser permissions.

## Session operations

These seven fixed operations accept no arguments and return only `{}` on positive authentication evidence. Typed errors report sign-out or an indeterminate outcome; indeterminate evidence cannot clear a known sign-out. No credentials or account fields are returned. Copilot probes use their own tab and do not queue sends behind the probe.

| Operation | Arguments | What it reads |
| --- | --- | --- |
| `chatgpt.session` | None (`{}`) | Reads `GET https://chatgpt.com/api/auth/session` and requires an access token, which stays in the worker. |
| `dots.session` | None (`{}`) | Uses the same ChatGPT session check; reads no dot thread or messages. |
| `claudeai.session` | None (`{}`) | Fetches `GET https://claude.ai/api/organizations` fresh and requires a valid organization UUID, preferring an organization with chat capability. |
| `grok.session` | None (`{}`) | Reads `GET https://grok.com/rest/app-chat/conversations?pageSize=1`; a nonempty conversations list is positive evidence, an empty successful list is `indeterminate`, and HTTP 401 is `not_logged_in`. |
| `gemini.session` | None (`{}`) | Fetches `https://gemini.google.com/app` fresh and checks its session token and build label; those values stay in worker memory. |
| `perplexity.session` | None (`{}`) | Reads `GET https://www.perplexity.ai/api/auth/session` and requires a signed-in `user` with an `id`; user fields are not returned. |
| `copilot.session` | None (`{}`) | Opens its own background tab at `https://copilot.com/chat`, checks the rendered page for a signed-in account using the send gate, then closes the tab without typing or sending. |
