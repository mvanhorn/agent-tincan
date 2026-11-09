package notes

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
)

// incidentQuery has the shape of the request that found nothing: two
// words the family note lacks, among eight it holds. The names are
// invented.
const incidentQuery = "Alaska Airlines Mileage Plan Anya Bram Cleo Dara Ezra Finn"

// putIncidentLibrary holds the family note (the numbers are invented), a
// second note with the same eight words in its body only, and an
// unrelated one.
func putIncidentLibrary(t *testing.T, h *fakeHelper) (family, second fakeNote) {
	t.Helper()
	family = h.put(t, fakeNote{Title: "Family Alaska Airlines Numbers", Body: "Anya 000111222\nBram 000111333\nCleo 000111444\nDara 000111555\nEzra 000111666\nFinn 000111777", Tags: []string{"travel"}})
	second = h.put(t, fakeNote{Title: "Trip ideas", Body: "alaska airlines with anya, bram, cleo, dara, ezra and finn in june"})
	h.put(t, fakeNote{Title: "Groceries", Body: "milk and bread"})
	return family, second
}

// The incident query finds the family note first,
// says no note held every word, and names the words it matched.
func TestIncidentQueryFindsFamilyNoteFirst(t *testing.T) {
	r := newRig(t, relay.Config{})
	family, second := putIncidentLibrary(t, r.helper)
	res := r.ask(t, "grokbot", `note: {"op":"search","query":"`+incidentQuery+`"}`)
	body := res.Reply.Body
	if res.Status != envelope.StatusAnswered {
		t.Fatalf("status %s", res.Status)
	}
	if !strings.HasPrefix(body, `No note contains every word of "`+incidentQuery+`"`) {
		t.Fatalf("reply does not say no note held every word:\n%s", body)
	}
	first := `1. "Family Alaska Airlines Numbers" (id ` + family.ID + `), matches 8 of 10 words: alaska, airlines, anya, bram, cleo, dara, ezra, finn`
	if !strings.Contains(body, first) {
		t.Fatalf("family note not first with its words:\n%s", body)
	}
	if !strings.Contains(body, `2. "Trip ideas" (id `+second.ID+`), matches 8 of 10 words`) {
		t.Fatalf("second note not second:\n%s", body)
	}
	if strings.Contains(body, "Groceries") {
		t.Fatalf("a note with none of the words is listed:\n%s", body)
	}
	if c := len(r.helper.callsOf(t, "search")); c != 11 {
		t.Fatalf("searches = %d, want the full query and 10 words", c)
	}
	r.lineWith(t, res.Reply.RequestID, "search fallback finished")
}

// The query that matches in full gets the normal reply and no per-word
// searches.
func TestFullMatchRunsNoFallback(t *testing.T) {
	r := newRig(t, relay.Config{})
	family, _ := putIncidentLibrary(t, r.helper)
	res := r.ask(t, "grokbot", `note: {"op":"search","query":"Alaska Airlines Numbers"}`)
	if !strings.HasPrefix(res.Reply.Body, `1 note matches "Alaska Airlines Numbers"`) || !strings.Contains(res.Reply.Body, family.ID) {
		t.Fatalf("reply %q", res.Reply.Body)
	}
	if c := len(r.helper.callsOf(t, "search")); c != 1 {
		t.Fatalf("searches = %d", c)
	}
}

// A one-word query that matches nothing is answered as before.
func TestSingleWordMissRunsNoFallback(t *testing.T) {
	r := newRig(t, relay.Config{})
	putIncidentLibrary(t, r.helper)
	res := r.ask(t, "grokbot", `note: {"op":"search","query":"Atmos"}`)
	if res.Reply.Body != `No notes match "Atmos".` {
		t.Fatalf("reply %q", res.Reply.Body)
	}
	if c := len(r.helper.callsOf(t, "search")); c != 1 {
		t.Fatalf("searches = %d", c)
	}
}

// No word found anywhere: said plainly.
func TestFallbackWithNoWordFound(t *testing.T) {
	r := newRig(t, relay.Config{})
	putIncidentLibrary(t, r.helper)
	res := r.ask(t, "grokbot", `note: {"op":"search","query":"zebra quokka"}`)
	if res.Reply.Body != `No notes match "zebra quokka", and no note contains any of its words on its own.` {
		t.Fatalf("reply %q", res.Reply.Body)
	}
}

// Trashed and archived notes found by a per-word search are not listed.
func TestFallbackListsActiveNotesOnly(t *testing.T) {
	r := newRig(t, relay.Config{})
	gone := r.helper.put(t, fakeNote{Title: "Old alaska trip", Body: "alaska", State: "trashed"})
	kept := r.helper.put(t, fakeNote{Title: "Packing", Body: "boots"})
	res := r.ask(t, "grokbot", `note: {"op":"search","query":"alaska boots"}`)
	if strings.Contains(res.Reply.Body, gone.ID) || !strings.Contains(res.Reply.Body, kept.ID) {
		t.Fatalf("reply %q", res.Reply.Body)
	}
}

