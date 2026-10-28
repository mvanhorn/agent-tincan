package history

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFramingRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	msg := NativeRequest{ID: 7, Op: OpChatGPTList, Args: OpArgs{Count: 3}}
	if err := WriteMessage(&buf, msg, MaxHostMessage); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	n := binary.LittleEndian.Uint32(raw[:4])
	if int(n) != len(raw)-4 {
		t.Fatalf("length prefix %d, body %d", n, len(raw)-4)
	}
	body, err := ReadMessage(&buf, MaxChromeMessage)
	if err != nil {
		t.Fatal(err)
	}
	var got NativeRequest
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != 7 || got.Op != OpChatGPTList || got.Args.Count != 3 {
		t.Fatalf("round trip: %+v", got)
	}
}

func TestFramingOversizeRejected(t *testing.T) {
	var buf bytes.Buffer
	big := strings.Repeat("x", MaxHostMessage)
	if err := WriteMessage(&buf, big, MaxHostMessage); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("write oversize: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatal("oversize write emitted bytes")
	}
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], 2<<20)
	r := io.MultiReader(bytes.NewReader(hdr[:]), strings.NewReader("{}"))
	if _, err := ReadMessage(r, 1<<20); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("read oversize: %v", err)
	}
	binary.LittleEndian.PutUint32(hdr[:], 10)
	if _, err := ReadMessage(io.MultiReader(bytes.NewReader(hdr[:]), strings.NewReader("{}")), 1<<20); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated body: %v", err)
	}
	if _, err := ReadMessage(strings.NewReader(""), 1<<20); !errors.Is(err, io.EOF) {
		t.Fatalf("clean EOF: %v", err)
	}
	if MaxHostMessage != 1<<20 {
		t.Fatalf("host to Chrome limit is 1 MB, got %d", MaxHostMessage)
	}
	if MaxChromeMessage <= MaxHostMessage {
		t.Fatal("inbound limit should be larger than outbound")
	}
}

func TestValidateOp(t *testing.T) {
	ok := []struct {
		op Op
		a  OpArgs
	}{
		{OpChatGPTList, OpArgs{Count: 1}},
		{OpClaudeAIList, OpArgs{Count: 100}},
		{OpChatGPTDetail, OpArgs{ID: "6a1f0c2e-1111-4a2b-9c3d-000000000001"}},
		{OpClaudeAIDetail, OpArgs{ID: "c1a0d000-0000-4000-8000-000000000001"}},
		{OpChatGPTFile, OpArgs{FileID: "file-Sk3tchAbc123", ConversationID: "6a1f0c2e-1111-4a2b-9c3d-000000000001"}},
		{OpChatGPTFile, OpArgs{FileID: "file_00000000abcd1234"}},
		{OpClaudeAIFile, OpArgs{FileID: "f11e0000-0000-4000-8000-0000000000aa"}},
		{OpChatGPTSend, OpArgs{Message: "hello"}},
		{OpChatGPTSend, OpArgs{Message: "hi", NewChat: true}},
		{OpClaudeAISend, OpArgs{Message: strings.Repeat("x", MaxSendMessage), ConversationID: "c1a0d000-0000-4000-8000-000000000001"}},
		{OpChatGPTClose, OpArgs{ConversationID: "6a1f0c2e-1111-4a2b-9c3d-000000000001"}},
		{OpClaudeAIClose, OpArgs{ConversationID: "c1a0d000-0000-4000-8000-000000000001"}},
		{OpExtensionReload, OpArgs{}},
	}
	for _, c := range ok {
		if err := ValidateOp(c.op, c.a); err != nil {
			t.Errorf("%s %+v: %v", c.op, c.a, err)
		}
	}
	bad := []struct {
		op Op
		a  OpArgs
	}{
		{"chatgpt.eval", OpArgs{Count: 1}},
		{"", OpArgs{}},
		{OpChatGPTList, OpArgs{Count: 0}},
		{OpChatGPTList, OpArgs{Count: 101}},
		{OpChatGPTList, OpArgs{Count: 1, ID: "abc"}},
		{OpChatGPTDetail, OpArgs{ID: "../../backend-api/me"}},
		{OpChatGPTDetail, OpArgs{ID: "abc?x=1"}},
		{OpChatGPTDetail, OpArgs{ID: "a.b"}},
		{OpChatGPTDetail, OpArgs{ID: ""}},
		{OpChatGPTDetail, OpArgs{ID: strings.Repeat("a", 129)}},
		{OpClaudeAIFile, OpArgs{FileID: "x/y"}},
		{OpClaudeAIFile, OpArgs{FileID: "f1", ConversationID: "c1"}},
		{OpChatGPTFile, OpArgs{}},
		{OpChatGPTList, OpArgs{Count: 1, Message: "hi"}},
		{OpChatGPTDetail, OpArgs{ID: "abc", NewChat: true}},
		{OpChatGPTSend, OpArgs{}},
		{OpChatGPTSend, OpArgs{Message: " \n\t"}},
		{OpChatGPTSend, OpArgs{Message: strings.Repeat("x", MaxSendMessage+1)}},
		{OpChatGPTSend, OpArgs{Message: "bad \xff utf8"}},
		{OpChatGPTSend, OpArgs{Message: "hi", ConversationID: "../c/x"}},
		{OpChatGPTSend, OpArgs{Message: "hi", ConversationID: "abc", NewChat: true}},
		{OpClaudeAISend, OpArgs{Message: "hi", ID: "abc"}},
		{OpClaudeAISend, OpArgs{Message: "hi", Count: 1}},
		{OpChatGPTClose, OpArgs{}},
		{OpChatGPTClose, OpArgs{ConversationID: "../c/x"}},
		{OpClaudeAIClose, OpArgs{ConversationID: "abc", ID: "abc"}},
		{OpClaudeAIClose, OpArgs{ConversationID: "abc", Message: "hi"}},
		{OpExtensionReload, OpArgs{Count: 1}},
		{OpExtensionReload, OpArgs{Message: "x"}},
	}
	for _, c := range bad {
		if err := ValidateOp(c.op, c.a); err == nil {
			t.Errorf("%q %+v: want rejection", c.op, c.a)
		}
	}
}

