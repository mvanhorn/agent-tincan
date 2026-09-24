package envelope

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRoundTripKeepsEveryField(t *testing.T) {
	in := Request{
		ID: "r1", From: "instinct", To: "muse", ParentID: "r0", TraceID: "t1",
		Hop: 2, Chain: []string{"grokbot", "instinct"}, Kind: KindAsk,
		Body: "call the dentist", CreatedAt: time.Unix(1_790_000_000, 0).UTC(),
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Request
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != in.ID || out.From != in.From || out.To != in.To || out.ParentID != in.ParentID ||
		out.TraceID != in.TraceID || out.Hop != in.Hop || strings.Join(out.Chain, ",") != "grokbot,instinct" ||
		out.Kind != in.Kind || out.Body != in.Body || !out.CreatedAt.Equal(in.CreatedAt) {
		t.Fatalf("round trip lost data:\nin  %+v\nout %+v", in, out)
	}
}

// AE6: the relay sets From from WhoIs. Anything the client claims is ignored.
func TestParseSendDiscardsClientFrom(t *testing.T) {
	raw := []byte(`{"from":"grokbot","to":"instinct","body":"hi"}`)
	req, err := ParseSend(raw, "muse", DefaultMaxBody)
	if err != nil {
		t.Fatal(err)
	}
	if req.From != "muse" {
		t.Fatalf("From = %q, want attributed sender muse", req.From)
	}
}

// Relay-owned fields are never taken from the client.
func TestParseSendIgnoresRelayOwnedFields(t *testing.T) {
	raw := []byte(`{"id":"forged","to":"instinct","body":"hi","trace_id":"t9","hop":0,"chain":["x"],"created_at":"2001-01-01T00:00:00Z"}`)
	req, err := ParseSend(raw, "muse", DefaultMaxBody)
	if err != nil {
		t.Fatal(err)
	}
	if req.ID != "" || req.TraceID != "" || req.Hop != 0 || len(req.Chain) != 0 || !req.CreatedAt.IsZero() {
		t.Fatalf("client-supplied relay fields survived: %+v", req)
	}
}

func TestParseSendKeepsParentAndDefaultsKind(t *testing.T) {
	req, err := ParseSend([]byte(`{"to":"grokbot","body":"new time Tue 3pm","parent_id":"r7"}`), "muse", DefaultMaxBody)
	if err != nil {
		t.Fatal(err)
	}
	if req.ParentID != "r7" || req.Kind != KindAsk {
		t.Fatalf("got parent %q kind %q, want r7 ask", req.ParentID, req.Kind)
	}
}

func TestParseSendRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"missing to":   `{"body":"hi"}`,
		"empty body":   `{"to":"muse","body":""}`,
		"unknown kind": `{"to":"muse","body":"hi","kind":"shout"}`,
		"not json":     `to=muse`,
		"self send":    `{"to":"muse","body":"hi"}`,
	}
	for name, raw := range cases {
		if _, err := ParseSend([]byte(raw), "muse", DefaultMaxBody); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}
}

func TestParseSendRejectsOversizeBody(t *testing.T) {
	raw := []byte(`{"to":"instinct","body":"` + strings.Repeat("x", 100) + `"}`)
	_, err := ParseSend(raw, "muse", 50)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("want ErrBodyTooLarge, got %v", err)
	}
}

func TestReplyStatusValidation(t *testing.T) {
	for _, s := range []Status{StatusAnswered, StatusFailed, StatusDeclined} {
		if !s.Terminal() {
			t.Errorf("%s should be a terminal reply status", s)
		}
	}
	if _, err := ParseReply([]byte(`{"body":"ok","status":"queued"}`), DefaultMaxBody); err == nil {
		t.Error("reply with non-terminal status should be rejected")
	}
	rep, err := ParseReply([]byte(`{"body":"done, Tue 3pm"}`), DefaultMaxBody)
	if err != nil || rep.Status != StatusAnswered {
		t.Fatalf("reply without status should default to answered, got %+v %v", rep, err)
	}
}