// Words the budget did not reach are named.
func TestFallbackOutOfTimeNamesUnsearchedWords(t *testing.T) {
	// "tent" answers at once and "stove" runs until the budget ends, so
	// the outcome does not depend on how loaded the machine is.
	const id = "0F6A2C1E-3B7D-4E0A-9C21-5D8E7F6A1B2C"
	b := &stubBackend{name: "Agent Notes", search: func(ctx context.Context, q string) ([]NoteSummary, error) {
		switch q {
		case "tent":
			return []NoteSummary{{ID: id, Title: "Tent", State: StateActive}}, nil
		case "stove", "lantern":
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return nil, nil
	}}
	r := rigWith(t, b)
	r.svc.fallbackBudget = 300 * time.Millisecond
	res := r.ask(t, "grokbot", `note: {"op":"search","query":"tent stove lantern"}`)
	body := res.Reply.Body
	if !strings.Contains(body, id) || !strings.Contains(body, "Not searched (out of time): ") || !strings.Contains(body, "stove") || !strings.Contains(body, "lantern") {
		t.Fatalf("reply %q", body)
	}
}

func TestFallbackWords(t *testing.T) {
	words, skipped := fallbackWords("Find the Tent, tent and my STOVE for us: a b c one two three four five six seven eight nine ten")
	want := []string{"tent", "stove", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten"}
	if !slices.Equal(words, want) {
		t.Fatalf("words = %q, want %q", words, want)
	}
	if !slices.Equal(skipped, nil) {
		t.Fatalf("skipped = %q", skipped)
	}
	_, skipped = fallbackWords("alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima mike")
	if !slices.Equal(skipped, []string{"mike"}) {
		t.Fatalf("skipped = %q, want the 13th word", skipped)
	}
}

func TestFallbackReplyNamesWordsPastTheCap(t *testing.T) {
	body := fallbackReply("q", fallbackResult{Searched: []string{"a"}, Skipped: []string{"mike"}, Hits: []fallbackHit{{Note: NoteSummary{ID: "x", Title: "T", State: StateActive}, Words: []string{"a"}}}}, 10)
	if !strings.Contains(body, "Not searched (past the 12-word limit): mike.") {
		t.Fatalf("reply %q", body)
	}
}

// Ties order by title words, then pinned, then id.
func TestRankFallbackTieBreaks(t *testing.T) {
	a := NoteSummary{ID: "a", Title: "nothing here", State: StateActive}
	b := NoteSummary{ID: "b", Title: "nothing here", State: StateActive, Pinned: true}
	c := NoteSummary{ID: "c", Title: "tent", State: StateActive}
	d := NoteSummary{ID: "d", Title: "nothing", State: StateActive}
	e := NoteSummary{ID: "e", Title: "tent", State: "archived"}
	hits := rankFallback([]string{"tent", "stove"}, map[string][]NoteSummary{
		"tent":  {d, a, b, c, e},
		"stove": {a, b, c},
	})
	var ids []string
	for _, h := range hits {
		ids = append(ids, h.Note.ID)
	}
	if !slices.Equal(ids, []string{"c", "b", "a", "d"}) {
		t.Fatalf("order = %q", ids)
	}
}

// When no word could be searched on its own, the reply says so and the
// request fails: nothing is known about the library.
func TestFallbackThatSearchedNothingFails(t *testing.T) {
	r := newRig(t, relay.Config{})
	r.helper.put(t, fakeNote{Title: "Tent", Body: "tent"})
	r.svc.fallbackBudget = time.Nanosecond
	res := r.ask(t, "grokbot", `note: {"op":"search","query":"stove lantern"}`)
	if res.Status != envelope.StatusFailed {
		t.Fatalf("status %s, want failed", res.Status)
	}
	body := res.Reply.Body
	if !strings.HasPrefix(body, `No note contains every word of "stove lantern", and the search for each word on its own could not run.`) ||
		!strings.Contains(body, "Not searched (out of time): stove, lantern.") {
		t.Fatalf("reply %q", body)
	}
}

// A fallback that searched some words but not all says only that the
// searched ones matched nothing.
func TestFallbackWithUnsearchedWordsClaimsOnlyWhatItSearched(t *testing.T) {
	body := fallbackReply("stove lantern", fallbackResult{Searched: []string{"stove"}, TimedOut: []string{"lantern"}}, 10)
	want := "No note contains every word of \"stove lantern\", and none of the words searched on their own matched a note.\n\nNot searched (out of time): lantern."
	if body != want {
		t.Fatalf("reply %q, want %q", body, want)
	}
}

// When the helper fails on the fallback's searches, doctor's health shows
// that failure, not the earlier successful full search.
func TestFallbackHelperFailureIsRecordedInHealth(t *testing.T) {
	r := newRig(t, relay.Config{})
	r.helper.failWordSearches(t, "helper_crashed")
	res := r.ask(t, "grokbot", `note: {"op":"search","query":"stove lantern"}`)
	if res.Status != envelope.StatusFailed {
		t.Fatalf("status %s", res.Status)
	}
	if h := readHealth(t, r.cfg.HealthPath); h.OK || h.Command != "search" {
		t.Fatalf("health %+v, want the fallback's failure", h)
	}
}
