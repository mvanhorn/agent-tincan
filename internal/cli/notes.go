package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/notes"
)

func notesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "notes",
		Short: "Run the notes agent, which saves and finds notes in Agent Notes for teammates",
		Long: "The notes agent runs on the Mac with Agent Notes and drives its bundled agent-notes helper:\n" +
			"teammates ask it to save a note, search notes, or read one back. Set it up once with\n" +
			"tincan notes install, then check it with tincan notes doctor. See docs/adapters/notes.md.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(notesServeCmd(), notesInstallCmd(), notesDoctorCmd())
	return cmd
}

// defaultNotesAppSupport is the service's own Application Support root on
// this machine.
func defaultNotesAppSupport() string {
	home, _ := os.UserHomeDir()
	return notes.DefaultAppSupportRoot(runtime.GOOS, home)
}

// notesPaths resolves the --app-support and --spool-dir flags.
func notesPaths(appSupport, spoolDir string) (string, string) {
	if appSupport == "" {
		appSupport = defaultNotesAppSupport()
	}
	appSupport = expandHome(appSupport)
	if spoolDir == "" {
		spoolDir = notes.SpoolDirIn(appSupport)
	}
	return appSupport, expandHome(spoolDir)
}

func notesServeCmd() *cobra.Command {
	var configPath, libraryRoot, helperPath, appSupport, spoolDir, readAllow, addAllow, scratchDir string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the notes agent: save, search and read Agent Notes notes for teammates over the relay",
		Long: "Long-polls the relay as the notes agent and handles each request with the agent-notes helper,\n" +
			"run against --library-root with the service's own Application Support root. An add that cannot be\n" +
			"applied yet is kept in the spool and retried, so it is never lost. Free-text requests go through a\n" +
			"tool-less codex exec call that turns the request text into a structured one.\n" +
			"The allowlist files (one agent per line) limit who may search and read, and who may add; a missing\n" +
			"file lets every joined agent. The last helper result is written for tincan notes doctor.\n" +
			"Normally started by the service definition tincan notes install writes. See docs/adapters/notes.md.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if libraryRoot == "" {
				return errors.New("--library-root is required: the Agent Notes library folder the notes agent saves into")
			}
			if configPath == "" {
				configPath = serviceConfigPath(notes.DefaultAgentName)
			}
			configPath = expandHome(configPath)
			cfg, err := client.LoadConfigFrom(configPath)
			if err != nil {
				return fmt.Errorf("notes config %s: %w", configPath, err)
			}
			if cfg.Relay == "" {
				return fmt.Errorf("no relay configured in %s: run TINCAN_CONFIG=%s tincan join <code> --relay http://tincan-relay", configPath, configPath)
			}
			if cfg.Agent != "" && cfg.Agent != notes.DefaultAgentName {
				return wrongServeAgent("notes serve", notes.DefaultAgentName, configPath+" is joined as", cfg.Agent)
			}
			appSupport, spoolDir = notesPaths(appSupport, spoolDir)
			if scratchDir == "" {
				scratchDir = filepath.Join(appSupport, "codex-scratch")
			}
			r, err := client.NewRelayForFile(cfg, configPath)
			if err != nil {
				return err
			}
			svc, err := notes.New(notes.Config{
				Relay:             r,
				HelperPath:        expandHome(helperPath),
				LibraryRoot:       expandHome(libraryRoot),
				AppSupportRoot:    appSupport,
				SpoolDir:          spoolDir,
				ReadAllowlistPath: expandHome(readAllow),
				AddAllowlistPath:  expandHome(addAllow),
				HealthPath:        notes.HealthPathIn(appSupport),
				Extractor:         notes.NewCodexExtractor(expandHome(scratchDir)),
				Log:               cmd.ErrOrStderr(),
			})
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if client.NeedsRelayInfo(cfg) {
				learnRelayKeyWithin(ctx, r)
			}
			// The relay, not the config file, says who this machine is.
			me, err := r.WhoAmI(ctx)
			if err != nil {
				return fmt.Errorf("notes serve: could not confirm this agent's identity with the relay: %w", client.RejoinHint(err, cfg.Relay))
			}
			if me.Name != notes.DefaultAgentName {
				return wrongServeAgent("notes serve", notes.DefaultAgentName, "the relay knows the machine using "+configPath+" as", me.Name)
			}
			if me.Kind != notes.DefaultAgentName {
				cmd.PrintErrf("tincan notes: warning: the relay stores kind %q for this agent, not \"notes\", so adds wait only 24 hours for this Mac; fix it with: tincan kind notes notes\n", me.Kind)
			}
			cmd.PrintErrf("tincan notes: serving as %s on %s, library %s\n", me.Name, cfg.Relay, libraryRoot)
			err = svc.Run(ctx)
			if ctx.Err() != nil {
				cmd.PrintErrln("tincan notes: stopped")
				return nil
			}
			return err
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "the notes agent's client config (default: $TINCAN_CONFIG, else ~/.config/tincan/notes.json)")
	cmd.Flags().StringVar(&libraryRoot, "library-root", "", "the Agent Notes library folder notes are saved into and read from (required)")
	cmd.Flags().StringVar(&helperPath, "helper", notes.DefaultHelperPath, "the agent-notes helper")
	cmd.Flags().StringVar(&appSupport, "app-support", "", "the service's own Application Support root (default: ~/Library/Application Support/tincan-notes)")
	cmd.Flags().StringVar(&spoolDir, "spool-dir", "", "where adds not yet applied are kept (default: <app-support>/spool)")
	cmd.Flags().StringVar(&readAllow, "read-allowlist", "~/.config/tincan/notes-allow.txt", "file of agents allowed to search and read notes, one per line (missing means every joined agent)")
	cmd.Flags().StringVar(&addAllow, "add-allowlist", "~/.config/tincan/notes-add-allow.txt", "file of agents allowed to add notes, one per line (missing means every joined agent)")
	cmd.Flags().StringVar(&scratchDir, "codex-scratch", "", "empty working directory for the tool-less codex step (default: <app-support>/codex-scratch)")
	return cmd
}

