# Grok Bot (always on, wiped and restored)

Grok Bot's computer is regularly, often daily, wiped and restored onto a fresh machine. Only the home folder and the workspace come back. System folders (`/var/lib`, `/usr/local`, `/var/run`), root-owned 0600 files and `~/.local/state` do not.

Use Grok Bot's managed Tailscale connector to put Grok Bot on your tailnet. Grok Bot runs that connection on Cursor's side, so a wipe does not touch it. Grok Bot's own guidance is clear: do not install a Tailscale client on the bot's computer, because it is wiped when that computer is replaced. Sections 1 to 6 below follow that path.

An older version of this page told you to install a userspace Tailscale client on the bot's computer with a tagged auth key. That method still works on sandbox hosts that allow their own Tailscale client and keep only the home folder. It is not what Grok Bot recommends, so it now lives in [Fallback: a self-installed userspace Tailscale](#fallback-a-self-installed-userspace-tailscale).

What was not live-tested: this page follows Grok Bot's published Tailscale plugin docs (`cursor/plugins`, `third_party/tailscale`). The `tincan-up.sh` managed mode is covered by tests with fake binaries. Joining, `tincan mcp`, a relay wake and a posture grant through the managed connector have not been run end to end on a real Grok Bot.

## What survives a wipe

| Thing | Where it lives | Survives |
|---|---|---|
| Grok Bot's tailnet device | the managed connector, on Cursor's side | yes, it is not on the computer |
| `tincan` and `tincan-up.sh` | `~/.local/bin/` | yes |
| Grok Bot's tincan config | `~/.config/tincan/` | yes |
| Relay state, if the relay runs here | `~/.config/tincan-relay/` (the default `--state-dir`) | yes |
| `TS_MODE`, `START_RELAY`, `RELAY_ADMIN` | Grok Bot environment variables | yes, they live on Cursor's side |

## 1. Connect the managed Tailscale connector

In the Grok Bot desktop app, open Connect apps > Your plugins > Tailscale. It has one slot per tailnet. The Tailscale plugin's `tailscale-setup` skill walks through both paths; the short version:

- Personal tailnet. You must own or administer the tailnet. Click Authenticate on its slot and accept Tailscale's consent screen. The device works right away. It joins untagged, with your own access.
- Company tailnet (Tailscale Premium or Enterprise). A team admin enables it once at cursor.com, Integrations > Tailscale, and checks "Members' Grok Bot devices may join this tailnet". You then click Authenticate on that tailnet's slot. If the tailnet requires device approval, an admin approves the `grokbot-` device under Machines. The device carries the `custom:grokbot` attribute.

The device shows up in the Tailscale admin with a name like `grokbot-user-...`. Commands on Grok Bot's computer then reach tailnet hosts by their full `name.tailnet.ts.net` name or a `100.x` address, over HTTP(S) or TCP, through that device. There is nothing to install and no proxy to set.

Two limits from Grok Bot's docs matter for tincan:

