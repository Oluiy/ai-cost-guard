package cost

// DefaultMaxTokensEstimate is the worst-case completion length assumed for
// budget pre-checks when a request doesn't set max_tokens.
const DefaultMaxTokensEstimate = 4096

// imageTokenCeiling is a deliberately generous flat per-image worst-case
// token estimate. Real image token cost depends on pixel dimensions,
// which this estimator doesn't decode (that would mean base64-decoding
// and parsing image headers on every request just to reserve budget).
// Providers' actual image costs run roughly 85-2000+ tokens depending on
// resolution/detail; this errs high on purpose so a reservation stays a
// ceiling instead of silently treating every image as free.
const imageTokenCeiling = 1600

// EstimatePromptTokens roughly sizes a chat request's prompt from its
// message text (~4 chars/token) plus a flat per-image ceiling
// (imageTokenCeiling) for any image_url parts in multimodal messages.
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
				if partMap["type"] == "image_url" {
					total += imageTokenCeiling
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

// EstimateImageCost returns the cost of an /v1/images/generations request.
// Exact, not a ceiling: "n" (image count) is a known request field, so
// there's nothing to estimate the way max_tokens is unknowable up front.
func EstimateImageCost(model string, payload map[string]any) float64 {
	n := 1
	if v, ok := payload["n"].(float64); ok && v > 0 {
		n = int(v)
	}
	return CalculateImageCost(model, n)
}

// EstimateAudioSpeechCost returns the cost of an /v1/audio/speech request.
// Exact, not a ceiling: the input text is fully known up front, same as
// an embeddings request.
func EstimateAudioSpeechCost(model string, payload map[string]any) float64 {
	text, _ := payload["input"].(string)
	return CalculateAudioSpeechCost(model, len(text))
}

// minAudioBytesPerSecond is a deliberately low assumption (8kbps mono
// speech, near the bottom of any real codec's bitrate) so dividing a file's
// byte size by it produces a ceiling on its duration, not an underestimate
// — same "err high on purpose" philosophy as imageTokenCeiling above, used
// here because an upload's actual duration isn't knowable without decoding
// the audio file, which this estimator deliberately avoids doing per
// request just to reserve budget.
const minAudioBytesPerSecond = 1000

// EstimateAudioTranscriptionCost returns a worst-case cost ceiling for an
// /v1/audio/transcriptions request, from the uploaded file's byte size —
// the one thing known before the provider call. Reconcile against the
// provider's actual reported duration for the real cost afterward, the
// same way every other endpoint reconciles its estimated vs. actual cost.
func EstimateAudioTranscriptionCost(model string, fileSizeBytes int64) float64 {
	durationSeconds := float64(fileSizeBytes) / minAudioBytesPerSecond
	return CalculateAudioTranscriptionCost(model, durationSeconds)
}
