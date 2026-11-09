package store

import (
	"context"
	"math"
	"strings"
	"time"
	"unicode"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

const searchCandidateLimit = 2000

// searchVisibleWindow is how many of an agent's visible requests one search
// query covers; a search walks them in chunks of this size.
var searchVisibleWindow = 5000

const searchBackfillBatchSize = 500

func (s *Store) migrateSearch() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'requests_fts'`).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		_, err = tx.Exec(`CREATE TABLE IF NOT EXISTS search_backfill (id INTEGER PRIMARY KEY CHECK (id = 1), high_water INTEGER NOT NULL);
 INSERT OR IGNORE INTO search_backfill VALUES (1, 0);
 CREATE INDEX IF NOT EXISTS requests_trace ON requests(trace_id);`)
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	_, err = tx.Exec(`
 CREATE VIRTUAL TABLE requests_fts USING fts5(request_id UNINDEXED, trace_id UNINDEXED, body, reply_body);
 CREATE TABLE search_backfill (id INTEGER PRIMARY KEY CHECK (id = 1), high_water INTEGER NOT NULL);
 INSERT INTO search_backfill VALUES (1, 0);
 CREATE TRIGGER requests_search_insert AFTER INSERT ON requests BEGIN
   INSERT INTO requests_fts(rowid, request_id, trace_id, body, reply_body) VALUES (new.rowid, new.id, new.trace_id, new.body, '');
 END;
 CREATE TRIGGER replies_search_insert AFTER INSERT ON replies BEGIN
   UPDATE requests_fts SET reply_body = new.body WHERE rowid = (SELECT rowid FROM requests WHERE id = new.request_id);
 END;
 CREATE INDEX IF NOT EXISTS requests_trace ON requests(trace_id);
 `)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// backfillSearchBatch commits the index and its checkpoint together. Triggers
// may already have indexed rows beyond the checkpoint; replacing them is safe.
// A request whose clarification question was answered has no reply row, so
// its latest question is indexed as the reply text, as the insert trigger
// left it.
func (s *Store) backfillSearchBatch() (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var high, end int64
	if err := tx.QueryRow(`SELECT high_water FROM search_backfill WHERE id = 1`).Scan(&high); err != nil {
		return false, err
	}
	if err := tx.QueryRow(`SELECT COALESCE(MAX(rowid), ?) FROM
 (SELECT rowid FROM requests WHERE rowid > ? ORDER BY rowid LIMIT ?)`, high, high, searchBackfillBatchSize).Scan(&end); err != nil {
		return false, err
	}
	if end == high {
		return false, tx.Commit()
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO requests_fts(rowid, request_id, trace_id, body, reply_body)
 SELECT r.rowid, r.id, r.trace_id, r.body, COALESCE(p.body, json_extract(r.exchanges, '$[#-1].question'), '') FROM requests r
 LEFT JOIN replies p ON p.request_id = r.id WHERE r.rowid > ? AND r.rowid <= ?`, high, end); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`UPDATE search_backfill SET high_water = ? WHERE id = 1`, end); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// Search finds request and reply text, newest requests first. An empty
// participant searches everything; otherwise only its chains are visible.
func (s *Store) Search(ctx context.Context, query, participant string, limit int) ([]envelope.SearchResult, error) {
	return s.SearchAs(ctx, query, participant, true, limit)
}

