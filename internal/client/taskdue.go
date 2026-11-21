package client

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

var errTaskDueUnsupported = errors.New("this relay does not support task due dates; upgrade the relay")

// ParseTaskDue resolves a calendar day or a time in the relay's task zone.
func ParseTaskDue(phrase, zone string, now time.Time) (envelope.TaskDue, error) {
	loc, err := envelope.TaskLocation(zone)
	if err != nil {
		return envelope.TaskDue{}, err
	}
	phrase = strings.ToLower(strings.Join(strings.Fields(phrase), " "))
	if phrase == "clear" {
		return envelope.TaskDue{}, nil
	}
	now = now.In(loc)
	if at, err := time.Parse(time.RFC3339, strings.ToUpper(phrase)); err == nil {
		return timedTaskDue(at.In(loc)), nil
	}
	if delay, ok := strings.CutPrefix(phrase, "in "); ok {
		if d, err := time.ParseDuration(delay); err == nil && d > 0 {
			return timedTaskDue(now.Add(d)), nil
		}
	}
	day, clock, _ := strings.Cut(phrase, " ")
	var date time.Time
	var weekday bool
	switch day {
	case "today":
		date = now
	case "tomorrow":
		date = now.AddDate(0, 0, 1)
	default:
		date, err = time.ParseInLocation(time.DateOnly, day, loc)
		if err != nil {
			for offset := range 7 {
				candidate := now.AddDate(0, 0, offset)
				name := strings.ToLower(candidate.Weekday().String())
				if day == name || day == name[:3] {
					date, weekday = candidate, true
					break
				}
			}
		}
	}
	if date.IsZero() {
		return envelope.TaskDue{}, fmt.Errorf("invalid due date %q: use a date, a weekday, today, tomorrow, in 2h, or RFC3339", phrase)
	}
	if clock == "" {
		return envelope.TaskDue{DueDay: date.Format(time.DateOnly)}, nil
	}
	clock = strings.ReplaceAll(clock, " ", "")
	for _, layout := range []string{"3pm", "3:04pm", "15:04"} {
		wall, err := time.Parse(layout, clock)
		if err != nil {
			continue
		}
		at := taskWallTime(date, wall)
		if weekday && at.Before(now) {
			date = date.AddDate(0, 0, 7)
			at = taskWallTime(date, wall)
		}
		if at.Day() != date.Day() || at.Hour() != wall.Hour() || at.Minute() != wall.Minute() {
			return envelope.TaskDue{}, fmt.Errorf("due time %q does not exist on %s in %s", clock, date.Format(time.DateOnly), loc)
		}
		return timedTaskDue(at), nil
	}
	return envelope.TaskDue{}, fmt.Errorf("invalid due time %q: use a time such as 3pm or 15:00", clock)
}

func taskWallTime(date, wall time.Time) time.Time {
	return time.Date(date.Year(), date.Month(), date.Day(), wall.Hour(), wall.Minute(), 0, 0, date.Location())
}

func timedTaskDue(at time.Time) envelope.TaskDue {
	seconds := at.Unix()
	return envelope.TaskDue{DueDay: at.Format(time.DateOnly), DueAt: &seconds}
}

func (c Capabilities) parseTaskDue(when string) (envelope.TaskDue, error) {
	if c.TaskZone == "" {
		return envelope.TaskDue{}, errTaskDueUnsupported
	}
	return ParseTaskDue(when, c.TaskZone, time.Now())
}

// SetTaskDue changes or clears a due date as the filer or as the owner.
func SetTaskDue(ctx context.Context, c Rawer, owner bool, id int64, when string) (envelope.Task, error) {
	caps, err := taskCapabilities(ctx, c)
	if err != nil {
		return envelope.Task{}, err
	}
	due, err := caps.parseTaskDue(when)
	if err != nil {
		return envelope.Task{}, err
	}
	var out TaskResult
	err = c.Raw(ctx, "POST", taskPath(owner, id, "/due"), due, &out)
	out.Task.DueZone = caps.TaskZone
	return out.Task, err
}

func taskDueRank(t envelope.Task, now time.Time) int {
	if t.DueDay == "" || t.Status == envelope.TaskDone || t.Status == envelope.TaskDropped {
		return 2
	}
	loc, err := envelope.TaskLocation(t.DueZone)
	if err != nil {
		return 2
	}
	today := now.In(loc).Format(time.DateOnly)
	switch {
	case t.DueDay < today || t.DueAt != nil && *t.DueAt < now.Unix():
		return 0
	case t.DueDay == today:
		return 1
	default:
		return 2
	}
}

func orderTasksByDue(list []envelope.Task, now time.Time) {
	slices.SortStableFunc(list, func(a, b envelope.Task) int {
		ar, br := taskDueRank(a, now), taskDueRank(b, now)
		if ar != br || ar == 2 {
			return cmp.Compare(ar, br)
		}
		if day := cmp.Compare(a.DueDay, b.DueDay); day != 0 {
			return day
		}
		switch {
		case a.DueAt == nil && b.DueAt == nil:
			return 0
		case a.DueAt == nil:
			return -1
		case b.DueAt == nil:
			return 1
		default:
			return cmp.Compare(*a.DueAt, *b.DueAt)
		}
	})
}

// FormatTaskDue presents the due day and optional local wall time.
func FormatTaskDue(t envelope.Task) string {
	if t.DueAt != nil {
		if loc, err := envelope.TaskLocation(t.DueZone); err == nil {
			return time.Unix(*t.DueAt, 0).In(loc).Format("2006-01-02 15:04 MST")
		}
	}
	return t.DueDay
}

// FormatTaskDueChange describes the result of setting or clearing the due date.
func FormatTaskDueChange(t envelope.Task) string {
	if due := FormatTaskDue(t); due != "" {
		return fmt.Sprintf("Task #%d is due %s.", t.ID, due)
	}
	return fmt.Sprintf("Cleared the due date for task #%d.", t.ID)
}
