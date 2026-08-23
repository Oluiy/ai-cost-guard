package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Oluiy/ai-cost-guard/internal/cost"
)

// OpenAICompatProvider talks to any provider that implements the OpenAI
// chat/completions API shape (OpenAI itself, Groq, Together, etc).
type OpenAICompatProvider struct {
	BaseURL string // e.g. "https://api.openai.com/v1"
	APIKey  string
	Client  *http.Client
}

// NewOpenAICompatProvider builds a provider for an OpenAI-compatible endpoint.
func NewOpenAICompatProvider(baseURL, apiKey string) *OpenAICompatProvider {
	return &OpenAICompatProvider{
		BaseURL: baseURL,
		APIKey:  apiKey,
		Client:  &http.Client{Timeout: 120 * time.Second},
	}
}

type openAIResponse struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

type openAIEmbeddingsResponse struct {
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
	} `json:"usage"`
}

func (p *OpenAICompatProvider) Embeddings(ctx context.Context, model string, rawBody []byte) ([]byte, Usage, int, error) {
	body, err := setModel(rawBody, model)
	if err != nil {
		return nil, Usage{}, 0, fmt.Errorf("rewriting model in request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, Usage{}, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.APIKey)

	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, Usage{}, 0, err
	}
	defer resp.Body.Close()

	respBody, err := readUpstreamBody(resp.Body)
	if err != nil {
		return nil, Usage{}, resp.StatusCode, err
	}
	if resp.StatusCode >= 400 {
		return respBody, Usage{}, resp.StatusCode, fmt.Errorf("upstream returned status %d", resp.StatusCode)
	}

	var parsed openAIEmbeddingsResponse
	usage := Usage{}
	if err := json.Unmarshal(respBody, &parsed); err == nil {
		usage = Usage{PromptTokens: parsed.Usage.PromptTokens}
	}

	return respBody, usage, resp.StatusCode, nil
}

func (p *OpenAICompatProvider) ChatCompletion(ctx context.Context, model string, rawBody []byte) ([]byte, Usage, string, int, error) {
	body, err := setModel(rawBody, model)
	if err != nil {
		return nil, Usage{}, "", 0, fmt.Errorf("rewriting model in request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, Usage{}, "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.APIKey)

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

	var parsed openAIResponse
	finishReason := ""
	usage := Usage{}
	if err := json.Unmarshal(respBody, &parsed); err == nil {
		usage = Usage{PromptTokens: parsed.Usage.PromptTokens, CompletionTokens: parsed.Usage.CompletionTokens}
		if len(parsed.Choices) > 0 {
			finishReason = parsed.Choices[0].FinishReason
		}
	}

	return respBody, usage, finishReason, resp.StatusCode, nil
}

// setModel returns rawBody with its top-level "model" field replaced.
func setModel(rawBody []byte, model string) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(rawBody, &m); err != nil {
		return nil, err
	}
	m["model"] = model
	return json.Marshal(m)
}

// streamChunkView is the subset of an OpenAI chat.completion.chunk this
// package needs to track cost/finish_reason/tool-calls while relaying the
// rest through to the client verbatim.
type streamChunkView struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// toolCallAccumulator rebuilds one complete tool call from OpenAI's
// incrementally-streamed deltas: the id/name typically arrive once on the
// first delta for a given index, and `arguments` arrives split across many
// subsequent deltas that must be concatenated in order, not replaced.
type toolCallAccumulator struct {
	id, name string
	args     strings.Builder
}

// prepareStreamBody rewrites rawBody's model, forces stream:true, and
// requests a trailing usage chunk (widely, though not universally,
// supported by OpenAI-compatible providers) so cost tracking doesn't have
// to fall back to estimating tokens from the streamed text.
func prepareStreamBody(rawBody []byte, model string) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(rawBody, &m); err != nil {
		return nil, err
	}
	m["model"] = model
	m["stream"] = true
	if _, exists := m["stream_options"]; !exists {
		m["stream_options"] = map[string]any{"include_usage": true}
	}
	return json.Marshal(m)
}

