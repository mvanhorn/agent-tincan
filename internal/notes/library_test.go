package notes

import (
	"os"
	"path/filepath"
	"testing"
)

func makeLibrary(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".agentnotes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".agentnotes", "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverLibraryRootPrefersTheAppsActiveLibrary(t *testing.T) {
	home := t.TempDir()
	active := filepath.Join(home, "Library", "Application Support", "AgentNotes", "Libraries", "d6541b2b-e625-48a7-a87c-8bf22998bfb6")
	documents := filepath.Join(home, "Documents", "Agent Notes")
	makeLibrary(t, active)
	makeLibrary(t, documents)
	read := func(domain, key string) string {
		if domain != appDefaultsDomain || key != activeLibraryIDKey {
			t.Fatalf("read %s %s", domain, key)
		}
		return "D6541B2B-E625-48A7-A87C-8BF22998BFB6\n"
	}
	if got := discoverLibraryRoot(home, read); got != active {
		t.Fatalf("got %q, want %q", got, active)
	}
}

func TestDiscoverLibraryRootFallsBackToDocuments(t *testing.T) {
	home := t.TempDir()
	documents := filepath.Join(home, "Documents", "Agent Notes")
	makeLibrary(t, documents)
	none := func(string, string) string { return "" }
	if got := discoverLibraryRoot(home, none); got != documents {
		t.Fatalf("got %q, want %q", got, documents)
	}
	stale := func(string, string) string { return "0000" }
	if got := discoverLibraryRoot(home, stale); got != documents {
		t.Fatalf("a missing active library must fall back: got %q", got)
	}
}

func TestDiscoverLibraryRootIgnoresAPathInThePreference(t *testing.T) {
	home := t.TempDir()
	makeLibrary(t, filepath.Join(home, "elsewhere"))
	escape := func(string, string) string { return "../../../elsewhere" }
	if got := discoverLibraryRoot(home, escape); got != "" {
		t.Fatalf("got %q, want no library", got)
	}
}