- The connector only works on your own turns. On a personal tailnet that means your messages, linked teammates, and routines you schedule. On a company tailnet it means your own Grok Bot chats, Slack DMs, and routines you schedule. Slack Connect channels never use a tailnet. The relay wakes Grok Bot through a routine you create (see [Wake](#wake)), which should count as a routine you schedule; this has not been confirmed on a live run.
- A short name such as `tincan-relay` or a `100.x` address goes to your default tailnet. Use the relay's full MagicDNS name, `tincan-relay.<tailnet>.ts.net`, so it always goes to the right one. The tailnet name is on the DNS page of the Tailscale admin.

## 2. Limit what Grok Bot's device can reach

Grok Bot only needs the relay. Do not tag the managed device: applying a tag such as `tag:grokbot` to it disconnects it and expires its key. That is an open Grok Bot bug, [cursor/plugins#1140](https://github.com/cursor/plugins/issues/1140). Until it is fixed, how far you can limit the device depends on the tailnet.

Company tailnet. Limit the device with a posture on `custom:grokbot`, as Grok Bot's docs say, not with a tag. Merge this into the policy file, with your own login (or a group) as `src` and the relay's tag or host as `dst`, on the port the relay listens on (80 for the default tsnet relay):

```hujson
{
  "postures": {
    "posture:grokbot": ["custom:grokbot == true"],
    "posture:human": ["custom:grokbot NOT SET"],
  },
  "defaultSrcPosture": ["posture:human"],
  "grants": [
    {
      "src": ["owner@example.com"],
      "srcPosture": ["posture:grokbot"],
      "dst": ["tag:tincan-relay"],
      "ip": ["tcp:80"],
    },
  ],
  "tests": [
    {
      "src": "owner@example.com",
      "srcPostureAttrs": {"custom:grokbot": true},
      "accept": ["tag:tincan-relay:80"],
    },
  ],
}
```

`defaultSrcPosture` keeps the Grok Bot device out of every other grant. If the policy already has default postures, add `"custom:grokbot NOT SET"` inside each of them and leave `defaultSrcPosture` as it is.

Personal tailnet. The device is untagged and has your full access: it can reach anything your own devices can. There is no supported way to narrow that right now. Tagging it breaks it (#1140), and the `custom:grokbot` posture is documented only for company tailnets. Decide whether that is acceptable. If it is not, use a company tailnet with the posture above, or wait for #1140 to be fixed.

Either way, the relay never treats Grok Bot's device as an admin device unless its name is in the relay's `--admin` list (see [the trust model](../trust-model.md)). The device is untagged and owned by your login, so that name is the only thing keeping it out. Never put the `grokbot-user-...` name in `--admin` or in `RELAY_ADMIN`, and do not use it for admin commands. Run those on the relay host or your own untagged laptop.

## 3. The startup script

[`examples/grokbot/tincan-up.sh`](../../examples/grokbot/tincan-up.sh) lives in the agent-tincan repo. Copy it to `~/.local/bin/` and make it executable. It is idempotent and needs no sudo.

With the managed connector, set `TS_MODE=managed` as a Grok Bot environment variable. The script then installs, starts and logs in to no Tailscale at all. It:

1. Notes a `tailscaled` that is still running on the computer, which the managed connector does not need.
2. Checks that `tincan` is installed at `~/.local/bin/tincan` (or `TINCAN`). A missing or non-executable `tincan` is unhealthy.
3. With `START_RELAY=1`, starts `tincan relay` if no relay is running as this user, as described below. Read section 5 before using this.
4. Runs `tincan doctor --json` and checks that the relay answered.

Without `TS_MODE`, or with `TS_MODE=userspace`, it runs the self-installed userspace Tailscale described in the [fallback](#fallback-a-self-installed-userspace-tailscale). There, it:

1. Installs `tailscale` and `tailscaled` into `~/.local/lib/tailscale` if they are missing. It downloads the static build from pkgs.tailscale.com and checks its sha256. Set `TS_VERSION` to pin a version. If the installed version differs from `TS_VERSION`, it installs the pinned one and restarts tailscaled.
2. Starts tailscaled in userspace mode if nothing answers on the socket.
3. Brings the node up. A node that still has its identity but is stopped comes up without a key. A logged-out node logs in only with `TS_AUTHKEY`, passed to `tailscale up` on stdin (`--auth-key=file:/dev/stdin`), so the key is never written to disk or shown in the process list. It never starts a browser login. It then waits for the node to be running. Settings: `TS_HOSTNAME` (default `grokbot`) and `TS_TAGS` (default `tag:grokbot`, which must match the key).
4. Checks that `tincan` is installed, as above.
5. With `START_RELAY=1`, starts the relay, as above.
6. Runs `tincan doctor`.

The relay, in both modes, always runs as its own tsnet node with its state in `~/.config/tincan-relay`, never with `--listen`, so it keeps its name and address whatever happens to the computer's own tailnet link. `RELAY_ADMIN` sets its `--admin` list. `TS_AUTHKEY` is never passed to the relay. `RELAY_TS_AUTHKEY` is passed to it as its `TS_AUTHKEY` only while `~/.config/tincan-relay/tsnet` holds no identity yet, so the relay's first login uses its own tagged key.

| Exit | Meaning | Fix |
|---|---|---|
| 0 | healthy | |
| 1 | something unhealthy, including a missing `tincan` or an unknown `TS_MODE` | read the output, run `tincan doctor` |
| 2 | userspace: device waiting for approval | approve it in the admin under Machines; next time make the key with Pre-approved on |
| 3 | userspace: logged out and the auth key is missing, the wrong type, expired, already used or rejected | the owner generates a fresh one-off key as in the fallback and sets the secret, then removes it after the login |
| 4 | userspace: legacy layout, a `tincan relay` started with `--listen` or a system `tailscaled` with its state in `/var/lib/tailscale` is running; nothing was started | move the box to the fallback layout (see [Moving an existing box to this layout](#moving-an-existing-box-to-this-layout)) |
| 5 | managed: the relay did not answer | check that the connector is connected, that this turn is one the connector serves (section 1), and that the relay URL is the full MagicDNS name. If the script says the config still uses the userspace proxy, clear it as in section 4 |

## 4. Join Grok Bot

```bash
tincan invite grokbot --kind vm-webhook                           # relay host or admin device
tincan join <code> --relay http://tincan-relay.<tailnet>.ts.net   # on Grok Bot's computer
```

Do not pass `--proxy`. The managed connector needs none. With no saved proxy, tincan uses the computer's standard proxy variables, if it has any, like any other program there.

`--proxy` is saved in `~/.config/tincan/client.json` as `"proxy"`, and a later `tincan join` or `tincan rejoin` keeps the saved value unless you pass `--proxy` again. A computer that was joined through the userspace proxy (`--proxy http://localhost:1055`) therefore keeps sending relay traffic to that dead port. Clear it with an empty value, `--proxy=`, on the next join or rejoin. A `TINCAN_PROXY` environment variable overrides the saved value, so remove it too if it is set.

Give Grok Bot the tools by adding `tincan mcp` as an MCP server (use the full path `~/.local/bin/tincan`), or let it call the `tincan` CLI from its shell.

If you ever remove and reconnect the connector, Grok Bot gets a new device. An untagged device owned by your login whose name matches the old one, apart from a `-<digits>` suffix, is re-admitted on its first call ([Rebuilt machines](../trust-model.md#rebuilt-machines)). If the name changed and tincan says this is not a joined agent, re-link with a new invite and `tincan join <code> --replace`.

## 5. Where the relay runs

Run the relay on a separate always-on host, or at least as a separate OS user, as the [README](../../README.md) says. That is the default for Grok Bot too:

```bash
tincan relay --admin <your-laptop>   # on the relay host, as the relay's own user
```

Do not run the relay as Grok Bot's user. The relay's local admin socket (`admin.sock` in its state dir) is protected only by file mode 0600, so any process running as the relay's user can use it with the owner's authority: invite and remove agents, and approve held requests. That user can also read the relay's database and the webhook and email secrets in `wake.json`. If the relay runs as Grok Bot's user, Grok Bot is effectively the owner.

On a Grok Bot computer only the main user's home folder survives a wipe, so a separate relay user there would lose its state on every wipe. Host the relay on another always-on machine instead.

If you run a relay here anyway (`START_RELAY=1`), know that Grok Bot has owner authority over it. Keep its `--state-dir` in the home folder (the default, `~/.config/tincan-relay`); that folder holds the relay's own Tailscale node, so the relay keeps its name and address across wipes. The relay is its own tsnet node and does not use the managed connector. The script starts it with `TS_AUTHKEY` removed from its environment, so if the relay's state were ever lost, it would not quietly register as a new node with Grok Bot's tag. A relay on the bot's computer with `TS_MODE=managed` has not been live-tested.

## 6. Health check

Grok Bot's standing instructions (`tincan onboard --section agents` for a `vm-webhook` agent) run `~/.local/bin/tincan-up.sh` at the start of every turn, before `check_inbox`. A Grok Bot computer has no init system that runs anything at boot, so the first turn after a rebuild, whatever starts it, is what brings the relay back if it runs here, and what reports a relay Grok Bot cannot reach. Set `TS_MODE=managed`, and `START_RELAY=1` and `RELAY_ADMIN` if the relay runs here, as Grok Bot environment variables, not in a file: they live on Cursor's side and survive a rebuild.

You can also add a recurring Grok Bot routine, hourly for example, that runs the script on the bot's computer itself, not on one of your other computers:

```bash
~/.local/bin/tincan-up.sh
```

Exit 0 means healthy. For any other exit code, tell the owner and include the output. Grok Bot should also run the script at the start of any turn where `tincan` fails.

With the managed connector, `tincan doctor` warns under `relay moves` that this computer cannot list the tailnet. That is expected: there is no local tailscaled to ask. The full MagicDNS name keeps working when the relay's address changes, because the tsnet relay keeps its name. If you ever rename the relay, run `tincan rejoin --relay http://<new name>.<tailnet>.ts.net` on Grok Bot's computer.

## Moving an existing grokbot agent to the managed connector

A Grok Bot that joined through the self-installed userspace Tailscale is a tagged `grokbot` device. The managed device is a different, untagged device named `grokbot-user-...`, so the relay does not move the agent on its own: the name does not match (see [Rebuilt machines](../trust-model.md#rebuilt-machines)). Move it once:

1. Connect the managed connector (section 1), and set up the posture grant if you are on a company tailnet (section 2).
2. Install the release's `tincan-up.sh` into `~/.local/bin`, set `TS_MODE=managed` as a Grok Bot environment variable, and reload the standing instructions with `tincan onboard --section agents`.
3. Stop the userspace tailscaled the script started, and remove `~/.local/lib/tailscale`, `~/.config/tailscale` and the `~/.local/bin/tailscale` wrapper. Unset `TS_SOCKET` if you set it for tincan. A relay running here as its own tsnet node is not affected.
4. On the relay host or an admin device, run `tincan invite grokbot --kind vm-webhook`. On Grok Bot's computer, run:

   ```bash
   tincan join <code> --replace --relay http://tincan-relay.<tailnet>.ts.net --proxy=
   ```

   `--proxy=` clears the saved userspace proxy (section 4). Use the full MagicDNS name: a short name goes to the default tailnet.
5. Run `~/.local/bin/tincan-up.sh`. It should exit 0 and print `relay ok through the managed Tailscale connector`. Then check that `tincan agents` on an admin device lists grokbot.
6. Delete the old `grokbot` device in the Tailscale admin, remove any `TS_AUTHKEY` left in Grok Bot's secrets, and drop the `tag:grokbot` rules from the policy if nothing else uses them.

## Fallback: a self-installed userspace Tailscale

This is the method this page used to recommend. Grok Bot now says not to install a Tailscale client on its computer, so on Grok Bot use the managed connector above. Keep this for other sandbox hosts that allow their own Tailscale client and keep only the user's home folder between rebuilds, or for a Grok Bot setup you have not moved yet. `tincan-up.sh` runs it when `TS_MODE` is unset or `userspace`.

If you set up Tailscale the default way on such a host (installed as a system package, state in `/var/lib/tailscale`, signed in through a browser), every wipe makes a new device that someone has to approve in the Tailscale admin. Set it up once with a one-off, pre-approved, tagged auth key and keep Tailscale's state in the home folder. After that the host rejoins on its own, as the same device, and the key is no longer needed.

### What survives a wipe with this method

| Thing | Where to keep it | Survives |
|---|---|---|
| `tailscale`, `tailscaled` | `~/.local/lib/tailscale/` (wrapper in `~/.local/bin`) | yes |
| Tailscale node identity | `~/.config/tailscale/` (chmod 700) | yes |
| Relay state, if the relay runs here | `~/.config/tincan-relay/` (the default `--state-dir`) | yes |
| tincan config | `~/.config/tincan/` | yes |
| tailscaled socket and log | `~/.cache/tailscale/` | recreated each start |
| Auth key | secret `TS_AUTHKEY`, only until the first login | not needed after the first login; remove it |

### Make a tag and an auth key (once, in the Tailscale admin)

Add a tag that an admin owns to the tailnet policy:

```json
"tagOwners": { "tag:grokbot": ["autogroup:admin"] }
```

Limit what the tag can reach. The host only needs the relay, so allow `tag:grokbot` to reach the relay on tcp:80 (or the port your relay listens on) and nothing else. For example, with the relay's tag or tailnet IP in place of `<relay>`:

```json
"acls": [{ "action": "accept", "src": ["tag:grokbot"], "dst": ["<relay>:80"] }]
```

This tag is for a device this method installs itself. Do not apply it to Grok Bot's managed device (section 2).

Then go to Settings > Keys > Generate auth key:

- Reusable: off. A one-off key enrolls this host once. The host's shell can read its own environment, so a key left there could be leaked, and a reusable one would let whoever holds it add more tagged devices with no approval.
- Pre-approved: on. This one matters most. If your tailnet has device approval turned on, a key without it still leaves every new device waiting for approval, which is the daily chore again.
- Ephemeral: off. An ephemeral node is removed when it goes offline, and a wipe takes it offline.
- Tags: `tag:grokbot`.
- Expiry: short, a day is enough. The key is only used for the first login. Tagged devices have node key expiry off by default, so a host that keeps its state keeps working after the auth key expires.

Add the key as a secret environment variable named `TS_AUTHKEY`. Never put it in a file, a script or a chat. The key must start with `tskey-auth-`. An API access token or OAuth client secret will not work.

After the first successful login, remove `TS_AUTHKEY` from the secrets. The identity now lives in `~/.config/tailscale` and survives wipes, so the key is not needed again. If the identity is ever lost, the script exits 3 and the owner issues a fresh one-off key.

### Run tailscaled as your user (no systemd, no root)

There is no systemd and no root, so tailscaled runs as the normal user in userspace mode. With no TUN device, programs reach the tailnet through tailscaled's local proxy. `tincan-up.sh` starts it like this:

```bash
tailscaled --tun=userspace-networking \
  --statedir="$HOME/.config/tailscale" \
  --socket="$HOME/.cache/tailscale/tailscaled.sock" \
  --socks5-server=localhost:1055 --outbound-http-proxy-listen=localhost:1055
```

One port serves both SOCKS5 and HTTP. Every `tailscale` command needs `--socket=$HOME/.cache/tailscale/tailscaled.sock`. The script installs a small `tailscale` wrapper that adds it for you.

After an ordinary wipe, `~/.config/tailscale` comes back, so the script restarts the same device with the same name and IP. It does not use the key, and nothing needs approval.

### Join through the proxy

```bash
tincan invite grokbot --kind vm-webhook                                   # relay host or admin device
tincan join <code> --relay http://tincan-relay --proxy http://localhost:1055   # on the host
```

`--proxy` is saved in `~/.config/tincan/client.json`, so `tincan mcp` and the CLI keep using it. The host is tagged, so it is never an admin device. Run admin commands on the relay host or an untagged laptop.

### If the Tailscale identity is lost anyway

If `~/.config/tailscale` is lost, the script exits 3 because the key was removed after the first login. The owner issues a fresh one-off key and sets `TS_AUTHKEY`, and the script logs in again with it. It needs no approval, but it creates a new device with a new IP, and the name may get a `-1` suffix while the old device is listed. The relay never re-admits a tagged node on its own (see [the trust model](../trust-model.md#rebuilt-machines)), so `tincan rejoin` will not move the agent to the new device. Delete the old device in the admin, then re-link with a new invite:

```bash
tincan invite grokbot --socket ~/.config/tincan-relay/admin.sock   # on the relay host
tincan join <code> --replace --relay http://tincan-relay --proxy http://localhost:1055
```

With the state in the home folder this should be rare. An ordinary wipe needs no re-link.

### Moving an existing box to this layout

A host set up some other way (system Tailscale with its state in `/var/lib/tailscale`, a relay started with `--listen`, a hand-written keep-alive loop) loses its node and its relay on the next rebuild, and `tincan-up.sh` exits 4 while that setup is running. On Grok Bot, move to the managed connector instead (see [above](#moving-an-existing-grokbot-agent-to-the-managed-connector)). On another host, move it once, with the owner present:

1. Install the release's `tincan-up.sh` into `~/.local/bin` and reload the standing instructions with `tincan onboard --section agents`. Set `START_RELAY=1` and `RELAY_ADMIN=<the owner's untagged laptop>` as environment variables.
2. The owner creates two one-off, pre-approved, non-ephemeral keys: one tagged `tag:grokbot`, set as `TS_AUTHKEY`, and one tagged with a relay-only tag such as `tag:tincan-relay` (owned by `autogroup:admin`), set as `RELAY_TS_AUTHKEY`. If the tailnet policy limits agents to the relay's IP, change those rules to the relay's tag now.
3. Stop the `--listen` relay, its keep-alive loops and the system `tailscaled`. The team is down from here until step 4 finishes.
4. Run `tincan-up.sh`. It brings up the host's node in `~/.config/tailscale` and starts the relay as its own tagged tsnet node with the existing state dir. Remove both keys from the secrets afterwards.
5. The host's node is new and tagged, so the relay never re-admits it on its own (see [the trust model](../trust-model.md#rebuilt-machines)). Re-link it once: `tincan invite grokbot --socket ~/.config/tincan-relay/admin.sock`, then `tincan join <code> --replace --relay http://tincan-relay --proxy http://localhost:1055`. Other agents find the relay through its advertised addresses and the tailnet netmap.
6. When `tincan agents` lists grokbot and the other online agents, delete the old system-Tailscale devices in the Tailscale admin.
7. Drill: rebuild the host, then send the agent one message. Its first turn must log `started tincan relay`, every agent must reconnect, an admin command such as `tincan held` must work from the owner's laptop, and no new Tailscale device may appear.

### Relay rediscovery and the custom socket

When the relay moves, the client first tries the relay's advertised addresses, then every IPv4 address on the local Tailscale netmap (`internal/client/discover.go`). It reads the netmap from tailscaled's LocalAPI, and falls back to `tailscale status --json` only if LocalAPI is unreachable. With a userspace tailscaled on a custom socket, tincan finds the socket from the running tailscaled's `--socket` flag, or under the home folder (`~/.tailscale*/`, `~/.cache/tailscale/`, `~/.config/tailscale/`, `~/.local/share/tailscale/`, `~/.local/state/tailscale/`); when more than one tailscaled runs, it lists them all. If `tincan doctor` still warns under `relay moves` that it cannot list the tailnet, set `TS_SOCKET` in the environment of every tincan process, including whatever starts `tincan mcp`:

```sh
export TS_SOCKET="$HOME/.cache/tailscale/tailscaled.sock"
```

The `tailscale` wrapper the script installs in `~/.local/bin` still adds `--socket` for your own `tailscale` commands, and it also serves the CLI fallback when `~/.local/bin` is on `PATH`. With the managed connector there is no local tailscaled, so this does not apply (see section 6).

## Know when the relay is down

A relay that is down cannot send its own outage notice, so watch it from another always-on machine. `tincan relay-watch` probes the relay every `--every` (default 1m) with a joined client config's relay discovery, so a relay that moved still counts as up. After `--after` (default 10m) with no answer it runs `--alert-cmd` once, and once more when the relay answers again. The command gets `TINCAN_WATCH_MESSAGE`, `TINCAN_WATCH_EVENT` (`down` or `up`), `TINCAN_WATCH_RELAY` and `TINCAN_WATCH_SINCE`. [`examples/watch/imessage-alert.sh`](../../examples/watch/imessage-alert.sh) sends the message by iMessage to `TINCAN_ALERT_TO`. To run it as a service on a Mac that stays on:

```bash
tincan relay-watch install --config <a joined client config on that machine> \
  --alert-cmd 'TINCAN_ALERT_TO=you@example.com /path/to/imessage-alert.sh'
```

It writes a launchd agent (a systemd user unit on Linux) and prints the command that starts it. If the relay runs on Grok Bot's computer, the alert after a rebuild is the cue to send Grok Bot a message, which runs `tincan-up.sh` and brings the relay back.

## Wake

Grok Bot wakes on its webhook. In the relay's `wake.json`:

```json
{ "grokbot": { "method": "webhook", "url": "<Grok Bot webhook URL>", "bearer_token": "<webhook key>" } }
```

The relay posts `{"source":"agent-tincan","message":"Agent Tincan: 2 requests waiting. Run check_inbox ..."}`. Grok Bot then calls `check_inbox`. The same wake can also mean a reply to one of Grok Bot's own requests is waiting (the message then counts replies). `check_inbox` shows it, and Grok Bot finishes the work that was waiting on it.

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
