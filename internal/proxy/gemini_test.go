package proxy

import (
	"encoding/json"
	"testing"
)

func TestParseGeminiInlineImage_DataURI(t *testing.T) {
	inline := parseGeminiInlineImage("data:image/png;base64,AAAA")
	if inline == nil {
		t.Fatal("expected an inline image block")
	}
	if inline.MimeType != "image/png" || inline.Data != "AAAA" {
		t.Fatalf("unexpected inline block: %+v", inline)
	}
}

func TestParseGeminiInlineImage_RemoteURLUnsupported(t *testing.T) {
	// Unlike Anthropic (which passes a remote URL through for the
	// provider to fetch), Gemini has no such option for inline content —
	// and ai-guard deliberately doesn't fetch it server-side (SSRF risk),
	// so this must come back nil rather than something half-translated.
	if inline := parseGeminiInlineImage("https://example.com/cat.png"); inline != nil {
		t.Fatalf("expected nil for a remote URL, got %+v", inline)
	}
}

func TestBuildGeminiRequest_SystemMessageExtracted(t *testing.T) {
	body := []byte(`{
		"model": "gemini-1.5-pro",
		"messages": [
			{"role": "system", "content": "You are terse."},
			{"role": "user", "content": "hi"}
		]
	}`)
	req, err := buildGeminiRequest(body, "gemini-1.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.SystemInstruction == nil || req.SystemInstruction.Parts[0].Text != "You are terse." {
		t.Fatalf("expected systemInstruction %q, got %+v", "You are terse.", req.SystemInstruction)
	}
	if len(req.Contents) != 1 || req.Contents[0].Role != "user" {
		t.Fatalf("expected exactly one user content, got %+v", req.Contents)
	}
}

func TestBuildGeminiRequest_AssistantRoleBecomesModel(t *testing.T) {
	body := []byte(`{
		"model": "gemini-1.5-pro",
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "hello"}
		]
	}`)
	req, err := buildGeminiRequest(body, "gemini-1.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(req.Contents) != 2 {
		t.Fatalf("expected 2 contents, got %d", len(req.Contents))
	}
	if req.Contents[0].Role != "user" {
		t.Fatalf("got role %q, want user", req.Contents[0].Role)
	}
	if req.Contents[1].Role != "model" {
		t.Fatalf("got role %q, want model (OpenAI's \"assistant\" translated)", req.Contents[1].Role)
	}
}

func TestBuildGeminiRequest_MultimodalContent(t *testing.T) {
	body := []byte(`{
		"model": "gemini-1.5-pro",
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "what is this?"},
				{"type": "image_url", "image_url": {"url": "data:image/jpeg;base64,ZZZZ"}}
			]}
		]
	}`)
	req, err := buildGeminiRequest(body, "gemini-1.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(req.Contents) != 1 {
		t.Fatalf("expected 1 content, got %d", len(req.Contents))
	}
	parts := req.Contents[0].Parts
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts (text + image), got %d: %+v", len(parts), parts)
	}
	if parts[0].Text != "what is this?" {
		t.Fatalf("unexpected text part: %+v", parts[0])
	}
	if parts[1].InlineData == nil || parts[1].InlineData.MimeType != "image/jpeg" {
		t.Fatalf("unexpected image part: %+v", parts[1])
	}
}

