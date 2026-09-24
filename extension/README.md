# Agent Tincan History extension

Manifest V3 extension that lets the `history` agent read ChatGPT and claude.ai
conversations, and the `chatgpt-web` and `claude-web` agents send to them,
through the user's own logged-in Chrome. Its service worker runs a fixed set of
operations (`ops.js`) for the native host `com.agenttincan.history`
(`tincan history native-host`) and returns JSON, with images as base64 in
chunks of at most 384 KiB. It accepts nothing else and never runs code from a
message or a page. The ChatGPT access token is read from `/api/auth/session`
inside the worker and never leaves it.

The send operations (`chatgpt.send`, `claudeai.send`, in `send.js`) open a
background tab of their own, fill the message box through fixed page functions
injected with `chrome.scripting` (message as an argument, isolated world),
click send, and return once the conversation id is in the tab's address. They
never watch the page for the answer: the Go side reads the conversation until
the answer is finished, then calls `chatgpt.close` or `claudeai.close`, which
close only the tab a send left open for that conversation (any such tab is
closed after 10 minutes regardless).
All page selectors are in the `SELECTORS` table in `send.js`; see
docs/adapters/web-agents.md.

On connect the worker sends the host a hello with its version and the sha256
of each file, hashed once when the worker started (so it describes the code
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
