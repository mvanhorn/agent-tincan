package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/notes"
)

func notesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "notes",
		Short: "Run the notes agent, which saves and finds notes in Agent Notes, Bear or Apple Notes for teammates",
		Long: "The notes agent runs on the Mac with your notes app (Agent Notes, Bear or Apple Notes, chosen with\n" +
			"--backend): teammates ask it to save a note, search notes, or read one back. Set it up once with\n" +
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

const (
	defaultReadAllowlist = "~/.config/tincan/notes-allow.txt"
	defaultAddAllowlist  = "~/.config/tincan/notes-add-allow.txt"
	backendFlagHelp      = "the notes app: agent-notes, bear (macOS) or apple-notes (macOS)"
)

// notesLibraryRoot is the Agent Notes library: the flag, else the one
// Agent Notes has open. stamp starts the line that names a found library
// (serviceStamp() in the service's log, "" for a command run by hand).
func notesLibraryRoot(cmd *cobra.Command, libraryRoot, stamp string) (string, error) {
	if libraryRoot != "" {
		return libraryRoot, nil
	}
	if libraryRoot = notes.DiscoverLibraryRoot(); libraryRoot == "" {
		return "", errors.New("no Agent Notes library found: open Agent Notes once, or pass --library-root with the library folder the notes agent saves into")
	}
	cmd.PrintErrf("%stincan notes: using the Agent Notes library at %s (pass --library-root to choose another)\n", stamp, libraryRoot)
	return libraryRoot, nil
}

// warnOpenAllowlists warns when an allowlist file is missing. Bear and
// Apple Notes hold everything the owner ever wrote, not one library made
// for agents, so letting every joined agent in deserves a word. stamp
// starts each warning, as for notesLibraryRoot.
func warnOpenAllowlists(cmd *cobra.Command, readAllow, addAllow, stamp string) {
	for _, f := range []struct{ path, who string }{{readAllow, "search and read"}, {addAllow, "add"}} {
		if _, err := os.Stat(expandHome(f.path)); errors.Is(err, fs.ErrNotExist) {
			cmd.PrintErrf("%stincan notes: warning: %s does not exist, so every joined agent may %s your notes; list the agents allowed, one per line\n", stamp, f.path, f.who)
		}
	}
}

