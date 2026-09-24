# Agent Tincan History extension

Manifest V3 extension that lets the `history` agent read ChatGPT and claude.ai
conversations through the user's own logged-in Chrome. Its service worker runs
a fixed set of read operations (`ops.js`) for the native host
`com.agenttincan.history` (`tincan history native-host`) and returns JSON,
with images as base64 in chunks of at most 384 KiB. It accepts nothing else and
never runs code from a message or a page. The ChatGPT access token is read from
`/api/auth/session` inside the worker and never leaves it.

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
