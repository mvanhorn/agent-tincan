package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/notes"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// notesDoctorEnv is a notes agent's machine: its config joined to relayURL
// as agent, an executable helper, and an Application Support root.
type notesDoctorEnv struct {
	config, helper, appSupport string
}

func newNotesDoctorEnv(t *testing.T, relayURL, agent string) notesDoctorEnv {
	t.Helper()
	skipOnWindows(t, "the notes service is a macOS service")
	t.Setenv("TINCAN_RELAY", "")
	t.Setenv("TINCAN_PROXY", "")
	dir := t.TempDir()
	e := notesDoctorEnv{
		config:     filepath.Join(dir, "notes.json"),
		helper:     filepath.Join(dir, "agent-notes"),
		appSupport: filepath.Join(dir, "support"),
	}
	b, _ := json.Marshal(client.Config{Relay: relayURL, Agent: agent})
	if err := os.WriteFile(e.config, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.helper, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e notesDoctorEnv) writeHealth(t *testing.T, h notes.Health) {
	t.Helper()
	if err := os.MkdirAll(e.appSupport, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(h)
	if err := os.WriteFile(notes.HealthPathIn(e.appSupport), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (e notesDoctorEnv) run(t *testing.T) (string, error) {
	t.Helper()
	out, _, err := runSplit(t, notesCmd(), "doctor", "--config", e.config, "--helper", e.helper, "--app-support", e.appSupport)
	return out, err
}

// joinNotes joins a "notes" agent through a kindless invite, as
// tincan invite notes without --kind does.
func joinNotes(t *testing.T, m *testrelay.Mesh) {
	t.Helper()
	m.JoinOnMachineOf(t, "muse", "notes")
}

func setKind(t *testing.T, m *testrelay.Mesh, kind string) {
	t.Helper()
	if err := m.Client(t, "admin").SetKind(t.Context(), "notes", kind); err != nil {
		t.Fatal(err)
	}
}

func wantLine(t *testing.T, out, prefix string, parts ...string) {
	t.Helper()
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		// The check's line and its fix line.
		block := line
		if i+1 < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i+1]), "fix:") {
			block += "\n" + lines[i+1]
		}
		for _, p := range parts {
			if !strings.Contains(block, p) {
				t.Errorf("%q check missing %q:\n%s", prefix, p, out)
			}
		}
		return
	}
	t.Errorf("no line starting %q:\n%s", prefix, out)
}

func TestNotesDoctorPassesWithNotesKind(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	joinNotes(t, m)
	setKind(t, m, "notes")
	e := newNotesDoctorEnv(t, m.URL("notes"), "notes")
	e.writeHealth(t, notes.Health{UpdatedAt: time.Now().UTC(), Command: "search", OK: true, IdempotencyVerified: true})
	out, err := e.run(t)
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	wantLine(t, out, "[OK] relay kind", `"notes"`)
	wantLine(t, out, "[OK] helper:")
	wantLine(t, out, "[OK] spool")
	wantLine(t, out, "[OK] relay:")
	wantLine(t, out, "[OK] last helper result", "search")
}

func TestNotesDoctorFailsWithoutNotesKind(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	joinNotes(t, m) // kindless invite: no kind stored
	e := newNotesDoctorEnv(t, m.URL("notes"), "notes")
	out, err := e.run(t)
	if err == nil {
		t.Fatalf("doctor passed with no kind:\n%s", out)
	}
	wantLine(t, out, "[FAIL] relay kind", "no kind", "tincan kind notes notes", "24 hours")

	setKind(t, m, "codex")
	out, err = e.run(t)
	if err == nil {
		t.Fatalf("doctor passed with kind codex:\n%s", out)
	}
	wantLine(t, out, "[FAIL] relay kind", `"codex"`, "tincan kind notes notes")
}

// A re-join through a kindless invite after the agent was removed leaves
// no kind, and doctor catches it.
func TestNotesDoctorFailsAfterKindlessRejoin(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	joinNotes(t, m)
	setKind(t, m, "notes")
	e := newNotesDoctorEnv(t, m.URL("notes"), "notes")
	if out, err := e.run(t); err != nil {
		t.Fatalf("doctor failed before re-join: %v\n%s", err, out)
	}
	if err := m.Client(t, "admin").Remove(t.Context(), "notes"); err != nil {
		t.Fatal(err)
	}
	joinNotes(t, m)
	out, err := e.run(t)
	if err == nil {
		t.Fatalf("doctor passed after a kindless re-join:\n%s", out)
	}
	wantLine(t, out, "[FAIL] relay kind", "tincan kind notes notes")
}

