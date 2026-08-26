package cost

// DefaultMaxTokensEstimate is the worst-case completion length assumed for
// budget pre-checks when a request doesn't set max_tokens.
const DefaultMaxTokensEstimate = 4096

// EstimatePromptTokens roughly sizes a chat request's prompt from its
// message text (~4 chars/token). It only counts text content; image/audio
// parts in multimodal messages are not sized and are undercounted.
func EstimatePromptTokens(payload map[string]any) int {
	messages, _ := payload["messages"].([]any)
	total := 0
	for _, m := range messages {
		msgMap, ok := m.(map[string]any)
		if !ok {
			continue
		}
		switch content := msgMap["content"].(type) {
		case string:
			total += len(content) / 4
		case []any:
			for _, part := range content {
				partMap, ok := part.(map[string]any)
				if !ok {
					continue
				}
				if text, ok := partMap["text"].(string); ok {
					total += len(text) / 4
				}
			}
		}
	}
	if total == 0 {
		total = 1
	}
	return total
}

// EstimateMaxTokens returns the request's declared max_tokens, or a
// conservative default if unset, for use as a worst-case completion size.
func EstimateMaxTokens(payload map[string]any) int {
	if v, ok := payload["max_tokens"].(float64); ok && v > 0 {
		return int(v)
	}
	return DefaultMaxTokensEstimate
}

// EstimateWorstCaseCost returns the maximum this request could plausibly
// cost, for a pre-flight budget reservation before the upstream call.
func EstimateWorstCaseCost(model string, payload map[string]any) float64 {
	return Calculate(model, EstimatePromptTokens(payload), EstimateMaxTokens(payload))
}

// EstimateEmbeddingTokens sizes an embeddings request's "input" field,
// which may be a single string or an array of strings. Unrecognized
// shapes (e.g. token-ID array input) fall back to 0.
func EstimateEmbeddingTokens(payload map[string]any) int {
	total := 0
	switch input := payload["input"].(type) {
	case string:
		total = len(input) / 4
	case []any:
		for _, item := range input {
			if s, ok := item.(string); ok {
				total += len(s) / 4
			}
		}
	}
	if total == 0 {
		total = 1
	}
	return total
}

// EstimateEmbeddingCost returns the cost of an embeddings request. Exact,
// not a ceiling: there's no completion side to estimate.
func EstimateEmbeddingCost(model string, payload map[string]any) float64 {
	return Calculate(model, EstimateEmbeddingTokens(payload), 0)
}
