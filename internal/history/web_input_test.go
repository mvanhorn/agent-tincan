package history

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

func TestImageInputSitesRequireLiveProof(t *testing.T) {
	for _, site := range webSites {
		if site.imageInput {
			t.Errorf("%s enabled without live proof", site.source)
		}
	}
}

func TestImageRequestGateDoesNotSendText(t *testing.T) {
	rig := newWebRig(t)
	c := rig.mesh.Client(t, "codex")
	upload, err := c.UploadAttachment(t.Context(), "meme.png", "image/png", bytes.NewReader(testPNG), int64(len(testPNG)))
	if err != nil {
		t.Fatal(err)
	}
	req, err := c.SendAttached(t.Context(), "chatgpt-web", "describe this", envelope.KindAsk, "", []string{upload.ID}, false)
	if err != nil {
		t.Fatal(err)
	}
	res := rig.serve(t, c, req.ID)
	if res.Reply.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "meme.png") || !strings.Contains(res.Reply.Body, "live acceptance") {
		t.Fatalf("reply: %+v", res.Reply)
	}
	if rig.browser.exchanges() != 0 {
		t.Fatal("disabled input touched native browser")
	}
}

func TestInputJournalRetainsUnresolvedNewChat(t *testing.T) {
	w := &WebAgent{JournalPath: filepath.Join(t.TempDir(), "journal.json")}
	e := webSend{State: webSendIntent, Recorded: time.Now().Add(-365 * 24 * time.Hour)}
	if err := w.journalStrict("request", e); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.loadJournal().Requests["request"]; !ok {
		t.Fatal("intent pruned")
	}
	if err := os.WriteFile(w.JournalPath, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.loadInputJournal(); err == nil {
		t.Fatal("corrupt journal accepted")
	}
}

func TestInputValidationBoundaries(t *testing.T) {
	if err := validateInputImage(testPNG, "image/png"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		data []byte
		mime string
	}{{testPNG, "image/jpeg"}, {testPNG[:len(testPNG)-10], "image/png"}, {[]byte("<svg/>"), "image/png"}} {
		if validateInputImage(c.data, c.mime) == nil {
			t.Fatal("invalid image accepted")
		}
	}
	a := envelope.Attachment{ID: "id", Name: "a.png", MIME: "image/png", Size: MaxInputFileBytes}
	if err := validateInputMetadata([]envelope.Attachment{a, a}); err != nil {
		t.Fatal(err)
	}
	a.Size = MaxInputTotalBytes / 4
	if err := validateInputMetadata([]envelope.Attachment{a, a, a, a}); err != nil {
		t.Fatal(err)
	}
	for _, input := range [][]envelope.Attachment{
		{a, a, a, a, a},
		{{ID: "id", Name: "a.png", MIME: "image/png", Size: MaxInputFileBytes + 1}},
		{a, {ID: "id", Name: "a.pdf", MIME: "application/pdf", Size: 2}},
		{{ID: "id", Name: "../a.png", MIME: "image/png", Size: 1}},
	} {
		if validateInputMetadata(input) == nil {
			t.Fatal("invalid metadata accepted")
		}
	}
	var out bytes.Buffer
	writer := inputWriter{w: &out, remaining: 3}
	if _, err := writer.Write([]byte("four")); err == nil || out.Len() != 0 {
		t.Fatal("writer exceeded budget")
	}
}

// In-process HTTP exercises recipient downloads without a network listener.
type inputRoundTrip func(*http.Request) (*http.Response, error)

func (f inputRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestInputStageExactBytesAndCleanup(t *testing.T) {
	sum := sha256.Sum256(testPNG)
	calls := 0
	relay := client.NewRelayHTTP("http://relay.test", &http.Client{Transport: inputRoundTrip(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/v1/capabilities" {
			return inputCapabilities(), nil
		}
		calls++
		if req.URL.Path != "/v1/attachments/image-id" {
			t.Errorf("wrong download %s", req.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"image/png"}, http.CanonicalHeaderKey(client.AttachmentSHA256Header): []string{fmt.Sprintf("%x", sum)}}, Body: io.NopCloser(bytes.NewReader(testPNG))}, nil
	})})
	root := t.TempDir()
	w := WebAgent{Relay: relay, TempDir: root}
	a := envelope.Attachment{ID: "image-id", Name: "meme.png", MIME: "image/png", Size: int64(len(testPNG))}
	files, cleanup, err := w.stageInputs(t.Context(), []envelope.Attachment{a, a})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(files) != 2 {
		t.Fatal("image order/count lost")
	}
	for _, f := range files {
		data, err := os.ReadFile(f.path)
		if err != nil || !bytes.Equal(data, testPNG) {
			t.Fatal("staged bytes differ")
		}
		st, err := os.Stat(f.path)
		if err != nil || st.Mode().Perm() != 0600 {
			t.Fatal("unsafe staged file")
		}
		st, err = os.Stat(filepath.Dir(f.path))
		if err != nil || st.Mode().Perm() != 0700 {
			t.Fatal("unsafe staging directory")
		}
		if filepath.Base(f.path) == a.Name {
			t.Fatal("sender filename used as path")
		}
	}
	cleanup()
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatal("staging leaked")
	}
	a.Size++
	_, cleanup, err = w.stageInputs(t.Context(), []envelope.Attachment{a})
	cleanup()
	if err == nil {
		t.Fatal("size mismatch accepted")
	}
	entries, _ = os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatal("failure leaked staging")
	}
}

