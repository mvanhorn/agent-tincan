package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// traceResp is the relay's trace response.
type traceResp struct {
	TraceID string             `json:"trace_id"`
	Steps   []envelope.Result  `json:"steps"`
	Events  []store.AuditEvent `json:"events"`
}

// adminRelay lets admin commands go through the local admin socket on the
// relay host instead of the network.
func adminRelay(socket, relayURL string) (*client.Relay, error) {
	if socket == "" && relayURL == "" {
		// On the relay machine itself the local admin socket makes this
		// machine the admin: no --admin device and no flags needed.
		socket = localAdminSocket()
	}
	if socket != "" {
		return client.NewRelaySocket(socket), nil
	}
	r, _, err := relayFor(relayURL)
	return r, err
}

func traceCmd() *cobra.Command {
	var socket, relayURL string
	var limit int
	var pings bool
	var format, out string
	var force bool
	cmd := &cobra.Command{
		Use:   "trace [trace-id]",
		Short: "Show who asked what: one request chain, or the most recent chains (admin)",
		Long: `Show who asked what: one request chain, or the most recent chains (admin).

The text view cuts bodies short for reading. To keep a whole chain as a
record, use --format md or --format json, and --out to write it to a file
that only you can read:

  tincan trace <trace-id> --format md --out errand.md`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !slices.Contains(traceFormats, format) {
				return fmt.Errorf("unknown --format %q: use text, md or json", format)
			}
			if len(args) == 0 && (cmd.Flags().Changed("format") || out != "" || force) {
				return errors.New("--format, --out and --force need a trace id")
			}
			if force && out == "" {
				return errors.New("--force needs --out")
			}
			r, err := adminRelay(socket, relayURL)
			if err != nil {
				return err
			}
			if len(args) == 0 {
				var out struct {
					Traces []envelope.Result `json:"traces"`
				}
				if err := r.Raw(cmd.Context(), "GET", fmt.Sprintf("/v1/trace?limit=%d&exclude_pings=%t", limit, !pings), nil, &out); err != nil {
					return err
				}
				out.Traces = filterPingTraces(out.Traces, pings)
				for _, st := range out.Traces {
					if st.Progress != nil {
						cmd.Printf("%s  %s\n", st.Request.TraceID, client.FormatProgress(st.Progress))
					}
					cmd.Printf("%s  %s -> %s  %-9s %s\n", st.Request.TraceID, st.Request.From, st.Request.To, st.Status, oneLine(string(st.Request.Kind)+" "+st.Request.Body))
				}
				if len(out.Traces) == 0 {
					cmd.Println("No requests yet.")
				}
				return nil
			}
			var tr traceResp
			if err := r.Raw(cmd.Context(), "GET", "/v1/trace/"+args[0], nil, &tr); err != nil {
				return err
			}
			if !pings && len(tr.Steps) > 0 && tr.Steps[0].Request.Kind == envelope.KindPing {
				if format != "text" || out != "" {
					return errors.New("ping hidden; use --pings to export it")
				}
				cmd.Println("Ping hidden; use --pings to show it.")
				return nil
			}
			text, err := renderTrace(tr, format)
			if err != nil {
				return err
			}
			if out == "" {
				cmd.Print(text)
				return nil
			}
			if err := writeTraceFile(out, []byte(text), force); err != nil {
				return err
			}
			cmd.Printf("Wrote trace %s to %s.\n", tr.TraceID, out)
			return nil
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", "relay admin socket (when running on the relay host)")
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay URL (default: saved config)")
	cmd.Flags().BoolVar(&pings, "pings", false, "include ping checks")
	cmd.Flags().IntVar(&limit, "limit", 20, "how many recent chains to list")
	cmd.Flags().StringVar(&format, "format", "text", "output format for one chain: text, md or json")
	cmd.Flags().StringVar(&out, "out", "", "write the chain to this file, mode 0600, instead of printing it")
	cmd.Flags().BoolVar(&force, "force", false, "replace an existing --out file")
	return cmd
}

// traceFormats are the values --format takes.
var traceFormats = []string{"text", "md", "json"}

func renderTrace(tr traceResp, format string) (string, error) {
	switch format {
	case "md":
		return formatTraceMarkdown(tr), nil
	case "json":
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false) // bodies stay readable in the file
		enc.SetIndent("", "  ")
		if err := enc.Encode(tr); err != nil {
			return "", err
		}
		return b.String(), nil
	}
	return formatTrace(tr), nil
}

// writeTraceFile writes an export 0600, since a chain can hold private text.
// Without force an existing path is refused, a symlink included. With force
// the path is replaced, not truncated, so the old file's mode cannot survive
// and a symlink is not written through.
func writeTraceFile(path string, data []byte, force bool) error {
	if force {
		return writeFileReplacing(path, data, 0o600)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%s already exists; use --force to replace it", path)
	}
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if err := errors.Join(werr, f.Close()); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

func formatTrace(tr traceResp) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Trace %s\n", tr.TraceID)
	for _, st := range tr.Steps {
		fmt.Fprintf(&b, "%s%s -> %s  [%s]  %s\n", strings.Repeat("  ", max(st.Request.Hop-1, 0)), st.Request.From, traceTarget(st.Request), st.Status, oneLine(string(st.Request.Kind)+" "+st.Request.Body))
		if st.Progress != nil {
			fmt.Fprintf(&b, "  %s\n", client.FormatProgress(st.Progress))
		}
		if st.Reply != nil {
			fmt.Fprintf(&b, "%s  reply from %s: %s (status %s)\n", strings.Repeat("  ", max(st.Request.Hop-1, 0)), st.Reply.From, oneLine(st.Reply.Body), st.Reply.Status)
		}
	}
	if len(tr.Events) > 0 {
		b.WriteString("Events:\n")
		for _, e := range tr.Events {
			fmt.Fprintf(&b, "  #%d %-9s %-10s %s%s\n", e.Seq, e.Event, e.Actor, e.RequestID, traceEventDetail(e))
		}
	}
	return b.String()
}