func notesServeCmd() *cobra.Command {
	var configPath, backendName, libraryRoot, helperPath, appSupport, spoolDir, readAllow, addAllow, scratchDir string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the notes agent: save, search and read notes in the chosen notes app for teammates over the relay",
		Long: "Long-polls the relay as the notes agent and handles each request in the --backend notes app: the\n" +
			"agent-notes helper run against --library-root (the default), Bear, or Apple Notes. An add that cannot be\n" +
			"applied yet is kept in the spool and retried, so it is never lost. Free-text requests go through a\n" +
			"tool-less codex exec call that turns the request text into a structured one.\n" +
			"The allowlist files (one agent per line) limit who may search and read, and who may add; a missing\n" +
			"file lets every joined agent. The last result and an access check are written for tincan notes doctor.\n" +
			"Normally started by the service definition tincan notes install writes. See docs/adapters/notes.md.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			defer useServiceLog()()
			if err := notes.CheckBackendName(backendName); err != nil {
				return err
			}
			if backendName == notes.BackendAgentNotes {
				var err error
				if libraryRoot, err = notesLibraryRoot(cmd, libraryRoot, serviceStamp()); err != nil {
					return err
				}
			} else if runtime.GOOS != "darwin" {
				return fmt.Errorf("the %s backend runs only on macOS", backendName)
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
			logf := func(format string, args ...any) {
				cmd.PrintErr(client.LogLines(time.Now(), fmt.Sprintf("tincan notes: "+format, args...)))
			}
			backend := notes.NewBackend(backendName, appSupport, logf)
			svc, err := notes.New(notes.Config{
				Relay:             r,
				Backend:           backend,
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
				cmd.PrintErrf("%stincan notes: warning: the relay stores kind %q for this agent, not \"notes\", so adds wait only 24 hours for this Mac; fix it with: tincan kind notes notes\n", serviceStamp(), me.Kind)
			}
			if backend == nil {
				cmd.PrintErrf("%stincan notes: serving as %s on %s, library %s\n", serviceStamp(), me.Name, cfg.Relay, libraryRoot)
			} else {
				cmd.PrintErrf("%stincan notes: serving as %s on %s, notes in %s\n", serviceStamp(), me.Name, cfg.Relay, backend.Name())
				warnOpenAllowlists(cmd, readAllow, addAllow, serviceStamp())
			}
			err = svc.Run(ctx)
			if ctx.Err() != nil {
				cmd.PrintErrf("%stincan notes: stopped\n", serviceStamp())
				return nil
			}
			return err
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "the notes agent's client config (default: $TINCAN_CONFIG, else ~/.config/tincan/notes.json)")
	cmd.Flags().StringVar(&backendName, "backend", notes.BackendAgentNotes, backendFlagHelp)
	cmd.Flags().StringVar(&libraryRoot, "library-root", "", "agent-notes only: the Agent Notes library folder notes are saved into and read from (default: the library Agent Notes has open)")
	cmd.Flags().StringVar(&helperPath, "helper", notes.DefaultHelperPath, "agent-notes only: the agent-notes helper")
	cmd.Flags().StringVar(&appSupport, "app-support", "", "the service's own Application Support root (default: ~/Library/Application Support/tincan-notes)")
	cmd.Flags().StringVar(&spoolDir, "spool-dir", "", "where adds not yet applied are kept (default: <app-support>/spool)")
	cmd.Flags().StringVar(&readAllow, "read-allowlist", defaultReadAllowlist, "file of agents allowed to search and read notes, one per line (missing means every joined agent)")
	cmd.Flags().StringVar(&addAllow, "add-allowlist", defaultAddAllowlist, "file of agents allowed to add notes, one per line (missing means every joined agent)")
	cmd.Flags().StringVar(&scratchDir, "codex-scratch", "", "empty working directory for the tool-less codex step (default: <app-support>/codex-scratch)")
	return cmd
}

func notesInstallCmd() *cobra.Command {
	var backendName, libraryRoot, helperPath, binary string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Write the notes service definition (launchd on macOS, systemd on Linux); does not start it",
		Long: "Writes a launchd agent (" + notes.ServiceLabel + ") on macOS, or a systemd user unit on Linux, that runs\n" +
			"tincan notes serve with --backend (and --library-root for agent-notes) with TINCAN_CONFIG=~/.config/tincan/notes.json and keeps it\n" +
			"running. It prints the command that starts the service and never starts it itself.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := notes.CheckBackendName(backendName); err != nil {
				return err
			}
			opts := notes.ServiceOptions{Binary: binary, Backend: backendName}
			if backendName == notes.BackendAgentNotes {
				root, err := notesLibraryRoot(cmd, libraryRoot, "")
				if err != nil {
					return err
				}
				if opts.LibraryRoot, err = filepath.Abs(expandHome(root)); err != nil {
					return err
				}
				opts.HelperPath = expandHome(helperPath)
			}
			res, err := notes.InstallService(opts)
			if err != nil {
				return err
			}
			cmd.Printf("notes service definition: %s (not started)\n", res.Path)
			home, _ := os.UserHomeDir()
			cfgPath := filepath.Join(home, ".config", "tincan", "notes.json")
			if cfg, err := client.LoadConfigFrom(cfgPath); err != nil || cfg.Relay == "" {
				cmd.Printf("the notes agent is not joined yet; first run:\n  TINCAN_CONFIG=%s tincan join <code> --relay <relay url>\n", cfgPath)
			}
			if backendName != notes.BackendAgentNotes {
				warnOpenAllowlists(cmd, defaultReadAllowlist, defaultAddAllowlist, "")
			}
			cmd.Printf("start it with:\n  %s\n", res.Next)
			cmd.Println("then check it with: tincan notes doctor")
			return nil
		},
	}
	cmd.Flags().StringVar(&backendName, "backend", notes.BackendAgentNotes, backendFlagHelp)
	cmd.Flags().StringVar(&libraryRoot, "library-root", "", "agent-notes only: the Agent Notes library folder notes are saved into and read from (default: the library Agent Notes has open)")
	cmd.Flags().StringVar(&helperPath, "helper", notes.DefaultHelperPath, "agent-notes only: the agent-notes helper the service runs")
	cmd.Flags().StringVar(&binary, "binary", "", "tincan binary the service runs (default: this executable)")
	return cmd
}