func TestInputHandleBoundariesWithoutNetwork(t *testing.T) {
	for _, scenario := range []string{"chatgpt", "claude-ai", "grok", "gemini", "perplexity", "copilot", "dots", "denied", "held", "intent", "corrupt"} {
		t.Run(scenario, func(t *testing.T) {
			var reply struct {
				Body   string `json:"body"`
				Status string `json:"status"`
			}
			downloads := 0
			r := client.NewRelayHTTP("http://relay.test", &http.Client{Transport: inputRoundTrip(func(req *http.Request) (*http.Response, error) {
				status := 200
				if strings.HasSuffix(req.URL.Path, "/reply") {
					if err := json.NewDecoder(req.Body).Decode(&reply); err != nil {
						t.Fatal(err)
					}
				}
				if strings.HasSuffix(req.URL.Path, "/claim") && scenario == "held" {
					status = 409
				}
				if strings.Contains(req.URL.Path, "/attachments/") {
					downloads++
					t.Error("unauthorized/disabled attachment download")
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			})})
			browser := newFakeBrowser()
			w := WebAgent{Relay: r, Native: &Client{Channel: browser}, Name: "chatgpt-web", Site: SourceChatGPT, Allowlist: StaticAllowlist("codex"), JournalPath: filepath.Join(t.TempDir(), "journal.json"), Thread: "01234567-89ab-cdef-0123-456789abcdef"}
			if siteFor(Source(scenario)) != nil {
				w.Site = Source(scenario)
			}
			req := envelope.Request{ID: "r1", From: "codex", To: w.Name, Body: "look", Chain: []string{"codex"}, Attachments: []envelope.Attachment{{ID: "f1", Name: "meme.png", MIME: "image/png", Size: 10}}}
			if scenario == "denied" {
				req.Chain = []string{"stranger", "codex"}
			}
			if scenario == "intent" {
				if err := w.journalStrict(req.ID, webSend{State: webSendIntent, Recorded: time.Now()}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "corrupt" {
				if err := os.WriteFile(w.JournalPath, []byte("broken"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			w.Handle(t.Context(), req)
			if downloads != 0 || browser.exchanges() != 0 {
				t.Fatal("boundary made external input operation")
			}
			want := "live acceptance"
			switch scenario {
			case "denied":
				want = ""
				if reply.Status != string(envelope.StatusDeclined) {
					t.Fatalf("denial %+v", reply)
				}
			case "held":
				want = ""
				if reply.Body != "" {
					t.Fatal("held request answered")
				}
			case "intent":
				want = "uncertain"
			case "corrupt":
				want = "journal"
			}
			if !strings.Contains(reply.Body, want) {
				t.Fatalf("got %q, want %q", reply.Body, want)
			}
		})
	}
}

func TestInputStageRejectsMissingOrBadChecksum(t *testing.T) {
	for _, checksum := range []string{"", strings.Repeat("0", 64)} {
		t.Run(checksum, func(t *testing.T) {
			r := client.NewRelayHTTP("http://relay.test", &http.Client{Transport: inputRoundTrip(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/v1/capabilities" {
					return inputCapabilities(), nil
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"image/png"}, http.CanonicalHeaderKey(client.AttachmentSHA256Header): []string{checksum}}, Body: io.NopCloser(bytes.NewReader(testPNG))}, nil
			})})
			w := WebAgent{Relay: r, TempDir: t.TempDir()}
			_, cleanup, err := w.stageInputs(t.Context(), []envelope.Attachment{{ID: "f", Name: "image.png", MIME: "image/png", Size: int64(len(testPNG))}})
			cleanup()
			if err == nil || !strings.Contains(err.Error(), "checksum") {
				t.Fatalf("checksum failure: %v", err)
			}
		})
	}
}

func inputCapabilities() *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"attachments":true,"max_attachments":4,"max_attachment_bytes":10485760}`))}
}

func TestInputPixelAnimationAndJPEGValidation(t *testing.T) {
	huge := bytes.Clone(testPNG)
	binary.BigEndian.PutUint32(huge[16:20], maxInputPixels+1)
	binary.BigEndian.PutUint32(huge[20:24], 1)
	binary.BigEndian.PutUint32(huge[29:33], crc32.ChecksumIEEE(huge[12:29]))
	if err := validateInputImage(huge, "image/png"); err == nil || !strings.Contains(err.Error(), "pixels") {
		t.Fatalf("pixel limit: %v", err)
	}
	chunk := make([]byte, 20)
	binary.BigEndian.PutUint32(chunk[:4], 8)
	copy(chunk[4:8], "acTL")
	binary.BigEndian.PutUint32(chunk[8:12], 2)
	binary.BigEndian.PutUint32(chunk[16:20], crc32.ChecksumIEEE(chunk[4:16]))
	animated := append(bytes.Clone(testPNG[:33]), chunk...)
	animated = append(animated, testPNG[33:]...)
	if err := validateInputImage(animated, "image/png"); err == nil || !strings.Contains(err.Error(), "animated") {
		t.Fatalf("animation: %v", err)
	}
	var jpegBytes bytes.Buffer
	if err := jpeg.Encode(&jpegBytes, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatal(err)
	}
	if err := validateInputImage(jpegBytes.Bytes(), "image/jpeg"); err != nil {
		t.Fatal(err)
	}
}

func TestInputReconcileNeedsUniqueImageEvidence(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	message := func(id string) map[string]any {
		return map[string]any{"id": id, "author": map[string]any{"role": "user"}, "create_time": now.Unix(), "content": map[string]any{"parts": []any{"look", map[string]any{"content_type": "image_asset_pointer", "asset_pointer": "file-service://file-1"}}}, "metadata": map[string]any{"attachments": []any{map[string]any{"id": "file-1", "name": "meme.png", "mime_type": "image/png"}}}}
	}
	mapping := map[string]any{"n1": map[string]any{"parent": "root", "message": message("u1")}, "root": map[string]any{"message": map[string]any{"id": "u0", "author": map[string]any{"role": "user"}}}}
	calls := 0
	w := WebAgent{Site: SourceChatGPT, Native: &Client{Channel: channelFunc(func(_ context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error {
		if req.Op != OpChatGPTDetail {
			t.Fatalf("reconciliation tried %s", req.Op)
		}
		calls++
		raw, _ := json.Marshal(map[string]any{"current_node": "n1", "mapping": mapping})
		_, err := recv(NativeResponse{ID: req.ID, OK: true, Result: raw})
		return err
	})}}
	e := webSend{ConversationID: "c1", PrevUserID: "u0", SubmittedAt: now, State: webSendIntent}
	files := []envelope.Attachment{{Name: "meme.png", MIME: "image/png"}}
	got, ok := w.reconcileInput(t.Context(), "look", files, e)
	if !ok || got.UserMessageID != "u1" || got.State != webSendSent {
		t.Fatalf("reconcile %+v %v", got, ok)
	}
	mapping["n1"].(map[string]any)["parent"] = "n2"
	mapping["n2"] = map[string]any{"parent": "root", "message": message("u2")}
	if _, ok := w.reconcileInput(t.Context(), "look", files, e); ok {
		t.Fatal("ambiguous send accepted")
	}
	e.ConversationID = ""
	before := calls
	if _, ok := w.reconcileInput(t.Context(), "look", files, e); ok || calls != before {
		t.Fatal("new chat intent searched or resent")
	}
}
