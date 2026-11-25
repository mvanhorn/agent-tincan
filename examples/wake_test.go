package examples

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Each wake's Source names the file its go:embed reads, which the docs link to.
func TestWakeScriptsMatchTheRepo(t *testing.T) {
	for _, name := range WakeNames() {
		t.Run(name, func(t *testing.T) {
			w, err := Wake(name)
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.FromSlash(w.Source))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(w.Script, want) {
				t.Fatalf("%s: embedded script differs from %s", name, w.Source)
			}
			if w.Notice == "" {
				t.Fatal("no install notice")
			}
		})
	}
}

// Scripts that source the shared library say so, and the others do not.
func TestWakeNeedsLib(t *testing.T) {
	for _, name := range WakeNames() {
		w, _ := Wake(name)
		sources := bytes.Contains(w.Script, []byte(WakeLibName))
		if sources && !w.NeedsLib {
			t.Errorf("%s sources %s but does not ask for it", name, WakeLibName)
		}
		if name == "codex" && w.NeedsLib {
			t.Errorf("codex-wake.sh stands on its own")
		}
	}
}

func TestWakeUnsupported(t *testing.T) {
	for _, name := range []string{"grok-cli", "nope", ""} {
		_, err := Wake(name)
		if err == nil {
			t.Fatalf("%q: want an error", name)
		}
		if !strings.Contains(err.Error(), "codex") {
			t.Errorf("%q: error should list the supported wakes: %v", name, err)
		}
	}
}
