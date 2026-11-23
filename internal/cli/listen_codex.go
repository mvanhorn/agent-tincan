package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

// The agent-tincan entry of Codex's config.toml is read and edited line by
// line, so the owner's comments and order stay; the repo has no TOML library.

var (
	tomlAnyTable = regexp.MustCompile(`^\s*\[`)
	// default_tools_approval_mode as a bare, basic-quoted or literal-quoted
	// key; m[1] is the value after the =.
	tomlApprovalKey = regexp.MustCompile(`^\s*(?:default_tools_approval_mode|"default_tools_approval_mode"|'default_tools_approval_mode')\s*=(.*)$`)
	// TINCAN_CONFIG as a bare or quoted key, with a basic or literal string.
	tomlEnvConfig = regexp.MustCompile(`(?:^|[\s{,])(?:TINCAN_CONFIG|"TINCAN_CONFIG"|'TINCAN_CONFIG')\s*=\s*(?:("(?:[^"\\]|\\.)*")|'([^']*)')`)
)

// codexEntry is the agent-tincan table of a Codex config.toml: its line
// range (header to before the next table that is not its own sub-table),
// whether its own table (not a sub-table) sets the approval mode to
// approve, and the TINCAN_CONFIG it pins ("" when none, meaning the
// default config). unclear is set when TINCAN_CONFIG appears in a form
// this reader cannot take, or more than once with different values.
type codexEntry struct {
	start, end      int
	mainEnd         int // where the entry's own table ends: its first sub-table, else end
	approve, hasKey bool
	config          string
	unclear         bool
}

// tomlStripComment drops a # comment that is not inside a string.
func tomlStripComment(line string) string {
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote == '"' && c == '\\':
			i++
		case quote != 0 && c == quote:
			quote = 0
		case quote == 0 && (c == '"' || c == '\''):
			quote = c
		case quote == 0 && c == '#':
			return line[:i]
		}
	}
	return line
}

func findCodexEntry(lines []string) (codexEntry, bool) {
	e := codexEntry{start: -1}
	seen := false
	for i, raw := range lines {
		line := tomlStripComment(raw)
		trimmed := strings.TrimSpace(line)
		if e.start < 0 {
			if m := tomlSection.FindStringSubmatch(trimmed); m != nil && m[1]+m[2] == "agent-tincan" {
				e.start, e.end, e.mainEnd = i, len(lines), len(lines)
			}
			continue
		}
		if tomlAnyTable.MatchString(line) {
			if !strings.HasPrefix(trimmed, "[mcp_servers.agent-tincan.") && !strings.HasPrefix(trimmed, `[mcp_servers."agent-tincan".`) {
				e.end = i
				if e.mainEnd > i {
					e.mainEnd = i
				}
				break
			}
			if e.mainEnd > i {
				e.mainEnd = i
			}
		}
		if m := tomlApprovalKey.FindStringSubmatch(line); m != nil && i < e.mainEnd {
			e.hasKey = true
			v := strings.TrimSpace(m[1])
			e.approve = v == `"approve"` || v == `'approve'`
		}
		if !strings.Contains(line, "TINCAN_CONFIG") {
			continue
		}
		m := tomlEnvConfig.FindStringSubmatch(line)
		if m == nil {
			e.unclear = true
			continue
		}
		v := m[2]
		if m[1] != "" {
			var err error
			if v, err = strconv.Unquote(m[1]); err != nil {
				e.unclear = true
				continue
			}
		}
		if seen && v != e.config {
			e.unclear = true
		}
		e.config, seen = v, true
	}
	return e, e.start >= 0
}

// sameConfig reports whether an MCP entry's TINCAN_CONFIG value names cfg.
// No value means the default config.
func sameConfig(pinned, cfg, def string) bool {
	if pinned == "" {
		pinned = def
	}
	return filepath.Clean(client.ExpandHome(pinned)) == filepath.Clean(cfg)
}

// codexEntryMismatch is why the agent-tincan entry would make a wake act
// as an agent other than the one whose config is cfg, or "".
func codexEntryMismatch(e codexEntry, path, cfg, def string) string {
	if e.unclear {
		return fmt.Sprintf("The agent-tincan entry in %s sets TINCAN_CONFIG in a way tincan cannot read for sure, so it cannot tell which agent a wake would act as. Set it once, as env = { TINCAN_CONFIG = %s }, and run this again.", path, tomlQuote(cfg))
	}
	if sameConfig(e.config, cfg, def) {
		return ""
	}
	pinned := e.config
	if pinned == "" {
		pinned = "the default config (" + def + ")"
	}
	return fmt.Sprintf("The agent-tincan entry in %s runs tincan as %s, not %s, so a wake would act as another agent. Point its TINCAN_CONFIG at %s and run this again.", path, pinned, cfg, cfg)
}

func tomlQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// readCodexConfig reads Codex's config.toml through any symlink, so a
// dotfile-managed file is edited where it lives. A missing file reads
// empty.
func readCodexConfig(path string) (real string, raw []byte, err error) {
	real = path
	if r, err := filepath.EvalSymlinks(path); err == nil {
		real = r
	}
	raw, err = os.ReadFile(real)
	if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	return real, raw, err
}

// checkCodexEntry refuses, before install writes anything, a Codex config
// whose agent-tincan entry would make a wake act as another agent.
func checkCodexEntry(path, cfg, def string) error {
	_, raw, err := readCodexConfig(path)
	if err != nil {
		return fmt.Errorf("read Codex's config: %w", err)
	}
	if e, ok := findCodexEntry(strings.Split(string(raw), "\n")); ok {
		if msg := codexEntryMismatch(e, path, cfg, def); msg != "" {
			return errors.New(msg + " Nothing was installed.")
		}
	}
	return nil
}

// setCodexApproval sets default_tools_approval_mode = "approve" on the
// agent-tincan entry of Codex's config.toml, or adds the entry when none
// names tincan. It changes nothing, and returns a notice saying what to do,
// when a tincan entry has another name or the entry would make a wake act
// as another agent.
func setCodexApproval(path, binary, cfg, def string) (changed bool, notice string, err error) {
	real, raw, err := readCodexConfig(path)
	if err != nil {
		return false, "", err
	}
	mode := os.FileMode(0o600)
	if fi, serr := os.Stat(real); serr == nil {
		mode = fi.Mode().Perm()
	}
	s := string(raw)
	lines := strings.Split(s, "\n")
	e, ok := findCodexEntry(lines)
	if !ok {
		for _, other := range tomlServers(s) {
			return false, fmt.Sprintf("%s has a tincan entry named %s, not agent-tincan, so it was left alone. Add default_tools_approval_mode = \"approve\" to that table, or rename it agent-tincan and run this again.", path, other.Name), nil
		}
		var b strings.Builder
		b.WriteString(s)
		if s != "" && !strings.HasSuffix(s, "\n") {
			b.WriteString("\n")
		}
		if s != "" {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "[mcp_servers.agent-tincan]\ncommand = %s\nargs = [\"mcp\"]\nenv = { TINCAN_CONFIG = %s }\ndefault_tools_approval_mode = \"approve\"\n", tomlQuote(binary), tomlQuote(cfg))
		if err := os.MkdirAll(filepath.Dir(real), 0o700); err != nil {
			return false, "", err
		}
		return true, "", writeFileReplacing(real, []byte(b.String()), mode)
	}
	if msg := codexEntryMismatch(e, path, cfg, def); msg != "" {
		return false, msg + " It was left alone.", nil
	}
	if e.approve {
		return false, "", nil
	}
	const keyLine = `default_tools_approval_mode = "approve"`
	if e.hasKey {
		for i := e.start + 1; i < e.mainEnd; i++ {
			if tomlApprovalKey.MatchString(tomlStripComment(lines[i])) {
				lines[i] = keyLine
			}
		}
	} else {
		lines = append(lines[:e.start+1], append([]string{keyLine}, lines[e.start+1:]...)...)
	}
	if n := codexApprovalKeys(lines); n != 1 {
		return false, "", fmt.Errorf("the edited agent-tincan entry would set default_tools_approval_mode %d times, so %s was left alone", n, path)
	}
	return true, "", writeFileReplacing(real, []byte(strings.Join(lines, "\n")), mode)
}

// codexApprovalKeys counts the default_tools_approval_mode keys in the
// agent-tincan entry's own table, 0 when there is no entry.
func codexApprovalKeys(lines []string) int {
	e, ok := findCodexEntry(lines)
	if !ok {
		return 0
	}
	n := 0
	for i := e.start + 1; i < e.mainEnd; i++ {
		if tomlApprovalKey.MatchString(tomlStripComment(lines[i])) {
			n++
		}
	}
	return n
}

// codexConfigPath is Codex's config file under codexHome, else under
// ~/.codex.
func codexConfigPath(home, codexHome string) string {
	if codexHome != "" {
		return filepath.Join(codexHome, "config.toml")
	}
	return filepath.Join(home, ".codex", "config.toml")
}