// A send names attachments by id only; name, mime, and size are the relay's
// to fill from the upload, so client-supplied values are dropped.
func TestParseSendKeepsAttachmentIDsOnly(t *testing.T) {
	raw := []byte(`{"to":"instinct","body":"see image","attachments":[{"id":"a1","name":"evil/../x","mime":"text/html","size":9},{"id":"a2"}]}`)
	req, err := ParseSend(raw, "muse", DefaultMaxBody)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Attachments) != 2 || req.Attachments[0] != (Attachment{ID: "a1"}) || req.Attachments[1] != (Attachment{ID: "a2"}) {
		t.Fatalf("attachments = %+v, want ids a1 and a2 only", req.Attachments)
	}
}

// A send without attachments decodes exactly as before.
func TestParseSendWithoutAttachmentsUnchanged(t *testing.T) {
	req, err := ParseSend([]byte(`{"to":"instinct","body":"hi"}`), "muse", DefaultMaxBody)
	if err != nil {
		t.Fatal(err)
	}
	if req.Attachments != nil {
		t.Fatalf("attachments = %+v, want nil", req.Attachments)
	}
	raw, _ := json.Marshal(req)
	if strings.Contains(string(raw), "attachments") {
		t.Fatalf("request without attachments encodes the field: %s", raw)
	}
}

func TestParseSendRejectsBadAttachments(t *testing.T) {
	many := `{"to":"instinct","body":"x","attachments":[`
	var ids []string
	for i := range MaxAttachments + 1 {
		ids = append(ids, `{"id":"a`+string(rune('a'+i))+`"}`)
	}
	many += strings.Join(ids, ",") + `]}`
	cases := map[string]string{
		"too many":  many,
		"empty id":  `{"to":"instinct","body":"x","attachments":[{"id":""}]}`,
		"duplicate": `{"to":"instinct","body":"x","attachments":[{"id":"a1"},{"id":"a1"}]}`,
	}
	for name, raw := range cases {
		if _, err := ParseSend([]byte(raw), "muse", DefaultMaxBody); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}
	_, err := ParseSend([]byte(many), "muse", DefaultMaxBody)
	if !errors.Is(err, ErrTooManyAttachments) {
		t.Fatalf("too many: err = %v, want ErrTooManyAttachments", err)
	}
}

// Attachments may carry a message with an empty body, since the file is the
// content.
func TestParseSendAllowsEmptyBodyWithAttachment(t *testing.T) {
	req, err := ParseSend([]byte(`{"to":"instinct","body":"","attachments":[{"id":"a1"}]}`), "muse", DefaultMaxBody)
	if err != nil || len(req.Attachments) != 1 {
		t.Fatalf("req = %+v, err = %v", req, err)
	}
}

func TestParseReplyAttachments(t *testing.T) {
	rep, err := ParseReply([]byte(`{"body":"here","attachments":[{"id":"a1","mime":"text/html"}]}`), DefaultMaxBody)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Attachments) != 1 || rep.Attachments[0] != (Attachment{ID: "a1"}) {
		t.Fatalf("attachments = %+v", rep.Attachments)
	}
	var ids []string
	for i := range MaxAttachments + 1 {
		ids = append(ids, `{"id":"a`+string(rune('a'+i))+`"}`)
	}
	if _, err := ParseReply([]byte(`{"body":"x","attachments":[`+strings.Join(ids, ",")+`]}`), DefaultMaxBody); !errors.Is(err, ErrTooManyAttachments) {
		t.Fatalf("too many: err = %v", err)
	}
	if _, err := ParseReply([]byte(`{"body":"x","attachments":[{"id":"a1"},{"id":"a1"}]}`), DefaultMaxBody); err == nil {
		t.Fatal("duplicate ids should be rejected")
	}
}

func TestAttachmentJSONShape(t *testing.T) {
	raw, err := json.Marshal(Request{To: "muse", Body: "x", Attachments: []Attachment{{ID: "a1", Name: "cat.png", MIME: "image/png", Size: 42}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"attachments":[{"id":"a1","name":"cat.png","mime":"image/png","size":42}]`) {
		t.Fatalf("json = %s", raw)
	}
}
