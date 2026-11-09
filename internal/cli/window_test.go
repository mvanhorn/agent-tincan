//go:build unix

package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
)

// fakeTermWindow is a terminal with one window whose screen walks through
// screens (the last one stays) and that can be closed.
type fakeTermWindow struct {
	mu      sync.Mutex
	screens []string
	reads   int
	gone    bool
	opened  []string // dirs
	typed   []string
	openErr error
}

func (f *fakeTermWindow) open(_ context.Context, name, dir string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.openErr != nil {
		return "", f.openErr
	}
	f.opened = append(f.opened, dir)
	return "workspace:7", nil
}

func (f *fakeTermWindow) screen(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gone {
		return "", errWindowGone
	}
	if len(f.screens) == 0 {
		return "", nil
	}
	s := f.screens[min(f.reads, len(f.screens)-1)]
	f.reads++
	return s, nil
}

func (f *fakeTermWindow) typeLine(_ context.Context, _, line string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.typed = append(f.typed, line)
	return nil
}

func (f *fakeTermWindow) close() { f.mu.Lock(); f.gone = true; f.mu.Unlock() }

func (f *fakeTermWindow) typedLines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.typed...)
}

func fastWindow(t *testing.T) {
	t.Helper()
	old := []time.Duration{windowStartTimeout, windowShellSettle, windowPoll, windowWatchEvery}
	windowStartTimeout, windowShellSettle, windowPoll, windowWatchEvery = time.Second, 50*time.Millisecond, 5*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() {
		windowStartTimeout, windowShellSettle, windowPoll, windowWatchEvery = old[0], old[1], old[2], old[3]
	})
}

const claudeScreen = " ▐▛███▜▌   Claude Code v2.1.319\n────\n❯ \n────\n  ? for shortcuts"

func TestWindowStartSeesClaudeInAnyPermissionMode(t *testing.T) {
	fastWindow(t)
	for _, mode := range []string{"? for shortcuts", "⏵⏵ bypass permissions on", "⏸ plan mode on"} {
		f := &fakeTermWindow{screens: []string{"Last login: today", " Claude Code v2\n❯ \n  " + mode}}
		got, err := waitForWindowStart(t.Context(), f, "w", "auto")
		if err != nil || got != "claude" {
			t.Errorf("%s: got %q, %v; want claude", mode, got, err)
		}
	}
}

func TestWindowStartSeesASettledShell(t *testing.T) {
	fastWindow(t)
	f := &fakeTermWindow{screens: []string{"Last login: today\nmatt@mac ~ % "}}
	got, err := waitForWindowStart(t.Context(), f, "w", "auto")
	if err != nil || got != "shell" {
		t.Fatalf("got %q, %v; want shell", got, err)
	}
}

func TestWindowStartWaitsForClaudeWhenToldItStartsClaude(t *testing.T) {
	fastWindow(t)
	// A long quiet login line would look like a shell under auto.
	screens := []string{"Last login: today"}
	for range 40 {
		screens = append(screens, "Last login: today")
	}
	f := &fakeTermWindow{screens: append(screens, claudeScreen)}
	got, err := waitForWindowStart(t.Context(), f, "w", "claude")
	if err != nil || got != "claude" {
		t.Fatalf("got %q, %v; want claude", got, err)
	}
}

func TestWindowStartFailsOnAnEmptyOrClosedWindow(t *testing.T) {
	fastWindow(t)
	if _, err := waitForWindowStart(t.Context(), &fakeTermWindow{}, "w", "auto"); err == nil {
		t.Error("an empty window: want an error")
	}
	f := &fakeTermWindow{}
	f.close()
	if _, err := waitForWindowStart(t.Context(), f, "w", "auto"); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("a closed window: %v, want closed", err)
	}
}