// This is the one genuinely tricky part of the Gemini translation: unlike
// OpenAI/Anthropic (which correlate a tool result to its call by an
// opaque ID carried on both sides), Gemini correlates by function NAME
// only. The "tool" role message here only carries tool_call_id — the
// name has to come from the earlier assistant message that issued the
// call, which is exactly what this test would catch if that lookup broke.
func TestBuildGeminiRequest_ToolResultCorrelatesToCallByName(t *testing.T) {
	body := []byte(`{
		"model": "gemini-1.5-pro",
		"messages": [
			{"role": "user", "content": "what's the weather in nyc?"},
			{"role": "assistant", "content": "", "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"nyc\"}"}}
			]},
			{"role": "tool", "tool_call_id": "call_1", "content": "72F and sunny"}
		],
		"tools": [
			{"type": "function", "function": {"name": "get_weather", "description": "gets weather", "parameters": {"type": "object"}}}
		]
	}`)
	req, err := buildGeminiRequest(body, "gemini-1.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(req.Contents) != 3 {
		t.Fatalf("expected 3 contents (user, model functionCall, user functionResponse), got %d: %+v", len(req.Contents), req.Contents)
	}

	modelMsg := req.Contents[1]
	if modelMsg.Role != "model" || len(modelMsg.Parts) != 1 || modelMsg.Parts[0].FunctionCall == nil {
		t.Fatalf("expected model message with one functionCall part, got %+v", modelMsg)
	}
	if modelMsg.Parts[0].FunctionCall.Name != "get_weather" {
		t.Fatalf("unexpected functionCall: %+v", modelMsg.Parts[0].FunctionCall)
	}

	resultMsg := req.Contents[2]
	if resultMsg.Role != "user" || len(resultMsg.Parts) != 1 || resultMsg.Parts[0].FunctionResponse == nil {
		t.Fatalf("expected user message with one functionResponse part, got %+v", resultMsg)
	}
	if resultMsg.Parts[0].FunctionResponse.Name != "get_weather" {
		t.Fatalf("functionResponse.Name = %q, want %q (looked up from the call's tool_call_id, not the id itself)",
			resultMsg.Parts[0].FunctionResponse.Name, "get_weather")
	}

	if len(req.Tools) != 1 || len(req.Tools[0].FunctionDeclarations) != 1 || req.Tools[0].FunctionDeclarations[0].Name != "get_weather" {
		t.Fatalf("expected one translated tool declaration, got %+v", req.Tools)
	}
}

func TestSplitGeminiParts_TextAndFunctionCall(t *testing.T) {
	candidates := []geminiCandidate{{
		Content: geminiContent{Parts: []geminiPart{
			{Text: "Let me check that. "},
			{FunctionCall: &geminiFunctionCall{Name: "get_weather", Args: json.RawMessage(`{"city":"nyc"}`)}},
		}},
	}}
	text, toolCalls := splitGeminiParts(candidates)
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
	if toolCalls[0]["id"] == "" {
		t.Fatal("expected a synthesized (non-empty) tool call id — Gemini has no call-id concept of its own")
	}
}

func TestSplitGeminiParts_EmptyFunctionCallArgsDefaultToEmptyObject(t *testing.T) {
	candidates := []geminiCandidate{{
		Content: geminiContent{Parts: []geminiPart{
			{FunctionCall: &geminiFunctionCall{Name: "ping"}},
		}},
	}}
	_, toolCalls := splitGeminiParts(candidates)
	fn := toolCalls[0]["function"].(map[string]any)
	if fn["arguments"] != "{}" {
		t.Fatalf("got arguments %q, want \"{}\"", fn["arguments"])
	}
}

func TestSplitGeminiParts_NoCandidates(t *testing.T) {
	text, toolCalls := splitGeminiParts(nil)
	if text != "" || toolCalls != nil {
		t.Fatalf("expected empty result for no candidates, got text=%q toolCalls=%+v", text, toolCalls)
	}
}

func TestMapGeminiFinishReason(t *testing.T) {
	cases := map[string]string{
		"STOP":                "stop",
		"MAX_TOKENS":          "length",
		"SAFETY":              "content_filter",
		"RECITATION":          "content_filter",
		"":                    "",
		"SOMETHING_NEW_ADDED": "stop", // unrecognized reasons degrade to "stop" rather than being passed through raw
	}
	for in, want := range cases {
		if got := mapGeminiFinishReason(in); got != want {
			t.Errorf("mapGeminiFinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}
