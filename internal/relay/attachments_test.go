package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/identity/identitytest"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// pngBytes starts with the PNG signature so content sniffing sees an image.
var pngBytes = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{7}, 100)...)

// attachHarness is a harness with attachments stored in a temp dir.
func attachHarness(t *testing.T, cfg Config) (*harness, string) {
	t.Helper()
	h := newHarness(t, cfg)
	dir := t.TempDir()
	h.srv.SetAttachmentDir(dir)
	return h, dir
}

// raw sends body from addr through the real handler chain. A negative
// length sends it with no Content-Length, as a chunked upload does.
func (h *harness) raw(addr, method, path string, body io.Reader, length int64, header map[string]string) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(method, path, body)
	req.RemoteAddr = addr
	if length >= 0 {
		req.ContentLength = length
	} else {
		req.ContentLength = -1
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	return rec
}

// upload stores data as addr's agent and returns what the relay reported.
func (h *harness) upload(addr, name, mime string, data []byte) client.UploadedAttachment {
	h.t.Helper()
	rec := h.raw(addr, "POST", "/v1/attachments?name="+name, bytes.NewReader(data), int64(len(data)), map[string]string{"Content-Type": mime})
	if rec.Code != http.StatusCreated {
		h.t.Fatalf("upload %s from %s: status %d: %s", name, addr, rec.Code, rec.Body.String())
	}
	var out client.UploadedAttachment
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		h.t.Fatal(err)
	}
	return out
}

func sendWith(to, body string, ids ...string) string {
	atts := make([]string, len(ids))
	for i, id := range ids {
		atts[i] = `{"id":"` + id + `"}`
	}
	return `{"to":"` + to + `","body":"` + body + `","attachments":[` + strings.Join(atts, ",") + `]}`
}

func blobFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// The upload route is exempt from the relay-wide 1 MB body cap and applies
// its own 10 MB cap, through the real handler chain.
func TestUploadFiveMBSucceedsElevenRejected(t *testing.T) {
	h, dir := attachHarness(t, Config{})
	five := bytes.Repeat([]byte("a"), 5<<20)
	got := h.upload(grokAddr, "five.bin", "application/octet-stream", five)
	if got.Size != 5<<20 || got.SHA256 != sum(five) {
		t.Fatalf("5 MB upload = %+v", got)
	}
	eleven := bytes.Repeat([]byte("b"), 11<<20)
	rec := h.raw(grokAddr, "POST", "/v1/attachments?name=eleven.bin", bytes.NewReader(eleven), int64(len(eleven)), nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("11 MB with Content-Length: status %d, want 413: %s", rec.Code, rec.Body.String())
	}
	// Without a Content-Length the 10 MB reader stops it.
	rec = h.raw(grokAddr, "POST", "/v1/attachments?name=eleven.bin", bytes.NewReader(eleven), -1, nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("11 MB chunked: status %d, want 413: %s", rec.Code, rec.Body.String())
	}
	// Rejected uploads leave no file, temp file, or reservation behind.
	if files := blobFiles(t, dir); len(files) != 1 || files[0] != got.ID {
		t.Fatalf("blob dir = %v, want only %s", files, got.ID)
	}
	var n int
	if err := h.st.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("attachment rows = %d, %v; want 1", n, err)
	}
}

// Every other route keeps the 1 MB cap.
func TestOtherRoutesKeepBodyCap(t *testing.T) {
	h, _ := attachHarness(t, Config{})
	big := `{"to":"instinct","body":"` + strings.Repeat("x", 2<<20) + `"}`
	rec := h.raw(grokAddr, "POST", "/v1/send", strings.NewReader(big), int64(len(big)), nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("2 MB send: status %d, want 413", rec.Code)
	}
}

