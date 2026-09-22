package proxy

import "strings"

// RouteProvider maps a model name to the provider config key that should
// serve it. This is a heuristic based on common naming conventions; users
// with custom/self-hosted models can still hit the gateway by using a
// provider-prefixed model name (e.g. "groq/my-model").
func RouteProvider(model string) string {
	if idx := strings.Index(model, "/"); idx > 0 {
		prefix := model[:idx]
		switch prefix {
		case "openai", "anthropic", "gemini", "groq", "together":
			return prefix
		case "canopylabs": // GROQ-TTS-PREVIEW: Groq's Orpheus TTS model ids
			return "groq"
		}
		// e.g. "meta-llama/Llama-3-70b-chat-hf" -> together by default
		return "together"
	}

	lower := strings.ToLower(model)
	switch {
	case strings.HasPrefix(lower, "gpt-"), strings.HasPrefix(lower, "o1"), strings.HasPrefix(lower, "o3"), strings.HasPrefix(lower, "text-"):
		return "openai"
	case strings.HasPrefix(lower, "claude"):
		return "anthropic"
	case strings.HasPrefix(lower, "gemini"):
		return "gemini"
	case strings.HasPrefix(lower, "llama"), strings.HasPrefix(lower, "mixtral"), strings.HasPrefix(lower, "gemma"), strings.HasPrefix(lower, "deepseek"):
		return "groq"
	// Images/audio model names, so they don't fall through to the
	// catch-all "openai" default for names that don't actually look
	// like chat models (dall-e-3, tts-1-hd, whisper-large-v3-turbo).
	case strings.HasPrefix(lower, "dall-e"), strings.HasPrefix(lower, "gpt-image"), strings.HasPrefix(lower, "tts-"):
		return "openai"
	case strings.HasPrefix(lower, "whisper-1"):
		return "openai"
	case strings.HasPrefix(lower, "whisper-large-v3"):
		return "groq"
	case strings.HasPrefix(lower, "flux"), strings.HasPrefix(lower, "stable-diffusion"):
		return "together"
	default:
		return "openai"
	}
}
