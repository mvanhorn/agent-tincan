package client_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

var png = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{3}, 64)...)

func attachMesh(t *testing.T) *testrelay.Mesh {
	t.Helper()
	m := testrelay.New(t, relay.Config{})
	m.Server.SetAttachmentDir(t.TempDir())
	return m
}

// An upload, a send that carries it, and a download by the target, through
// the real relay.
func TestAttachmentRoundTripThroughRelay(t *testing.T) {
	m := attachMesh(t)
	ctx := t.Context()
	grok, inst := m.Client(t, "grokbot"), m.Client(t, "instinct")

	caps, err := grok.Capabilities(ctx)
	if err != nil || !caps.Attachments || caps.MaxAttachments != envelope.MaxAttachments {
		t.Fatalf("capabilities = %+v, %v", caps, err)
	}
	up, err := grok.UploadAttachment(ctx, "cat.png", "image/png", bytes.NewReader(png), int64(len(png)))
	if err != nil {
		t.Fatal(err)
	}
	if up.ID == "" || up.MIME != "image/png" || up.Size != int64(len(png)) {
		t.Fatalf("upload = %+v", up)
	}
	req, err := grok.SendAttached(ctx, "instinct", "look at this", envelope.KindAsk, "", []string{up.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Attachments) != 1 || req.Attachments[0].Name != "cat.png" {
		t.Fatalf("sent = %+v", req.Attachments)
	}
	in, err := inst.Poll(ctx, 0)
	if err != nil || len(in.Requests) != 1 || len(in.Requests[0].Attachments) != 1 {
		t.Fatalf("poll = %+v, %v", in, err)
	}
	var buf bytes.Buffer
	got, err := inst.DownloadAttachment(ctx, in.Requests[0].Attachments[0].ID, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), png) || got.MIME != "image/png" || got.SHA256 != up.SHA256 || got.Size != int64(len(png)) {
		t.Fatalf("download = %+v (%d bytes)", got, buf.Len())
	}
	// A third agent is refused; an admin device is not.
	buf.Reset()
	if _, err := m.Client(t, "muse").DownloadAttachment(ctx, up.ID, &buf); !client.IsStatus(err, http.StatusNotFound) {
		t.Fatalf("third agent: err = %v, want 404", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("refused download wrote %d bytes", buf.Len())
	}
	if _, err := m.Client(t, "admin").DownloadAttachment(ctx, up.ID, &buf); err != nil {
		t.Fatalf("admin: %v", err)
	}
}

func TestReplyAttachedThroughRelay(t *testing.T) {
	m := attachMesh(t)
	ctx := t.Context()
	grok, inst := m.Client(t, "grokbot"), m.Client(t, "instinct")
	res, err := grok.AskAttached(ctx, "instinct", "send the image", "", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	up, err := inst.UploadAttachment(ctx, "notes.txt", "", strings.NewReader("hello"), -1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(up.MIME, "text/plain") {
		t.Fatalf("detected mime = %q", up.MIME)
	}
	if _, err := inst.ReplyAttached(ctx, res.Request.ID, "here", envelope.StatusAnswered, []string{up.ID}); err != nil {
		t.Fatal(err)
	}
	got, err := grok.Get(ctx, res.Request.ID, 0)
	if err != nil || got.Reply == nil || len(got.Reply.Attachments) != 1 || got.Reply.Attachments[0].ID != up.ID {
		t.Fatalf("get = %+v, %v", got, err)
	}
	var buf bytes.Buffer
	if _, err := grok.DownloadAttachment(ctx, up.ID, &buf); err != nil || buf.String() != "hello" {
		t.Fatalf("download = %q, %v", buf.String(), err)
	}
}

// A relay that stores no attachments makes the client refuse to send them,
// without sending anything, while plain sends work as before.
func TestAttachmentsRefusedClientSideWhenRelayLacksThem(t *testing.T) {
	m := testrelay.New(t, relay.Config{}) // in-memory store, no attachment dir
	ctx := t.Context()
	grok := m.Client(t, "grokbot")
	if caps, err := grok.Capabilities(ctx); err != nil || caps.Attachments {
		t.Fatalf("capabilities = %+v, %v", caps, err)
	}
	if _, err := grok.UploadAttachment(ctx, "a.txt", "text/plain", strings.NewReader("a"), 1); !errors.Is(err, client.ErrAttachmentsUnsupported) {
		t.Fatalf("upload: err = %v", err)
	}
	if _, err := grok.SendAttached(ctx, "instinct", "x", envelope.KindAsk, "", []string{"00000000000000000000"}); !errors.Is(err, client.ErrAttachmentsUnsupported) {
		t.Fatalf("send: err = %v", err)
	}
	if n, _ := m.Store.CountQueued(ctx, "instinct"); n != 0 {
		t.Fatalf("a refused send queued %d requests", n)
	}
	if _, err := grok.SendAttached(ctx, "instinct", "plain", envelope.KindAsk, "", nil); err != nil {
		t.Fatalf("plain send: %v", err)
	}
}

// A relay from before capabilities answers 404 there; the client reports
// no support and never posts the send.
func TestOldRelayWithoutCapabilities(t *testing.T) {
	var sends atomic.Int32
	r := distServer(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1/send" {
			sends.Add(1)
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("404 page not found"))
	})
	ctx := context.Background()
	caps, err := r.Capabilities(ctx)
	if err != nil || caps.Attachments {
		t.Fatalf("capabilities = %+v, %v", caps, err)
	}
	if _, err := r.SendAttached(ctx, "instinct", "x", envelope.KindAsk, "", []string{"a1"}); !errors.Is(err, client.ErrAttachmentsUnsupported) {
		t.Fatalf("send: err = %v", err)
	}
	if _, err := r.ReplyAttached(ctx, "r1", "x", envelope.StatusAnswered, []string{"a1"}); !errors.Is(err, client.ErrAttachmentsUnsupported) {
		t.Fatalf("reply: err = %v", err)
	}
	if sends.Load() != 0 {
		t.Fatalf("posted %d sends to a relay without attachments", sends.Load())
	}
}

// A download whose bytes do not match the relay's sha256 is an error.
func TestDownloadAttachmentChecksSHA256(t *testing.T) {
	r := distServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(client.AttachmentSHA256Header, strings.Repeat("0", 64))
		w.Write([]byte("tampered"))
	})
	var buf bytes.Buffer
	if _, err := r.DownloadAttachment(context.Background(), "a1", &buf); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("err = %v, want a sha256 mismatch", err)
	}
}

