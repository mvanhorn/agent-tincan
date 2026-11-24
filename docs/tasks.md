# Task deadlines, briefings and chat delegation

## Put asks on the list

When you name a task host with `tincan tasks host set <agent>`, the relay records each ask between teammates as a task when it sends the request. The first line becomes the title and the full message becomes the detail. The asking agent files it and the receiving agent is assigned. The sender sees the task number. Asks and replies still flow directly between agents on the same request ID, even when the host is offline.

An answer returns the task with its result. A request for clarification puts the task at question. If an attempt ends without a result, the task reopens and the host looks for a way to finish it. If the sender cancels an untouched auto-filed ask, the relay drops the task and records why. The owner or a host acting on the owner's direct words can mark a task done while its ask is open. The relay answers the asker with the completion note and releases the claim.

For a follow-up, run `tincan ask <agent> "<message>" --about <task-number-or-request-id>` or set `about` on the MCP `ask` tool. The relay can also recognize a single accessible live request ID or a task number written as `task #N` in the message. An ask sent while handling a claimed task attempt uses the verified claim when no other task is named. The follow-up and its reply go into the parent task's history. Use the original ask to deliver the task result. Closing or dropping the parent cancels any follow-ups still open under it. Explicit task assignments and repeats already have a task. Notifications, pings, asks from the relay itself, and packed chat transfers stay off the list. Guest and outside-source asks keep the proposed-task recovery path if they end without a result.

Notes, history, pass and council asks, room mentions, follow-on hops, and scheduled asks start as quiet tasks. A normal answer records the result and closes the task automatically. An acknowledgment that leaves work pending is kept for the real answer. Quiet tasks stay out of the default list and send no status notice. To look up a quiet task, run `tincan tasks --all` or call `list_tasks` with `include_quiet: true`. Add `--closed` or set `closed: true` to include completed checks. Use `tincan tasks show <id>` or `get_task` for the result and history. Service task text stays with the original requester and recipient. Other hosts see the task status. Room access and approval rules still apply. Follow up on a room ask in its room.

A quiet task appears on the ordinary list if its ask fails, is declined, expires, or spends four hours waiting for clarification. The host gets an action notice when a teammate attempt fails or expires, or a clarification is overdue. An escalated question stays open for the asker to answer. The briefing counts quiet answers since the last scheduled briefing and keeps their task details out.

Use `tincan ask <agent> "<message>" --quiet` or set `quiet: true` on the MCP `ask` tool to file a housekeeping ask as quiet. Examples include upgrades, health checks, config confirmations and corrections to an earlier reply. Never use quiet for work the owner asked for. Quiet asks follow the usual approval and task access rules. Older relays ignore the option.

On the relay, `--file-all-asks auto` is the default and follows whether a task host is named. Use `--file-all-asks on` to file even without a host, or `--file-all-asks off` to disable filing at send. The existing recovery of eligible unfinished asks continues in every mode.

## Set a deadline

```bash
tincan tasks add "Call the dentist" --due "Friday 3pm" --owner
tincan tasks due 12 tomorrow --owner
tincan tasks due 12 clear --owner
```

Use `--owner` on an admin device. A teammate can set or clear a deadline on a task it filed without that flag. In MCP use `add_task` with `due`, or `update_task` with action `due` and a `when` value.

A weekday, `today`, `tomorrow`, or a date such as `2026-11-27` sets a due day. Add a time for a reminder, or give `in 2h` or an RFC 3339 timestamp. The relay's `--task-zone` defaults to `America/Los_Angeles`. The CLI and MCP use that zone even on a machine set to UTC. Overdue and due-today tasks come first in the list.

A due day alone appears on the list. A timed deadline sends one reminder to named task hosts. Closing the task or changing its deadline cancels outstanding reminders, and delivery stops six hours after the deadline.

## Get a morning briefing

A relay with a named task host sends a briefing at 07:00 in the task time zone by default. It includes up to three tasks due today or overdue, returned results to review, issues needing input, open or working tasks with what is holding them up, recent completions, one suggested task, and unconfirmed updates. Unfinished tasks come back in each briefing until they are done or dropped. The relay skips an empty briefing. A newly queued briefing replaces any older briefing still awaiting delivery or acknowledgment.

Set `--task-briefing HH:MM` and `--task-briefing-zone <IANA-zone>` on the relay to change the schedule. Use `--task-briefing off` to turn off the daily briefing. On an admin device, `tincan tasks briefing --now` queues the current briefing without changing that schedule.

## Work from private chat

Name the teammate you already talk to with `tincan tasks host set <agent>` on an admin device. In its private chat, try “Call the dentist Friday at 3pm” to file a task, “give 12 to muse” to assign one, or “make 12 due tomorrow” to change a deadline. The host can file a clean title, detail and deadline, optionally handing the work to a teammate in the same call.

For assignment and changes to existing work, the host uses `decide_task` and keeps your exact words in the task history. The same operations are available from the host's shell:

```bash
tincan tasks decide 12 assign --to muse --words "give 12 to muse"
tincan tasks decide 12 due tomorrow --words "make 12 due tomorrow"
```

The host acts on your direct words. You approve outside-source tasks and reset assignment counts on an admin device. When a returned result fully answers the request, the host closes the task and tells you the answer and task number in one sentence. The host uses `decide_task` with `decision: "done"` and a one-line `reason`. You can reopen the task if it needs more work.

## Follow through on unfinished work

After 15 minutes without new information on a claimed task, the relay nudges the assignee once. After 30 minutes it tells the host to check the task and act. Repeating the same update, even with different timestamps, keeps the claim alive and leaves the follow-through timer running. A substantive update restarts the timer. Set `--task-nudge-after` and `--task-stall-after` on the relay to change the intervals.

When a task host returns a result to another agent, the relay waits an hour for the asker to follow up. A follow-up ask linked to the task or a comment from the asker restarts the timer. Once the window passes, the relay asks an eligible host to review the result and close the task. If no host can act, the relay marks the task done and keeps the result in its history. Set `--task-returned-close-after` on the relay to change the one-hour window.

The host checks the history and confirms the old attempt is safe to replace. It can use `decide_task` with `decision: "retry"`, `to`, and a one-line `reason` describing a different approach. The relay allows two such retries and applies the usual send approvals. If the host does the work directly, it records the verified outcome with `decision: "complete"` and `result`. The shell equivalents are `tincan tasks decide <id> retry --to <agent> --reason "<different approach>"` and `tincan tasks decide <id> complete --result "<verified outcome>"`. It asks you when the next step requires a decision, such as spending or booking, or when the retries have failed. For host setup, see the [Grok Bot adapter](adapters/grokbot.md#keep-your-task-list-in-grok-bot).

## Configure a fallback

Task notices to a host include a short title when that host can read it at poll or claim time. The relay stores the notice as metadata. Room, service and withheld text stay out of notices. The host uses `get_task` to check the current task, takes the appropriate action, and gives you a plain-language update when it has a result or needs your input. It acknowledges notices it handled or is about to post. Fallback alerts carry status metadata only.

When a host misses an acknowledgment, the relay tries up to three times. After the last attempt, it queues a notice for the `notify` agent in the relay's `approval.json`. Choose a joined agent with a wake method that can reach you. For an email-woken agent, its email prompts it to read the relay inbox and pass on the notice.

From an admin device or the relay's local admin socket, run:

```sh
tincan relay notify instinct
```

Run `tincan relay notify --clear` to remove the destination. The relay saves the change in `approval.json` and keeps existing gate rules and hold settings. The relay logs a startup warning when task hosts have no fallback destination. `tincan doctor` reports the same check from an admin device.
