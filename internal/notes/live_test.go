package notes

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// These run against the real apps on this Mac, only when asked. They log
// counts and note ids, never note text.

const liveTitle = "Tincan backend test (safe to delete)"

func TestLiveBear(t *testing.T) {
	if os.Getenv("TINCAN_LIVE_BEAR") != "1" {
		t.Skip("set TINCAN_LIVE_BEAR=1 to run against Bear")
	}
	b := NewBear(t.TempDir(), t.Logf)
	n, err := b.Probe(t.Context())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	t.Logf("Bear has %d notes", n)
	found, err := b.Search(t.Context(), "the")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	t.Logf("search found %d notes", len(found))
	if len(found) > 0 {
		note, err := b.Read(t.Context(), found[0].ID)
		if err != nil && err != ErrNoteLocked {
			t.Fatalf("read %s: %v", found[0].ID, err)
		}
		t.Logf("read %s (state %s, %d bytes)", note.Summary.ID, note.Summary.State, len(note.Body))
	}

	key := "tincan:live-" + time.Now().Format("150405")
	res, err := b.Create(t.Context(), liveTitle, "Made by the tincan live test.", []string{"tincan-test"}, nil, key)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := res.Note.Summary.ID
	t.Logf("created %s", id)
	t.Cleanup(func() {
		u := "bear://x-callback-url/trash?id=" + url.PathEscape(id) + "&show_window=no"
		if err := b.openURL(context.Background(), u); err != nil {
			t.Errorf("trash %s: %v (delete the test note by hand)", id, err)
		}
	})
	again, err := b.Create(t.Context(), liveTitle, "Made by the tincan live test.", []string{"tincan-test"}, nil, key)
	if err != nil || !again.Existed || again.Note.Summary.ID != id {
		t.Fatalf("redelivered create = %s existed=%v, %v; want %s", again.Note.Summary.ID, again.Existed, err, id)
	}
}

func TestLiveAppleNotes(t *testing.T) {
	if os.Getenv("TINCAN_LIVE_APPLE_NOTES") != "1" {
		t.Skip("set TINCAN_LIVE_APPLE_NOTES=1 to run against Notes (asks for Automation access)")
	}
	a := NewAppleNotes(t.TempDir(), t.Logf)
	n, err := a.Probe(t.Context())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	t.Logf("Notes has %d notes", n)
	found, err := a.Search(t.Context(), "the")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	t.Logf("search found %d notes", len(found))
	if len(found) > 0 {
		note, err := a.Read(t.Context(), found[0].ID)
		if err != nil && err != ErrNoteLocked {
			t.Fatalf("read %s: %v", found[0].ID, err)
		}
		t.Logf("read %s (state %s, %d bytes)", note.Summary.ID, note.Summary.State, len(note.Body))
	}

	// Create, redeliver, read back and delete a test note of our own.
	key := fmt.Sprintf("tincan:live-%d", time.Now().UnixNano())
	res, err := a.Create(t.Context(), liveTitle, "made by TestLiveAppleNotes", nil, nil, key)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := res.Note.Summary.ID
	t.Cleanup(func() {
		if err := a.script(context.Background(), liveAppleDelete, map[string]string{"id": id}, &struct{}{}); err != nil {
			t.Logf("delete test note %s: %v (delete it by hand)", id, err)
		}
	})
	again, err := a.Create(t.Context(), liveTitle, "made by TestLiveAppleNotes", nil, nil, key)
	if err != nil || !again.Existed || again.Note.Summary.ID != id {
		t.Fatalf("redelivered create = %+v, %v; want the same note", again.Note.Summary, err)
	}
	got, err := a.Read(t.Context(), id)
	if err != nil || got.Summary.State != StateActive || !strings.Contains(got.Body, "made by TestLiveAppleNotes") {
		t.Fatalf("read back = %+v, %v", got.Summary, err)
	}
	t.Logf("created, deduped and read %s", id)
}

// liveAppleDelete moves the test note to Recently Deleted.
const liveAppleDelete = jxaPrelude + `
function run() {
  var a = input();
  Notes.delete(Notes.notes.byId(a.id));
  return JSON.stringify({});
}
`
