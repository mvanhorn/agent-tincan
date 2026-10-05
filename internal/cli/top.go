package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	textwidth "golang.org/x/text/width"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

type topFrame struct {
	refreshError string
	roster       client.Roster
	held         []envelope.Request
	chains       []envelope.Result
	admin        bool
	note         string
	at           time.Time
}

func topCmd() *cobra.Command {
	var socket, relayURL string
	var once bool
	var interval time.Duration
	cmd := &cobra.Command{Use: "top", Short: "Watch the mesh live (read-only)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if interval < time.Second {
				return fmt.Errorf("interval must be at least 1s")
			}
			r, err := adminRelay(socket, relayURL)
			if err != nil {
				return err
			}
			out, ok := cmd.OutOrStdout().(*os.File)
			live := !once && ok && term.IsTerminal(int(out.Fd())) && term.IsTerminal(int(os.Stdin.Fd()))
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			if live {
				cleanup, err := topInput(ctx, cancel)
				if err != nil {
					return err
				}
				defer cleanup()
			}
			return topLoop(ctx, live, interval, func(ctx context.Context) (topFrame, error) {
				return fetchFrame(ctx, r)
			}, func(f topFrame) error {
				width, height := 120, 24
				if live {
					if w, h, err := term.GetSize(int(out.Fd())); err == nil && w > 0 && h > 0 {
						width, height = w, h
					}
				}
				frame := renderFrame(f, width)
				if live {
					frame = "\x1b[H\x1b[2J" + strings.ReplaceAll(topLiveFrame(f, width, height), "\n", "\r\n")
				}
				if _, err := fmt.Fprint(cmd.OutOrStdout(), frame); err != nil {
					return err
				}
				return nil
			})
		}}
	cmd.Flags().StringVar(&socket, "socket", "", "relay admin socket")
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay URL (default: saved config)")
	cmd.Flags().BoolVar(&once, "once", false, "print one plain snapshot and exit")
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "refresh interval (minimum 1s)")
	return cmd
}

