package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
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
	// "none" is deliberately not a case here: it's intercepted before
	// translateToolChoice is ever called (see TestBuildAnthropicRequest_ToolChoiceNoneOmitsTools).
	cases := map[string]map[string]string{
		`"auto"`:     {"type": "auto"},
		`"required"`: {"type": "any"},
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

// TestBuildAnthropicRequest_ToolChoiceNoneOmitsTools verifies the actual
// fix for the "none" gap: instead of translating to a hopeful "auto" and
// still sending tool definitions (which leaves the model free to call one
// anyway), tool_choice: "none" now strips `tools` entirely, which is the
// only way to guarantee Anthropic can't call a tool.
func TestBuildAnthropicRequest_ToolChoiceNoneOmitsTools(t *testing.T) {
	body := `{
		"model": "claude-sonnet-5",
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [{"type": "function", "function": {"name": "get_weather", "parameters": {}}}],
		"tool_choice": "none"
	}`
	req, err := buildAnthropicRequest([]byte(body), "claude-sonnet-5", false)
	if err != nil {
		t.Fatalf("buildAnthropicRequest: %v", err)
	}
	if len(req.Tools) != 0 {
		t.Errorf("expected no tools sent when tool_choice is none, got %+v", req.Tools)
	}
	if req.ToolChoice != nil {
		t.Errorf("expected no tool_choice sent when tool_choice is none, got %+v", req.ToolChoice)
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

// TestRelay_ToolCallArgumentsStreamIncrementally verifies the fix for
// batched-not-incremental streaming: each input_json_delta fragment is
// relayed to the client as its own SSE chunk as it arrives, rather than
// being buffered and sent as one complete chunk when the block closes.
// It also verifies that a text block sandwiched between two tool_use
// blocks doesn't throw off the tool_calls[].index the client keys its
// reconstruction on (that index counts tool calls only, not content
// blocks in general).
func TestRelay_ToolCallArgumentsStreamIncrementally(t *testing.T) {
	sse := []string{
		event("message_start", `{"message":{"usage":{"input_tokens":10}}}`),
		event("content_block_start", `{"index":0,"content_block":{"type":"tool_use","id":"call_1","name":"get_weather"}}`),
		event("content_block_delta", `{"index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`),
		event("content_block_delta", `{"index":0,"delta":{"type":"input_json_delta","partial_json":"\"NYC\"}"}}`),
		event("content_block_stop", `{"index":0}`),
		event("content_block_start", `{"index":1,"content_block":{"type":"text"}}`),
		event("content_block_delta", `{"index":1,"delta":{"type":"text_delta","text":"checking..."}}`),
		event("content_block_stop", `{"index":1}`),
		event("content_block_start", `{"index":2,"content_block":{"type":"tool_use","id":"call_2","name":"get_time"}}`),
		event("content_block_delta", `{"index":2,"delta":{"type":"input_json_delta","partial_json":"{}"}}`),
		event("content_block_stop", `{"index":2}`),
		event("message_delta", `{"delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`),
	}
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(strings.Join(sse, "")))}
	session := &anthropicStreamSession{resp: resp, model: "claude-sonnet-5"}

	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	_, toolCalls, _, finishReason, err := session.Relay(w)
	if err != nil {
		t.Fatalf("Relay: %v", err)
	}
	w.Flush()

	if finishReason != "tool_calls" {
		t.Errorf("finishReason = %q, want tool_calls", finishReason)
	}
	if len(toolCalls) != 2 {
		t.Fatalf("expected 2 finalized tool calls, got %d: %+v", len(toolCalls), toolCalls)
	}
	if args := toolCalls[0]["function"].(map[string]any)["arguments"]; args != `{"city":"NYC"}` {
		t.Errorf("call_1 arguments = %v, want full joined JSON", args)
	}

	out := buf.String()
	// The two partial_json fragments must be relayed as separate chunks,
	// not buffered and joined into one chunk at content_block_stop.
	if !strings.Contains(out, `"arguments":"{\"city\":"`) {
		t.Errorf("expected first fragment as its own chunk, got: %s", out)
	}
	if !strings.Contains(out, `"arguments":"\"NYC\"}"`) {
		t.Errorf("expected second fragment as its own chunk, got: %s", out)
	}
	if strings.Contains(out, `"arguments":"{\"city\":\"NYC\"}"`) {
		t.Errorf("fragments were batched into one chunk instead of streamed incrementally: %s", out)
	}
	// The second tool call (after an intervening text block) must be
	// addressed as tool_calls[].index 1, not 0 — otherwise a client
	// reconstructing by index merges it into the first tool call. (Map
	// keys marshal alphabetically, hence "id" before "index" below.)
	if !strings.Contains(out, `"id":"call_2","index":1,"type":"function"`) {
		t.Errorf("expected second tool call to open with index 1, got: %s", out)
	}
	if strings.Contains(out, `"id":"call_2","index":0,"type":"function"`) {
		t.Errorf("second tool call incorrectly opened with index 0 (collides with the first): %s", out)
	}
}

func event(eventType, data string) string {
	return "event: " + eventType + "\ndata: " + data + "\n\n"
}

// TestAnthropicProvider_MediaEndpointsAllError confirms Anthropic's three
// new Provider methods are deliberate, permanent "not supported" errors —
// Anthropic has no image generation or audio API of any kind, unlike
// Embeddings (also unsupported, but only because Anthropic just doesn't
// happen to offer it — same shape either way).
func TestAnthropicProvider_MediaEndpointsAllError(t *testing.T) {
	p := NewAnthropicProvider("https://api.anthropic.com/v1", "test-key")
	ctx := context.Background()

	if _, _, _, err := p.ImageGeneration(ctx, "claude-sonnet-5", nil); err == nil {
		t.Error("expected ImageGeneration to error, anthropic has no image API")
	}
	if _, _, _, err := p.AudioSpeech(ctx, "claude-sonnet-5", nil); err == nil {
		t.Error("expected AudioSpeech to error, anthropic has no TTS API")
	}
	if _, _, _, err := p.AudioTranscription(ctx, "claude-sonnet-5", strings.NewReader(""), "a.mp3", nil); err == nil {
		t.Error("expected AudioTranscription to error, anthropic has no STT API")
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
