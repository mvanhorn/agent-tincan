package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Gemini reads gemini.google.com through the Tincan Chrome extension. The
// extension fetches /app for the page's session values (SNlM0e, cfb2h,
// FdrFJe), then calls the app's batchexecute endpoint: MaZiqc lists
// conversations a page at a time and hNvQHb reads one conversation's
// latest turns. It hands back the decoded inner payloads as they are;
// every position is read here, defensively, and any shape this reader
// does not recognize is ErrEndpointChanged, never a crash.
//
// Conversation ids are canonical in the form of the conversation URL
// (/app/<16 hex>): the "c_" the batchexecute payloads carry exists only
// inside the extension (KTD7).
type Gemini struct {
	Client *Client
	Window Window
	Now    func() time.Time
	// AgentChats is the web agents' used list; those conversations are
	// left out unless all is asked for (DefaultWebUsedPath from New).
	AgentChats string
}

// NewGemini returns a Gemini reader over c.
func NewGemini(c *Client) *Gemini {
	return &Gemini{Client: c, AgentChats: DefaultWebUsedPath(SourceGemini)}
}

// Source implements Reader.
func (r *Gemini) Source() Source { return SourceGemini }

func (r *Gemini) live() *live {
	return &live{
		source:      SourceGemini,
		client:      r.Client,
		window:      r.Window,
		now:         r.Now,
		agentChats:  r.AgentChats,
		listOp:      OpGeminiList,
		detailOp:    OpGeminiDetail,
		parseList:   parseGeminiList,
		parseDetail: parseGeminiDetail,
		fileArgs:    geminiFileArgs,
		canonID:     geminiCanonicalID,
	}
}

// List implements Reader.
func (r *Gemini) List(ctx context.Context, count int, opts Options) (Page, error) {
	return r.live().List(ctx, count, opts)
}

// Read implements Reader.
func (r *Gemini) Read(ctx context.Context, q Query, opts Options) (Page, error) {
	return r.live().Read(ctx, q, opts)
}

// geminiIDPattern is a canonical Gemini conversation id: the hex in
// /app/<id> (16 digits today; a little slack either way).
var geminiIDPattern = regexp.MustCompile(`^[0-9a-f]{8,64}$`)

// geminiURLPattern matches a Gemini conversation URL's path: /app/<id>,
// /u/<n>/app/<id> (another signed-in account) and /gem/<name>/<id>.
var geminiURLPattern = regexp.MustCompile(`^/(?:u/\d{1,2}/)?(?:app|gem/[A-Za-z0-9_-]{1,128})/((?:c_)?[0-9A-Fa-f]{8,64})/?$`)

// geminiCanonicalID turns a Gemini conversation id in any of its forms
// ("c_<hex>" inside batchexecute, "<hex>" in the URL) into the canonical
// URL form.
func geminiCanonicalID(id string) (string, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	id = strings.TrimPrefix(id, "c_")
	if !geminiIDPattern.MatchString(id) {
		return "", false
	}
	return id, true
}

// geminiImageScheme is a placeholder pointer to a reply image: the
// response candidate's id and the image's position among the candidate's
// image URLs, in the order geminiImageURLs finds them. The extension finds
// the URL again from the same read, so no URL ever travels to it.
const geminiImageScheme = "gemini-image://"

// geminiImageHost is where Gemini's images are served from.
const geminiImageHost = "https://lh3.googleusercontent.com/"

func geminiImagePointer(candidate string, n int) string {
	return geminiImageScheme + candidate + "/" + strconv.Itoa(n)
}

// geminiFileArgs maps an image pointer to gemini.file: file_id is
// "<candidate id>-<index>" and the conversation id says which
// conversation to read it from.
func geminiFileArgs(convID, pointer string) (Op, OpArgs, bool) {
	rest, ok := strings.CutPrefix(pointer, geminiImageScheme)
	if !ok {
		return "", OpArgs{}, false
	}
	cand, n, ok := strings.Cut(rest, "/")
	if !ok || !validNativeID(cand) {
		return "", OpArgs{}, false
	}
	if i, err := strconv.Atoi(n); err != nil || i < 0 || i > 99 {
		return "", OpArgs{}, false
	}
	id, ok := geminiCanonicalID(convID)
	file := cand + "-" + n
	if !ok || !validNativeID(file) {
		return "", OpArgs{}, false
	}
	return OpGeminiFile, OpArgs{FileID: file, ConversationID: id}, true
}

