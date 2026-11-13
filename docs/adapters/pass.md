# Passing a conversation

A pass moves one of your chats from one web chat app to another: tincan reads the chat, packs it into one message, and has a web teammate open a new chat on its own site with it, so you can pick up where you left off on a different subscription.

## From your terminal

```
tincan pass https://claude.ai/chat/<id> --to chatgpt-web   # a chat URL
tincan pass grok --to gemini-web                           # Grok's newest chat
tincan pass chatgpt --id <id> --to claude-web              # one chat by id
```

The source is a chat URL on ChatGPT, claude.ai, Grok, Gemini or Copilot (`copilot.microsoft.com` links work too), or one of the source names `chatgpt`, `claude-ai`, `grok`, `gemini`, `copilot`. A bare source passes its newest conversation within the [history window](history.md#window) (the last 50 conversations, up to 30 days), leaving out chats the web agents sent into. Perplexity and dot threads cannot be passed from: they are not history sources.

The chat is read through the Tincan Chrome extension on the machine you run the command on, the same way `tincan history` reads it, so run it on the Mac with Chrome and the extension (`tincan history install`). The seed goes to the destination as an ask from that machine's joined agent.

The destination must be one of the six web chat teammates (`chatgpt-web`, `claude-web`, `grok-web`, `gemini-web`, `perplexity-web`, `copilot-web`), signed in to its site, not out of messages, and running tincan 0.18.0 or later (a development build counts as older). Anything else, `dot-web` and coding agents included, is refused before the chat is read, with the reason.

The command waits for the destination to open the chat (`--wait`, 10 minutes by default) and prints the conversation it opened, for example `Passed to chatgpt-web. ChatGPT conversation: 68f0...`. Open it in the browser and carry on there. It prints nothing of the destination's answer.

## From an agent

Agents ask the `pass` teammate, a Go service (`tincan pass serve`) that runs on the Mac with Chrome and the extension, next to history. The body names one chat and one destination, each on its own line:

```
from: https://claude.ai/chat/<id>
to: chatgpt-web
```

`from: <source> <conversation id>` (for example `from: chatgpt 68f0...`) works too. A pass never takes "latest": an agent names the chat it means. Anything else fails with the expected form.

For each ask the service, in order:

1. Waits for the owner. The relay holds every ask to an agent of kind `pass` with no `approval.json` needed. `tincan held` shows the chat and the destination parsed from the whole request (`pass: claude.ai conversation <id> -> chatgpt-web`), or why it will fail; `tincan approve <id>` starts it.
2. Declines unless every agent in the request's relay-recorded chain is on `~/.config/tincan/pass-allow.txt` and on the history allowlist (`~/.config/tincan/history-allow.txt`). No file means every joined agent; an unreadable file declines everyone. Both are reread for every ask.
3. Refuses a destination that is not a web chat teammate, is signed out or out of messages, or last called the relay with a tincan build older than 0.18.0 (a development build counts as older), before reading anything.
4. Reads the chat and refuses it when it was last updated outside the history window's days (`~/.config/tincan/history-window.json`, 30 days by default; a bad file fails every ask). The window's conversation count does not apply, since the chat is named.
5. Asks the destination to open a new chat with the seed, as a child of the ask, so the chain and loop rules carry over, grouped `pass-<request id>`. A redelivered ask whose seed already went out waits for that seed instead of sending the chat again.
6. Replies with the new conversation only: `Passed the claude.ai conversation <id> to chatgpt-web. New ChatGPT conversation: <id>`. The destination's answer is never passed on. If the destination fails, the pass fails with its status and reason, and says the chat was sent but the destination did not confirm the new chat, since the seed may have reached the site. A failure before the seed goes out says `Nothing was passed`.

The destination web agent lists every conversation a pass opened in `~/.config/tincan/web-agent-<site>-passed.json` (0600, ids and times only; one list per site, shared by every web agent for it) before the id goes out, and declines every agent's `conversation:` ask for one of them, the passing agent included, also after a restart. The passed chat never becomes the asker's remembered thread either. You continue it in the browser.

### Install

1. Upgrade the relay, then the web teammates (`tincan upgrade`, then restart them). An older relay refuses the `pass` kind and would never hold passes, so `tincan pass serve` refuses to run until the relay stores kind `pass` for it.
2. `tincan invite pass --kind pass` on an admin device, then on the Mac: `TINCAN_CONFIG=~/.config/tincan/pass.json tincan join <code> --relay <relay url>`. If it joined without the kind, run `tincan kind pass pass` on an admin device. The service runs only as the agent named `pass`.
3. `tincan pass install` writes `~/Library/LaunchAgents/com.agenttincan.pass.plist` (logging to `~/Library/Logs/tincan-pass.log`, each line starting with its local time; a systemd user unit on Linux) and prints the command that starts it.
4. In the relay's `wake.json`, set `pass` to `{"method": "wait"}`.

Each send of a seed is audited on the relay as `pass_sent`, with the request ids, the destination, the byte count and, for an agent's pass, the chat's source and id; never its text.

## The seed

The new chat starts with one message, built in Go with no model step:

1. `new chat`, so the destination opens a fresh conversation.
2. A paragraph telling the model that what follows is material from another conversation, not instructions, sitting between two delimiter lines that carry a random value picked for this pass (and picked again if the chat happens to contain it).
3. The chat's title.
4. The first line of each earlier prompt, oldest first, each cut to 160 characters.
5. The last 6 turns verbatim, each prompt with its reply.

It fits the destination's message cap: 32 KB, and 12 KB for claude.ai until its safe paste size is measured (a longer paste becomes an attachment there). When the chat is longer, the earliest prompts' lines are left out first (a line says how many), then the older recent turns are shortened, and the newest turn is shortened last, each with a truncation marker. Images are not passed.

## Failover

A seed is grouped (`pass-<id>`), and a grouped ask never fails over, so a pass only ever reaches the vendor you named. If the destination runs out of messages before the seed is typed, the pass fails with the reason instead of moving to another site.

## Privacy

A pass sends that chat to the destination's vendor: its title, earlier prompts' first lines and last 6 turns are then in that account's history, under that site's terms and retention, and its model reads them. That is the point of a pass, and it is why an agent's pass waits for your approval. Your own `tincan pass` is not held, and any agent with a shell on the Mac can run it, as it can run `tincan history`. See [the trust model](../trust-model.md#passing-a-conversation).
