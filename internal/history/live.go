package history

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// live is the read flow the live site readers share: list
// through the extension, open conversations one at a time while pick needs
// them, then fetch only the images of the turns that were selected.
type live struct {
	source Source
	client *Client
	window Window
	now    func() time.Time
	// agentChats is the web agents' used list (see DefaultWebUsedPath).
	agentChats string
	listOp     Op
	detailOp   Op
	// parseList turns a list result into conversations without messages.
	parseList func(raw json.RawMessage) ([]Conversation, error)
	// listMore, when set, reports whether a list result was cut short
	// with older conversations left unlisted, however many it holds.
	listMore func(raw json.RawMessage) bool
	// parseDetail turns a detail result into a thread whose images are
	// placeholders made by imageRef.
	parseDetail func(id string, raw json.RawMessage) (thread, error)
	// fileArgs maps an image pointer to its file operation arguments.
	fileArgs func(convID, pointer string) (Op, OpArgs, bool)
	// canonID, when set, turns a conversation id in any of the site's
	// forms into the canonical one every store holds.
	canonID func(id string) (string, bool)
	// undatedList: the list carries no times, newest first (Copilot's
	// sidebar). Its conversations are never aged out by the list, and a
	// read bounds each candidate's time by the one read before it, which
	// the sidebar's order guarantees is no older.
	undatedList bool
}

// imageRef is a placeholder image: the source's pointer and a display
// name, resolved to bytes only for selected turns. Placeholders never
// leave the package; unresolved ones are dropped.
func imageRef(pointer, name string) Image {
	return Image{Name: pointer + "\n" + name}
}

func splitRef(img Image) (pointer, name string, ok bool) {
	if img.Data != nil {
		return "", "", false
	}
	pointer, name, ok = strings.Cut(img.Name, "\n")
	return pointer, name, ok
}

func (l *live) clock() time.Time { return orNow(l.now) }

// list asks the extension for the n newest conversations, newest first.
// more says older conversations may be left unlisted: the list is full
// (n, capped at MaxListCount), or the source says it was cut short.
func (l *live) list(ctx context.Context, n int) (convs []Conversation, more bool, err error) {
	n = min(max(n, 1), MaxListCount)
	raw, err := l.client.Request(ctx, l.listOp, OpArgs{Count: n})
	if err != nil {
		return nil, false, err
	}
	convs, err = l.parseList(raw)
	if err != nil {
		return nil, false, unavailable(l.source, ErrEndpointChanged, "unexpected list shape")
	}
	sort.SliceStable(convs, func(i, j int) bool { return convs[i].UpdatedAt.After(convs[j].UpdatedAt) })
	if len(convs) > n {
		convs = convs[:n]
	}
	more = len(convs) >= n || (l.listMore != nil && l.listMore(raw))
	return convs, more, nil
}

func (l *live) detail(ctx context.Context, id string) (thread, error) {
	raw, err := l.client.Request(ctx, l.detailOp, OpArgs{ID: id})
	if err != nil {
		return thread{}, err
	}
	th, err := l.parseDetail(id, raw)
	if err != nil {
		return thread{}, unavailable(l.source, ErrEndpointChanged, "unexpected conversation shape")
	}
	return th, nil
}

// agentOwned returns the conversations the web agents sent into.
func (l *live) agentOwned() map[string]bool { return loadWebUsed(l.agentChats) }

// applied is the window a live read uses: the effective window with Max
// capped at the extension's list cap. The cap never takes Max to zero or
// below, which orDefault would silently reset to the default.
func (l *live) applied(opts Options) Window {
	w := effectiveWindow(l.window, opts)
	w.Max = min(w.Max, MaxListCount)
	return w
}

// List implements Reader.List.
func (l *live) List(ctx context.Context, count int, opts Options) (Page, error) {
	if err := checkListCount(count); err != nil {
		return Page{}, err
	}
	owned := l.agentOwned()
	// Skipped web agent chats must not use up the count, so ask for
	// enough extra to cover them.
	fetch := count
	if !opts.All {
		fetch += len(owned)
	}
	convs, full, err := l.list(ctx, fetch)
	if err != nil {
		return Page{}, err
	}
	w, now := l.applied(opts), l.clock()
	page := Page{Window: w}
	aged := false
	for _, c := range convs {
		if (!l.undatedList || !c.UpdatedAt.IsZero()) && !w.fresh(c.UpdatedAt, now) {
			aged = true
			continue
		}
		if owned[c.ID] {
			if !opts.All {
				continue
			}
			c.Automated = true
		}
		page.Conversations = append(page.Conversations, c)
		if len(page.Conversations) == count {
			break
		}
	}
	if len(page.Conversations) < count {
		switch {
		case aged:
			page.Limited = LimitAge
		case full:
			page.Limited = LimitCount
		}
	}
	return page, nil
}

