package history

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/png"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// fixtureNow is "today" for every fixture test.
var fixtureNow = time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

const fixtureScratch = "/Users/matt/.config/tincan/history-scratch"

// copyTree copies testdata/<name> into a temp dir so tests can set mtimes.
func copyTree(t *testing.T, name string) string {
	t.Helper()
	src := filepath.Join("testdata", name)
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func setMtime(t *testing.T, path, ts string) {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, tm, tm); err != nil {
		t.Fatal(err)
	}
}

var testPNG = func() []byte {
	var buf bytes.Buffer
	m := image.NewRGBA(image.Rect(0, 0, 1, 1))
	m.Set(0, 0, color.RGBA{R: 255, A: 255})
	if err := png.Encode(&buf, m); err != nil {
		panic(err)
	}
	return buf.Bytes()
}()

// colorName names the single pixel of a fixture PNG so tests can say which
// fixture image came back.
func colorName(t *testing.T, img Image) string {
	t.Helper()
	m, err := png.Decode(bytes.NewReader(img.Data))
	if err != nil {
		t.Fatalf("decode image: %v", err)
	}
	r, g, b, _ := m.At(0, 0).RGBA()
	names := map[[3]uint32]string{
		{255, 0, 0}: "red", {0, 0, 255}: "blue", {0, 255, 0}: "green",
		{255, 255, 0}: "yellow", {128, 0, 128}: "purple", {255, 128, 0}: "orange",
	}
	if n, ok := names[[3]uint32{r >> 8, g >> 8, b >> 8}]; ok {
		return n
	}
	return "unknown"
}

func colors(t *testing.T, imgs []Image) string {
	t.Helper()
	var out []string
	for _, im := range imgs {
		out = append(out, colorName(t, im))
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func TestQueryValidate(t *testing.T) {
	good := []Query{
		{Source: SourceCodex, Mode: ModeLatest},
		{Source: SourceClaudeCode, Mode: ModeSearch, Terms: []string{"fox", "logo"}, Count: 5},
		{Source: SourceChatGPT, Mode: ModeConversation, ConversationID: "01a0c000-0000-7000-8000-00000000000a"},
		{Source: SourceClaudeAI, Mode: ModeLatest, WantImages: true},
	}
	for _, q := range good {
		if err := q.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v, want nil", q, err)
		}
	}
	bad := map[string]Query{
		"unknown source":       {Source: "gemini", Mode: ModeLatest},
		"unknown mode":         {Source: SourceCodex, Mode: "delete"},
		"search without terms": {Source: SourceCodex, Mode: ModeSearch},
		"conversation no id":   {Source: SourceCodex, Mode: ModeConversation},
		"path in id":           {Source: SourceCodex, Mode: ModeConversation, ConversationID: "../../etc/passwd"},
		"count too big":        {Source: SourceCodex, Mode: ModeLatest, Count: MaxCount + 1},
		"negative count":       {Source: SourceCodex, Mode: ModeLatest, Count: -1},
		"too many terms":       {Source: SourceCodex, Mode: ModeSearch, Terms: strings.Fields("a b c d e f g h i")},
		"term too long":        {Source: SourceCodex, Mode: ModeSearch, Terms: []string{strings.Repeat("x", MaxTermLen+1)}},
		"blank term":           {Source: SourceCodex, Mode: ModeSearch, Terms: []string{"  "}},
	}
	for name, q := range bad {
		if err := q.Validate(); err == nil {
			t.Errorf("%s: Validate(%+v) = nil, want error", name, q)
		}
	}
}

func TestWindowAdmit(t *testing.T) {
	w := Window{Max: 2, MaxAge: 30 * 24 * time.Hour}
	now := fixtureNow
	if !w.Admit(0, now.Add(-time.Hour), now) {
		t.Fatal("recent first conversation should be admitted")
	}
	if w.Admit(2, now.Add(-time.Hour), now) {
		t.Fatal("third conversation should be outside a window of 2")
	}
	if w.Admit(0, now.Add(-31*24*time.Hour), now) {
		t.Fatal("31 day old conversation should be outside the 30 day cap")
	}
	d := DefaultWindow()
	if d.Max != 50 || d.MaxAge != 30*24*time.Hour {
		t.Fatalf("DefaultWindow = %+v", d)
	}
	var zero Window
	if zero.orDefault() != d {
		t.Fatal("zero Window should mean the default window")
	}
}

func TestSaveImagesWritesContentAddressedPrivateFiles(t *testing.T) {
	data := testPNG
	dir := filepath.Join(t.TempDir(), "imgs", "nested")
	convs := []Conversation{{Messages: []Message{{Role: RoleUser, Images: []Image{{Data: data}}}}}}
	if err := SaveImages(dir, convs); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v, want 0700", st.Mode().Perm())
	}
	img := convs[0].Messages[0].Images[0]
	sum := sha256.Sum256(data)
	want := filepath.Join(dir, hex.EncodeToString(sum[:])+".png")
	if img.Path != want {
		t.Fatalf("Path = %q, want %q", img.Path, want)
	}
	if img.MIME != "image/png" || img.SHA256 != hex.EncodeToString(sum[:]) || img.Size != int64(len(data)) {
		t.Fatalf("image metadata = %+v", img)
	}
	fst, err := os.Stat(want)
	if err != nil {
		t.Fatal(err)
	}
	if fst.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v, want 0600", fst.Mode().Perm())
	}
	got, _ := os.ReadFile(want)
	if !bytes.Equal(got, data) {
		t.Fatal("saved bytes differ")
	}
	// Saving again is idempotent.
	if err := SaveImages(dir, convs); err != nil {
		t.Fatalf("second save: %v", err)
	}
}

