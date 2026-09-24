package history

import (
	"archive/zip"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// makefileExtensionFiles returns the Makefile's EXTENSION_FILES.
func makefileExtensionFiles(t *testing.T, root string) []string {
	t.Helper()
	mk, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^EXTENSION_FILES\s*:=\s*(.+)$`).FindSubmatch(mk)
	if m == nil {
		t.Fatal("no EXTENSION_FILES in the Makefile")
	}
	return strings.Fields(string(m[1]))
}

// make store builds the Web Store zip: exactly EXTENSION_FILES, with a
// manifest that is the source manifest minus "key" (the store rejects a
// key), and extension/manifest.json itself left with its key.
func TestStoreZip(t *testing.T) {
	for _, tool := range []string{"make", "zip", "node", "shasum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "store.zip")
	cmd := exec.Command("make", "-s", "-C", root, "store", "STORE_ZIP="+out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make store: %v\n%s", err, b)
	}
	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var names []string
	var manifest []byte
	for _, f := range zr.File {
		names = append(names, f.Name)
		if f.Name == "manifest.json" {
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			manifest, err = io.ReadAll(rc)
			rc.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	want := makefileExtensionFiles(t, root)
	slices.Sort(names)
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("store zip holds %v, want %v", names, want)
	}
	var got map[string]any
	if err := json.Unmarshal(manifest, &got); err != nil {
		t.Fatalf("store manifest: %v", err)
	}
	if _, ok := got["key"]; ok {
		t.Fatal("store manifest still has a key")
	}
	srcBytes, err := os.ReadFile(filepath.Join(root, "extension", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var src map[string]any
	if err := json.Unmarshal(srcBytes, &src); err != nil {
		t.Fatal(err)
	}
	if _, ok := src["key"]; !ok {
		t.Fatal("extension/manifest.json lost its key")
	}
	delete(src, "key")
	a, _ := json.Marshal(src)
	b, _ := json.Marshal(got)
	if string(a) != string(b) {
		t.Fatalf("store manifest differs from the source beyond the key:\n%s\n%s", b, a)
	}
}

// The Makefile zips EXTENSION_FILES and ops.js hashes its own
// EXTENSION_FILES for the hello; the two lists must name the same files,
// or a packaged extension would ship without a file the host compares (or
// the other way round).
func TestExtensionFilesMatchMakefile(t *testing.T) {
	root := filepath.Join("..", "..")
	ops, err := os.ReadFile(filepath.Join(root, "extension", "ops.js"))
	if err != nil {
		t.Fatal(err)
	}
	fromMake := makefileExtensionFiles(t, root)
	j := regexp.MustCompile(`export const EXTENSION_FILES = Object\.freeze\(\[([^\]]*)\]\)`).FindSubmatch(ops)
	if j == nil {
		t.Fatal("no EXTENSION_FILES in extension/ops.js")
	}
	var fromOps []string
	for _, q := range regexp.MustCompile(`'([^']+)'`).FindAllSubmatch(j[1], -1) {
		fromOps = append(fromOps, string(q[1]))
	}
	slices.Sort(fromMake)
	slices.Sort(fromOps)
	if len(fromMake) == 0 || !slices.Equal(fromMake, fromOps) {
		t.Fatalf("Makefile EXTENSION_FILES %v != extension/ops.js EXTENSION_FILES %v", fromMake, fromOps)
	}
	for _, f := range fromOps {
		if _, err := os.Stat(filepath.Join(root, "extension", f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}