func notesInstallCmd() *cobra.Command {
	var libraryRoot, helperPath, binary string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Write the notes service definition (launchd on macOS, systemd on Linux); does not start it",
		Long: "Writes a launchd agent (" + notes.ServiceLabel + ") on macOS, or a systemd user unit on Linux, that runs\n" +
			"tincan notes serve against --library-root with TINCAN_CONFIG=~/.config/tincan/notes.json and keeps it\n" +
			"running. It prints the command that starts the service and never starts it itself.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if libraryRoot == "" {
				return errors.New("--library-root is required: the Agent Notes library folder the notes agent saves into")
			}
			root, err := filepath.Abs(expandHome(libraryRoot))
			if err != nil {
				return err
			}
			res, err := notes.InstallService(notes.ServiceOptions{
				Binary:      binary,
				LibraryRoot: root,
				HelperPath:  expandHome(helperPath),
			})
			if err != nil {
				return err
			}
			cmd.Printf("notes service definition: %s (not started)\n", res.Path)
			home, _ := os.UserHomeDir()
			cfgPath := filepath.Join(home, ".config", "tincan", "notes.json")
			if cfg, err := client.LoadConfigFrom(cfgPath); err != nil || cfg.Relay == "" {
				cmd.Printf("the notes agent is not joined yet; first run:\n  TINCAN_CONFIG=%s tincan join <code> --relay <relay url>\n", cfgPath)
			}
			cmd.Printf("start it with:\n  %s\n", res.Next)
			cmd.Println("then check it with: tincan notes doctor")
			return nil
		},
	}
	cmd.Flags().StringVar(&libraryRoot, "library-root", "", "the Agent Notes library folder notes are saved into and read from (required)")
	cmd.Flags().StringVar(&helperPath, "helper", notes.DefaultHelperPath, "the agent-notes helper the service runs")
	cmd.Flags().StringVar(&binary, "binary", "", "tincan binary the service runs (default: this executable)")
	return cmd
}

