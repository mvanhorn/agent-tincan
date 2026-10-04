package onboard

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/identity"
)

// The stock map's key set is pinned by explicit lists rather than derived
// from isService: every service kind and every product-tool kind has a line,
// and hosting shapes and owner-configured frameworks have none. A new service
// kind without a line fails here.
func TestStockGoodAtCoversServiceAndProductKinds(t *testing.T) {
	products := []string{KindClaudeCode, KindCodex, KindGeminiCLI, KindGrokCLI, KindChatGPT}
	none := []string{KindVMWebhook, KindE2BEmail, KindProxySandbox, KindScheduled, KindGeneric, KindHermes, KindOpenClaw}
	want := slices.Clone(products)
	for _, k := range Kinds {
		if isService(k) {
			want = append(want, k)
			if StockGoodAt("", k) == "" {
				t.Errorf("service kind %s has no stock line", k)
			}
		}
	}
	for _, k := range products {
		if StockGoodAt("", k) == "" {
			t.Errorf("product kind %s has no stock line", k)
		}
	}
	for _, k := range none {
		if g := StockGoodAt("", k); g != "" {
			t.Errorf("kind %s has stock line %q, want none", k, g)
		}
	}
	var keys []string
	for k := range stockGoodAt {
		keys = append(keys, k)
	}
	slices.Sort(want)
	slices.Sort(keys)
	if !slices.Equal(keys, want) {
		t.Fatalf("stock line keys %v, want %v", keys, want)
	}
	if StockGoodAt("", "") != "" {
		t.Fatal("an agent with no kind and no product name has a stock line")
	}
}

// The lines that were wrong now name what the kind actually does.
func TestStockGoodAtLineContent(t *testing.T) {
	history := StockGoodAt("", KindHistory)
	for _, s := range []string{"ChatGPT", "claude.ai", "Claude Code", "Grok CLI", "images"} {
		if !strings.Contains(history, s) {
			t.Errorf("history line %q lacks %q", history, s)
		}
	}
	claude := StockGoodAt("", KindClaudeWeb)
	if !strings.Contains(claude, "no images") || strings.Contains(claude, "generated images") {
		t.Errorf("claude-web line = %q", claude)
	}
}

// With no stored kind, a product runtime name picks the line; a stored kind
// wins over the name, and a personal name gets no line.
func TestStockGoodAtByName(t *testing.T) {
	if got, want := StockGoodAt("codex", ""), stockGoodAt[KindCodex]; got != want || got == "" {
		t.Errorf("codex with no kind = %q, want %q", got, want)
	}
	if got := StockGoodAt("muse", ""); got != "" {
		t.Errorf("muse with no kind = %q, want none", got)
	}
	if got := StockGoodAt("codex", KindVMWebhook); got != "" {
		t.Errorf("codex stored as vm-webhook = %q, want none", got)
	}
}

// Every stock line is one the relay would accept from the owner, stored
// unchanged, and follows the output hygiene rules.
func TestStockGoodAtLinesPassValidationAndHygiene(t *testing.T) {
	ctx := context.Background()
	store := identity.NewMemoryStore()
	dir := identity.NewDirectory(store, nil, identity.Config{})
	for kind, line := range stockGoodAt {
		if err := store.PutAgent(ctx, identity.Agent{Name: kind, Kind: kind}); err != nil {
			t.Fatal(err)
		}
		if _, err := dir.SetGoodAt(ctx, identity.LocalAdmin, kind, line); err != nil {
			t.Errorf("%s stock line refused: %v", kind, err)
			continue
		}
		if a, _, _ := store.AgentByName(ctx, kind); a.GoodAt != line {
			t.Errorf("%s stock line stored as %q, want %q unchanged", kind, a.GoodAt, line)
		}
		for _, bad := range []string{"—", "–", "**"} {
			if strings.Contains(line, bad) {
				t.Errorf("%s stock line contains forbidden %q: %q", kind, bad, line)
			}
		}
	}
}
