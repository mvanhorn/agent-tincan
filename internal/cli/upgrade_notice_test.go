package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

func TestUpgradeInboxAndWait(t *testing.T) {
	old := client.Version
	client.Version = "0.5.4"
	t.Cleanup(func() { client.Version = old })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"requests": []any{map[string]any{"id": "request-1", "body": "hello"}}, "upgrade_available": "92.0.0"})
	}))
	defer srv.Close()
	r, _ := client.NewRelay(srv.URL, "")
	var out, errOut bytes.Buffer
	if err := checkInboxJSON(t.Context(), r, 0, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["upgrade_available"] != "92.0.0" {
		t.Fatalf("JSON = %s", out.String())
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	in, _, err := waitForInbox(ctx, r, 0, client.RepliesKeep)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Requests) != 1 || in.Requests[0].ID != "request-1" {
		t.Fatalf("wait requests = %+v", in.Requests)
	}
	if got := formatWait(ctx, r, in); !strings.Contains(got, "92.0.0 is available") || !strings.Contains(got, "hello") {
		t.Fatalf("wait = %q", got)
	}
	out.Reset()
	if err := checkInbox(ctx, r, 0, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "upgrade") {
		t.Fatalf("duplicate across paths: %s", out.String())
	}
}

func TestListenUpgradeEnvironment(t *testing.T) {
	skipOnWindows(t, skipNoShell)
	old := client.Version
	client.Version = "0.5.4"
	t.Cleanup(func() { client.Version = old })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"waiting": 0, "upgrade_available": "93.0.0"})
	}))
	defer srv.Close()
	r, _ := client.NewRelay(srv.URL, "")
	path := filepath.Join(t.TempDir(), "notice")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := listen(ctx, r, `printf '%s:%s' "$TINCAN_WAITING" "$TINCAN_UPGRADE_AVAILABLE" > `+path, true); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "0:93.0.0" {
		t.Fatalf("environment = %q, %v", got, err)
	}
}

type upgradePusher struct {
	calls   int
	cancel  context.CancelFunc
	content string
}

func (p *upgradePusher) Ready() <-chan struct{} { ch := make(chan struct{}); close(ch); return ch }
func (p *upgradePusher) Push(_ context.Context, content string, _ map[string]string) error {
	p.calls++
	if p.calls == 1 {
		return errors.New("temporary failure")
	}
	p.content = content
	p.cancel()
	return nil
}

func TestChannelUpgradeRetriesAndDeduplicates(t *testing.T) {
	old := client.Version
	client.Version = "0.5.4"
	t.Cleanup(func() { client.Version = old })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"waiting": 0, "upgrade_available": "94.0.0"})
	}))
	defer srv.Close()
	r, _ := client.NewRelay(srv.URL, "")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	p := &upgradePusher{cancel: cancel}
	pushWaiting(ctx, r, p)
	if p.calls != 2 || !strings.Contains(p.content, "94.0.0 is available") {
		t.Fatalf("push = %+v", p)
	}
	if got := client.UpgradeNotice("94.0.0"); !strings.Contains(got, "94.0.0 is available") {
		t.Fatalf("a channel event hid the inbox notice: %q", got)
	}
	if err := client.ReportUpgradeOn(client.UpgradeSurfaceChannel, "94.0.0", func(string) error {
		t.Fatal("channel notice repeated")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeInboxText(t *testing.T) {
	old := client.Version
	client.Version = "0.5.4"
	t.Cleanup(func() { client.Version = old })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"requests": []any{}, "upgrade_available": "96.0.0"})
	}))
	defer srv.Close()
	r, _ := client.NewRelay(srv.URL, "")
	var out, errOut bytes.Buffer
	if err := checkInbox(t.Context(), r, 0, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "96.0.0 is available") {
		t.Fatalf("inbox = %q", out.String())
	}
}

func TestWaitDoesNotEndForUpgradeAlone(t *testing.T) {
	old := client.Version
	client.Version = "0.5.4"
	t.Cleanup(func() { client.Version = old })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"requests": []any{}, "upgrade_available": "97.0.0"})
	}))
	defer srv.Close()
	r, _ := client.NewRelay(srv.URL, "")
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := waitForInbox(ctx, r, 0, client.RepliesKeep); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait error = %v, want deadline exceeded", err)
	}
}

func TestListenUpgradeRetriesAndDeduplicates(t *testing.T) {
	skipOnWindows(t, skipNoShell)
	oldVersion, oldCooldown := client.Version, listenCooldown
	client.Version, listenCooldown = "0.5.4", time.Millisecond
	t.Cleanup(func() { client.Version, listenCooldown = oldVersion, oldCooldown })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"waiting": 0, "upgrade_available": "98.0.0"})
	}))
	defer srv.Close()
	r, _ := client.NewRelay(srv.URL, "")
	path := filepath.Join(t.TempDir(), "attempts")
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	command := `if [ ! -e ` + path + ` ]; then echo failed > ` + path + `; exit 1; fi; echo "$TINCAN_WAITING:$TINCAN_UPGRADE_AVAILABLE" >> ` + path
	if err := listen(ctx, r, command, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("listen error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "failed\n0:98.0.0\n" {
		t.Fatalf("attempts = %q, %v", got, err)
	}
	if err := client.ReportUpgradeOn(client.UpgradeSurfaceListen, "98.0.0", func(string) error {
		t.Fatal("successful listener notice not deduplicated")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestNudgeReturnsCommandFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer srv.Close()
	r, _ := client.NewRelay(srv.URL, "")
	if err := nudge(t.Context(), r, "exit 1", 0, true, "99.0.0"); err == nil {
		t.Fatal("nudge swallowed command failure")
	}
}
