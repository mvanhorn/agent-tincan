package notes

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The Agent Notes helper matches a multi-word query only when a note holds
// every word, so a natural-language request that names one word too many
// finds nothing. When that happens the notes agent searches the words one
// by one and ranks notes by how many of them they hold.

const (
	// fallbackMaxWords caps the per-word searches; later words are named
	// in the reply as not searched.
	fallbackMaxWords = 12
	// fallbackBudget bounds all per-word searches, inside the request's
	// own timeout.
	fallbackBudget = 90 * time.Second
	// fallbackMinRunes drops words too short to mean anything as a
	// substring.
	fallbackMinRunes = 3
)

// fallbackStopwords are common words of three or more letters that would
// match nearly every note.
var fallbackStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "from": true, "that": true, "this": true,
	"what": true, "which": true, "are": true, "was": true, "were": true, "you": true, "your": true,
	"our": true, "any": true, "all": true, "has": true, "have": true, "had": true, "not": true,
	"but": true, "can": true, "about": true, "into": true, "note": true, "notes": true,
	"find": true, "show": true, "please": true, "their": true, "there": true, "them": true,
}

// fallbackWords splits query into the distinct lowercase words worth
// searching alone, in query order: at most fallbackMaxWords, the rest
// returned as skipped.
func fallbackWords(query string) (words, skipped []string) {
	seen := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if utf8.RuneCountInString(w) < fallbackMinRunes || fallbackStopwords[w] || seen[w] {
			continue
		}
		seen[w] = true
		if len(words) < fallbackMaxWords {
			words = append(words, w)
		} else {
			skipped = append(skipped, w)
		}
	}
	return words, skipped
}

// fallbackHit is one note a per-word search found.
type fallbackHit struct {
	Note       NoteSummary
	Words      []string // the searched words it holds, in query order
	TitleWords int      // how many of them are in its title
}

// rankFallback merges per-word results (searched words in query order,
// each with what its search returned) into active notes ranked by words
// matched, then words in the title, then pinned, then id.
func rankFallback(searched []string, found map[string][]NoteSummary) []fallbackHit {
	byID := map[string]*fallbackHit{}
	var order []string
	for _, w := range searched {
		for _, n := range found[w] {
			if n.State != StateActive {
				continue
			}
			h := byID[n.ID]
			if h == nil {
				h = &fallbackHit{Note: n}
				byID[n.ID] = h
				order = append(order, n.ID)
			}
			if slices.Contains(h.Words, w) {
				continue
			}
			h.Words = append(h.Words, w)
			if strings.Contains(strings.ToLower(n.Title), w) {
				h.TitleWords++
			}
		}
	}
	hits := make([]fallbackHit, 0, len(order))
	for _, id := range order {
		hits = append(hits, *byID[id])
	}
	slices.SortStableFunc(hits, func(a, b fallbackHit) int {
		switch {
		case len(a.Words) != len(b.Words):
			return len(b.Words) - len(a.Words)
		case a.TitleWords != b.TitleWords:
			return b.TitleWords - a.TitleWords
		case a.Note.Pinned != b.Note.Pinned:
			if a.Note.Pinned {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Note.ID, b.Note.ID)
	})
	return hits
}

// fallbackResult is what the per-word searches found and what they could
// not cover.
type fallbackResult struct {
	Hits     []fallbackHit
	Searched []string
	Skipped  []string // past the word cap
	TimedOut []string // not searched before the budget ran out
	Failed   []string // the helper failed on them
}

// searchFallback searches each word of query alone within the service's
// fallback budget.
func (s *Service) searchFallback(ctx context.Context, query string) fallbackResult {
	words, skipped := fallbackWords(query)
	res := fallbackResult{Skipped: skipped}
	bctx, cancel := context.WithTimeout(ctx, s.fallbackBudget)
	defer cancel()
	found := map[string][]NoteSummary{}
	for i, w := range words {
		if bctx.Err() != nil {
			res.TimedOut = append(res.TimedOut, words[i:]...)
			break
		}
		notes, err := s.helper.Search(bctx, w)
		if err != nil {
			if bctx.Err() != nil {
				res.TimedOut = append(res.TimedOut, words[i:]...)
				break
			}
			res.Failed = append(res.Failed, w)
			continue
		}
		found[w] = notes
		res.Searched = append(res.Searched, w)
	}
	res.Hits = rankFallback(res.Searched, found)
	return res
}

// fallbackReply says plainly that no note held every word, then lists the
// closest notes with the words each holds, and names any word left out.
func fallbackReply(query string, res fallbackResult, count int) string {
	q := oneLine(query)
	var b strings.Builder
	switch {
	case len(res.Searched) == 0:
		fmt.Fprintf(&b, "No note contains every word of %q, and the search for each word on its own could not run.", q)
	case len(res.Hits) == 0:
		fmt.Fprintf(&b, "No notes match %q, and no note contains any of its words on its own.", q)
	default:
		hits := res.Hits[:min(len(res.Hits), count)]
		fmt.Fprintf(&b, "No note contains every word of %q. Closest matches, by how many of the %d searched words each contains:\n", q, len(res.Searched))
		for i, h := range hits {
			title := oneLine(h.Note.Title)
			if title == "" {
				title = "(untitled)"
			}
			fmt.Fprintf(&b, "\n%d. %q (id %s), matches %d of %d words: %s\n", i+1, title, h.Note.ID, len(h.Words), len(res.Searched), strings.Join(h.Words, ", "))
			if len(h.Note.Tags) > 0 {
				fmt.Fprintf(&b, "   Tags: %s\n", oneLine(strings.Join(h.Note.Tags, ", ")))
			}
			if sn := oneLine(h.Note.Snippet); sn != "" {
				fmt.Fprintf(&b, "   %s\n", capRunes(sn, 300))
			}
		}
		if len(res.Hits) > len(hits) {
			fmt.Fprintf(&b, "\n%d more notes contain some of the words.\n", len(res.Hits)-len(hits))
		}
	}
	out := strings.TrimRight(b.String(), "\n")
	sep := "\n"
	for _, ns := range []struct {
		why   string
		words []string
	}{
		{fmt.Sprintf("past the %d-word limit", fallbackMaxWords), res.Skipped},
		{"out of time", res.TimedOut},
		{"the Agent Notes helper failed", res.Failed},
	} {
		if len(ns.words) > 0 {
			out += fmt.Sprintf("%s\nNot searched (%s): %s.", sep, ns.why, strings.Join(ns.words, ", "))
			sep = ""
		}
	}
	if len(out) > maxReplyBytes {
		const notice = "\n(reply truncated)"
		out = capBytes(out, maxReplyBytes-len(notice)) + notice
	}
	return out
}
