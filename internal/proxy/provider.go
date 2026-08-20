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

	// OpenStream starts a streaming request upstream and returns once the
	// response status is known — before anything has been written to any
	// client. That's what lets a caller still fall back to another
	// provider/model on early failure, exactly like ChatCompletion: the
	// returned StreamSession is only committed to a client once the
	// caller chooses to call Relay on it.
	OpenStream(ctx context.Context, model string, rawBody []byte) (StreamSession, int, error)

	// Embeddings sends rawBody (an OpenAI /v1/embeddings request) upstream
	// and returns an OpenAI-shaped response body plus usage. Providers
	// without an embeddings API (e.g. Anthropic) return a descriptive
	// error rather than silently no-op'ing.
	Embeddings(ctx context.Context, model string, rawBody []byte) (respBody []byte, usage Usage, statusCode int, err error)
}

// StreamSession is an established, successful streaming connection to a
// provider, ready to be relayed to a client.
type StreamSession interface {
	// Relay drains the upstream stream into w — translating from the
	// provider's native SSE format if needed (see AnthropicProvider) —
	// and returns the accumulated text, tool calls, usage, and
	// finish_reason once it ends. None of this is known up front the way
	// a non-streaming ChatCompletion response's body is, but the caller
	// (the cache layer) still needs the complete answer to reconstruct a
	// normal, non-streaming response shape once the stream ends.
	Relay(w *bufio.Writer) (text string, toolCalls []map[string]any, usage Usage, finishReason string, err error)
	// Close releases the underlying connection. Safe to call whether or
	// not Relay was ever called.
	Close() error
}