// writeSSELine relays one already-framed "data: ..." line (or any other
// SSE line) to the client, restoring the blank-line separator the scanner
// that produced it stripped.
func writeSSELine(w *bufio.Writer, line string) error {
	if _, err := w.Write([]byte(line)); err != nil {
		return err
	}
	if _, err := w.Write([]byte("\n\n")); err != nil {
		return err
	}
	return w.Flush()
}

// openAIStreamSession holds an already-established, successful streaming
// HTTP response, ready to be relayed.
type openAIStreamSession struct {
	resp        *http.Response
	requestBody []byte // for the prompt-token estimate fallback in Relay
}

func (p *OpenAICompatProvider) OpenStream(ctx context.Context, model string, rawBody []byte) (StreamSession, int, error) {
	body, err := prepareStreamBody(rawBody, model)
	if err != nil {
		return nil, 0, fmt.Errorf("rewriting model in request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		errBody, _ := readUpstreamBody(resp.Body)
		return nil, resp.StatusCode, fmt.Errorf("upstream returned status %d: %s", resp.StatusCode, errBody)
	}
	return &openAIStreamSession{resp: resp, requestBody: body}, resp.StatusCode, nil
}

func (s *openAIStreamSession) Close() error {
	return s.resp.Body.Close()
}

func (s *openAIStreamSession) Relay(w *bufio.Writer) (string, []map[string]any, Usage, string, error) {
	defer s.resp.Body.Close()

	var (
		usage          Usage
		finishReason   string
		text           strings.Builder
		haveExactUsage bool
		toolCallOrder  []int
		toolCalls      = map[int]*toolCallAccumulator{}
	)

	scanner := bufio.NewScanner(s.resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if err := writeSSELine(w, line); err != nil {
			// The client is gone; nothing left to relay to, but return
			// what we've accumulated so far so it's still logged/costed.
			return text.String(), finalizeToolCalls(toolCallOrder, toolCalls), usage, finishReason, err
		}

		payload, isData := strings.CutPrefix(line, "data: ")
		if !isData || payload == "[DONE]" {
			continue
		}
		var chunk streamChunkView
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if chunk.Usage != nil {
			usage = Usage{PromptTokens: chunk.Usage.PromptTokens, CompletionTokens: chunk.Usage.CompletionTokens}
			haveExactUsage = true
		}
		if len(chunk.Choices) > 0 {
			text.WriteString(chunk.Choices[0].Delta.Content)
			if chunk.Choices[0].FinishReason != nil {
				finishReason = *chunk.Choices[0].FinishReason
			}
			for _, tc := range chunk.Choices[0].Delta.ToolCalls {
				acc, seen := toolCalls[tc.Index]
				if !seen {
					acc = &toolCallAccumulator{}
					toolCalls[tc.Index] = acc
					toolCallOrder = append(toolCallOrder, tc.Index)
				}
				if tc.ID != "" {
					acc.id = tc.ID
				}
				if tc.Function.Name != "" {
					acc.name = tc.Function.Name
				}
				acc.args.WriteString(tc.Function.Arguments)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return text.String(), finalizeToolCalls(toolCallOrder, toolCalls), usage, finishReason, err
	}

	if !haveExactUsage {
		// This provider didn't honor stream_options.include_usage (or
		// doesn't support it); estimate from what we actually sent/saw
		// rather than logging/billing the request as free.
		var payload map[string]any
		if err := json.Unmarshal(s.requestBody, &payload); err == nil {
			usage.PromptTokens = cost.EstimatePromptTokens(payload)
		}
		usage.CompletionTokens = len(text.String()) / 4
	}

	return text.String(), finalizeToolCalls(toolCallOrder, toolCalls), usage, finishReason, nil
}

// finalizeToolCalls converts accumulated per-index tool call state into
// the same OpenAI tool_calls shape ChatCompletion (non-streaming) and the
// Anthropic translator use, so cache reconstruction is uniform regardless
// of which provider or mode produced the answer.
func finalizeToolCalls(order []int, calls map[int]*toolCallAccumulator) []map[string]any {
	if len(order) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(order))
	for _, idx := range order {
		acc := calls[idx]
		args := acc.args.String()
		if !json.Valid([]byte(args)) {
			args = "{}"
		}
		out = append(out, map[string]any{
			"id":   acc.id,
			"type": "function",
			"function": map[string]any{
				"name":      acc.name,
				"arguments": args,
			},
		})
	}
	return out
}
