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