func TestDownloadAttachmentOversized(t *testing.T) {
	old := client.MaxAttachmentBytes
	client.MaxAttachmentBytes = 16
	t.Cleanup(func() { client.MaxAttachmentBytes = old })
	r := distServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.(http.Flusher).Flush() // no Content-Length: the limit reader must catch it
		w.Write(bytes.Repeat([]byte("x"), 64))
	})
	var buf bytes.Buffer
	if _, err := r.DownloadAttachment(context.Background(), "a1", &buf); err == nil || !strings.Contains(err.Error(), "larger than 16 bytes") {
		t.Fatalf("err = %v", err)
	}
	if buf.Len() > 17 {
		t.Fatalf("wrote %d bytes", buf.Len())
	}
}

// Uploads and downloads outlast the short API timeout.
func TestAttachmentTransfersUseLongTimeout(t *testing.T) {
	if client.AttachmentTimeout <= client.Defaults(client.APIClient).ClientTimeout {
		t.Fatalf("AttachmentTimeout %v should exceed the API timeout", client.AttachmentTimeout)
	}
}

// UploadFiles checks the relay's capabilities once for the whole batch,
// not again for each file.
func TestUploadFilesChecksCapabilitiesOnce(t *testing.T) {
	var capsHits, uploads atomic.Int32
	r := distServer(t, func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch req.URL.Path {
		case "/v1/capabilities":
			capsHits.Add(1)
			w.Write([]byte(`{"attachments":true}`))
		case "/v1/attachments":
			n := uploads.Add(1)
			fmt.Fprintf(w, `{"id":"a%d","name":%q}`, n, req.URL.Query().Get("name"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	dir := t.TempDir()
	paths := []string{filepath.Join(dir, "one.txt"), filepath.Join(dir, "two.txt")}
	for _, p := range paths {
		if err := os.WriteFile(p, []byte("hello"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ups, err := r.UploadFiles(context.Background(), paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 2 || ups[0].Name != "one.txt" || ups[1].Name != "two.txt" {
		t.Fatalf("uploads = %+v", ups)
	}
	if got := capsHits.Load(); got != 1 {
		t.Fatalf("capabilities called %d times, want 1", got)
	}
	if got := uploads.Load(); got != 2 {
		t.Fatalf("uploaded %d files, want 2", got)
	}
}
