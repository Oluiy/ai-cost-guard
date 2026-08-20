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
		case "openai", "anthropic", "groq", "together":
			return prefix
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
	case strings.HasPrefix(lower, "llama"), strings.HasPrefix(lower, "mixtral"), strings.HasPrefix(lower, "gemma"), strings.HasPrefix(lower, "deepseek"):
		return "groq"
	default:
		return "openai"
	}
}
