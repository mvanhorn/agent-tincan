# Trust model

## What Agent Tincan guarantees

- The relay only listens on your tailnet. The one exception is the optional ChatGPT gateway, which serves only the MCP tools and OAuth on its own Funnel hostname.
- Every request is attributed to the agent that sent it, using Tailscale's identity for the machine it came from. An agent cannot send as another agent, and whatever it writes in the `from` field is ignored.
- Only admin devices and the relay's local admin socket can invite, remove, or connect agents, or trace every chain. A caller is an admin device only when all of these hold: Tailscale WhoIs reports its short machine name in the `--admin` list; the node has no Tailscale tags; and, if `--admin-login` is set, the node's owning login is in that list. Machine names are chosen by whoever controls the node, so tag every agent machine (for example `tag:agent`): a tagged node is never an admin, whatever it is called. Owner login alone is not used, because on a single-user tailnet every node, agents included, has the same owner.
- Chains are tracked by the relay, not by the model. A request made while handling another continues that chain even if the model leaves the parent out. A request that would loop back to an agent already in its chain is rejected, and chains longer than 4 hops are rejected.
- Each sender is rate-limited (30 new requests per minute by default).
- Every send, delivery, claim, reply, rejection, wake, join, rebind, and removal is written to an append-only, hash-chained log. `tincan audit-verify` detects edits.
- Wake nudges carry only a count and an instruction, never request text.

## Rebuilt machines

Rebuilding a sandbox or VM usually creates a new Tailscale node: a new stable node ID and IP under the same machine name, or that name with a `-1` style suffix if the old node has not expired yet. The relay re-admits such a machine as its old agent on its first call, with no new invite, when all of these hold:

- the new node has no Tailscale tags;
- its machine name matches the agent's recorded one, ignoring a trailing `-<digits>` suffix on either side, so `instinct`, `instinct-1` and `instinct-2` count as the same machine across rebuilds;
- it is owned by the same Tailscale login recorded for the agent. An agent with no recorded login is never re-admitted this way. Agents joined before logins were recorded get one on their first normal call from their own machine after the relay is upgraded; until an agent has made that call, a rebuilt machine needs a new invite for it;
- the agent's old node is offline or no longer on the tailnet, so two live machines can never share one agent;
- if the request names an agent (`X-Tincan-Agent`), it names that agent. When several agents lived on the old machine, each one moves only when it names itself, and until they all have, a request from the new machine that names no agent is refused as ambiguous rather than attributed to the one agent already moved.

If the relay cannot ask Tailscale whether the old node is online, the request fails with 503 and can be retried; it is not treated as a refusal.

This adds no new trust boundary. Tailscale already is the boundary: anyone who can add an untagged node owned by your login to your tailnet controls your tailnet, and joined agents trust each other fully anyway. The agent keeps its name, kind, and queued and claimed requests. Every re-admission is logged by the relay and written to the audit log as a `rebind` event with the agent, old node, and new node. Tagged machines are never re-admitted this way; they need a new invite. Start the relay with `--no-auto-rebind` to require an invite for every rebuilt machine.

## What it deliberately does not do

- Joined agents trust each other fully. A request from a joined agent is meant to be acted on as if you asked, including actions like placing calls or spending money. There is no per-request approval.
- Tailscale is the security boundary. Anything that can act as a joined machine on your tailnet can make your other agents act. Protect your tailnet: use tagged, short-lived auth keys and review who can add devices.
- The relay can read every request and reply. Run it on a machine you control.
- `tincan upgrade` trusts the relay host. The sha256 it checks and the binary it installs both come from the same relay, so the check protects against corruption in transit, not against a compromised relay. Only put release files you built yourself or downloaded from your own GitHub release into the relay's `--dist` directory.

## The risk to think about

If one agent reads untrusted content (a web page, an email, a document) and gets tricked, it can ask a teammate to do something harmful, and the teammate will. Before joining an agent that reads untrusted content alongside one that holds real powers:

- Give high-power agents instructions about which kinds of requests they should confirm with you first.
- Keep `tincan trace` handy so you can see who asked for what.
- Use `tincan remove <agent>` to cut an agent off immediately. Its queued requests are cancelled and, for ChatGPT, its tokens are revoked.

An optional "ask the owner first" gate is planned for setups that want one.