func notesDoctorCmd() *cobra.Command {
	var configPath, backendName, helperPath, appSupport, spoolDir string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the notes agent's setup and the service's last helper result",
		Long: "Checks the agent-notes helper (or, with --backend bear or apple-notes, the service's own access check\n" +
			"and the allowlists), the spool directory, the relay, the kind the relay stores for this agent (it must\n" +
			"be notes, or requests wait only 24 hours), and the running service's last result. It reads those from\n" +
			"the service's health file, since the service may lack access that Terminal has. Exit status is 1 when\n" +
			"a check fails.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := notes.CheckBackendName(backendName); err != nil {
				return err
			}
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
				backend:    backendName,
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
	cmd.Flags().StringVar(&backendName, "backend", notes.BackendAgentNotes, backendFlagHelp)
	cmd.Flags().StringVar(&helperPath, "helper", notes.DefaultHelperPath, "agent-notes only: the agent-notes helper the service runs")
	cmd.Flags().StringVar(&appSupport, "app-support", "", "the service's Application Support root (default: ~/Library/Application Support/tincan-notes)")
	cmd.Flags().StringVar(&spoolDir, "spool-dir", "", "the service's spool directory (default: <app-support>/spool)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the report as JSON")
	return cmd
}

var errNotesDoctorFailed = errors.New("tincan notes doctor found problems (see above)")

type notesDoctorOptions struct {
	exe, backend, config, helper, appSupport, spoolDir string
	// probe checks the backend from this process; nil uses the real app.
	probe func(context.Context) (int, error)
}

func runNotesDoctor(ctx context.Context, o notesDoctorOptions) doctorReport {
	rep := doctorReport{Version: Version, Binary: o.exe}
	add := func(c check) { rep.Checks = append(rep.Checks, c) }

	if o.backend == "" || o.backend == notes.BackendAgentNotes {
		add(helperCheck(o.helper))
	} else {
		add(serviceAccessCheck(notes.HealthPathIn(o.appSupport), o.backend, o.exe))
		add(terminalAccessCheck(ctx, o))
		for _, f := range []string{defaultReadAllowlist, defaultAddAllowlist} {
			if _, err := os.Stat(expandHome(f)); errors.Is(err, fs.ErrNotExist) {
				add(check{"allowlist", "warn", f + " does not exist, so every joined agent may use your notes",
					"List the agents allowed, one per line, in " + f + "."})
			}
		}
	}
	add(spoolCheck(o.spoolDir))
	for _, c := range notesRelayChecks(ctx, o.config) {
		add(c)
	}
	add(healthCheck(notes.HealthPathIn(o.appSupport), o.exe))
	add(pendingRepliesCheck(notes.HealthPathIn(o.appSupport), time.Now()))

	rep.OK = true
	for _, c := range rep.Checks {
		if c.Status == "fail" {
			rep.OK = false
		}
	}
	return rep
}

// serviceAccessCheck reports the access check the running service last
// recorded. macOS grants access to the service's process, not to this
// terminal, so this is the check that counts.
func serviceAccessCheck(healthPath, backend, exe string) check {
	const name = "service access"
	var h notes.Health
	b, err := os.ReadFile(healthPath)
	if err == nil {
		err = json.Unmarshal(b, &h)
	}
	start := "Start the service (tincan notes install --backend " + backend + " prints the command), then run tincan notes doctor --backend " + backend + " again."
	if err != nil || h.Probe == nil {
		return check{name, "warn", "the service has not recorded an access check yet", start}
	}
	if want := notes.BackendDisplayName(backend); h.Backend != want {
		return check{name, "warn", fmt.Sprintf("the service's last access check was for %s, not %s", cmp.Or(h.Backend, "another app"), want), start}
	}
	when := h.Probe.At.Local().Format(time.DateTime)
	if h.Probe.Code == "" && h.Probe.Message == "" {
		detail := fmt.Sprintf("%s: the service saw %d notes at %s", h.Backend, h.Probe.Count, when)
		if h.Probe.Note != "" {
			detail += "; " + h.Probe.Note
		}
		return check{name, "ok", detail, ""}
	}
	detail := fmt.Sprintf("%s: the service could not reach its notes at %s", h.Backend, when)
	if h.Probe.Code != "" {
		detail += ": " + h.Probe.Code
	}
	if h.Probe.Message != "" {
		detail += ": " + h.Probe.Message
	}
	return check{name, "fail", detail, backendFix(backend, h.Probe.Code, exe)}
}

