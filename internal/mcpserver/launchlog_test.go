package mcpserver

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestRecorderNotesTheHandshake(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(ProbeEnv, "")
	rec := StartRecorder(dir, "1.2.3", false)
	if rec == nil {
		t.Fatal("no recorder")
	}
	ls := ReadLaunches(dir)
	if len(ls) != 1 || ls[0].Version != "1.2.3" || !ls[0].Initialized.IsZero() {
		t.Fatalf("after start: %+v", ls)
	}
	// Lines can arrive split across reads.
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"host","version":"7"}}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n"
	half := len(in) / 2
	rec.Write([]byte(in[:half]))
	rec.Write([]byte(in[half:]))
	rec.SetFraming("content-length")
	rec.End(fmt.Errorf("stdin closed"))

	l := ReadLaunches(dir)[0]
	if l.Client != "host 7" || l.Initialized.IsZero() || l.ToolsListed.IsZero() || l.Framing != "content-length" || l.Ended.IsZero() || l.Error != "stdin closed" {
		t.Fatalf("record %+v", l)
	}
}

// MCP 2026-07-28 has no initialize. These are the lines Claude Code 2.1.309
// sent when it negotiated that revision, trimmed to the fields that matter.
func TestRecorderNotesServerDiscover(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(ProbeEnv, "")
	rec := StartRecorder(dir, "1.2.3", false)
	if rec == nil {
		t.Fatal("no recorder")
	}
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"claude-code","title":"Claude Code","version":"2.1.309"}}`
	rec.Write([]byte(`{"jsonrpc":"2.0","id":"server-discover-probe-1","method":"server/discover","params":{` + meta + `}}` + "\n" +
		`{"jsonrpc":"2.0","id":"listen:0","method":"subscriptions/listen","params":{` + meta + `,"notifications":{"toolsListChanged":true}}}` + "\n" +
		`{"method":"server/discover","jsonrpc":"2.0","id":0,"params":{` + meta + `}}` + "\n" +
		`{"method":"tools/list","jsonrpc":"2.0","id":1,"params":{` + meta + `}}` + "\n"))

	l := ReadLaunches(dir)[0]
	if l.Client != "claude-code 2.1.309" || l.Initialized.IsZero() || l.ToolsListed.IsZero() {
		t.Fatalf("record %+v", l)
	}
}

func TestRecorderOffForDoctorProbe(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(ProbeEnv, "1")
	if rec := StartRecorder(dir, "1", false); rec != nil {
		t.Fatal("the doctor's probe must not be recorded as an app launch")
	}
	var nilRec *Recorder
	nilRec.Write([]byte("x\n"))
	nilRec.End(nil)
}

func TestRecorderPrunes(t *testing.T) {
	dir := t.TempDir()
	for i := range keepLaunches + 5 {
		name := fmt.Sprintf("20260101T000000.%09dZ-1.json", i)
		if err := os.WriteFile(dir+"/"+name, []byte(`{"version":"old"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(ProbeEnv, "")
	StartRecorder(dir, "new", false)
	ls := ReadLaunches(dir)
	if len(ls) != keepLaunches || ls[0].Version != "new" {
		t.Fatalf("kept %d, newest %q", len(ls), ls[0].Version)
	}
	for _, e := range launchFiles(dir) {
		if strings.HasPrefix(e, "20260101T000000.000000000Z") {
			t.Fatal("oldest record was kept")
		}
	}
}

// A record counts as running only while its pid belongs to the process that
// wrote it: a process that started after the record reused the pid.
func TestLaunchRunning(t *testing.T) {
	now := time.Now()
	if !LaunchRunning(Launch{PID: os.Getpid(), Started: now}) {
		t.Fatal("this process's record is not running")
	}
	if LaunchRunning(Launch{PID: os.Getpid(), Started: now, Ended: now}) {
		t.Fatal("an ended record counts as running")
	}
	if LaunchRunning(Launch{PID: os.Getpid(), Started: now.Add(-24 * time.Hour)}) {
		t.Fatal("a pid reused by a later process counts as running")
	}
}

// A process killed before it could note its end leaves a record with no end
// time, and that record stops counting once the process is gone.
func TestLaunchRunningSeesAGoneProcess(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if LaunchRunning(Launch{PID: cmd.Process.Pid, Started: time.Now()}) {
		t.Fatal("a process that is gone counts as running")
	}
}

// Pruning keeps the record of a server that is still running, however many
// launches came after it.
func TestPruneKeepsRunningServers(t *testing.T) {
	dir := t.TempDir()
	live := fmt.Sprintf(`{"version":"old","pid":%d,"started":%q}`, os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
	if err := os.WriteFile(dir+"/20250101T000000.000000000Z-1.json", []byte(live), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := range keepLaunches + 5 {
		name := fmt.Sprintf("20260101T000000.%09dZ-1.json", i)
		if err := os.WriteFile(dir+"/"+name, []byte(`{"version":"x"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	prune(dir)
	names := launchFiles(dir)
	if len(names) != keepLaunches || names[0] != "20250101T000000.000000000Z-1.json" {
		t.Fatalf("kept %d, oldest %q", len(names), names[0])
	}
}
