package proxy

import (
	"io"

	"github.com/google/uuid"
)

func randomSuffix() string {
	return uuid.NewString()
}

// maxUpstreamResponseBytes caps how much of a non-streaming provider
// response body gets read into memory. Providers is a slightly loose
// term here — `base_url` is user-configurable to point at any
// OpenAI-compatible endpoint, including self-hosted ones, so this isn't
// just about trusting OpenAI/Anthropic/Groq/Together to behave: a
// misconfigured or compromised custom endpoint returning an unbounded
// body would otherwise be read in full via io.ReadAll, one bad response
// away from exhausting memory. 32MB comfortably covers any real chat
// completion or embeddings response.
const maxUpstreamResponseBytes = 32 * 1024 * 1024

// readUpstreamBody reads resp.Body capped at maxUpstreamResponseBytes.
func readUpstreamBody(body io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(body, maxUpstreamResponseBytes+1))
}
