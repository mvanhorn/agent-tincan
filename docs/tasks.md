# Task deadlines, briefings and chat delegation

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

A relay with a named task host sends a briefing at 07:00 in the task time zone by default. It includes up to three tasks due today or overdue, tasks waiting on you, work completed since the last automatic briefing, one suggested task and unconfirmed news from the past day. The relay skips an empty briefing.

Set `--task-briefing HH:MM` and `--task-briefing-zone <IANA-zone>` on the relay to change the schedule. Use `--task-briefing off` to turn off the daily briefing. On an admin device, `tincan tasks briefing --now` queues the current briefing without changing that schedule.

## Work from private chat

Name the teammate you already talk to with `tincan tasks host set <agent>` on an admin device. In its private chat, try “Call the dentist Friday at 3pm” to file a task, “give 12 to muse” to assign one, or “make 12 due tomorrow” to change a deadline. The host can file a clean title, detail and deadline, optionally handing the work to a teammate in the same call.

For assignment and changes to existing work, the host uses `decide_task` and keeps your exact words in the task history. The same operations are available from the host's shell:

```bash
tincan tasks decide 12 assign --to muse --words "give 12 to muse"
tincan tasks decide 12 due tomorrow --words "make 12 due tomorrow"
```

The host acts on your direct words. You approve outside-source tasks and reset assignment counts on an admin device. A teammate's reply returns a task for your decision. For host setup, see the [Grok Bot adapter](adapters/grokbot.md#keep-your-task-list-in-grok-bot).

## Configure a fallback

Reminders and briefings carry task numbers, statuses, deadlines and agent names. The host uses `get_task` when you ask for details. It posts the exact `owner_message` lines in your private chat and records the acknowledgment immediately before posting them.

When a host misses an acknowledgment, the relay tries up to three times. On exhaustion, it queues a notice for the `notify` agent in the relay's `approval.json`. Choose a joined agent with a wake method that can reach you. For an email-woken agent, its email prompts it to read the relay inbox and pass on the notice.

```json
{
  "gate": {},
  "notify": "instinct"
}
```

Add `notify` to the existing policy and keep its other settings. Store the file in the relay's state directory with mode `0600`. The relay logs a startup warning when task hosts have no fallback destination. `tincan doctor` reports the same check from an admin device.