// errGeminiShape is a payload position that does not hold what it should.
var errGeminiShape = errors.New("unexpected gemini payload shape")

// gIndex returns v[i] when v is an array long enough, else nil.
func gIndex(v any, i int) any {
	a, ok := v.([]any)
	if !ok || i < 0 || i >= len(a) {
		return nil
	}
	return a[i]
}

// gPath follows positions from v; nil when any step is missing.
func gPath(v any, path ...int) any {
	for _, i := range path {
		v = gIndex(v, i)
	}
	return v
}

// gArray is v as an array: ok when v is an array, and when v is null and
// null is allowed (an empty list).
func gArray(v any, nullOK bool) ([]any, bool) {
	if v == nil {
		return nil, nullOK
	}
	a, ok := v.([]any)
	return a, ok
}

// gTime reads a [seconds, nanos] timestamp; zero when absent or malformed.
func gTime(v any) time.Time {
	sec, ok := gIndex(v, 0).(float64)
	if !ok || sec <= 0 {
		return time.Time{}
	}
	nanos, _ := gIndex(v, 1).(float64)
	if nanos < 0 || nanos >= 1e9 {
		nanos = 0
	}
	return time.Unix(int64(sec), int64(nanos)).UTC()
}

func decodeGemini(raw json.RawMessage) (any, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// gmList is the list operation's answer: the inner MaZiqc payload of each
// page the extension read, in order.
type gmList struct {
	Pages []json.RawMessage `json:"pages"`
}

// parseGeminiList reads each page's conversations: page[2] is the list
// (null or missing on an empty account), and each entry is ["c_<id>",
// title, ..., [seconds, nanos] at 5].
func parseGeminiList(raw json.RawMessage) ([]Conversation, error) {
	var l gmList
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, err
	}
	if l.Pages == nil {
		return nil, errors.New("no pages")
	}
	var out []Conversation
	seen := map[string]bool{}
	entries := 0
	for _, p := range l.Pages {
		page, err := decodeGemini(p)
		if err != nil {
			return nil, err
		}
		if _, ok := gArray(page, false); !ok {
			return nil, errGeminiShape
		}
		items, ok := gArray(gIndex(page, 2), true)
		if !ok {
			return nil, errGeminiShape
		}
		entries += len(items)
		for _, it := range items {
			if _, ok := it.([]any); !ok {
				return nil, errGeminiShape
			}
			rawID, ok := gIndex(it, 0).(string)
			if !ok {
				return nil, errGeminiShape
			}
			id, ok := geminiCanonicalID(rawID)
			if !ok || seen[id] {
				continue
			}
			seen[id] = true
			title, _ := gIndex(it, 1).(string)
			out = append(out, Conversation{Source: SourceGemini, ID: id, Title: title, UpdatedAt: gTime(gIndex(it, 5))})
		}
	}
	// Entries with not one "c_" + hex id among them mean the id format
	// changed, not an empty history.
	if entries > 0 && len(out) == 0 {
		return nil, errGeminiShape
	}
	return out, nil
}

// gmTurn is one prompt and its chosen answer from an hNvQHb read.
type gmTurn struct {
	promptID string
	prompt   string
	// replyID is the chosen response candidate ("rc_..."), "" while
	// there is none.
	replyID string
	reply   string
	images  []string
	at      time.Time
}