func notesDoctorCmd() *cobra.Command {
	var configPath, helperPath, appSupport, spoolDir string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the notes agent's setup and the service's last helper result",
		Long: "Checks the agent-notes helper, the spool directory, the relay, the kind the relay stores for this\n" +
			"agent (it must be notes, or requests wait only 24 hours), and the running service's last helper\n" +
			"result. It reads that result from the service's health file rather than running the helper itself,\n" +
			"since the service may lack folder access that Terminal has. Exit status is 1 when a check fails.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if configPath == "" {
				configPath = serviceConfigPath(notes.DefaultAgentName)
			}
			exe, err := os.Executable()
			if err == nil {
				exe, _ = filepath.EvalSymlinks(exe)
			}
			appSupport, spoolDir = notesPaths(appSupport, spoolDir)
			rep := runNotesDoctor(cmd.Context(), notesDoctorOptions{
				exe:        exe,
				config:     expandHome(configPath),
				helper:     expandHome(helperPath),
				appSupport: appSupport,
				spoolDir:   spoolDir,
			})
			if asJSON {
				if err := printJSON(cmd, rep); err != nil {
					return err
				}
			} else {
				printDoctor(cmd.OutOrStdout(), rep)
			}
			if !rep.OK {
				return errNotesDoctorFailed
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "the notes agent's client config (default: $TINCAN_CONFIG, else ~/.config/tincan/notes.json)")
	cmd.Flags().StringVar(&helperPath, "helper", notes.DefaultHelperPath, "the agent-notes helper the service runs")
	cmd.Flags().StringVar(&appSupport, "app-support", "", "the service's Application Support root (default: ~/Library/Application Support/tincan-notes)")
	cmd.Flags().StringVar(&spoolDir, "spool-dir", "", "the service's spool directory (default: <app-support>/spool)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the report as JSON")
	return cmd
}

var errNotesDoctorFailed = errors.New("tincan notes doctor found problems (see above)")

type notesDoctorOptions struct {
	exe, config, helper, appSupport, spoolDir string
}

func runNotesDoctor(ctx context.Context, o notesDoctorOptions) doctorReport {
	rep := doctorReport{Version: Version, Binary: o.exe}
	add := func(c check) { rep.Checks = append(rep.Checks, c) }

	add(helperCheck(o.helper))
	add(spoolCheck(o.spoolDir))
	for _, c := range notesRelayChecks(ctx, o.config) {
		add(c)
	}
	add(healthCheck(notes.HealthPathIn(o.appSupport), o.exe))

	rep.OK = true
	for _, c := range rep.Checks {
		if c.Status == "fail" {
			rep.OK = false
		}
	}
	return rep
}

func helperCheck(path string) check {
	err := notesHelperUsable(path)
	switch {
	case err == nil:
		return check{"helper", "ok", path, ""}
	case errors.Is(err, fs.ErrNotExist):
		return check{"helper", "fail", "no agent-notes helper at " + path,
			"Install Agent Notes (the helper ships inside it at " + notes.DefaultHelperPath + "), or pass --helper with the helper's path to tincan notes install and doctor."}
	default:
		return check{"helper", "fail", err.Error(), "Reinstall Agent Notes, or pass --helper with the helper's path."}
	}
}

// notesHelperUsable reports whether path is an executable regular file.
func notesHelperUsable(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a file", path)
	}
	if st.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s is not executable", path)
	}
	return nil
}

func spoolCheck(dir string) check {
	fix := "Make " + dir + " writable by this user, or pass --spool-dir to tincan notes serve and doctor."
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return check{"spool", "fail", "cannot create " + dir + ": " + err.Error(), fix}
	}
	f, err := os.CreateTemp(dir, ".doctor-")
	if err != nil {
		return check{"spool", "fail", dir + " is not writable: " + err.Error(), fix}
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
	return check{"spool", "ok", dir + " is writable", ""}
}

