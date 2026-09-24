package history

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// live is the read flow the ChatGPT and claude.ai readers share: list
// through the extension, open conversations one at a time while pick needs
// them, then fetch only the images of the turns that were selected.
type live struct {
	source   Source
	client   *Client
	window   Window
	now      func() time.Time
	listOp   Op
	detailOp Op
	// parseList turns a list result into conversations without messages.
	parseList func(raw json.RawMessage) ([]Conversation, error)
	// parseDetail turns a detail result into a thread whose images are
	// placeholders made by imageRef.
	parseDetail func(id string, raw json.RawMessage) (thread, error)
	// fileArgs maps an image pointer to its file operation arguments.
	fileArgs func(convID, pointer string) (Op, OpArgs, bool)
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
func (l *live) list(ctx context.Context, n int) ([]Conversation, error) {
	n = min(max(n, 1), MaxListCount)
	raw, err := l.client.Request(ctx, l.listOp, OpArgs{Count: n})
	if err != nil {
		return nil, err
	}
	convs, err := l.parseList(raw)
	if err != nil {
		return nil, unavailable(l.source, ErrEndpointChanged, "unexpected list shape")
	}
	sort.SliceStable(convs, func(i, j int) bool { return convs[i].UpdatedAt.After(convs[j].UpdatedAt) })
	if len(convs) > n {
		convs = convs[:n]
	}
	return convs, nil
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

// List implements Reader.List.
func (l *live) List(ctx context.Context, count int, _ Options) ([]Conversation, error) {
	if err := checkListCount(count); err != nil {
		return nil, err
	}
	convs, err := l.list(ctx, count)
	if err != nil {
		return nil, err
	}
	w, now := l.window.orDefault(), l.clock()
	out := convs[:0]
	for _, c := range convs {
		if w.fresh(c.UpdatedAt, now) {
			out = append(out, c)
		}
	}
	return out, nil
}

// Read implements Reader.Read.
func (l *live) Read(ctx context.Context, q Query, _ Options) ([]Conversation, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	if err := checkSource(l.source, q); err != nil {
		return nil, err
	}
	if q.Mode == ModeConversation {
		if !validNativeID(q.ConversationID) {
			return nil, fmt.Errorf("invalid %s conversation id %q", l.source, q.ConversationID)
		}
		th, err := l.detail(ctx, q.ConversationID)
		if err != nil {
			return nil, err
		}
		out := []Conversation{conversationMessages(th, q.wantsImages())}
		return l.resolve(ctx, out), nil
	}
	w := l.window.orDefault()
	cands, err := l.list(ctx, w.Max)
	if err != nil {
		return nil, err
	}
	out, err := pick(q, w, l.clock(), len(cands),
		func(i int) time.Time { return cands[i].UpdatedAt },
		func(i int) (thread, bool, error) {
			if err := ctx.Err(); err != nil {
				return thread{}, false, err
			}
			th, err := l.detail(ctx, cands[i].ID)
			if err != nil {
				return thread{}, false, err
			}
			if th.conv.UpdatedAt.IsZero() {
				th.conv.UpdatedAt = cands[i].UpdatedAt
			}
			if th.conv.Title == "" {
				th.conv.Title = cands[i].Title
			}
			return th, len(th.turns) > 0, nil
		})
	if err != nil {
		return nil, err
	}
	return l.resolve(ctx, out), nil
}

// resolve fetches the bytes of every placeholder image in convs. An image
// that cannot be fetched or is not an allowed image is dropped; the text
// answer still stands.
func (l *live) resolve(ctx context.Context, convs []Conversation) []Conversation {
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
				if !ok {
					continue
				}
				data, _, err := l.client.File(ctx, op, args)
				if err != nil {
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
