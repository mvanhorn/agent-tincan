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
	// agent-tincan as a key under [mcp_servers], and mcp_servers.agent-tincan
	// as a top-level dotted key.
	tomlEntryKey    = regexp.MustCompile(`^(?:agent-tincan|"agent-tincan"|'agent-tincan')\s*[.=]`)
	tomlTopEntryKey = regexp.MustCompile(`^(?:mcp_servers|"mcp_servers"|'mcp_servers')\s*\.\s*(?:agent-tincan|"agent-tincan"|'agent-tincan')\s*[.=]`)
	tomlEnvConfig   = regexp.MustCompile(`(?:^|[\s{,])(?:TINCAN_CONFIG|"TINCAN_CONFIG"|'TINCAN_CONFIG')\s*=\s*(?:("(?:[^"\\]|\\.)*")|'([^']*)')`)
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

// tomlTableHeader reads a [table] header line into its keys: whitespace
// inside the brackets and around each dot is dropped and a quoted key is
// unquoted, so [ mcp_servers . "agent-tincan" ] reads as mcp_servers,
// agent-tincan. A trailing comment is allowed. ok is false for anything
// else, an [[array]] header included.
func tomlTableHeader(line string) (keys []string, ok bool) {
	line = strings.TrimSpace(tomlStripComment(line))
	if !strings.HasPrefix(line, "[") || strings.HasPrefix(line, "[[") || !strings.HasSuffix(line, "]") {
		return nil, false
	}
	in := line[1 : len(line)-1]
	i := 0
	skip := func() {
		for i < len(in) && (in[i] == ' ' || in[i] == '\t') {
			i++
		}
	}
	for {
		skip()
		if i >= len(in) {
			return nil, false
		}
		switch in[i] {
		case '"':
			j := i + 1
			for j < len(in) && in[j] != '"' {
				if in[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(in) {
				return nil, false
			}
			k, err := strconv.Unquote(in[i : j+1])
			if err != nil {
				return nil, false
			}
			keys, i = append(keys, k), j+1
		case '\'':
			j := strings.IndexByte(in[i+1:], '\'')
			if j < 0 {
				return nil, false
			}
			keys, i = append(keys, in[i+1:i+1+j]), i+j+2
		default:
			j := i
			for j < len(in) && isTomlBareKeyByte(in[j]) {
				j++
			}
			if j == i {
				return nil, false
			}
			keys, i = append(keys, in[i:j]), j
		}
		skip()
		if i == len(in) {
			return keys, true
		}
		if in[i] != '.' {
			return nil, false
		}
		i++
	}
}

func isTomlBareKeyByte(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// isCodexEntryKey reports whether table keys start with
// mcp_servers.agent-tincan.
func isCodexEntryKey(keys []string) bool {
	return len(keys) >= 2 && keys[0] == "mcp_servers" && keys[1] == "agent-tincan"
}

// tomlCodexEntryUnread is a line of s that may define the agent-tincan
// entry in a form tomlTableHeader cannot read, or "" when there is none:
// a header naming agent-tincan that does not parse, or the entry set with
// keys under [mcp_servers] or at the top level. Adding a new
// [mcp_servers.agent-tincan] table next to it could define it twice.
func tomlCodexEntryUnread(s string) string {
	var table []string
	for raw := range strings.SplitSeq(s, "\n") {
		line := strings.TrimSpace(tomlStripComment(raw))
		if strings.HasPrefix(line, "[") {
			keys, ok := tomlTableHeader(line)
			if !ok {
				table = nil
				if strings.Contains(line, "agent-tincan") {
					return strings.TrimSpace(raw)
				}
				continue
			}
			table = keys
			continue
		}
		switch {
		case len(table) == 1 && table[0] == "mcp_servers" && tomlEntryKey.MatchString(line),
			table == nil && tomlTopEntryKey.MatchString(line):
			return strings.TrimSpace(raw)
		}
	}
	return ""
}

func findCodexEntry(lines []string) (codexEntry, bool) {
	e := codexEntry{start: -1}
	seen := false
	for i, raw := range lines {
		line := tomlStripComment(raw)
		trimmed := strings.TrimSpace(line)
		if e.start < 0 {
			if keys, ok := tomlTableHeader(trimmed); ok && len(keys) == 2 && isCodexEntryKey(keys) {
				e.start, e.end, e.mainEnd = i, len(lines), len(lines)
			}
			continue
		}
		if tomlAnyTable.MatchString(line) {
			if keys, ok := tomlTableHeader(trimmed); !ok || len(keys) < 3 || !isCodexEntryKey(keys) {
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
		if line := tomlCodexEntryUnread(s); line != "" {
			return false, "", fmt.Errorf("%s may already define the agent-tincan entry at %q, in a form tincan cannot edit, so it was left alone. Write that table as [mcp_servers.agent-tincan] and run this again", path, line)
		}
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
