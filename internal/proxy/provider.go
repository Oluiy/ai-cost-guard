// Package proxy implements the OpenAI-compatible /v1/chat/completions
// gateway: routing, provider translation, caching, budgets, and fallback.
package proxy

import (
	"bufio"
	"context"
)

// Usage is normalized token usage for a single completion.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
}

// Provider forwards an OpenAI-shaped chat completion request to an upstream
// LLM API and returns an OpenAI-shaped response.
type Provider interface {
	// ChatCompletion sends rawBody (an OpenAI chat/completions request,
	// already rewritten to target `model`) upstream and returns an
	// OpenAI-shaped response body plus normalized usage/finish_reason.
	ChatCompletion(ctx context.Context, model string, rawBody []byte) (respBody []byte, usage Usage, finishReason string, statusCode int, err error)

	// OpenStream starts a streaming request and returns once the status
	// is known, before anything is written to a client — so a caller can
	// still fall back to another provider on early failure.
	OpenStream(ctx context.Context, model string, rawBody []byte) (StreamSession, int, error)

	// Embeddings sends rawBody upstream and returns an OpenAI-shaped
	// response plus usage. Providers with no embeddings API return an
	// error rather than a silent no-op.
	Embeddings(ctx context.Context, model string, rawBody []byte) (respBody []byte, usage Usage, statusCode int, err error)
}

// StreamSession is an established, successful streaming connection to a
// provider, ready to be relayed to a client.
type StreamSession interface {
	// Relay drains the upstream stream into w, translating from the
	// provider's native SSE format if needed, and returns the
	// accumulated text, tool calls, usage, and finish_reason once it ends.
	Relay(w *bufio.Writer) (text string, toolCalls []map[string]any, usage Usage, finishReason string, err error)
	// Close releases the underlying connection. Safe to call whether or
	// not Relay was ever called.
	Close() error
}
