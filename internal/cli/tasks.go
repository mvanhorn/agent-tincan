package cli

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// errTaskNoAgent refuses an owner's assign from a machine with no agent
// identity, before anything reaches the relay.
var errTaskNoAgent = errors.New("assigning a task sends an ask as an agent, and this machine has not joined as one, so nothing was changed; join here with `tincan join`, or assign from the Tasks tab of the owner page (/phone/ on the relay)")

// errTaskHostOwner refuses --host with --owner: the owner already reads every
// task, and the host view is an agent's.
var errTaskHostOwner = errors.New("--host is a task host's view of the list; the owner reads every task with --owner alone")

// taskSide holds the flags that say whose call a tasks command makes: the
// local agent's, or the owner's through the admin routes.
type taskSide struct {
	owner            bool
	relayURL, socket string
}

// flags adds --owner, --relay and --socket to cmd; what says what --owner
// does there.
func (s *taskSide) flags(cmd *cobra.Command, what string) {
	cmd.Flags().BoolVar(&s.owner, "owner", false, what+" (admin devices only)")
	adminFlags(cmd, &s.relayURL, &s.socket)
}

// asOwner reports whether the command acts as the owner: with --owner or
// --socket, and on a relay host that never joined, where the local admin
// socket is the only way to the relay.
func (s *taskSide) asOwner() bool {
	if s.owner || s.socket != "" {
		return true
	}
	if s.relayURL != "" || localAdminSocket() == "" {
		return false
	}
	cfg, _ := client.LoadConfig()
	return cfg.Relay == ""
}

// relay is the relay to call and whether to call it as the owner.
func (s *taskSide) relay() (*client.Relay, bool, error) {
	owner := s.asOwner()
	r, err := scheduleRelay(owner, s.socket, s.relayURL)
	return r, owner, err
}

// ownerAgentRelay is the relay for an owner's call that sends an ask: the
// call goes to the admin routes and names this machine's agent, which the
// ask is sent as, so it never goes through the admin socket.
func ownerAgentRelay(relayURL string) (*client.Relay, error) {
	cfg, err := client.LoadConfig()
	if err != nil {
		return nil, err
	}
	if cfg.Agent == "" {
		return nil, errTaskNoAgent
	}
	r, _, err := relayFor(relayURL)
	return r, err
}

// taskArg reads a task id argument; a leading # is allowed.
func taskArg(arg string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(arg, "#"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("task id %q is not a number", arg)
	}
	return id, nil
}

