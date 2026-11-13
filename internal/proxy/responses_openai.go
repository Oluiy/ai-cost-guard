package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// ResponsesProvider is implemented by providers that serve the OpenAI
// Responses API natively. It's a separate interface rather than a Provider
// method because most providers don't have the API at all, and translating
// it (server-side conversation state, built-in tools) would be a guess.
type ResponsesProvider interface {
	Responses(ctx context.Context, model string, rawBody []byte) (respBody []byte, usage Usage, finishReason string, statusCode int, err error)
	OpenResponsesStream(ctx context.Context, model string, rawBody []byte) (ResponsesStream, int, error)
}

// ResponsesStream is an established streaming Responses connection.
type ResponsesStream interface {
	// Relay copies the upstream event stream to w verbatim and returns the
	// usage from its terminal response.* event.
	Relay(w *bufio.Writer) (usage Usage, finishReason string, err error)
	Close() error
}

// supportsResponses is true for the kinds whose upstream actually serves
// /responses: OpenAI and Groq. Together doesn't.
func (p *OpenAICompatProvider) supportsResponses() bool {
	return p.Kind != "together"
}

// serverToolItems are Responses output item types that mean the provider
// ran a tool on its side, billed per use rather than per token.
var serverToolItems = map[string]bool{
	"web_search_call":       true,
	"file_search_call":      true,
	"code_interpreter_call": true,
	"image_generation_call": true,
	"mcp_call":              true,
}

type responsesObject struct {
	Status            string `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Usage *struct {
		InputTokens        int `json:"input_tokens"`
		OutputTokens       int `json:"output_tokens"`
		InputTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
	Output []struct {
		Type string `json:"type"`
	} `json:"output"`
}

// view returns the normalized usage and finish reason. output_tokens
// already includes reasoning tokens, which are billed as output.
func (r responsesObject) view() (Usage, string) {
	var u Usage
	if r.Usage != nil {
		u = Usage{
			PromptTokens:     r.Usage.InputTokens,
			CompletionTokens: r.Usage.OutputTokens,
			CachedTokens:     r.Usage.InputTokensDetails.CachedTokens,
		}
	}
	for _, o := range r.Output {
		if serverToolItems[o.Type] {
			u.ToolUses++
		}
	}
	finish := ""
	switch {
	case r.Status == "incomplete" && r.IncompleteDetails != nil && r.IncompleteDetails.Reason == "max_output_tokens":
		finish = "length"
	case r.Status != "":
		finish = r.Status
	}
	return u, finish
}

func (p *OpenAICompatProvider) responsesRequest(ctx context.Context, model string, rawBody []byte, stream bool) (*http.Request, error) {
	body, err := setModel(rawBody, model)
	if err != nil {
		return nil, fmt.Errorf("rewriting model in request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	return req, nil
}

func (p *OpenAICompatProvider) Responses(ctx context.Context, model string, rawBody []byte) ([]byte, Usage, string, int, error) {
	req, err := p.responsesRequest(ctx, model, rawBody, false)
	if err != nil {
		return nil, Usage{}, "", 0, err
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, Usage{}, "", 0, err
	}
	defer resp.Body.Close()

	respBody, err := readUpstreamBody(resp.Body)
	if err != nil {
		return nil, Usage{}, "", resp.StatusCode, err
	}
	if resp.StatusCode >= 400 {
		return respBody, Usage{}, "", resp.StatusCode, fmt.Errorf("upstream returned status %d", resp.StatusCode)
	}
	var obj responsesObject
	if err := json.Unmarshal(respBody, &obj); err != nil {
		return respBody, Usage{}, "", resp.StatusCode, nil
	}
	u, finish := obj.view()
	return respBody, u, finish, resp.StatusCode, nil
}

type openAIResponsesStream struct{ resp *http.Response }

func (p *OpenAICompatProvider) OpenResponsesStream(ctx context.Context, model string, rawBody []byte) (ResponsesStream, int, error) {
	req, err := p.responsesRequest(ctx, model, rawBody, true)
	if err != nil {
		return nil, 0, err
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		errBody, _ := readUpstreamBody(resp.Body)
		return nil, resp.StatusCode, fmt.Errorf("upstream returned status %d: %s", resp.StatusCode, errBody)
	}
	return &openAIResponsesStream{resp: resp}, resp.StatusCode, nil
}

func (s *openAIResponsesStream) Close() error { return s.resp.Body.Close() }

// Relay forwards every line untouched and only inspects data lines of the
// terminal events (response.completed / .incomplete / .failed), the one
// place the final usage appears.
func (s *openAIResponsesStream) Relay(w *bufio.Writer) (Usage, string, error) {
	defer s.resp.Body.Close()
	var (
		usage  Usage
		finish string
	)
	r := bufio.NewReaderSize(s.resp.Body, 64*1024)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := w.Write(line); werr != nil {
				return usage, finish, werr
			}
			if len(bytes.TrimSpace(line)) == 0 {
				if ferr := w.Flush(); ferr != nil {
					return usage, finish, ferr
				}
			}
			if data, ok := bytes.CutPrefix(line, []byte("data:")); ok && bytes.Contains(data, []byte(`"response.`)) && bytes.Contains(data, []byte(`"usage"`)) {
				var ev struct {
					Type     string          `json:"type"`
					Response responsesObject `json:"response"`
				}
				if json.Unmarshal(bytes.TrimSpace(data), &ev) == nil {
					switch ev.Type {
					case "response.completed", "response.incomplete", "response.failed":
						usage, finish = ev.Response.view()
					}
				}
			}
		}
		if err != nil {
			_ = w.Flush()
			if err == io.EOF {
				return usage, finish, nil
			}
			return usage, finish, err
		}
	}
}
