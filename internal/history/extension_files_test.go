package history

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The Makefile zips EXTENSION_FILES and ops.js hashes its own
// EXTENSION_FILES for the hello; the two lists must name the same files,
// or a packaged extension would ship without a file the host compares (or
// the other way round).
func TestExtensionFilesMatchMakefile(t *testing.T) {
	root := filepath.Join("..", "..")
	mk, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	ops, err := os.ReadFile(filepath.Join(root, "extension", "ops.js"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^EXTENSION_FILES\s*:=\s*(.+)$`).FindSubmatch(mk)
	if m == nil {
		t.Fatal("no EXTENSION_FILES in the Makefile")
	}
	fromMake := strings.Fields(string(m[1]))
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