// tasksCmd is tincan tasks: the team's shared task list. Any joined agent
// reads, adds and comments as itself; the decisions on a task (approve,
// done, reopen, reset) are the owner's, from an admin device.
func tasksCmd() *cobra.Command {
	var side taskSide
	var closed, asJSON, host bool
	cmd := &cobra.Command{
		Use:   "tasks",
		Short: "The team's shared task list: what is wanted, who has it, and what came back",
		Long: `The team's shared task list: what is wanted, who has it, and what came back.

With no subcommand it lists the tasks this agent may read, grouped by
status, or with --owner every task. Done and dropped tasks are counted;
--closed lists them too. A task host the owner named passes --host to read
the whole list from its shell, as its list_tasks tool does.

add, show, comment and drop act as this machine's agent, or with --owner as
the owner. assign, approve, done, reopen and reset are the owner's and need
an admin device (or the relay host).

host set and host clear, also the owner's, name a teammate a task host: it
reads every task through its list_tasks and get_task tools and keeps the
list for you from a conversation with it. A task host cannot approve a task
or mark one done.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, owner, err := side.relay()
			if err != nil {
				return err
			}
			defer printHeld(cmd, r)
			if host {
				if owner {
					return errTaskHostOwner
				}
				list, counts, view, err := client.HostTasks(cmd.Context(), r, closed)
				if err != nil {
					return err
				}
				if asJSON {
					return writeJSON(cmd.OutOrStdout(), list)
				}
				cmd.Print(forTerminal(cmd.OutOrStdout(), client.FormatHostTasks(list, counts, view)))
				return nil
			}
			list, counts, err := client.Tasks(cmd.Context(), r, owner, closed)
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), list)
			}
			cmd.Print(forTerminal(cmd.OutOrStdout(), client.FormatTasks(list, counts)))
			return nil
		},
	}
	side.flags(cmd, "list every task, as the owner")
	cmd.Flags().BoolVar(&closed, "closed", false, "list done and dropped tasks too")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().BoolVar(&host, "host", false, "read the whole list, as a task host the owner named")
	cmd.AddCommand(taskAddCmd(), taskShowCmd(), taskAssignCmd(), taskCommentCmd(), taskDropCmd(),
		taskDecideCmd(client.TaskDone, "Mark an open or returned task done"),
		taskDecideCmd(client.TaskApprove, "Approve a proposed task, so it can be assigned"),
		taskDecideCmd(client.TaskReopen, "Put a done or dropped task back to open, or to proposed if it was never approved"),
		taskDecideCmd(client.TaskReset, "Set a task's count of agent assigns back to zero"),
		taskHostCmd())
	return cmd
}

// taskHostCmd is tincan tasks host: the owner's choice of which teammates
// keep the task list for them.
func taskHostCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "host",
		Short: "Name the teammates that keep the task list for you (admin devices only)",
		Long: `Name a teammate a task host, or clear the mark.

A task host is a teammate you trust to keep the team's task list for you
from a conversation with it. Several teammates may be hosts at once. Only an
admin device, or the relay host, names or clears one: no agent can.

A guest, a phone, a history, notes, pass or council agent, an agent driven
through a web chat page, and an agent approval.json gates cannot be a host.
A host that becomes one of those keeps the mark and is shown as suspended
until it is eligible again.

  tincan tasks host set grokbot
  tincan tasks host clear grokbot

"tincan agents" shows each task host.`,
	}
	cmd.AddCommand(taskHostSetCmd("set <agent>", "Name <agent> a task host", true),
		taskHostSetCmd("clear <agent>", "Stop <agent> being a task host", false))
	return cmd
}

func taskHostSetCmd(use, short string, on bool) *cobra.Command {
	var relayURL, socket string
	cmd := &cobra.Command{
		Use:   use,
		Short: short + " (admin devices only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := adminRelay(socket, relayURL)
			if err != nil {
				return err
			}
			warnings, err := r.SetTaskHost(cmd.Context(), args[0], on)
			if err != nil {
				return olderRelayTaskHost(err)
			}
			if !on {
				cmd.Printf("%q is no longer a task host. Tasks it assigned earlier stay attributed to %s.\n", args[0], args[0])
				return nil
			}
			cmd.Printf("%q is now a task host.\n", args[0])
			for _, w := range warnings {
				cmd.Printf("Warning: %s\n", w)
			}
			return nil
		},
	}
	adminFlags(cmd, &relayURL, &socket)
	return cmd
}

// olderRelayTaskHost turns an older relay's plain "404 page not found" for
// the task host route into an upgrade hint. The route's own 404, for an
// agent the relay does not know, comes as a JSON error and passes through.
func olderRelayTaskHost(err error) error {
	apiErr, ok := errors.AsType[*client.APIError](err)
	if !ok || apiErr.Code != http.StatusNotFound || !strings.Contains(apiErr.Message, "page not found") {
		return err
	}
	return fmt.Errorf("%w. The relay is older than this tincan and cannot store task hosts yet: upgrade the relay to this release and restart it", err)
}

func taskAddCmd() *cobra.Command {
	var side taskSide
	var in client.TaskAdd
	var mine, detailStdin, asJSON bool
	var every string
	cmd := &cobra.Command{
		Use:   "add <title...>",
		Short: "Add a task: unassigned, handed to a teammate with --to, or the owner's own with --mine",
		Long: `Add a task: unassigned, handed to a teammate with --to, or the owner's own
with --mine.

The task is filed by this machine's agent, and --to sends that teammate an
ask from it. With --owner the owner files it (admin devices only); --to then
still sends the ask as this machine's agent, so the machine needs one.

A task whose text came from outside the team names where with --source
(mail, meeting). It waits as proposed, with nothing sent, until the owner
approves it.

With --every the task repeats: the relay assigns it to the --to teammate
again that often, as a new attempt on the same task, skipping a time while
an attempt is still open. Marking the task done or dropping it ends the
repeat, and tincan unschedule ends the repeat alone. The repeat is one of
the sending agent's scheduled asks, with their limits.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in.Title = strings.Join(args, " ")
			if mine {
				in.Assignee = envelope.TaskOwner
			}
			if every != "" {
				to := in.Assignee
				if mine {
					to = ""
				}
				var err error
				if in.EverySeconds, err = client.TaskEverySeconds(every, to, time.Now(), errors.New("--every needs --to: a repeating task goes to a teammate")); err != nil {
					return err
				}
			}
			if detailStdin {
				// One byte over the relay's limit, so too long a detail is
				// refused there instead of being cut here.
				b, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 64<<10+1))
				if err != nil {
					return fmt.Errorf("read the detail from stdin: %w", err)
				}
				in.Detail = strings.TrimSpace(string(b))
			}
			owner := side.asOwner()
			var r *client.Relay
			var err error
			if owner && in.Assignee != "" && !mine {
				r, err = ownerAgentRelay(side.relayURL)
			} else {
				r, err = scheduleRelay(owner, side.socket, side.relayURL)
			}
			if err != nil {
				return err
			}
			defer printHeld(cmd, r)
			res, err := client.AddTask(cmd.Context(), r, owner, in)
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), res)
			}
			t := res.Task
			switch {
			case res.Existing:
				cmd.Printf("Task #%d already has that key and is %s; nothing was added.\n", t.ID, t.Status)
			case res.AssignError != "":
				cmd.Print(forTerminal(cmd.OutOrStdout(), fmt.Sprintf("Added task #%d, now %s. It was not assigned: %s\n", t.ID, t.Status, res.AssignError)))
			case t.Status == envelope.TaskProposed:
				cmd.Printf("Added task #%d, now %s. It waits for the owner's approval; nothing was sent.\n", t.ID, t.Status)
			case t.Assignee != "" && !mine:
				cmd.Printf("Added task #%d, now %s, assigned to %s.%s\n", t.ID, t.Status, t.Assignee, client.FormatTaskHeld(t))
			default:
				cmd.Printf("Added task #%d, now %s.\n", t.ID, t.Status)
			}
			if repeat := client.FormatTaskRepeat(t); repeat != "" && !res.Existing {
				cmd.Println(strings.TrimSpace(repeat))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&in.Detail, "detail", "", "what is wanted, beyond the title")
	cmd.Flags().BoolVar(&detailStdin, "detail-stdin", false, "read the detail from stdin")
	cmd.Flags().StringVar(&in.Assignee, "to", "", "teammate to hand the task to")
	cmd.Flags().BoolVar(&mine, "mine", false, "mark the task as the owner's own to do")
	cmd.Flags().StringVar(&in.Key, "key", "", "a key of your own; adding again with the same key returns the first task")
	cmd.Flags().StringVar(&in.Source, "source", "", "where the text came from when that is outside the team, such as mail or meeting")
	cmd.Flags().StringVar(&every, "every", "", "repeat: assign the task to the --to teammate again this often, such as 24h")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	side.flags(cmd, "file the task as the owner")
	cmd.MarkFlagsMutuallyExclusive("to", "mine")
	cmd.MarkFlagsMutuallyExclusive("detail", "detail-stdin")
	return cmd
}

