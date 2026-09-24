package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/mcpserver"
)

// check is one line of the doctor's report.
type check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok, warn, or fail
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

type doctorReport struct {
	Version  string             `json:"version"`
	Binary   string             `json:"binary"`
	Checks   []check            `json:"checks"`
	Launches []mcpserver.Launch `json:"launches,omitempty"`
	Configs  []mcpConfigEntry   `json:"mcp_configs,omitempty"`
	Fix      []string           `json:"fix,omitempty"`
	OK       bool               `json:"ok"`
}

func doctorCmd() *cobra.Command {
	var asJSON bool
	var configs []string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Find out why this agent's tincan tools are missing, and how to fix it",
		Long: `Check everything between this agent and its teammates, and print a fix for
each problem found. Run it whenever the tincan MCP tools are missing, show 0
tools, or stop answering. It works without MCP, so an agent whose app lost
the tools can still run it from a shell.

It checks, in order: the saved join, the relay, whether this binary is the
relay's current release, a self-test of tincan mcp in both stdio framings,
the MCP config entries that point at tincan, and whether the app has
actually been starting tincan mcp (every tincan mcp records its launch).

Exit status is 1 when a check fails.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			exe, err := os.Executable()
			if err == nil {
				exe, _ = filepath.EvalSymlinks(exe)
			}
			rep := runDoctor(cmd.Context(), exe, configs)
			if asJSON {
				if err := printJSON(cmd, rep); err != nil {
					return err
				}
			} else {
				printDoctor(cmd.OutOrStdout(), rep)
			}
			if !rep.OK {
				return errDoctorFailed
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the report as JSON")
	cmd.Flags().StringSliceVar(&configs, "config", nil, "extra MCP config file to inspect (repeatable)")
	return cmd
}

// errDoctorFailed makes tincan exit 1 without repeating the report.
var errDoctorFailed = errors.New("tincan doctor found problems (see above)")

func runDoctor(ctx context.Context, exe string, extraConfigs []string) doctorReport {
	rep := doctorReport{Version: Version, Binary: exe}
	add := func(c check) { rep.Checks = append(rep.Checks, c) }

	// 1. Join and relay.
	cfg, err := client.LoadConfig()
	joined := false
	var r *client.Relay
	switch {
	case err != nil:
		add(check{"config", "fail", fmt.Sprintf("cannot read %s: %v", client.ConfigPath(), err), "Fix or delete the file, then run tincan rejoin --relay <relay URL>."})
	case cfg.Relay == "":
		add(check{"config", "fail", "no relay configured at " + client.ConfigPath(), "Run tincan rejoin --relay <relay URL> (a machine that was never joined needs an invite: tincan join <code> --relay <relay URL>)."})
	default:
		add(check{"config", "ok", fmt.Sprintf("relay %s, agent %q (%s)", cfg.Relay, cfg.Agent, client.ConfigPath()), ""})
		r, err = client.NewRelayFor(cfg)
		if err != nil {
			add(check{"relay", "fail", err.Error(), "Check the relay URL and proxy in " + client.ConfigPath() + "."})
			break
		}
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		me, err := r.WhoAmI(cctx)
		cancel()
		switch {
		case client.IsNotJoined(err):
			add(check{"relay", "fail", "the relay does not know this machine: " + err.Error(), "Run tincan rejoin. If it says the machine was never joined, ask the owner for an invite."})
		case err != nil:
			add(check{"relay", "fail", "cannot reach the relay: " + err.Error(), "Check that this machine is on the tailnet (tailscale status) and the relay is running."})
		default:
			joined = true
			add(check{"relay", "ok", fmt.Sprintf("reachable; this machine is agent %q", me.Name), ""})
		}
	}

	// 2. Version against the relay's release.
	if joined && exe != "" {
		add(versionCheck(ctx, r, exe))
	}

	// 3. Self-test of tincan mcp in both framings.
	if exe != "" {
		for _, framed := range []bool{false, true} {
			add(probeCheck(ctx, exe, framed, joined))
		}
	}

	// 4. MCP config entries that point at tincan.
	rep.Configs = findMCPConfigs(extraConfigs)
	add(configCheck(rep.Configs, exe))

	// 5. Whether an app has been starting tincan mcp at all.
	rep.Launches = mcpserver.ReadLaunches(mcpserver.LaunchDir(client.ConfigPath()))
	if len(rep.Launches) > 5 {
		rep.Launches = rep.Launches[:5]
	}
	add(launchCheck(rep.Launches, time.Now()))

	rep.OK = true
	hostProblem := false
	for _, c := range rep.Checks {
		if c.Status == "fail" {
			rep.OK = false
		}
		if c.Status != "ok" && (c.Name == "app launches" || c.Name == "mcp config") {
			hostProblem = true
		}
	}
	if hostProblem {
		rep.Fix = hostFix(exe)
	}
	return rep
}

func versionCheck(ctx context.Context, r *client.Relay, exe string) check {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	m, err := r.Dist(cctx)
	if err != nil {
		return check{"version", "warn", fmt.Sprintf("tincan %s; the relay does not say which release is current (%v)", Version, err), ""}
	}
	want := m.SHA256Of("tincan_" + runtime.GOOS + "_" + runtime.GOARCH)
	have, err := fileSHA256(exe)
	if want == "" || err != nil {
		return check{"version", "warn", fmt.Sprintf("tincan %s; relay release %s has no build to compare for this platform", Version, m.Version), ""}
	}
	if have != want {
		return check{"version", "fail", fmt.Sprintf("tincan %s at %s is not the relay's release %s", Version, exe, m.Version), "Run tincan upgrade, then restart the app that runs tincan mcp so it starts the new build."}
	}
	return check{"version", "ok", fmt.Sprintf("tincan %s matches the relay's release", Version), ""}
}

// probeCheck starts exe mcp the way an app would and asks it for its tools.
func probeCheck(ctx context.Context, exe string, framed, joined bool) check {
	name := "mcp self-test (newline)"
	if framed {
		name = "mcp self-test (content-length)"
	}
	tools, err := probeMCP(ctx, exe, framed)
	switch {
	case err != nil && !joined:
		return check{name, "fail", "tincan mcp did not start: " + err.Error(), "Fix the config and relay checks above first; tincan mcp needs a joined agent."}
	case err != nil:
		return check{name, "fail", "tincan mcp did not answer: " + err.Error(), "Run tincan upgrade. If this still fails, report it with the output of tincan doctor --json."}
	}
	var missing []string
	for _, t := range mcpserver.ToolNames {
		if !slices.Contains(tools, t) {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		return check{name, "fail", fmt.Sprintf("tincan mcp listed %d tools, missing %s", len(tools), strings.Join(missing, ", ")), "Run tincan upgrade."}
	}
	return check{name, "ok", fmt.Sprintf("%s mcp lists all %d tools", exe, len(tools)), ""}
}

// probeMCP runs initialize and tools/list against exe mcp and returns the
// tool names. The probe is not recorded as an app launch.
func probeMCP(ctx context.Context, exe string, framed bool) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, exe, "mcp")
	c.Env = append(os.Environ(), mcpserver.ProbeEnv+"=1")
	in, err := c.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := c.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr strings.Builder
	c.Stderr = &stderr
	if err := c.Start(); err != nil {
		return nil, err
	}
	defer func() {
		in.Close()
		_ = c.Wait()
	}()
	send := func(msg string) error {
		if framed {
			_, err := fmt.Fprintf(in, "Content-Length: %d\r\n\r\n%s", len(msg), msg)
			return err
		}
		_, err := io.WriteString(in, msg+"\n")
		return err
	}
	msgs := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"tincan-doctor","version":"` + Version + `"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	}
	for _, m := range msgs {
		if err := send(m); err != nil {
			return nil, withStderr(err, stderr.String())
		}
	}
	br := bufio.NewReader(out)
	for {
		raw, err := readMessage(br, framed)
		if err != nil {
			if ctx.Err() != nil {
				err = errors.New("no reply within 20s")
			}
			return nil, withStderr(err, stderr.String())
		}
		var resp struct {
			ID     json.RawMessage `json:"id"`
			Result struct {
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			} `json:"result"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &resp) != nil || string(resp.ID) != "2" {
			continue
		}
		if resp.Error != nil {
			return nil, errors.New(resp.Error.Message)
		}
		names := make([]string, len(resp.Result.Tools))
		for i, t := range resp.Result.Tools {
			names[i] = t.Name
		}
		return names, nil
	}
}

