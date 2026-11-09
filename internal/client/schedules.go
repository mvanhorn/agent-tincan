package client

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// ErrSchedulesUnsupported identifies a relay that predates scheduled asks.
// Nothing was sent.
var ErrSchedulesUnsupported = errors.New("relay does not support scheduled asks, so nothing was sent; upgrade the relay")

// requireSchedules fails with ErrSchedulesUnsupported unless the relay
// advertises schedules. It runs before every schedule call, so a scheduled
// ask is never handed to a relay that would drop its time.
func requireSchedules(ctx context.Context, c Rawer) error {
	caps, err := capabilities(ctx, c)
	if err != nil {
		return fmt.Errorf("check relay capabilities: %w", err)
	}
	if !caps.Schedules {
		return ErrSchedulesUnsupported
	}
	return nil
}

// ScheduledAsk is an ask for the relay to send later. At is the first time,
// and with Every the relay repeats it; with Every and no At the first time
// is one interval from now.
type ScheduledAsk struct {
	To     string
	Body   string
	Urgent bool
	At     time.Time
	Every  time.Duration
}

// ScheduleAsk asks the relay to send a as this agent later.
func ScheduleAsk(ctx context.Context, c Rawer, a ScheduledAsk) (envelope.Schedule, error) {
	if err := requireSchedules(ctx, c); err != nil {
		return envelope.Schedule{}, err
	}
	in := struct {
		To           string    `json:"to"`
		Body         string    `json:"body"`
		Urgent       bool      `json:"urgent,omitempty"`
		At           time.Time `json:"at,omitzero"`
		EverySeconds int64     `json:"every_seconds,omitempty"`
	}{a.To, a.Body, a.Urgent, a.At, int64(a.Every / time.Second)}
	var out envelope.Schedule
	err := c.Raw(ctx, "POST", "/v1/schedules", in, &out)
	return out, err
}

// Schedules lists this agent's schedules, or with all every agent's (admin
// devices only).
func Schedules(ctx context.Context, c Rawer, all bool) ([]envelope.Schedule, error) {
	if err := requireSchedules(ctx, c); err != nil {
		return nil, err
	}
	q := url.Values{"after": {"0"}}
	if all {
		q.Set("all", "1")
	}
	var list []envelope.Schedule
	var after int64
	for {
		path := "/v1/schedules?" + q.Encode()
		var out struct {
			Schedules []envelope.Schedule `json:"schedules"`
			NextAfter int64               `json:"next_after"`
		}
		if err := c.Raw(ctx, "GET", path, nil, &out); err != nil {
			return nil, err
		}
		list = append(list, out.Schedules...)
		if out.NextAfter == 0 {
			return list, nil
		}
		if out.NextAfter <= after {
			return nil, errors.New("relay repeated a schedule page")
		}
		after = out.NextAfter
		q.Set("after", strconv.FormatInt(after, 10))
	}
}

// CancelSchedule stops schedule id, one of this agent's or, from an admin
// device, any.
func CancelSchedule(ctx context.Context, c Rawer, id int64) (envelope.Schedule, error) {
	if err := requireSchedules(ctx, c); err != nil {
		return envelope.Schedule{}, err
	}
	var out envelope.Schedule
	err := c.Raw(ctx, "DELETE", "/v1/schedules/"+strconv.FormatInt(id, 10), nil, &out)
	return out, err
}

// ParseWhen reads the at and every of tincan ask and the ask tool. at is an
// RFC 3339 time or a delay from now such as 2h, and every a duration; either
// may be empty. The relay decides what it will take.
func ParseWhen(at, every string, now time.Time) (time.Time, time.Duration, error) {
	var when time.Time
	var interval time.Duration
	if at != "" {
		var err error
		if when, err = time.Parse(time.RFC3339, at); err != nil {
			d, derr := time.ParseDuration(at)
			if derr != nil {
				return time.Time{}, 0, fmt.Errorf("at %q is neither an RFC 3339 time nor a delay such as 2h", at)
			}
			when = now.Add(d)
		}
	}
	if every != "" {
		var err error
		if interval, err = time.ParseDuration(every); err != nil {
			return time.Time{}, 0, fmt.Errorf("every %q is not a duration such as 1h", every)
		}
		// The wire counts whole seconds, and no every_seconds means no repeat.
		if interval < time.Second {
			return time.Time{}, 0, fmt.Errorf("every %q is too short", every)
		}
	}
	return when, interval, nil
}

// TerminalSafe maps the control and bidi runes in s to spaces. Relay text is
// data, including when it contains terminal control sequences: it must not
// be able to rewrite what a listing shows the owner.
func TerminalSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return ' '
		}
		return r
	}, s)
}

// ScheduleText is an ask's text as a listing shows it, on one line. The relay
// cuts its preview from this, so padding cannot use up the preview.
func ScheduleText(body string) string {
	return strings.Join(strings.Fields(TerminalSafe(body)), " ")
}

// schedulePreview is how much of an ask's text a schedule listing shows.
const schedulePreview = 80

// FormatSchedule is what tincan ask and the ask tool print for a new
// schedule, the same on both.
func FormatSchedule(s envelope.Schedule, now time.Time) string {
	return fmt.Sprintf("Scheduled ask #%d to %s: %s. Nothing was sent yet; its reply comes like any other. Stop it with tincan unschedule %d or cancel_schedule.\n", s.ID, s.To, scheduleWhen(s, now), s.ID)
}

// FormatSchedules renders a schedule list for tincan schedules and the
// list_schedules tool.
func FormatSchedules(list []envelope.Schedule, now time.Time) string {
	if len(list) == 0 {
		return "No scheduled asks.\n"
	}
	var b strings.Builder
	for _, s := range list {
		body := ScheduleText(s.Body)
		if r := []rune(body); len(r) > schedulePreview {
			body = string(r[:schedulePreview]) + "..."
		}
		fmt.Fprintf(&b, "#%d %s to %s: %s", s.ID, s.From, s.To, scheduleWhen(s, now))
		if s.Blocked {
			b.WriteString(", last fire refused")
		}
		fmt.Fprintf(&b, ": %s\n", body)
	}
	return b.String()
}

func scheduleWhen(s envelope.Schedule, now time.Time) string {
	if s.EverySeconds == 0 {
		return "once at " + clockTime(s.NextAt, now)
	}
	return fmt.Sprintf("next at %s, every %v", clockTime(s.NextAt, now), time.Duration(s.EverySeconds)*time.Second)
}
