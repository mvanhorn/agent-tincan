package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// mcpConfigEntry is one MCP server entry, in a config file an app reads,
// that runs or names tincan.
type mcpConfigEntry struct {
	File string `json:"file"`
	// Scope is where in the file the entry sits when the file keeps a
	// server map per project (Claude Code: "projects.<path>"); "" for the
	// map the app loads everywhere.
	Scope    string   `json:"scope,omitempty"`
	Name     string   `json:"name"`
	Command  string   `json:"command"`
	Args     []string `json:"args,omitempty"`
	Problems []string `json:"problems,omitempty"`
	broken   bool     // a problem that stops the app from running tincan
	unread   bool     // names tincan, but tincan could not read its command
}

// mcpConfigFiles are the MCP config files of the apps tincan agents run in.
// Missing files are skipped.
func mcpConfigFiles(extra []string) []string {
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	files := []string{
		filepath.Join(home, ".claude.json"),
		filepath.Join(home, ".claude", "settings.json"),
		filepath.Join(home, ".cursor", "mcp.json"),
		filepath.Join(home, ".codex", "config.toml"),
		filepath.Join(home, ".gemini", "settings.json"),
		filepath.Join(home, ".codeium", "windsurf", "mcp_config.json"),
		filepath.Join(home, ".hermes", "config.yaml"),
		filepath.Join(home, ".openclaw", "openclaw.json"),
		filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json"),
		filepath.Join(home, ".config", "Claude", "claude_desktop_config.json"),
		filepath.Join(cwd, ".mcp.json"),
		filepath.Join(cwd, ".cursor", "mcp.json"),
		"/workspace/.mcp.json",
		"/workspace/.cursor/mcp.json",
	}
	files = append(files, extra...)
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		if st, err := os.Stat(f); err == nil && !st.IsDir() {
			out = append(out, f)
		}
	}
	return out
}

func findMCPConfigs(extra []string) []mcpConfigEntry {
	var all []mcpConfigEntry
	for _, f := range mcpConfigFiles(extra) {
		raw, err := os.ReadFile(f)
		if err != nil || !strings.Contains(string(raw), "tincan") {
			continue
		}
		var es []mcpConfigEntry
		switch {
		case strings.HasSuffix(f, ".toml"):
			es = tomlServers(string(raw))
		case strings.HasSuffix(f, ".yaml") || strings.HasSuffix(f, ".yml"):
			es = yamlServers(string(raw))
		default:
			var v any
			if json.Unmarshal(raw, &v) != nil {
				all = append(all, mcpConfigEntry{File: f, Problems: []string{"mentions tincan but is not valid JSON, so the app may ignore the whole file"}, broken: true})
				continue
			}
			jsonServers(v, "", &es)
		}
		for i := range es {
			es[i].File = f
		}
		// Servers the app loads together compete: the map it loads
		// everywhere plus the one for the current project. Entries under
		// different projects never load together, so a server added in two
		// directories is not two servers.
		// A project server with the same name as a user-level one
		// overrides it (Claude Code: local and project scope win over
		// user), so only differently named servers count as extra.
		var global []string
		perScope, names := map[string]int{}, map[string]map[string]bool{}
		for _, e := range es {
			if e.Scope == "" {
				global = append(global, e.Name)
				continue
			}
			perScope[e.Scope]++
			if names[e.Scope] == nil {
				names[e.Scope] = map[string]bool{}
			}
			names[e.Scope][e.Name] = true
		}
		inScope := func(scope string) int {
			n := perScope[scope]
			for _, g := range global {
				if !names[scope][g] {
					n++
				}
			}
			return n
		}
		for i := range es {
			together := len(global)
			if es[i].Scope != "" {
				together = inScope(es[i].Scope)
			} else {
				for scope := range perScope {
					if !names[scope][es[i].Name] {
						together = max(together, inScope(scope))
					}
				}
			}
			if together > 1 {
				es[i].Problems = append(es[i].Problems, fmt.Sprintf("one of %d tincan servers this file loads together; keep one", together))
				es[i].broken = true
			}
		}
		all = append(all, es...)
	}
	return all
}

// jsonServers collects tincan entries from any "mcpServers" or "servers"
// map in v, at any depth (Claude Code keeps one per project). path names
// the map that holds v. Only a Claude Code project map ("projects.<path>")
// becomes the entry's Scope; any other map (the top of the file, OpenClaw's
// mcp.servers) is one the app loads everywhere, so its Scope is "".
func jsonServers(v any, path string, out *[]mcpConfigEntry) {
	switch t := v.(type) {
	case []any:
		for _, x := range t {
			jsonServers(x, path, out)
		}
	case map[string]any:
		for k, x := range t {
			if servers, ok := x.(map[string]any); ok && (k == "mcpServers" || k == "servers" || k == "mcp_servers") {
				for name, s := range servers {
					if e, ok := jsonEntry(name, s); ok {
						if strings.HasPrefix(path, "projects.") {
							e.Scope = path
						}
						*out = append(*out, e)
					}
				}
				continue
			}
			sub := k
			if path != "" {
				sub = path + "." + k
			}
			jsonServers(x, sub, out)
		}
	}
}

