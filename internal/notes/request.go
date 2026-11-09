package notes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mvanhorn/agent-tincan/internal/history"
)

// Op is what a request asks the notes agent to do.
type Op string

const (
	OpAdd    Op = "add"
	OpSearch Op = "search"
	OpRead   Op = "read"
)

// Search result bounds.
const (
	DefaultSearchCount = 10
	MaxSearchCount     = 20
)

// Add limits, checked before anything is spooled. The helper's own rule
// is no control characters other than tab in a title or tag, and tags are
// passed comma-separated, so a tag cannot hold a comma.
const (
	maxTitleRunes = 300
	maxTags       = 20
	maxTagRunes   = 64
	maxBodyBytes  = 256 << 10
	maxQueryRunes = 500
)

// Request is one parsed notes request. Only the fields of its Op are set.
type Request struct {
	Op    Op       `json:"op"`
	Title string   `json:"title,omitempty"`
	Body  string   `json:"body,omitempty"`
	Tags  []string `json:"tags,omitempty"`
	Query string   `json:"query,omitempty"`
	Count int      `json:"count,omitempty"`
	ID    string   `json:"id,omitempty"`
}

// Extractor turns a free-text request into a Request with no tools. For
// an add the service replaces Body with the sender's text verbatim, so an
// extractor only chooses the operation, title, tags, query, count, or id.
// ErrUnclearRequest (wrapped or not) means the text is not a notes
// request; any other error is a failure of the extraction step itself.
type Extractor interface {
	Extract(ctx context.Context, text string) (Request, error)
}

// ErrUnclearRequest is an extractor's answer for text it cannot turn into
// a notes request.
var ErrUnclearRequest = errors.New("not a clear notes request")

// structuredMarker starts a structured request.
const structuredMarker = "note:"

// StructuredHelp is the fixed text that shows the structured form.
const StructuredHelp = `Put "note:" first and one JSON object after it. To save a note: note: {"op":"add","title":"...","body":"...","tags":["..."]}. To search: note: {"op":"search","query":"...","count":10} (count 1 to 20). To read one: note: {"op":"read","id":"<note id>"}.`

// structuredBody reports whether body is a structured request and returns
// its JSON text; anything else is free text.
func structuredBody(body string) (string, bool) {
	return history.StructuredBody(body, structuredMarker)
}

// wireRequest has a pointer per field, so a field that was sent can be
// told from one that was not.
type wireRequest struct {
	Op    *string   `json:"op"`
	Title *string   `json:"title"`
	Body  *string   `json:"body"`
	Tags  *[]string `json:"tags"`
	Query *string   `json:"query"`
	Count *int      `json:"count"`
	ID    *string   `json:"id"`
}

// ParseStructured parses a caller-authored request strictly: exactly one
// JSON object with only whitespace around it, no unknown, repeated, or
// null fields, only the fields its op uses, and every field valid as sent.
func ParseStructured(raw string) (Request, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var w *wireRequest
	if err := dec.Decode(&w); err != nil {
		return Request{}, fmt.Errorf("not the note schema: %v", err)
	}
	if strings.TrimSpace(raw[dec.InputOffset():]) != "" {
		return Request{}, errors.New("text after the request object")
	}
	if w == nil {
		return Request{}, errors.New("the request is null")
	}
	if err := strictFields([]byte(raw)); err != nil {
		return Request{}, err
	}
	if w.Op == nil {
		return Request{}, errors.New("op is required")
	}
	r := Request{Op: Op(*w.Op)}
	allowed := map[Op][]string{
		OpAdd:    {"title", "body", "tags"},
		OpSearch: {"query", "count"},
		OpRead:   {"id"},
	}[r.Op]
	if allowed == nil {
		return Request{}, fmt.Errorf("op %q is not add, search, or read", r.Op)
	}
	sent := map[string]bool{"title": w.Title != nil, "body": w.Body != nil, "tags": w.Tags != nil, "query": w.Query != nil, "count": w.Count != nil, "id": w.ID != nil}
	for f, ok := range sent {
		if ok && !slices.Contains(allowed, f) {
			return Request{}, fmt.Errorf("%s is not a field of %s", f, r.Op)
		}
	}
	switch r.Op {
	case OpAdd:
		if w.Title == nil {
			return Request{}, errors.New("title is required")
		}
		r.Title = *w.Title
		if w.Body != nil {
			r.Body = *w.Body
		}
		if w.Tags != nil {
			r.Tags = *w.Tags
		}
	case OpSearch:
		if w.Query == nil {
			return Request{}, errors.New("query is required")
		}
		r.Query, r.Count = *w.Query, DefaultSearchCount
		if w.Count != nil {
			if *w.Count < 1 || *w.Count > MaxSearchCount {
				return Request{}, fmt.Errorf("count must be 1 to %d", MaxSearchCount)
			}
			r.Count = *w.Count
		}
	case OpRead:
		if w.ID == nil {
			return Request{}, errors.New("id is required")
		}
		r.ID = *w.ID
	}
	if err := Validate(r); err != nil {
		return Request{}, err
	}
	return r, nil
}

