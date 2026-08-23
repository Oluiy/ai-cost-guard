// Package cost holds model pricing data and cost calculation.
package cost

import "strings"

// Price is USD cost per 1K tokens for a model.
type Price struct {
	InputPer1K  float64
	OutputPer1K float64
}

// Table maps model name -> pricing. Prices are USD per 1,000 tokens.
// Source: published provider pricing pages, approximate as of 2025.
var Table = map[string]Price{
	// OpenAI
	"gpt-4o":        {InputPer1K: 0.0025, OutputPer1K: 0.010},
	"gpt-4o-mini":   {InputPer1K: 0.00015, OutputPer1K: 0.0006},
	"gpt-4-turbo":   {InputPer1K: 0.010, OutputPer1K: 0.030},
	"gpt-4":         {InputPer1K: 0.030, OutputPer1K: 0.060},
	"gpt-3.5-turbo": {InputPer1K: 0.0005, OutputPer1K: 0.0015},
	"o1":            {InputPer1K: 0.015, OutputPer1K: 0.060},
	"o1-mini":       {InputPer1K: 0.0011, OutputPer1K: 0.0044},
	"o3-mini":       {InputPer1K: 0.0011, OutputPer1K: 0.0044},

	// Anthropic
	"claude-5-sonnet":   {InputPer1K: 0.003, OutputPer1K: 0.015},
	"claude-3-5-sonnet": {InputPer1K: 0.003, OutputPer1K: 0.015},
	"claude-3-5-haiku":  {InputPer1K: 0.0008, OutputPer1K: 0.004},
	"claude-3-opus":     {InputPer1K: 0.015, OutputPer1K: 0.075},
	"claude-3-sonnet":   {InputPer1K: 0.003, OutputPer1K: 0.015},
	"claude-3-haiku":    {InputPer1K: 0.00025, OutputPer1K: 0.00125},

	// Google Gemini
	"gemini-2.5-pro":        {InputPer1K: 0.00125, OutputPer1K: 0.010},
	"gemini-2.5-flash":      {InputPer1K: 0.0003, OutputPer1K: 0.0025},
	"gemini-2.5-flash-lite": {InputPer1K: 0.0001, OutputPer1K: 0.0004},
	"gemini-2.0-flash":      {InputPer1K: 0.0001, OutputPer1K: 0.0004},
	"gemini-1.5-pro":        {InputPer1K: 0.00125, OutputPer1K: 0.005},
	"gemini-1.5-flash":      {InputPer1K: 0.000075, OutputPer1K: 0.0003},
	"gemini-embedding-001":  {InputPer1K: 0.00015},

	// Groq (Llama / Mixtral hosted)
	"llama-3.1-8b-instant":    {InputPer1K: 0.00005, OutputPer1K: 0.00008},
	"llama-3.1-70b-versatile": {InputPer1K: 0.00059, OutputPer1K: 0.00079},
	"llama-3.3-70b-versatile": {InputPer1K: 0.00059, OutputPer1K: 0.00079},
	"mixtral-8x7b-32768":      {InputPer1K: 0.00024, OutputPer1K: 0.00024},
	"gemma2-9b-it":            {InputPer1K: 0.0002, OutputPer1K: 0.0002},

	// Together AI
	"meta-llama/Llama-3-8b-chat-hf":        {InputPer1K: 0.0002, OutputPer1K: 0.0002},
	"meta-llama/Llama-3-70b-chat-hf":       {InputPer1K: 0.0009, OutputPer1K: 0.0009},
	"mistralai/Mixtral-8x7B-Instruct-v0.1": {InputPer1K: 0.0006, OutputPer1K: 0.0006},

	// OpenAI embeddings (input-only; OutputPer1K unused for these models)
	"text-embedding-3-small": {InputPer1K: 0.00002},
	"text-embedding-3-large": {InputPer1K: 0.00013},
	"text-embedding-ada-002": {InputPer1K: 0.0001},
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
	// Try prefix match for versioned/dated model names.
	for name, p := range Table {
		if strings.HasPrefix(model, name) {
			return p, true
		}
	}
	return defaultPrice, false
}

// Calculate returns the USD cost for a request given token counts.
func Calculate(model string, promptTokens, completionTokens int) float64 {
	p, _ := Lookup(model)
	return (float64(promptTokens)/1000.0)*p.InputPer1K + (float64(completionTokens)/1000.0)*p.OutputPer1K
}