func topLoop(ctx context.Context, live bool, interval time.Duration, fetch func(context.Context) (topFrame, error), render func(topFrame) error) error {
	var last topFrame
	for {
		f, err := fetch(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			if !live {
				return err
			}
			f = last
			if f.at.IsZero() {
				f.at = time.Now()
			}
			f.refreshError = fmt.Sprintf("refresh failed at %s: %v (retrying)", time.Now().Format("15:04:05"), err)
		} else {
			last = f
		}
		if err := render(f); err != nil {
			return err
		}
		if !live {
			return nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func fetchFrame(ctx context.Context, r *client.Relay) (topFrame, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	f := topFrame{at: time.Now()}
	var err error
	f.roster, err = r.Roster(ctx)
	if err != nil {
		return f, err
	}
	if err = r.Raw(ctx, "GET", "/v1/admin/held", nil, &f.held); err != nil {
		if e, ok := errors.AsType[*client.APIError](err); ok && (e.Code == 403 || e.Code == 404) {
			f.note = "held and recent chains need an admin device"
			if e.Code == 404 {
				f.note = "held and recent chains unavailable on this relay"
			}
			return f, nil
		}
		return f, err
	}
	f.admin = true
	var out struct {
		Traces []envelope.Result `json:"traces"`
	}
	if err = r.Raw(ctx, "GET", "/v1/trace?limit=8&exclude_pings=true", nil, &out); err != nil {
		if e, ok := errors.AsType[*client.APIError](err); ok && (e.Code == 403 || e.Code == 404) {
			f.note = "recent chains unavailable on this relay"
			return f, nil
		}
		return f, err
	}
	f.chains = filterPingTraces(out.Traces, false)
	return f, nil
}

func attention(a client.AgentInfo, relayVersion string, now time.Time) (score int, flags []string) {
	overdue := a.OverdueNote(now) != ""
	// Only webhook and email wakes come from the relay. An offline agent on
	// an agent-side method (command, channel, wait) has lost the process
	// that would notice its queue, and a scheduled one counts only once it
	// misses its checks. A relay-woken one counts once the relay says its
	// last wake went unanswered.
	relayWakes := a.Wake == "webhook" || a.Wake == "email" || (a.Wake == "schedule" && !overdue)
	if !a.Online && a.Queued > 0 && !relayWakes {
		score += 8
		flags = append(flags, "QUEUED-OFFLINE")
	}
	if a.SignedOutSite != "" {
		score += 6
		flags = append(flags, "SIGNED-OUT")
	}
	if a.Unanswered {
		score += 6
		flags = append(flags, "UNANSWERED")
	}
	if a.Queued > 0 && !a.OldestQueued.IsZero() && now.Sub(a.OldestQueued) > time.Hour {
		score += 4
		flags = append(flags, "STALE")
	}
	if overdue {
		score += 2
		flags = append(flags, "OVERDUE")
	}
	if client.Ahead(relayVersion, a.Version) {
		score++
		flags = append(flags, "OLD BUILD")
	}
	if a.Claimed > 0 {
		flags = append(flags, "CLAIMED")
	}
	return
}

func renderFrame(f topFrame, width int) string {
	return renderTopFrame(f, width, 0)
}

func renderTopFrame(f topFrame, width, height int) string {
	var b strings.Builder
	line := func(s string) { b.WriteString(topText(s, max(width, 1))); b.WriteByte('\n') }
	online, queued := 0, 0
	for _, a := range f.roster.Agents {
		if a.Online {
			online++
		}
		queued += a.Queued
	}
	held := "?"
	if f.admin {
		held = fmt.Sprint(len(f.held))
	}
	line(fmt.Sprintf("tincan top | relay %s | online %d/%d | queued %d | held %s | %s", f.roster.RelayVersion, online, len(f.roster.Agents), queued, held, f.at.Format("15:04:05")))
	if f.refreshError != "" {
		line(f.refreshError)
	}
	line("AGENT            STATE   WAKE         QUEUED OLDEST CLAIMS VERSION")
	agents := slices.Clone(f.roster.Agents)
	slices.SortStableFunc(agents, func(a, b client.AgentInfo) int {
		sa, _ := attention(a, f.roster.RelayVersion, f.at)
		sb, _ := attention(b, f.roster.RelayVersion, f.at)
		if sa != sb {
			return sb - sa
		}
		return strings.Compare(a.Name, b.Name)
	})
	heldLimit, chainLimit := len(f.held), len(f.chains)
	budget := height - 2
	if f.refreshError != "" {
		budget--
	}
	if f.note != "" {
		budget--
	}
	if height > 0 && f.admin {
		heldLimit, chainLimit = min(3, heldLimit), min(3, chainLimit)
		budget -= 2 + heldLimit + chainLimit
		if heldLimit < len(f.held) {
			budget--
		}
		if chainLimit < len(f.chains) {
			budget--
		}
		minimum := 0
		if len(agents) > 0 {
			minimum = 1
		}
		for budget < minimum && (heldLimit > 0 || chainLimit > 0) {
			if heldLimit >= chainLimit && heldLimit > 0 {
				if heldLimit < len(f.held) {
					budget++
				}
				heldLimit--
			} else {
				if chainLimit < len(f.chains) {
					budget++
				}
				chainLimit--
			}
		}
	}
	for i, a := range agents {
		_, flags := attention(a, f.roster.RelayVersion, f.at)
		rows := 1
		if len(flags) > 0 {
			rows++
		}
		reserve := 0
		if i < len(agents)-1 {
			reserve = 1
		}
		if height > 0 && rows+reserve > budget {
			line(fmt.Sprintf("... %d more agents (tincan top --once for all)", len(agents)-i))
			break
		}
		budget -= rows
		age := "-"
		if a.Queued > 0 && !a.OldestQueued.IsZero() {
			age = client.QueueAge(f.at.Sub(a.OldestQueued))
		}
		line(fmt.Sprintf("%s %-7s %s %6d %6s %6d %s", topPad(a.Name, 16), a.State(), topPad(a.Wake, 12), a.Queued, age, a.Claimed, a.Version))
		if len(flags) > 0 {
			line("  " + strings.Join(flags, " | "))
		}
	}
	if f.admin {
		line(fmt.Sprintf("Held for approval (%d):", len(f.held)))
		for _, r := range f.held[:heldLimit] {
			line(fmt.Sprintf("  %s %s -> %s %s", r.ID, r.From, r.To, topText(r.Body, 60)))
		}
		if heldLimit < len(f.held) {
			line(fmt.Sprintf("  ... %d more", len(f.held)-heldLimit))
		}
		line("Recent chains:")
		for _, r := range f.chains[:chainLimit] {
			line(fmt.Sprintf("  %s %s -> %s [%s] %s", r.Request.CreatedAt.Local().Format("15:04:05"), r.Request.From, r.Request.To, r.Status, topText(r.Request.Body, 60)))
		}
		if chainLimit < len(f.chains) {
			line(fmt.Sprintf("  ... %d more", len(f.chains)-chainLimit))
		}
	}
	if f.note != "" {
		line(f.note)
	}
	return b.String()
}

// Relay text is data, including when it contains terminal control sequences.
func topText(s string, width int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return ' '
		}
		return r
	}, s)
	cells := 0
	for _, r := range s {
		cells += topRuneWidth(r)
	}
	if cells <= width {
		return s
	}
	limit := max(width, 0)
	suffix := ""
	if width > 3 {
		limit -= 3
		suffix = "..."
	}
	cells = 0
	sequenceStart := 0
	joined := false
	for i, r := range s {
		if r != '\u200d' && topRuneWidth(r) > 0 && !joined {
			sequenceStart = i
		}
		joined = r == '\u200d' || (joined && topRuneWidth(r) == 0)
		cells += topRuneWidth(r)
		if cells > limit {
			return s[:sequenceStart] + suffix
		}
	}
	return s
}

// Bound only interactive frames; snapshots retain every rendered line.
func topLiveFrame(f topFrame, width, height int) string {
	if height <= 0 {
		return ""
	}
	lines := strings.Split(strings.TrimSuffix(renderTopFrame(f, width, height), "\n"), "\n")
	return strings.Join(lines[:min(height, len(lines))], "\n")
}

func topPad(s string, width int) string {
	s = topText(s, width)
	cells := 0
	for _, r := range s {
		cells += topRuneWidth(r)
	}
	return s + strings.Repeat(" ", max(0, width-cells))
}

func topRuneWidth(r rune) int {
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Cf, r) {
		return 0
	}
	kind := textwidth.LookupRune(r).Kind()
	if kind == textwidth.EastAsianWide || kind == textwidth.EastAsianFullwidth ||
		(r >= 0x1f000 && r <= 0x1faff) || (r >= 0x2600 && r <= 0x27bf) {
		return 2
	}
	return 1
}
