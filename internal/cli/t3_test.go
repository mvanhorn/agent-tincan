package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/t3bridge"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

func TestOnboardKindT3Code(t *testing.T) {
	if !onboard.KnownKind(onboard.KindT3Code) {
		t.Fatal("KindT3Code not in KnownKind")
	}
	if onboard.RuntimeKind("t3") != onboard.KindT3Code || onboard.RuntimeKind("t3-code") != onboard.KindT3Code {
		t.Fatalf("unexpected RuntimeKind for t3: %q", onboard.RuntimeKind("t3"))
	}
	if got := onboard.StockGoodAt("t3", ""); got == "" {
		t.Fatal("StockGoodAt(t3) is empty")
	}
	kit, err := onboard.Build(onboard.Options{
		RelayURL: "http://tincan-relay",
		Owner:    "Matt",
		Roster:   []onboard.Member{{Name: "t3", Kind: onboard.KindT3Code}},
	})
	if err != nil {
		t.Fatalf("onboard.Build: %v", err)
	}
	if len(kit.Agents) != 1 || kit.Agents[0].Kind != onboard.KindT3Code || kit.Agents[0].Wake != "wait" {
		t.Fatalf("unexpected t3 agent block: %+v", kit.Agents)
	}
	if !strings.Contains(kit.Agents[0].Instructions, "watch <thread-url-or-id>") {
		t.Fatalf("t3 instructions missing watch guidance: %q", kit.Agents[0].Instructions)
	}
}

func TestT3ServicePlistAndSystemd(t *testing.T) {
	plist := t3ServicePlist(t3ServiceOptions{
		binary:      "/Users/test/bin/tincan",
		config:      "/Users/test/.config/tincan/t3.json",
		path:        "/usr/local/bin:/usr/bin",
		home:        "/Users/test",
		t3URL:       "http://127.0.0.1:3773",
		defaultProj: "agent-tincan & co",
	})
	for _, want := range []string{
		"<string>com.agenttincan.t3</string>",
		"<string>serve</string>",
		"<string>--t3-url</string>",
		"<string>agent-tincan &amp; co</string>",
		"tincan-t3.log",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing %q:\n%s", want, plist)
		}
	}

	unit := t3ServiceSystemd(t3ServiceOptions{
		binary: "/usr/local/bin/tincan",
		config: "/home/u/.config/tincan/t3.json",
		path:   "/usr/local/bin:/usr/bin",
	})
	if !strings.Contains(unit, "ExecStart=") || !strings.Contains(unit, "t3 serve") {
		t.Fatalf("unexpected systemd unit:\n%s", unit)
	}
}

func TestT3WatchAndUnwatchCLI(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "t3.json")

	cmd := Root()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"t3", "watch", "https://app.t3.codes/threads/env-1/thr-88", "--notify", "grokbot", "--note", "overnight", "--config", cfgPath})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("t3 watch: %v", err)
	}
	snap, err := t3bridge.NewStateStore(t3bridge.StatePathForConfig(cfgPath)).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Watches["thr-88"]) != 1 || snap.Watches["thr-88"][0].NotifyAgent != "grokbot" {
		t.Fatalf("watch not persisted: %+v", snap.Watches)
	}

	cmd2 := Root()
	cmd2.SetOut(&out)
	cmd2.SetArgs([]string{"t3", "unwatch", "thr-88", "--config", cfgPath})
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("t3 unwatch: %v", err)
	}
	snap2, err := t3bridge.NewStateStore(t3bridge.StatePathForConfig(cfgPath)).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap2.Watches["thr-88"]) != 0 {
		t.Fatalf("watch not removed: %+v", snap2.Watches)
	}
}

func TestT3DoctorJSONNeverLeaksToken(t *testing.T) {
	const secretTok = "super-secret-t3-token-do-not-print"
	mesh := testrelay.New(t, relay.Config{})
	t3Relay := mesh.JoinOnMachineOf(t, "muse", "t3")

	home := t.TempDir()
	t3Home := filepath.Join(home, ".t3")
	cfgDir := filepath.Join(home, ".config", "tincan")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(cfgDir, "t3.json")
	if err := client.SaveConfigTo(cfgPath, client.Config{Agent: "t3", Relay: t3Relay.Base()}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]any{
				"_meta":  map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{"name": "T3 Code", "version": "0.1.13"}},
				"events": []map[string]any{{"name": "thread.run_settled"}},
			},
		})
	}))
	defer srv.Close()

	if err := t3bridge.SaveAuth(t3bridge.AuthPathForConfig(cfgPath), t3bridge.AuthConfig{
		Origin:    srv.URL,
		Token:     secretTok,
		IssuedAt:  time.Now().UTC(),
		ExpiresAt: time.Now().UTC().Add(10 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	setHome(t, home)
	cmd := Root()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"t3", "doctor", "--config", cfgPath, "--t3-home", t3Home, "--t3-url", srv.URL, "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("t3 doctor --json failed: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), secretTok) {
		t.Fatalf("t3 doctor leaked bearer token:\n%s", out.String())
	}
	var rep struct {
		OK     bool    `json:"ok"`
		Checks []check `json:"checks"`
	}
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("invalid doctor JSON: %v\n%s", err, out.String())
	}
	if !rep.OK {
		t.Fatalf("expected ok=true, got %+v", rep)
	}
}

func TestRecordT3OutboundResult(t *testing.T) {
	rs := t3bridge.NewRouteStore(filepath.Join(t.TempDir(), "t3-routes.json"))
	recordT3OutboundResult(rs, "thr-9", client.Result{
		Request: envelope.Request{ID: "req-pending", To: "grokbot"},
		Status:  envelope.StatusQueued,
	})
	rt, ok, err := rs.Lookup("req-pending")
	if err != nil || !ok || rt.ThreadID != "thr-9" || rt.InlineDone {
		t.Fatalf("pending route mismatch: %+v, %v, %v", rt, ok, err)
	}

	recordT3OutboundResult(rs, "thr-9", client.Result{
		Request: envelope.Request{ID: "req-pending", To: "grokbot"},
		Status:  envelope.StatusAnswered,
	})
	rt, ok, err = rs.Lookup("req-pending")
	if err != nil || !ok || !rt.InlineDone {
		t.Fatalf("expected inline done marked: %+v, %v, %v", rt, ok, err)
	}
}