func withStderr(err error, stderr string) error {
	if s := strings.TrimSpace(stderr); s != "" {
		return fmt.Errorf("%w (stderr: %s)", err, s)
	}
	return err
}

// readMessage reads one reply in the framing the probe used.
func readMessage(br *bufio.Reader, framed bool) ([]byte, error) {
	if !framed {
		line, err := br.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		return line, nil
	}
	length := -1
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if length >= 0 {
				break
			}
			continue
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Content-Length") {
			if length, err = strconv.Atoi(strings.TrimSpace(v)); err != nil {
				return nil, fmt.Errorf("bad Content-Length %q", v)
			}
		}
	}
	body := make([]byte, length)
	_, err := io.ReadFull(br, body)
	return body, err
}

// launchCheck reads the launch records every tincan mcp writes. They are
// the one view of what the app does: a host can show a server as connected
// with 0 tools without ever running it.
func launchCheck(ls []mcpserver.Launch, now time.Time) check {
	const name = "app launches"
	if len(ls) == 0 {
		return check{name, "warn", "no record of any app starting tincan mcp on this machine (every tincan mcp from this release on records its start). If your agent's tincan tools are missing, its app is not running tincan at all.", "Follow the steps under \"To fix the app's tincan connection\" below."}
	}
	l := ls[0]
	who := l.Client
	if who == "" {
		who = l.Parent
	}
	if who == "" {
		who = "an app"
	}
	when := fmt.Sprintf("%s ago", now.Sub(l.Started).Round(time.Second))
	switch {
	case l.Error != "" && l.Initialized.IsZero():
		return check{name, "fail", fmt.Sprintf("%s started tincan mcp %s but it stopped before the app connected: %s", who, when, l.Error), "Fix that error (tincan rejoin for a join problem), then restart the app."}
	case l.Initialized.IsZero() && !l.Ended.IsZero():
		return check{name, "fail", fmt.Sprintf("%s started tincan mcp %s and closed it without ever sending initialize", who, when), "The app gave up on tincan before talking to it. Remove and re-add the tincan server in the app, then restart the app."}
	case l.Initialized.IsZero():
		return check{name, "warn", fmt.Sprintf("%s started tincan mcp %s (pid %d) and has not sent initialize yet", who, when, l.PID), "If the tools are still missing, restart the app."}
	case l.ToolsListed.IsZero():
		return check{name, "fail", fmt.Sprintf("%s connected %s (framing %s) but never asked for the tool list, so it shows 0 tools", who, when, l.Framing), "Remove and re-add the tincan server in the app, then restart the app fully."}
	}
	detail := fmt.Sprintf("%s started tincan mcp %s and listed the tools", who, when)
	if l.ToolCalls > 0 {
		detail += " and called them"
	}
	if !l.Ended.IsZero() {
		detail += fmt.Sprintf("; that process ended %s ago", now.Sub(l.Ended).Round(time.Second))
		if l.Error != "" {
			detail += " with: " + l.Error
		}
		return check{name, "warn", detail, "If the tools are missing now, the app has not started tincan since. Restart the app."}
	}
	if l.Version != "" && l.Version != Version {
		return check{name, "warn", detail + fmt.Sprintf(" (running tincan %s, this binary is %s)", l.Version, Version), "Restart the app so it runs the current build."}
	}
	return check{name, "ok", detail, ""}
}

