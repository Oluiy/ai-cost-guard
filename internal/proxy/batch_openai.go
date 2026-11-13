package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
)

// BatchProvider is implemented by providers that serve the OpenAI Files and
// Batches APIs. Like ResponsesProvider it's separate from Provider: most
// providers have no equivalent, and Anthropic's Message Batches has a
// different shape that would need real translation.
type BatchProvider interface {
	// Forward sends one request to the provider's API (path is relative to
	// its base URL, e.g. "/batches/batch_1") and returns the raw reply.
	Forward(ctx context.Context, method, path, contentType string, body []byte) (status int, respContentType string, respBody []byte, err error)
}

// supportsBatch is true for OpenAI and Groq. Together's batch API differs.
func (p *OpenAICompatProvider) supportsBatch() bool { return p.Kind != "together" }

func (p *OpenAICompatProvider) Forward(ctx context.Context, method, path, contentType string, body []byte) (int, string, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.BaseURL+path, rd)
	if err != nil {
		return 0, "", nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	resp, err := p.Client.Do(req)
	if err != nil {
		return 0, "", nil, err
	}
	defer resp.Body.Close()
	out, err := readUpstreamBody(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), out, err
}
