// Package history reads Matt's conversations with ChatGPT, claude.ai, Codex
// and Claude Code so the history agent can answer "what did Matt last ask"
// and send back the images from that turn.
//
// Every source is an adapter behind Reader. Adapters share one Query shape,
// one recency window and one image pipeline: readers return image bytes,
// SaveImages writes them to a caller-supplied private directory.
package history

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Source names one conversation store.
type Source string

// Sources the history agent knows. Only the local ones have readers in
// this package so far; the live ones are added by later adapters.
const (
	SourceCodex      Source = "codex"
	SourceClaudeCode Source = "claude-code"
	SourceChatGPT    Source = "chatgpt"
	SourceClaudeAI   Source = "claude-ai"
)

// Sources lists every valid Source.
var Sources = []Source{SourceChatGPT, SourceClaudeAI, SourceCodex, SourceClaudeCode}

// Mode is what a query asks for.
type Mode string

// Query modes.
const (
	// ModeLatest returns Matt's newest prompts (Count of them).
	ModeLatest Mode = "latest"
	// ModeSearch returns recent conversations whose title or one of Matt's
	// prompts contains every term.
	ModeSearch Mode = "search"
	// ModeConversation returns one conversation by id.
	ModeConversation Mode = "conversation"
)

// Bounds on a Query. The query can come from an LLM, so every field is
// checked in Go before any read.
const (
	MaxCount     = 20
	MaxTerms     = 8
	MaxTermLen   = 100
	DefaultCount = 1
	// DefaultSearchCount is how many conversations a search returns when
	// Count is unset.
	DefaultSearchCount = 5
	// MaxImageBytes caps one image, matching the relay's attachment cap.
	MaxImageBytes = 10 << 20
	// MaxImages caps the images in one result, matching the relay's
	// attachments-per-message cap.
	MaxImages = 8
	// MaxConversationMessages caps a ModeConversation result; the newest
	// messages are kept.
	MaxConversationMessages = 200
	// maxLine caps one JSONL line; longer lines are skipped.
	maxLine = 64 << 20
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Query is the structured form of a history question.
type Query struct {
	Source         Source   `json:"source"`
	Mode           Mode     `json:"mode"`
	Terms          []string `json:"terms,omitempty"`
	ConversationID string   `json:"conversation_id,omitempty"`
	Count          int      `json:"count,omitempty"`
	WantImages     bool     `json:"want_images,omitempty"`
	// WithImages selects the most recent qualifying turn that has images
	// (attached to Matt's prompt or generated in the reply) instead of the
	// most recent turn. It implies WantImages.
	WithImages bool `json:"with_images,omitempty"`
}

// Validate checks q against the schema and bounds.
func (q Query) Validate() error {
	known := false
	for _, s := range Sources {
		if q.Source == s {
			known = true
		}
	}
	if !known {
		return fmt.Errorf("unknown source %q", q.Source)
	}
	if q.Count < 0 || q.Count > MaxCount {
		return fmt.Errorf("count must be between 1 and %d", MaxCount)
	}
	if len(q.Terms) > MaxTerms {
		return fmt.Errorf("at most %d search terms", MaxTerms)
	}
	for _, t := range q.Terms {
		if strings.TrimSpace(t) == "" {
			return errors.New("empty search term")
		}
		if len(t) > MaxTermLen {
			return fmt.Errorf("search term longer than %d bytes", MaxTermLen)
		}
	}
	if q.ConversationID != "" && !idPattern.MatchString(q.ConversationID) {
		return fmt.Errorf("invalid conversation id %q", q.ConversationID)
	}
	switch q.Mode {
	case ModeLatest:
	case ModeSearch:
		if len(q.Terms) == 0 {
			return errors.New("search needs at least one term")
		}
	case ModeConversation:
		if q.ConversationID == "" {
			return errors.New("conversation mode needs a conversation id")
		}
	default:
		return fmt.Errorf("unknown mode %q", q.Mode)
	}
	return nil
}

// wantsImages reports whether q asks for image bytes. WithImages implies
// it: a turn picked for its images is only useful with them.
func (q Query) wantsImages() bool { return q.WantImages || q.WithImages }

// count returns the effective result count for q.
func (q Query) count() int {
	if q.Count > 0 {
		return q.Count
	}
	if q.Mode == ModeSearch {
		return DefaultSearchCount
	}
	return DefaultCount
}

// Role is who wrote a message.
type Role string

// Message roles.
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Image is one image from a conversation. Readers fill Data (never
// serialized); SaveImages writes it and fills Path.
type Image struct {
	Name   string `json:"name,omitempty"`
	MIME   string `json:"mime"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Path   string `json:"path,omitempty"`
	Data   []byte `json:"-"`
}

// Message is one turn's user prompt or assistant reply.
type Message struct {
	Role   Role      `json:"role"`
	Text   string    `json:"text"`
	Time   time.Time `json:"time,omitzero"`
	Images []Image   `json:"images,omitempty"`
}

// Conversation is one thread or session. List results carry no Messages.
type Conversation struct {
	Source    Source    `json:"source"`
	ID        string    `json:"id"`
	Title     string    `json:"title,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
	// Cwd is the working directory of a local agent thread.
	Cwd string `json:"cwd,omitempty"`
	// Originator is the client that wrote the thread (Codex originator,
	// Claude Code entrypoint).
	Originator string `json:"originator,omitempty"`
	// Automated marks unattended runs (codex exec, Claude Code SDK) that
	// are not "what Matt asked" and only appear with Options.All.
	Automated bool      `json:"automated,omitempty"`
	Messages  []Message `json:"messages,omitempty"`
}

// Options are caller-side switches that never come from the query.
type Options struct {
	// All includes automated runs. The history service's scratch dir is
	// excluded regardless.
	All bool
}

// Reader is one source adapter.
type Reader interface {
	Source() Source
	// List returns up to count conversations, newest first, without
	// messages, within the recency age cap.
	List(ctx context.Context, count int, opts Options) ([]Conversation, error)
	// Read answers q.
	Read(ctx context.Context, q Query, opts Options) ([]Conversation, error)
}

// Window is the recency window for latest and search: the Max most recent
// conversations per source, none older than MaxAge.
type Window struct {
	Max    int
	MaxAge time.Duration
}

// DefaultWindow is 50 conversations capped at 30 days, the same for every
// source.
func DefaultWindow() Window { return Window{Max: 50, MaxAge: 30 * 24 * time.Hour} }

func (w Window) orDefault() Window {
	d := DefaultWindow()
	if w.Max <= 0 {
		w.Max = d.Max
	}
	if w.MaxAge <= 0 {
		w.MaxAge = d.MaxAge
	}
	return w
}

// Admit reports whether the conversation at position n (0 is newest,
// counting only eligible conversations) updated at updated is inside w.
func (w Window) Admit(n int, updated, now time.Time) bool {
	return n < w.Max && w.fresh(updated, now)
}

func (w Window) fresh(updated, now time.Time) bool {
	return !updated.Before(now.Add(-w.MaxAge))
}

// DefaultScratchDir is the history service's own working directory, whose
// conversations are never reported.
func DefaultScratchDir() string { return configPath("TINCAN_HISTORY_SCRATCH", "history-scratch") }

// configPath returns $env when env is non-empty and set, else
// ~/.config/tincan/<leaf>, or "" when the home directory is unknown.
func configPath(env, leaf string) string {
	if env != "" {
		if d := os.Getenv(env); d != "" {
			return d
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "tincan", leaf)
}

// orNow returns f(), or time.Now() when f is nil.
func orNow(f func() time.Time) time.Time {
	if f != nil {
		return f()
	}
	return time.Now()
}

// checkListCount rejects a non-positive List count.
func checkListCount(count int) error {
	if count <= 0 {
		return fmt.Errorf("list count must be positive")
	}
	return nil
}

// checkSource rejects a query addressed to a source other than want.
func checkSource(want Source, q Query) error {
	if q.Source != want {
		return fmt.Errorf("%s reader cannot answer source %q", want, q.Source)
	}
	return nil
}

// inDir reports whether path is dir or inside it. Empty dir matches
// nothing.
func inDir(path, dir string) bool {
	if dir == "" || path == "" {
		return false
	}
	path, dir = filepath.Clean(path), filepath.Clean(dir)
	if path == dir {
		return true
	}
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// imageExt is the mime allowlist; the extension of a saved image comes
// only from here.
var imageExt = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// newImage checks data against the size cap and the mime allowlist, by
// content sniffing, and fills the metadata.
func newImage(data []byte, name string) (Image, bool) {
	if len(data) == 0 || len(data) > MaxImageBytes {
		return Image{}, false
	}
	mime := http.DetectContentType(data)
	if _, ok := imageExt[mime]; !ok {
		return Image{}, false
	}
	sum := sha256.Sum256(data)
	return Image{
		Name:   filepath.Base(name),
		MIME:   mime,
		SHA256: hex.EncodeToString(sum[:]),
		Size:   int64(len(data)),
		Data:   data,
	}, true
}

// decodeDataURL decodes a base64 data: URL.
func decodeDataURL(u string) ([]byte, bool) {
	if !strings.HasPrefix(u, "data:") {
		return nil, false
	}
	comma := strings.IndexByte(u, ',')
	if comma < 0 || !strings.HasSuffix(u[:comma], ";base64") {
		return nil, false
	}
	return decodeBase64(u[comma+1:])
}

func decodeBase64(s string) ([]byte, bool) {
	if base64.StdEncoding.DecodedLen(len(s)) > MaxImageBytes+3 {
		return nil, false
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, false
	}
	return b, true
}

// readImageFile reads a local image file if it is a regular file under the
// size cap with an allowlisted type.
func readImageFile(path string) (Image, bool) {
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() > MaxImageBytes {
		return Image{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Image{}, false
	}
	return newImage(b, path)
}

// appendImage adds img unless an identical image is already present.
func appendImage(imgs []Image, img Image) []Image {
	for _, have := range imgs {
		if have.SHA256 == img.SHA256 {
			return imgs
		}
	}
	return append(imgs, img)
}

// capImages trims the images across convs to MaxImages, keeping the
// earliest.
func capImages(convs []Conversation) {
	n := 0
	for ci := range convs {
		for mi := range convs[ci].Messages {
			m := &convs[ci].Messages[mi]
			room := max(MaxImages-n, 0)
			if len(m.Images) > room {
				m.Images = m.Images[:room]
			}
			n += len(m.Images)
		}
	}
}

// SaveImages writes every image in convs to dir as <sha256><ext>, mode
// 0600, creating dir with mode 0700 if missing, and sets each Path. File
// names come only from the content hash and the mime allowlist.
func SaveImages(dir string, convs []Conversation) error {
	if dir == "" {
		return errors.New("no images directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for ci := range convs {
		for mi := range convs[ci].Messages {
			imgs := convs[ci].Messages[mi].Images
			for ii := range imgs {
				if err := saveImage(dir, &imgs[ii]); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func saveImage(dir string, img *Image) error {
	checked, ok := newImage(img.Data, img.Name)
	if !ok {
		return fmt.Errorf("image %q is not an allowed image type or is too large", img.Name)
	}
	checked.Name = img.Name
	target := filepath.Join(dir, checked.SHA256+imageExt[checked.MIME])
	if st, err := os.Lstat(target); err == nil {
		if !st.Mode().IsRegular() {
			return fmt.Errorf("refusing to write image over non-regular file %s", target)
		}
		checked.Path = target
		*img = checked
		return nil
	}
	if err := writeFileAtomic(target, checked.Data, 0o600); err != nil {
		return err
	}
	checked.Path = target
	*img = checked
	return nil
}

// scanJSONL calls fn with each line of r (without the newline) until fn
// returns false. Lines longer than limit are skipped; decoding and
// malformed-line handling are fn's job.
func scanJSONL(r io.Reader, limit int, fn func(line []byte) bool) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var buf []byte
	skipping := false
	for {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 && !skipping {
			if len(buf)+len(chunk) > limit+1 {
				skipping = true
				buf = buf[:0]
			} else {
				buf = append(buf, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		done := err != nil
		if !skipping {
			line := bytes.TrimRight(buf, "\r\n")
			if len(line) > 0 && !fn(line) {
				return nil
			}
		}
		buf = buf[:0]
		skipping = false
		if done {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// scanFile opens path and scans it with scanJSONL.
func scanFile(path string, fn func(line []byte) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return scanJSONL(f, maxLine, fn)
}

// parseTime parses a record timestamp; zero if absent or malformed.
func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// matchTerms reports whether every term occurs in text, case-insensitively.
func matchTerms(text string, terms []string) bool {
	text = strings.ToLower(text)
	for _, t := range terms {
		if !strings.Contains(text, strings.ToLower(strings.TrimSpace(t))) {
			return false
		}
	}
	return true
}

// turn is one user prompt and the reply to it, the unit of "what Matt
// asked" in every local source.
type turn struct {
	prompt Message
	reply  Message
	// replyImages are images the assistant generated in the reply.
	replyImages []Image
}

// hasImages reports whether t has images attached to the prompt or
// generated in the reply. Live readers hold unresolved placeholders here,
// which count.
func (t turn) hasImages() bool { return len(t.prompt.Images) > 0 || len(t.replyImages) > 0 }

// eligible returns the turns of th that q may select: every turn, or with
// WithImages only the turns that have images.
func eligible(q Query, turns []turn) []turn {
	if !q.WithImages {
		return turns
	}
	var out []turn
	for _, t := range turns {
		if t.hasImages() {
			out = append(out, t)
		}
	}
	return out
}

// messages renders t as a user message and, if present, an assistant reply.
func (t turn) messages(withImages bool) []Message {
	u := t.prompt
	a := t.reply
	a.Role = RoleAssistant
	if withImages {
		a.Images = t.replyImages
	} else {
		u.Images, a.Images = nil, nil
	}
	out := []Message{u}
	if a.Text != "" || len(a.Images) > 0 {
		out = append(out, a)
	}
	return out
}

// thread is one parsed local conversation.
type thread struct {
	conv  Conversation
	turns []turn
}

// pick selects turns for a latest or search query from n candidate
// threads ordered newest first. bound(i) is candidate i's update time, an
// upper bound on its newest prompt. load(i) parses it and reports false
// for excluded threads, which do not count toward the window. pick stops
// at the window's edge and as soon as the answer cannot change. With
// q.WithImages only turns that have images are selected, so the scan goes
// back through the window until it finds them.
func pick(q Query, w Window, now time.Time, n int, bound func(i int) time.Time, load func(i int) (thread, bool, error)) ([]Conversation, error) {
	w = w.orDefault()
	want := q.count()
	type hit struct {
		conv Conversation
		t    turn
	}
	var hits []hit
	admitted := 0
	for i := range n {
		b := bound(i)
		if !w.Admit(admitted, b, now) {
			break
		}
		if q.Mode == ModeLatest && len(hits) >= want && !hits[want-1].t.prompt.Time.Before(b) {
			break
		}
		if q.Mode == ModeSearch && len(hits) >= want {
			break
		}
		th, ok, err := load(i)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		admitted++
		turns := eligible(q, th.turns)
		switch q.Mode {
		case ModeLatest:
			for _, t := range turns {
				hits = append(hits, hit{th.conv, t})
			}
			sortHits(hits, func(a, b int) bool { return hits[a].t.prompt.Time.After(hits[b].t.prompt.Time) })
			if len(hits) > want {
				hits = hits[:want]
			}
		case ModeSearch:
			var found *turn
			for j := len(turns) - 1; j >= 0; j-- {
				if matchTerms(turns[j].prompt.Text, q.Terms) {
					found = &turns[j]
					break
				}
			}
			if found == nil && matchTerms(th.conv.Title, q.Terms) {
				if len(turns) == 0 {
					if !q.WithImages {
						hits = append(hits, hit{conv: th.conv})
					}
					continue
				}
				found = &turns[len(turns)-1]
			}
			if found != nil {
				hits = append(hits, hit{th.conv, *found})
			}
		}
	}
	out := make([]Conversation, 0, len(hits))
	for _, h := range hits {
		c := h.conv
		if h.t.prompt.Role != "" {
			c.Messages = h.t.messages(q.wantsImages())
		}
		out = append(out, c)
	}
	capImages(out)
	return out, nil
}

// sortHits is an insertion sort; hit lists are tiny.
func sortHits[T any](s []T, less func(a, b int) bool) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && less(j, j-1); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// conversationMessages renders every turn of th, newest messages kept
// under MaxConversationMessages.
func conversationMessages(th thread, withImages bool) Conversation {
	c := th.conv
	c.Messages = nil
	for _, t := range th.turns {
		c.Messages = append(c.Messages, t.messages(withImages)...)
	}
	if len(c.Messages) > MaxConversationMessages {
		c.Messages = c.Messages[len(c.Messages)-MaxConversationMessages:]
	}
	out := []Conversation{c}
	capImages(out)
	return out[0]
}

// titleFrom makes a list title from a prompt: first line, 80 runes.
func titleFrom(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) > 80 {
		s = string(r[:80]) + "..."
	}
	return s
}

// ErrNotFound is returned when a conversation id is not visible.
var ErrNotFound = errors.New("conversation not found")
