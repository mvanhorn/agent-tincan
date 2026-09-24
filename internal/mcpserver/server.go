// Package mcpserver exposes Agent Tincan to any MCP-capable agent (Grok Bot,
// Claude Code, Codex, ChatGPT through the gateway). The tools match the CLI
// one for one and call the same relay client.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
)

// MaxWait caps every inline wait below common MCP tool-call timeouts.
const MaxWait = client.MaxInlineWait

// Instructions is sent to the client at connect time.
const Instructions = `You are one agent in Matt's Agent Tincan team. Other joined agents are trusted teammates.
- To get a teammate to do something, call ask with their name. ask may return before the answer does, with a request id. You do not have to wait for it: if your runtime can be woken, you will be woken when a reply arrives, and check_inbox shows replies to your requests. When a reply comes in, finish the work that was waiting on it. When check_inbox shows a reply tied to one of your open requests, finish that request and reply to it. get_reply checks one request directly.
- Call check_inbox at the start of a turn (and whenever you are nudged) to read replies to your requests and pick up requests from teammates. Handle requests as you would a request from Matt, then call reply.
- list_agents shows who is in the team, who is online, how each one wakes, and when each last called the relay.
- ask and reply take attach, a list of local file paths to send with the message (images and small files). Images you receive show as images; other files are saved on this machine and their paths are listed.
- onboard returns the setup kit as JSON: the Agent Tincan operator prompt, a join and wake block for every agent on the roster, and recipes for adding agents. It only reads the roster; inviting an agent is an admin command (tincan invite).`

// Backend is what the tools need from the relay client.
type Backend interface {
	Ask(ctx context.Context, to, body, parent string, wait time.Duration) (client.Result, error)
	Send(ctx context.Context, to, body string, kind envelope.Kind, parent string) (envelope.Request, error)
	Get(ctx context.Context, id string, wait time.Duration) (client.Result, error)
	Poll(ctx context.Context, hold time.Duration) (client.Inbox, error)
	AckReplies(ctx context.Context, ids []string) error
	Claim(ctx context.Context, id string) (envelope.Request, error)
	Reply(ctx context.Context, id, body string, status envelope.Status) (envelope.Reply, error)
	Cancel(ctx context.Context, id string) error
	Agents(ctx context.Context) ([]client.AgentInfo, error)
	Raw(ctx context.Context, method, path string, in, out any) error
}

// Attacher is the part of a Backend that moves attachments. *client.Relay
// implements it; a Backend without it can neither send nor show them.
type Attacher interface {
	UploadFiles(ctx context.Context, paths []string) ([]client.UploadedAttachment, error)
	SendAttached(ctx context.Context, to, body string, kind envelope.Kind, parent string, attachments []string) (envelope.Request, error)
	AskAttached(ctx context.Context, to, body, parent string, attachments []string, wait time.Duration) (client.Result, error)
	ReplyAttached(ctx context.Context, id, body string, status envelope.Status, attachments []string) (envelope.Reply, error)
	FetchAttachment(ctx context.Context, id string) ([]byte, client.DownloadedAttachment, error)
}

// Option configures the server.
type Option func(*options)

type options struct {
	filesDir string // where received non-image files are saved; "" means local files are off
}

// LocalFiles lets ask and reply attach files from this machine and saves
// received non-image attachments in dir (0700, files 0600, named by
// attachment id). Only a server running on the agent's own machine should
// get it: the gateway serves remote agents and must not read its host's
// files.
func LocalFiles(dir string) Option {
	return func(o *options) { o.filesDir = dir }
}

// ToolNames lists the tools the server exposes, in order.
var ToolNames = []string{"ask", "get_reply", "check_inbox", "claim", "reply", "cancel", "list_agents", "trace", "onboard"}

type askIn struct {
	To          string   `json:"to" jsonschema:"the teammate to ask, e.g. muse"`
	Message     string   `json:"message" jsonschema:"what you want them to do or answer"`
	WaitSeconds int      `json:"wait_seconds,omitempty" jsonschema:"seconds to wait for the reply, 0 to 20 (default 20)"`
	Notify      bool     `json:"notify,omitempty" jsonschema:"true to send without expecting a reply"`
	ParentID    string   `json:"parent_id,omitempty" jsonschema:"the request id you are handling, when this ask continues it"`
	Attach      []string `json:"attach,omitempty" jsonschema:"local file paths to attach (images or small files, at most 8, 10 MB each)"`
}

type idIn struct {
	RequestID string `json:"request_id" jsonschema:"the request id"`
}