func TestNativeManifestDir(t *testing.T) {
	home := "/home/u"
	cases := map[string]string{
		"darwin":  "/home/u/Library/Application Support/Google/Chrome/NativeMessagingHosts",
		"linux":   "/home/u/.config/google-chrome/NativeMessagingHosts",
		"windows": filepath.Join(home, "AppData", "Local", "AgentTincan", "NativeMessagingHosts"),
	}
	for goos, want := range cases {
		got, err := NativeManifestDir(goos, home)
		if err != nil || got != want {
			t.Errorf("%s: got %q %v, want %q", goos, got, err, want)
		}
	}
	if _, err := NativeManifestDir("plan9", home); err == nil {
		t.Error("plan9: want error")
	}
}

func readManifest(t *testing.T, path string) HostManifest {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m HostManifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestInstallNativeHost(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			home := t.TempDir()
			res, err := InstallNativeHost(InstallOptions{
				GOOS:        goos,
				Home:        home,
				Binary:      "/opt/it's here/tincan",
				ExtensionID: DefaultExtensionID,
				NativeDir:   filepath.Join(home, ".config", "tincan", "history-native"),
			})
			if err != nil {
				t.Fatal(err)
			}
			dir, _ := NativeManifestDir(goos, home)
			if res.ManifestPath != filepath.Join(dir, NativeHostName+".json") {
				t.Fatalf("manifest path %s", res.ManifestPath)
			}
			m := readManifest(t, res.ManifestPath)
			if m.Name != NativeHostName || m.Type != "stdio" || m.Path != res.WrapperPath || m.Description == "" {
				t.Fatalf("manifest %+v", m)
			}
			if len(m.AllowedOrigins) != 2 || m.AllowedOrigins[0] != "chrome-extension://"+DefaultExtensionID+"/" ||
				m.AllowedOrigins[1] != "chrome-extension://"+StoreExtensionID+"/" {
				t.Fatalf("allowed_origins %v", m.AllowedOrigins)
			}
			st, err := os.Stat(res.WrapperPath)
			if err != nil || st.Mode().Perm() != 0o700 {
				t.Fatalf("wrapper %v %v", st, err)
			}
			w, _ := os.ReadFile(res.WrapperPath)
			if !strings.HasPrefix(string(w), "#!/bin/sh\n") || !strings.Contains(string(w), `exec '/opt/it'\''s here/tincan' history native-host "$@"`) {
				t.Fatalf("wrapper:\n%s", w)
			}
			if res.Note != "" {
				t.Fatalf("unexpected note %q", res.Note)
			}
		})
	}
	t.Run("windows", func(t *testing.T) {
		home := t.TempDir()
		res, err := InstallNativeHost(InstallOptions{GOOS: "windows", Home: home, Binary: `C:\tincan\tincan.exe`, ExtensionID: DefaultExtensionID, NativeDir: filepath.Join(home, "native")})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(res.WrapperPath, ".bat") {
			t.Fatalf("wrapper %s", res.WrapperPath)
		}
		w, _ := os.ReadFile(res.WrapperPath)
		if !strings.Contains(string(w), `"C:\tincan\tincan.exe" history native-host %*`) {
			t.Fatalf("bat:\n%s", w)
		}
		if !strings.Contains(res.Note, `reg add "HKCU\Software\Google\Chrome\NativeMessagingHosts\`+NativeHostName+`"`) || !strings.Contains(res.Note, res.ManifestPath) {
			t.Fatalf("note %q", res.Note)
		}
	})
	t.Run("bad extension id", func(t *testing.T) {
		home := t.TempDir()
		for _, id := range []string{"ABCDEFGHIJKLMNOPABCDEFGHIJKLMNOP", "abc", strings.Repeat("q", 32)} {
			if _, err := InstallNativeHost(InstallOptions{GOOS: "linux", Home: home, Binary: "/bin/tincan", ExtensionID: id, NativeDir: filepath.Join(home, "n")}); err == nil {
				t.Errorf("id %q accepted", id)
			}
		}
	})
}