func TestSaveImagesRefusesSymlinkAtTarget(t *testing.T) {
	dir := t.TempDir()
	sum := sha256.Sum256(testPNG)
	outside := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, hex.EncodeToString(sum[:])+".png")); err != nil {
		t.Fatal(err)
	}
	convs := []Conversation{{Messages: []Message{{Images: []Image{{Data: testPNG}}}}}}
	if err := SaveImages(dir, convs); err == nil {
		t.Fatal("SaveImages over a symlink = nil, want error")
	}
	if b, _ := os.ReadFile(outside); string(b) != "keep" {
		t.Fatal("symlink target was modified")
	}
}

func TestNewImageRejectsNonAllowlistedData(t *testing.T) {
	if _, ok := newImage([]byte("#!/bin/sh\necho hi\n"), "script.png"); ok {
		t.Fatal("shell script accepted as an image")
	}
	if _, ok := newImage(nil, "empty.png"); ok {
		t.Fatal("empty data accepted")
	}
	big := make([]byte, MaxImageBytes+1)
	copy(big, testPNG)
	if _, ok := newImage(big, "big.png"); ok {
		t.Fatal("oversized image accepted")
	}
	img, ok := newImage(testPNG, "a.png")
	if !ok || img.MIME != "image/png" {
		t.Fatalf("png rejected: %+v %v", img, ok)
	}
}

func TestDecodeDataURL(t *testing.T) {
	if _, ok := decodeDataURL("data:image/png;base64,!!!notbase64"); ok {
		t.Fatal("bad base64 accepted")
	}
	if _, ok := decodeDataURL("https://example.com/x.png"); ok {
		t.Fatal("remote URL accepted")
	}
}

func TestScanJSONLSkipsMalformedAndHugeLinesAndStopsEarly(t *testing.T) {
	in := "{\"a\":1}\nnot json\n{\"a\":2}\n" + strings.Repeat("x", 300) + "\n{\"a\":3}\n{\"a\":4}\n"
	var seen []string
	err := scanJSONL(strings.NewReader(in), 200, func(line []byte) bool {
		seen = append(seen, string(line))
		return len(seen) < 4
	})
	if err != nil {
		t.Fatal(err)
	}
	// The 300 byte line is over the 200 byte cap and is dropped; scanning
	// stops after the fourth delivered line.
	want := []string{`{"a":1}`, "not json", `{"a":2}`, `{"a":3}`}
	if strings.Join(seen, "|") != strings.Join(want, "|") {
		t.Fatalf("seen = %q", seen)
	}
}
