package mcpserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProbeEnv marks a tincan mcp started by tincan doctor's own self-test, so
// the probe is not recorded as an app launching tincan.
const ProbeEnv = "TINCAN_DOCTOR_PROBE"

// keepLaunches is how many launch records a machine keeps.
const keepLaunches = 20

// Launch is what one tincan mcp process saw of the app that started it.
// tincan doctor reads these to tell "the app never starts tincan" apart
// from "the app starts it but never asks for the tools".
type Launch struct {
	Started     time.Time `json:"started"`
	PID         int       `json:"pid"`
	Parent      string    `json:"parent,omitempty"` // the process that started tincan mcp, best effort
	Cwd         string    `json:"cwd,omitempty"`
	Version     string    `json:"version"`
	Channel     bool      `json:"channel,omitempty"`
	Framing     string    `json:"framing,omitempty"` // newline or content-length, once the app has written
	Client      string    `json:"client,omitempty"`  // clientInfo from initialize
	Initialized time.Time `json:"initialized,omitzero"`
	ToolsListed time.Time `json:"tools_listed,omitzero"`
	ToolCalls   int       `json:"tool_calls,omitempty"`
	Ended       time.Time `json:"ended,omitzero"`
	Error       string    `json:"error,omitempty"`
}

// LaunchDir is where launch records live, next to the agent config.
func LaunchDir(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "mcp-launches")
}

// Recorder keeps this process's launch record up to date on disk. A nil
// Recorder records nothing.
type Recorder struct {
	mu   sync.Mutex
	path string
	l    Launch
	buf  []byte // partial line seen by Write
}

// StartRecorder writes a launch record for this process in dir and prunes
// old ones. It returns nil when recording is off (the doctor's probe) or the
// directory cannot be written; recording never stops the server.
func StartRecorder(dir, version string, channel bool) *Recorder {
	if os.Getenv(ProbeEnv) != "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil
	}
	now := time.Now().UTC()
	cwd, _ := os.Getwd()
	r := &Recorder{
		path: filepath.Join(dir, fmt.Sprintf("%s-%d.json", now.Format("20060102T150405.000000000Z"), os.Getpid())),
		l: Launch{
			Started: now,
			PID:     os.Getpid(),
			Parent:  parentName(os.Getppid()),
			Cwd:     cwd,
			Version: version,
			Channel: channel,
		},
	}
	r.save()
	prune(dir)
	return r
}

// Write takes the newline-delimited JSON the server reads, so the record
// notes initialize, tools/list and tool calls as they arrive.
func (r *Recorder) Write(p []byte) (int, error) {
	if r == nil {
		return len(p), nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	changed := false
	for {
		i := bytes.IndexByte(r.buf, '\n')
		if i < 0 {
			break
		}
		changed = r.saw(r.buf[:i]) || changed
		r.buf = r.buf[i+1:]
	}
	if changed {
		r.saveLocked()
	}
	return len(p), nil
}

func (r *Recorder) saw(line []byte) bool {
	var m struct {
		Method string `json:"method"`
		Params struct {
			ClientInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"clientInfo"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &m) != nil {
		return false
	}
	now := time.Now().UTC()
	switch m.Method {
	case "initialize":
		r.l.Initialized = now
		r.l.Client = strings.TrimSpace(m.Params.ClientInfo.Name + " " + m.Params.ClientInfo.Version)
	case "tools/list":
		r.l.ToolsListed = now
	case "tools/call":
		r.l.ToolCalls++
		// Only the first call is worth a disk write; later ones change
		// nothing the doctor reports.
		return r.l.ToolCalls == 1
	default:
		return false
	}
	return true
}

// SetFraming records which framing the app used.
func (r *Recorder) SetFraming(framing string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.l.Framing == framing {
		return
	}
	r.l.Framing = framing
	r.saveLocked()
}

// End records how the server stopped.
func (r *Recorder) End(err error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.l.Ended = time.Now().UTC()
	if err != nil {
		r.l.Error = err.Error()
	}
	r.saveLocked()
}

func (r *Recorder) save() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.saveLocked()
}

func (r *Recorder) saveLocked() {
	raw, err := json.MarshalIndent(r.l, "", "  ")
	if err != nil {
		return
	}
	tmp := r.path + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		_ = os.Rename(tmp, r.path)
	}
}

// ReadLaunches returns the launch records in dir, newest first.
func ReadLaunches(dir string) []Launch {
	names := launchFiles(dir)
	out := make([]Launch, 0, len(names))
	for i := len(names) - 1; i >= 0; i-- {
		raw, err := os.ReadFile(filepath.Join(dir, names[i]))
		if err != nil {
			continue
		}
		var l Launch
		if json.Unmarshal(raw, &l) == nil {
			out = append(out, l)
		}
	}
	return out
}

// launchFiles lists record files oldest first; names sort by start time.
func launchFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

func prune(dir string) {
	names := launchFiles(dir)
	for len(names) > keepLaunches {
		_ = os.Remove(filepath.Join(dir, names[0]))
		names = names[1:]
	}
}

// parentName names the process that started tincan mcp: /proc on Linux,
// ps elsewhere, empty when neither works. Only the program and its first
// argument are kept, since later arguments can carry secrets.
func parentName(ppid int) string {
	if ppid <= 1 {
		return ""
	}
	var cmd string
	if raw, err := os.ReadFile("/proc/" + strconv.Itoa(ppid) + "/cmdline"); err == nil && len(raw) > 0 {
		cmd = strings.ReplaceAll(string(raw), "\x00", " ")
	} else if out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(ppid)).Output(); err == nil {
		cmd = string(out)
	}
	f := strings.Fields(cmd)
	if len(f) > 2 {
		f = f[:2]
	}
	name := strings.Join(f, " ")
	if len(name) > 160 {
		name = name[:160]
	}
	return name
}
