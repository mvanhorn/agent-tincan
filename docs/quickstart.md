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

It picks the build for the machine (macOS on Apple silicon or Intel, Linux on x86-64 or ARM64), downloads the newest stable release, checks it against the release's `checksums.txt`, installs it to `~/.local/bin/tincan` without `sudo`, and prints `tincan version`. If `~/.local/bin` is not on your PATH it tells you how to add it. `TINCAN_INSTALL_DIR=<dir>` installs elsewhere; `TINCAN_VERSION=v0.5.0` pins a release. Prereleases such as `v0.6.0-rc1` install only when `TINCAN_VERSION` names them.

Or download manually from the [releases page](https://github.com/mvanhorn/agent-tincan/releases): `tincan_<os>_<arch>` plus `checksums.txt`.

### Windows (experimental, CI tested only)

A Windows machine can be a teammate. `join`, `ask`, `inbox`, `reply`, `mcp`, `doctor` and `upgrade` run on 64-bit Windows (x86-64 or ARM64). This support is experimental, CI tested only: a Windows CI job runs the client's tests and a smoke test of the built exe against a test relay, but no one has run it on a real Windows machine yet. Install and connect Tailscale for Windows first.

In PowerShell, this downloads the newest stable release, checks it against the release's `checksums.txt`, and installs it to `%LOCALAPPDATA%\tincan\tincan.exe`. The last line adds that folder to your user PATH; run it once and open a new terminal.

```powershell
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
$name = "tincan_windows_$arch.exe"
$base = 'https://github.com/mvanhorn/agent-tincan/releases/latest/download'
$dir = Join-Path $env:LOCALAPPDATA 'tincan'
$new = Join-Path $dir "$name.new"
$sums = Join-Path $dir 'checksums.txt'
New-Item -ItemType Directory -Force -Path $dir | Out-Null
Invoke-WebRequest "$base/$name" -OutFile $new -UseBasicParsing
Invoke-WebRequest "$base/checksums.txt" -OutFile $sums -UseBasicParsing
$line = Select-String -Path $sums -Pattern ([regex]::Escape($name) + '$') | Select-Object -First 1
$want = ($line.Line -split '\s+')[0]
$have = (Get-FileHash $new -Algorithm SHA256).Hash
if (-not $want -or $want -ne $have) { Remove-Item $new; throw "checksum mismatch for $name" }
Move-Item -Force $new (Join-Path $dir 'tincan.exe')
[Environment]::SetEnvironmentVariable('Path', [Environment]::GetEnvironmentVariable('Path', 'User') + ";$dir", 'User')
```

The exe is not code signed, so Windows may warn before running it. To update later, run `tincan upgrade`.

What differs on Windows:

- The client config is `%AppData%\tincan\client.json`. For a second agent, set `$env:TINCAN_CONFIG` to its own file.
- The relay stays on Linux or macOS. `tincan relay` refuses on Windows.
- `tincan listen` and `tincan relay-watch` run commands through `sh -c`, so they refuse on Windows, and the wake scripts in `examples/` are bash. Use the MCP tools or `tincan wait`. Socket wake needs unix sockets, so it does not run either.
- `tincan upgrade` needs a relay on a release that serves the Windows builds, so upgrade the relay first. Windows will not replace a running exe, so the old one is renamed to `tincan.old-<timestamp>` next to the new one. Later runs delete it once nothing holds it open, and an app's `tincan mcp` can hold it until the app restarts.
- Windows has no unix file modes, so the config and downloaded attachments are protected by the permissions on your user profile folder, not by 0600 files. Two tincan processes saving the config at once are not serialized.

## 3. Start the relay

On the router:

```bash
TS_AUTHKEY=tskey-auth-... tincan relay --admin my-laptop
```

- It joins your tailnet as `tincan-relay`, so agents reach it at `http://tincan-relay`.
- `--admin` lists the machine names (as shown by `tailscale status`) allowed to invite and remove agents. The relay host is always an admin with no flags: `tincan invite muse` there uses its local admin socket. Pass `--socket <state-dir>/admin.sock` only when the relay runs with a non-default `--state-dir` (the path is printed at startup; on macOS the default dir has a space, so quote it).
- A machine is an admin only if its name is in `--admin` and it has no Tailscale tags. Tag your agent machines (for example `tag:agent`, via `tailscale up --advertise-tags=tag:agent` or an auth key with that tag) so they can never be admins, even if one is renamed to match an admin machine.
- `--admin-login you@example.com` additionally requires an admin machine to be owned by that Tailscale login. It narrows admin rights on shared tailnets, but on a single-user tailnet every node has the same owner, so the machine list and tags still do the real work.
- If the host already runs tailscaled and you would rather not add a node, use `--listen 100.x.y.z --port 8787` with the host's tailnet IP. The default is better: its own node keeps a stable name and IP even when the host re-joins Tailscale. With `--listen`, agents that can see the tailnet find a new host IP themselves; a proxy-only agent needs `tincan rejoin`. Back up the state dir either way.

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

Owners can watch the mesh with `tincan top --relay http://tincan-relay` (no flags on the relay host). It refreshes every 2 seconds; `--interval 5s` changes the cadence, and `q` or Ctrl-C quits. Admin devices see held approvals and recent chains alongside the roster. `tincan top --once` prints a read-only snapshot.

Check with `tincan agents`. An admin device never joins, so it has no saved relay: pass it with `tincan agents --relay http://tincan-relay` (or set `TINCAN_RELAY=http://tincan-relay`), or run it on the relay host, where it needs no flags. A joined agent just runs `tincan agents`. It also shows when each agent last called the relay, by polling or by any send, reply, or get ("last seen 12m ago", or "never seen"), and which tincan build each last called with, next to the relay's own, so an agent that needs `tincan upgrade` stands out.

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

On an admin device that never joined, add `--relay http://tincan-relay` to each (or set `TINCAN_RELAY`). On the relay host they need no flags: its local admin socket is used. Pass `--socket <state-dir>/admin.sock` only for a socket elsewhere.

## Upgrading

Agents can update tincan from the relay itself, which is how an agent without GitHub access (Instinct, for example) gets a new release.

On the relay host, keep a dist directory and start the relay with it:

```bash
tincan relay --admin my-laptop --dist ~/tincan-dist
```

For each release, either run `tincan relay-upgrade --from-github v0.8.0` from your admin device (see below; the relay needs `--release-url`), which has the relay fetch the release into that directory itself, or drop the raw binaries into it by hand, named `tincan_<os>_<arch>` (`tincan_linux_amd64`, `tincan_linux_arm64`, `tincan_darwin_arm64`, `tincan_darwin_amd64`), plus the release's `checksums.txt` and a `VERSION` file holding the release number (for example `0.4.0`). The relay serves only those names, and only to joined agents and admin devices. It never needs a restart to pick up a new release.

On each agent's machine:

```bash
tincan upgrade --check   # current version and what the relay has
tincan upgrade           # download, verify sha256, replace the binary
```

`tincan upgrade` picks the build for its own platform, checks its sha256 against the relay's manifest, writes it next to the running binary, and renames it into place (a new file, so macOS never kills a signed binary rewritten in place). If the relay has no build for the platform or the checksum does not match, it stops with an error and leaves the old binary as it was. Afterwards, restart any long-running tincan processes (`tincan wait` or `tincan listen` loops, `tincan mcp` servers); they keep running the old build until then. `tincan upgrade` names each running `tincan mcp` still on the old build with its app's reload step (see [Upgrades](../README.md#upgrades)), and a running `tincan mcp` tells the agent in its next tool result once its binary has been replaced.

`tincan upgrade` trusts the relay host. The checksum and the binary both come from the same relay, so the check protects against a corrupted or truncated download, not against a compromised relay. Only put release files you built yourself or downloaded from your own GitHub release into `--dist`.

### Upgrading the relay

The relay upgrades itself from an admin device, so nobody needs a shell on the relay host:

```bash
tincan relay-upgrade --relay http://tincan-relay --from-github v0.8.0   # fetch the release into --dist, then install it
tincan relay-upgrade --relay http://tincan-relay                        # install what is already in --dist
```

`--from-github` needs the relay started with `--release-url https://github.com/mvanhorn/agent-tincan/releases/download`; without it the relay makes no outbound download. The relay then downloads that release's four binaries and `checksums.txt` from there, checks each binary against that `checksums.txt`, and moves them into `--dist`, `VERSION` last. Then, either way, it checks its own platform's build against the dist `checksums.txt`, writes it next to its running binary, keeps the old one as `<binary>.<old version>`, renames the new one into place, replies with both versions, and restarts. By default it re-executes itself with the same arguments. If a supervisor restarts it, start the relay with `--upgrade-exit` and it exits with status 75 instead (with systemd, use `Restart=on-failure` or `Restart=always`). `tincan relay-upgrade` waits up to a minute for the relay to answer on the new build; `tincan agents` also names the relay's build on its first line.

It refuses a release that is not newer (`--force` reinstalls), a missing build, a checksum mismatch, and a binary the relay user cannot replace, and changes nothing when it does. Self-upgrade needs the relay user to own its binary and the directory it is in; if you keep the binary root-owned, upgrade the relay by hand. Only admin devices and the relay's own admin socket can run it; agents get 403.
