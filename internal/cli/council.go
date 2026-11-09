package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/council"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/history"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
)

// councilCmd is the Council service's command group: serve, install, and
// doctor run the service; the owner's commands sit beside them.
func councilCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   `council ["question" | command]`,
		Short: "Run Council, which puts one question to every model on the team and returns a verdict",
		Long: "Put a question to every model on the team: tincan council \"question\" sends it to Council as this\n" +
			"agent, shows each stage as Council reports it, prints the verdict and ranking, and saves the report\n" +
			"and scorecard to a local folder. Council asks are held for the owner's approval; run in a terminal on\n" +
			"an admin device, tincan council approves its own request, and anywhere else it waits for approval\n" +
			"like any ask.\n\n" +
			"Council is a teammate that runs on the owner's machine: any teammate asks it a question, every\n" +
			"model on the team answers, the members rank each other's answers blind, and a chairman writes the\n" +
			"verdict. Set it up once with tincan council install, then check it with tincan council doctor.\n" +
			"See docs/adapters/council.md.",
		Args: cobra.ArbitraryArgs,
	}
	cmd.AddCommand(councilServeCmd(), councilInstallCmd(), councilDoctorCmd(), councilLeaderboardCmd())
	var to, chairman string
	var members, attach []string
	var asJSON bool
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return cmd.Help()
		}
		question := strings.TrimSpace(strings.Join(args, " "))
		if question == "" {
			return errors.New("the question is empty")
		}
		form := struct {
			Question string   `json:"question"`
			Members  []string `json:"members,omitempty"`
			Chairman string   `json:"chairman,omitempty"`
		}{question, members, chairman}
		return convene(cmd, "Convening", to, form, attach, asJSON, true)
	}
	cmd.Flags().StringVar(&to, "to", "", "the Council agent to ask (default: the roster's council-kind agent, else council)")
	cmd.Flags().StringArrayVar(&attach, "attach", nil, "a local file every member receives with the question (repeatable)")
	cmd.Flags().StringSliceVar(&members, "members", nil, "the members to ask instead of the default roster (comma-separated or repeatable)")
	cmd.Flags().StringVar(&chairman, "chairman", "", "the member to try first as chairman")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the council-result block and exit 0 completed, 1 failed or declined, 2 still held")
	return cmd
}

func councilLeaderboardCmd() *cobra.Command {
	var to, category string
	var card bool
	cmd := &cobra.Command{
		Use:   "leaderboard",
		Short: "Show Council's leaderboard, overall or for one category, and optionally save its card",
		Long: "Asks Council for its leaderboard: members ranked by wins, then mean score from blind peer review.\n" +
			"--card saves the leaderboard card (a PNG sized for posting) to a local folder and prints its path.\n" +
			"Like any council ask it is held for the owner's approval unless run in a terminal on an admin device.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			form := struct {
				Op       string `json:"op"`
				Category string `json:"category,omitempty"`
			}{string(council.OpLeaderboard), category}
			return convene(cmd, "Asking", to, form, nil, false, card)
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "the Council agent to ask (default: the roster's council-kind agent, else council)")
	cmd.Flags().StringVar(&category, "category", "", "one category's leaderboard (default: overall)")
	cmd.Flags().BoolVar(&card, "card", false, "save the leaderboard card as a PNG and print its path")
	return cmd
}

// councilPollEvery is how often tincan council checks its request for a
// new progress note or the reply.
var councilPollEvery = 2 * time.Second

