// Package cost holds model pricing data and cost calculation.
package cost

import (
	"sort"
	"strings"
)

// Price is USD cost per 1K tokens for a model, plus the provider that
// serves it. Provider is what lets fallback validation reject a model
// whose provider isn't configured.
type Price struct {
	InputPer1K  float64
	OutputPer1K float64
	Provider    string
	// Embedding marks models usable only on /v1/embeddings, so they're
	// excluded from chat fallback lists.
	Embedding bool
}

// Table maps model name -> pricing. Prices are USD per 1,000 tokens.
// Source: each provider's own published pricing page, verified live as of
// 2026-08-28 (see platform.claude.com/docs/en/about-claude/pricing,
// developers.openai.com/api/docs/pricing, ai.google.dev/gemini-api/docs/pricing,
// console.groq.com/docs/models, docs.together.ai/docs/serverless-models).
var Table = map[string]Price{
	// OpenAI
	"gpt-5":         {InputPer1K: 0.00125, OutputPer1K: 0.010, Provider: "openai"},
	"gpt-5-mini":    {InputPer1K: 0.00025, OutputPer1K: 0.002, Provider: "openai"},
	"gpt-5-nano":    {InputPer1K: 0.00005, OutputPer1K: 0.0004, Provider: "openai"},
	"gpt-4o":        {InputPer1K: 0.0025, OutputPer1K: 0.010, Provider: "openai"},
	"gpt-4o-mini":   {InputPer1K: 0.00015, OutputPer1K: 0.0006, Provider: "openai"},
	"gpt-4.1":       {InputPer1K: 0.002, OutputPer1K: 0.008, Provider: "openai"},
	"gpt-4.1-mini":  {InputPer1K: 0.0004, OutputPer1K: 0.0016, Provider: "openai"},
	"gpt-4.1-nano":  {InputPer1K: 0.0001, OutputPer1K: 0.0004, Provider: "openai"},
	"gpt-4-turbo":   {InputPer1K: 0.010, OutputPer1K: 0.030, Provider: "openai"},
	"gpt-4":         {InputPer1K: 0.030, OutputPer1K: 0.060, Provider: "openai"},
	"gpt-3.5-turbo": {InputPer1K: 0.0005, OutputPer1K: 0.0015, Provider: "openai"},
	"o1":            {InputPer1K: 0.015, OutputPer1K: 0.060, Provider: "openai"},
	"o1-mini":       {InputPer1K: 0.0011, OutputPer1K: 0.0044, Provider: "openai"},
	"o3":            {InputPer1K: 0.002, OutputPer1K: 0.008, Provider: "openai"},
	"o3-mini":       {InputPer1K: 0.0011, OutputPer1K: 0.0044, Provider: "openai"},
	"o4-mini":       {InputPer1K: 0.0011, OutputPer1K: 0.0044, Provider: "openai"},

	// Anthropic — current generation. Claude 3.x (3-opus, 3-5-sonnet,
	// 3-7-sonnet, etc.) is fully retired on the direct API as of this
	// writing; a request using those names now fails upstream, which is
	// the actual cause of "claude-3-5-sonnet always fails" reports.
	"claude-sonnet-5":   {InputPer1K: 0.002, OutputPer1K: 0.010, Provider: "anthropic"},
	"claude-opus-5":     {InputPer1K: 0.005, OutputPer1K: 0.025, Provider: "anthropic"},
	"claude-fable-5":    {InputPer1K: 0.010, OutputPer1K: 0.050, Provider: "anthropic"},
	"claude-haiku-4-5":  {InputPer1K: 0.001, OutputPer1K: 0.005, Provider: "anthropic"},
	"claude-sonnet-4-5": {InputPer1K: 0.003, OutputPer1K: 0.015, Provider: "anthropic"},
	"claude-opus-4-5":   {InputPer1K: 0.005, OutputPer1K: 0.025, Provider: "anthropic"},

	// Google Gemini
	"gemini-3.5-flash":      {InputPer1K: 0.0015, OutputPer1K: 0.009, Provider: "gemini"},
	"gemini-3.5-flash-lite": {InputPer1K: 0.0003, OutputPer1K: 0.0025, Provider: "gemini"},
	"gemini-2.5-pro":        {InputPer1K: 0.00125, OutputPer1K: 0.010, Provider: "gemini"},
	"gemini-2.5-flash":      {InputPer1K: 0.0003, OutputPer1K: 0.0025, Provider: "gemini"},
	"gemini-2.5-flash-lite": {InputPer1K: 0.0001, OutputPer1K: 0.0004, Provider: "gemini"},
	"gemini-embedding-001":  {InputPer1K: 0.00015, Provider: "gemini", Embedding: true},

	// Groq. gpt-oss-120b/20b are also hosted here, but deliberately not
	// listed: a bare "openai/..." prefix in FitGuard's routing means
	// "force-route to the openai provider" (see route.go), so adding
	// that name here would make FitGuard send it to OpenAI, which
	// doesn't serve it, instead of Groq.
	"llama-3.1-8b-instant":    {InputPer1K: 0.00005, OutputPer1K: 0.00008, Provider: "groq"},
	"llama-3.3-70b-versatile": {InputPer1K: 0.00059, OutputPer1K: 0.00079, Provider: "groq"},

	// Together AI
	"meta-llama/Llama-3.3-70B-Instruct-Turbo": {InputPer1K: 0.00104, OutputPer1K: 0.00104, Provider: "together"},

	// OpenAI embeddings (input-only; OutputPer1K unused for these models)
	"text-embedding-3-small": {InputPer1K: 0.00002, Provider: "openai", Embedding: true},
	"text-embedding-3-large": {InputPer1K: 0.00013, Provider: "openai", Embedding: true},
	"text-embedding-ada-002": {InputPer1K: 0.0001, Provider: "openai", Embedding: true},
}

