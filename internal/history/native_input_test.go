package history

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestInputNativeValidationAndFrames(t *testing.T) {
	f := InputFile{Name: strings.Repeat(`"`, 255), MIME: "image/png", Size: MaxInputFileBytes, SHA256: strings.Repeat("a", 64)}
	requests := []NativeRequest{
		{Op: "chatgpt.input_begin", Input: &InputArgs{Files: []InputFile{f, f}}},
		{Op: "chatgpt.input_chunk", Input: &InputArgs{Token: strings.Repeat("a", 32), Data: base64.StdEncoding.EncodeToString(make([]byte, InputChunkBytes))}},
		{Op: "chatgpt.send_images", Args: OpArgs{Message: strings.Repeat("\x00", MaxSendMessage)}, Input: &InputArgs{Token: strings.Repeat("a", 32)}},
	}
	for _, req := range requests {
		if err := validateInputRequest(req); err != nil {
			t.Fatal(err)
		}
		for _, cap := range []int{maxServiceRequest, MaxHostMessage} {
			var b bytes.Buffer
			if err := WriteMessage(&b, req, cap); err != nil {
				t.Fatal(err)
			}
		}
	}
	bad := []NativeRequest{
		{Op: "grok.input_begin", Input: &InputArgs{Files: []InputFile{f}}},
		{Op: "chatgpt.input_begin", Input: &InputArgs{Connection: "spoof", Files: []InputFile{f}}},
		{Op: "chatgpt.input_begin", Input: &InputArgs{Files: []InputFile{f, f, f}}},
		{Op: "chatgpt.input_begin", Input: &InputArgs{Files: []InputFile{f}, Data: "code"}},
		{Op: "chatgpt.input_chunk", Input: &InputArgs{Token: strings.Repeat("a", 32), Data: "???"}},
		{Op: "chatgpt.send_images", Input: &InputArgs{Token: strings.Repeat("a", 32)}},
	}
	for _, req := range bad {
		if validateInputRequest(req) == nil {
			t.Errorf("accepted %s", req.Op)
		}
	}
	var req NativeRequest
	if decodeNativeRequest([]byte(`{"id":1,"op":"chatgpt.input_begin","args":{},"input":{"url":"https://example.com"}}`), &req) == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestInputHostPersistentConnectionAndDisconnect(t *testing.T) {
	// Only a test fixture may enable the unproven candidate.
	site := siteFor(SourceChatGPT)
	site.imageInput = true
	defer func() { site.imageInput = false }()
	chromeRead, chromeWrite := io.Pipe()
	repliesRead, repliesWrite := io.Pipe()
	defer chromeRead.Close()
	defer chromeWrite.Close()
	defer repliesRead.Close()
	defer repliesWrite.Close()
	h := &NativeHost{In: repliesRead, Out: chromeWrite, pending: map[int64]chan []byte{}, RequestTimeout: time.Second, hello: &Hello{ImageInput: ImageInputCapability{Version: 1, Sites: []string{"chatgpt"}}}}
	go func() { _ = h.readChrome() }()
	a, b := net.Pipe()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	defer a.Close()
	defer b.Close()
	go h.serve(ctx, b)
	seen := make(chan NativeRequest, 4)
	go func() {
		for {
			body, err := ReadMessage(chromeRead, MaxHostMessage)
			if err != nil {
				return
			}
			var req NativeRequest
			if json.Unmarshal(body, &req) != nil {
				return
			}
			seen <- req
			if req.Op == "chatgpt.input_abort" {
				return
			}
			result := json.RawMessage(`{"token":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
			if req.Op == "chatgpt.input_chunk" {
				result = json.RawMessage(`{"seq":0}`)
			}
			if WriteMessage(repliesWrite, NativeResponse{ID: req.ID, OK: true, Result: result}, MaxHostMessage) != nil {
				return
			}
		}
	}()
	session := inputSession{conn: a}
	f := InputFile{Name: "a.png", MIME: "image/png", Size: 1, SHA256: strings.Repeat("a", 64)}
	if _, err := session.request(ctx, NativeRequest{Op: "chatgpt.input_begin", Input: &InputArgs{Files: []InputFile{f}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.request(ctx, NativeRequest{Op: "chatgpt.input_chunk", Input: &InputArgs{Token: strings.Repeat("a", 32), Data: "YQ=="}}); err != nil {
		t.Fatal(err)
	}
	a.Close()
	first, second := <-seen, <-seen
	if first.Input.Connection == "" || second.Input.Connection != first.Input.Connection {
		t.Fatal("connection ownership lost")
	}
	select {
	case abort := <-seen:
		if abort.Op != "chatgpt.input_abort" || abort.Input.Connection != first.Input.Connection {
			t.Fatal("disconnect did not abort")
		}
	case <-ctx.Done():
		t.Fatal("disconnect leaked transfer")
	}
}

func TestInputCapabilityFailsClosed(t *testing.T) {
	for _, s := range []ExtensionStatus{{}, {Hello: true}, {Hello: true, ImageInput: ImageInputCapability{Version: 2, Sites: []string{"chatgpt"}}}} {
		if s.supportsInput(SourceChatGPT) {
			t.Fatal("old/unknown capability accepted")
		}
	}
	if !(ExtensionStatus{Hello: true, ImageInput: ImageInputCapability{Version: 1, Sites: []string{"chatgpt"}}}).supportsInput(SourceChatGPT) {
		t.Fatal("loaded capability rejected")
	}
}

func TestInputNegotiationUsesHostStatus(t *testing.T) {
	site := siteFor(SourceChatGPT)
	site.imageInput = true
	defer func() { site.imageInput = false }()
	for _, good := range []bool{false, true} {
		c := Client{Channel: channelFunc(func(_ context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error {
			if req.Op != OpHostStatus {
				t.Fatalf("unexpected negotiation %s", req.Op)
			}
			raw := json.RawMessage(`{"hello":true,"image_input":{"version":1,"sites":["chatgpt"]}}`)
			if !good {
				raw = json.RawMessage(`{"hello":true}`)
			}
			_, err := recv(NativeResponse{ID: req.ID, OK: true, Result: raw})
			return err
		})}
		err := c.requireImageInput(t.Context(), SourceChatGPT)
		if good && err != nil {
			t.Fatal(err)
		}
		if !good && (err == nil || !strings.Contains(err.Error(), "upgrade")) {
			t.Fatalf("old host failure %v", err)
		}
	}
}