// SearchAs is Search for a participant that may (rooms) or may not read
// rooms at all; one rooms exclude finds no ask built from a mention, in
// any room.
func (s *Store) SearchAs(ctx context.Context, query, participant string, rooms bool, limit int) ([]envelope.SearchResult, error) {
	out := []envelope.SearchResult{}
	terms := strings.FieldsFunc(query, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.IsMark(r)
	})
	if len(terms) == 0 {
		return out, nil
	}
	for i, term := range terms {
		terms[i] = `"` + strings.ReplaceAll(term, `"`, `""`) + `"`
	}
	if limit <= 0 {
		limit = 20
	}
	limit = min(limit, 50)
	match := strings.Join(terms, " AND ")
	if participant == "" {
		// Admins search the whole index, newest searchCandidateLimit matches.
		return s.searchRows(ctx, `SELECT rowid,`+searchExcerpts+` FROM requests_fts
 WHERE requests_fts MATCH ? ORDER BY rowid DESC LIMIT ?`, limit, match, searchCandidateLimit)
	}
	// An agent searches only its own chains, never a request held for the
	// owner and not approved unless it sent it (see RedactFor), never a
	// pass seed it neither sent nor received (see RedactPassFor), and never
	// an ask built from a room mention unless it may read rooms and is now
	// a member of that room, whether it sent the ask, got it or only shares
	// the chain (see RedactRoom). Walk them newest first in chunks of
	// searchVisibleWindow requests, one short query per chunk, so matches
	// it cannot see are never examined, the store's single connection is
	// released between chunks, and older matches are still reached when the
	// newer chunks do not fill the limit.
	below := int64(math.MaxInt64)
	for {
		var chunkLow int64
		var chunkRows int
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MIN(rowid), 0), COUNT(*) FROM (
 SELECT r.rowid FROM requests r WHERE r.rowid < ? AND r.trace_id IN (
   SELECT trace_id FROM requests WHERE from_agent = ?
   UNION SELECT trace_id FROM requests WHERE to_agent = ? AND NOT (was_held = 1 AND approved = 0 AND follow_of != '')
 ) ORDER BY r.rowid DESC LIMIT ?)`, below, participant, participant, searchVisibleWindow).Scan(&chunkLow, &chunkRows); err != nil {
			return nil, err
		}
		if chunkRows == 0 {
			break
		}
		hits, err := s.searchRows(ctx, `SELECT rowid,`+searchExcerpts+` FROM requests_fts
 WHERE requests_fts MATCH ? AND rowid >= ? AND rowid < ? AND rowid IN (
   SELECT r.rowid FROM requests r WHERE r.rowid >= ? AND r.rowid < ?
     AND NOT (r.was_held = 1 AND r.approved = 0 AND r.from_agent != ?)
     AND NOT (substr(r.group_id, 1, ?) = ? AND r.from_agent != ? AND r.to_agent != ?)
     AND (r.room_post_id IS NULL OR ? AND EXISTS (SELECT 1 FROM room_posts p JOIN room_members m ON m.room_id = p.room_id AND m.agent = ? WHERE p.id = r.room_post_id))
     AND r.trace_id IN (
     SELECT trace_id FROM requests WHERE from_agent = ?
     UNION SELECT trace_id FROM requests WHERE to_agent = ? AND NOT (was_held = 1 AND approved = 0 AND follow_of != '')
   ))
 ORDER BY rowid DESC LIMIT ?`, limit-len(out), match, chunkLow, below, chunkLow, below, participant,
			len(envelope.PassGroupPrefix), envelope.PassGroupPrefix, participant, participant, rooms, participant, participant, participant, limit-len(out))
		if err != nil {
			return nil, err
		}
		out = append(out, hits...)
		if len(out) >= limit || chunkRows < searchVisibleWindow {
			break
		}
		below = chunkLow
	}
	// Chunks are walked newest first by insertion order, so out is already
	// in the order results are returned: newest request first.
	return out, nil
}

// searchExcerpts selects an excerpt of each body only when that body matched.
const searchExcerpts = `
 CASE WHEN highlight(requests_fts, 2, '[', ']') != body THEN snippet(requests_fts, 2, '[', ']', '…', 24) ELSE '' END AS snippet,
 CASE WHEN highlight(requests_fts, 3, '[', ']') != reply_body THEN snippet(requests_fts, 3, '[', ']', '…', 24) ELSE '' END AS reply_snippet`

// searchRows runs candidates (rowid, snippet, reply_snippet from requests_fts)
// and returns at most limit results, newest request first.
func (s *Store) searchRows(ctx context.Context, candidates string, limit int, args ...any) ([]envelope.SearchResult, error) {
	out := []envelope.SearchResult{}
	// The index keeps the last text stored as the reply. A clarification
	// question stays there after it is answered (its reply row is removed)
	// or after the request expires while waiting, so only a final reply's
	// match is a reply excerpt; any other is the question's.
	rows, err := s.db.QueryContext(ctx, `SELECT r.id, r.trace_id, r.from_agent, r.to_agent, r.status, r.created_at,
 c.snippet, c.reply_snippet, p.request_id IS NULL OR p.status = 'needs_input', r.attachments, COALESCE(p.attachments, '')
 FROM (`+candidates+`) c JOIN requests r ON r.rowid = c.rowid
 LEFT JOIN replies p ON p.request_id = r.id
 ORDER BY r.rowid DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var hit envelope.SearchResult
		var created int64
		var requestAttachments, replyAttachments string
		var question bool
		if err := rows.Scan(&hit.RequestID, &hit.TraceID, &hit.From, &hit.To, &hit.Status, &created, &hit.Snippet, &hit.ReplySnippet, &question, &requestAttachments, &replyAttachments); err != nil {
			return nil, err
		}
		if question {
			hit.QuestionSnippet, hit.ReplySnippet = hit.ReplySnippet, ""
		}
		hit.CreatedAt = time.UnixMilli(created).UTC()
		for _, raw := range []string{requestAttachments, replyAttachments} {
			attachments, err := decodeAttachments(raw)
			if err != nil {
				return nil, err
			}
			for _, attachment := range attachments {
				hit.AttachmentNames = append(hit.AttachmentNames, attachment.Name)
			}
		}
		out = append(out, hit)
	}
	return out, rows.Err()
}
