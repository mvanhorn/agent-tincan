# Tincan on your phone

The relay serves a page for your phone at `/phone/`. It shows the team as `tincan top` does, with the same status for every agent, and lists the asks held for your approval with their sender, target and attachment names. You approve or deny a held ask from the page, and once the phone joins as its own agent you can ask any teammate and read the answer there.

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

Without certificates the relay logs why (`no HTTPS: ...`) and the page works over plain HTTP on the relay's usual address. Plain HTTP inside the tailnet is still encrypted by WireGuard. The page is the same either way, except that alerts need HTTPS.

A `tailscale serve` front is not supported: the relay would see every request come from the host itself and could not tell which device is asking.

## Asking from the phone

The Ask tab, beside Team under the held list, lets you ask any teammate from the phone. The phone asks as its own agent, so every ask is attributed to a joined agent like any other.

1. On the Ask tab, tap Join this phone. The name defaults to `phone`. The page mints an invite with kind `phone` and joins with it at once, from the phone itself; there is no code to copy. It refuses a name that already belongs to another agent, since an invite for an existing name would move that agent to the phone.
2. Pick a teammate, write the ask and tap Send. Held asks (to council or dot-web, or under `approval.json`) show as held for your approval, and you approve them in the held list like any other.
3. Your asks shows the 20 most recent, rebuilt from the relay each time the page opens: waiting, working on it (with the latest progress note), held for your approval, needs your input (with a box to answer), answered (with the answer), could not do it, or declined.

Replies and questions are teammates' text and are shown as text only. The phone's agent has wake method `none` and the stock line "the owner's phone; asks only, never answers": it never answers, so teammates should not ask it. For the same reason a phone agent is never a target when work moves off a limited teammate, and never an @mention target in a room, though it may join a room to read and post.

Before approving a held ask that this phone's own agent sent, or that came through it, the page asks you to confirm, so one tap cannot both ask and approve.

If the phone's Tailscale node is replaced, the page says it is not joined; tap Join this phone again with the same name.

## Alerts

With HTTPS, the page can alert you when an ask is held. The row under the team says which state applies:

- Alerts need HTTPS: the page was opened over plain HTTP. Turn on HTTPS certificates for the tailnet; until then the approval policy's `notify` teammate is the alert path.
- Add to Home Screen first: on iPhone, Safari only allows alerts from a page opened from the Home Screen.
- Turn on alerts: tap it and allow notifications. The page asks only when you tap.
- Alerts are on: this phone gets an alert for every held ask. Turn off removes its subscription from the relay.
- Alerts are blocked: notifications for Tincan are off in the phone's settings.

An alert says only "A request is waiting for your approval" (or how many are). It never carries the request text, who asked or whom. Tapping it opens the page at the held list. The alert goes from the relay through your phone's push service (Apple, Google, Mozilla or Microsoft), encrypted to the phone; see [the trust model](trust-model.md). The page sends its subscription again every time it opens, so a phone that was offline or reinstalled picks alerts back up.

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
