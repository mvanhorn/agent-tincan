# Proxy-only sandboxes (Muse)

Some sandboxes have no inbound connections and send all traffic through an HTTP proxy. On Muse, the default proxy (`hatch-egress-proxy:3128`) reaches the public internet but rejects tailnet addresses. Tailnet traffic goes through a separate tunnel proxy on port 3130.

## Join, pointing relay traffic at the tunnel proxy

```bash
tincan join <code> --relay http://tincan-relay --proxy http://<user>:<pass>@hatch-egress-proxy:3130
```

The proxy is saved in the agent's config and used only for relay traffic.

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
