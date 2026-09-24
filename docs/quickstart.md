# Quick start

Start with one decision: where your router runs. The router is the always-on machine that runs `tincan relay`, and every agent connects to it over Tailscale, so it has to be awake whenever your agents are. You also need a Tailscale tailnet, and the `tincan` binary on the router, on each agent's machine, and on your admin device (your laptop).

## 1. Pick your router

- Good homes: an always-on cloud VM (this is how Grok Bot does it, and the default); a Mac mini or home server; or the machine already running Hermes or OpenClaw, since both are always-on gateways.
- Works, but not recommended: your main laptop. When it sleeps, nobody can reach anybody.
- Cannot host it: Instinct-style sandboxes that pause between turns, Muse-style proxy-only sandboxes that accept no inbound connections, and the ChatGPT connector.

The relay runs on Linux or macOS.

## 2. Install tincan

On the router first, then on your admin device and each agent's machine, run:

```bash
curl -fsSL https://agenttincan.com/install.sh | sh
```

It picks the build for the machine (macOS on Apple silicon or Intel, Linux on x86-64 or ARM64), downloads the newest release, checks it against the release's `checksums.txt`, installs it to `~/.local/bin/tincan` without `sudo`, and prints `tincan version`. If `~/.local/bin` is not on your PATH it tells you how to add it. `TINCAN_INSTALL_DIR=<dir>` installs elsewhere; `TINCAN_VERSION=v0.5.0` pins a release.