func jsonEntry(name string, v any) (mcpConfigEntry, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return mcpConfigEntry{}, false
	}
	e := mcpConfigEntry{Name: name}
	e.Command, _ = m["command"].(string)
	if args, ok := m["args"].([]any); ok {
		for _, a := range args {
			s, _ := a.(string)
			e.Args = append(e.Args, s)
		}
	}
	if !mentionsTincan(e) {
		return e, false
	}
	if d, _ := m["disabled"].(bool); d {
		e.Problems = append(e.Problems, "disabled")
		e.broken = true
	}
	if e.Command == "" {
		if u, _ := m["url"].(string); u != "" {
			e.Command = u // a remote (gateway) entry; nothing local to check
			return e, true
		}
	}
	return e, true
}

var (
	tomlSection = regexp.MustCompile(`^\[mcp_servers\.(?:"([^"]+)"|([^".\]]+))\]\s*$`)
	tomlString  = regexp.MustCompile(`^(\w+)\s*=\s*"([^"]*)"`)
	tomlArray   = regexp.MustCompile(`^args\s*=\s*\[(.*)\]`)
	tomlItem    = regexp.MustCompile(`"([^"]*)"`)
)

// tomlServers reads [mcp_servers.<name>] tables from a Codex config.toml.
// It understands only the flat command/args/enabled keys Codex uses.
func tomlServers(s string) []mcpConfigEntry {
	var out []mcpConfigEntry
	var cur *mcpConfigEntry
	flush := func() {
		if cur != nil && mentionsTincan(*cur) {
			out = append(out, *cur)
		}
		cur = nil
	}
	sub := false // inside a sub-table such as [mcp_servers.x.env]
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			if cur != nil && strings.HasPrefix(line, "[mcp_servers."+cur.Name+".") {
				sub = true
				continue
			}
			flush()
			sub = false
			if m := tomlSection.FindStringSubmatch(line); m != nil {
				cur = &mcpConfigEntry{Name: m[1] + m[2]}
			}
			continue
		}
		if cur == nil || sub {
			continue
		}
		if m := tomlArray.FindStringSubmatch(line); m != nil {
			for _, it := range tomlItem.FindAllStringSubmatch(m[1], -1) {
				cur.Args = append(cur.Args, it[1])
			}
		} else if m := tomlString.FindStringSubmatch(line); m != nil && m[1] == "command" {
			cur.Command = m[2]
		} else if strings.ReplaceAll(line, " ", "") == "enabled=false" {
			cur.Problems = append(cur.Problems, "disabled")
			cur.broken = true
		}
	}
	flush()
	return out
}

// yamlServers reads the mcp_servers block of a Hermes config.yaml. tincan
// carries no YAML parser, so it reads only the flat shape Hermes documents:
// a server name, then command, args (a flow list or a block list) and
// enabled, at deeper indents. Anything else in the file is skipped.
func yamlServers(s string) []mcpConfigEntry {
	var out []mcpConfigEntry
	var cur *mcpConfigEntry
	block, server := -1, -1 // indents of the mcp_servers key and of server names
	field := -1             // indent of the current server's own keys
	inArgs := false
	flush := func() {
		if cur != nil && mentionsTincan(*cur) {
			// A shape this reader does not follow (a flow mapping, an
			// anchor) leaves no command and no args: say so, do not fail.
			cur.unread = cur.Command == "" && len(cur.Args) == 0 && !cur.broken
			out = append(out, *cur)
		}
		cur = nil
	}
	for line := range strings.SplitSeq(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if block >= 0 && indent <= block {
			flush()
			block, server = -1, -1
		}
		if block < 0 {
			if yamlScalar(trimmed) == "mcp_servers:" {
				block = indent
			}
			continue
		}
		if server < 0 || indent <= server {
			// A server name is a key with nothing after the colon.
			if key, value, ok := strings.Cut(trimmed, ":"); ok && strings.TrimSpace(value) == "" && !strings.HasPrefix(trimmed, "- ") {
				flush()
				server = indent
				cur = &mcpConfigEntry{Name: yamlScalar(key)}
				field = -1
				inArgs = false
			}
			continue
		}
		if cur == nil {
			continue
		}
		if inArgs && strings.HasPrefix(trimmed, "- ") {
			cur.Args = append(cur.Args, yamlScalar(strings.TrimPrefix(trimmed, "- ")))
			continue
		}
		if field < 0 {
			field = indent
		}
		if indent > field {
			// A key in a sub-block such as env belongs to that block, not
			// to the server.
			continue
		}
		inArgs = false
		key, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		value = yamlScalar(value)
		switch strings.TrimSpace(key) {
		case "command":
			cur.Command = value
		case "args":
			if value == "" {
				inArgs = true
			} else if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
				for it := range strings.SplitSeq(strings.Trim(value, "[]"), ",") {
					if it = yamlScalar(it); it != "" {
						cur.Args = append(cur.Args, it)
					}
				}
			}
		case "enabled":
			if yamlFalse(value) {
				cur.Problems = append(cur.Problems, "disabled")
				cur.broken = true
			}
		case "disabled":
			if yamlTrue(value) {
				cur.Problems = append(cur.Problems, "disabled")
				cur.broken = true
			}
		}
	}
	flush()
	return out
}

