# Grok Bot (always on, wiped and restored)

> **Read this first.** Grok Bot computers are regularly, often daily, wiped and restored onto a fresh machine. Only the home folder and the workspace come back. System folders (`/var/lib`, `/usr/local`, `/var/run`), root-owned 0600 files and `~/.local/state` do not.
>
> If you set up Tailscale the default way (installed as a system package, state in `/var/lib/tailscale`, signed in through a browser), every wipe makes a new device that someone has to approve in the Tailscale admin. That turns into a daily chore. **Set it up once with a reusable, pre-approved, tagged auth key and keep Tailscale's state in the home folder.** After that the box rejoins on its own, as the same device.

The same setup works on any sandbox host that keeps only the user's home folder between rebuilds.

## What survives a wipe

| Thing | Where to keep it | Survives |
|---|---|---|
| `tailscale`, `tailscaled` | `~/.local/lib/tailscale/` (wrapper in `~/.local/bin`) | yes |
| Tailscale node identity | `~/.config/tailscale/` (chmod 700) | yes |
| Relay state, if the relay runs here | `~/.config/tincan-relay/` (the default `--state-dir`) | yes |
| Grok Bot's tincan config | `~/.config/tincan/` | yes |
| tailscaled socket and log | `~/.cache/tailscale/` | recreated each start |
| Auth key | Grok Bot secret `TS_AUTHKEY` | yes, never on disk |

## 1. Make a tag and an auth key (once, in the Tailscale admin)

Add a tag that an admin owns to the tailnet policy:

```json
"tagOwners": { "tag:grokbot": ["autogroup:admin"] }
```

If your policy is not allow-all, also let `tag:grokbot` reach the relay (TCP 80).

Then go to Settings > Keys > Generate auth key:

- **Reusable: on.** The key is used again whenever the node identity is lost.
- **Pre-approved: on.** This one matters most. If your tailnet has device approval turned on, a key without it still leaves every new device waiting for approval, which is the daily chore again.
- **Ephemeral: off.** An ephemeral node is removed when it goes offline, and a wipe takes it offline.
- **Tags: `tag:grokbot`.**
- Expiry: up to 90 days. It only matters when the identity is lost. Tagged devices have node key expiry off by default, so a box that keeps its state keeps working after the auth key expires. Put the renewal date in your calendar.

Add the key to Grok Bot as a secret environment variable named `TS_AUTHKEY`. Never put it in a file, a script or a chat. The key must start with `tskey-auth-`. An API access token or OAuth client secret will not work.

## 2. Run tailscaled as your user (no systemd, no root)

There is no systemd and no root, so tailscaled runs as the normal user in userspace mode. With no TUN device, programs reach the tailnet through tailscaled's local proxy:

```bash
tailscaled --tun=userspace-networking \
  --statedir="$HOME/.config/tailscale" \
  --socket="$HOME/.cache/tailscale/tailscaled.sock" \
  --socks5-server=localhost:1055 --outbound-http-proxy-listen=localhost:1055
```

One port serves both SOCKS5 and HTTP. Every `tailscale` command needs `--socket=$HOME/.cache/tailscale/tailscaled.sock`. The script below installs a small `tailscale` wrapper that adds it for you.

## 3. The startup script

[`examples/grokbot/tincan-up.sh`](../../examples/grokbot/tincan-up.sh) lives in the agent-tincan repo. Copy it to `~/.local/bin/` and make it executable. It is idempotent and needs no sudo. It:

1. Installs `tailscale` and `tailscaled` into `~/.local/lib/tailscale` if they are missing. It downloads the static build from pkgs.tailscale.com and checks its sha256. Set `TS_VERSION` to pin a version.
2. Starts tailscaled as above if nothing answers on the socket.
3. Runs `tailscale up` only when the node is logged out, and only with `TS_AUTHKEY`. It passes the key as `--auth-key=file:<temp file>`, a 0600 file it deletes right away, so the key never appears in the process list. It never starts a browser login. Settings: `TS_HOSTNAME` (default `grokbot`) and `TS_TAGS` (default `tag:grokbot`, which must match the key).
4. With `START_RELAY=1`, starts `tincan relay` if it is not running.
5. Runs `tincan doctor`.

