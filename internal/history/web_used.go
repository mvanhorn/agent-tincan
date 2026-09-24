package history

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// maxWebUsed is how many conversations the used list keeps, newest first.
const maxWebUsed = 1000

// DefaultWebUsedPath is the list of site conversations a web agent has
// sent into. History leaves them out of the owner's own conversations, the
// way it leaves out codex exec and Claude Code SDK runs.
func DefaultWebUsedPath(site Source) string {
	return configPath("", "web-agent-"+string(site)+"-conversations.json")
}

// webUsed is the used list: conversation id -> when a web agent last sent
// into it. It holds ids and times only.
type webUsed struct {
	Conversations map[string]time.Time `json:"conversations"`
}

// loadWebUsed returns the ids in the used list at path. A missing or
// unreadable list is empty; malformed ids are dropped.
func loadWebUsed(path string) map[string]bool {
	out := map[string]bool{}
	if path == "" {
		return out
	}
	var u webUsed
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &u) != nil {
		return out
	}
	for id := range u.Conversations {
		if validNativeID(id) {
			out[id] = true
		}
	}
	return out
}

// recordWebUsed adds id to the used list at path, keeping the newest
// maxWebUsed entries.
func recordWebUsed(path, id string, now time.Time) error {
	if !validNativeID(id) {
		return fmt.Errorf("invalid conversation id %q", id)
	}
	u := webUsed{Conversations: map[string]time.Time{}}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &u)
	}
	if u.Conversations == nil {
		u.Conversations = map[string]time.Time{}
	}
	u.Conversations[id] = now.UTC()
	if len(u.Conversations) > maxWebUsed {
		ids := make([]string, 0, len(u.Conversations))
		for k := range u.Conversations {
			ids = append(ids, k)
		}
		sort.Slice(ids, func(i, j int) bool { return u.Conversations[ids[i]].After(u.Conversations[ids[j]]) })
		for _, k := range ids[maxWebUsed:] {
			delete(u.Conversations, k)
		}
	}
	b, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(path, append(b, '\n'), 0o600)
}