// terminalAccessCheck checks the backend from this process. It can differ
// from the service's result, so it only warns.
func terminalAccessCheck(ctx context.Context, o notesDoctorOptions) check {
	const name = "access from this terminal"
	probe := o.probe
	if probe == nil {
		probe = notes.NewBackend(o.backend, o.appSupport, func(string, ...any) {}).Probe
	}
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	n, err := probe(cctx)
	if err != nil {
		return check{name, "warn", err.Error(), "This is Terminal's access, not the service's; the service access line above is what the notes agent sees."}
	}
	return check{name, "ok", fmt.Sprintf("%d notes", n), ""}
}

// backendFix is what to do about a failed Bear or Apple Notes check.
func backendFix(backend, code, exe string) string {
	who := "the tincan binary"
	if exe != "" {
		who += " (" + exe + ")"
	}
	restart := ", then restart the service: launchctl kickstart -k gui/$(id -u)/" + notes.ServiceLabel
	switch {
	case backend == notes.BackendAppleNotes && code == notes.CodePermissionDenied:
		return "In System Settings > Privacy & Security > Automation, allow " + who + " to control Notes" + restart
	case backend == notes.BackendBear && code == notes.CodePermissionDenied:
		return "In System Settings > Privacy & Security, grant " + who + " Full Disk Access or access to Bear's data" + restart
	case backend == notes.BackendBear:
		return "Install Bear and open it once, so its database exists."
	}
	return "Open Notes once and check the service log (~/Library/Logs/tincan-notes.log)."
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
	if h.Command == "" {
		// Only the undelivered-replies list has been written so far.
		return check{name, "warn", "no helper result yet in " + path, "Send notes a request, then run tincan notes doctor again."}
	}
	when := h.UpdatedAt.Local().Format(time.DateTime)
	// Whatever ran last, a helper that ignores idempotency keys stays a
	// failure until a create shows it honors them: a successful search
	// must not hide that adds can be duplicated.
	if h.IdempotencyUnsupported {
		return check{name, "fail", fmt.Sprintf("%s: the helper ignored an add's idempotency key, so a redelivered add could be saved twice (last helper call: %s at %s)", "helper_too_old", h.Command, when),
			helperFix("helper_too_old", exe)}
	}
	ledgered := h.Backend != "" && h.Backend != notes.AgentNotesName
	if h.OK && !h.IdempotencyVerified && !ledgered {
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

// pendingRepliesCheck reports replies the service computed but has not
// delivered, from the health file: request ids, ages, attempts and error
// classes, never reply text. A refused credential fails: retrying will
// not fix it.
func pendingRepliesCheck(path string, now time.Time) check {
	const name = "undelivered replies"
	b, err := os.ReadFile(path)
	if err != nil {
		return check{name, "ok", "none recorded", ""}
	}
	var h notes.Health
	if json.Unmarshal(b, &h) != nil || len(h.Pending) == 0 {
		return check{name, "ok", "none", ""}
	}
	const shown = 5
	var parts []string
	refused := false
	for i, p := range h.Pending {
		if p.Refused() {
			refused = true
		}
		if i >= shown {
			continue
		}
		d := fmt.Sprintf("%s (%s, stored %s ago", p.RequestID, p.Op, now.Sub(p.StoredAt).Round(time.Second))
		if p.Attempts > 0 {
			d += fmt.Sprintf(", %d failed attempts, last error %s", p.Attempts, p.LastError)
		}
		parts = append(parts, d+")")
	}
	detail := fmt.Sprintf("%d waiting: %s", len(h.Pending), strings.Join(parts, "; "))
	if len(h.Pending) > shown {
		detail += fmt.Sprintf("; and %d more", len(h.Pending)-shown)
	}
	if refused {
		return check{name, "fail", detail, "The relay refuses this agent's replies. Check the notes agent's join (tincan notes doctor --config) and rejoin it if it was removed; stored replies are kept for 24 hours."}
	}
	return check{name, "warn", detail, "The service keeps retrying. Check that the relay is reachable from this Mac; each stored reply is kept for at most 24 hours."}
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