func TestUploadFetchRoundTrip(t *testing.T) {
	h, dir := attachHarness(t, Config{})
	up := h.upload(grokAddr, "cat.png", "image/png", pngBytes)
	if up.ID == "" || up.Name != "cat.png" || up.MIME != "image/png" || up.Size != int64(len(pngBytes)) || up.SHA256 != sum(pngBytes) {
		t.Fatalf("upload = %+v", up)
	}
	if _, err := os.Stat(filepath.Join(dir, up.ID)); err != nil {
		t.Fatalf("blob not stored by id: %v", err)
	}
	rec := h.do(grokAddr, "GET", "/v1/attachments/"+up.ID, "", http.StatusOK, nil)
	if !bytes.Equal(rec.Body.Bytes(), pngBytes) {
		t.Fatal("fetched bytes differ from the upload")
	}
	hd := rec.Header()
	if hd.Get("Content-Type") != "image/png" || hd.Get(client.AttachmentSHA256Header) != sum(pngBytes) ||
		hd.Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(hd.Get("Content-Disposition"), "attachment") {
		t.Fatalf("headers = %v", hd)
	}
}

// Without a usable Content-Type the relay sniffs the media type. A name
// with a path in it is kept for display only, reduced to its last element.
func TestUploadSniffsMIMEAndCleansName(t *testing.T) {
	h, dir := attachHarness(t, Config{})
	up := h.upload(grokAddr, "..%2F..%2Fetc%2Fpasswd", "", pngBytes)
	if up.MIME != "image/png" {
		t.Fatalf("sniffed mime = %q, want image/png", up.MIME)
	}
	if up.Name != "passwd" {
		t.Fatalf("name = %q, want passwd", up.Name)
	}
	if files := blobFiles(t, dir); len(files) != 1 || files[0] != up.ID {
		t.Fatalf("blob dir = %v", files)
	}
	up = h.upload(grokAddr, "notes.txt", "not a media type", []byte("hello there"))
	if !strings.HasPrefix(up.MIME, "text/plain") {
		t.Fatalf("mime for a bad Content-Type = %q, want sniffed text/plain", up.MIME)
	}
}

func TestSendCarriesAttachmentsToTarget(t *testing.T) {
	h, _ := attachHarness(t, Config{})
	up := h.upload(grokAddr, "cat.png", "image/png", pngBytes)
	var req envelope.Request
	h.do(grokAddr, "POST", "/v1/send", sendWith("instinct", "look", up.ID), http.StatusCreated, &req)
	want := envelope.Attachment{ID: up.ID, Name: "cat.png", MIME: "image/png", Size: int64(len(pngBytes))}
	if len(req.Attachments) != 1 || req.Attachments[0] != want {
		t.Fatalf("sent attachments = %+v", req.Attachments)
	}
	var got pollResult
	h.do(instinctAddr, "GET", "/v1/poll?hold=0", "", http.StatusOK, &got)
	if len(got.Requests) != 1 || len(got.Requests[0].Attachments) != 1 || got.Requests[0].Attachments[0] != want {
		t.Fatalf("delivered = %+v", got.Requests)
	}
	// The target may fetch what it was sent.
	h.do(instinctAddr, "GET", "/v1/attachments/"+up.ID, "", http.StatusOK, nil)
}

func TestTooManyAttachmentsRejected(t *testing.T) {
	h, _ := attachHarness(t, Config{})
	var ids []string
	for range envelope.MaxAttachments + 1 {
		ids = append(ids, h.upload(grokAddr, "a.txt", "text/plain", []byte("a")).ID)
	}
	rec := h.do(grokAddr, "POST", "/v1/send", sendWith("instinct", "x", ids...), http.StatusBadRequest, nil)
	if !strings.Contains(rec.Body.String(), "too many attachments") {
		t.Fatalf("error = %s", rec.Body.String())
	}
	h.do(grokAddr, "POST", "/v1/send", sendWith("instinct", "x", ids[:envelope.MaxAttachments]...), http.StatusCreated, nil)
}

