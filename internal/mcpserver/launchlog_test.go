package mcpserver

import (
	"fmt"
	"os"
	"strings"
	"testing"
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