func TestNotesDoctorMissingHelper(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	joinNotes(t, m)
	setKind(t, m, "notes")
	e := newNotesDoctorEnv(t, m.URL("notes"), "notes")
	e.helper = filepath.Join(t.TempDir(), "no-such-helper")
	out, err := e.run(t)
	if err == nil {
		t.Fatalf("doctor passed with no helper:\n%s", out)
	}
	wantLine(t, out, "[FAIL] helper:", e.helper, "Install Agent Notes", "--helper")

	// Present but not executable.
	if err := os.WriteFile(e.helper, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = e.run(t)
	if err == nil {
		t.Fatalf("doctor passed with a non-executable helper:\n%s", out)
	}
	wantLine(t, out, "[FAIL] helper:", "not executable")
}

func TestNotesDoctorReportsHelperError(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	joinNotes(t, m)
	setKind(t, m, "notes")
	cases := []struct {
		code string
		want []string
	}{
		{"missing_authorization", []string{"missing_authorization", "library access denied", "Files and Folders"}},
		{"operation_failed", []string{"operation_failed", "Files and Folders"}},
		{"helper_too_old", []string{"helper_too_old", "Update Agent Notes"}},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			e := newNotesDoctorEnv(t, m.URL("notes"), "notes")
			e.writeHealth(t, notes.Health{UpdatedAt: time.Now().UTC(), Command: "create", RequestID: "r1", Code: tc.code, Message: "library access denied"})
			out, err := e.run(t)
			if err == nil {
				t.Fatalf("doctor passed with a failed helper result:\n%s", out)
			}
			wantLine(t, out, "[FAIL] last helper result", append(tc.want, "create")...)
			if strings.Contains(out, "Full Disk Access") {
				t.Errorf("fix suggests Full Disk Access, which the codex extractor would inherit:\n%s", out)
			}
		})
	}
}

// helper_too_old is sticky: a later successful search does not make the
// doctor call the helper healthy while adds run without idempotency.
func TestNotesDoctorFailsWhileIdempotencyUnsupported(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	joinNotes(t, m)
	setKind(t, m, "notes")
	e := newNotesDoctorEnv(t, m.URL("notes"), "notes")
	e.writeHealth(t, notes.Health{UpdatedAt: time.Now().UTC(), Command: "search", OK: true, IdempotencyUnsupported: true})
	out, err := e.run(t)
	if err == nil {
		t.Fatalf("doctor passed with an idempotency-unsupported helper:\n%s", out)
	}
	wantLine(t, out, "[FAIL] last helper result", "helper_too_old", "Update Agent Notes")
}

// A successful search says nothing about duplicate adds: until a create
// has shown the helper honors idempotency keys, doctor warns.
func TestNotesDoctorWarnsWhileIdempotencyUnverified(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	joinNotes(t, m)
	setKind(t, m, "notes")
	e := newNotesDoctorEnv(t, m.URL("notes"), "notes")
	e.writeHealth(t, notes.Health{UpdatedAt: time.Now().UTC(), Command: "search", OK: true})
	out, err := e.run(t)
	if err != nil {
		t.Fatalf("doctor failed on an unverified helper: %v\n%s", err, out)
	}
	wantLine(t, out, "[WARN] last helper result", "duplicate protection not yet verified", "no add has run against this helper")
}

func TestNotesDoctorNoHealthYet(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	joinNotes(t, m)
	setKind(t, m, "notes")
	e := newNotesDoctorEnv(t, m.URL("notes"), "notes")
	out, err := e.run(t)
	if err != nil {
		t.Fatalf("doctor failed with no health file yet: %v\n%s", err, out)
	}
	wantLine(t, out, "[WARN] last helper result", "no helper result yet")
}

func TestNotesDoctorWrongAgent(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	e := newNotesDoctorEnv(t, m.URL("muse"), "muse")
	out, err := e.run(t)
	if err == nil {
		t.Fatalf("doctor passed for agent muse:\n%s", out)
	}
	wantLine(t, out, "[FAIL] relay:", `"muse"`, "notes.json")
}

func TestNotesDoctorUnreachableRelay(t *testing.T) {
	e := newNotesDoctorEnv(t, "http://127.0.0.1:1", "notes")
	out, err := e.run(t)
	if err == nil {
		t.Fatalf("doctor passed with no relay:\n%s", out)
	}
	wantLine(t, out, "[FAIL] relay:", "cannot reach")
}

func TestNotesInstallPrintsBootstrap(t *testing.T) {
	skipOnWindows(t, "the notes service is a macOS service")
	if os.Getenv("HOME") == "" {
		t.Skip("no HOME")
	}
	home := t.TempDir()
	setHome(t, home)
	lib := filepath.Join(home, "Notes Library")
	out, _, err := runSplit(t, notesCmd(), "install", "--library-root", lib, "--binary", "/usr/local/bin/tincan")
	if err != nil {
		if strings.Contains(err.Error(), "no notes service definition") {
			t.Skip(err)
		}
		t.Fatalf("install: %v\n%s", err, out)
	}
	for _, want := range []string{"notes service definition:", "(not started)", "notes.json", "join"} {
		if !strings.Contains(out, want) {
			t.Errorf("install output missing %q:\n%s", want, out)
		}
	}
}

func TestNotesInstallRequiresLibraryRoot(t *testing.T) {
	skipOnWindows(t, "the notes service is a macOS service")
	_, _, err := runSplit(t, notesCmd(), "install", "--binary", "/usr/local/bin/tincan")
	if err == nil || !strings.Contains(err.Error(), "library-root") {
		t.Fatalf("install without --library-root: %v", err)
	}
}