// geminiTurns reads an hNvQHb payload: inner[0] is the turns, each
// [["c_<id>", "r_<id>"], _, [[prompt], ...], [[candidates...], _, _,
// "rc_<chosen>"], [seconds, nanos]], and a candidate is ["rc_<id>",
// [text], ...]. The answer part is null or missing while Gemini has not
// started one. Turns are returned oldest first.
func geminiTurns(raw json.RawMessage) ([]gmTurn, error) {
	inner, err := decodeGemini(raw)
	if err != nil {
		return nil, err
	}
	if _, ok := gArray(inner, false); !ok {
		return nil, errGeminiShape
	}
	list, ok := gArray(gIndex(inner, 0), true)
	if !ok {
		return nil, errGeminiShape
	}
	var out []gmTurn
	for _, t := range list {
		if _, ok := t.([]any); !ok {
			return nil, errGeminiShape
		}
		rid, ok := gPath(t, 0, 1).(string)
		if !ok || !validNativeID(rid) {
			return nil, errGeminiShape
		}
		prompt, ok := gPath(t, 2, 0, 0).(string)
		if !ok {
			return nil, errGeminiShape
		}
		gt := gmTurn{promptID: rid, prompt: prompt, at: gTime(gIndex(t, 4))}
		answer := gIndex(t, 3)
		if answer != nil {
			cands, ok := gArray(gIndex(answer, 0), true)
			if !ok {
				return nil, errGeminiShape
			}
			chosen, _ := gIndex(answer, 3).(string)
			var cand any
			for _, c := range cands {
				if id, _ := gIndex(c, 0).(string); chosen != "" && id == chosen {
					cand = c
					break
				}
			}
			// A chosen id that names no candidate is a shape this reader
			// does not know: the first candidate may be a draft the owner
			// did not pick, so it is never taken in its place. With no
			// chosen id at all, the first candidate is the answer.
			if cand == nil && chosen != "" {
				return nil, errGeminiShape
			}
			if cand == nil && len(cands) > 0 {
				cand = cands[0]
			}
			if cand != nil {
				id, ok := gIndex(cand, 0).(string)
				if !ok || !validNativeID(id) {
					return nil, errGeminiShape
				}
				text, _ := gPath(cand, 1, 0).(string)
				gt.replyID, gt.reply, gt.images = id, cleanGeminiText(text), geminiImageURLs(cand)
			}
		}
		out = append(out, gt)
	}
	// hNvQHb lists the newest turn first; the web agent and history want
	// the conversation in order.
	sort.SliceStable(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	return out, nil
}

// geminiPlaceholder is the stand-in Gemini's text carries where a
// generated image is shown.
var geminiPlaceholder = regexp.MustCompile(`https?://googleusercontent\.com/[a-z_]+_content/\d+(?:_\d+)*\s*`)

func cleanGeminiText(s string) string {
	return strings.TrimSpace(geminiPlaceholder.ReplaceAllString(s, ""))
}

// geminiImageURLs lists the image URLs in a response candidate: every
// string anywhere in its arrays, outside its text at position 1, that is
// an https URL on lh3.googleusercontent.com, first occurrence first, in
// depth-first array order. The extension's gemini.file walks the candidate
// the same way to find the n-th URL again.
func geminiImageURLs(cand any) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			if strings.HasPrefix(x, geminiImageHost) && !seen[x] {
				seen[x] = true
				out = append(out, x)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	for i, e := range gArrayOrNil(cand) {
		if i != 1 {
			walk(e)
		}
	}
	return out
}

func gArrayOrNil(v any) []any {
	a, _ := v.([]any)
	return a
}

func parseGeminiDetail(id string, raw json.RawMessage) (thread, error) {
	turns, err := geminiTurns(raw)
	if err != nil {
		return thread{}, err
	}
	cid, ok := geminiCanonicalID(id)
	if !ok {
		return thread{}, fmt.Errorf("invalid gemini conversation id %q", id)
	}
	th := thread{conv: Conversation{Source: SourceGemini, ID: cid}}
	for _, t := range turns {
		tr := turn{prompt: Message{Role: RoleUser, Text: t.prompt, Time: t.at}, promptID: t.promptID}
		if t.replyID != "" {
			tr.reply = Message{Role: RoleAssistant, Text: t.reply, Time: t.at}
			for n := range t.images {
				tr.replyImages = append(tr.replyImages, imageRef(geminiImagePointer(t.replyID, n), fmt.Sprintf("gemini-image-%d", n+1)))
			}
		}
		if t.at.After(th.conv.UpdatedAt) {
			th.conv.UpdatedAt = t.at
		}
		th.turns = append(th.turns, tr)
	}
	return th, nil
}

// geminiNodes reads a conversation for the reply wait. hNvQHb has no
// finished marker, so an answer is never marked finished here: the site's
// text-stability rule (longer than claude.ai's, since Gemini pauses while
// it thinks) decides when it is done.
func geminiNodes(raw json.RawMessage) ([]webNode, error) {
	turns, err := geminiTurns(raw)
	if err != nil {
		return nil, err
	}
	var out []webNode
	for _, t := range turns {
		out = append(out, webNode{id: t.promptID, user: true, text: t.prompt, at: t.at})
		if t.replyID != "" {
			out = append(out, webNode{id: t.replyID, reply: true, text: t.reply, at: t.at, images: len(t.images)})
		}
	}
	return out, nil
}
