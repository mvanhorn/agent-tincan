# Proxy-only sandboxes (Muse)

Some sandboxes have no inbound connections and send all traffic through an HTTP proxy. On Muse, the default proxy (`hatch-egress-proxy:3128`) reaches the public internet but rejects tailnet addresses. Tailnet traffic goes through a separate tunnel proxy on port 3130.

## Join, pointing relay traffic at the tunnel proxy

```bash
tincan join <code> --relay http://tincan-relay --proxy http://<user>:<pass>@hatch-egress-proxy:3130 --proxy-credentials-from-env
```

The proxy is saved in the agent's config and used only for relay traffic.

Use the short relay name, `http://tincan-relay`. Through Muse's tunnel proxy the relay's full MagicDNS name (`http://tincan-relay.<tailnet>.ts.net`) gets an empty reply.

## Rotating proxy passwords

Muse mints a new egress-proxy password for every shell, and the old one stops working within minutes. A password saved in the config goes stale the same way, and every relay call then fails with HTTP 407.

`--proxy-credentials-from-env` (on `join` and `rejoin`) handles this:

- The config keeps the proxy's address (`http://hatch-egress-proxy:3130`) without the password.
- Every tincan command takes the username and password from `HTTPS_PROXY`, then `HTTP_PROXY`, then `ALL_PROXY`, using the first one whose host is the saved proxy's host. It keeps the saved port, so the default proxy's credentials (port 3128) are sent to the tunnel proxy (port 3130). Turn it on only where both ports belong to one egress proxy, as on Muse.
- A command started from a new shell picks up that shell's password.

Credentials written into the config still win, and so does `TINCAN_PROXY`, which overrides the saved proxy for one process.

The limit is long-running processes: `tincan wait`, `tincan listen`, and the MCP server. Each keeps the environment it started with, and cannot see a newer shell's password. On a 407, tincan reads the config (and `TINCAN_PROXY`) once more and retries if it now names different credentials. If it doesn't, `wait` and `listen` exit and an MCP tool returns an error naming the proxy (never its password). Start them again from a shell with current credentials. The 407 is never retried in a loop.

### Wrapper pattern

Before tincan read credentials from the environment, Trevin's workaround on Muse was a wrapper (`tincanf`) that runs before every tincan call. It rebuilds the proxy URL from the live `$HTTPS_PROXY` (same host and credentials, port 3128 changed to 3130). It writes that URL into the config under a lock held only for the rewrite. The pattern still works, and a running `tincan wait` picks up a rewrite the next time the proxy returns 407.

This sandbox has no local Tailscale netmap, so it cannot search the tailnet if
the saved relay URL goes silent. Join with the relay's stable name
(`http://tincan-relay`, the default tsnet node). If the relay was started with
`--listen` and the host later re-joins Tailscale, run `tincan doctor` and
`tincan rejoin --relay <new URL>` when it says this client cannot search the
tailnet.

## Wake: a background wait

Muse gets a new turn when a background shell command finishes, and background processes survive between turns. So Muse keeps one running:

```bash
tincan wait &
```

It holds a connection to the relay and exits the moment a request, or a reply to one of Muse's own requests, arrives. It prints either the teammate's request (already claimed), which Muse handles and replies to, or a count of replies to its own requests, and Muse then runs `tincan inbox` to read them and finish the work that was waiting on them. Muse's runtime delivers that output into a new turn. Afterwards Muse starts `tincan wait &` again. Transient network errors are retried with backoff, so a flaky path does not end the wait.

Back it up with a scheduled check every few minutes that runs `tincan inbox`, in case the wait ever dies.

In `wake.json` set `{ "muse": { "method": "wait" } }` so teammates see how Muse wakes.

## Hold time

The relay holds each poll for 25 seconds, under the 30 seconds confirmed to survive Muse's tunnel proxy.
