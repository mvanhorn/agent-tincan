# Chrome Web Store upload

Steps for publishing the Agent Tincan History extension on the Chrome Web Store. Store review for these permissions (native messaging, scripting, and host access to chatgpt.com and claude.ai) usually takes several days, so launch uses the unpacked install ([docs/adapters/history.md](adapters/history.md#install)) in the meantime. Nothing in the docs changes to point at the store until the listing is live.

## 1. Build the store zip

From a checkout of the release tag:

```bash
make store
```

This writes `dist/tincan-history-extension-store.zip` and prints its sha256. It holds exactly the files Chrome loads (`manifest.json`, `background.js`, `ops.js`, `send.js`), with the manifest's `key` field removed: the Web Store rejects a manifest with a `key` because it assigns the id itself. `extension/manifest.json` and the release zip (`make extension`) keep the key, so the unpacked install keeps its fixed id `ciejooalclcpgpapboofdbbddphldhnh`. `TestStoreZip` in `internal/history` checks the zip.

The listing was submitted on September 24, 2026 as item `goldflchpojcjmifnljlfkgoahjgeajn` (publisher MVH). Each store upload needs a higher `version` in the manifest than the last one published.

## 2. Create the item

1. Open the Chrome Web Store developer dashboard: https://chrome.google.com/webstore/devconsole (sign in with the publisher Google account; the one-time developer registration fee applies if the account is new).
2. Click New item and upload `dist/tincan-history-extension-store.zip`.
3. Fill in the tabs below, then Submit for review. Choose to publish manually after approval if you want to control the launch moment.

## 3. Store listing

Name (from the manifest): `Agent Tincan History`

Short description (the manifest `description`, 129 characters; the store limit is 132):

> Lets your Agent Tincan history agent read, and your web agents send to, ChatGPT and claude.ai through your own logged-in browser.

Detailed description:

> Agent Tincan lets your AI agents ask each other to do things over your own private Tailscale network. This extension is the browser half of two Agent Tincan agents that run on your own computer:
>
> - The history agent answers your agents' questions about your past conversations, such as "what did I ask ChatGPT about the lease last week?", including images from those chats.
> - The ChatGPT and Claude web agents let your agents send a message to ChatGPT or Claude as you and get the answer back.
>
> The extension works through the session you are already logged in with. It runs only a fixed set of operations (list conversations, read a conversation, fetch its images, send a message in a background tab it opens itself, close that tab) and only when the Agent Tincan helper on your computer asks. It talks only to that helper, over Chrome native messaging. No data is sent to Agent Tincan or any other third party, there is no analytics or advertising, and no cookie or token leaves your browser.
>
> Only agents you have joined to your own relay can ask, and allowlists on your own computer can narrow that further. The extension does nothing on its own without the Agent Tincan helper installed (`tincan history install`).
>
> Privacy policy: https://agenttincan.com/privacy

Category: Developer Tools (Productivity also fits; pick one).

Language: English.

Screenshots: see section 7.

## 4. Privacy practices tab

Single purpose:

> Lets the user's own Agent Tincan agents, through a local helper program the user installs, read the user's ChatGPT and claude.ai conversations and send messages to ChatGPT and Claude, using the user's existing logged-in browser session.

Permission justifications (the dashboard asks for each one):

| Permission | Justification |
| --- | --- |
| `nativeMessaging` | The extension's only output channel. It receives operation requests from, and returns results to, the Agent Tincan helper installed on the user's computer (native host `com.agenttincan.history`). No data is sent anywhere else. |
| `alarms` | A once-a-minute alarm reconnects to the local helper if it is not running yet or restarted, so the service worker picks the connection back up without user action. |
| `scripting` | To send a message, the extension opens its own background tab on chatgpt.com or claude.ai and injects fixed functions (bundled in the package) that type the message into the message box and click send. It never scripts a tab the user opened. |
| `https://chatgpt.com/*` | Lists and reads the user's ChatGPT conversations and their files through ChatGPT's own endpoints with the user's session, and opens the background tab used to send a message. |
| `https://*.oaiusercontent.com/*` | ChatGPT stores generated and uploaded images behind signed download links on this domain. The extension downloads those images, without cookies or credentials, when the user's agent asks for a conversation's images. |
| `https://claude.ai/*` | Lists and reads the user's claude.ai conversations and their files through claude.ai's own endpoints with the user's session, and opens the background tab used to send a message. |

Remote code: No, I am not using remote code. (All code is in the package; messages are inserted as text, never executed.)

Data usage, what to tick:

- Personally identifiable information: tick. Conversations can contain names and other personal details.
- Personal communications: tick. The extension reads the user's chat conversations and sends messages.
- Website content: tick. It reads conversation text and images from chatgpt.com and claude.ai.
- Leave the rest unticked: health, financial and payment, authentication information (the ChatGPT session token is used only inside the extension for the current request and never collected or transmitted), location, web history, user activity.

Certify all three statements:

- I do not sell or transfer user data to third parties, outside of the approved use cases.
- I do not use or transfer user data for purposes that are unrelated to my item's single purpose.
- I do not use or transfer user data to determine creditworthiness or for lending purposes.

Privacy policy URL: `https://agenttincan.com/privacy`

## 5. Distribution

Visibility: Public (or Unlisted if you want to share the link before announcing). Regions: all.

## 6. Test instructions for the reviewer (optional field)

> The extension needs the Agent Tincan helper (`tincan history install`) and a logged-in chatgpt.com or claude.ai session to do anything. Without the helper it only retries a local native messaging connection once a minute. See https://agenttincan.com/privacy for exactly what it accesses.

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