// councilInteractive reports whether the owner is at a terminal: stdin
// and stdout are both terminals. A script or an agent's shell is not, so
// its council waits for approval like any other ask.
var councilInteractive = func() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// convene sends form to Council as this agent, approves the request when
// the relay holds it and the owner is at a terminal on an admin device,
// shows progress notes as stage lines until the reply comes, and prints
// it. With saveFiles it saves the reply's attachments (a council's report
// and scorecard, the leaderboard's card) and prints their paths.
func convene(cmd *cobra.Command, verb, to string, form any, attach []string, asJSON, saveFiles bool) error {
	r, cfg, err := connect()
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	kindAgent := councilKindAgent(ctx, r)
	if to == "" {
		to = kindAgent
		if to == "" {
			to = council.DefaultAgentName
		}
	}
	raw, err := json.Marshal(form)
	if err != nil {
		return err
	}
	me := cfg.Agent
	if who, err := r.WhoAmI(ctx); err == nil {
		me = who.Name
	}
	ups, err := r.UploadFiles(ctx, attach)
	if err != nil {
		return err
	}
	req, err := r.SendAttached(ctx, to, council.RequestPrefix+" "+string(raw), envelope.KindAsk, "", client.AttachmentIDs(ups), false)
	if err != nil {
		return err
	}
	// With --json, stdout carries only the result; the rest goes to stderr.
	info := cmd.OutOrStdout()
	if asJSON {
		info = cmd.ErrOrStderr()
	}
	fmt.Fprintf(info, "%s %s as %s (request %s).\n", verb, to, me, req.ID)

	if req.Status == envelope.StatusHeld {
		if why := selfApprove(ctx, req.ID); why != "" {
			fmt.Fprintf(info, "Request %s: held, waiting for the owner's approval (%s). Approve it on an admin device with: tincan approve %s. Then check on it with: tincan get %s\n",
				req.ID, why, req.ID, req.ID)
			if asJSON {
				return printResultJSON(cmd.OutOrStdout(), client.Result{Request: req, Status: req.Status})
			}
			return nil
		}
		fmt.Fprintf(info, "Approved request %s on this admin device.\n", req.ID)
	}

	res, err := waitCouncil(ctx, r, req.ID, info)
	if err != nil {
		return err
	}
	var saved []string
	if res.Reply != nil && saveFiles {
		// Only the council-kind agent's report is trusted enough to save
		// as HTML a browser opens; anyone else's stays .bin.
		fromCouncil := kindAgent != "" && to == kindAgent && (res.Reply.From == "" || res.Reply.From == kindAgent)
		saved = saveCouncilFiles(ctx, r, cfg, req.ID, res.Reply.Attachments, fromCouncil, cmd.ErrOrStderr())
	}
	if asJSON {
		block, ok := councilResultBlock(res)
		if !ok {
			return printResultJSON(cmd.OutOrStdout(), res)
		}
		for _, p := range saved {
			fmt.Fprintf(info, "Saved %s\n", p)
		}
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), block); err != nil {
			return err
		}
		if res.Status != envelope.StatusAnswered {
			return &ExitError{Code: 1, Silent: true}
		}
		return nil
	}
	if res.Reply != nil {
		cmd.Print(strings.TrimRight(res.Reply.Body, "\n") + "\n")
	} else {
		cmd.Print(client.FormatResult(res))
	}
	for _, p := range saved {
		cmd.Printf("Saved %s\n", p)
	}
	return nil
}

// councilKindAgent is the roster's council-kind agent, or "" when the
// roster has none or cannot be read.
func councilKindAgent(ctx context.Context, r *client.Relay) string {
	ro, err := r.Roster(ctx)
	if err != nil {
		return ""
	}
	for _, a := range ro.Agents {
		if a.Kind == onboard.KindCouncil {
			return a.Name
		}
	}
	return ""
}

// selfApprove approves held request id through the same path as tincan
// approve, but only when the owner is at a terminal. It returns
// why the request is still held, or "" once approved.
func selfApprove(ctx context.Context, id string) string {
	if !councilInteractive() {
		return "not run from a terminal, so it waits like any agent's ask"
	}
	r, err := adminRelay("", "")
	if err == nil {
		err = r.Raw(ctx, "POST", "/v1/admin/requests/"+url.PathEscape(id)+"/approve", map[string]string{"reason": ""}, nil)
	}
	switch {
	case err == nil:
		return ""
	case client.IsStatus(err, http.StatusForbidden):
		return "this is not an admin device"
	default:
		return "could not approve it here: " + err.Error()
	}
}

// councilGetTries is how many checks in a row may fail (other than with
// a 4xx from the relay) before tincan council stops waiting.
const councilGetTries = 5

