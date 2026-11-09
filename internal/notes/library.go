package notes

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Agent Notes remembers the library it has open in its preferences, under this
// domain and key, and keeps that library under Application Support.
const (
	appDefaultsDomain  = "com.agentnotes.app"
	activeLibraryIDKey = "activeAppOwnedLibraryID"
)

// DiscoverLibraryRoot returns the library Agent Notes itself has open, so the
// notes agent reads and saves the same notes the owner sees. It falls back to
// the first-launch default, ~/Documents/Agent Notes, and returns "" when
// neither holds an Agent Notes library.
func DiscoverLibraryRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return discoverLibraryRoot(home, readAppDefault)
}

func discoverLibraryRoot(home string, readDefault func(domain, key string) string) string {
	var candidates []string
	if id := strings.ToLower(strings.TrimSpace(readDefault(appDefaultsDomain, activeLibraryIDKey))); id != "" && !strings.ContainsAny(id, "/.") {
		candidates = append(candidates, filepath.Join(home, "Library", "Application Support", "AgentNotes", "Libraries", id))
	}
	candidates = append(candidates, filepath.Join(home, "Documents", "Agent Notes"))
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(c, ".agentnotes", "config.json")); err == nil {
			return c
		}
	}
	return ""
}

func readAppDefault(domain, key string) string {
	out, err := exec.Command("/usr/bin/defaults", "read", domain, key).Output()
	if err != nil {
		return ""
	}
	return string(out)
}
