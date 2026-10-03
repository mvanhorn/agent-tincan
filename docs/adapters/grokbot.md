# Grok Bot (always on, wiped and restored)

> **Read this first.** Grok Bot computers are regularly, often daily, wiped and restored onto a fresh machine. Only the home folder and the workspace come back. System folders (`/var/lib`, `/usr/local`, `/var/run`), root-owned 0600 files and `~/.local/state` do not.
>
> If you set up Tailscale the default way (installed as a system package, state in `/var/lib/tailscale`, signed in through a browser), every wipe makes a new device that someone has to approve in the Tailscale admin. That turns into a daily chore. **Set it up once with a one-off, pre-approved, tagged auth key and keep Tailscale's state in the home folder.** After that the box rejoins on its own, as the same device, and the key is no longer needed.

The same setup works on any sandbox host that keeps only the user's home folder between rebuilds.

## What survives a wipe

| Thing | Where to keep it | Survives |
|---|---|---|
| `tailscale`, `tailscaled` | `~/.local/lib/tailscale/` (wrapper in `~/.local/bin`) | yes |
| Tailscale node identity | `~/.config/tailscale/` (chmod 700) | yes |
| Relay state, if the relay runs here | `~/.config/tincan-relay/` (the default `--state-dir`) | yes |
| Grok Bot's tincan config | `~/.config/tincan/` | yes |
| tailscaled socket and log | `~/.cache/tailscale/` | recreated each start |
| Auth key | Grok Bot secret `TS_AUTHKEY`, only until the first login | not needed after the first login; remove it |

## 1. Make a tag and an auth key (once, in the Tailscale admin)

Add a tag that an admin owns to the tailnet policy:

```json
"tagOwners": { "tag:grokbot": ["autogroup:admin"] }
```

Limit what the tag can reach. Grok Bot only needs the relay, so allow `tag:grokbot` to reach the relay on tcp:80 (or the port your relay listens on) and nothing else. For example, with the relay's tag or tailnet IP in place of `<relay>`:

```json
"acls": [{ "action": "accept", "src": ["tag:grokbot"], "dst": ["<relay>:80"] }]
```

Then go to Settings > Keys > Generate auth key:

- **Reusable: off.** A one-off key enrolls this box once. Grok Bot's shell can read its own environment, so a key left there could be leaked, and a reusable one would let whoever holds it add more tagged devices with no approval.
- **Pre-approved: on.** This one matters most. If your tailnet has device approval turned on, a key without it still leaves every new device waiting for approval, which is the daily chore again.
- **Ephemeral: off.** An ephemeral node is removed when it goes offline, and a wipe takes it offline.
- **Tags: `tag:grokbot`.**
- Expiry: short, a day is enough. The key is only used for the first login. Tagged devices have node key expiry off by default, so a box that keeps its state keeps working after the auth key expires.

Add the key to Grok Bot as a secret environment variable named `TS_AUTHKEY`. Never put it in a file, a script or a chat. The key must start with `tskey-auth-`. An API access token or OAuth client secret will not work.

After the first successful login, remove `TS_AUTHKEY` from Grok Bot's secrets. The identity now lives in `~/.config/tailscale` and survives wipes, so the key is not needed again. If the identity is ever lost, the script exits 3 and the owner issues a fresh one-off key.

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

1. Installs `tailscale` and `tailscaled` into `~/.local/lib/tailscale` if they are missing. It downloads the static build from pkgs.tailscale.com and checks its sha256. Set `TS_VERSION` to pin a version. If the installed version differs from `TS_VERSION`, it installs the pinned one and restarts tailscaled.
2. Starts tailscaled as above if nothing answers on the socket.
3. Brings the node up. A node that still has its identity but is stopped comes up without a key. A logged-out node logs in only with `TS_AUTHKEY`, passed to `tailscale up` on stdin (`--auth-key=file:/dev/stdin`), so the key is never written to disk or shown in the process list. It never starts a browser login. It then waits for the node to be running. Settings: `TS_HOSTNAME` (default `grokbot`) and `TS_TAGS` (default `tag:grokbot`, which must match the key).
4. Checks that `tincan` is installed at `~/.local/bin/tincan` (or `TINCAN`). A missing or non-executable `tincan` is unhealthy.
5. With `START_RELAY=1`, starts `tincan relay` if no relay is running as this user, and checks that it stayed up. `RELAY_ADMIN` sets its `--admin` list. Read section 5 before using this.
6. Runs `tincan doctor`.