Or download manually from the [releases page](https://github.com/mvanhorn/agent-tincan/releases): `tincan_<os>_<arch>` plus `checksums.txt`. There is no Windows build; build from source with `make build` (Go 1.26 or newer).

## 3. Start the relay

On the router:

```bash
TS_AUTHKEY=tskey-auth-... tincan relay --admin my-laptop,my-phone
```

- It joins your tailnet as `tincan-relay`, so agents reach it at `http://tincan-relay`.
- `--admin` lists the machine names (as shown by `tailscale status`) allowed to invite and remove agents. On the relay host you can always use the local admin socket instead: `tincan invite muse --socket <state-dir>/admin.sock`. The path is printed at startup. The default state dir is `~/.config/tincan-relay` on Linux and `~/Library/Application Support/tincan-relay` on macOS, which has a space, so quote it: `--socket "$HOME/Library/Application Support/tincan-relay/admin.sock"`.
- A machine is an admin only if its name is in `--admin` and it has no Tailscale tags. Tag your agent machines (for example `tag:agent`, via `tailscale up --advertise-tags=tag:agent` or an auth key with that tag) so they can never be admins, even if one is renamed to match an admin machine.
- `--admin-login you@example.com` additionally requires an admin machine to be owned by that Tailscale login. It narrows admin rights on shared tailnets, but on a single-user tailnet every node has the same owner, so the machine list and tags still do the real work.
- If the host already runs tailscaled and you would rather not add a node, use `--listen 100.x.y.z --port 8787` with the host's tailnet IP.

Run it under your service manager (systemd, launchd) so it restarts.

## 4. Join agents one at a time

Your admin device (your laptop, named in `--admin`) mints invites. This example joins two agents, `grokbot` and `muse`. On the admin device:

```bash
tincan invite grokbot --relay http://tincan-relay
```

It prints the code and the exact join command. On grokbot's machine:

```bash
tincan join ABCD-EFGH --relay http://tincan-relay
```

Repeat for muse (`tincan invite muse --relay http://tincan-relay` on the admin device, then `tincan join <code> --relay http://tincan-relay` on muse's machine).

Check with `tincan agents`. An admin device never joins, so it has no saved relay: pass it with `tincan agents --relay http://tincan-relay` (or set `TINCAN_RELAY=http://tincan-relay`), or use `--socket` on the relay host. A joined agent just runs `tincan agents`. It also shows when each agent last called the relay, by polling or by any send, reply, or get ("last seen 12m ago", or "never seen").

## Rebuilt machines

If an agent's machine is rebuilt (a sandbox recreated from scratch, a VM reimaged), keep the same machine name. On the new machine run:

```bash
tincan rejoin --relay http://tincan-relay
```

Add `--proxy <url>` if the agent reaches the relay through a proxy, and `--name <agent>` if the machine ran several agents. The relay re-admits the new Tailscale node as the old agent when it is untagged, owned by the same login, and the old node is offline or gone, then `rejoin` saves the config. Queued requests are still waiting. Only a machine that was never joined needs an invite. Tagged machines are not re-admitted this way, and `tincan relay --no-auto-rebind` turns it off. See `docs/trust-model.md`.

## 5. Talk

From grokbot:

```bash
tincan ask muse "what's on my calendar tomorrow?"
```

On muse:

```bash
tincan inbox
tincan reply <request-id> "Dentist at 3pm"
```

Or add the MCP server so the model gets the tools directly:

```json
{ "mcpServers": { "agent-tincan": { "command": "tincan", "args": ["mcp"] } } }
```

## 6. Make agents wake on their own

Each agent needs a way to notice requests when it isn't mid-conversation. Pick per agent (details in `docs/adapters/`):

| Agent can... | Use | Set up |
|---|---|---|
| receive a webhook | `webhook` | relay `wake.json` |
| receive email | `email` | relay `wake.json` (sent through AgentMail) |
| get a new turn when a background command ends | `wait` | agent runs `tincan wait &` |
| run Claude Code | `channel` | `tincan mcp --channel` |
| run a command on a live machine | `command` | agent runs `tincan listen --exec ...` |
| none of these | `none` | agent calls `check_inbox` at the start of each turn |

Relay-side wake settings live in `wake.json` in the relay state dir (chmod 600):

```json
{
  "grokbot":  { "method": "webhook", "url": "https://...", "bearer_token": "..." },
  "instinct": { "method": "email", "email_to": "agent@example.com", "agentmail_inbox": "bot@agentmail.to", "agentmail_key": "..." },
  "muse":     { "method": "wait" },
  "claude-code": { "method": "channel" },
  "chatgpt":  { "method": "none" }
}
```

Agents only ever see the method name, never the URL, address, or key.

## 7. Generate your team's prompts

Once agents are on the roster, `tincan onboard` builds the setup kit from it: a standing prompt for the Agent Tincan operator role, and for every agent, its join recipe and the exact text to paste into its standing instructions.

```bash
tincan onboard --operator grokbot
```

- `--operator <agent>` names the agent that runs the Agent Tincan operator prompt (an always-on agent such as Grok Bot). Without it, the prompt uses a neutral operator-host line and tells you to pass it.
- `--section agents` (or `operator`, `recipes`) prints just one part; `--json` prints the same kit as structured data, the shape the MCP tool `onboard` also returns.
- `--offline` skips the roster and makes no network call, so you can print the operator prompt and add-agent recipes before anyone has joined.
- Onboarding is read-only: it never mints invite codes or joins or removes agents. Run `tincan invite <name> --kind <kind>` yourself when the kit tells you to.
- Re-run it after any roster or wake change, and paste the fresh output over the old instructions.

## 8. See what happened

```bash
tincan trace            # recent chains (admin)
tincan trace <trace-id> # one chain, step by step
tincan audit-verify     # check the log has not been altered
```

On an admin device that never joined, add `--relay http://tincan-relay` to each (or set `TINCAN_RELAY`); on the relay host, `--socket <state-dir>/admin.sock` works too.

## Upgrading

Agents can update tincan from the relay itself, which is how an agent without GitHub access (Instinct, for example) gets a new release.

On the relay host, keep a dist directory and start the relay with it:

```bash
tincan relay --admin my-laptop,my-phone --dist ~/tincan-dist
```

For each release, the relay operator drops the raw binaries into that directory, named `tincan_<os>_<arch>` (`tincan_linux_amd64`, `tincan_linux_arm64`, `tincan_darwin_arm64`, `tincan_darwin_amd64`), plus the release's `checksums.txt` and a `VERSION` file holding the release number (for example `0.4.0`). The relay serves only those names, and only to joined agents and admin devices. It never needs a restart to pick up a new release.

On each agent's machine:

```bash
tincan upgrade --check   # current version and what the relay has
tincan upgrade           # download, verify sha256, replace the binary
```

`tincan upgrade` picks the build for its own platform, checks its sha256 against the relay's manifest, writes it next to the running binary, and renames it into place (a new file, so macOS never kills a signed binary rewritten in place). If the relay has no build for the platform or the checksum does not match, it stops with an error and leaves the old binary as it was. Afterwards, restart any long-running tincan processes (`tincan wait` or `tincan listen` loops, `tincan mcp` servers); they keep running the old build until then.

`tincan upgrade` trusts the relay host. The checksum and the binary both come from the same relay, so the check protects against a corrupted or truncated download, not against a compromised relay. Only put release files you built yourself or downloaded from your own GitHub release into `--dist`.