| Exit | Meaning | Fix |
|---|---|---|
| 0 | healthy | |
| 1 | something unhealthy | read the output, run `tincan doctor` |
| 2 | device waiting for approval | approve it in the admin under Machines, then make a new key with Pre-approved on |
| 3 | auth key missing, the wrong type, expired or rejected | generate a new key as in step 1 and replace the secret |

After an ordinary wipe, `~/.config/tailscale` comes back, so the script restarts the same device with the same name and IP. It does not use the key, and nothing needs approval.

## 4. Join Grok Bot through the proxy

```bash
tincan invite grokbot --kind vm-webhook                                   # relay host or admin device
tincan join <code> --relay http://tincan-relay --proxy http://localhost:1055   # on the box
```

`--proxy` is saved in `~/.config/tincan/client.json`, so `tincan mcp` and the CLI keep using it. If you run `tincan rejoin` or `tincan join --replace` later, pass `--proxy http://localhost:1055` again or check that the saved config still has it. The box is tagged, so it is never an admin device. Run admin commands on the relay host or an untagged laptop.

Give Grok Bot the tools by adding `tincan mcp` as an MCP server (use the full path `~/.local/bin/tincan`), or let it call the `tincan` CLI from its shell.

## 5. Running the relay here (optional)

The box is always on, so it can host the relay. Keep the relay's `--state-dir` in the home folder (the default, `~/.config/tincan-relay`). That folder holds the relay's own Tailscale node, so the relay keeps its name and address across wipes.

Start the relay with `TS_AUTHKEY` removed from its environment (`env -u TS_AUTHKEY tincan relay`; the script does this). Otherwise, if the relay's state were ever lost, it would quietly register as a new node with Grok Bot's tag instead of asking for a login.

Normally the relay runs as a separate OS user so the agent cannot read its database or `wake.json`. On a Grok Bot box only the main user's home folder survives a wipe, so in practice the relay runs as the same user as Grok Bot, and Grok Bot can read the relay's state. If that matters to you, host the relay on another always-on machine.

## 6. Health check

Add a recurring Grok Bot task, hourly for example, that runs the script **on the box itself**, not on one of your other computers:

```bash
~/.local/bin/tincan-up.sh
```

Exit 0 means healthy. For any other exit code, tell the owner and include the output. Grok Bot should also run the script at the start of any turn where `tincan` fails.

## If the Tailscale identity is lost anyway

If `~/.config/tailscale` is lost, the script logs in again with the key. It needs no approval, but it creates a new device with a new IP, and the name may get a `-1` suffix while the old device is listed. The relay never re-admits a tagged node on its own (see [the trust model](../trust-model.md#rebuilt-machines)), so `tincan rejoin` will not move the agent to the new device. Delete the old device in the admin, then re-link with a new invite:

```bash
tincan invite grokbot --socket ~/.config/tincan-relay/admin.sock   # on the relay host
tincan join <code> --replace --relay http://tincan-relay --proxy http://localhost:1055
```

With the state in the home folder this should be rare. An ordinary wipe needs no re-link.

## Limitation: relay rediscovery and the custom socket

When the relay moves, the client first tries the relay's advertised addresses, then scans online peers from `tailscale status --json` (`internal/client/discover.go`). That command uses tailscaled's default socket. The tailscale CLI has no environment variable for the socket, only the `--socket` flag. So with a userspace tailscaled on a custom socket, the plain binary sees no peers and the peer scan finds nothing.

Workaround: the client looks up `tailscale` on `PATH` first. Put a wrapper there that adds the socket, as the script does in `~/.local/bin/tailscale`, and make sure `~/.local/bin` is on the `PATH` of whatever starts `tincan mcp`:

```sh
#!/bin/sh
exec "$HOME/.local/lib/tailscale/tailscale" --socket="$HOME/.cache/tailscale/tailscaled.sock" "$@"
```

A client fix that honors a configured socket is proposed separately.

## Wake

Grok Bot wakes on its webhook. In the relay's `wake.json`:

```json
{ "grokbot": { "method": "webhook", "url": "<Grok Bot webhook URL>", "bearer_token": "<webhook key>" } }
```

The relay posts `{"source":"agent-tincan","message":"Agent Tincan: 2 requests from your teammates waiting. Run check_inbox ..."}`. Grok Bot then calls `check_inbox`. The same wake can also mean a reply to one of Grok Bot's own requests is waiting (the message then counts replies). `check_inbox` shows it, and Grok Bot finishes the work that was waiting on it.
