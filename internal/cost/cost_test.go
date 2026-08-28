package cost

import "testing"

// TestApplyOverrides_CorrectsExistingModel is the fix for pricing drift:
// an operator can correct a stale built-in price via config without
// waiting on a fitguard release. Provider/Embedding must survive
// untouched since a routine correction only supplies new prices.
func TestApplyOverrides_CorrectsExistingModel(t *testing.T) {
	const model = "gpt-4o"
	original := Table[model]
	t.Cleanup(func() { Table[model] = original })

	ApplyOverrides(map[string]Price{model: {InputPer1K: 0.009, OutputPer1K: 0.099}})

	got := Table[model]
	if got.InputPer1K != 0.009 || got.OutputPer1K != 0.099 {
		t.Fatalf("got %+v, want corrected prices", got)
	}
	if got.Provider != original.Provider {
		t.Fatalf("Provider changed from %q to %q; a price-only override must not touch it", original.Provider, got.Provider)
	}
}

// TestApplyOverrides_AddsUnknownModel lets an operator register a model
// the built-in table doesn't know about yet (e.g. a brand-new release),
// same fix as above but for the "missing" case rather than "stale".
func TestApplyOverrides_AddsUnknownModel(t *testing.T) {
	const model = "test-only-model-xyz"
	t.Cleanup(func() { delete(Table, model) })

	ApplyOverrides(map[string]Price{model: {InputPer1K: 0.001, OutputPer1K: 0.002, Provider: "openai"}})

	got, ok := Lookup(model)
	if !ok {
		t.Fatal("expected the overridden model to be known after ApplyOverrides")
	}
	if got.Provider != "openai" {
		t.Fatalf("got provider %q, want openai", got.Provider)
	}
}
