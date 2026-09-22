package dashboard

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Oluiy/ai-cost-guard/internal/logging"
)

// csvSafe defuses CSV/formula injection (CWE-1236) in the report export —
// `model` in particular is fully attacker-controlled (any
// /v1/chat/completions caller sets it), so this is a real, not
// hypothetical, injection vector into whatever spreadsheet tool an admin
// opens the exported report with.
func TestCSVSafe_PrefixesFormulaTriggerCharacters(t *testing.T) {
	for _, in := range []string{
		"=cmd|' /C calc'!A0",
		"+1+1",
		"-1+1",
		"@SUM(1+1)",
	} {
		got := csvSafe(in)
		if got == in {
			t.Errorf("csvSafe(%q) left the value unescaped, want a leading quote", in)
		}
		if got[0] != '\'' {
			t.Errorf("csvSafe(%q) = %q, want it prefixed with a single quote", in, got)
		}
		if got[1:] != in {
			t.Errorf("csvSafe(%q) = %q, want original value preserved after the prefix", in, got)
		}
	}
}

func TestCSVSafe_LeavesOrdinaryValuesUnchanged(t *testing.T) {
	for _, in := range []string{"gpt-4o", "user_123", "claude-3-haiku", ""} {
		if got := csvSafe(in); got != in {
			t.Errorf("csvSafe(%q) = %q, want unchanged", in, got)
		}
	}
}

func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	store, err := logging.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("logging.Open failed: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return &Handler{Store: store}
}

// TestModelsForProvider is the fix for filtering cost by provider: since
// requests carry only a model name, not a provider column, this is the
// resolution step — every logged model that actually routes to the
// requested provider (see cost.ProviderFor), used as the "model IN (...)"
// set passed into the logging queries.
func TestModelsForProvider(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()
	now := time.Now().UTC()

	h.Store.Insert(ctx, logging.Record{Timestamp: now, UserID: "alice", Model: "gpt-4o"})
	h.Store.Insert(ctx, logging.Record{Timestamp: now, UserID: "alice", Model: "claude-sonnet-5"})
	h.Store.Insert(ctx, logging.Record{Timestamp: now, UserID: "alice", Model: "claude-haiku-4-5"})

	models, err := h.modelsForProvider(ctx, "anthropic")
	if err != nil {
		t.Fatalf("modelsForProvider failed: %v", err)
	}
	want := map[string]bool{"claude-sonnet-5": true, "claude-haiku-4-5": true}
	if len(models) != len(want) {
		t.Fatalf("got %v, want exactly %v", models, want)
	}
	for _, m := range models {
		if !want[m] {
			t.Errorf("modelsForProvider(anthropic) included unexpected model %q", m)
		}
	}

	if models, err := h.modelsForProvider(ctx, ""); err != nil || models != nil {
		t.Errorf("modelsForProvider(\"\") = (%v, %v), want (nil, nil) — empty provider means unfiltered", models, err)
	}
}

func TestDistinctProviders(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()
	now := time.Now().UTC()

	h.Store.Insert(ctx, logging.Record{Timestamp: now, UserID: "alice", Model: "gpt-4o"})
	h.Store.Insert(ctx, logging.Record{Timestamp: now, UserID: "alice", Model: "claude-sonnet-5"})
	h.Store.Insert(ctx, logging.Record{Timestamp: now, UserID: "alice", Model: "gpt-4o-mini"}) // same provider as gpt-4o, must not duplicate

	providers, err := h.distinctProviders(ctx)
	if err != nil {
		t.Fatalf("distinctProviders failed: %v", err)
	}
	want := []string{"anthropic", "openai"} // sorted
	if len(providers) != len(want) {
		t.Fatalf("got %v, want %v", providers, want)
	}
	for i, p := range want {
		if providers[i] != p {
			t.Fatalf("got %v, want %v", providers, want)
		}
	}
}