// waitCouncil checks request id every councilPollEvery until it is done,
// printing each new progress note to w as a stage line. A failed check is
// retried, up to councilGetTries in a row; a 4xx from the relay ends the
// wait at once.
func waitCouncil(ctx context.Context, r *client.Relay, id string, w io.Writer) (client.Result, error) {
	var last envelope.Progress
	failed := 0
	for {
		res, err := r.Get(ctx, id, 0)
		if err != nil {
			var apiErr *client.APIError
			if ctx.Err() != nil || (errors.As(err, &apiErr) && apiErr.Code >= 400 && apiErr.Code < 500) {
				return res, err
			}
			if failed++; failed >= councilGetTries {
				return res, fmt.Errorf("stopped waiting for request %s after %d failed checks: %w; check on it later with: tincan get %s", id, failed, err, id)
			}
			select {
			case <-ctx.Done():
				return res, ctx.Err()
			case <-time.After(councilPollEvery):
			}
			continue
		}
		failed = 0
		if p := res.Progress; p != nil && (p.Note != last.Note || !p.At.Equal(last.At)) {
			last = *p
			fmt.Fprintf(w, "Council: %s\n", p.Note)
		}
		if res.Done() {
			return res, nil
		}
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(councilPollEvery):
		}
	}
}

// saveCouncilFiles downloads a reply's attachments into this agent's
// attachments folder under council/<request id> (0700, files 0600), named
// by attachment id, and returns their paths. An HTML file is saved as
// .html only when fromCouncil (the reply came from the roster's
// council-kind agent); otherwise it keeps the allowlist's .bin. A file
// that cannot be saved is reported on errOut and skipped.
func saveCouncilFiles(ctx context.Context, r *client.Relay, cfg client.Config, id string, atts []envelope.Attachment, fromCouncil bool, errOut io.Writer) []string {
	if len(atts) == 0 {
		return nil
	}
	dir := filepath.Join(client.AttachmentDir(cfg), "council", filepath.Base(id))
	var paths []string
	for _, a := range atts {
		data, d, err := r.FetchAttachment(ctx, a.ID)
		if err == nil {
			var p string
			if p, err = client.SaveAttachmentFile(dir, a.ID, d.MIME, data); err == nil {
				// The report is HTML, which the attachment allowlist saves
				// as .bin; give it an extension a browser opens.
				if fromCouncil && client.MediaType(d.MIME) == "text/html" {
					html := strings.TrimSuffix(p, ".bin") + ".html"
					if err = os.Rename(p, html); err == nil {
						p = html
					}
				}
				paths = append(paths, p)
			}
		}
		if err != nil {
			fmt.Fprintf(errOut, "tincan council: could not save %s (%s): %v\n", a.Name, a.ID, err)
		}
	}
	return paths
}

// councilResultBlock is the JSON in the reply's last fenced
// council-result block.
func councilResultBlock(res client.Result) (string, bool) {
	if res.Reply == nil {
		return "", false
	}
	const open = council.ResultFence + "\n"
	body := res.Reply.Body
	_, rest, found := strings.CutLast(body, open)
	if !found {
		return "", false
	}
	block, _, ok := strings.Cut(rest, "\n```")
	if !ok {
		return "", false
	}
	block = strings.TrimSpace(block)
	if !json.Valid([]byte(block)) {
		return "", false
	}
	return block, true
}

// councilDir resolves the --dir flag: the service's own folder, where
// council.json and, by default, its database and reports live.
func councilDir(dir string) string {
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = council.DefaultDir(runtime.GOOS, home)
	}
	return expandHome(dir)
}

const councilDirFlagHelp = "Council's own folder, holding council.json, its database, and reports (default: ~/Library/Application Support/tincan-council)"

