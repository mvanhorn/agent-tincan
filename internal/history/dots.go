package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Dots reads the owner's OpenAI dot's DM (one of the dot's threads) for
// the dot-web agent. It is not a history source (the site entry is
// webOnly): there is no list, only dots.detail, which the extension
// answers with the DM room's latest messages, oldest first, and the room's
// owner (its creator) and whether the dot is paused:
//
//	{thread, room, owner, paused, items: [{id, at, from, text, attachments}]}
//
// A message whose from is the owner is the owner's (a request this agent
// typed is one); any other from is the dot.
type Dots struct {
	Client *Client
	Now    func() time.Time
}

// NewDots returns a dot DM reader over c.
func NewDots(c *Client) *Dots { return &Dots{Client: c} }

var errDotsNoHistory = errors.New("dots is not a history source")

// Source implements Reader.
func (r *Dots) Source() Source { return SourceDots }

// List implements Reader. A dot's DM is not a history source.
func (r *Dots) List(context.Context, int, Options) (Page, error) { return Page{}, errDotsNoHistory }

// Read implements Reader. A dot's DM is not a history source.
func (r *Dots) Read(context.Context, Query, Options) (Page, error) { return Page{}, errDotsNoHistory }

func (r *Dots) live() *live {
	return &live{
		source:      SourceDots,
		client:      r.Client,
		now:         r.Now,
		detailOp:    OpDotsDetail,
		parseList:   func(json.RawMessage) ([]Conversation, error) { return nil, errDotsNoHistory },
		parseDetail: parseDotsDetail,
		fileArgs:    noFiles,
		canonID:     dotsCanonicalID,
	}
}

// dotsThreadPattern is a dot's thread id as tincan takes it: lowercase
// hex and hyphens (a UUID in practice), loosely.
var dotsThreadPattern = regexp.MustCompile(`^[0-9a-f-]{8,64}$`)

// dotsCanonicalID lowercases a dot thread id and checks its shape.
func dotsCanonicalID(id string) (string, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	return id, dotsThreadPattern.MatchString(id) && validNativeID(id)
}

// dotsConvPath matches a dot's thread address, /dots/<thread>. The DM page
// itself is at /dots/<dot id>, which names no thread.
var dotsConvPath = regexp.MustCompile(`^/dots/([0-9A-Fa-f-]{8,64})/?$`)

// dotsNotLoggedIn is the not_logged_in reason for a dot.
func dotsNotLoggedIn(string) string {
	return "not signed in to ChatGPT in Chrome; sign in to chatgpt.com in Chrome, then ask again"
}

// DotFeed is a dot DM read by dots.detail, or a Duet channel read by
// duet.detail in the same shape: the thread, its room, the owner's account
// user id, whether the dot is paused, whether the assistant is working
// (nil when the site does not say, as a dot's feed never does), and the
// latest messages, oldest first.
type DotFeed struct {
	Thread   string
	Room     string
	Owner    string
	Paused   bool
	Working  *bool
	Messages []DotMessage
}

// DotMessage is one message of a dot's DM or a Duet channel. Owner is set
// when the owner wrote it, Other when someone who is neither the owner nor
// the assistant did (a Duet member or another agent); otherwise the dot or
// Claude did.
type DotMessage struct {
	ID          string
	At          time.Time
	From        string
	Text        string
	Attachments int
	Owner       bool
	Other       bool
}

type dotsDetail struct {
	Thread  string `json:"thread"`
	Room    string `json:"room"`
	Owner   string `json:"owner"`
	Paused  bool   `json:"paused"`
	Working *bool  `json:"working"`
	Items   []struct {
		ID   string      `json:"id"`
		At   copilotTime `json:"at"`
		From string      `json:"from"`
		// Role, sent by duet.detail only: owner, claude or other.
		Role        string `json:"role"`
		Text        string `json:"text"`
		Attachments int    `json:"attachments"`
	} `json:"items"`
}

// ParseDotFeed reads the extension's dots.detail result. A result with no
// owner or no items list, or a message with no id or author, is an error.
func ParseDotFeed(raw json.RawMessage) (DotFeed, error) {
	var d dotsDetail
	if err := json.Unmarshal(raw, &d); err != nil {
		return DotFeed{}, err
	}
	if strings.TrimSpace(d.Owner) == "" {
		return DotFeed{}, errors.New("no owner")
	}
	if d.Items == nil {
		return DotFeed{}, errors.New("no items")
	}
	f := DotFeed{Thread: d.Thread, Room: d.Room, Owner: d.Owner, Paused: d.Paused, Working: d.Working}
	for _, m := range d.Items {
		if m.ID == "" || m.From == "" {
			return DotFeed{}, errors.New("a message without an id or author")
		}
		f.Messages = append(f.Messages, DotMessage{
			ID: m.ID, At: m.At.Time, From: m.From, Text: m.Text,
			Attachments: max(m.Attachments, 0), Owner: m.From == d.Owner,
			Other: m.From != d.Owner && m.Role == "other",
		})
	}
	return f, nil
}

// dotsNodes reads the DM for the reply wait: each owner message is a user
// node, and each run of dot messages after one is a single reply node
// (dotRun). The feed marks nothing finished, so a reply is finished by the
// site's text-stability rule (DefaultDotStablePolls, DefaultDotStableFor),
// which lets a dot that answers in several messages finish them all.
func dotsNodes(raw json.RawMessage) ([]webNode, error) { return feedNodes(dotsWords, raw) }

// dotsWords name a dot and its DM.
var dotsWords = &feedWords{who: "your dot", them: "the dot", theirs: "the dot's", where: "the DM", place: "its DM", url: "https://chatgpt.com/dots/", from: "the owner's dot"}

// feedNodes reads a one-thread site's feed as dotsNodes describes. A
// message from anyone but the owner and the assistant is neither a turn
// nor part of an answer. When the feed says whether the assistant is
// working, a run is busy while it is (or, for a site that always says,
// while a read cannot tell), so text that holds still mid-run is not
// taken as finished; once it is not, the text-stability rule finishes the
// run, so a message posted just after the read still counts.
func feedNodes(w *feedWords, raw json.RawMessage) ([]webNode, error) {
	f, err := ParseDotFeed(raw)
	if err != nil {
		return nil, err
	}
	var out []webNode
	msgs := slices.DeleteFunc(slices.Clone(f.Messages), func(m DotMessage) bool { return m.Other })
	for i := 0; i < len(msgs); {
		if msgs[i].Owner {
			out = append(out, webNode{id: msgs[i].ID, user: true, text: msgs[i].Text, at: msgs[i].At})
			i++
			continue
		}
		j := i
		for j < len(msgs) && !msgs[j].Owner {
			j++
		}
		n := dotRun(w, f.Thread, msgs[i:j])
		if w.working {
			n.busy = f.Working == nil || *f.Working
		} else if f.Working != nil {
			n.busy = *f.Working
		}
		out = append(out, n)
		i = j
	}
	return out, nil
}

// dotRun is one run of the dot's messages as one reply node: its texts
// joined by a blank line (with a note for attachments tincan does not
// carry), identified by the run's first message and dated by its last.
//
// A run that ends in the dot's own "@tincan ask <agent>" is a dot waiting
// on a teammate: the watcher cannot send that ask while this request holds
// the send path, so the run would end there. The reply keeps the ask line
// and notes where the dot's final answer will be.
func dotRun(w *feedWords, thread string, run []DotMessage) webNode {
	var texts []string
	files := 0
	for _, m := range run {
		if t := strings.TrimSpace(m.Text); t != "" {
			texts = append(texts, m.Text)
		}
		files += m.Attachments
	}
	if files > 0 {
		what := "an attachment"
		if files > 1 {
			what = fmt.Sprintf("%d attachments", files)
		}
		texts = append(texts, fmt.Sprintf("(%s also sent %s; open %s to see them)", w.who, what, w.where))
	}
	if ask, ok := parseDotAsk(run[len(run)-1].Text); ok && ask.bad == "" {
		dm := w.place
		if thread != "" {
			dm = w.place + ": " + w.url + thread
		}
		texts = append(texts, fmt.Sprintf("(%s asked %s for help; tincan will pass %s's answer to %s, and %s final answer will be in %s)", w.who, ask.target, ask.target, w.them, w.theirs, dm))
	}
	return webNode{id: run[0].ID, reply: true, text: strings.Join(texts, "\n\n"), at: run[len(run)-1].At}
}

// parseDotsDetail turns the DM into turns: each owner message and the dot
// messages after it, as dotsNodes joins them.
func parseDotsDetail(id string, raw json.RawMessage) (thread, error) {
	return parseFeedDetail(SourceDots, dotsWords, id, raw)
}

// parseFeedDetail is parseDotsDetail for the one-thread site src, worded
// by w.
func parseFeedDetail(src Source, w *feedWords, id string, raw json.RawMessage) (thread, error) {
	nodes, err := feedNodes(w, raw)
	if err != nil {
		return thread{}, err
	}
	th := thread{conv: Conversation{Source: src, ID: id}}
	var cur *turn
	for _, n := range nodes {
		if n.user {
			if cur != nil {
				th.turns = append(th.turns, *cur)
			}
			cur = &turn{prompt: Message{Role: RoleUser, Text: n.text, Time: n.at}, promptID: n.id}
		} else if cur != nil {
			cur.reply = Message{Role: RoleAssistant, Text: n.text, Time: n.at}
		}
		if th.conv.UpdatedAt.Before(n.at) {
			th.conv.UpdatedAt = n.at
		}
	}
	if cur != nil {
		th.turns = append(th.turns, *cur)
	}
	return th, nil
}

// ParseWebThread checks a --thread value for site: the site's canonical
// thread id, and false when the site takes none or the id is malformed.
func ParseWebThread(site Source, id string) (string, bool) {
	s := siteFor(site)
	if s == nil || !s.oneThread {
		return "", false
	}
	return s.canonical(id)
}

// WebSiteTakesThread reports whether site's web agent serves one fixed
// thread, given with --thread.
func WebSiteTakesThread(site Source) bool {
	s := siteFor(site)
	return s != nil && s.oneThread
}

// DotPrimary is the account's primary dot as dots.primary names it: its
// current thread (canonical, as --thread takes it) and its name.
type DotPrimary struct {
	Thread string
	Name   string
}

// DotPrimary asks the extension for the account's primary dot, for web
// install when no --thread is given.
func (c *Client) DotPrimary(ctx context.Context) (DotPrimary, error) {
	raw, err := c.Request(ctx, OpDotsPrimary, OpArgs{})
	if err != nil {
		return DotPrimary{}, err
	}
	var p struct {
		Thread string `json:"thread"`
		Name   string `json:"name"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return DotPrimary{}, unavailable(SourceDots, ErrEndpointChanged, "unexpected primary dot")
	}
	thread, ok := ParseWebThread(SourceDots, p.Thread)
	if !ok {
		return DotPrimary{}, unavailable(SourceDots, ErrEndpointChanged, "unexpected primary dot thread")
	}
	return DotPrimary{Thread: thread, Name: p.Name}, nil
}
