package watch

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/history"
)

// rig drives a Watcher one probe per simulated minute.
type rig struct {
	w      *Watcher
	now    time.Time
	up     bool
	alerts []Event
	fail   int // alert calls still to fail
	logs   []string
}

func newRig() *rig {
	r := &rig{now: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC), up: true}
	r.w = &Watcher{
		Relay: func() string { return "http://tincan-relay" },
		After: 10 * time.Minute,
		Now:   func() time.Time { return r.now },
		Probe: func(context.Context) error {
			if r.up {
				return nil
			}
			return errors.New("dial tcp: connection refused")
		},
		Alert: func(_ context.Context, e Event) error {
			if r.fail > 0 {
				r.fail--
				return errors.New("exit status 1")
			}
			r.alerts = append(r.alerts, e)
			return nil
		},
		Logf: func(f string, a ...any) { r.logs = append(r.logs, fmt.Sprintf(f, a...)) },
	}
	return r
}

// minutes probes once a minute for n minutes, the relay up or down.
func (r *rig) minutes(t *testing.T, n int, up bool) {
	t.Helper()
	r.up = up
	for range n {
		r.w.Check(t.Context())
		r.now = r.now.Add(time.Minute)
	}
}

func TestRelayUpThroughoutNeverAlerts(t *testing.T) {
	r := newRig()
	r.minutes(t, 60, true)
	if len(r.alerts) != 0 {
		t.Fatalf("alerts while up: %+v", r.alerts)
	}
}

func TestShortOutageDoesNotAlert(t *testing.T) {
	r := newRig()
	r.minutes(t, 10, false) // failed probes at minutes 0..9: down 9 minutes
	r.minutes(t, 30, true)
	if len(r.alerts) != 0 {
		t.Fatalf("alerts for a 9 minute outage: %+v", r.alerts)
	}
}

func TestOutageAlertsOnceAfterTenMinutes(t *testing.T) {
	r := newRig()
	start := r.now
	r.minutes(t, 10, false)
	if len(r.alerts) != 0 {
		t.Fatalf("alert before 10 minutes: %+v", r.alerts)
	}
	r.minutes(t, 30, false)
	if len(r.alerts) != 1 {
		t.Fatalf("alerts = %d, want 1 (once, not once per probe): %+v", len(r.alerts), r.alerts)
	}
	e := r.alerts[0]
	if e.Kind != Down || e.Relay != "http://tincan-relay" || !e.Since.Equal(start) {
		t.Fatalf("down event %+v", e)
	}
	for _, want := range []string{"http://tincan-relay", "10m", "connection refused"} {
		if !strings.Contains(e.Message, want) {
			t.Errorf("down message %q lacks %q", e.Message, want)
		}
	}
}

func TestRecoveryAlertsOnce(t *testing.T) {
	r := newRig()
	r.minutes(t, 25, false)
	r.minutes(t, 30, true)
	if len(r.alerts) != 2 {
		t.Fatalf("alerts = %+v, want down then up", r.alerts)
	}
	e := r.alerts[1]
	if e.Kind != Up || e.Relay != "http://tincan-relay" {
		t.Fatalf("recovery event %+v", e)
	}
	for _, want := range []string{"http://tincan-relay", "back", "25m"} {
		if !strings.Contains(e.Message, want) {
			t.Errorf("recovery message %q lacks %q", e.Message, want)
		}
	}
	// A second outage is a new episode with its own alert.
	r.minutes(t, 11, false)
	if len(r.alerts) != 3 || r.alerts[2].Kind != Down {
		t.Fatalf("second outage alerts %+v", r.alerts)
	}
}

func TestFailedAlertIsLoggedAndRetriedEachProbe(t *testing.T) {
	r := newRig()
	r.fail = 2
	r.minutes(t, 11, false) // first try at minute 10 fails
	if len(r.alerts) != 0 {
		t.Fatalf("alerts %+v", r.alerts)
	}
	if !strings.Contains(strings.Join(r.logs, "\n"), "exit status 1") {
		t.Fatalf("failed alert not logged: %q", r.logs)
	}
	r.minutes(t, 1, false) // second try fails
	r.minutes(t, 1, false) // third succeeds
	r.minutes(t, 5, false)
	if len(r.alerts) != 1 || r.alerts[0].Kind != Down {
		t.Fatalf("alerts after retries %+v, want one down", r.alerts)
	}

	r.fail = 1
	r.minutes(t, 1, true) // recovery alert fails
	r.minutes(t, 1, true) // retried, succeeds
	r.minutes(t, 5, true)
	if len(r.alerts) != 2 || r.alerts[1].Kind != Up {
		t.Fatalf("alerts after recovery retry %+v, want down then one up", r.alerts)
	}
}