// hostFix is the repair for an app that lists tincan but does not run it.
func hostFix(exe string) []string {
	if exe == "" {
		exe = "/path/to/tincan"
	}
	entry, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"tincan": map[string]any{"command": exe, "args": []string{"mcp"}}}})
	return []string{
		"Remove every tincan server from the app, including old or oddly named ones (for example \"user-tincan\" or \"user-tincan mcp\"). The mcp config entries above show where they are.",
		"Add exactly one server named tincan: " + string(entry),
		"Use the full path to the binary and put mcp in args, not in the command. Claude Code adds --channel after mcp for channel wakes.",
		"Quit and reopen the app (a reload of one server is not always enough), then start a new chat.",
		"Run tincan doctor again: app launches should say the app listed the tools.",
	}
}

func printDoctor(w io.Writer, rep doctorReport) {
	fmt.Fprintf(w, "tincan %s at %s\n\n", rep.Version, rep.Binary)
	for _, c := range rep.Checks {
		fmt.Fprintf(w, "[%s] %s: %s\n", strings.ToUpper(c.Status), c.Name, c.Detail)
		if c.Fix != "" {
			fmt.Fprintf(w, "       fix: %s\n", c.Fix)
		}
	}
	if len(rep.Configs) > 0 {
		fmt.Fprintln(w, "\nMCP config entries that mention tincan:")
		for _, e := range rep.Configs {
			fmt.Fprintf(w, "  %s: %q runs %s %s", e.File, e.Name, e.Command, strings.Join(e.Args, " "))
			if len(e.Problems) > 0 {
				fmt.Fprintf(w, "  <- %s", strings.Join(e.Problems, "; "))
			}
			fmt.Fprintln(w)
		}
	}
	if len(rep.Fix) > 0 {
		fmt.Fprintln(w, "\nTo fix the app's tincan connection:")
		for i, s := range rep.Fix {
			fmt.Fprintf(w, "  %d. %s\n", i+1, s)
		}
	}
	if rep.OK {
		fmt.Fprintln(w, "\nNo failures.")
	}
}
