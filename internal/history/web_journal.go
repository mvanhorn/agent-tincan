package history

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// DefaultWebJournalRetention is how long a send journal entry is kept: the
// relay's 30-minute claim lease (after which it requeues a claimed request
// that got no reply) plus a margin for a second requeue and the request
// timeout.
const DefaultWebJournalRetention = 30*time.Minute + time.Hour

// webJournal is the send journal: request id -> what was sent for it. It
// holds ids and times only, never messages or replies. A requeued request
// found here is resumed by reading its reply from the conversation again
// rather than by sending it again (or by keeping reply text on disk).
type webJournal struct {
	Requests map[string]webSend `json:"requests"`
}

const (
	webSendSent     = "sent"
	webSendAnswered = "answered"
)

type webSend struct {
	ConversationID string `json:"conversation_id"`
	// UserMessageID is this request's user message, once seen.
	UserMessageID string `json:"user_message_id,omitempty"`
	// PrevUserID is the conversation's last user message before the send.
	PrevUserID  string    `json:"prev_user_id,omitempty"`
	SubmittedAt time.Time `json:"submitted_at,omitzero"`
	// Recorded is when the entry was first written; pruning goes by it.
	Recorded time.Time `json:"recorded"`
	// State is sent, or answered once the reply was built and sent.
	State string `json:"state"`
}

func (w *WebAgent) journalRetention() time.Duration {
	if w.JournalRetention > 0 {
		return w.JournalRetention
	}
	return DefaultWebJournalRetention
}

// loadJournal reads the journal, dropping entries past the retention and
// ones whose ids are malformed. A missing or unreadable journal is empty.
func (w *WebAgent) loadJournal() webJournal {
	j := webJournal{Requests: map[string]webSend{}}
	if w.JournalPath == "" {
		return j
	}
	b, err := os.ReadFile(w.JournalPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			w.logf("journal %s: %v (starting fresh)", w.JournalPath, err)
		}
		return j
	}
	if err := json.Unmarshal(b, &j); err != nil {
		w.logf("journal %s: %v (starting fresh)", w.JournalPath, err)
		j = webJournal{}
	}
	if j.Requests == nil {
		j.Requests = map[string]webSend{}
	}
	cutoff := time.Now().Add(-w.journalRetention())
	for id, e := range j.Requests {
		if e.Recorded.Before(cutoff) || !validNativeID(e.ConversationID) || (e.UserMessageID != "" && !validNativeID(e.UserMessageID)) {
			delete(j.Requests, id)
		}
	}
	return j
}

// journal records (or updates) reqID's entry and prunes old ones. Callers
// hold w.mu.
func (w *WebAgent) journal(reqID string, e webSend) {
	if w.JournalPath == "" {
		return
	}
	j := w.loadJournal()
	j.Requests[reqID] = e
	b, err := json.MarshalIndent(j, "", "  ")
	if err == nil {
		err = os.MkdirAll(filepath.Dir(w.JournalPath), 0o700)
	}
	if err == nil {
		err = writeFileAtomic(w.JournalPath, append(b, '\n'), 0o600)
	}
	if err != nil {
		w.logf("journal %s: %v", w.JournalPath, err)
	}
}