// traceTarget names a request's target and, after failover moved it off a
// web agent that was out of messages, where it started.
func traceTarget(req envelope.Request) string {
	if len(req.MovedFrom) == 0 {
		return req.To
	}
	return req.To + " (moved from " + strings.Join(req.MovedFrom, ", ") + ")"
}

// traceEventDetail is the part of an audit event's detail worth reading.
func traceEventDetail(e store.AuditEvent) string {
	var fields struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if json.Unmarshal([]byte(e.Detail), &fields) != nil {
		return ""
	}
	if e.Event == "replied" && fields.Status != "" {
		return " (reply status " + fields.Status + ")"
	}
	if e.Event == "rejected" && fields.Reason != "" {
		return " (" + oneLine(fields.Reason) + ")"
	}
	return ""
}

// formatTraceMarkdown renders a whole chain as a document. Everything a
// teammate wrote goes in a fence, so no body can write into the document.
func formatTraceMarkdown(tr traceResp) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Trace %s\n", tr.TraceID)
	for _, st := range tr.Steps {
		req := st.Request
		fmt.Fprintf(&b, "\n## Hop %d: %s -> %s [%s]\n\n", req.Hop, req.From, traceTarget(req), st.Status)
		fmt.Fprintf(&b, "- Request: %s\n- Kind: %s\n- Sent: %s\n", req.ID, req.Kind, traceTime(req.CreatedAt))
		if req.ParentID != "" {
			fmt.Fprintf(&b, "- Parent: %s\n", req.ParentID)
		}
		writeFenced(&b, "Request", req.Body)
		writeAttachments(&b, req.Attachments)
		for _, x := range st.Exchanges {
			writeFenced(&b, "Question, "+traceTime(x.At), x.Question)
			if x.Answer != "" {
				writeFenced(&b, "Answer", x.Answer)
			}
		}
		if p := st.Progress; p != nil { // the relay sends it only while a hop is claimed
			writeFenced(&b, fmt.Sprintf("Note from %s, %s", p.By, traceTime(p.At)), p.Note)
		}
		if st.Reply != nil {
			writeFenced(&b, fmt.Sprintf("Reply from %s, status %s, %s", st.Reply.From, st.Reply.Status, traceTime(st.Reply.CreatedAt)), st.Reply.Body)
			writeAttachments(&b, st.Reply.Attachments)
		}
	}
	if len(tr.Events) > 0 {
		var ev strings.Builder
		for _, e := range tr.Events { // the raw detail, since the terminal's summary cuts it
			fmt.Fprintln(&ev, strings.TrimSpace(fmt.Sprintf("#%d %s %s %s %s %s", e.Seq, traceTime(e.At), e.Event, e.Actor, e.RequestID, e.Detail)))
		}
		b.WriteString("\n## Events\n")
		writeFenced(&b, "", ev.String())
	}
	return b.String()
}

func traceTime(t time.Time) string {
	if t.IsZero() {
		return "time unknown"
	}
	return t.UTC().Format(time.RFC3339)
}

func writeAttachments(b *strings.Builder, atts []envelope.Attachment) {
	if len(atts) == 0 {
		return
	}
	var names strings.Builder
	for _, a := range atts {
		fmt.Fprintf(&names, "%s (%s, %d bytes)\n", a.Name, a.MIME, a.Size)
	}
	writeFenced(b, "Attachments", names.String())
}

// writeFenced writes text in a backtick fence one longer than any run of
// backticks in it, after an optional label.
func writeFenced(b *strings.Builder, label, text string) {
	longest, run := 2, 0
	for _, c := range text {
		if c != '`' {
			run = 0
			continue
		}
		run++
		longest = max(longest, run)
	}
	fence := strings.Repeat("`", longest+1)
	if label != "" {
		fmt.Fprintf(b, "\n%s:\n", label)
	}
	fmt.Fprintf(b, "\n%stext\n%s\n%s\n", fence, strings.TrimSuffix(text, "\n"), fence)
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 80 {
		return s[:77] + "..."
	}
	return s
}

func auditCmd() *cobra.Command {
	var socket, relayURL string
	cmd := &cobra.Command{
		Use:   "audit-verify",
		Short: "Check that the relay's audit log has not been altered (admin)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := adminRelay(socket, relayURL)
			if err != nil {
				return err
			}
			var out struct {
				OK      bool   `json:"ok"`
				Checked int    `json:"checked"`
				Error   string `json:"error"`
			}
			if err := r.Raw(cmd.Context(), "GET", "/v1/admin/audit/verify", nil, &out); err != nil {
				return err
			}
			if !out.OK {
				return fmt.Errorf("audit log check failed after %d entries: %s", out.Checked, out.Error)
			}
			cmd.Printf("Audit log intact (%d entries).\n", out.Checked)
			return nil
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", "relay admin socket (when running on the relay host)")
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay URL (default: saved config)")
	return cmd
}

// localAdminSocket returns the relay's admin socket in the default state
// dir when a relay runs on this machine, or "".
func localAdminSocket() string {
	p := filepath.Join(defaultStateDir(), "admin.sock")
	if st, err := os.Stat(p); err == nil && st.Mode()&os.ModeSocket != 0 {
		return p
	}
	return ""
}

func filterPingTraces(in []envelope.Result, pings bool) []envelope.Result {
	if pings {
		return in
	}
	var out []envelope.Result
	for _, r := range in {
		if r.Request.Kind != envelope.KindPing {
			out = append(out, r)
		}
	}
	return out
}
