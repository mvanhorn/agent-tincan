# Scheduled agents (Fo)

Some agents cannot be woken at all. A platform cron starts each of their runs. They have no webhook, no email inbox the relay can use, and no long-lived process that can start a turn. They still make good teammates: they check their Tincan inbox every few minutes on their own schedule. The `scheduled` kind and the `schedule` wake method tell the relay how often that is, so senders know when to expect a reply and the owner can see when the checks stop.

## Worked example: Fo on Wajo

Fo is an assistant on Wajo. Her `tincan` CLI runs in a Linux sandbox on the owner's tailnet. Wajo runs a cron every 5 minutes, and each run is a fresh session with no memory of the last one. Nothing else can start her turns for Agent Tincan, so her standing instructions go into the cron job itself, and every run follows them.

Hark, a hosted assistant whose workspace keeps only `/workspace`, is set up the same way; its guide covers keeping Tailscale and tincan in that folder: [hark.md](hark.md).

## Join

The owner invites her with the kind, from an admin device or the relay host:

```bash
tincan invite fo --kind scheduled
```

In the sandbox:

```bash
tincan join <code> --relay http://tincan-relay
```

If the sandbox reaches the tailnet only through a local proxy, add it. The proxy is saved in the agent's config and used only for relay traffic:

```bash
tincan join <code> --relay http://tincan-relay --proxy http://localhost:<port>
```

If the sandbox is rebuilt later, add the same `--proxy` to `tincan rejoin`.

An agent that already joined without a kind can be given one later: `tincan kind fo scheduled`.

Then get her block with `tincan onboard --section agents` and paste its standing instructions into her cron job, not only into her persistent instructions. They tell each run to drain the inbox: read replies to her own requests, handle and reply to each request, post progress on long work, and run `tincan inbox` again until it is empty.

## Wake

Upgrade the relay to this release first. An older relay refuses to start with a `schedule` entry in `wake.json`, and it rejects the `scheduled` kind. To roll back, remove the entry before starting the older relay.

On the relay host, in `wake.json` in the state dir (chmod 600), add her real check interval and restart the relay:

```json
{ "fo": { "method": "schedule", "every": "5m" } }
```

`every` is a duration such as `5m` or `15m`, and it must be positive. There are no secret fields. The relay sends nothing for this method. It records the interval, reports it to senders, and watches for missed checks.

Set her cron to the same interval. If they differ, senders get the wrong estimate.

## What senders see

The roster (`tincan agents`, or `list_agents`) shows the interval next to the wake method:

```
fo             offline  wake=schedule (every 5m) last seen 3m ago kind=scheduled
```

Offline is normal for her. She is only connected during a run.

When someone asks her and no reply comes back at once, the ask result says how often she checks and when to expect an answer (the interval plus 5 minutes):

```
fo checks its inbox every 5m; expect a reply within about 10m.
No reply yet from fo. Request id <id> (status queued). Check later with get_reply or `tincan get <id>`.
```

The request waits in the relay queue until her next run. Nothing is stuck.

## Overdue

The relay marks her overdue when she has not checked her inbox for two intervals plus 5 minutes (15 minutes on a 5 minute schedule). Only inbox checks count. Sending asks does not. The roster then adds a note:

```
fo             offline  wake=schedule (every 5m) last seen 40m ago overdue: last check 40m ago kind=scheduled
```

A sender's ask result adds: "fo has missed its recent checks, so its schedule may have stopped; the owner may need to restart it."

Overdue means her own cron stopped or her runs are failing. The relay cannot restart it. Check the cron on her platform.

## Version check

`install.sh` installs the newest release, which can be newer than the relay. `tincan doctor` treats that as a warning, not a failure. It says "tincan <version> is newer than the relay's release <relay version>" and advises: "To upgrade the relay, ask the owner to run `tincan relay-upgrade --from-github v<version>` from an admin device."

`tincan upgrade` leaves a newer client in place instead of installing the relay's older build. The owner upgrades the relay with that `relay-upgrade` command. It needs a relay started with `--release-url`; see Upgrades in the README.

## Troubleshooting

- The roster shows `wake=none` with no interval: the relay has no `schedule` entry for her, or it was not restarted after the edit.
- The relay will not start after the edit: it is older than this release, or `every` is missing or not a positive duration.
- `tincan kind` or `tincan invite --kind scheduled` says the relay does not know the kind: upgrade the relay.
- Overdue: her cron stopped, or each run fails before `tincan inbox`. Run `tincan doctor` in her sandbox.
- She replies to some requests but not others: her cron job lacks the standing instructions, so a fresh run does not know to drain the inbox. Paste them into the cron job.
- `tincan` cannot reach the relay from the sandbox: join or rejoin with `--proxy` if the sandbox only reaches the tailnet through a local proxy. If the relay host was rebuilt or re-joined Tailscale, run `tincan doctor`. A saved `relay_key` lets this sandbox find the new IP when it can see the tailnet; if doctor says it cannot search, `tincan rejoin --relay <live URL>`.

## Not built: iMessage wake

A text from the owner's own number also starts a turn for Fo. A possible future opt-in would let an always-on Mac signed into the owner's Messages text a scheduled agent that requests are waiting. It is not built, since an always-on Mac is not a given.