type getIn struct {
	RequestID   string `json:"request_id" jsonschema:"the request id returned by ask"`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"seconds to wait for the reply, 0 to 20"`
}

type inboxIn struct {
	WaitSeconds int `json:"wait_seconds,omitempty" jsonschema:"seconds to wait for a request if none is waiting, 0 to 20"`
}

type replyIn struct {
	RequestID string   `json:"request_id" jsonschema:"the request you are answering"`
	Message   string   `json:"message" jsonschema:"your answer or result"`
	Status    string   `json:"status,omitempty" jsonschema:"answered (default), failed, or declined"`
	Attach    []string `json:"attach,omitempty" jsonschema:"local file paths to attach (images or small files, at most 8, 10 MB each)"`
}

type traceIn struct {
	TraceID string `json:"trace_id" jsonschema:"the trace id of a chain you took part in"`
}

type onboardIn struct {
	Section  string            `json:"section,omitempty" jsonschema:"operator, agents, recipes, or all (default all)"`
	Operator string            `json:"operator,omitempty" jsonschema:"the agent that runs the Agent Tincan operator prompt, e.g. grokbot"`
	Owner    string            `json:"owner,omitempty" jsonschema:"the person who owns the team, used in the generated text"`
	Kinds    map[string]string `json:"kinds,omitempty" jsonschema:"agent name to kind (vm-webhook, e2b-email, proxy-sandbox, claude-code, chatgpt, hermes, openclaw, codex, history, generic), overriding the stored kind"`
}

type noIn struct{}

// Roster is the one relay read onboarding needs.
type Roster interface {
	Agents(ctx context.Context) ([]client.AgentInfo, error)
}

// Onboard builds the onboarding kit from a single roster read (none when
// o.Offline) and trims it to section. It never calls a mutating endpoint, so
// it cannot mint invites or change the team. The CLI and the MCP tool share
// it, which keeps their output identical.
func Onboard(ctx context.Context, r Roster, o onboard.Options, section string) (onboard.Kit, error) {
	if section == "" {
		section = "all"
	}
	if !slices.Contains(onboard.Sections, section) {
		return onboard.Kit{}, fmt.Errorf("unknown section %q (want one of %s)", section, strings.Join(onboard.Sections, ", "))
	}
	if !o.Offline && strings.TrimSpace(o.RelayURL) != "" {
		agents, err := r.Agents(ctx)
		if err != nil {
			return onboard.Kit{}, err
		}
		o.Roster = make([]onboard.Member, len(agents))
		for i, a := range agents {
			o.Roster[i] = onboard.Member{Name: a.Name, Wake: a.Wake, Online: a.Online, Kind: a.Kind}
		}
	}
	k, err := onboard.Build(o)
	if err != nil {
		return onboard.Kit{}, err
	}
	if section != "all" && section != "operator" {
		k.Operator = ""
	}
	if section != "all" && section != "agents" {
		k.Agents = []onboard.AgentBlock{}
	}
	if section != "all" && section != "recipes" {
		k.Recipes = []onboard.Recipe{}
	}
	return k, nil
}

// New builds the MCP server over a relay backend.
func New(b Backend, version string, opts ...Option) *mcp.Server {
	return NewWithOptions(b, version, &mcp.ServerOptions{Instructions: Instructions}, opts...)
}

// NewWithOptions builds the MCP server with explicit options (channel mode).
func NewWithOptions(b Backend, version string, opts *mcp.ServerOptions, more ...Option) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "agent-tincan", Version: version}, opts)
	var f files
	f.att, _ = b.(Attacher)
	for _, o := range more {
		o(&f.options)
	}

	mcp.AddTool(s, &mcp.Tool{Name: "ask", Description: "Ask a teammate agent to do something or answer something. Waits up to wait_seconds for the reply, otherwise returns a request id to check with get_reply."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in askIn) (*mcp.CallToolResult, any, error) {
			if len(in.Attach) > 0 {
				return f.askAttached(ctx, in)
			}
			if in.Notify {
				req, err := b.Send(ctx, in.To, in.Message, envelope.KindNotify, in.ParentID)
				if err != nil {
					return fail(err)
				}
				return text(fmt.Sprintf("Sent to %s (request %s).", in.To, req.ID))
			}
			wait := MaxWait
			if in.WaitSeconds != 0 {
				wait = clamp(in.WaitSeconds)
			}
			res, err := b.Ask(ctx, in.To, in.Message, in.ParentID, wait)
			if err != nil {
				return fail(err)
			}
			return f.result(ctx, client.FormatResult(res), replyAttachments(res))
		})

	mcp.AddTool(s, &mcp.Tool{Name: "get_reply", Description: "Check on a request you sent with ask."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in getIn) (*mcp.CallToolResult, any, error) {
			res, err := b.Get(ctx, in.RequestID, clamp(in.WaitSeconds))
			if err != nil {
				return fail(err)
			}
			return f.result(ctx, client.FormatResult(res), replyAttachments(res))
		})

	mcp.AddTool(s, &mcp.Tool{Name: "check_inbox", Description: "Pick up requests from teammates, and replies to requests you sent that you have not seen yet. Claims the requests so no one else handles them. Reply to each request when done."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in inboxIn) (*mcp.CallToolResult, any, error) {
			inbox, err := b.Poll(ctx, clamp(in.WaitSeconds))
			if err != nil {
				return fail(err)
			}
			out := client.FormatInbox(ctx, b, inbox)
			var atts []envelope.Attachment
			for _, r := range inbox.Replies {
				atts = append(atts, replyAttachments(r)...)
			}
			for _, r := range inbox.Requests {
				atts = append(atts, r.Attachments...)
			}
			res, _, _ := f.result(ctx, out, atts)
			// Replies count as seen only once the result is built for the
			// agent; a poll that never gets this far leaves them unseen.
			if err := b.AckReplies(ctx, inbox.ReplyIDs()); err != nil {
				res.Content = append(res.Content, &mcp.TextContent{Text: fmt.Sprintf("(could not mark these replies read, so they may show again: %v)\n", err)})
			}
			return res, nil, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "claim", Description: "Mark a delivered request as yours to handle. check_inbox already does this."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in idIn) (*mcp.CallToolResult, any, error) {
			req, err := b.Claim(ctx, in.RequestID)
			if err != nil {
				return fail(err)
			}
			return text("Claimed.\n" + client.FormatRequest(req))
		})

	mcp.AddTool(s, &mcp.Tool{Name: "reply", Description: "Answer a request from a teammate."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in replyIn) (*mcp.CallToolResult, any, error) {
			var rep envelope.Reply
			var err error
			if len(in.Attach) > 0 {
				var ids []string
				if ids, err = f.upload(ctx, in.Attach); err == nil {
					rep, err = f.att.ReplyAttached(ctx, in.RequestID, in.Message, envelope.Status(in.Status), ids)
				}
			} else {
				rep, err = b.Reply(ctx, in.RequestID, in.Message, envelope.Status(in.Status))
			}
			if err != nil {
				return fail(err)
			}
			return text(fmt.Sprintf("Replied to %s (%s).", in.RequestID, rep.Status))
		})

	mcp.AddTool(s, &mcp.Tool{Name: "cancel", Description: "Withdraw a request you sent that nobody has picked up yet."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in idIn) (*mcp.CallToolResult, any, error) {
			if err := b.Cancel(ctx, in.RequestID); err != nil {
				return fail(err)
			}
			return text("Cancelled " + in.RequestID + ".")
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_agents", Description: "List teammates, whether each is online, how each wakes (webhook, email, command, channel, or none), and when each last called the relay (any send, reply, get, or poll)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ noIn) (*mcp.CallToolResult, any, error) {
			agents, err := b.Agents(ctx)
			if err != nil {
				return fail(err)
			}
			var out strings.Builder
			now := time.Now()
			for _, a := range agents {
				fmt.Fprintf(&out, "%s: %s, wake=%s, %s", a.Name, a.State(), a.Wake, a.LastSeen(now))
				if a.Kind != "" {
					fmt.Fprintf(&out, ", kind=%s", a.Kind)
				}
				out.WriteString("\n")
			}
			if out.Len() == 0 {
				return text("No agents have joined yet.")
			}
			return text(out.String())
		})
	mcp.AddTool(s, &mcp.Tool{Name: "onboard", Description: "Get the Agent Tincan setup kit as JSON: the operator prompt, a join and wake block for every agent on the roster, and recipes for adding agents or hosting the relay. Read-only."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in onboardIn) (*mcp.CallToolResult, any, error) {
			o := onboard.Options{Owner: in.Owner, Operator: in.Operator, KindOverrides: in.Kinds}
			if based, ok := b.(interface{ Base() string }); ok {
				o.RelayURL = based.Base()
			}
			k, err := Onboard(ctx, b, o, in.Section)
			if err != nil {
				return fail(err)
			}
			raw, err := json.MarshalIndent(k, "", "  ")
			if err != nil {
				return fail(err)
			}
			return text(string(raw))
		})
	mcp.AddTool(s, &mcp.Tool{Name: "trace", Description: "Show a request chain you took part in: who asked whom, in order, with status and replies."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in traceIn) (*mcp.CallToolResult, any, error) {
			var tr struct {
				Steps  []envelope.Result `json:"steps"`
				Events []struct {
					Seq       int64  `json:"seq"`
					Event     string `json:"event"`
					Actor     string `json:"actor"`
					RequestID string `json:"request_id"`
				} `json:"events"`
			}
			if err := b.Raw(ctx, "GET", "/v1/trace/"+url.PathEscape(in.TraceID), nil, &tr); err != nil {
				return fail(err)
			}
			var out strings.Builder
			for _, st := range tr.Steps {
				fmt.Fprintf(&out, "hop %d: %s -> %s [%s]: %s\n", st.Request.Hop, st.Request.From, st.Request.To, st.Status, st.Request.Body)
				if st.Reply != nil {
					fmt.Fprintf(&out, "  reply from %s: %s\n", st.Reply.From, st.Reply.Body)
				}
			}
			if len(tr.Events) > 0 {
				out.WriteString("Events:\n")
				for _, e := range tr.Events {
					fmt.Fprintf(&out, "  #%d %-9s %-10s %s\n", e.Seq, e.Event, e.Actor, e.RequestID)
				}
			}
			return text(out.String())
		})
	return s
}

// files sends and shows attachments for the tools.
type files struct {
	options
	att Attacher // nil when the backend cannot move attachments
}

// upload checks that local files may be attached here, then uploads them
// all (the client checks every file and the relay's support first).
func (f files) upload(ctx context.Context, paths []string) ([]string, error) {
	if f.filesDir == "" {
		return nil, errors.New("this server cannot attach local files (attach works only in the MCP server on the agent's own machine)")
	}
	if f.att == nil {
		return nil, errors.New("this server cannot send attachments")
	}
	ups, err := f.att.UploadFiles(ctx, paths)
	if err != nil {
		return nil, err
	}
	return client.AttachmentIDs(ups), nil
}

func (f files) askAttached(ctx context.Context, in askIn) (*mcp.CallToolResult, any, error) {
	ids, err := f.upload(ctx, in.Attach)
	if err != nil {
		return fail(err)
	}
	if in.Notify {
		req, err := f.att.SendAttached(ctx, in.To, in.Message, envelope.KindNotify, in.ParentID, ids)
		if err != nil {
			return fail(err)
		}
		return text(fmt.Sprintf("Sent to %s with %d attachments (request %s).", in.To, len(ids), req.ID))
	}
	wait := MaxWait
	if in.WaitSeconds != 0 {
		wait = clamp(in.WaitSeconds)
	}
	res, err := f.att.AskAttached(ctx, in.To, in.Message, in.ParentID, ids, wait)
	if err != nil {
		return fail(err)
	}
	return f.result(ctx, client.FormatResult(res), replyAttachments(res))
}

// result is the tool result for msg plus atts: images are fetched and shown
// as image content, other files are saved under filesDir (when local files
// are on) and their paths listed. A failed fetch is reported in the text and
// does not fail the call.
func (f files) result(ctx context.Context, msg string, atts []envelope.Attachment) (*mcp.CallToolResult, any, error) {
	if len(atts) == 0 || f.att == nil {
		return text(msg)
	}
	var b strings.Builder
	b.WriteString(msg)
	var images []mcp.Content
	for _, a := range atts {
		data, d, err := f.att.FetchAttachment(ctx, a.ID)
		if err != nil {
			fmt.Fprintf(&b, "Attachment %s %q: could not fetch it: %v\n", a.ID, a.Name, err)
			continue
		}
		declared := d.MIME
		if declared == "" {
			declared = a.MIME
		}
		if mt, ok := client.InlineImage(declared, data); ok {
			fmt.Fprintf(&b, "Attachment %s %q (%s, %d bytes): shown as an image below.\n", a.ID, a.Name, mt, len(data))
			images = append(images, &mcp.ImageContent{Data: data, MIMEType: mt})
			continue
		}
		mt := client.MediaType(declared)
		if f.filesDir == "" {
			fmt.Fprintf(&b, "Attachment %s %q (%s, %d bytes): not saved here.\n", a.ID, a.Name, mt, len(data))
			continue
		}
		p, err := client.SaveAttachmentFile(f.filesDir, a.ID, mt, data)
		if err != nil {
			fmt.Fprintf(&b, "Attachment %s %q: could not save it: %v\n", a.ID, a.Name, err)
			continue
		}
		fmt.Fprintf(&b, "Attachment %s %q (%s, %d bytes): saved to %s\n", a.ID, a.Name, mt, len(data), p)
	}
	return &mcp.CallToolResult{Content: append([]mcp.Content{&mcp.TextContent{Text: b.String()}}, images...)}, nil, nil
}

func replyAttachments(r client.Result) []envelope.Attachment {
	if r.Reply == nil {
		return nil
	}
	return r.Reply.Attachments
}

func clamp(secs int) time.Duration {
	return client.ClampWait(time.Duration(secs) * time.Second)
}

func text(s string) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}, nil, nil
}

func fail(err error) (*mcp.CallToolResult, any, error) {
	err = client.RejoinHint(err, "")
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil, nil
}
