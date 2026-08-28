package cost

import "testing"

func TestEstimatePromptTokens(t *testing.T) {
	payload := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "12345678"}, // 8 chars -> 2 tokens
		},
	}
	if got := EstimatePromptTokens(payload); got != 2 {
		t.Fatalf("got %d, want 2", got)
	}
}

func TestEstimatePromptTokens_MultimodalContentParts(t *testing.T) {
	payload := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "12345678"},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/x.png"}},
				},
			},
		},
	}
	// 8 chars -> 2 text tokens, plus a flat per-image ceiling: an image
	// part must contribute to the estimate, not be silently undercounted
	// to zero.
	want := 2 + imageTokenCeiling
	if got := EstimatePromptTokens(payload); got != want {
		t.Fatalf("got %d, want %d", got, want)
	}
}

func TestEstimatePromptTokens_MultipleImagesEachCounted(t *testing.T) {
	payload := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/a.png"}},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/b.png"}},
				},
			},
		},
	}
	if got, want := EstimatePromptTokens(payload), 2*imageTokenCeiling; got != want {
		t.Fatalf("got %d, want %d", got, want)
	}
}

func TestEstimateMaxTokens_UsesRequestValueWhenPresent(t *testing.T) {
	payload := map[string]any{"max_tokens": float64(256)}
	if got := EstimateMaxTokens(payload); got != 256 {
		t.Fatalf("got %d, want 256", got)
	}
}

func TestEstimateMaxTokens_FallsBackToDefault(t *testing.T) {
	if got := EstimateMaxTokens(map[string]any{}); got != DefaultMaxTokensEstimate {
		t.Fatalf("got %d, want %d", got, DefaultMaxTokensEstimate)
	}
}

func TestEstimateWorstCaseCost_UsesMaxTokensNotTypicalCompletion(t *testing.T) {
	small := map[string]any{
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
		"max_tokens": float64(10),
	}
	large := map[string]any{
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
		"max_tokens": float64(10000),
	}
	if EstimateWorstCaseCost("gpt-4o", small) >= EstimateWorstCaseCost("gpt-4o", large) {
		t.Fatal("expected a higher max_tokens request to estimate a higher worst-case cost")
	}
}

func TestEstimateEmbeddingTokens_StringInput(t *testing.T) {
	if got := EstimateEmbeddingTokens(map[string]any{"input": "12345678"}); got != 2 {
		t.Fatalf("got %d, want 2", got)
	}
}

func TestEstimateEmbeddingTokens_BatchArrayInput(t *testing.T) {
	payload := map[string]any{"input": []any{"12345678", "1234"}}
	if got := EstimateEmbeddingTokens(payload); got != 3 { // 8/4 + 4/4
		t.Fatalf("got %d, want 3", got)
	}
}

func TestEstimateEmbeddingCost_NoCompletionComponent(t *testing.T) {
	// Embeddings have no completion side; cost should equal input-only
	// pricing regardless of what OutputPer1K would otherwise contribute.
	payload := map[string]any{"input": "12345678"}
	got := EstimateEmbeddingCost("text-embedding-3-small", payload)
	want := Calculate("text-embedding-3-small", 2, 0)
	if got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}
