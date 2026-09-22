package proxy

import (
	"encoding/json"
)

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