| Exit | Meaning | Fix |
|---|---|---|
| 0 | healthy | |
| 1 | something unhealthy, including a missing `tincan` | read the output, run `tincan doctor` |
| 2 | device waiting for approval | approve it in the admin under Machines; next time make the key with Pre-approved on |
| 3 | logged out and the auth key is missing, the wrong type, expired, already used or rejected | the owner generates a fresh one-off key as in step 1 and sets the secret, then removes it after the login |

After an ordinary wipe, `~/.config/tailscale` comes back, so the script restarts the same device with the same name and IP. It does not use the key, and nothing needs approval.

## 4. Join Grok Bot through the proxy

```bash
tincan invite grokbot --kind vm-webhook                                   # relay host or admin device
tincan join <code> --relay http://tincan-relay --proxy http://localhost:1055   # on the box
```

`--proxy` is saved in `~/.config/tincan/client.json`, so `tincan mcp` and the CLI keep using it. If you run `tincan rejoin` or `tincan join --replace` later, pass `--proxy http://localhost:1055` again or check that the saved config still has it. The box is tagged, so it is never an admin device. Run admin commands on the relay host or an untagged laptop.

Give Grok Bot the tools by adding `tincan mcp` as an MCP server (use the full path `~/.local/bin/tincan`), or let it call the `tincan` CLI from its shell.

## 5. Where the relay runs

Run the relay on a separate always-on host, or at least as a separate OS user, as the [README](../../README.md) says. That is the default for Grok Bot too:

```bash
tincan relay --admin <your-laptop>   # on the relay host, as the relay's own user
```

Do not run the relay as Grok Bot's user. The relay's local admin socket (`admin.sock` in its state dir) is protected only by file mode 0600, so any process running as the relay's user can use it with the owner's authority: invite and remove agents, and approve held requests. That user can also read the relay's database and the webhook and email secrets in `wake.json`. If the relay runs as Grok Bot's user, Grok Bot is effectively the owner.

On a Grok Bot box only the main user's home folder survives a wipe, so a separate relay user there would lose its state on every wipe. Host the relay on another always-on machine instead.

If you run a relay here anyway (`START_RELAY=1`), know that Grok Bot has owner authority over it. Keep its `--state-dir` in the home folder (the default, `~/.config/tincan-relay`); that folder holds the relay's own Tailscale node, so the relay keeps its name and address across wipes. The script starts it with `TS_AUTHKEY` removed from its environment, so if the relay's state were ever lost, it would not quietly register as a new node with Grok Bot's tag.

## 6. Health check

Add a recurring Grok Bot task, hourly for example, that runs the script **on the box itself**, not on one of your other computers:

```bash
~/.local/bin/tincan-up.sh
```

Exit 0 means healthy. For any other exit code, tell the owner and include the output. Grok Bot should also run the script at the start of any turn where `tincan` fails.

## If the Tailscale identity is lost anyway

If `~/.config/tailscale` is lost, the script exits 3 because the key was removed after the first login. The owner issues a fresh one-off key and sets `TS_AUTHKEY`, and the script logs in again with it. It needs no approval, but it creates a new device with a new IP, and the name may get a `-1` suffix while the old device is listed. The relay never re-admits a tagged node on its own (see [the trust model](../trust-model.md#rebuilt-machines)), so `tincan rejoin` will not move the agent to the new device. Delete the old device in the admin, then re-link with a new invite:

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

A userspace tailscaled on a custom socket must be reachable by tincan, whichever way you do it. If your tincan build reads a `TS_SOCKET` environment variable for its Tailscale calls, setting `TS_SOCKET` for tincan processes is the supported way. The wrapper keeps working either way.

## Wake

Grok Bot wakes on its webhook. In the relay's `wake.json`:

```json
{ "grokbot": { "method": "webhook", "url": "<Grok Bot webhook URL>", "bearer_token": "<webhook key>" } }
```

The relay posts `{"source":"agent-tincan","message":"Agent Tincan: 2 requests from your teammates waiting. Run check_inbox ..."}`. Grok Bot then calls `check_inbox`. The same wake can also mean a reply to one of Grok Bot's own requests is waiting (the message then counts replies). `check_inbox` shows it, and Grok Bot finishes the work that was waiting on it.
