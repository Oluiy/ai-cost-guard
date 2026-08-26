package config

import (
	"strings"
	"testing"

	"github.com/Oluiy/ai-cost-guard/internal/cost"
)

func providers(names ...string) map[string]Provider {
	m := map[string]Provider{}
	for _, n := range names {
		m[n] = Provider{APIKey: "sk-test"}
	}
	return m
}

func TestValidateFallback_AcceptsModelsFromConfiguredProviders(t *testing.T) {
	err := ValidateFallback(
		[]string{"gpt-4o-mini", "claude-3-haiku", "gemini-2.5-flash"},
		providers("openai", "anthropic", "gemini"),
	)
	if err != nil {
		t.Fatalf("expected all three to be accepted: %v", err)
	}
}

// The whole point: a fallback naming an unconfigured provider is skipped
// silently at request time, so the chain quietly degrades to nothing.
func TestValidateFallback_RejectsModelFromUnconfiguredProvider(t *testing.T) {
	err := ValidateFallback([]string{"claude-3-haiku"}, providers("openai"))
	if err == nil {
		t.Fatal("expected a model from an unconfigured provider to be rejected")
	}
	if !strings.Contains(err.Error(), "anthropic") {
		t.Errorf("error should name the missing provider, got: %v", err)
	}
	if !strings.Contains(err.Error(), "gpt-4o") {
		t.Errorf("error should suggest usable models, got: %v", err)
	}
}

func TestValidateFallback_RejectsUnknownModel(t *testing.T) {
	err := ValidateFallback([]string{"totally-made-up-model"}, providers("openai"))
	if err == nil {
		t.Fatal("expected an unknown model to be rejected")
	}
	if !strings.Contains(err.Error(), "not a known model") {
		t.Errorf("unexpected error: %v", err)
	}
}

// A provider-prefixed name is how you point at a custom or self-hosted
// model, so it's accepted when that provider is configured.
func TestValidateFallback_AcceptsProviderPrefixedCustomModel(t *testing.T) {
	if err := ValidateFallback([]string{"groq/my-finetune"}, providers("groq")); err != nil {
		t.Fatalf("expected a provider-prefixed custom model to be accepted: %v", err)
	}
	if err := ValidateFallback([]string{"groq/my-finetune"}, providers("openai")); err == nil {
		t.Fatal("expected it to be rejected when groq isn't configured")
	}
}

func TestValidateFallback_RejectsEmptyEntry(t *testing.T) {
	if err := ValidateFallback([]string{""}, providers("openai")); err == nil {
		t.Fatal("expected an empty fallback entry to be rejected")
	}
}

// Versioned/dated names must resolve to their base model's provider.
func TestValidateFallback_ResolvesDatedModelNames(t *testing.T) {
	if err := ValidateFallback([]string{"claude-3-5-sonnet-20241022"}, providers("anthropic")); err != nil {
		t.Fatalf("expected a dated model name to resolve: %v", err)
	}
}

func TestChatModelsFor_ExcludesEmbeddingsAndOtherProviders(t *testing.T) {
	got := cost.ChatModelsFor([]string{"openai"})
	for _, m := range got {
		if strings.HasPrefix(m, "text-embedding") {
			t.Errorf("embedding model %q should not be offered as a chat fallback", m)
		}
		if strings.HasPrefix(m, "claude") {
			t.Errorf("anthropic model %q returned for an openai-only list", m)
		}
	}
}
