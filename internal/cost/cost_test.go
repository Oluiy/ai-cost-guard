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

func TestCalculateImageCost(t *testing.T) {
	if got, want := CalculateImageCost("gpt-image-1", 3), 3*Table["gpt-image-1"].PerImage; got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestCalculateAudioSpeechCost(t *testing.T) {
	got := CalculateAudioSpeechCost("tts-1", 2000)
	want := 2.0 * Table["tts-1"].Per1KCharsAudio
	if got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestCalculateAudioTranscriptionCost(t *testing.T) {
	got := CalculateAudioTranscriptionCost("whisper-1", 120) // 2 minutes
	want := 2.0 * Table["whisper-1"].PerMinuteAudio
	if got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestChatModelsFor_ExcludesImageAndAudioModels is the fix for
// isChatModel: an image/audio/embedding model has no real "chat fallback"
// meaning and must never show up in the fallback picker alongside actual
// chat models.
func TestChatModelsFor_ExcludesImageAndAudioModels(t *testing.T) {
	models := ChatModelsFor([]string{"openai"})
	for _, m := range models {
		if m == "gpt-image-1" || m == "tts-1" || m == "whisper-1" {
			t.Errorf("ChatModelsFor included non-chat model %q", m)
		}
	}
	found := false
	for _, m := range models {
		if m == "gpt-4o" {
			found = true
		}
	}
	if !found {
		t.Error("ChatModelsFor should still include real chat models like gpt-4o")
	}
}

// TestCompute_CachedTokensAreDiscounted is the fix for cached workloads
// being mis-reported: 1000 input tokens, 800 of them cache reads, must be
// charged at the discounted rate for those 800, not the full input rate.
func TestCompute_CachedTokensAreDiscounted(t *testing.T) {
	b := Compute("gpt-4o", TokenUsage{Prompt: 1000, Cached: 800, Completion: 100})
	p := Table["gpt-4o"]
	wantInput := 0.2 * p.InputPer1K
	wantCached := 0.8 * p.CachedInputPer1K
	wantOut := 0.1 * p.OutputPer1K
	if diff := b.Total() - (wantInput + wantCached + wantOut); diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("total %v, want %v", b.Total(), wantInput+wantCached+wantOut)
	}
	if b.Total() >= 1.0*p.InputPer1K+0.1*p.OutputPer1K {
		t.Fatal("cached tokens were not discounted")
	}
}

// Anthropic bills cache writes at a premium; an unverified model must fall
// back to the full input rate (over-report) instead of a zero rate.
func TestCompute_CacheWriteAndUnverifiedFallback(t *testing.T) {
	b := Compute("claude-sonnet-5", TokenUsage{Prompt: 1000, CacheWrite: 1000})
	if want := Table["claude-sonnet-5"].CacheWritePer1K; b.CacheWriteUSD != want {
		t.Fatalf("cache write cost %v, want %v", b.CacheWriteUSD, want)
	}
	// o1 has no verified cached rate, so cached tokens bill at full price.
	b = Compute("o1", TokenUsage{Prompt: 1000, Cached: 1000})
	if want := Table["o1"].InputPer1K; b.CachedUSD != want {
		t.Fatalf("unverified cached rate: got %v, want full input rate %v", b.CachedUSD, want)
	}
}

func TestCompute_MalformedCountsNeverGoNegative(t *testing.T) {
	b := Compute("gpt-4o", TokenUsage{Prompt: 100, Cached: 500, CacheWrite: 500})
	if b.Total() < 0 || b.InputUSD < 0 {
		t.Fatalf("negative cost from malformed counts: %+v", b)
	}
}

// Calculate must stay equal to Compute with no cache fields, since the
// embeddings path and older callers still use it.
func TestCalculate_MatchesComputeWithoutCache(t *testing.T) {
	if got, want := Calculate("gpt-4o", 1234, 567), Compute("gpt-4o", TokenUsage{Prompt: 1234, Completion: 567}).Total(); got != want {
		t.Fatalf("Calculate=%v Compute=%v", got, want)
	}
}

func TestAnthropicCurrentModelsHaveExplicitEntries(t *testing.T) {
	for _, m := range []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-fable-5-1"} {
		if _, ok := Table[m]; !ok {
			t.Errorf("%s missing from Table; it would prefix-match an older model with the wrong cache rate", m)
		}
	}
}
