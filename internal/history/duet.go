package history

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

// Duet reads the owner's 1:1 Claude Duet channel on claude.ai/duet for
// the duet-web agent. Like a dot's DM it is not a history source (the site
// entry is webOnly): there is only duet.detail, which the extension
// answers in the dot feed's shape, with each message's author role and
// whether Claude is working:
//
//	{thread, room, owner, paused, working, items: [{id, at, from, role, text, attachments}]}
//
// The extension refuses a channel shared with anyone else, archived or not
// the owner's (channel_refused), on every read and send.
type Duet struct {
	Client *Client
	Now    func() time.Time
}

// NewDuet returns a Duet channel reader over c.
func NewDuet(c *Client) *Duet { return &Duet{Client: c} }

var errDuetNoHistory = errors.New("duet is not a history source")

// Source implements Reader.
func (r *Duet) Source() Source { return SourceDuet }

// List implements Reader. A Duet channel is not a history source.
func (r *Duet) List(context.Context, int, Options) (Page, error) { return Page{}, errDuetNoHistory }

// Read implements Reader. A Duet channel is not a history source.
func (r *Duet) Read(context.Context, Query, Options) (Page, error) { return Page{}, errDuetNoHistory }

func (r *Duet) live() *live {
	return &live{
		source:      SourceDuet,
		client:      r.Client,
		now:         r.Now,
		detailOp:    OpDuetDetail,
		parseList:   func(json.RawMessage) ([]Conversation, error) { return nil, errDuetNoHistory },
		parseDetail: parseDuetDetail,
		fileArgs:    noFiles,
		canonID:     duetCanonicalID,
	}
}

// duetChannelPattern is a Duet channel id as its address shows it.
var duetChannelPattern = regexp.MustCompile(`^chan_[A-Za-z0-9]{8,64}$`)

// duetConvPath matches a Duet channel's address, /duet/<channel>, or the
// older /messages/<channel> that claude.ai redirects there.
var duetConvPath = regexp.MustCompile(`^/(?:duet|messages)/(chan_[A-Za-z0-9]{8,64})/?$`)

// duetCanonicalID checks a channel id's shape. It also takes the channel's
// address (https://claude.ai/duet/<channel>), which is what the owner
// can copy from the browser.
func duetCanonicalID(id string) (string, bool) {
	id = strings.TrimSpace(id)
	if rest, ok := strings.CutPrefix(id, "https://claude.ai"); ok {
		m := duetConvPath.FindStringSubmatch(strings.SplitN(strings.SplitN(rest, "?", 2)[0], "#", 2)[0])
		if m == nil {
			return "", false
		}
		id = m[1]
	}
	return id, duetChannelPattern.MatchString(id) && validNativeID(id)
}

// duetNotLoggedIn is the not_logged_in reason for a Duet channel.
func duetNotLoggedIn(string) string {
	return "not signed in to claude.ai in Chrome; sign in to claude.ai in Chrome, then ask again"
}

// duetNodes reads the channel for the reply wait as feedNodes does: Claude
// is done once a read confirms it is not working (a read that cannot
// tell counts as working) and its reply has held still for the site's
// stability window.
func duetNodes(raw json.RawMessage) ([]webNode, error) { return feedNodes(duetWords, raw) }

// duetWords name Claude and its Duet channel.
var duetWords = &feedWords{who: "Claude", them: "Claude", theirs: "Claude's", where: "the channel", place: "the channel", url: "https://claude.ai/duet/", from: "the owner's Duet channel", working: true}

// parseDuetDetail turns the channel into turns, as parseDotsDetail does.
func parseDuetDetail(id string, raw json.RawMessage) (thread, error) {
	return parseFeedDetail(SourceDuet, duetWords, id, raw)
}

// CheckWebThread reads site's one thread once through the extension, so
// web install and web serve refuse a thread the site would refuse later (a
// shared Duet channel) before anything is installed or served. It returns
// the extension's error unchanged; a site that serves no fixed thread
// needs no check.
func (c *Client) CheckWebThread(ctx context.Context, src Source, thread string) error {
	s := siteFor(src)
	if s == nil || !s.oneThread {
		return nil
	}
	raw, err := c.Request(ctx, s.op(opDetail), OpArgs{ID: thread})
	if err != nil {
		return err
	}
	if _, err := ParseDotFeed(raw); err != nil {
		return unavailable(src, ErrEndpointChanged, "unexpected "+theConv(src)+" shape: "+err.Error())
	}
	return nil
}