func taskShowCmd() *cobra.Command {
	var side taskSide
	var asJSON, host bool
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show one task whole, with its result and history",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := taskArg(args[0])
			if err != nil {
				return err
			}
			r, owner, err := side.relay()
			if err != nil {
				return err
			}
			defer printHeld(cmd, r)
			var task envelope.Task
			var events []envelope.TaskEvent
			switch {
			case host && owner:
				return errTaskHostOwner
			case host:
				task, events, err = client.HostTask(cmd.Context(), r, id)
			default:
				task, events, err = client.Task(cmd.Context(), r, owner, id)
			}
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"task": task, "events": events})
			}
			cmd.Print(forTerminal(cmd.OutOrStdout(), client.FormatTask(task, events, time.Now())))
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().BoolVar(&host, "host", false, "read the task as a task host the owner named")
	side.flags(cmd, "read any task, as the owner")
	return cmd
}

// taskAssignCmd is tincan tasks assign: the owner's assign. The ask it sends
// comes from this machine's agent, as tincan ask's does, and the task's
// history names the owner.
func taskAssignCmd() *cobra.Command {
	var relayURL string
	cmd := &cobra.Command{
		Use:   "assign <id> [teammate]",
		Short: "Assign a task to a teammate as the owner; with no teammate, list who could take it (admin devices only)",
		Long: `Assign a task to a teammate as the owner (admin devices only). With no
teammate it prints the roster, with each teammate's status and good-at line,
and changes nothing.

Assigning sends the teammate an ask from this machine's agent, so the
machine must have joined as one; the owner page assigns from a machine that
has not. A task someone already has is taken from them and their ask
cancelled.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := taskArg(args[0])
			if err != nil {
				return err
			}
			if len(args) == 1 {
				r, err := adminRelay("", relayURL)
				if err != nil {
					return err
				}
				task, _, err := client.Task(cmd.Context(), r, true, id)
				if err != nil {
					return err
				}
				agents, err := r.Agents(cmd.Context())
				if err != nil {
					return err
				}
				cmd.Print(forTerminal(cmd.OutOrStdout(), fmt.Sprintf("Task #%d is %s: %s\nAssign it with `tincan tasks assign %d <teammate>`. The team:\n%s",
					task.ID, task.Status, client.ScheduleText(task.Title), task.ID, formatAgents(agents, time.Now()))))
				return nil
			}
			r, err := ownerAgentRelay(relayURL)
			if err != nil {
				return err
			}
			task, err := client.AssignTask(cmd.Context(), r, true, id, args[1])
			if err != nil {
				return err
			}
			cmd.Printf("Task #%d is now %s, assigned to %s.%s\n", task.ID, task.Status, task.Assignee, client.FormatTaskHeld(task))
			return nil
		},
	}
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay URL (default: saved config)")
	return cmd
}

func taskCommentCmd() *cobra.Command {
	var side taskSide
	cmd := &cobra.Command{
		Use:   "comment <id> <comment...>",
		Short: "Comment on a task; the next attempt at it carries the comment",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := taskArg(args[0])
			if err != nil {
				return err
			}
			r, owner, err := side.relay()
			if err != nil {
				return err
			}
			defer printHeld(cmd, r)
			res, err := client.CommentTask(cmd.Context(), r, owner, id, strings.Join(args[1:], " "))
			if err != nil {
				return err
			}
			t := res.Task
			cmd.Printf("Commented on task #%d, which is %s.\n", t.ID, t.Status)
			if t.CurrentRequest != "" {
				cmd.Printf("An ask is open under it, and a comment wakes nobody: %s gets this one with the next attempt at the task.\n", t.Assignee)
			}
			return nil
		},
	}
	side.flags(cmd, "comment as the owner, on any task")
	return cmd
}

func taskDropCmd() *cobra.Command {
	var side taskSide
	var note string
	cmd := &cobra.Command{
		Use:   "drop <id>",
		Short: "Give a task up: one this agent filed with no ask open, or with --owner any task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := taskArg(args[0])
			if err != nil {
				return err
			}
			r, owner, err := side.relay()
			if err != nil {
				return err
			}
			defer printHeld(cmd, r)
			task, err := client.DropTask(cmd.Context(), r, owner, id, note)
			if err != nil {
				return err
			}
			cmd.Printf("Task #%d is now %s.\n", task.ID, task.Status)
			return nil
		},
	}
	cmd.Flags().StringVar(&note, "note", "", "why, for the task's history")
	side.flags(cmd, "drop any task as the owner, cancelling an ask open under it")
	return cmd
}

// taskDecideCmd is one of the owner's decisions on a task, named as the
// relay's route is.
func taskDecideCmd(decision, short string) *cobra.Command {
	var relayURL, socket, note string
	cmd := &cobra.Command{
		Use:   decision + " <id>",
		Short: short + " (admin devices only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := taskArg(args[0])
			if err != nil {
				return err
			}
			r, err := adminRelay(socket, relayURL)
			if err != nil {
				return err
			}
			task, err := client.DecideTask(cmd.Context(), r, decision, id, note)
			if err != nil {
				return err
			}
			if decision == client.TaskReset {
				cmd.Printf("Task #%d is %s; agents can assign it again.\n", task.ID, task.Status)
				return nil
			}
			cmd.Printf("Task #%d is now %s.\n", task.ID, task.Status)
			return nil
		},
	}
	if decision != client.TaskReset {
		cmd.Flags().StringVar(&note, "note", "", "why, for the task's history")
	}
	adminFlags(cmd, &relayURL, &socket)
	return cmd
}
