package envelope

import (
	"errors"
	"fmt"
	"time"
	_ "time/tzdata" // Keep IANA zone support on hosts without a zone database.
)

const (
	TaskNoticeReminder      = "task-reminder"
	TaskNoticeBriefing      = "task-briefing"
	TaskNoticeStall         = "task-stall"
	TaskNoticeAction        = "task-action"
	TaskNoticeReturnedClose = "task-returned-close"
)

// TaskReminderMessage is the owner-facing text shared by delivery and fallback.
func TaskReminderMessage(id int64) string {
	return fmt.Sprintf("Reminder: task #%d is due now.", id)
}

const DefaultTaskZone = "America/Los_Angeles"

// TaskDue holds the calendar day in the owner's zone. A time is optional
// and is represented by Unix seconds, including zero.
type TaskDue struct {
	DueDay string `json:"due_day,omitempty"`
	DueAt  *int64 `json:"due_at,omitempty"`
}

// TaskDueChange preserves both deadlines independently of any owner words.
type TaskDueChange struct {
	Before TaskDue `json:"before"`
	After  TaskDue `json:"after"`
}

// TaskLocation resolves the configured IANA zone independently of the process zone.
func TaskLocation(zone string) (*time.Location, error) {
	if zone == "" {
		zone = DefaultTaskZone
	}
	if zone == "Local" {
		return nil, errors.New("use an IANA time zone, not Local")
	}
	return time.LoadLocation(zone)
}

// Validate accepts normalized due fields, including an empty pair to clear them.
func (d TaskDue) Validate(loc *time.Location) error {
	if d.DueDay == "" && d.DueAt == nil {
		return nil
	}
	if day, err := time.Parse(time.DateOnly, d.DueDay); err != nil || day.Format(time.DateOnly) != d.DueDay {
		return errors.New("due_day must be a date in YYYY-MM-DD format")
	}
	if d.DueAt != nil && time.Unix(*d.DueAt, 0).In(loc).Format(time.DateOnly) != d.DueDay {
		return fmt.Errorf("due_at must fall on due_day in %s", loc)
	}
	return nil
}
