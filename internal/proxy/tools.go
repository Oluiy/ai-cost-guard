package proxy

import "strings"

// serverToolsRequested reports whether a chat request asks the provider to
// run a tool on ITS side (web search, code execution, MCP, a "compound"
// system with built-in tools...) as opposed to an ordinary function the
// caller executes. Two consequences follow, which is why this is one
// shared check:
//
//   - The response is not a pure function of the request. A cached
//     web_search answer is yesterday's results served as current, so these
//     requests must bypass the response cache.
//   - The provider bills each use separately from tokens (web search is
//     billed per search, code execution per container-hour), and those
//     fees are not in the token counts FitGuard prices.
func serverToolsRequested(model string, parsed map[string]any) bool {
	if _, ok := parsed["web_search_options"]; ok {
		return true
	}
	lower := strings.ToLower(model)
	if strings.Contains(lower, "search-preview") || strings.Contains(lower, "compound") {
		return true
	}
	tools, _ := parsed["tools"].([]any)
	for _, t := range tools {
		m, _ := t.(map[string]any)
		if typ, _ := m["type"].(string); typ != "" && typ != "function" {
			return true
		}
	}
	return false
}

// serverToolUses is what gets recorded for a request. Counts the provider
// reported win; otherwise a request that asked for server tools on a
// provider that actually forwards them counts as 1 ("used, count unknown").
// Anthropic and Gemini are excluded because their translation layers only
// forward function tools, so a server tool requested there never runs and
// there's no fee to be missing.
func serverToolUses(providerName string, requested bool, observed int) int {
	if observed > 0 {
		return observed
	}
	if requested && providerName != "anthropic" && providerName != "gemini" {
		return 1
	}
	return 0
}
