package proxy

import (
	"encoding/json"
	"testing"
)

func TestParseImageSource_DataURI(t *testing.T) {
	src := parseImageSource("data:image/png;base64,AAAA")
	if src.Type != "base64" {
		t.Fatalf("got type %q, want base64", src.Type)
	}
	if src.MediaType != "image/png" {
		t.Fatalf("got media type %q, want image/png", src.MediaType)
	}
	if src.Data != "AAAA" {
		t.Fatalf("got data %q, want AAAA", src.Data)
	}
}

func TestParseImageSource_RemoteURL(t *testing.T) {
	src := parseImageSource("https://example.com/cat.png")
	if src.Type != "url" {
		t.Fatalf("got type %q, want url", src.Type)
	}
	if src.URL != "https://example.com/cat.png" {
		t.Fatalf("got url %q, want the original url unchanged", src.URL)
	}
}

func TestBuildAnthropicRequest_SystemMessageExtracted(t *testing.T) {
	body := []byte(`{
		"model": "claude-3-5-sonnet",
		"messages": [
			{"role": "system", "content": "You are terse."},
			{"role": "user", "content": "hi"}
		]
	}`)
	req, err := buildAnthropicRequest(body, "claude-3-5-sonnet", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.System != "You are terse." {
		t.Fatalf("got system %q, want %q", req.System, "You are terse.")
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
		t.Fatalf("expected exactly one user message, got %+v", req.Messages)
	}
}

func TestBuildAnthropicRequest_MultimodalContent(t *testing.T) {
	body := []byte(`{
		"model": "claude-3-5-sonnet",
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "what is this?"},
				{"type": "image_url", "image_url": {"url": "data:image/jpeg;base64,ZZZZ"}}
			]}
		]
	}`)
	req, err := buildAnthropicRequest(body, "claude-3-5-sonnet", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(req.Messages))
	}
	blocks := req.Messages[0].Content
	if len(blocks) != 2 {
		t.Fatalf("expected 2 content blocks (text + image), got %d: %+v", len(blocks), blocks)
	}
	if blocks[0].Type != "text" || blocks[0].Text != "what is this?" {
		t.Fatalf("unexpected text block: %+v", blocks[0])
	}
	if blocks[1].Type != "image" || blocks[1].Source == nil || blocks[1].Source.Type != "base64" {
		t.Fatalf("unexpected image block: %+v", blocks[1])
	}
}

func TestBuildAnthropicRequest_ToolCallsAndResultsTranslated(t *testing.T) {
	body := []byte(`{
		"model": "claude-3-5-sonnet",
		"messages": [
			{"role": "user", "content": "what's the weather in nyc?"},
			{"role": "assistant", "content": "", "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"nyc\"}"}}
			]},
			{"role": "tool", "tool_call_id": "call_1", "content": "72F and sunny"}
		],
		"tools": [
			{"type": "function", "function": {"name": "get_weather", "description": "gets weather", "parameters": {"type": "object"}}}
		],
		"tool_choice": {"type": "function", "function": {"name": "get_weather"}}
	}`)
	req, err := buildAnthropicRequest(body, "claude-3-5-sonnet", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("expected 3 messages (user, assistant tool_use, user tool_result), got %d: %+v", len(req.Messages), req.Messages)
	}

	assistantMsg := req.Messages[1]
	if assistantMsg.Role != "assistant" || len(assistantMsg.Content) != 1 || assistantMsg.Content[0].Type != "tool_use" {
		t.Fatalf("expected assistant message with one tool_use block, got %+v", assistantMsg)
	}
	if assistantMsg.Content[0].Name != "get_weather" || assistantMsg.Content[0].ID != "call_1" {
		t.Fatalf("unexpected tool_use block: %+v", assistantMsg.Content[0])
	}

	toolResultMsg := req.Messages[2]
	if toolResultMsg.Role != "user" || len(toolResultMsg.Content) != 1 || toolResultMsg.Content[0].Type != "tool_result" {
		t.Fatalf("expected user message with one tool_result block, got %+v", toolResultMsg)
	}
	if toolResultMsg.Content[0].ToolUseID != "call_1" || toolResultMsg.Content[0].Content != "72F and sunny" {
		t.Fatalf("unexpected tool_result block: %+v", toolResultMsg.Content[0])
	}

	if len(req.Tools) != 1 || req.Tools[0].Name != "get_weather" {
		t.Fatalf("expected one translated tool, got %+v", req.Tools)
	}

	choice, ok := req.ToolChoice.(map[string]string)
	if !ok || choice["type"] != "tool" || choice["name"] != "get_weather" {
		t.Fatalf("unexpected tool_choice translation: %+v", req.ToolChoice)
	}
}

func TestTranslateToolChoice(t *testing.T) {
	cases := map[string]map[string]string{
		`"auto"`:     {"type": "auto"},
		`"required"`: {"type": "any"},
		`"none"`:     {"type": "auto"}, // documented best-effort fallback
		`{"type":"function","function":{"name":"foo"}}`: {"type": "tool", "name": "foo"},
	}
	for raw, want := range cases {
		got, ok := translateToolChoice(json.RawMessage(raw)).(map[string]string)
		if !ok {
			t.Errorf("translateToolChoice(%s): unexpected type %T", raw, got)
			continue
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("translateToolChoice(%s) = %v, want %v", raw, got, want)
			}
		}
	}
}

func TestSplitResponseBlocks_TextAndToolUse(t *testing.T) {
	blocks := []anthropicResponseBlock{
		{Type: "text", Text: "Let me check that. "},
		{Type: "tool_use", ID: "toolu_1", Name: "get_weather", Input: json.RawMessage(`{"city":"nyc"}`)},
	}
	text, toolCalls := splitResponseBlocks(blocks)
	if text != "Let me check that. " {
		t.Fatalf("got text %q", text)
	}
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(toolCalls))
	}
	fn, ok := toolCalls[0]["function"].(map[string]any)
	if !ok || fn["name"] != "get_weather" || fn["arguments"] != `{"city":"nyc"}` {
		t.Fatalf("unexpected tool call shape: %+v", toolCalls[0])
	}
}

func TestSplitResponseBlocks_EmptyToolUseInputDefaultsToEmptyObject(t *testing.T) {
	blocks := []anthropicResponseBlock{
		{Type: "tool_use", ID: "toolu_1", Name: "ping"},
	}
	_, toolCalls := splitResponseBlocks(blocks)
	fn := toolCalls[0]["function"].(map[string]any)
	if fn["arguments"] != "{}" {
		t.Fatalf("got arguments %q, want \"{}\"", fn["arguments"])
	}
}

func TestMapAnthropicStopReason(t *testing.T) {
	cases := map[string]string{
		"end_turn":      "stop",
		"stop_sequence": "stop",
		"max_tokens":    "length",
		"tool_use":      "tool_calls",
		"something_new": "something_new", // unrecognized reasons pass through rather than being swallowed
	}
	for in, want := range cases {
		if got := mapAnthropicStopReason(in); got != want {
			t.Errorf("mapAnthropicStopReason(%q) = %q, want %q", in, got, want)
		}
	}
}
