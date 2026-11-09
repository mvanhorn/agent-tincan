# A2A

[A2A](https://a2a-protocol.org) is an open protocol for agents to call each other. Agent Tincan can publish chosen teammates to A2A 1.0 clients, so someone outside your team can ask them from a client on the open internet.

An A2A client is always a guest. It gets a guest's limits and nothing more: it asks only the teammates you list, its asks wait for you, and its time ends when you said. Read [Guests](trust-model.md#guests) and [A2A](trust-model.md#a2a) in the trust model first.

This has been tested against the in-process test relay only, not with a real A2A client or over a live Funnel.

## Turn it on

It stays off unless you do all three.

1. Start the relay with the public gateway (`--chatgpt-gateway`), so it has a public hostname.

2. Write `a2a.json` in the relay's state directory, mode 0600. It names the teammates that get a public card, and the description that card shows:

   ```json
   {
     "teammates": [
       {"name": "muse", "description": "Writes and edits. Answers can take a while."}
     ]
   }
   ```

   Anyone can read these descriptions, with no login. Listing a teammate here gives nobody access to it. The relay reads the file on every call, so an edit takes effect at once, and removing the file turns A2A off. A file with a mistake in it, or that other users can read, publishes nothing, and the relay logs why.

3. Connect a guest for the client:

   ```bash
   tincan connect pal --guest --ask muse --for 30d
   ```

   The term is 30 days at most. The command prints the agent card URL for each teammate and a one-time code, good for 10 minutes. Give both to the client's owner. It warns you if a listed teammate is not in `a2a.json`, and if nothing would tell you about a held ask.

The client needs both things: a teammate must be published in `a2a.json` and listed in `--ask`.

## What the client does

It reads the card, signs in with OAuth (authorization code with PKCE, and dynamic client registration) by entering the code on the login page, then sends `SendMessage`, polls `GetTask`, and can call `CancelTask`. See the [protocol](protocol.md#a2a) for the wire.

Each ask waits in `tincan held` for you, as a guest's does. The task stays in `TASK_STATE_SUBMITTED` until you approve it and the teammate picks it up. If the teammate needs input, the task goes to `TASK_STATE_INPUT_REQUIRED`, and the client's next `SendMessage` with that `taskId` is the answer. That answer is not held, and what the teammate sends next is, until you approve it.

## What it does not do

- No streaming, no push notifications, no gRPC or HTTP+JSON, only JSON-RPC.
- Text only. A file or data part is refused, and a teammate's attachments do not reach the client.
- A client with no browser or person to complete the login is not supported.
- `SendMessage` waits 25 seconds at most, and then returns the state so far, even though the spec has it wait until the task ends or needs input. A task can wait days for you, so a client must poll `GetTask`.
- Each task stands alone. A new message's `contextId` is ignored, and a teammate sees no earlier task. Only a task waiting for input can be continued.
- A retried `SendMessage` is a new ask.
- A client can cancel a task only until a teammate has been given it, or claimed it in the past. A task that went back to the queue after a claim or an answer cannot be cancelled.
- A gateway guest cannot use the MCP tools.

## Ending it

- `tincan remove pal` ends the guest at once and frees the name. Its tokens stop working and its open tasks are cancelled.
- At the end of its term every call is refused, then the sweep revokes its tokens and cancels its open tasks.
- To give a guest a new code, remove it and connect it again. `connect --guest` takes a free name only.
- Removing `a2a.json`, or a teammate from it, closes that card and endpoint at once. Clients may keep a copy of a card for 5 minutes.