func TestAttachmentQuotasRejected(t *testing.T) {
	t.Run("per agent", func(t *testing.T) {
		h, _ := attachHarness(t, Config{Attachments: AttachmentConfig{PerAgentBytes: 100}})
		h.upload(grokAddr, "a", "text/plain", bytes.Repeat([]byte("a"), 60))
		rec := h.raw(grokAddr, "POST", "/v1/attachments?name=b", bytes.NewReader(bytes.Repeat([]byte("b"), 60)), 60, nil)
		if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), "agent attachment quota exceeded") {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		h.upload(museAddr, "c", "text/plain", bytes.Repeat([]byte("c"), 60)) // other agents are unaffected
	})
	t.Run("relay wide", func(t *testing.T) {
		h, _ := attachHarness(t, Config{Attachments: AttachmentConfig{TotalBytes: 100}})
		h.upload(grokAddr, "a", "text/plain", bytes.Repeat([]byte("a"), 60))
		rec := h.raw(museAddr, "POST", "/v1/attachments?name=b", bytes.NewReader(bytes.Repeat([]byte("b"), 60)), 60, nil)
		if rec.Code != http.StatusInsufficientStorage || !strings.Contains(rec.Body.String(), "relay attachment storage is full") {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("low disk", func(t *testing.T) {
		h, dir := attachHarness(t, Config{Attachments: AttachmentConfig{
			MinFreeBytes: 1000,
			FreeBytes:    func(string) (int64, error) { return 1050, nil },
		}})
		h.upload(grokAddr, "a", "text/plain", bytes.Repeat([]byte("a"), 40))
		rec := h.raw(grokAddr, "POST", "/v1/attachments?name=b", bytes.NewReader(bytes.Repeat([]byte("b"), 60)), 60, nil)
		if rec.Code != http.StatusInsufficientStorage || !strings.Contains(rec.Body.String(), "disk") {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		if files := blobFiles(t, dir); len(files) != 1 {
			t.Fatalf("blob dir = %v", files)
		}
	})
}

func TestSendRejectsBadAttachmentReferences(t *testing.T) {
	h, _ := attachHarness(t, Config{})
	theirs := h.upload(museAddr, "m.png", "image/png", pngBytes)
	rec := h.do(grokAddr, "POST", "/v1/send", sendWith("instinct", "x", theirs.ID), http.StatusForbidden, nil)
	if !strings.Contains(rec.Body.String(), "uploaded by another agent") {
		t.Fatalf("error = %s", rec.Body.String())
	}
	h.do(grokAddr, "POST", "/v1/send", sendWith("instinct", "x", "00000000000000000000"), http.StatusBadRequest, nil)
	mine := h.upload(grokAddr, "g.png", "image/png", pngBytes)
	h.do(grokAddr, "POST", "/v1/send", sendWith("instinct", "x", mine.ID), http.StatusCreated, nil)
	h.do(grokAddr, "POST", "/v1/send", sendWith("muse", "again", mine.ID), http.StatusConflict, nil)
	// A reply is held to the same rule.
	req := h.send(museAddr, "grokbot", "send me a picture")
	h.do(grokAddr, "POST", "/v1/requests/"+req.ID+"/reply", `{"body":"here","attachments":[{"id":"`+theirs.ID+`"}]}`, http.StatusForbidden, nil)
}

func TestFetchAuthorization(t *testing.T) {
	h, _ := attachHarness(t, Config{})
	up := h.upload(grokAddr, "cat.png", "image/png", pngBytes)
	// Before it is sent only the uploader (and admins) may fetch it.
	h.do(instinctAddr, "GET", "/v1/attachments/"+up.ID, "", http.StatusNotFound, nil)
	h.do(grokAddr, "POST", "/v1/send", sendWith("instinct", "look", up.ID), http.StatusCreated, nil)
	h.do(grokAddr, "GET", "/v1/attachments/"+up.ID, "", http.StatusOK, nil)
	h.do(instinctAddr, "GET", "/v1/attachments/"+up.ID, "", http.StatusOK, nil)
	// A third agent not on the request cannot, and learns nothing.
	rec := h.do(museAddr, "GET", "/v1/attachments/"+up.ID, "", http.StatusNotFound, nil)
	if strings.Contains(rec.Body.String(), "cat.png") {
		t.Fatalf("refusal leaked metadata: %s", rec.Body.String())
	}
	h.do(strangerAddr, "GET", "/v1/attachments/"+up.ID, "", http.StatusForbidden, nil)
	// An admin device can.
	h.do(macAddr, "GET", "/v1/attachments/"+up.ID, "", http.StatusOK, nil)
	h.do(grokAddr, "GET", "/v1/attachments/not-an-id", "", http.StatusNotFound, nil)
	h.do(grokAddr, "GET", "/v1/attachments/00000000000000000000", "", http.StatusNotFound, nil)
}

// A reply's attachment is readable by the asker and the replier.
func TestReplyAttachmentFetchableByAsker(t *testing.T) {
	h, _ := attachHarness(t, Config{})
	req := h.send(grokAddr, "instinct", "send the image")
	up := h.upload(instinctAddr, "cat.png", "image/png", pngBytes)
	var rep envelope.Reply
	h.do(instinctAddr, "POST", "/v1/requests/"+req.ID+"/reply", `{"body":"here","attachments":[{"id":"`+up.ID+`"}]}`, http.StatusOK, &rep)
	if len(rep.Attachments) != 1 || rep.Attachments[0].Name != "cat.png" {
		t.Fatalf("reply attachments = %+v", rep.Attachments)
	}
	var res envelope.Result
	h.do(grokAddr, "GET", "/v1/requests/"+req.ID, "", http.StatusOK, &res)
	if res.Reply == nil || len(res.Reply.Attachments) != 1 || res.Reply.Attachments[0].ID != up.ID {
		t.Fatalf("get reply = %+v", res.Reply)
	}
	h.do(grokAddr, "GET", "/v1/attachments/"+up.ID, "", http.StatusOK, nil)
	h.do(museAddr, "GET", "/v1/attachments/"+up.ID, "", http.StatusNotFound, nil)
}

func TestAttachmentAuditEvents(t *testing.T) {
	h, _ := attachHarness(t, Config{})
	up := h.upload(grokAddr, "cat.png", "image/png", pngBytes)
	var req envelope.Request
	h.do(grokAddr, "POST", "/v1/send", sendWith("instinct", "look", up.ID), http.StatusCreated, &req)
	h.do(instinctAddr, "GET", "/v1/attachments/"+up.ID, "", http.StatusOK, nil)
	rows, err := h.st.DB().Query(`SELECT event, request_id, actor, detail FROM audit WHERE event LIKE 'attachment_%' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var event, reqID, actor, detail string
		if err := rows.Scan(&event, &reqID, &actor, &detail); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(detail, up.ID) {
			t.Fatalf("%s detail %q lacks the attachment id", event, detail)
		}
		got = append(got, fmt.Sprintf("%s %s %s", event, reqID, actor))
	}
	want := []string{"attachment_uploaded  grokbot", "attachment_fetched " + req.ID + " instinct"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("audit = %q, want %q", got, want)
	}
}

// The sweep removes an orphan's file after 24 hours, and an attachment's
// file 7 days after its request is final, keeping the row marked deleted.
func TestSweepRemovesAttachmentFiles(t *testing.T) {
	h, dir := attachHarness(t, Config{})
	now := time.Now()
	h.st.SetClock(func() time.Time { return now })
	orphan := h.upload(grokAddr, "o.png", "image/png", pngBytes)
	kept := h.upload(grokAddr, "k.png", "image/png", pngBytes)
	var req envelope.Request
	h.do(grokAddr, "POST", "/v1/send", sendWith("instinct", "look", kept.ID), http.StatusCreated, &req)
	h.do(instinctAddr, "POST", "/v1/requests/"+req.ID+"/reply", `{"body":"seen"}`, http.StatusOK, nil)
	// A temp file from an upload cut off by a crash.
	stray := filepath.Join(dir, ".upload-stray")
	if err := os.WriteFile(stray, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-2 * time.Hour)
	os.Chtimes(stray, old, old)
	ctx := context.Background()

	now = now.Add(25 * time.Hour)
	h.srv.SweepAttachments(ctx)
	if _, err := os.Stat(filepath.Join(dir, orphan.ID)); !os.IsNotExist(err) {
		t.Fatalf("orphan file still present: %v", err)
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatalf("stray temp file still present: %v", err)
	}
	h.do(grokAddr, "GET", "/v1/attachments/"+orphan.ID, "", http.StatusNotFound, nil)
	h.do(instinctAddr, "GET", "/v1/attachments/"+kept.ID, "", http.StatusOK, nil)

	now = now.Add(7 * 24 * time.Hour)
	h.srv.SweepAttachments(ctx)
	if files := blobFiles(t, dir); len(files) != 0 {
		t.Fatalf("blob dir after retention = %v", files)
	}
	rec := h.do(instinctAddr, "GET", "/v1/attachments/"+kept.ID, "", http.StatusGone, nil)
	if !strings.Contains(rec.Body.String(), "deleted") {
		t.Fatalf("gone body = %s", rec.Body.String())
	}
	a, err := h.st.Attachment(ctx, kept.ID)
	if err != nil || a.DeletedAt.IsZero() {
		t.Fatalf("row = %+v, %v; want kept and marked deleted", a, err)
	}
}

// A relay without attachment storage says so, refuses uploads and sends
// that carry attachments, and handles old-shape sends exactly as before.
func TestAttachmentsDisabledAndCapability(t *testing.T) {
	h := newHarness(t, Config{}) // in-memory store: no attachment dir
	var caps client.Capabilities
	h.do(grokAddr, "GET", "/v1/capabilities", "", http.StatusOK, &caps)
	if caps.Attachments {
		t.Fatalf("capabilities = %+v, want no attachments", caps)
	}
	rec := h.raw(grokAddr, "POST", "/v1/attachments?name=a", strings.NewReader("a"), 1, nil)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "not enabled") {
		t.Fatalf("upload when disabled: %d %s", rec.Code, rec.Body.String())
	}
	h.do(grokAddr, "POST", "/v1/send", sendWith("instinct", "x", "00000000000000000000"), http.StatusBadRequest, nil)
	rec = h.do(grokAddr, "POST", "/v1/send", `{"to":"instinct","body":"plain"}`, http.StatusCreated, nil)
	if strings.Contains(rec.Body.String(), "attachments") {
		t.Fatalf("old-shape send response mentions attachments: %s", rec.Body.String())
	}

	h2, _ := attachHarness(t, Config{})
	h2.do(grokAddr, "GET", "/v1/capabilities", "", http.StatusOK, &caps)
	if !caps.Attachments || caps.MaxAttachmentBytes != 10<<20 || caps.MaxAttachments != envelope.MaxAttachments {
		t.Fatalf("capabilities = %+v", caps)
	}
	h2.do(strangerAddr, "GET", "/v1/capabilities", "", http.StatusForbidden, nil)
	rec = h2.do(grokAddr, "POST", "/v1/send", `{"to":"instinct","body":"plain"}`, http.StatusCreated, nil)
	if strings.Contains(rec.Body.String(), "attachments") {
		t.Fatalf("old-shape send response mentions attachments: %s", rec.Body.String())
	}
}

// A relay on a database file keeps attachments beside it by default.
func TestFileStoreEnablesAttachmentsBesideDatabase(t *testing.T) {
	base := t.TempDir()
	st, err := store.Open(filepath.Join(base, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	who := identitytest.New(map[string]identity.Node{macAddr: {ID: "nMAC", Name: "macbook-pro-44"}, grokAddr: {ID: "nGROK", Name: "grok-bot"}})
	srv := New(identity.NewDirectory(st, who, identity.Config{Admins: []string{"macbook-pro-44"}}), st, Config{})
	h := &harness{t: t, srv: srv, h: srv.Handler(), st: st, who: who}
	var inv struct{ Code string }
	h.do(macAddr, "POST", "/v1/admin/invite", `{"name":"grokbot"}`, http.StatusOK, &inv)
	h.do(grokAddr, "POST", "/v1/join", `{"code":"`+inv.Code+`"}`, http.StatusOK, nil)
	up := h.upload(grokAddr, "a.txt", "text/plain", []byte("hi"))
	fi, err := os.Stat(filepath.Join(base, "attachments", up.ID))
	if err != nil {
		t.Fatalf("blob not beside the database: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("blob mode = %v, want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Join(base, "attachments"))
	if err != nil || di.Mode().Perm() != 0o700 {
		t.Fatalf("attachment dir mode = %v, %v; want 0700", di.Mode().Perm(), err)
	}
}
