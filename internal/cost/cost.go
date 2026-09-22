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

	// PerImage is USD per generated image, for /v1/images/generations
	// models billed flat-per-image (most of them). Exactly one of
	// PerImage or InputPer1K/OutputPer1K is set for an image model —
	// OpenAI's gpt-image-1 is actually billed as input/output "image
	// tokens" like a chat model, so it uses InputPer1K/OutputPer1K
	// instead; PerImage here is this table's flat approximation of that
	// (typical cost at default quality/size), not gpt-image-1's exact
	// per-request price. See EstimateImageCost.
	PerImage float64
	// PerMinuteAudio is USD per minute of audio, for
	// /v1/audio/transcriptions (speech-to-text) models — the one pricing
	// dimension every provider's transcription API bills on.
	PerMinuteAudio float64
	// Per1KCharsAudio is USD per 1,000 input characters, for
	// /v1/audio/speech (text-to-speech) models billed flat-per-character.
	Per1KCharsAudio float64
}

// isChatModel reports whether p represents an ordinary chat-completions
// model — used to keep image/audio/embedding models out of the fallback
// picker, which only makes sense for chat models.
func isChatModel(p Price) bool {
	return !p.Embedding && p.PerImage == 0 && p.PerMinuteAudio == 0 && p.Per1KCharsAudio == 0
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

	// Images, TTS, and transcription. Source: developers.openai.com/api/docs/pricing,
	// ai.google.dev/gemini-api/docs/pricing, console.groq.com/docs/models,
	// docs.together.ai — verified live as of 2026-09-03, except where noted
	// below. Flat per-unit prices here are approximations for a few
	// models billed on a different real dimension (see PerImage's doc
	// comment); correct via the `pricing:` config override
	// (docs/guide/configuration.html#pricing) the same way any other
	// drifted entry in this table would be.
	"gpt-image-1": {PerImage: 0.04, Provider: "openai"}, // token-billed upstream; this is a default-quality/size approximation
	"tts-1":       {Per1KCharsAudio: 0.015, Provider: "openai"},
	"tts-1-hd":    {Per1KCharsAudio: 0.030, Provider: "openai"},
	"whisper-1":   {PerMinuteAudio: 0.006, Provider: "openai"},

	// Gemini image/speech models sit behind the newer unified
	// /v1beta/interactions endpoint (not the older generateContent this
	// package's chat path uses). Verified against ai.google.dev/gemini-api/docs/pricing
	// (2026-09): 1K-resolution image generation is $0.067/image (confirmed
	// exact). TTS is actually token-billed ($1/1M input text tokens,
	// $20/1M output audio tokens) — a real cost shape this table can't
	// represent directly (see Price.Per1KCharsAudio's doc comment), so
	// this is a flat-per-character approximation of that real rate, not
	// an independently-confirmed per-character price.
	"gemini-3.1-flash-image":       {PerImage: 0.067, Provider: "gemini"},
	"gemini-3.1-flash-tts-preview": {Per1KCharsAudio: 0.015, Provider: "gemini"},

	// Groq: transcription (GA) and Orpheus TTS (preview, see
	// GROQ-TTS-PREVIEW below). Groq has no image generation.
	"whisper-large-v3":       {PerMinuteAudio: 0.00185, Provider: "groq"},
	"whisper-large-v3-turbo": {PerMinuteAudio: 0.000667, Provider: "groq"},
	// GROQ-TTS-PREVIEW: Orpheus TTS, $22/1M characters per Groq's model
	// page for the English model (checked 2026-09); the Arabic model's
	// price is assumed equal, not confirmed. Preview/evaluation-only per Groq.
	"canopylabs/orpheus-v1-english":   {Per1KCharsAudio: 0.022, Provider: "groq"},
	"canopylabs/orpheus-arabic-saudi": {Per1KCharsAudio: 0.022, Provider: "groq"},

	// Together AI. Verified against together.ai/pricing (2026-09).
	"black-forest-labs/FLUX.1.1-pro":   {PerImage: 0.04, Provider: "together"},
	"black-forest-labs/FLUX.1-schnell": {PerImage: 0.0027, Provider: "together"},
	// Named "together/whisper-large-v3", not Together's own catalog id
	// ("openai/whisper-large-v3") — that id's "openai/" prefix would be
	// misread by RouteProvider as "route to the openai provider", since
	// provider-name prefixes are reserved (route.go). togetherUpstreamModel
	// (internal/proxy/openai.go) maps this FitGuard-facing name to
	// Together's real upstream model id before the request goes out.
	"together/whisper-large-v3": {PerMinuteAudio: 0.0015, Provider: "together"},
}

// ApplyOverrides merges operator-supplied pricing corrections into Table,
// keyed by model name (see config.Config.Pricing — this is the fix for
// the built-in table drifting from real provider pricing). For a model
// already in Table, Provider/Embedding are carried over from the
// existing entry unless the override sets them, so a routine price
// correction only needs InputPer1K/OutputPer1K. For a model not already
// in Table, the override's Provider is used as-is (required for the new
// model to route/validate correctly).
func ApplyOverrides(overrides map[string]Price) {
	for name, o := range overrides {
		existing, known := Table[name]
		if !known {
			Table[name] = o
			continue
		}
		existing.InputPer1K = o.InputPer1K
		existing.OutputPer1K = o.OutputPer1K
		if o.Provider != "" {
			existing.Provider = o.Provider
		}
		if o.Embedding {
			existing.Embedding = o.Embedding
		}
		Table[name] = existing
	}
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
		if !isChatModel(price) || !allowed[price.Provider] {
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

// CalculateImageCost returns the USD cost of generating n images with
// model, for a flat-per-image priced model (PerImage). Token-billed image
// models (PerImage == 0, e.g. gpt-image-1) should use Calculate with the
// response's real usage tokens instead — see Price.PerImage's doc comment.
func CalculateImageCost(model string, n int) float64 {
	p, _ := Lookup(model)
	return float64(n) * p.PerImage
}

// CalculateAudioSpeechCost returns the USD cost of a text-to-speech
// request over charCount input characters, for a flat-per-character
// priced model (Per1KCharsAudio).
func CalculateAudioSpeechCost(model string, charCount int) float64 {
	p, _ := Lookup(model)
	return (float64(charCount) / 1000.0) * p.Per1KCharsAudio
}

// CalculateAudioTranscriptionCost returns the USD cost of transcribing
// durationSeconds of audio with model.
func CalculateAudioTranscriptionCost(model string, durationSeconds float64) float64 {
	p, _ := Lookup(model)
	return (durationSeconds / 60.0) * p.PerMinuteAudio
}
