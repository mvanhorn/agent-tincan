# Tincan on your phone

The relay serves a page for your phone at `/phone/`. It shows the team as `tincan top` does, with the same status for every agent, and lists the asks held for your approval with their sender, target and attachment names. You approve or deny a held ask from the page.

The page works only from an admin device: a machine named in the relay's `--admin` list, untagged, and (with `--admin-login`) owned by a listed login. Any other machine gets 403 for the page and every file it loads. Nothing is hosted anywhere else; the page talks only to your relay over your tailnet.

## Open it

1. Add the phone to the relay's admin list. Find its machine name with `tailscale status` (for example `iphone182`) and restart the relay with `--admin macbook-pro-44,iphone182`. The phone must be signed in to Tailscale as you and must not carry a tag.
2. On the phone, with Tailscale connected, open the page the relay logs at startup:
   - with HTTPS on your tailnet: `https://<relay>.<tailnet>.ts.net/phone/`
   - without it: `http://<relay>/phone/` (the relay's own address, as `tincan agents` reaches it)
3. Add it to the home screen. In Safari, tap Share, then Add to Home Screen. In Chrome on Android, tap the menu, then Install app.

Open the address exactly as the relay logs it, ending in `/phone/`. The relay refuses requests addressed to any name that is not its own.

## HTTPS

When your tailnet has HTTPS certificates turned on (Tailscale admin console, DNS page, HTTPS Certificates), the relay also listens on port 443 under its MagicDNS name and logs `phone page: https://.../phone/`. With the default tsnet relay this is its own node; with `--listen` it binds port 443 on the `--listen` address and uses the host's certificate, so the relay user needs permission to bind that port.

Without certificates the relay logs why (`no HTTPS: ...`) and the page works over plain HTTP on the relay's usual address. Plain HTTP inside the tailnet is still encrypted by WireGuard. The page itself is the same either way.

A `tailscale serve` front is not supported: the relay would see every request come from the host itself and could not tell which device is asking.

## What the page shows

- Held for approval, at the top, with a count. Each held ask shows who asked whom (with the target's kind, so a council or dot-web ask is plain), how long it has been held, its chain, the start of its text, and each attachment's name and size. Deny is on the left, Approve on the right.
- Team, below. Every agent with its status (busy, idle, stuck, offline, with the same detail `tincan agents` shows), kind, queue, good-at line and what it is working on, ordered as `tincan top` orders them: stuck and signed-out agents first.

The page refreshes every 5 seconds while it is open. When a refresh fails it says "Offline, last updated" with the time and turns off Approve and Deny until the relay answers again. When the device is not an admin it says so.

Status is always written out, never shown by color alone, and every change in the held list and every decision is announced to screen readers.

## Approving and denying

Each held ask in the page's list comes with two one-time codes from the relay, one for approve and one for deny. They work only from the device the list was sent to, only for that ask and that decision, and only for 10 minutes. The relay keeps only a hash of each, in memory, and never logs them. A code is used up by its first use, and any decision ends the hold for every code.

- A second tap, or a decision another device or the CLI already made, shows "Already decided" with the result instead of an error. Approving twice never releases twice.
- After the page has been in the background, it fetches fresh codes before Approve and Deny turn on again.

The relay audits each decision as `approved` or `denied` with the device's machine name. Approval stays with people: no agent tool lists, approves or denies held asks.
