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
)

// AnthropicProvider translates OpenAI-shaped chat/completions requests into
// Anthropic's Messages API and translates the response back, including
// multimodal (image) content, tool/function calling, and streaming.
type AnthropicProvider struct {
	BaseURL string // e.g. "https://api.anthropic.com/v1"
	APIKey  string
	Client  *http.Client
}

func NewAnthropicProvider(baseURL, apiKey string) *AnthropicProvider {
	return &AnthropicProvider{
		BaseURL: baseURL,
		APIKey:  apiKey,
		Client:  &http.Client{Timeout: 120 * time.Second},
	}
}

// --- OpenAI-side request shapes (permissive: content may be a string or
// an array of parts, which is why it's json.RawMessage here rather than a
// fixed field) ---

type openAIChatRequest struct {
	Model       string           `json:"model"`
	Messages    []openAIMessage  `json:"messages"`
	Temperature *float64         `json:"temperature,omitempty"`
	MaxTokens   *int             `json:"max_tokens,omitempty"`
	Tools       []openAITool     `json:"tools,omitempty"`
	ToolChoice  *json.RawMessage `json:"tool_choice,omitempty"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    *json.RawMessage `json:"content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIContentPart struct {
	Type     string `json:"type"` // "text" or "image_url"
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

// parseOpenAIContent returns a message's content as plain text if it's a
// JSON string, or as content parts if it's an array (the multimodal
// shape). Exactly one of the two return values is populated.
func parseOpenAIContent(raw *json.RawMessage) (text string, parts []openAIContentPart) {
	if raw == nil {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(*raw, &s); err == nil {
		return s, nil
	}
	var arr []openAIContentPart
	if err := json.Unmarshal(*raw, &arr); err == nil {
		return "", arr
	}
	return "", nil
}

// --- Anthropic-side request shapes ---

type anthropicRequest struct {
	Model       string             `json:"model"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	MaxTokens   int                `json:"max_tokens"`
	Temperature *float64           `json:"temperature,omitempty"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
	ToolChoice  any                `json:"tool_choice,omitempty"`
	Stream      bool               `json:"stream,omitempty"`
}

type anthropicMessage struct {
	Role    string                  `json:"role"`
	Content []anthropicContentBlock `json:"content"`
}

type anthropicContentBlock struct {
	Type      string                `json:"type"` // "text", "image", "tool_use", "tool_result"
	Text      string                `json:"text,omitempty"`
	Source    *anthropicImageSource `json:"source,omitempty"`
	ID        string                `json:"id,omitempty"`          // tool_use
	Name      string                `json:"name,omitempty"`        // tool_use
	Input     json.RawMessage       `json:"input,omitempty"`       // tool_use
	ToolUseID string                `json:"tool_use_id,omitempty"` // tool_result
	Content   string                `json:"content,omitempty"`     // tool_result
}

type anthropicImageSource struct {
	Type      string `json:"type"` // "base64" or "url"
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

const defaultAnthropicMaxTokens = 1024

// buildAnthropicRequest converts an OpenAI-shaped chat/completions
// request into Anthropic's Messages API shape: system messages become
// the top-level `system` field, image_url parts become image blocks,
// tool_calls/results become tool_use/tool_result blocks, and tool
// definitions become Anthropic's input_schema form.
func buildAnthropicRequest(rawBody []byte, model string, stream bool) (anthropicRequest, error) {
	var oaiReq openAIChatRequest
	if err := json.Unmarshal(rawBody, &oaiReq); err != nil {
		return anthropicRequest{}, fmt.Errorf("parsing request: %w", err)
	}

	aReq := anthropicRequest{
		Model:       model,
		Temperature: oaiReq.Temperature,
		MaxTokens:   defaultAnthropicMaxTokens,
		Stream:      stream,
	}
	if oaiReq.MaxTokens != nil {
		aReq.MaxTokens = *oaiReq.MaxTokens
	}

	var systemParts []string
	for _, m := range oaiReq.Messages {
		switch m.Role {
		case "system":
			text, _ := parseOpenAIContent(m.Content)
			if text != "" {
				systemParts = append(systemParts, text)
			}
			continue
		case "tool":
			text, _ := parseOpenAIContent(m.Content)
			aReq.Messages = append(aReq.Messages, anthropicMessage{
				Role: "user",
				Content: []anthropicContentBlock{{
					Type: "tool_result", ToolUseID: m.ToolCallID, Content: text,
				}},
			})
			continue
		}

		var blocks []anthropicContentBlock
		text, parts := parseOpenAIContent(m.Content)
		if text != "" {
			blocks = append(blocks, anthropicContentBlock{Type: "text", Text: text})
		}
		for _, part := range parts {
			switch part.Type {
			case "text":
				blocks = append(blocks, anthropicContentBlock{Type: "text", Text: part.Text})
			case "image_url":
				if part.ImageURL == nil {
					continue
				}
				blocks = append(blocks, anthropicContentBlock{Type: "image", Source: parseImageSource(part.ImageURL.URL)})
			}
		}
		for _, tc := range m.ToolCalls {
			args := json.RawMessage(tc.Function.Arguments)
			if !json.Valid(args) {
				args = json.RawMessage("{}")
			}
			blocks = append(blocks, anthropicContentBlock{
				Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: args,
			})
		}
		if len(blocks) == 0 {
			continue
		}
		aReq.Messages = append(aReq.Messages, anthropicMessage{Role: m.Role, Content: blocks})
	}
	aReq.System = strings.Join(systemParts, "\n\n")

	// OpenAI's tool_choice: "none" means "the model must not call a tool."
	// Anthropic has no equivalent field: it decides whether to call a tool
	// based solely on whether any `tools` were sent. So the exact
	// translation is to omit `tools` (and `tool_choice`) entirely, not to
	// send tools with a hopeful "auto"/"any" — that leaves the model free
	// to call one anyway.
	if !toolChoiceIsNone(oaiReq.ToolChoice) {
		for _, t := range oaiReq.Tools {
			if t.Type != "function" {
				continue
			}
			aReq.Tools = append(aReq.Tools, anthropicTool{
				Name: t.Function.Name, Description: t.Function.Description, InputSchema: t.Function.Parameters,
			})
		}
		if oaiReq.ToolChoice != nil {
			aReq.ToolChoice = translateToolChoice(*oaiReq.ToolChoice)
		}
	}

	return aReq, nil
}

// toolChoiceIsNone reports whether an OpenAI tool_choice value is the
// literal string "none".
func toolChoiceIsNone(raw *json.RawMessage) bool {
	if raw == nil {
		return false
	}
	var s string
	return json.Unmarshal(*raw, &s) == nil && s == "none"
}

// parseImageSource turns an OpenAI image_url.url into an Anthropic image
// source: a "data:<mime>;base64,<data>" URI becomes a base64 block
// (Anthropic doesn't fetch arbitrary URLs on your behalf for those), and
// anything else is passed through as a remote URL source.
func parseImageSource(url string) *anthropicImageSource {
	if mediaType, data, ok := strings.Cut(strings.TrimPrefix(url, "data:"), ";base64,"); ok && strings.Contains(url, "data:") {
		return &anthropicImageSource{Type: "base64", MediaType: mediaType, Data: data}
	}
	return &anthropicImageSource{Type: "url", URL: url}
}

// translateToolChoice maps OpenAI's tool_choice values to Anthropic's.
// Callers must handle "none" separately (see toolChoiceIsNone) before
// reaching here — it isn't a valid input to this function.
func translateToolChoice(raw json.RawMessage) any {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "required":
			return map[string]string{"type": "any"}
		default:
			return map[string]string{"type": "auto"}
		}
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Type == "function" {
		return map[string]string{"type": "tool", "name": obj.Function.Name}
	}
	return map[string]string{"type": "auto"}
}

// --- Anthropic-side response shapes ---

type anthropicResponse struct {
	Content    []anthropicResponseBlock `json:"content"`
	Role       string                   `json:"role"`
	Model      string                   `json:"model"`
	StopReason string                   `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type anthropicResponseBlock struct {
	Type  string          `json:"type"` // "text" or "tool_use"
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// Embeddings always errors: Anthropic has no embeddings API. Fails
// loudly rather than returning an empty success.
func (p *AnthropicProvider) Embeddings(ctx context.Context, model string, rawBody []byte) ([]byte, Usage, int, error) {
	return nil, Usage{}, 0, fmt.Errorf("anthropic has no embeddings API; route embeddings requests to an openai/groq/together model instead")
}

func (p *AnthropicProvider) ChatCompletion(ctx context.Context, model string, rawBody []byte) ([]byte, Usage, string, int, error) {
	aReq, err := buildAnthropicRequest(rawBody, model, false)
	if err != nil {
		return nil, Usage{}, "", 0, err
	}

	body, err := json.Marshal(aReq)
	if err != nil {
		return nil, Usage{}, "", 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, Usage{}, "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", p.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

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

	var aResp anthropicResponse
	if err := json.Unmarshal(respBody, &aResp); err != nil {
		return nil, Usage{}, "", resp.StatusCode, fmt.Errorf("parsing anthropic response: %w", err)
	}

	text, toolCalls := splitResponseBlocks(aResp.Content)
	finishReason := mapAnthropicStopReason(aResp.StopReason)
	usage := Usage{PromptTokens: aResp.Usage.InputTokens, CompletionTokens: aResp.Usage.OutputTokens}

	message := map[string]any{"role": "assistant", "content": text}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		if text == "" {
			message["content"] = nil
		}
	}

	openaiShaped := map[string]any{
		"id":      "chatcmpl-" + randomSuffix(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{"index": 0, "message": message, "finish_reason": finishReason},
		},
		"usage": map[string]any{
			"prompt_tokens":     usage.PromptTokens,
			"completion_tokens": usage.CompletionTokens,
			"total_tokens":      usage.PromptTokens + usage.CompletionTokens,
		},
	}
	outBody, err := json.Marshal(openaiShaped)
	if err != nil {
		return nil, Usage{}, "", resp.StatusCode, err
	}

	return outBody, usage, finishReason, resp.StatusCode, nil
}

// splitResponseBlocks separates an Anthropic response's content blocks
// into accumulated text and OpenAI-shaped tool_calls.
func splitResponseBlocks(blocks []anthropicResponseBlock) (text string, toolCalls []map[string]any) {
	var sb strings.Builder
	for _, b := range blocks {
		switch b.Type {
		case "text":
			sb.WriteString(b.Text)
		case "tool_use":
			input := b.Input
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   b.ID,
				"type": "function",
				"function": map[string]any{
					"name":      b.Name,
					"arguments": string(input),
				},
			})
		}
	}
	return sb.String(), toolCalls
}

func mapAnthropicStopReason(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return reason
	}
}

// --- Streaming ---

// anthropicStreamEvent covers the fields used across Anthropic's
// message_start/content_block_start/content_block_delta/message_delta
// event payloads; unused fields for a given event type are simply zero.
type anthropicStreamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message struct {
		Usage struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// streamToolCallState accumulates one tool_use block's partial_json
// deltas. seq is the tool call's position among tool calls only (0, 1, 2,
// ...) — the OpenAI tool_calls[].index a client keys its reconstruction
// on — which is not the same as Anthropic's content block index, since
// text blocks can appear between tool_use blocks.
type streamToolCallState struct {
	id, name string
	seq      int
	args     strings.Builder
}

// anthropicStreamSession holds an already-established, successful
// streaming HTTP response, ready to be relayed.
type anthropicStreamSession struct {
	resp  *http.Response
	model string
}

func (p *AnthropicProvider) OpenStream(ctx context.Context, model string, rawBody []byte) (StreamSession, int, error) {
	aReq, err := buildAnthropicRequest(rawBody, model, true)
	if err != nil {
		return nil, 0, err
	}
	body, err := json.Marshal(aReq)
	if err != nil {
		return nil, 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", p.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
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
	return &anthropicStreamSession{resp: resp, model: model}, resp.StatusCode, nil
}

func (s *anthropicStreamSession) Close() error {
	return s.resp.Body.Close()
}

func (s *anthropicStreamSession) Relay(w *bufio.Writer) (string, []map[string]any, Usage, string, error) {
	defer s.resp.Body.Close()

	id := "chatcmpl-" + randomSuffix()
	created := time.Now().Unix()
	usage := Usage{}
	finishReason := ""
	var text strings.Builder
	var toolCallOrder []int
	toolBlocks := map[int]*streamToolCallState{}

	scanner := bufio.NewScanner(s.resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var lastEventType string
	for scanner.Scan() {
		line := scanner.Text()
		if eventType, ok := strings.CutPrefix(line, "event: "); ok {
			lastEventType = eventType
			continue
		}
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || payload == "" {
			continue
		}

		var evt anthropicStreamEvent
		if err := json.Unmarshal([]byte(payload), &evt); err != nil {
			continue
		}

		switch lastEventType {
		case "message_start":
			usage.PromptTokens = evt.Message.Usage.InputTokens

		case "content_block_start":
			if evt.ContentBlock.Type == "tool_use" {
				st := &streamToolCallState{id: evt.ContentBlock.ID, name: evt.ContentBlock.Name, seq: len(toolCallOrder)}
				toolBlocks[evt.Index] = st
				toolCallOrder = append(toolCallOrder, evt.Index)
				// First chunk for this tool call: id/type/name, same as a
				// real OpenAI stream's opening tool_calls delta.
				chunk := openAIChunk(id, s.model, created, map[string]any{"tool_calls": []map[string]any{{
					"index": st.seq, "id": st.id, "type": "function",
					"function": map[string]any{"name": st.name, "arguments": ""},
				}}}, nil)
				if err := writeSSEChunk(w, chunk); err != nil {
					return text.String(), finalizeAnthropicToolCalls(toolCallOrder, toolBlocks), usage, finishReason, err
				}
			}

		case "content_block_delta":
			switch evt.Delta.Type {
			case "text_delta":
				text.WriteString(evt.Delta.Text)
				chunk := openAIChunk(id, s.model, created, map[string]any{"content": evt.Delta.Text}, nil)
				if err := writeSSEChunk(w, chunk); err != nil {
					return text.String(), finalizeAnthropicToolCalls(toolCallOrder, toolBlocks), usage, finishReason, err
				}
			case "input_json_delta":
				if st, ok := toolBlocks[evt.Index]; ok {
					st.args.WriteString(evt.Delta.PartialJSON)
					// Relayed as-is, incrementally: a client concatenates
					// `arguments` fragments across chunks and only parses
					// the joined result, same as it already does for a
					// native OpenAI tool-call stream. No need to wait for
					// the block to close, and no need for each fragment to
					// be valid JSON on its own.
					if evt.Delta.PartialJSON != "" {
						chunk := openAIChunk(id, s.model, created, map[string]any{"tool_calls": []map[string]any{{
							"index":    st.seq,
							"function": map[string]any{"arguments": evt.Delta.PartialJSON},
						}}}, nil)
						if err := writeSSEChunk(w, chunk); err != nil {
							return text.String(), finalizeAnthropicToolCalls(toolCallOrder, toolBlocks), usage, finishReason, err
						}
					}
				}
			}

		case "message_delta":
			usage.CompletionTokens = evt.Usage.OutputTokens
			if evt.Delta.StopReason != "" {
				finishReason = mapAnthropicStopReason(evt.Delta.StopReason)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return text.String(), finalizeAnthropicToolCalls(toolCallOrder, toolBlocks), usage, finishReason, err
	}

	if finishReason == "" {
		finishReason = "stop"
	}
	if err := writeSSEChunk(w, openAIChunk(id, s.model, created, map[string]any{}, &finishReason)); err != nil {
		return text.String(), finalizeAnthropicToolCalls(toolCallOrder, toolBlocks), usage, finishReason, err
	}
	if err := writeSSEDone(w); err != nil {
		return text.String(), finalizeAnthropicToolCalls(toolCallOrder, toolBlocks), usage, finishReason, err
	}

	return text.String(), finalizeAnthropicToolCalls(toolCallOrder, toolBlocks), usage, finishReason, nil
}

// finalizeAnthropicToolCalls converts accumulated per-index tool_use
// block state into the same shape ChatCompletion (non-streaming) and the
// OpenAI-compat streamer use, so cache reconstruction is uniform.
func finalizeAnthropicToolCalls(order []int, blocks map[int]*streamToolCallState) []map[string]any {
	if len(order) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(order))
	for _, idx := range order {
		st := blocks[idx]
		args := st.args.String()
		if !json.Valid([]byte(args)) {
			args = "{}"
		}
		out = append(out, map[string]any{
			"id":   st.id,
			"type": "function",
			"function": map[string]any{
				"name":      st.name,
				"arguments": args,
			},
		})
	}
	return out
}