// notesRelayChecks checks the notes agent's join and the kind the relay
// stores for it. Without kind notes the relay keeps requests to it for 24
// hours, not 30 days, so an add sent while the Mac sleeps can be lost.
func notesRelayChecks(ctx context.Context, configPath string) []check {
	joinFix := "Join the notes agent with its own config: TINCAN_CONFIG=~/.config/tincan/notes.json tincan join <code> --relay <relay url>, or pass --config."
	cfg, err := client.LoadConfigFrom(configPath)
	if err != nil {
		return []check{{"relay", "fail", fmt.Sprintf("cannot read %s: %v", configPath, err), joinFix}}
	}
	if cfg.Relay == "" {
		return []check{{"relay", "fail", "no relay configured in " + configPath, joinFix}}
	}
	r, err := client.NewRelayForFile(cfg, configPath)
	if err != nil {
		return []check{{"relay", "fail", err.Error(), "Check the relay URL and proxy in " + configPath + "."}}
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	me, err := r.WhoAmI(cctx)
	switch {
	case client.IsNotJoined(err):
		return []check{{"relay", "fail", "the relay does not know this machine as the notes agent: " + err.Error(), "Run TINCAN_CONFIG=" + configPath + " tincan rejoin; a machine that was never joined needs an invite."}}
	case err != nil:
		return []check{{"relay", "fail", "cannot reach the relay at " + cfg.Relay + ": " + err.Error(), "Check that this Mac is on the tailnet (tailscale status) and the relay is running."}}
	case me.Name != notes.DefaultAgentName:
		return []check{{"relay", "fail", fmt.Sprintf("%s is joined as %q, not \"notes\"", configPath, me.Name), joinFix}}
	}
	out := []check{{"relay", "ok", fmt.Sprintf("reachable at %s; this machine is agent %q", r.Base(), me.Name), ""}}
	kindFix := "On an admin device run: tincan kind notes notes. Until then the relay drops requests to notes after 24 hours instead of 30 days."
	switch me.Kind {
	case notes.DefaultAgentName:
		out = append(out, check{"relay kind", "ok", `the relay stores kind "notes", so requests to notes wait up to 30 days`, ""})
	case "":
		out = append(out, check{"relay kind", "fail", "the relay stores no kind for notes (joined through an invite without --kind notes), so requests to it wait only 24 hours", kindFix})
	default:
		out = append(out, check{"relay kind", "fail", fmt.Sprintf("the relay stores kind %q for notes, not \"notes\", so requests to it wait only 24 hours", me.Kind), kindFix})
	}
	return out
}

// healthCheck reports the running service's last helper result.
func healthCheck(path, exe string) check {
	const name = "last helper result"
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return check{name, "warn", "no helper result yet at " + path + " (the service has not run, or has handled no request)",
			"Start the service (tincan notes install prints the command) and send notes a request, then run tincan notes doctor again."}
	}
	if err != nil {
		return check{name, "warn", "cannot read " + path + ": " + err.Error(), ""}
	}
	var h notes.Health
	if err := json.Unmarshal(b, &h); err != nil {
		return check{name, "warn", path + " is not a health file: " + err.Error(), ""}
	}
	when := h.UpdatedAt.Local().Format(time.DateTime)
	// Whatever ran last, a helper that ignores idempotency keys stays a
	// failure until a create shows it honors them: a successful search
	// must not hide that adds can be duplicated.
	if h.IdempotencyUnsupported {
		return check{name, "fail", fmt.Sprintf("%s: the helper ignored an add's idempotency key, so a redelivered add could be saved twice (last helper call: %s at %s)", "helper_too_old", h.Command, when),
			helperFix("helper_too_old", exe)}
	}
	if h.OK && !h.IdempotencyVerified {
		// Only a create shows whether the helper honors idempotency keys:
		// a successful search or read says nothing about duplicate adds.
		return check{name, "warn", fmt.Sprintf("duplicate protection not yet verified: no add has run against this helper (%s succeeded at %s)", h.Command, when),
			"Send notes one add; doctor reports ok once the helper has shown it honors idempotency keys."}
	}
	if h.OK {
		return check{name, "ok", fmt.Sprintf("%s succeeded at %s", h.Command, when), ""}
	}
	detail := fmt.Sprintf("%s failed at %s", h.Command, when)
	if h.RequestID != "" {
		detail += " (request " + h.RequestID + ")"
	}
	if h.Code != "" {
		detail += ": " + h.Code
	}
	if h.Message != "" {
		detail += ": " + h.Message
	}
	return check{name, "fail", detail, helperFix(h.Code, exe)}
}

// helperFix is what to do about a failed helper result with code.
func helperFix(code, exe string) string {
	switch code {
	case "missing_authorization", "operation_failed":
		who := "the tincan binary"
		if exe != "" {
			who += " (" + exe + ")"
		}
		// Only the library folder: Full Disk Access would also extend to the
		// codex extractor the service runs as a child process.
		return "The service cannot open the library. In System Settings > Privacy & Security, grant " + who +
			" Files and Folders access to the library's folder, then restart the service: " +
			"launchctl kickstart -k gui/$(id -u)/" + notes.ServiceLabel
	case "helper_too_old":
		return "Update Agent Notes: this helper cannot save notes safely for the notes agent. Install the latest Agent Notes, then restart the service."
	case "helper_unavailable":
		return "Check that Agent Notes is installed and that --helper names its agent-notes helper."
	}
	return "Check the service log (~/Library/Logs/tincan-notes.log)."
}