// yamlFalse and yamlTrue read YAML 1.1 booleans the way PyYAML (Hermes)
// does: false/no/off and true/yes/on, in any case.
func yamlFalse(v string) bool {
	switch strings.ToLower(v) {
	case "false", "no", "off":
		return true
	}
	return false
}

func yamlTrue(v string) bool {
	switch strings.ToLower(v) {
	case "true", "yes", "on":
		return true
	}
	return false
}

// yamlScalar is a YAML scalar without its trailing comment or its quotes.
func yamlScalar(v string) string {
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	v = strings.TrimSpace(v)
	if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
		return v[1 : len(v)-1]
	}
	return v
}

func mentionsTincan(e mcpConfigEntry) bool {
	return strings.Contains(e.Name, "tincan") || strings.Contains(e.Command, "tincan") || strings.Contains(strings.Join(e.Args, " "), "tincan")
}

// configCheck judges the entries against this binary.
func configCheck(es []mcpConfigEntry, exe string) check {
	const name = "mcp config"
	if len(es) == 0 {
		return check{name, "warn", "no MCP config file tincan knows about mentions tincan. Hosted apps (Grok Bot, ChatGPT, Muse) keep their servers in their own settings, so this is expected there.", "Check the app's MCP settings for a tincan server, or pass --config <file>."}
	}
	self := ""
	if exe != "" {
		self, _ = filepath.EvalSymlinks(exe)
	}
	broken, differs, unread := 0, 0, 0
	for i := range es {
		e := &es[i]
		if e.unread {
			e.Problems = append(e.Problems, "could not read its command and args; check this entry by hand")
			unread++
			continue
		}
		if strings.Contains(e.Name, " ") {
			e.Problems = append(e.Problems, "the name has a space; name it tincan")
			e.broken = true
		}
		if strings.HasPrefix(e.Command, "http") {
			continue
		}
		if strings.Contains(strings.TrimSpace(e.Command), " ") {
			e.Problems = append(e.Problems, "command holds arguments; put the binary path in command and mcp in args")
			e.broken = true
		} else if e.Command != "" {
			path, err := exec.LookPath(e.Command)
			if err != nil {
				e.Problems = append(e.Problems, "command not found or not executable: "+e.Command)
				e.broken = true
			} else if real, _ := filepath.EvalSymlinks(path); self != "" && real != "" && real != self {
				e.Problems = append(e.Problems, "runs a different tincan than this one ("+real+")")
				differs++
			}
		}
		if len(e.Args) == 0 || e.Args[0] != "mcp" {
			e.Problems = append(e.Problems, `args must start with "mcp"`)
			e.broken = true
		}
		if e.broken {
			broken++
		}
	}
	switch {
	case broken > 0:
		return check{name, "fail", fmt.Sprintf("%d of %d tincan entries will not start tincan mcp (listed below)", broken, len(es)), "Fix or remove those entries; see \"To fix the app's tincan connection\" below."}
	case differs > 0:
		return check{name, "warn", fmt.Sprintf("%d of %d tincan entries run a different tincan binary than this one", differs, len(es)), "Point them at " + self + ", or upgrade that binary too."}
	case unread > 0:
		return check{name, "warn", fmt.Sprintf("could not read %d of %d tincan entries (listed below)", unread, len(es)), "Check that each runs this tincan with args [\"mcp\"]."}
	}
	return check{name, "ok", fmt.Sprintf("%d tincan entr%s, each runs this binary with mcp", len(es), map[bool]string{true: "y", false: "ies"}[len(es) == 1]), ""}
}