func TestOpenWindowTypesThePromptIntoClaudeOrTheClaudeLineIntoAShell(t *testing.T) {
	fastWindow(t)
	w := windowLaunch{claude: "/opt/my tools/claude", dir: "/work", starts: "auto"}
	f := &fakeTermWindow{screens: []string{claudeScreen}}
	if _, err := openWindow(t.Context(), f, w, "n", "fix it's test"); err != nil {
		t.Fatal(err)
	}
	if got := f.typedLines(); len(got) != 1 || got[0] != "fix it's test" {
		t.Errorf("into Claude typed %q", got)
	}
	f = &fakeTermWindow{screens: []string{"% "}}
	if _, err := openWindow(t.Context(), f, w, "n", "fix it's test"); err != nil {
		t.Fatal(err)
	}
	got := f.typedLines()
	if len(got) != 1 {
		t.Fatalf("into a shell typed %q", got)
	}
	out, err := exec.Command("sh", "-c", "printf '%s\\n' "+strings.TrimPrefix(got[0], shellQuote(w.claude)+" ")).Output()
	if err != nil || string(out) != "fix it's test\n" || !strings.HasPrefix(got[0], `'/opt/my tools/claude' `) {
		t.Errorf("shell line %q does not run claude with the prompt (%q, %v)", got[0], out, err)
	}
	if strings.Contains(got[0], "dangerously") {
		t.Errorf("shell line %q skips permissions", got[0])
	}
	if f.opened[0] != "/work" {
		t.Errorf("opened in %q, want /work", f.opened[0])
	}
}

func TestOpenLocalWindowPutsALongOrMultiLineTaskInAFile(t *testing.T) {
	fastWindow(t)
	t.Setenv("TINCAN_CONFIG", filepath.Join(t.TempDir(), "client.json"))
	f := &fakeTermWindow{screens: []string{claudeScreen}}
	if _, err := openLocalWindow(t.Context(), f, windowLaunch{dir: "/w", starts: "auto"}, "line one\nline two"); err != nil {
		t.Fatal(err)
	}
	typed := f.typedLines()[0]
	file := strings.TrimSuffix(strings.TrimPrefix(typed, "Read the task in "), " and do it.")
	got, err := os.ReadFile(file)
	if err != nil || string(got) != "line one\nline two\n" {
		t.Fatalf("typed %q; task file %q, %v", typed, got, err)
	}
	if fi, _ := os.Stat(filepath.Dir(file)); fi.Mode().Perm() != 0o700 {
		t.Errorf("run dir mode %v, want 0700", fi.Mode().Perm())
	}
}

// windowRig claims an ask to muse under a worker session, as serve's
// listener does before it starts window open --request.
func windowRig(t *testing.T, guest bool) (*workerRig, windowRequest) {
	t.Helper()
	fastWindow(t)
	w := newWorkerRig(t, relay.Config{})
	id := w.ask("summarize the README")
	sess := "worker-test"
	wr, err := w.client(sess)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wr.Claim(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	return w, windowRequest{id: id, from: "grokbot", session: sess, config: filepath.Join(t.TempDir(), "window.json"), tincan: "/usr/local/bin/tincan", guest: guest, body: "summarize the README"}
}

func runRequestAsync(t *testing.T, w *workerRig, f *fakeTermWindow, req windowRequest) chan error {
	t.Helper()
	wr, err := w.client(req.session)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- runWindowRequest(t.Context(), wr, f, windowLaunch{dir: "/w", claude: "/bin/claude", starts: "auto"}, req)
	}()
	return done
}

func waitDone(t *testing.T, done chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("runWindowRequest did not return")
		return nil
	}
}