func councilServeCmd() *cobra.Command {
	var configPath, dir string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run Council: claim convening requests over the relay and run councils one at a time",
		Long: "Long-polls the relay as the council agent. Each convening request is claimed at once and queued;\n" +
			"councils run one at a time, with a progress note at each stage. Council settings are read from\n" +
			"council.json in --dir. It refuses to run unless the relay stores kind council for this agent, since\n" +
			"only then does the relay hold agents' councils for the owner's approval.\n" +
			"Normally started by the service definition tincan council install writes.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if configPath == "" {
				configPath = serviceConfigPath(council.DefaultAgentName)
			}
			configPath = expandHome(configPath)
			cfg, err := client.LoadConfigFrom(configPath)
			if err != nil {
				return fmt.Errorf("council config %s: %w", configPath, err)
			}
			if cfg.Relay == "" {
				return fmt.Errorf("no relay configured in %s: run TINCAN_CONFIG=%s tincan join <code> --relay http://tincan-relay", configPath, configPath)
			}
			if cfg.Agent != "" && cfg.Agent != council.DefaultAgentName {
				return wrongServeAgent("council serve", council.DefaultAgentName, configPath+" is joined as", cfg.Agent)
			}
			dir = councilDir(dir)
			settings, err := council.LoadConfig(council.ConfigPathIn(dir))
			if err != nil {
				return err
			}
			data, reports := council.Folders(dir, settings)
			r, err := client.NewRelayForFile(cfg, configPath)
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
				return fmt.Errorf("council serve: could not confirm this agent's identity with the relay: %w", client.RejoinHint(err, cfg.Relay))
			}
			if me.Name != council.DefaultAgentName {
				return wrongServeAgent("council serve", council.DefaultAgentName, "the relay knows the machine using "+configPath+" as", me.Name)
			}
			st, err := council.OpenStore(council.StorePathIn(data))
			if err != nil {
				return err
			}
			defer st.Close()
			svc, err := council.NewService(council.ServiceConfig{Relay: r, Config: settings, Store: st, ReportDir: reports, Log: cmd.ErrOrStderr()})
			if err != nil {
				return err
			}
			cmd.PrintErrf("tincan council: serving as %s on %s, folder %s\n", me.Name, cfg.Relay, data)
			err = svc.Run(ctx)
			if ctx.Err() != nil {
				cmd.PrintErrln("tincan council: stopped")
				return nil
			}
			return err
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Council's client config (default: $TINCAN_CONFIG, else ~/.config/tincan/council.json)")
	cmd.Flags().StringVar(&dir, "dir", "", councilDirFlagHelp)
	return cmd
}

func councilInstallCmd() *cobra.Command {
	var binary string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Write the Council service definition (launchd on macOS, systemd on Linux); does not start it",
		Long: "Writes a launchd agent (" + council.ServiceLabel + ") on macOS, or a systemd user unit on Linux, that runs\n" +
			"tincan council serve with TINCAN_CONFIG=~/.config/tincan/council.json and keeps it running. It prints the\n" +
			"command that starts the service and never starts it itself.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := council.InstallService(history.ServiceOptions{Binary: binary})
			if err != nil {
				return err
			}
			cmd.Printf("council service definition: %s (not started)\n", res.Path)
			home, _ := os.UserHomeDir()
			cfgPath := filepath.Join(home, ".config", "tincan", "council.json")
			if cfg, err := client.LoadConfigFrom(cfgPath); err != nil || cfg.Relay == "" {
				cmd.Printf("Council is not joined yet; first invite it with --kind council, then run:\n  TINCAN_CONFIG=%s tincan join <code> --relay <relay url>\n", cfgPath)
			}
			cmd.Printf("start it with:\n  %s\n", res.Next)
			cmd.Println("then check it with: tincan council doctor")
			return nil
		},
	}
	cmd.Flags().StringVar(&binary, "binary", "", "tincan binary the service runs (default: this executable)")
	return cmd
}

func councilDoctorCmd() *cobra.Command {
	var configPath, dir string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check Council's setup: its relay join and kind, council.json, and its folders",
		Long: "Checks that the relay knows this machine as the council agent with kind council (without it the\n" +
			"relay would not hold agents' councils for the owner, and council serve refuses to run), that\n" +
			"council.json is valid, and that Council's folders are writable. Exit status is 1 when a check fails.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if configPath == "" {
				configPath = serviceConfigPath(council.DefaultAgentName)
			}
			exe, err := os.Executable()
			if err == nil {
				exe, _ = filepath.EvalSymlinks(exe)
			}
			rep := runCouncilDoctor(cmd.Context(), exe, expandHome(configPath), councilDir(dir))
			if asJSON {
				if err := printJSON(cmd, rep); err != nil {
					return err
				}
			} else {
				printDoctor(cmd.OutOrStdout(), rep)
			}
			if !rep.OK {
				return errCouncilDoctorFailed
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Council's client config (default: $TINCAN_CONFIG, else ~/.config/tincan/council.json)")
	cmd.Flags().StringVar(&dir, "dir", "", councilDirFlagHelp)
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the report as JSON")
	return cmd
}

