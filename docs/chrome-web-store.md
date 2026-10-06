# Chrome Web Store upload

Steps for publishing the Agent Tincan History extension on the Chrome Web Store. Store review for these permissions (native messaging, scripting, host access to chatgpt.com and claude.ai, and optional host access to grok.com, assets.grok.com, gemini.google.com, lh3.googleusercontent.com, www.perplexity.ai, copilot.com and copilot.microsoft.com) usually takes several days, so launch uses the unpacked install ([docs/adapters/history.md](adapters/history.md#install)) in the meantime. Nothing in the docs changes to point at the store until the listing is live.

## 1. Build the store zip

From a checkout of the release tag:

```bash
make store
```

This writes `dist/tincan-history-extension-store.zip` and prints its sha256. It holds exactly the files Chrome loads (`manifest.json`, `background.js`, `ops.js`, `send.js`, `options.html`, `options.js` and the icons), with the manifest's `key` field removed: the Web Store rejects a manifest with a `key` because it assigns the id itself. `extension/manifest.json` and the release zip (`make extension`) keep the key, so the unpacked install keeps its fixed id `ciejooalclcpgpapboofdbbddphldhnh`. `TestStoreZip` in `internal/history` checks the zip.

The listing was submitted on September 24, 2026 as item `goldflchpojcjmifnljlfkgoahjgeajn` (publisher MVH). Each store upload needs a higher `version` in the manifest than the last one published. The v0.7.0 release uploads version 0.5.0, which adds `https://www.perplexity.ai/*`, `https://copilot.com/*` and `https://copilot.microsoft.com/*` as optional hosts (justifications below); like the other optional sites, they are granted from the options page, so the update asks users for nothing new.

## Updating through the API

After the first submission, `make release` uploads and publishes each new extension version through the Chrome Web Store API (v1.1): `GET items/<id>?projection=DRAFT` for the store's version, `PUT upload/chromewebstore/v1.1/items/<id>` with the store zip, then `POST items/<id>/publish`, each with the `x-goog-api-version: 2` header. A hidden release command does the calls:

```bash
go run ./cmd/tincan release-tools cws-upload dist/tincan-history-extension-store.zip --dry-run    # credentials and versions only
go run ./cmd/tincan release-tools cws-upload dist/tincan-history-extension-store.zip --publish
```

The extension has its own version (`extension/manifest.json`), separate from tincan's. When the zip's version is not higher than the store's, cws-upload uploads and publishes nothing, says so and exits 0, so a release that did not touch the extension goes on. Bump the manifest version to ship an extension change. When the store reports the published version and the draft already holds the zip's version unpublished (a publish that failed earlier), cws-upload publishes that draft instead, after waiting for its upload to finish; a draft whose upload failed is uploaded again. A failed published-version lookup (other than the API not supporting it) stops the command rather than skip a draft that may be unpublished. `cws-upload --publish-only` publishes the current draft without uploading, for when the store does not report the published version.

These are the only Google calls in tincan. The command is hidden and maintainer-only: no user-facing command reaches it, and it runs only when the maintainer runs it.

One-time setup:

1. In the Google Cloud console, in a project owned by the publisher account, enable the Chrome Web Store API and create an OAuth client of type Desktop app. Add the publisher account as a test user on the OAuth consent screen.
2. Write `~/.config/tincan-release/cws-oauth.json` (or point `TINCAN_CWS_CREDENTIALS` elsewhere) with `client_id`, `client_secret` and `item_id` (`goldflchpojcjmifnljlfkgoahjgeajn`), `chmod 600` the file and `chmod 700` its directory. The tools refuse a file that anyone but you can read or write, or a directory others can open. It lives outside the repo; never commit it.
3. Run `go run ./cmd/tincan release-tools cws-auth`. It prints and opens Google's consent page for the `chromewebstore` scope, catches the answer on `http://127.0.0.1:8765` (`--port` changes it), and saves the refresh token into the same file. Neither command prints the client secret, the refresh token or an access token.

While the OAuth consent screen is in testing mode, Google's refresh tokens can expire after 7 days. When cws-upload reports `invalid_grant`, rerun cws-auth. `make release` runs the `--dry-run` check before it pushes the tag, so an expired token stops the release before anything is public.

## 2. Create the item

1. Open the Chrome Web Store developer dashboard: https://chrome.google.com/webstore/devconsole (sign in with the publisher Google account; the one-time developer registration fee applies if the account is new).
2. Click New item and upload `dist/tincan-history-extension-store.zip`.
3. Fill in the tabs below, then Submit for review. Choose to publish manually after approval if you want to control the launch moment.

## 3. Store listing

Name (from the manifest): `Agent Tincan History`

Short description (the manifest `description`, 113 characters; the store limit is 132). The store rejected a version that listed every site by name as keyword spam, so name no sites here:

> Lets your Agent Tincan agents read and use the AI chat sites you are already signed in to, from your own browser.

Detailed description:

> Agent Tincan lets your AI agents ask each other to do things over your own private Tailscale network. This extension is the browser half of two Agent Tincan agents that run on your own computer:
>
> - The history agent answers your agents' questions about your past AI chats, such as "what did I ask about the lease last week?", including images from those chats.
> - The web agents let your agents send a message to an AI chat site as you and get the answer back. ChatGPT and Claude are built in; the other supported sites are optional, and the extension has no access to them until you grant them on its options page.
>
> These sites' terms (Google's for Gemini, Perplexity's for Perplexity, the Microsoft Services Agreement for Copilot) do not allow automated access. The web agents act as you, on your own account, at a human pace, one request at a time; whether to use them is your decision, and Google's enforcement can reach your whole Google account, not only Gemini (Microsoft's, your whole Microsoft account). Gemini answers can also draw on Google apps connected to your account (Gmail, Drive, Calendar), so limit who may ask the Gemini web agent with its allowlist.
>
> The extension works through the session you are already logged in with. It runs only a fixed set of operations (list conversations, read a conversation, fetch its images, send a message in a background tab it opens itself, close that tab) and only when the Agent Tincan helper on your computer asks. It talks only to that helper, over Chrome native messaging. No data is sent to Agent Tincan or any other third party, there is no analytics or advertising, and no cookie or token leaves your browser.
>
> Only agents you have joined to your own relay can ask, and allowlists on your own computer can narrow that further. The extension does nothing on its own without the Agent Tincan helper installed (`tincan history install`).
>
> Privacy policy: https://agenttincan.com/privacy

Category: Developer Tools (Productivity also fits; pick one).

Language: English.

Screenshots: see section 7.

Account terms: OpenAI, Anthropic, xAI, Google and Microsoft prohibit automated access to their apps in their terms. The extension acts only as the signed-in user, on the user's own account, one request at a time at a human pace, and only when the user's own agents ask; the listing and `docs/adapters/web-agents.md` say so, and that the site can still limit an account it believes is automated.

## 4. Privacy practices tab

Single purpose:

> Lets the user's own Agent Tincan agents, through a local helper program the user installs, read the user's past conversations on the AI chat sites whose history it supports (every supported site except Perplexity) and send messages to the supported sites (reading back the answer), using the user's existing logged-in browser session.

Permission justifications (the dashboard asks for each one):

| Permission | Justification |
| --- | --- |
| `nativeMessaging` | The extension's only output channel. It receives operation requests from, and returns results to, the Agent Tincan helper installed on the user's computer (native host `com.agenttincan.history`). No data is sent anywhere else. |
| `alarms` | A once-a-minute alarm reconnects to the local helper if it is not running yet or restarted, so the service worker picks the connection back up without user action. |
| `scripting` | To send a message, the extension opens its own background tab on chatgpt.com, claude.ai, grok.com, gemini.google.com, www.perplexity.ai or copilot.com and injects fixed functions (bundled in the package) that type the message into the message box and click send. For Gemini, one more fixed function fetches an image Gemini generated from inside that same tab, before it closes. For Copilot, whose chat list has no endpoint the extension can read without a token, one more fixed function reads the chat links in the sidebar of a copilot.com background tab the extension opens and then closes. It never scripts a tab the user opened. |
| `https://chatgpt.com/*` | Lists and reads the user's ChatGPT conversations and their files through ChatGPT's own endpoints with the user's session, and opens the background tab used to send a message. |
| `https://*.oaiusercontent.com/*` | ChatGPT stores generated and uploaded images behind signed download links on this domain. The extension downloads those images, without cookies or credentials, when the user's agent asks for a conversation's images. |
| `https://claude.ai/*` | Lists and reads the user's claude.ai conversations and their files through claude.ai's own endpoints with the user's session, and opens the background tab used to send a message. |
| `https://grok.com/*` (optional) | Only after the user grants it on the options page: lists and reads the user's Grok conversations through grok.com's own endpoints with the user's session, and opens the background tab used to send a message. |
| `https://assets.grok.com/*` (optional) | Granted together with grok.com: Grok serves the images it generates from this host. The extension downloads an image only when the user's agent asks for a conversation's images, looking its address up from the conversation itself. |
| `https://gemini.google.com/*` (optional) | Granted by the user from the options page. Lists and reads the user's Gemini conversations through the Gemini app's own endpoint with the user's session, and opens the background tab used to send a message. |
| `https://lh3.googleusercontent.com/*` (optional) | Granted together with Gemini. Gemini serves the images it generates from this domain; the extension fetches an image only when the user's agent asks for a Gemini answer's images, and only an image that conversation shows. |
| `https://lh3.google.com/*` (optional) | Granted together with Gemini. Gemini's image links redirect through this host on their way back to `lh3.googleusercontent.com`; the extension only follows that redirect for an image it is already fetching. |
| `https://www.perplexity.ai/*` (optional) | Granted by the user from the options page. Checks that the user is signed in, opens the background tab used to send a message, and reads back that one thread (the answer and its source links) through Perplexity's own endpoint with the user's session. It does not list or export the user's Perplexity history. |
| `https://copilot.com/*` (optional) | Granted by the user from the options page. Reads the user's Copilot conversations from copilot.com's own conversation page data with the user's session (no token is read), reads the chat list from the page in a background tab the extension opens, and opens the background tab used to send a message. |
| `https://copilot.microsoft.com/*` (optional) | Granted together with copilot.com: Copilot starts at copilot.microsoft.com and sends the browser on to copilot.com. |

Site access is also checked at run time. Before any operation (other than closing its own tab) the extension asks Chrome whether it holds the site's own page origins (for ChatGPT, `chatgpt.com`; the `*.oaiusercontent.com` file host is needed only by the image downloads that reach it), and refuses with `permission_missing` when it does not (for example when the user withheld the site in Chrome's site access settings). The extension's options page (`options.html`, reached from `chrome://extensions` > Agent Tincan History > Details > Extension options) lists each site, shows whether it is granted, and has a Grant button that calls `chrome.permissions.request` from the click, so Chrome shows its own prompt. Sites added in later versions go under `optional_host_permissions` and are granted there, so an update that adds a site does not disable the extension or prompt users who do not use that site. The optional sites are Grok (grok.com and assets.grok.com), Gemini (gemini.google.com and its image host lh3.googleusercontent.com), Perplexity (www.perplexity.ai) and Copilot (copilot.com and copilot.microsoft.com): installing or updating asks nothing for them, and nothing touches any of them until the user clicks its Grant button. Their page origins are `grok.com`, `gemini.google.com`, `www.perplexity.ai` and `copilot.com`; `assets.grok.com` and `lh3.googleusercontent.com` are needed only by the image downloads. The options page is a packaged extension page with no inline script and no remote code.

Remote code: No, I am not using remote code. (All code is in the package; messages are inserted as text, never executed.)

Data usage, what to tick:

- Personally identifiable information: tick. Conversations can contain names and other personal details.
- Personal communications: tick. The extension reads the user's chat conversations and sends messages.
- Website content: tick. It reads conversation text and images from chatgpt.com, claude.ai and (once granted) grok.com, gemini.google.com, www.perplexity.ai and copilot.com.
- Leave the rest unticked: health, financial and payment, authentication information (the ChatGPT session token is used only inside the extension for the current request and never collected or transmitted), location, web history, user activity.

Certify all three statements:

- I do not sell or transfer user data to third parties, outside of the approved use cases.
- I do not use or transfer user data for purposes that are unrelated to my item's single purpose.
- I do not use or transfer user data to determine creditworthiness or for lending purposes.

Privacy policy URL: `https://agenttincan.com/privacy`

## 5. Distribution

Visibility: Public (or Unlisted if you want to share the link before announcing). Regions: all.

## 6. Test instructions for the reviewer (optional field)

> The extension needs the Agent Tincan helper (`tincan history install`) and a logged-in chatgpt.com, claude.ai or (once granted) grok.com, gemini.google.com, www.perplexity.ai or copilot.com session to do anything. Without the helper it only retries a local native messaging connection once a minute. See https://agenttincan.com/privacy for exactly what it accesses.

## 7. Screenshots

The store wants 1280x800 (or 640x400) PNG or JPEG, one to five. Three are enough:

1. The extension card: `chrome://extensions` with Developer mode off, showing the Agent Tincan History card and its details page (permissions list). Crop to 1280x800.
2. A history answer with an image: ask from Grok or Claude Code, for example `tincan ask history "show me the last image ChatGPT made for me"`, and capture the answer with the image arriving in the Grok chat or the Claude Code terminal.
3. A web-agent answer: `tincan ask chatgpt-web "Tincan test: say hello in five words"` from an agent, showing the question and the returned answer.

How to take them on the Mac: size the window to 1280x800 (in Chrome, open DevTools device toolbar with a 1280x800 custom size, or resize the window), press Cmd+Shift+4, then Space, and click the window (hold Option while clicking to drop the shadow). Then resize to exactly 1280x800 if needed: `sips -z 800 1280 shot.png`. Use a test conversation, not real private chats, and check nothing personal is visible before uploading.

A small tile icon (128x128) is also required for the listing; the manifest has no icons today, so export the site's tincan mark as a 128x128 PNG and upload it in the listing tab (adding icons to the manifest can come with the next version).

## 8. After approval: switch to the store build

Nothing to reinstall. Since v0.5.2 the native host accepts both the unpacked id (`ciejooalclcpgpapboofdbbddphldhnh`) and the store id (`goldflchpojcjmifnljlfkgoahjgeajn`), and `tincan history serve` adds the store id to an older install's host manifest when it starts. When both builds are installed, the unpacked build's host waits while the store build's host serves, so the store build wins without anyone choosing.

1. Click Publish on the dashboard once the item passes review.
2. Users add it from the store listing. It connects at once.
3. They can remove the unpacked extension in `chrome://extensions` whenever they like.
4. Update the install docs (README, docs/adapters/history.md, site/agents.txt part C) to lead with the store listing.