func TestWindowRequestReturnsOnceTheWindowReplies(t *testing.T) {
	w, req := windowRig(t, false)
	f := &fakeTermWindow{screens: []string{claudeScreen}}
	done := runRequestAsync(t, w, f, req)
	eventually(t, "the prompt is typed", func() bool { return len(f.typedLines()) == 1 })
	typed := f.typedLines()[0]
	if !strings.Contains(typed, req.id) || !strings.Contains(typed, "task.md") || strings.Contains(typed, "\n") {
		t.Fatalf("typed %q", typed)
	}
	task, err := os.ReadFile(filepath.Join(filepath.Dir(req.config), "window-runs", req.id, "task.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"summarize the README", "TINCAN_SESSION='worker-test'", "reply " + req.id, "TINCAN_ANSWER"} {
		if !strings.Contains(string(task), want) {
			t.Errorf("task file lacks %q:\n%s", want, task)
		}
	}
	// The window replies under the worker's session, as the task file says.
	wr, _ := w.client(req.session)
	if _, err := wr.Reply(t.Context(), req.id, "It is a relay for agents.", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	if err := waitDone(t, done); err != nil {
		t.Fatalf("runWindowRequest = %v", err)
	}
	if got := w.result(req.id, envelope.StatusAnswered); got.Reply == nil || got.Reply.Body != "It is a relay for agents." {
		t.Errorf("answer %+v", got.Reply)
	}
}

func TestWindowRequestFailsWhenTheWindowIsClosedFirst(t *testing.T) {
	w, req := windowRig(t, false)
	f := &fakeTermWindow{screens: []string{claudeScreen}}
	done := runRequestAsync(t, w, f, req)
	eventually(t, "the prompt is typed", func() bool { return len(f.typedLines()) == 1 })
	f.close()
	if err := waitDone(t, done); err == nil {
		t.Fatal("want an error once the window closed")
	}
	if got := w.result(req.id, envelope.StatusFailed); !strings.Contains(replyBody(got), "closed before it replied") {
		t.Errorf("failed reply %q", replyBody(got))
	}
}

func TestWindowRequestReturnsWhenTheWindowAsksAQuestion(t *testing.T) {
	w, req := windowRig(t, false)
	f := &fakeTermWindow{screens: []string{claudeScreen}}
	done := runRequestAsync(t, w, f, req)
	eventually(t, "the prompt is typed", func() bool { return len(f.typedLines()) == 1 })
	wr, _ := w.client(req.session)
	if _, err := wr.Reply(t.Context(), req.id, "Which README?", envelope.StatusNeedsInput); err != nil {
		t.Fatal(err)
	}
	if err := waitDone(t, done); err != nil {
		t.Fatalf("runWindowRequest = %v", err)
	}
}

func TestWindowRequestFromAGuestOpensNoWindow(t *testing.T) {
	w, req := windowRig(t, true)
	f := &fakeTermWindow{screens: []string{claudeScreen}}
	if err := waitDone(t, runRequestAsync(t, w, f, req)); err != nil {
		t.Fatal(err)
	}
	if len(f.opened) != 0 {
		t.Errorf("a guest's request opened %d windows", len(f.opened))
	}
	if got := w.result(req.id, envelope.StatusFailed); replyBody(got) != windowGuestRefusal {
		t.Errorf("failed reply %q", replyBody(got))
	}
}

func TestWindowRequestFailsWhenNoWindowOpens(t *testing.T) {
	w, req := windowRig(t, false)
	f := &fakeTermWindow{openErr: errors.New("cmux refused the connection")}
	if err := waitDone(t, runRequestAsync(t, w, f, req)); err == nil {
		t.Fatal("want an error")
	}
	if got := w.result(req.id, envelope.StatusFailed); !strings.Contains(replyBody(got), "cmux refused") {
		t.Errorf("failed reply %q", replyBody(got))
	}
}

func TestWindowServeExecRunsThroughSh(t *testing.T) {
	dir := t.TempDir()
	argv := filepath.Join(dir, "argv")
	fake := filepath.Join(dir, "my tincan")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nfor a; do printf '%s\\n' \"$a\"; done > '"+argv+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	w := windowLaunch{dir: "/Users/me/my code", terminal: "cmux", cmux: "/A/cmux", claude: "/b/claude", starts: "auto"}
	if err := exec.Command("sh", "-c", windowServeExec(fake, w)).Run(); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(argv)
	want := "window\nopen\n--request\n--dir\n/Users/me/my code\n--terminal\ncmux\n--cmux\n/A/cmux\n--claude\n/b/claude\n--starts\nauto\n"
	if string(got) != want {
		t.Errorf("argv:\n%s\nwant:\n%s", got, want)
	}
}

// fakeCmux is a cmux CLI that logs its arguments and answers like cmux.
func fakeCmux(t *testing.T, readScreen string) (cmuxTerminal, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	script := `#!/bin/sh
printf '%s|' "$@" >> '` + log + `'; echo >> '` + log + `'
case "$1" in
workspace) echo "OK workspace:42" ;;
read-screen) ` + readScreen + ` ;;
*) echo OK ;;
esac
`
	bin := filepath.Join(dir, "cmux")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return cmuxTerminal{bin: bin}, log
}

func TestCmuxTerminalCallsTheCLI(t *testing.T) {
	c, log := fakeCmux(t, `echo "Error: Workspace ref not found: $3"; exit 1`)
	ws, err := c.open(t.Context(), "tincan: x", "/w")
	if err != nil || ws != "workspace:42" {
		t.Fatalf("open = %q, %v", ws, err)
	}
	if err := c.typeLine(t.Context(), ws, "do it"); err != nil {
		t.Fatal(err)
	}
	if err := c.typeLine(t.Context(), ws, "a\nb"); err == nil {
		t.Error("a multi-line typed line: want an error")
	}
	if _, err := c.screen(t.Context(), ws); !errors.Is(err, errWindowGone) {
		t.Errorf("screen of a gone workspace = %v, want errWindowGone", err)
	}
	got, _ := os.ReadFile(log)
	want := "workspace|create|--name|tincan: x|--cwd|/w|--focus|false|\n" +
		"send|--workspace|workspace:42|do it|\n" +
		"send-key|--workspace|workspace:42|enter|\n" +
		"read-screen|--workspace|workspace:42|\n"
	if string(got) != want {
		t.Errorf("cmux calls:\n%s\nwant:\n%s", got, want)
	}
}

func TestCmuxErrorNamesTheSocketSetting(t *testing.T) {
	c, _ := fakeCmux(t, `echo "Access denied - only processes started inside cmux can connect"; exit 1`)
	_, err := c.screen(t.Context(), "workspace:1")
	if err == nil || !strings.Contains(err.Error(), "socketControlMode") {
		t.Errorf("err = %v, want it to name socketControlMode", err)
	}
}

func TestNewWindowTerminalRefusesOtherTerminals(t *testing.T) {
	if _, err := newWindowTerminal("ghostty", ""); err == nil || !strings.Contains(err.Error(), "cmux") {
		t.Errorf("err = %v", err)
	}
}

func TestWindowServicePlist(t *testing.T) {
	p := windowServicePlist(windowServiceOptions{
		binary: "/u/bin/tincan", config: "/h/.config/tincan/window.json", path: "/usr/bin:/a&b", home: "/h",
		launch:     windowLaunch{dir: "/h/code <x>", terminal: "cmux", cmux: "/A/cmux", claude: "/c/claude", starts: "auto"},
		maxWindows: 3, timeout: 12 * time.Hour,
	})
	for _, want := range []string{
		"<string>com.agenttincan.window</string>",
		"<string>/u/bin/tincan</string>\n    <string>window</string>\n    <string>serve</string>",
		"<string>/h/code &lt;x&gt;</string>",
		"<string>/h/.config/tincan/window.json</string>",
		"<string>/usr/bin:/a&amp;b</string>",
		"<string>12h0m0s</string>",
		"/h/Library/Logs/tincan-window.log",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist lacks %q:\n%s", want, p)
		}
	}
	f := filepath.Join(t.TempDir(), "w.plist")
	os.WriteFile(f, []byte(p), 0o644)
	if _, err := os.Stat("/usr/bin/plutil"); err == nil {
		if out, err := exec.Command("/usr/bin/plutil", "-lint", f).CombinedOutput(); err != nil {
			t.Errorf("plutil -lint: %v %s", err, out)
		}
	}
}

func TestWindowNameAndRunName(t *testing.T) {
	if got := windowName("fix the relay\nmore"); got != "tincan: fix the relay" {
		t.Errorf("windowName = %q", got)
	}
	if got := safeRunName("../x y"); strings.Contains(got, "/") || got == "" {
		t.Errorf("safeRunName = %q", got)
	}
	if got := safeRunName(".."); got == ".." || strings.Contains(got, "/") {
		t.Errorf("safeRunName(..) = %q", got)
	}
}

func replyBody(r envelope.Result) string {
	if r.Reply == nil {
		return ""
	}
	return r.Reply.Body
}