func TestRunProbesUntilCancelled(t *testing.T) {
	probes := make(chan struct{}, 10)
	w := &Watcher{
		Relay: func() string { return "http://tincan-relay" },
		After: time.Hour,
		Probe: func(context.Context) error { probes <- struct{}{}; return nil },
		Alert: func(context.Context, Event) error { return nil },
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx, time.Millisecond) }()
	for range 3 {
		select {
		case <-probes:
		case <-time.After(5 * time.Second):
			t.Fatal("no probe")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestCommandPassesEventInEnvironment(t *testing.T) {
	out := filepath.Join(t.TempDir(), "env")
	alert := Command(`printf '%s|%s|%s|%s' "$TINCAN_WATCH_EVENT" "$TINCAN_WATCH_RELAY" "$TINCAN_WATCH_SINCE" "$TINCAN_WATCH_MESSAGE" > ` + out)
	since := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	if err := alert(t.Context(), Event{Kind: Down, Relay: "http://r", Since: since, Message: "relay down; it's 'quoted'"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), "down|http://r|2026-10-05T12:00:00Z|relay down; it's 'quoted'"; got != want {
		t.Fatalf("command saw %q, want %q", got, want)
	}
}

func TestCommandNonZeroExitIsAnError(t *testing.T) {
	err := Command("echo no messages app >&2; exit 3")(t.Context(), Event{Kind: Down, Message: "m"})
	if err == nil || !strings.Contains(err.Error(), "exit status 3") || !strings.Contains(err.Error(), "no messages app") {
		t.Fatalf("err = %v, want exit status 3 with the command's output", err)
	}
}

// TestIMessageAlertScript runs examples/watch/imessage-alert.sh against a
// stub osascript and checks the recipient and message reach it as
// arguments.
func TestIMessageAlertScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script")
	}
	script, err := filepath.Abs("../../examples/watch/imessage-alert.sh")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	args := filepath.Join(t.TempDir(), "args")
	stub := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > " + args + "\n"
	if err := os.WriteFile(filepath.Join(bin, "osascript"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(env ...string) (string, error) {
		cmd := exec.Command(script)
		cmd.Env = append([]string{"PATH=" + bin + ":/usr/bin:/bin"}, env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	if _, err := run("TINCAN_ALERT_TO=owner@example.com", "TINCAN_WATCH_MESSAGE=relay down; it's 'quoted'"); err != nil {
		t.Fatalf("script failed: %v", err)
	}
	b, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.HasSuffix(got, "owner@example.com\nrelay down; it's 'quoted'\n") {
		t.Fatalf("osascript args end %q, want recipient then message", got)
	}
	if !strings.Contains(got, "Messages") {
		t.Fatalf("osascript program does not address Messages: %q", got)
	}

	if out, err := run("TINCAN_WATCH_MESSAGE=m"); err == nil || !strings.Contains(out, "TINCAN_ALERT_TO") {
		t.Fatalf("no recipient: err %v, output %q; want a failure naming TINCAN_ALERT_TO", err, out)
	}
}

func plistStrings(s string) string {
	dec := xml.NewDecoder(strings.NewReader(s))
	var strs []string
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		if cd, ok := tok.(xml.CharData); ok && strings.TrimSpace(string(cd)) != "" {
			strs = append(strs, string(cd))
		}
	}
	return strings.Join(strs, "|")
}

func TestInstallServiceDarwin(t *testing.T) {
	home := t.TempDir()
	res, err := InstallService(ServiceOptions{
		ServiceOptions: history.ServiceOptions{GOOS: "darwin", Home: home, Binary: "/opt/tin & can/tincan", UID: 501},
		Config:         "/Users/o/.config/tincan/hermes.json",
		AlertCmd:       `TINCAN_ALERT_TO=owner@example.com "/Users/o/bin/imessage-alert.sh" && echo <ok>`,
		After:          10 * time.Minute,
		Every:          time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "Library", "LaunchAgents", "com.agenttincan.relaywatch.plist"); res.Path != want {
		t.Fatalf("path = %s, want %s", res.Path, want)
	}
	b, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "__") {
		t.Fatalf("placeholder left in plist:\n%s", s)
	}
	joined := plistStrings(s)
	for _, want := range []string{
		"Label|com.agenttincan.relaywatch",
		"/opt/tin & can/tincan|relay-watch|--config|/Users/o/.config/tincan/hermes.json|--alert-cmd|" +
			`TINCAN_ALERT_TO=owner@example.com "/Users/o/bin/imessage-alert.sh" && echo <ok>` +
			"|--after|10m0s|--every|1m0s|EnvironmentVariables",
		"KeepAlive",
		home + "/Library/Logs/tincan-relay-watch.log",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("plist missing %q:\n%s", want, joined)
		}
	}
	if res.Next != "launchctl bootstrap gui/501 "+res.Path {
		t.Fatalf("next step = %q", res.Next)
	}
}

func TestInstallServiceLinux(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	home := t.TempDir()
	res, err := InstallService(ServiceOptions{
		ServiceOptions: history.ServiceOptions{GOOS: "linux", Home: home, Binary: "/opt/tin can/tincan"},
		Config:         "/home/o/.config/tincan/hermes.json",
		AlertCmd:       `notify "$HOME" 100%`,
		After:          10 * time.Minute,
		Every:          time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != filepath.Join(home, ".config", "systemd", "user", "tincan-relay-watch.service") {
		t.Fatalf("path = %s", res.Path)
	}
	b, _ := os.ReadFile(res.Path)
	s := string(b)
	// systemd reads \" as a quote, $$ as $ and %% as %, so the command
	// reaches relay-watch unchanged.
	want := `ExecStart="/opt/tin can/tincan" relay-watch --config "/home/o/.config/tincan/hermes.json" --alert-cmd "notify \"$$HOME\" 100%%" --after 10m0s --every 1m0s` + "\n"
	if !strings.Contains(s, want) {
		t.Errorf("unit missing %q:\n%s", want, s)
	}
	if !strings.Contains(s, "Restart=always") {
		t.Errorf("unit does not restart:\n%s", s)
	}
	if !strings.Contains(res.Next, "systemctl --user enable --now tincan-relay-watch.service") {
		t.Fatalf("next = %q", res.Next)
	}
}

func TestInstallServiceRefusesWhatItCannotWrite(t *testing.T) {
	base := ServiceOptions{
		ServiceOptions: history.ServiceOptions{GOOS: "darwin", Home: t.TempDir(), Binary: "/usr/local/bin/tincan", UID: 501},
		Config:         "/c.json",
		AlertCmd:       "true",
		After:          time.Minute,
		Every:          time.Minute,
	}
	for name, mut := range map[string]func(*ServiceOptions){
		"no alert command":     func(o *ServiceOptions) { o.AlertCmd = "" },
		"newline in command":   func(o *ServiceOptions) { o.AlertCmd = "a\nb" },
		"relative config path": func(o *ServiceOptions) { o.Config = "c.json" },
	} {
		o := base
		mut(&o)
		if _, err := InstallService(o); err == nil {
			t.Errorf("%s: installed", name)
		}
	}
}

func TestSpanReadsWholeHoursPlainly(t *testing.T) {
	for d, want := range map[time.Duration]string{
		20 * time.Second:             "under a minute",
		10 * time.Minute:             "10m",
		time.Hour:                    "1h",
		2*time.Hour + 5*time.Minute:  "2h5m",
		2*time.Hour + 10*time.Minute: "2h10m",
	} {
		if got := span(d); got != want {
			t.Errorf("span(%v) = %q, want %q", d, got, want)
		}
	}
}

// A recovery alert that keeps failing does not hide the next outage: that
// outage gets its own down alert, and the old recovery is still sent once
// alerts work again, naming when the relay came back.
func TestFailedRecoveryAlertDoesNotMaskNextOutage(t *testing.T) {
	r := newRig()
	r.minutes(t, 11, false) // down alert at minute 10
	back := r.now
	r.fail = 3
	r.minutes(t, 1, true)   // recovery alert fails
	r.minutes(t, 12, false) // down again; the retries fail twice, then work
	var kinds []string
	for _, e := range r.alerts {
		kinds = append(kinds, string(e.Kind))
	}
	if got := strings.Join(kinds, ","); got != "down,up,down" {
		t.Fatalf("alerts = %s, want down,up,down", got)
	}
	if !strings.Contains(r.alerts[1].Message, back.Local().Format("Jan 2 15:04 MST")) {
		t.Errorf("late recovery alert %q does not say when the relay came back", r.alerts[1].Message)
	}
	if !r.alerts[2].Since.After(back) {
		t.Errorf("second down alert since %v, want the new outage after %v", r.alerts[2].Since, back)
	}
}

// A recovery alert still failing when the next outage's down alert goes out
// is folded into that alert and never sent after it, so the owner is not
// told the relay is back while it is down.
func TestStaleRecoveryFoldsIntoNextDownAlert(t *testing.T) {
	r := newRig()
	r.minutes(t, 11, false) // down alert at minute 10
	back := r.now
	r.fail = 12
	r.minutes(t, 1, true)   // recovery alert fails
	r.minutes(t, 11, false) // every retry fails through the next outage's first ten minutes
	r.fail = 0
	r.minutes(t, 5, false) // the new outage's down alert goes out
	var kinds []string
	for _, e := range r.alerts {
		kinds = append(kinds, string(e.Kind))
	}
	if got := strings.Join(kinds, ","); got != "down,down" {
		t.Fatalf("alerts = %s, want down,down with no stale up", got)
	}
	if msg := r.alerts[1].Message; !strings.Contains(msg, "was back at "+back.Local().Format("Jan 2 15:04 MST")) {
		t.Errorf("second down alert %q does not carry the missed recovery", msg)
	}
}

func TestInstallServiceDarwinUsesLauncher(t *testing.T) {
	home := t.TempDir()
	res, err := InstallService(ServiceOptions{
		ServiceOptions: history.ServiceOptions{GOOS: "darwin", Home: home, Binary: "/opt/tincan", UID: 501, Launcher: "/A/agent-tincan"},
		Config:         "/c.json",
		AlertCmd:       "true",
		After:          10 * time.Minute,
		Every:          time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res.Path)
	joined := plistStrings(string(b))
	for _, want := range []string{"ProgramArguments|/A/agent-tincan|/opt/tincan|relay-watch|--config|/c.json", "AssociatedBundleIdentifiers|com.agenttincan.app"} {
		if !strings.Contains(joined, want) {
			t.Errorf("plist missing %q:\n%s", want, joined)
		}
	}
}