var errCouncilDoctorFailed = errors.New("tincan council doctor found problems (see above)")

func runCouncilDoctor(ctx context.Context, exe, configPath, dir string) doctorReport {
	rep := doctorReport{Version: Version, Binary: exe}
	rep.Checks = append(rep.Checks, councilRelayChecks(ctx, configPath)...)
	settingsPath := council.ConfigPathIn(dir)
	settings, err := council.LoadConfig(settingsPath)
	if err != nil {
		rep.Checks = append(rep.Checks, check{"council.json", "fail", err.Error(), "Fix or remove " + settingsPath + "; without it Council uses its defaults."})
	} else {
		detail := settingsPath + " is valid"
		if _, err := os.Stat(settingsPath); err != nil {
			detail = "no " + settingsPath + ", using the defaults"
		}
		rep.Checks = append(rep.Checks, check{"council.json", "ok", detail, ""})
		data, reports := council.Folders(dir, settings)
		rep.Checks = append(rep.Checks, writableCheck("data folder", data), writableCheck("report folder", reports))
	}
	rep.OK = true
	for _, c := range rep.Checks {
		if c.Status == "fail" {
			rep.OK = false
		}
	}
	return rep
}

// writableCheck checks that dir exists or can be created owner-only, and
// that this user can write in it.
func writableCheck(name, dir string) check {
	fix := "Make " + dir + " writable by this user, or point council.json's dir or report_dir elsewhere."
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return check{name, "fail", "cannot create " + dir + ": " + err.Error(), fix}
	}
	f, err := os.CreateTemp(dir, ".doctor-")
	if err != nil {
		return check{name, "fail", dir + " is not writable: " + err.Error(), fix}
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
	return check{name, "ok", dir + " is writable", ""}
}

// councilRelayChecks checks Council's join and the kind the relay stores
// for it. Without kind council the relay does not hold agents' councils
// for the owner, and council serve refuses to run.
func councilRelayChecks(ctx context.Context, configPath string) []check {
	joinFix := "Join Council with its own config: TINCAN_CONFIG=~/.config/tincan/council.json tincan join <code> --relay <relay url>, or pass --config."
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
		return []check{{"relay", "fail", "the relay does not know this machine as the council agent: " + err.Error(), "Run TINCAN_CONFIG=" + configPath + " tincan rejoin; a machine that was never joined needs an invite."}}
	case err != nil:
		return []check{{"relay", "fail", "cannot reach the relay at " + cfg.Relay + ": " + err.Error(), "Check that this machine is on the tailnet (tailscale status) and the relay is running."}}
	case me.Name != council.DefaultAgentName:
		return []check{{"relay", "fail", fmt.Sprintf("%s is joined as %q, not %q", configPath, me.Name, council.DefaultAgentName), joinFix}}
	}
	out := []check{{"relay", "ok", fmt.Sprintf("reachable at %s; this machine is agent %q", r.Base(), me.Name), ""}}
	kindFix := "Upgrade the relay to this release first, then on an admin device run: tincan kind council council. Until then council serve refuses to run."
	switch me.Kind {
	case onboard.KindCouncil:
		out = append(out, check{"relay kind", "ok", `the relay stores kind "council", so it holds agents' councils for the owner's approval`, ""})
	case "":
		out = append(out, check{"relay kind", "fail", "the relay stores no kind for council, so it would not hold agents' councils for the owner's approval", kindFix})
	default:
		out = append(out, check{"relay kind", "fail", fmt.Sprintf("the relay stores kind %q for council, not \"council\", so it would not hold agents' councils for the owner's approval", me.Kind), kindFix})
	}
	return out
}