func TestExtensionIDFromManifestKey(t *testing.T) {
	b, err := os.ReadFile("../../extension/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Key             string   `json:"key"`
		ManifestVersion int      `json:"manifest_version"`
		HostPermissions []string `json:"host_permissions"`
		Permissions     []string `json:"permissions"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	id, err := ExtensionIDFromKey(m.Key)
	if err != nil {
		t.Fatal(err)
	}
	if id != DefaultExtensionID {
		t.Fatalf("manifest key gives id %s, DefaultExtensionID is %s", id, DefaultExtensionID)
	}
	if m.ManifestVersion != 3 {
		t.Fatalf("manifest_version %d", m.ManifestVersion)
	}
	hp := strings.Join(m.HostPermissions, " ")
	for _, want := range []string{"https://chatgpt.com/*", "https://claude.ai/*"} {
		if !strings.Contains(hp, want) {
			t.Fatalf("host_permissions missing %s: %v", want, m.HostPermissions)
		}
	}
	if !strings.Contains(strings.Join(m.Permissions, " "), "nativeMessaging") {
		t.Fatalf("permissions %v", m.Permissions)
	}
	if _, err := ExtensionIDFromKey("not base64!"); err == nil {
		t.Fatal("bad key accepted")
	}
}

// shortDir returns a short temp dir; unix socket paths are limited to about
// 104 bytes on macOS.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "tch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func TestListenSocketPerms(t *testing.T) {
	base := shortDir(t)
	dir := filepath.Join(base, "native")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "host.sock")
	// A stale socket file from a crashed host is replaced.
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	ln, err := ListenSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	st, err := os.Stat(dir)
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v %v", st.Mode().Perm(), err)
	}
	st, err = os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 || st.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket mode %v %v", st.Mode(), err)
	}
	// A regular file at the socket path is never deleted.
	reg := filepath.Join(dir, "regular")
	os.WriteFile(reg, []byte("keep"), 0o600)
	if _, err := ListenSocket(reg); err == nil {
		t.Fatal("listened over a regular file")
	}
	if b, _ := os.ReadFile(reg); string(b) != "keep" {
		t.Fatal("regular file clobbered")
	}
}

func TestSocketChannelNoHost(t *testing.T) {
	path := filepath.Join(shortDir(t), "none.sock")
	for _, c := range []struct {
		running bool
		want    error
	}{{false, ErrChromeNotRunning}, {true, ErrExtensionNotConnected}} {
		ch := &SocketChannel{Path: path, ChromeRunning: func() bool { return c.running }}
		err := ch.Exchange(context.Background(), NativeRequest{Op: OpChatGPTList, Args: OpArgs{Count: 1}}, func(NativeResponse) (bool, error) { return true, nil })
		if !errors.Is(err, c.want) {
			t.Fatalf("running=%v: got %v want %v", c.running, err, c.want)
		}
	}
}

// fakeExtension plays Chrome's side of the native messaging pipe: it reads
// requests the host writes and answers with handle.
func fakeExtension(t *testing.T, fromHost io.Reader, toHost io.Writer, seen chan<- NativeRequest, handle func(NativeRequest) []NativeResponse) {
	t.Helper()
	go func() {
		for {
			b, err := ReadMessage(fromHost, MaxHostMessage)
			if err != nil {
				return
			}
			var req NativeRequest
			if err := json.Unmarshal(b, &req); err != nil {
				t.Errorf("host sent non-JSON: %v", err)
				return
			}
			if seen != nil {
				seen <- req
			}
			for _, fr := range handle(req) {
				fr.ID = req.ID
				if err := WriteMessage(toHost, fr, MaxChromeMessage); err != nil {
					return
				}
			}
		}
	}()
}

func TestNativeHostBridge(t *testing.T) {
	dir := filepath.Join(shortDir(t), "n")
	sock := filepath.Join(dir, "host.sock")
	chromeToHostR, chromeToHostW := io.Pipe()
	hostToChromeR, hostToChromeW := io.Pipe()
	seen := make(chan NativeRequest, 16)
	png := fakePNG(700 << 10)
	fakeExtension(t, hostToChromeR, chromeToHostW, seen, func(req NativeRequest) []NativeResponse {
		switch req.Op {
		case OpChatGPTList:
			return []NativeResponse{{OK: true, Result: json.RawMessage(`{"items":[]}`)}}
		case OpChatGPTFile:
			return chunkFrames(png, "image/png", 256<<10)
		}
		return []NativeResponse{{Error: &NativeError{Code: "bad_request", Message: "unknown op"}}}
	})
	host := &NativeHost{SocketPath: sock, In: chromeToHostR, Out: hostToChromeW, RequestTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- host.Run(context.Background()) }()
	waitFor(t, func() bool { _, err := os.Stat(sock); return err == nil })

	c := &Client{Channel: &SocketChannel{Path: sock, ChromeRunning: func() bool { return true }}, Timeout: 5 * time.Second}
	res, err := c.Request(context.Background(), OpChatGPTList, OpArgs{Count: 5})
	if err != nil || string(res) != `{"items":[]}` {
		t.Fatalf("list via host: %s %v", res, err)
	}
	if r := <-seen; r.Op != OpChatGPTList || r.Args.Count != 5 {
		t.Fatalf("extension saw %+v", r)
	}
	data, mime, err := c.File(context.Background(), OpChatGPTFile, OpArgs{FileID: "file-Sk3tchAbc123"})
	if err != nil || mime != "image/png" || !bytes.Equal(data, png) {
		t.Fatalf("file via host: %d bytes %s %v", len(data), mime, err)
	}
	<-seen

	// A malformed request sent straight to the socket is refused by the
	// host and never reaches the extension.
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	WriteMessage(conn, map[string]any{"id": 1, "op": "chatgpt.detail", "args": map[string]any{"id": "../me"}}, MaxHostMessage)
	b, err := ReadMessage(conn, MaxChromeMessage)
	conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	var resp NativeResponse
	json.Unmarshal(b, &resp)
	if resp.OK || resp.Error == nil || resp.Error.Code != "bad_request" {
		t.Fatalf("malformed request answer: %s", b)
	}
	select {
	case r := <-seen:
		t.Fatalf("malformed request reached the extension: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}

	// Chrome closing the pipe ends the host and removes the socket.
	chromeToHostW.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("host did not exit when Chrome closed stdin")
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket left behind: %v", err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestClientTimeouts(t *testing.T) {
	c := &Client{}
	if c.timeout(OpChatGPTSend) != SendClientTimeout || c.timeout(OpClaudeAISend) != SendClientTimeout {
		t.Fatal("send ops do not get the send timeout")
	}
	if c.timeout(OpChatGPTFile) != 90*time.Second || c.timeout(OpChatGPTList) != 30*time.Second {
		t.Fatal("read timeouts changed")
	}
	if SendHostTimeout <= 5*time.Minute || SendClientTimeout <= SendHostTimeout {
		t.Fatal("timeouts must nest: extension 5m < host < client")
	}
}

// A send goes through the host with its message intact and gets the long
// send timeout; extension.reload is never relayed from the socket.
func TestNativeHostRelaysSendRefusesReload(t *testing.T) {
	dir := filepath.Join(shortDir(t), "n")
	sock := filepath.Join(dir, "host.sock")
	chromeToHostR, chromeToHostW := io.Pipe()
	hostToChromeR, hostToChromeW := io.Pipe()
	seen := make(chan NativeRequest, 16)
	message := "line one\n<script>alert(1)</script> \"quoted\" " + strings.Repeat("<", 20000)
	fakeExtension(t, hostToChromeR, chromeToHostW, seen, func(req NativeRequest) []NativeResponse {
		if req.Op == OpChatGPTSend {
			// Answer after the plain request timeout, inside the send one.
			time.Sleep(300 * time.Millisecond)
			return []NativeResponse{{OK: true, Result: json.RawMessage(`{"conversation_id":"conv-9","url":"https://chatgpt.com/c/conv-9","submitted_at":1790000000123}`)}}
		}
		return []NativeResponse{{Error: &NativeError{Code: "bad_request", Message: "unknown op"}}}
	})
	host := &NativeHost{SocketPath: sock, In: chromeToHostR, Out: hostToChromeW, RequestTimeout: 100 * time.Millisecond, SendTimeout: 5 * time.Second}
	go func() { _ = host.Run(context.Background()) }()
	defer chromeToHostW.Close()
	waitFor(t, func() bool { _, err := os.Stat(sock); return err == nil })

	c := &Client{Channel: &SocketChannel{Path: sock, ChromeRunning: func() bool { return true }}, Timeout: 5 * time.Second}
	res, err := c.Send(context.Background(), SourceChatGPT, message, "", true)
	if err != nil || res.ConversationID != "conv-9" || !res.Submitted().Equal(time.UnixMilli(1790000000123)) {
		t.Fatalf("send via host: %+v %v", res, err)
	}
	if r := <-seen; r.Op != OpChatGPTSend || r.Args.Message != message || !r.Args.NewChat {
		t.Fatalf("extension saw %+v", r.Args.NewChat)
	}

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	_ = WriteMessage(conn, NativeRequest{ID: 3, Op: OpExtensionReload, Args: OpArgs{}}, MaxHostMessage)
	b, err := ReadMessage(conn, MaxChromeMessage)
	conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	var resp NativeResponse
	_ = json.Unmarshal(b, &resp)
	if resp.OK || resp.Error == nil || resp.Error.Code != "bad_request" {
		t.Fatalf("reload from the socket: %s", b)
	}
	select {
	case r := <-seen:
		t.Fatalf("reload from the socket reached the extension: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSendErrorCodes(t *testing.T) {
	for code, want := range map[string]error{
		"not_logged_in":      ErrNotLoggedIn,
		"composer_not_found": ErrComposerNotFound,
		"send_failed":        ErrSendFailed,
		"page_error":         ErrSendFailed,
		"timeout":            ErrTimeout,
		"not_found":          ErrNotFound,
		"unsupported":        ErrRejected,
		"bad_request":        ErrRejected,
	} {
		ch := channelFunc(func(_ context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error {
			_, err := recv(NativeResponse{ID: req.ID, Error: &NativeError{Code: code, Message: "detail"}})
			return err
		})
		_, err := (&Client{Channel: ch}).Send(context.Background(), SourceClaudeAI, "hi", "", false)
		if !errors.Is(err, want) {
			t.Errorf("%s: got %v, want %v", code, err, want)
		}
	}
	ch := channelFunc(func(_ context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error {
		_, err := recv(NativeResponse{ID: req.ID, OK: true, Result: json.RawMessage(`{"conversation_id":"../x"}`)})
		return err
	})
	if _, err := (&Client{Channel: ch}).Send(context.Background(), SourceChatGPT, "hi", "", false); !errors.Is(err, ErrEndpointChanged) {
		t.Fatalf("bad conversation id in the answer: %v", err)
	}
}

// channelFunc adapts a function to Channel.
type channelFunc func(ctx context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error

func (f channelFunc) Exchange(ctx context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error {
	return f(ctx, req, recv)
}

// writeExtensionDir writes a fake unpacked extension and returns the hello
// the loaded copy of exactly these files would send.
func writeExtensionDir(t *testing.T, dir, version string, files map[string]string) Hello {
	t.Helper()
	files["manifest.json"] = `{"manifest_version":3,"version":"` + version + `"}`
	h := Hello{Version: version, Unpacked: true, Files: map[string]string{}}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		sum, err := fileSHA256(p)
		if err != nil {
			t.Fatal(err)
		}
		h.Files[name] = sum
	}
	return h
}

func TestExtensionDrift(t *testing.T) {
	dir := t.TempDir()
	h := writeExtensionDir(t, dir, "0.2.0", map[string]string{"ops.js": "a", "send.js": "b"})
	reason, fp, err := ExtensionDrift(dir, h)
	if err != nil || reason != "" || fp == "" {
		t.Fatalf("same files: %q %q %v", reason, fp, err)
	}
	old := h
	old.Version = "0.1.0"
	if r, _, _ := ExtensionDrift(dir, old); !strings.Contains(r, "0.1.0 is loaded, 0.2.0 is on disk") {
		t.Fatalf("version drift: %q", r)
	}
	if err := os.WriteFile(filepath.Join(dir, "send.js"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, fp2, _ := ExtensionDrift(dir, h)
	if r != "send.js changed on disk" || fp2 == fp {
		t.Fatalf("content drift: %q (fingerprint changed: %v)", r, fp2 != fp)
	}
	// A reported name that is not a plain file name is ignored, never read.
	evil := Hello{Version: "0.2.0", Unpacked: true, Files: map[string]string{"../../etc/passwd": "x", "manifest.json": h.Files["manifest.json"]}}
	if r, _, err := ExtensionDrift(dir, evil); err != nil || r != "" {
		t.Fatalf("path in a hello: %q %v", r, err)
	}
	if _, _, err := ExtensionDrift(t.TempDir(), h); err == nil {
		t.Fatal("dir without a manifest accepted")
	}
}

// The host asks for a reload when the unpacked files on disk differ from
// the loaded ones, never for a store install or matching files, and only
// once per cooldown for the same files.
func TestNativeHostHelloTriggersReload(t *testing.T) {
	extDir := t.TempDir()
	loaded := writeExtensionDir(t, extDir, "0.1.0", map[string]string{"ops.js": "old"})
	onDisk := writeExtensionDir(t, extDir, "0.2.0", map[string]string{"ops.js": "new", "send.js": "new file"})

	sock := filepath.Join(shortDir(t), "n", "host.sock")
	chromeToHostR, chromeToHostW := io.Pipe()
	hostToChromeR, hostToChromeW := io.Pipe()
	var logBuf lockedBuffer
	host := &NativeHost{SocketPath: sock, In: chromeToHostR, Out: hostToChromeW, ExtensionDir: extDir, Log: &logBuf}
	go func() { _ = host.Run(context.Background()) }()
	defer chromeToHostW.Close()
	waitFor(t, func() bool { _, err := os.Stat(sock); return err == nil })

	fromHost := make(chan NativeRequest, 8)
	go func() {
		for {
			b, err := ReadMessage(hostToChromeR, MaxHostMessage)
			if err != nil {
				return
			}
			var r NativeRequest
			_ = json.Unmarshal(b, &r)
			fromHost <- r
		}
	}()
	hello := func(h Hello) {
		if err := WriteMessage(chromeToHostW, map[string]any{"id": 0, "hello": h}, MaxChromeMessage); err != nil {
			t.Fatal(err)
		}
	}
	expect := func(want bool, what string) {
		t.Helper()
		select {
		case r := <-fromHost:
			if !want || r.Op != OpExtensionReload || r.Args != (OpArgs{}) {
				t.Fatalf("%s: host sent %+v", what, r)
			}
		case <-time.After(300 * time.Millisecond):
			if want {
				t.Fatalf("%s: no reload request", what)
			}
		}
	}

	hello(onDisk)
	expect(false, "matching files")
	store := loaded
	store.Unpacked = false
	hello(store)
	expect(false, "store install")
	hello(loaded)
	expect(true, "old version loaded")
	// The reload did not take (say the loaded copy lives elsewhere): the
	// same hello again is not answered with another reload.
	hello(loaded)
	expect(false, "second hello toward the same files")
	if !strings.Contains(logBuf.String(), "already asked") {
		t.Fatalf("log: %s", logBuf.String())
	}
	// Once the cooldown has passed, it asks again.
	statePath := filepath.Join(filepath.Dir(sock), "reload-state.json")
	b, _ := os.ReadFile(statePath)
	var st reloadState
	_ = json.Unmarshal(b, &st)
	st.At = st.At.Add(-reloadCooldown - time.Minute)
	b, _ = json.Marshal(st)
	if err := os.WriteFile(statePath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	hello(loaded)
	expect(true, "after the cooldown")
	if fi, err := os.Stat(statePath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file: %v %v", fi, err)
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestInstallNativeHostExtensionDir(t *testing.T) {
	home := t.TempDir()
	ext := filepath.Join(home, "agent tincan", "extension")
	if err := os.MkdirAll(ext, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ext, "manifest.json"), []byte(`{"version":"0.2.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := InstallNativeHost(InstallOptions{GOOS: "darwin", Home: home, Binary: "/opt/tincan", NativeDir: filepath.Join(home, "n"), ExtensionDir: ext})
	if err != nil {
		t.Fatal(err)
	}
	w, _ := os.ReadFile(res.WrapperPath)
	want := "#!/bin/sh\nexport " + ExtensionDirEnv + "='" + ext + "'\nexec '/opt/tincan' history native-host \"$@\"\n"
	if string(w) != want {
		t.Fatalf("wrapper:\n%s\nwant:\n%s", w, want)
	}
	for _, bad := range []string{"relative/extension", filepath.Join(home, "missing"), ext + "'; rm -rf ~"} {
		if _, err := InstallNativeHost(InstallOptions{GOOS: "darwin", Home: home, Binary: "/opt/tincan", NativeDir: filepath.Join(home, "n"), ExtensionDir: bad}); err == nil {
			t.Errorf("extension dir %q accepted", bad)
		}
	}
}

func TestOldExtensionUnknownOpSaysReload(t *testing.T) {
	ch := channelFunc(func(_ context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error {
		_, err := recv(NativeResponse{ID: req.ID, Error: &NativeError{Code: "bad_request", Message: "unknown operation"}})
		return err
	})
	_, err := (&Client{Channel: ch}).Send(context.Background(), SourceChatGPT, "hi", "", false)
	if err == nil || !strings.Contains(err.Error(), "reload it once from chrome://extensions") {
		t.Fatalf("err = %v", err)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("pipe closed") }

// A reload request that could not be written does not start the cooldown:
// the next hello toward the same files asks again.
func TestNativeHostFailedReloadWriteDoesNotStartCooldown(t *testing.T) {
	extDir := t.TempDir()
	loaded := writeExtensionDir(t, extDir, "0.1.0", map[string]string{"ops.js": "old"})
	writeExtensionDir(t, extDir, "0.2.0", map[string]string{"ops.js": "new"})
	statePath := filepath.Join(t.TempDir(), "reload-state.json")
	var logBuf lockedBuffer
	host := &NativeHost{Out: failWriter{}, ExtensionDir: extDir, ReloadStatePath: statePath, Log: &logBuf}
	host.onHello(loaded)
	if !strings.Contains(logBuf.String(), "reload request failed") {
		t.Fatalf("log: %s", logBuf.String())
	}
	var out bytes.Buffer
	host.Out = &out
	host.onHello(loaded)
	b, err := ReadMessage(&out, MaxHostMessage)
	if err != nil {
		t.Fatalf("no reload request after a failed write: %v (log: %s)", err, logBuf.String())
	}
	var r NativeRequest
	if err := json.Unmarshal(b, &r); err != nil || r.Op != OpExtensionReload {
		t.Fatalf("host sent %s", b)
	}
	// This one went out, so the cooldown holds now.
	out.Reset()
	host.onHello(loaded)
	if out.Len() != 0 {
		t.Fatalf("asked again inside the cooldown: %q", out.String())
	}
}

// statusHost runs a native host whose extension side is driven by the
// test: hello writes a raw hello frame, as the extension would.
func statusHost(t *testing.T) (c *Client, hello func(v map[string]any), stop func()) {
	t.Helper()
	dir := filepath.Join(shortDir(t), "n")
	sock := filepath.Join(dir, "host.sock")
	chromeToHostR, chromeToHostW := io.Pipe()
	hostToChromeR, hostToChromeW := io.Pipe()
	seen := make(chan NativeRequest, 16)
	fakeExtension(t, hostToChromeR, chromeToHostW, seen, func(req NativeRequest) []NativeResponse {
		t.Errorf("host.status reached the extension: %+v", req)
		return nil
	})
	host := &NativeHost{SocketPath: sock, In: chromeToHostR, Out: hostToChromeW, RequestTimeout: 5 * time.Second}
	go func() { _ = host.Run(context.Background()) }()
	waitFor(t, func() bool { _, err := os.Stat(sock); return err == nil })
	var mu sync.Mutex
	hello = func(v map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		if err := WriteMessage(chromeToHostW, map[string]any{"id": 0, "hello": v}, MaxChromeMessage); err != nil {
			t.Fatal(err)
		}
	}
	c = &Client{Channel: &SocketChannel{Path: sock, ChromeRunning: func() bool { return true }}}
	return c, hello, func() { chromeToHostW.Close() }
}

// The host answers host.status itself from the last hello: an extension
// older than site grants has only ChatGPT and claude.ai, and a new hello
// on the same connection (a grant from the options page) updates the
// list without a reconnect.
func TestNativeHostExtensionStatus(t *testing.T) {
	c, hello, stop := statusHost(t)
	defer stop()
	ctx := context.Background()
	st, err := c.ExtensionStatus(ctx)
	if err != nil || st.Hello {
		t.Fatalf("before any hello: %+v %v", st, err)
	}
	hello(map[string]any{"version": "0.3.2", "unpacked": true, "files": map[string]string{}})
	waitFor(t, func() bool { st, _ = c.ExtensionStatus(ctx); return st.Hello })
	if !slices.Equal(st.Granted, []string{"chatgpt", "claudeai"}) || st.Version != "0.3.2" {
		t.Fatalf("old extension: %+v", st)
	}
	if !st.granted(SourceChatGPT) || !st.granted(SourceClaudeAI) {
		t.Fatalf("old extension grants: %+v", st)
	}
	// Grok is an optional site: an older extension never has it, and web
	// serve --site grok waits for the grant against it.
	if st.granted(SourceGrok) {
		t.Fatalf("old extension grants grok: %+v", st)
	}
	if err := c.CheckSiteGrant(ctx, SourceGrok); !errors.Is(err, ErrPermissionMissing) || !strings.Contains(err.Error(), "no access to grok.com") {
		t.Fatalf("grok without a grant at startup: %v", err)
	}
	hello(map[string]any{"version": "0.4.0", "unpacked": true, "files": map[string]string{}, "granted": []string{"claudeai"}})
	waitFor(t, func() bool { st, _ = c.ExtensionStatus(ctx); return st.Version == "0.4.0" })
	if !slices.Equal(st.Granted, []string{"claudeai"}) || st.granted(SourceChatGPT) {
		t.Fatalf("chatgpt withheld: %+v", st)
	}
	if err := c.CheckSiteGrant(ctx, SourceChatGPT); !errors.Is(err, ErrPermissionMissing) || !strings.Contains(err.Error(), "Extension options") {
		t.Fatalf("ungranted site at startup: %v", err)
	}
	if err := c.CheckSiteGrant(ctx, SourceClaudeAI); err != nil {
		t.Fatalf("granted site at startup: %v", err)
	}
	// The owner grants it: the next hello, same connection.
	hello(map[string]any{"version": "0.4.0", "unpacked": true, "files": map[string]string{}, "granted": []string{"chatgpt", "claudeai", "grok"}})
	waitFor(t, func() bool { return c.CheckSiteGrant(ctx, SourceChatGPT) == nil })
	if err := c.CheckSiteGrant(ctx, SourceGrok); err != nil {
		t.Fatalf("granted grok at startup: %v", err)
	}
	// An empty list is not an old extension: nothing is granted.
	hello(map[string]any{"version": "0.4.0", "unpacked": true, "files": map[string]string{}, "granted": []string{}})
	waitFor(t, func() bool { return c.CheckSiteGrant(ctx, SourceClaudeAI) != nil })
}

// With no extension connected (Chrome not started yet, as at login) or an
// older host that does not know host.status, the startup check passes:
// web serve starts and answers each request with the matching reply.
func TestCheckSiteGrantWithoutConnectedExtension(t *testing.T) {
	ctx := context.Background()
	none := &Client{Channel: &SocketChannel{Path: filepath.Join(shortDir(t), "none.sock"), ChromeRunning: func() bool { return false }}}
	if err := none.CheckSiteGrant(ctx, SourceChatGPT); err != nil {
		t.Fatalf("no host: %v", err)
	}
	old := &Client{Channel: channelFunc(func(_ context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error {
		_, err := recv(NativeResponse{ID: req.ID, Error: &NativeError{Code: "bad_request", Message: `unknown operation "host.status"`}})
		return err
	})}
	if err := old.CheckSiteGrant(ctx, SourceChatGPT); err != nil {
		t.Fatalf("older host: %v", err)
	}
	if err := (&Client{}).CheckSiteGrant(ctx, SourceChatGPT); err != nil {
		t.Fatalf("no channel: %v", err)
	}
}

func TestPermissionMissingAndBlockedErrorCodes(t *testing.T) {
	for code, want := range map[string]error{"permission_missing": ErrPermissionMissing, "blocked": ErrBlocked} {
		err := fromNativeError(SourceClaudeAI, &NativeError{Code: code, Message: "detail"})
		if !errors.Is(err, want) {
			t.Errorf("%s: %v", code, err)
		}
	}
	msg := fromNativeError(SourceClaudeAI, &NativeError{Code: "permission_missing", Message: "detail"}).Error()
	if !strings.Contains(msg, "claude.ai") || !strings.Contains(msg, "chrome://extensions > Agent Tincan History > Details > Extension options") {
		t.Fatalf("permission_missing reply: %s", msg)
	}
}

// A request whose caller's own deadline runs out is the caller running out
// of time, not Chrome failing to answer: only a call that hit the
// client's own timeout while the caller still had time is ErrTimeout.
func TestExchangeTellsCallerDeadlineFromChromeTimeout(t *testing.T) {
	blocked := channelFunc(func(ctx context.Context, _ NativeRequest, _ func(NativeResponse) (bool, error)) error {
		<-ctx.Done()
		return ctx.Err()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := (&Client{Channel: blocked}).Request(ctx, OpChatGPTList, OpArgs{Count: 5})
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrTimeout) {
		t.Fatalf("caller deadline: got %v, want context.DeadlineExceeded and not ErrTimeout", err)
	}

	_, err = (&Client{Channel: blocked, Timeout: 20 * time.Millisecond}).Request(context.Background(), OpChatGPTList, OpArgs{Count: 5})
	if !errors.Is(err, ErrTimeout) || !strings.Contains(err.Error(), "Chrome did not answer in time") {
		t.Fatalf("client timeout: got %v, want ErrTimeout", err)
	}
}

// pastDeadline is a context whose deadline has passed but whose timer has
// not fired yet: the moment a socket deadline at the same instant times
// out first.
type pastDeadline struct{ context.Context }

func (pastDeadline) Deadline() (time.Time, bool) { return time.Now().Add(-time.Millisecond), true }

// A socket that times out at the caller's deadline, just before the
// caller's context reports it, is still the caller running out of time.
func TestExchangeSocketTimeoutAtCallerDeadline(t *testing.T) {
	ch := channelFunc(func(context.Context, NativeRequest, func(NativeResponse) (bool, error)) error {
		return os.ErrDeadlineExceeded
	})
	_, err := (&Client{Channel: ch}).Request(pastDeadline{context.Background()}, OpChatGPTList, OpArgs{Count: 5})
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v, want context.DeadlineExceeded and not ErrTimeout", err)
	}
}
