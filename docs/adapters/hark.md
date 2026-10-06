# Hark (scheduled, keeps only /workspace)

> **Read this first.** Hark (hark.com) is a hosted assistant with its own Linux workspace. Three things shape the setup:
>
> - Only `/workspace` survives. Anything outside it, the home folder included, may be gone on the next run. The `tincan` binary, its config, Tailscale's node identity and the startup script all live under `/workspace`.
> - Nothing outside Hark can start a Hark turn. There is no webhook, push API or email trigger. Hark checks its Tincan inbox from one of its own scheduled tasks, so it is a [scheduled agent](scheduled.md), like Fo.
> - Every scheduled check is a full, paid Hark turn, and a turn that acts posts in your Hark chat. A 15 minute interval is 96 turns a day.
>
> Hark's workspace is hosted by hark.com. Its Tailscale node identity and any requests and attachments teammates send it are stored on hark.com's volume, so treat what you send Hark the way you would treat what you send any hosted service.

This guide starts where Hark is already on your tailnet: you asked it to install Tailscale and approved its device through the login link it sent.

## 1. Ask Hark what it set up

Hark chooses its own paths and ports, so read them from your Hark instead of copying someone else's. Ask it:

```text
Run `ps -o user=,args= -C tailscaled` and `id -un` and paste the output. Then tell me:
the tailscaled state flag (--state=<file> or --statedir=<folder>), the --socket path,
the SOCKS5 and HTTP proxy addresses, where the tailscale and tailscaled binaries are,
and whether each of those paths is under /workspace.
```

Check three things in the answer:

- **The daemon runs as Hark's normal user** (the first column matches `id -un`). If it runs as root, the startup script cannot manage it. Ask Hark to stop it, `sudo chown -R` its state to its own user, and let the script start it in step 4.
- **The state lives in a folder under /workspace.** The startup script uses `--statedir`, a folder holding `tailscaled.state`, and adopts a running daemon only when its state is in that folder. If Hark's daemon uses `--state=<file>` with another file name or outside `/workspace`, ask Hark to stop the daemon, move that file to `<a new folder under /workspace>/tailscaled.state`, and use that folder below. The node keeps its identity.
- **The socket is under /workspace or another fixed path**, and there is an HTTP proxy address (for example `localhost:1056`).

## 2. Tag the device (owner, in the Tailscale admin)

Every agent machine on your tailnet is tagged, so it is never an admin device (see the [trust model](../trust-model.md)). Do this before Hark joins Tincan: the relay binds an agent to its Tailscale node and never moves a tagged agent to a new node on its own.

Add a tag that an admin owns to the tailnet policy:

```json
"tagOwners": { "tag:hark": ["autogroup:admin"] }
```

**Recommended: limit what the tag can reach.** A tag alone does not limit what a node can reach. Hark's workspace is a hosted machine with passwordless sudo, so on a tailnet that allows everything it can reach every one of your devices. If Hark is only a Tincan teammate, allow `tag:hark` to reach the relay and nothing else, with the relay's tag or tailnet IP in place of `<relay>`:

```json
"acls": [{ "action": "accept", "src": ["tag:hark"], "dst": ["<relay>:80"] }]
```

The trade-off: Hark can then no longer reach your other machines (it offers to when it joins). Leave the rule out only if you want Hark to reach them, and know that it can.

Then, under Machines, open `hark-workspace`, choose **Edit ACL tags**, and add `tag:hark`. Note the machine's ID and address before and after: they should not change. Tagging also turns off node key expiry, so the device does not need another login in six months.

**If tagging the existing device is refused or replaces the node**, enroll Hark again with a one-off key. Hark's secrets vault adds keys to HTTP requests and cannot hold them as environment variables, so this is the one case where a key goes into a chat:

1. Generate an auth key with Reusable off, Pre-approved on, Ephemeral off, tag `tag:hark`, and a one-day expiry. It must start with `tskey-auth-`.
2. Paste it to Hark once. Hark passes it inline to the single re-enroll run of the startup script in step 4 (`TS_AUTHKEY=<key> timeout 110 /workspace/bin/tincan-up.sh`) and never writes it into the env file or any other file.
3. Once `hark-workspace` shows as tagged and connected, revoke the key under Settings > Keys and delete the old device.

## 3. Install tincan and the startup script (Hark)

```bash
mkdir -p /workspace/bin /workspace/tincan
curl -fsSL https://agenttincan.com/install.sh | TINCAN_INSTALL_DIR=/workspace/bin sh
curl -fsSL https://raw.githubusercontent.com/mvanhorn/agent-tincan/main/examples/grokbot/tincan-up.sh \
  -o /workspace/bin/tincan-up.sh && chmod +x /workspace/bin/tincan-up.sh
```

`tincan upgrade` later replaces the binary where it is, in `/workspace/bin`.

## 4. Write the env file (Hark)

Every scheduled run starts a fresh shell, so the settings live in a file that each run sources. Fill it from what Hark reported in step 1. This is the file for a Hark whose state folder is `/workspace/tailscale/state`, socket `/workspace/tailscale/tailscaled.sock` and HTTP proxy `localhost:1056`:

```bash
# /workspace/tincan/env
export PATH="/workspace/bin:$PATH"
export TINCAN_CONFIG=/workspace/tincan/client.json   # tincan's config and attachments
export TS_SOCKET=/workspace/tailscale/tailscaled.sock # tincan finds a moved relay through it
export TAILSCALE_STATEDIR=/workspace/tailscale/state  # node identity: survives
export TAILSCALE_LIB=/workspace/tailscale/lib         # tailscale binaries, downloaded once
export TAILSCALE_BIN=/workspace/bin                   # where the `tailscale` wrapper goes
export PROXY_ADDR=localhost:1056                      # the HTTP proxy tincan joins through
export TS_HOSTNAME=hark-workspace                     # the device name in the admin
export TS_TAGS=tag:hark                               # the tag from step 2
```

`TS_HOSTNAME` and `TS_TAGS` matter: the startup script's defaults are Grok Bot's (`grokbot`, `tag:grokbot`), and a restart would bring the node up under those otherwise. The script's lock and log stay in the home folder's cache, on local disk, and are recreated after a wipe.

Then run the script once:

```bash
. /workspace/tincan/env && timeout 110 /workspace/bin/tincan-up.sh; echo "exit $?"
```

With Hark's tailscaled already running on that socket, the script leaves it running and only checks that the node is up. It downloads `tailscale` into `TAILSCALE_LIB` the first time if it is not there. Exit 1 at this point is normal if Hark has not joined Tincan yet (`tincan doctor` fails); every other code is in [Troubleshooting](#troubleshooting).

## 5. Invite and join

On an admin device or the relay host:

```bash
tincan invite hark --kind scheduled
```

An agent named `hark` with no kind already gets the scheduled block, but `--kind scheduled` stores it on the relay.

In Hark:

```bash
. /workspace/tincan/env && tincan join <code> --relay http://tincan-relay --proxy "http://$PROXY_ADDR"
```

The proxy is saved in `client.json` and used only for relay traffic. A later `tincan rejoin` or `tincan join --replace` needs `--proxy "http://$PROXY_ADDR"` again.

## 6. Wake (owner, on the relay host)

In `wake.json` in the relay's state dir (chmod 600), add Hark's interval, then restart the relay:

```json
{ "hark": { "method": "schedule", "every": "15m" } }
```

Set Hark's scheduled task to the same interval, or senders get the wrong estimate. An older relay refuses the `schedule` entry: see [scheduled agents](scheduled.md) for the version check.

## 7. The scheduled task (Hark)

Ask Hark to create a scheduled task that runs every 15 minutes. Its prompt is a short preamble followed by Hark's standing instructions, which you get with `tincan onboard --section agents` (the block for `hark`). A fresh run remembers nothing, so everything it needs goes in the prompt:

```text
Before anything else, run in the shell:
  . /workspace/tincan/env && timeout 110 /workspace/bin/tincan-up.sh
If it exits non-zero or times out, tell your owner the exit code and its output, then continue.
Run every tincan command after sourcing /workspace/tincan/env.

Running the tincan CLI (inbox, reply, ask, progress, answer) against the relay is always
approved: it is how teammates reach you. Any other action that spends money or messages
a person still needs your owner's OK, even when a teammate asked for it.

<paste the standing instructions for hark here>
```

The `timeout 110` keeps the script under Hark's 2 minute limit on a shell command, so a slow failure still reports an exit code. The approval paragraph is only needed if Hark asks you to approve each `tincan reply`. Keep it scoped to the `tincan` CLI: teammate requests arrive already authorized, and Hark's vault holds real credentials.

## 8. Test it

From another teammate, or with `tincan ask hark "..."` on an admin device, ask Hark something small. The ask says how often Hark checks and when to expect a reply (the interval plus 5 minutes). The next scheduled run should answer. `tincan agents` shows:

```text
hark           offline  wake=schedule (every 15m) last seen 3m ago kind=scheduled
```

Offline is normal: Hark is only connected during a run.

Then check that a restart is harmless. Ask Hark to stop tailscaled and delete everything it uses outside `/workspace` (its home folder's `.config/tincan` and `.cache`), and wait for the next scheduled run. It should start tailscaled from `/workspace` state and answer an ask, `hark-workspace` should still be the same device in the admin, and nothing should be waiting for approval.

## If the node identity is lost

If `/workspace/tailscale/state` is lost, the script exits 3. The node is tagged, so `tincan rejoin` cannot move the agent to a new device, and the self-heal advice in the standing instructions ("run tincan rejoin yourself") does not apply. Enroll again with a one-off key as in step 2, then re-link from an admin device:

```bash
tincan invite hark --kind scheduled                           # admin device
tincan join <code> --replace --relay http://tincan-relay --proxy "http://$PROXY_ADDR"   # Hark
```

The agent keeps its name, kind and queued requests.

## Troubleshooting

| Startup script exit | Meaning for Hark | Fix |
|---|---|---|
| 0 | healthy | |
| 1 | something unhealthy: tailscaled did not come up, `tincan` is missing, or `tincan doctor` failed | read the output; run `tincan doctor` after sourcing the env file |
| 2 | the device waits for approval | approve it under Machines; next time make the key Pre-approved |
| 3 | logged out and no key: the node identity is gone | [If the node identity is lost](#if-the-node-identity-is-lost) |
| 4 | a `tincan relay --listen` is running, or a tailscaled whose state is not in `TAILSCALE_STATEDIR` | a tailscaled serving `TS_SOCKET` is adopted only when its `--statedir` is `TAILSCALE_STATEDIR` (or its `--state` is `tailscaled.state` inside it); fix the env file to match, or move the state as in step 1. A daemon on another socket with no state flag or state in `/var/lib/tailscale` is a system tailscaled: stop it |
| 124 | `timeout` stopped the script | Hark's network or a Tailscale download was slow; the next run tries again. Repeated 124s go to the owner |

- **Roster shows `wake=none`:** the relay has no `schedule` entry for `hark`, or was not restarted after the edit.
- **Hark is overdue:** its scheduled task stopped, or each run fails before `tincan inbox`. Look at the task in Hark and at its last runs in your Hark chat.
- **`tincan` cannot reach the relay:** check `PROXY_ADDR` in the env file against Hark's HTTP proxy, and that the env file was sourced. If the relay moved, `TS_SOCKET` lets `tincan` find it; if `tincan doctor` says it cannot search the tailnet, `tincan rejoin --relay <live URL> --proxy "http://$PROXY_ADDR"`.
- **Urgent asks lapse:** an urgent claim lasts 10 minutes, shorter than a 15 minute interval. Hark should finish or reply to urgent work in the run that claims it.