// Read implements Reader.Read.
func (l *live) Read(ctx context.Context, q Query, opts Options) (Page, error) {
	if err := q.Validate(); err != nil {
		return Page{}, err
	}
	if err := checkSource(l.source, q); err != nil {
		return Page{}, err
	}
	w := l.applied(opts)
	if q.Mode == ModeConversation {
		if l.canonID != nil {
			id, ok := l.canonID(q.ConversationID)
			if !ok {
				return Page{}, fmt.Errorf("invalid %s conversation id %q", l.source, q.ConversationID)
			}
			q.ConversationID = id
		}
		if !validNativeID(q.ConversationID) {
			return Page{}, fmt.Errorf("invalid %s conversation id %q", l.source, q.ConversationID)
		}
		th, err := l.detail(ctx, q.ConversationID)
		if err != nil {
			return Page{}, err
		}
		out := []Conversation{conversationMessages(th, q.wantsImages(), opts.Whole)}
		return Page{Conversations: l.resolve(ctx, out), Window: w}, nil
	}
	owned := l.agentOwned()
	// The web agents' chats are skipped below without using up a window
	// slot, so ask for enough extra to cover them, as List does.
	fetch := w.Max
	if !opts.All {
		fetch += len(owned)
	}
	// A full or cut-short list may have older conversations behind it.
	listStart := time.Now()
	cands, more, err := l.list(ctx, fetch)
	listTook := time.Since(listStart)
	if err != nil {
		return Page{}, &ReadProgressError{ListTook: listTook, Err: err}
	}
	// prev is the time of the last candidate read, for an undated list.
	var prev time.Time
	read := 0
	var detailTook time.Duration
	now := l.clock()
	page, err := pick(q, w, now, len(cands), more,
		func(i int) time.Time {
			if !l.undatedList || !cands[i].UpdatedAt.IsZero() {
				return cands[i].UpdatedAt
			}
			if prev.IsZero() {
				return now
			}
			return prev
		},
		func(i int) (thread, bool, error) {
			if err := ctx.Err(); err != nil {
				return thread{}, false, err
			}
			// A web agent's conversation does not use up a window slot.
			if owned[cands[i].ID] && !opts.All {
				return thread{}, false, nil
			}
			detailStart := time.Now()
			th, err := l.detail(ctx, cands[i].ID)
			detailTook += time.Since(detailStart)
			if err != nil {
				return thread{}, false, err
			}
			read++
			if th.conv.UpdatedAt.IsZero() {
				th.conv.UpdatedAt = cands[i].UpdatedAt
			}
			if th.conv.Title == "" {
				th.conv.Title = cands[i].Title
			}
			th.conv.Automated = owned[cands[i].ID]
			if l.undatedList {
				// With no time from the list or the read, its age cannot
				// be checked against the window, so it is not shown.
				if th.conv.UpdatedAt.IsZero() {
					return thread{}, false, nil
				}
				prev = th.conv.UpdatedAt
				// Past the window's age: not shown, and the next
				// candidate's bound (this time) stops the scan.
				if !w.orDefault().fresh(prev, now) {
					return thread{}, false, nil
				}
			}
			return th, len(th.turns) > 0, nil
		})
	if err != nil {
		return Page{}, &ReadProgressError{Listed: len(cands), Read: read, ListTook: listTook, DetailTook: detailTook, Err: err}
	}
	page.Conversations = l.resolve(ctx, page.Conversations)
	return page, nil
}

// ReadProgressError is a live read that failed: how long its list and
// its conversation reads took, how many conversations were listed and how
// many were read before err. Its text is err's, so it only adds detail
// for the history service's log. Images are fetched only after every
// read succeeded, so a failed read never spent time on them.
type ReadProgressError struct {
	Listed, Read         int
	ListTook, DetailTook time.Duration
	Err                  error
}

func (e *ReadProgressError) Error() string { return e.Err.Error() }
func (e *ReadProgressError) Unwrap() error { return e.Err }

// resolve fetches the bytes of every placeholder image in convs. An image
// that cannot be fetched or is not an allowed image is dropped; the text
// answer still stands. After a rate limit no further image is asked for.
func (l *live) resolve(ctx context.Context, convs []Conversation) []Conversation {
	limited := false
	for ci := range convs {
		for mi := range convs[ci].Messages {
			m := &convs[ci].Messages[mi]
			var kept []Image
			for _, img := range m.Images {
				pointer, name, ok := splitRef(img)
				if !ok {
					kept = append(kept, img)
					continue
				}
				op, args, ok := l.fileArgs(convs[ci].ID, pointer)
				if !ok || limited {
					continue
				}
				data, _, err := l.client.File(ctx, op, args)
				if err != nil {
					_, limited = rateLimited(err)
					continue
				}
				got, ok := newImage(data, name)
				if !ok {
					continue
				}
				got.Name = name
				kept = appendImage(kept, got)
			}
			m.Images = kept
		}
	}
	return convs
}

// flexTime reads a timestamp given as epoch seconds (number or numeric
// string), an RFC 3339 string, or null.
type flexTime struct{ time.Time }

func (t *flexTime) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == `""` {
		t.Time = time.Time{}
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		if p, err := time.Parse(time.RFC3339Nano, str); err == nil {
			t.Time = p.UTC()
			return nil
		}
		s = str
	}
	var f float64
	if err := json.Unmarshal([]byte(s), &f); err != nil {
		return fmt.Errorf("bad timestamp %s", b)
	}
	sec := int64(f)
	t.Time = time.Unix(sec, int64((f-float64(sec))*1e9)).UTC()
	return nil
}
