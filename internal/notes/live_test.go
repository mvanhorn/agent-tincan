package notes

import (
	"context"
	"net/url"
	"os"
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
}
