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
5. With `START_RELAY=1`, starts `tincan relay` if no relay is running as this user, and checks that it stayed up. The relay always runs as its own tsnet node with its state in `~/.config/tincan-relay`, never with `--listen`, so it keeps its name and address when the box's own Tailscale is lost. `RELAY_ADMIN` sets its `--admin` list. `TS_AUTHKEY` is never passed to the relay. `RELAY_TS_AUTHKEY` is passed to it as its `TS_AUTHKEY` only while `~/.config/tincan-relay/tsnet` holds no identity yet, so the relay's first login uses its own tagged key. Read section 5 before using this.
6. Runs `tincan doctor`.

| Exit | Meaning | Fix |
|---|---|---|
| 0 | healthy | |
| 1 | something unhealthy, including a missing `tincan` | read the output, run `tincan doctor` |
| 2 | device waiting for approval | approve it in the admin under Machines; next time make the key with Pre-approved on |
| 3 | logged out and the auth key is missing, the wrong type, expired, already used or rejected | the owner generates a fresh one-off key as in step 1 and sets the secret, then removes it after the login |
| 4 | legacy layout: a `tincan relay` started with `--listen`, or a system `tailscaled` with its state in `/var/lib/tailscale`, is running; nothing was started | move the box to this layout (see [Moving an existing box to this layout](#moving-an-existing-box-to-this-layout)) |

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

Grok Bot's standing instructions (`tincan onboard --section agents` for a `vm-webhook` agent) run `~/.local/bin/tincan-up.sh` at the start of every turn, before `check_inbox`. A Grok Bot box has no init system that runs anything at boot, so the first turn after a rebuild, whatever starts it, is what brings Tailscale and the relay back. Set `START_RELAY=1` and `RELAY_ADMIN` as Grok Bot environment variables, not in a file: they live on Cursor's side and survive a rebuild.


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

## Moving an existing box to this layout

A box set up some other way (system Tailscale with its state in `/var/lib/tailscale`, a relay started with `--listen`, a hand-written keep-alive loop) loses its node and its relay on the next rebuild, and `tincan-up.sh` exits 4 while that setup is running. Move it once, with the owner present:

1. Install the release's `tincan-up.sh` into `~/.local/bin` and reload the standing instructions with `tincan onboard --section agents`. Set `START_RELAY=1` and `RELAY_ADMIN=<the owner's untagged laptop>` as Grok Bot environment variables.
2. The owner creates two one-off, pre-approved, non-ephemeral keys: one tagged `tag:grokbot`, set as `TS_AUTHKEY`, and one tagged with a relay-only tag such as `tag:tincan-relay` (owned by `autogroup:admin`), set as `RELAY_TS_AUTHKEY`. If the tailnet policy limits agents to the relay's IP, change those rules to the relay's tag now.
3. Stop the `--listen` relay, its keep-alive loops and the system `tailscaled`. The team is down from here until step 4 finishes.
4. Run `tincan-up.sh`. It brings up the box's node in `~/.config/tailscale` and starts the relay as its own tagged tsnet node with the existing state dir. Remove both keys from Grok Bot's secrets afterwards.
5. The box's node is new and tagged, so the relay never re-admits it on its own (see [the trust model](../trust-model.md#rebuilt-machines)). Re-link Grok Bot once: `tincan invite grokbot --socket ~/.config/tincan-relay/admin.sock`, then `tincan join <code> --replace --relay http://tincan-relay --proxy http://localhost:1055`. Other agents find the relay through its advertised addresses and the tailnet netmap.
6. When `tincan agents` lists grokbot and the other online agents, delete the old system-Tailscale devices in the Tailscale admin.
7. Drill: click "Update Grok Bot's Computer", then send Grok Bot one message. Its first turn must log `started tincan relay`, every agent must reconnect, an admin command such as `tincan held` must work from the owner's laptop, and no new Tailscale device may appear.

## Know when the relay is down

A relay that is down cannot send its own outage notice, so watch it from another always-on machine. `tincan relay-watch` probes the relay every `--every` (default 1m) with a joined client config's relay discovery, so a relay that moved still counts as up. After `--after` (default 10m) with no answer it runs `--alert-cmd` once, and once more when the relay answers again. The command gets `TINCAN_WATCH_MESSAGE`, `TINCAN_WATCH_EVENT` (`down` or `up`), `TINCAN_WATCH_RELAY` and `TINCAN_WATCH_SINCE`. [`examples/watch/imessage-alert.sh`](../../examples/watch/imessage-alert.sh) sends the message by iMessage to `TINCAN_ALERT_TO`. To run it as a service on a Mac that stays on:

```bash
tincan relay-watch install --config <a joined client config on that machine> \
  --alert-cmd 'TINCAN_ALERT_TO=you@example.com /path/to/imessage-alert.sh'
```

It writes a launchd agent (a systemd user unit on Linux) and prints the command that starts it. After a rebuild the alert is the cue to send Grok Bot a message, which runs `tincan-up.sh` and brings the relay back.

## Relay rediscovery and the custom socket

When the relay moves, the client first tries the relay's advertised addresses, then every IPv4 address on the local Tailscale netmap (`internal/client/discover.go`). It reads the netmap from tailscaled's LocalAPI, and falls back to `tailscale status --json` only if LocalAPI is unreachable. With a userspace tailscaled on a custom socket, tincan finds the socket from the running tailscaled's `--socket` flag, or under the home folder (`~/.tailscale*/`, `~/.cache/tailscale/`, `~/.config/tailscale/`, `~/.local/share/tailscale/`, `~/.local/state/tailscale/`); when more than one tailscaled runs, it lists them all. If `tincan doctor` still warns under `relay moves` that it cannot list the tailnet, set `TS_SOCKET` in the environment of every tincan process, including whatever starts `tincan mcp`:

```sh
export TS_SOCKET="$HOME/.cache/tailscale/tailscaled.sock"
```

The `tailscale` wrapper the script installs in `~/.local/bin` still adds `--socket` for your own `tailscale` commands, and it also serves the CLI fallback when `~/.local/bin` is on `PATH`.

## Wake

Grok Bot wakes on its webhook. In the relay's `wake.json`:

```json
{ "grokbot": { "method": "webhook", "url": "<Grok Bot webhook URL>", "bearer_token": "<webhook key>" } }
```

The relay posts `{"source":"agent-tincan","message":"Agent Tincan: 2 requests from your teammates waiting. Run check_inbox ..."}`. Grok Bot then calls `check_inbox`. The same wake can also mean a reply to one of Grok Bot's own requests is waiting (the message then counts replies). `check_inbox` shows it, and Grok Bot finishes the work that was waiting on it.

If `tincan agents` shows grokbot with `unanswered="woken 12m ago, no check-in (webhook ok)"`, the relay's webhook reached the Grok Bot app but no session ran, so Grok Bot never polled. The relay wakes it again on `--wake-grace` until grokbot polls, the queue is empty, or `max_per_hour` is spent; a later 2xx does not move the unanswered time. Without a fallback (below) that is the same webhook each time. The requests stay queued until a session runs. A failed send shows the error in place of `ok`; for a 401 or 403, check `bearer_token` in `wake.json`.

After `--owner-notice-after` (default 3) unanswered wakes in a row, the relay also tells the owner once per silent episode, through the approval policy's `notify` destination (`<agent> has not checked in after <N> wakes in a row since <time> ...`). Name an agent there that the owner reads, not Grok Bot itself. `tincan wakes grokbot --since 2h` on an admin device lists each wake with its HTTP status, the webhook's reply when the relay recorded one, and the first poll after it.

## When the webhook says OK but nothing runs

The webhook only enqueues a run of the "Tincan wake" routine. Grok Bot answers 2xx as soon as the run is queued, before it starts. If the run itself then fails inside Grok Bot, every wake is `webhook ok` and the agent never starts. This has happened: routine runs failed with "Activity task failed" and "The background task was interrupted before it finished", after notices that "A background task was stopped after 50 minutes so it would not hang". Posting the same webhook again cannot fix that.

To check:

1. Open the "Tincan wake" routine in the Grok Bot app and look at the status of its recent runs. Failed runs show as "Activity task failed".
2. Look at the relay log or the `woke` audit event for the webhook's answer, for example `wake grokbot: ok, webhook, HTTP 200, 1 waiting, response: {"status":"queued"}`. The relay keeps the first 200 bytes of a 2xx response on one line, with the URL and keys redacted and long opaque tokens shown as `[token]`, so an error the platform still answered 2xx to shows there. A reply that still has escape sequences after redaction, or any form of a key, is shown as `[withheld: may contain a secret]`.
3. In Grok Bot's settings, "Update Grok Bot's Computer" has unstuck the routine before. Run a test ask afterwards and check that grokbot polls.

"Activity task failed" with no other detail is also what the app shows when the Cursor account has hit its on-demand spend limit: the webhook still returns 200, the routine never runs. Check the account's spending page before treating it as a broken routine.

Give grokbot a second path so a dead routine does not strand its requests. A `fallback` list in `wake.json` holds further webhook or email targets, each with the same fields as the primary. The first wake goes to the primary. Each follow-up after a silent `--wake-grace` goes to the next path, and after the last it cycles back to the primary. A poll starts the next episode on the primary again. All paths share `max_per_hour`. For example, with an AgentMail inbox whose listener can wake the bot:

```json
{
  "grokbot": {
    "method": "webhook", "url": "<Grok Bot webhook URL>", "bearer_token": "<webhook key>",
    "max_per_hour": 12,
    "fallback": [
      { "method": "email", "email_to": "<inbox whose listener wakes Grok Bot>", "agentmail_inbox": "<sending inbox>", "agentmail_key": "<AgentMail API key>" }
    ]
  }
}
```

A fallback cannot set `max_per_hour` or its own `fallback`, and a mistake is reported with the agent and the fallback's position, such as `wake grokbot: fallback 1: email needs email_to, agentmail_inbox, agentmail_key`. The roster and the asker's note name the path that sent the last wake, for example `woken 12m ago, no check-in (webhook ok (fallback 1: email))`. The URL, address and keys never reach agents.

If this inbox is also the sending inbox for an agent with `include_requests` (such as Instinct), its replies and the relay's "recorded" emails land here too. Tell Grok Bot to ignore mail whose subject contains `[tincan `; the relay handles it. A dedicated sending inbox for that agent avoids the overlap.