// defaultPrice is used for unknown models so cost tracking degrades
// gracefully instead of silently reporting $0.
var defaultPrice = Price{InputPer1K: 0.002, OutputPer1K: 0.006}

// Lookup returns the pricing for model, falling back to a conservative
// default if the model isn't in the table.
func Lookup(model string) (Price, bool) {
	if p, ok := Table[model]; ok {
		return p, true
	}
	// Try prefix match for versioned/dated model names, longest first so
	// "claude-3-5-sonnet-20241022" doesn't match a shorter "claude-3".
	best := ""
	for name := range Table {
		if strings.HasPrefix(model, name) && len(name) > len(best) {
			best = name
		}
	}
	if best != "" {
		return Table[best], true
	}
	return defaultPrice, false
}

// ProviderFor returns the provider that serves model, and whether the
// model is known at all. An unknown model has no provider, which is what
// lets fallback validation reject it.
func ProviderFor(model string) (provider string, known bool) {
	// An explicit "provider/model" prefix wins: it's how a caller names a
	// custom or self-hosted model that isn't in the table.
	if idx := strings.Index(model, "/"); idx > 0 {
		if p, ok := Table[model]; ok {
			return p.Provider, true
		}
		return model[:idx], true
	}
	p, ok := Lookup(model)
	if !ok {
		return "", false
	}
	return p.Provider, true
}

// ChatModelsFor returns every known chat model served by one of the given
// providers, sorted. Used to populate the fallback picker and to tell a
// user what they can actually choose.
func ChatModelsFor(providers []string) []string {
	allowed := make(map[string]bool, len(providers))
	for _, p := range providers {
		allowed[p] = true
	}
	var out []string
	for name, price := range Table {
		if price.Embedding || !allowed[price.Provider] {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Calculate returns the USD cost for a request given token counts.
func Calculate(model string, promptTokens, completionTokens int) float64 {
	p, _ := Lookup(model)
	return (float64(promptTokens)/1000.0)*p.InputPer1K + (float64(completionTokens)/1000.0)*p.OutputPer1K
}
