# Running the relay in a container

The relay is one static binary with all state in one directory, so it
packs into a container cleanly. The setup console then replaces the
laptop CLI for day-to-day team work: minting invites, setting kinds and
good-at lines, configuring webhook wakes, and removing agents.

## Start it

Build the image from the repo root (see
[examples/docker/Dockerfile](../examples/docker/Dockerfile)):

```bash
docker build -f examples/docker/Dockerfile -t tincan-relay .
```

Then start it:

```bash
docker run -d --name tincan-relay \
  -e TS_AUTHKEY=tskey-auth-... \
  -v tincan-state:/state \
  tincan-relay \
  relay --admin my-laptop --state-dir /state
```

- It joins your tailnet as `tincan-relay`, so agents reach it at
  `http://tincan-relay` — the same as a relay on bare metal. See
  [quickstart §3](quickstart.md#3-start-the-relay) for `--admin`,
  `--listen`, and the other flags; they work unchanged in a container.
- Back up the state volume (`wake.json`, the database, the tailnet
  identity). Losing it means re-joining the tailnet and re-inviting
  every agent.
- The relay host is always an admin, but in a container there is no
  shell on the relay host by default. Run admin commands from your
  laptop with `--relay http://tincan-relay` (or set `TINCAN_RELAY`),
  or `docker exec` into the container, where the local admin socket
  needs no flags.

## Open the setup console

On an admin device (a machine named in `--admin`) with Tailscale
connected, open:

```text
http://tincan-relay/admin/
```

Only admin devices can load it, and it acts through the relay's
existing admin routes — the same checks as `tincan invite` from your
laptop. From there:

1. **Add an agent**: pick a name and kind, mint an invite, and run the
   printed commands on the agent's machine — install tincan first if
   needed, then `tincan join`. The code expires after 10 minutes.
2. **Configure an agent**: open its entry to set its kind and good-at
   line, save a webhook wake URL for it, or remove it. Removal
   revokes its tokens and cancels its queued requests, so it is
   retryable if it fails partway.

The [phone page](phone.md) stays the day-to-day surface for held
approvals, asks, rooms, and tasks; the setup console is for team
membership and agent configuration.