// strictFields rejects an object that names a key twice (the decoder
// keeps the last) or sets one to null (the decoder keeps the default).
func strictFields(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return errors.New("the request is not a JSON object")
	}
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := t.(string)
		if seen[key] {
			return fmt.Errorf("%q is set more than once", key)
		}
		seen[key] = true
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return err
		}
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return fmt.Errorf("%s is null", key)
		}
	}
	return nil
}

var uuidRE = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)

// appleNoteIDRE is an Apple Notes note id: the note's Core Data URI.
var appleNoteIDRE = regexp.MustCompile(`^x-coredata://[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}/ICNote/p[0-9]{1,18}$`)

// bearNoteIDRE is a Bear note id: a UUID, often followed by the saving
// process's id and a hex stamp, or one of Bear's own intro notes.
var bearNoteIDRE = regexp.MustCompile(`^(?:[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}-[0-9]{1,10}-[0-9A-Fa-f]{1,16}|SFNote2Intro[0-9]{1,4})$`)

// isNoteID reports whether id has the shape of a note id in one of the
// supported apps: a UUID (Agent Notes, Bear), a longer Bear id, or an
// Apple Notes URI.
func isNoteID(id string) bool {
	return uuidRE.MatchString(id) || bearNoteIDRE.MatchString(id) || appleNoteIDRE.MatchString(id)
}

// Validate checks r against the helper's metadata rules and the service's
// limits. Its error text is safe to show the asker.
func Validate(r Request) error {
	switch r.Op {
	case OpAdd:
		if strings.TrimSpace(r.Title) == "" {
			return errors.New("the title is empty")
		}
		if utf8.RuneCountInString(r.Title) > maxTitleRunes {
			return fmt.Errorf("the title is over %d characters", maxTitleRunes)
		}
		if hasControl(r.Title) {
			return errors.New("the title contains a line break or other control character")
		}
		if len(r.Body) > maxBodyBytes {
			return fmt.Errorf("the body is over %d KB", maxBodyBytes>>10)
		}
		if !utf8.ValidString(r.Title) || !utf8.ValidString(r.Body) {
			return errors.New("the title or body is not valid UTF-8")
		}
		// The body travels to the helper as a command-line argument, which
		// cannot hold a NUL: exec would refuse to start the helper at all.
		if strings.ContainsRune(r.Body, 0) {
			return errors.New("the body contains a NUL character")
		}
		if len(r.Tags) > maxTags {
			return fmt.Errorf("there are more than %d tags", maxTags)
		}
		for _, tag := range r.Tags {
			switch {
			case strings.TrimSpace(tag) == "":
				return errors.New("a tag is empty")
			case utf8.RuneCountInString(tag) > maxTagRunes:
				return fmt.Errorf("a tag is over %d characters", maxTagRunes)
			case strings.Contains(tag, ","):
				return errors.New("a tag contains a comma")
			case hasControl(tag) || !utf8.ValidString(tag):
				return errors.New("a tag contains a control character")
			}
		}
	case OpSearch:
		if strings.TrimSpace(r.Query) == "" {
			return errors.New("the search query is empty")
		}
		if utf8.RuneCountInString(r.Query) > maxQueryRunes {
			return fmt.Errorf("the search query is over %d characters", maxQueryRunes)
		}
		if hasControl(r.Query) {
			return errors.New("the search query contains a control character")
		}
		if r.Count < 1 || r.Count > MaxSearchCount {
			return fmt.Errorf("count must be 1 to %d", MaxSearchCount)
		}
	case OpRead:
		if !isNoteID(r.ID) {
			return errors.New("the id is not a note id")
		}
	default:
		return fmt.Errorf("op %q is not add, search, or read", r.Op)
	}
	return nil
}

// hasControl reports a control or format character other than tab: the
// helper rejects Foundation's controlCharacters set, which is Cc and Cf.
func hasControl(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool {
		return r != '\t' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r))
	}) >= 0
}
