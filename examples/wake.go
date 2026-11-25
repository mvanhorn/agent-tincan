// Package examples carries the wake scripts that tincan listen install
// writes, embedded from the files the docs link to, so a release can install
// them without a repo checkout. go:embed cannot reach parent folders, which
// is why this package sits beside the scripts.
package examples

import (
	_ "embed"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// WakeLibName is the shared library some wake scripts source from the
// folder they run in.
const WakeLibName = "tincan-wake-lib.sh"

var (
	//go:embed codex/codex-wake.sh
	codexWake []byte
	//go:embed copilot-cli/copilot-wake.sh
	copilotWake []byte
	//go:embed cursor-cli/cursor-wake.sh
	cursorWake []byte
	//go:embed gemini-cli/gemini-wake.sh
	geminiWake []byte
	//go:embed claude-code/cmux-wake.sh
	cmuxWake []byte
	//go:embed lib/tincan-wake-lib.sh
	wakeLib []byte
)

// WakeScript is one installable wake: its script, the repo file it comes
// from, and what the owner is told before it is installed.
type WakeScript struct {
	Name     string // the --wake value, matching the agent kind
	File     string // file name it is written under
	Source   string // repo path, slash-separated
	Script   []byte
	NeedsLib bool   // sources WakeLibName from its own folder
	Tool     string // the program the wake starts, looked up on the service's PATH
	MacOS    bool   // drives a macOS-only app (cmux), so it is refused elsewhere
	Notice   string // what a woken agent may do with nobody watching
}

var wakes = map[string]WakeScript{
	"codex": {
		Name: "codex", File: "codex-wake.sh", Source: "codex/codex-wake.sh", Script: codexWake, Tool: "codex",
		Notice: "Each wake runs codex exec with approvals off (approval_policy=never) and the network on. " +
			"A teammate's request can run commands with no one confirming them, read any file this user can read, and send data off this machine. " +
			"Writes stay in TINCAN_CODEX_WORKDIR (default ~/tincan-codex) and the write roots you list in TINCAN_CODEX_WRITE_ROOTS. " +
			"The approval mode set on the agent-tincan entry in Codex's config also pre-approves the tincan tools in your own interactive Codex sessions.",
	},
	"copilot-cli": {
		Name: "copilot-cli", File: "copilot-wake.sh", Source: "copilot-cli/copilot-wake.sh", Script: copilotWake, NeedsLib: true, Tool: "copilot",
		Notice: "Each wake runs GitHub Copilot CLI headless with tool approval on and only the Agent Tincan tools allowed, so any other tool call is denied. " +
			"It runs in a COPILOT_HOME of its own, with the built-in GitHub MCP servers off.",
	},
	"cursor-cli": {
		Name: "cursor-cli", File: "cursor-wake.sh", Source: "cursor-cli/cursor-wake.sh", Script: cursorWake, NeedsLib: true, Tool: "cursor-agent",
		Notice: "Each wake runs Cursor's CLI agent headless with tool approval on and only the Agent Tincan tools allowed, so any other tool call is denied. " +
			"It uses a private Cursor config folder, so your own ~/.cursor settings, servers and hooks do not apply.",
	},
	"gemini-cli": {
		Name: "gemini-cli", File: "gemini-wake.sh", Source: "gemini-cli/gemini-wake.sh", Script: geminiWake, NeedsLib: true, Tool: "agy",
		Notice: "Each wake runs Antigravity CLI (agy) headless in its terminal sandbox, with per-run settings in strict permission mode: it may call MCP tools and read files, except the tincan config folder (apart from its own attachments) and ~/.ssh, and it cannot write files or run commands. " +
			"A teammate's request can still make it read any other file this user can read and pass what it reads on in a reply, and on a case-insensitive volume the read denies are best effort. " +
			"It refuses to run while agy has plugins installed. " +
			"Adding TINCAN_GEMINI_ALLOW_UNCONFINED=1 to the service turns tool approval off with nothing limiting what a run does.",
	},
	"claude-code": {
		Name: "claude-code", File: "cmux-wake.sh", Source: "claude-code/cmux-wake.sh", Script: cmuxWake, Tool: "claude", MacOS: true,
		Notice: "Each wake opens a Claude Code session in cmux with your own settings and permission mode. " +
			"If your Claude launcher skips permissions, a teammate's request can run anything on this machine with no one confirming it.",
	},
}

// WakeNames lists the wakes tincan listen install supports, sorted.
func WakeNames() []string {
	return slices.Sorted(maps.Keys(wakes))
}

// Wake returns the named wake. The Grok wake is not offered: it needs an API
// key in the listener's environment, and an installed service must not
// store one.
func Wake(name string) (WakeScript, error) {
	w, ok := wakes[name]
	if !ok {
		return WakeScript{}, fmt.Errorf("no installable wake %q; choose one of %s (grok-cli is set up by hand, see its adapter doc)", name, strings.Join(WakeNames(), ", "))
	}
	return w, nil
}

// WakeLib returns the shared wake library.
func WakeLib() []byte { return wakeLib }
