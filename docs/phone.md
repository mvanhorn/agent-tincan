# Tincan on your phone

The relay serves a page for your phone at `/phone/`. It shows the team as `tincan top` does, with the same status for every agent, and lists the asks held for your approval with their sender, target and attachment names. You approve or deny a held ask from the page, and once the phone joins as its own agent you can ask any teammate and read the answer there. The Tasks tab shows the team's task list and lets you add, assign and close tasks.

The page works only from an admin device: a machine named in the relay's `--admin` list, untagged, and (with `--admin-login`) owned by a listed login. Any other machine gets 403 for the page and every file it loads. Nothing is hosted anywhere else; the page talks only to your relay over your tailnet.

## Open it

1. Add the phone to the relay's admin list. Find its machine name with `tailscale status` (for example `iphone182`) and restart the relay with `--admin macbook-pro-44,iphone182`. The phone must be signed in to Tailscale as you and must not carry a tag.
2. On the phone, with Tailscale connected, open the page the relay logs at startup:
   - with HTTPS on your tailnet: `https://<relay>.<tailnet>.ts.net/phone/`
   - without it: `http://<relay>/phone/` (the relay's own address, as `tincan agents` reaches it)
3. Add it to the home screen. In Safari, tap Share, then Add to Home Screen. In Chrome on Android, tap the menu, then Install app.

Open the address exactly as the relay logs it, ending in `/phone/` (`/phone` without the slash redirects there). The relay refuses requests addressed to any name that is not its own.

## HTTPS

When your tailnet has HTTPS certificates turned on (Tailscale admin console, DNS page, HTTPS Certificates), the relay also listens on port 443 under its MagicDNS name and logs `phone page: https://.../phone/`. With the default tsnet relay this is its own node; with `--listen` it binds port 443 on the `--listen` address and uses the host's certificate, so the relay user needs permission to bind that port.

When the API itself is on port 443 (`--port 443`, or `--listen` with `:443`), the relay opens no HTTPS listener, since it would collide with the API's. It logs `no HTTPS: the API listener already uses port 443 ...` and the page works over plain HTTP on port 443, without alerts. Use another API port to get HTTPS.

Without certificates the relay logs why (`no HTTPS: ...`) and the page works over plain HTTP on the relay's usual address. Plain HTTP inside the tailnet is still encrypted by WireGuard. The page is the same either way, except that alerts need HTTPS.

A `tailscale serve` front is not supported: the relay would see every request come from the host itself and could not tell which device is asking.

## Asking from the phone

The Ask tab, beside Team under the held list, lets you ask any teammate from the phone. The phone asks as its own agent, so every ask is attributed to a joined agent like any other.

1. On the Ask tab, tap Join this phone. The name defaults to `phone`. The page mints an invite with kind `phone` and joins with it at once, from the phone itself; there is no code to copy. It refuses a name that already belongs to another agent, or to a phone agent other than this phone's own, since an invite for an existing name would move that agent to this phone. The relay checks the name again when the page mints the invite (`new_only`) and once more when the page redeems it, so a phone that joined under it since the page last read the roster, even between mint and join, is refused too. Teammates cannot send to a phone agent: the relay refuses with `phone agents ask; they do not take requests`.
2. Pick a teammate, write the ask and tap Send. Held asks (to council, dot-web or duet-web, or under `approval.json`) show as held for your approval, and you approve them in the held list like any other.
3. Your asks shows the 20 most recent, rebuilt from the relay each time the page opens: waiting, working on it (with the latest progress note), held for your approval, needs your input (with a box to answer), answered (with the answer), could not do it, or declined.

Replies and questions are teammates' text and are shown as text only. The phone's agent has wake method `none` and the stock line "the owner's phone; asks only, never answers": it never answers, so teammates should not ask it. For the same reason a phone agent is never a target when work moves off a limited teammate, and never an @mention target in a room, though it may join a room to read and post.

Before approving a held ask that this phone's own agent sent, or that came through it, the page asks you to confirm, so one tap cannot both ask and approve.

If the phone's Tailscale node is replaced, the page says it is not joined; tap Join this phone again with the same name. The page remembers its agent's name on the phone for this. If the phone's site data was cleared, the page no longer knows the name and refuses it; run `tincan remove <name>` on an admin machine first, or pick a new name.

## Rooms from the phone

The Rooms tab lists every room and lets you create one, add members and remove them, as `tincan room` does from an admin device. A room you create from the phone gets the phone's agent as a member, so you can take part. Before adding a web chat agent or ChatGPT or Sesame, the page asks you to confirm, since everything that member reads then goes to its vendor; when the relay refuses a member (council, dot-web, an `approval.json` target, or the history, notes and pass services) the page shows why.

Pick a room to read it. The page reads and posts as the phone's agent, so the phone must be a member; it checks for new posts every 5 seconds while the tab is open. Each post shows its author, time and whether it is a question, a failure or a decline, and a mention shows who it is still waiting on. An `@name` of a member in your post asks that member, and its reply shows in the room; the page lists any mention that was not sent and why. Posts are teammates' text and are shown as text only.

## Tasks from the phone

The Tasks tab shows the team's shared task list (see [Tasks](../README.md#tasks) in the README): every task, since the page is an admin client. The tab's count is the number of tasks waiting on you: proposed ones, questions for you, and returned ones.

A teammate you name a task host (`tincan tasks host set <agent>`, see [Run the list from an agent](../README.md#run-the-list-from-an-agent)) can show you the same list in its own chat, hand tasks out there, and mark a task done, reopen it or drop it when you ask it to. Its history entry names the host and quotes your words, and Reopen here undoes it, putting the task back where the host found it. Approving a proposed task and resetting an assign count stay on this page and the CLI.

To add a task, write a title and what is wanted, pick who it is for (no one, you, or a teammate) and tap Add task. Handing a task to a teammate sends an ask as the phone's agent, so the teammate choices need the phone joined; without that the page says so beside Join this phone. Adding an unassigned task or one of your own needs only the admin device. The page does not make repeating tasks; a task that repeats, added with `tincan tasks add --every` or `add_task`, shows its interval and teammate.

Tasks are grouped, each group with a count:

- Proposed: tasks waiting for your approval, with why (the source the filer declared, or that it was filed while the filer was handling a guest's ask) and the teammate the filer suggested. Nothing was sent to that teammate. Approve opens the task and offers to assign it, with the suggestion marked; Drop gives it up.
- Needs your answer: a task whose assignee asked a question on an ask this phone's agent sent, with a box to answer. The answer goes on the same ask and the task goes back to working. A question on an ask another agent sent stays under Working and names who it waits on.
- Returned: tasks with a result nobody has acted on. Done closes one, and Assign again sends it out for another attempt, which carries the result and the comments made since.
- Working: tasks with an ask open, and where that ask stands: held for your approval, waiting to be picked up, or picked up.
- Open: tasks with no ask under them and no assignee.
- Yours: open tasks marked as your own to do.
- Done and Dropped: closed tasks, folded away behind their counts. The page reads only the counts until you open one of the two, so a long history costs the page nothing; it then reads the closed tasks, and keeps them current while either is open. Reopen puts a task back to open, or to proposed when it was never approved. Past 10,000 tasks in all, the page shows the oldest closed ones and says how many; `tincan tasks --owner --closed` lists every one.

Tap a task to open it. It shows what is wanted, the latest result, the history (the latest 5 entries, with the older ones behind a control) and a comment box. A comment wakes nobody; the next attempt at the task carries it. The buttons depend on the task's status:

- Assign lists the teammates that can take a task, each with its status and good-at line as the Team tab shows them. Guests, phones and history, notes, pass and council agents are left out. On a task someone already has the button says Reassign: the task is taken from them and their ask cancelled.
- Marking a task done ends its open ask and tells the asker. Drop the task, which cancels the ask, or wait for the ask to end.
- Reset assign count shows once agents have assigned the task, with how many times. Agents may assign one task 5 times until you reset it.

Approve, Done, Reopen, Drop and Reset each use a one-time code from the relay, as Approve and Deny on a held ask do: the task list the page reads carries one code for each decision a task's status allows, good only from this device, for that task and decision, for 10 minutes. A decision that lands ends the task's other codes, and the page reads the list again after every decision. When a code has expired the relay says so and the next read replaces it. Adding, commenting and assigning need no code.

Task titles, details, results, comments and history are teammates' text and are shown as text only. The page does not send alerts for tasks: an alert is sent only for a held ask, which includes a task's ask when your rules hold it.

## Alerts

With HTTPS, the page can alert you when an ask is held. The row under the team says which state applies:

- Alerts need HTTPS: the page was opened over plain HTTP. Turn on HTTPS certificates for the tailnet; until then the approval policy's `notify` teammate is the alert path.
- Add to Home Screen first: on iPhone, Safari only allows alerts from a page opened from the Home Screen.
- Turn on alerts: tap it and allow notifications. The page asks only when you tap.
- Alerts are on: this phone gets an alert for held asks. Turn off removes its subscription from the relay, and the phone remembers the choice: opening the page again shows Turn on alerts rather than subscribing anew.
- Alerts are blocked: notifications for Tincan are off in the phone's settings.

An alert says only "A request is waiting for your approval" (or how many are). It never carries the request text, who asked or whom. Asks held close together share one alert. Tapping it opens the page at the held list. The alert goes from the relay through your phone's push service (Apple, Google, Mozilla or Microsoft), encrypted to the phone; see [the trust model](trust-model.md). The page sends its subscription again every time it opens (unless alerts were turned off on this phone), so a phone that was offline or reinstalled picks alerts back up.

## What the page shows

- Held for approval, at the top, with a count. Each held ask shows who asked whom (with the target's kind, so a council or dot-web ask is plain), how long it has been held, its chain, the start of its text, and each attachment's name and size. Deny is on the left, Approve on the right.
- Tasks, on its own tab, with the count of tasks waiting on you. See [Tasks from the phone](#tasks-from-the-phone).
- Team, below. Every agent with its status (busy, idle, stuck, offline, with the same detail `tincan agents` shows), kind, queue, good-at line and what it is working on, ordered as `tincan top` orders them: stuck and signed-out agents first.

The page refreshes every 5 seconds while it is open. A request the relay has not answered in 15 seconds counts as failed. When a refresh fails it says "Offline, last updated" with the time and turns off Approve and Deny until the relay answers again. When the device is not an admin it says so.

Status is always written out, never shown by color alone, and every change in the held list and every decision is announced to screen readers.

## Approving and denying

Each held ask in the page's list comes with two one-time codes from the relay, one for approve and one for deny. They work only from the device the list was sent to, only for that ask and that decision, and only for 10 minutes. The relay keeps only a hash of each, in memory, and never logs them. A code is used up by its first use, and any decision ends the hold for every code.

- A second tap, or a decision another device or the CLI already made, shows "Already decided" with the result instead of an error. Approving twice never releases twice.
- After the page has been in the background, it fetches fresh codes before Approve and Deny turn on again.

The relay audits each decision as `approved` or `denied` with the device's machine name. Approval stays with people: no agent tool lists, approves or denies held asks, and none approves a proposed task or resets its assign count. Only a task host you named marks a task done, reopens it or drops it from an agent, and only on your word.
