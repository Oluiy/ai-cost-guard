package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestWriteSSEChunk_Framing(t *testing.T) {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	if err := writeSSEChunk(w, []byte(`{"a":1}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := buf.String(); got != "data: {\"a\":1}\n\n" {
		t.Fatalf("got %q", got)
	}
}

func TestWriteSSEDone(t *testing.T) {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	if err := writeSSEDone(w); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := buf.String(); got != "data: [DONE]\n\n" {
		t.Fatalf("got %q", got)
	}
}

func TestOpenAIChunk_Shape(t *testing.T) {
	b := openAIChunk("id1", "gpt-4o", 100, map[string]any{"content": "hi"}, nil)
	var parsed map[string]any
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("chunk is not valid JSON: %v", err)
	}
	if parsed["object"] != "chat.completion.chunk" {
		t.Fatalf("got object %v, want chat.completion.chunk", parsed["object"])
	}
	choices := parsed["choices"].([]any)
	choice := choices[0].(map[string]any)
	if choice["finish_reason"] != nil {
		t.Fatalf("expected nil finish_reason for a non-final chunk, got %v", choice["finish_reason"])
	}
	delta := choice["delta"].(map[string]any)
	if delta["content"] != "hi" {
		t.Fatalf("got delta content %v, want \"hi\"", delta["content"])
	}
}

func TestStreamFromCached_TextOnly(t *testing.T) {
	cached := []byte(`{
		"model": "gpt-4o",
		"choices": [{"message": {"content": "hello there"}, "finish_reason": "stop"}]
	}`)
	var buf bytes.Buffer
	if err := streamFromCached(bufio.NewWriter(&buf), cached); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `"content":"hello there"`) {
		t.Fatalf("expected synthesized content chunk, got: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("expected finish_reason chunk, got: %s", out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
		t.Fatalf("expected stream to end with [DONE], got: %s", out)
	}
}

func TestStreamFromCached_WithToolCalls(t *testing.T) {
	cached := []byte(`{
		"model": "gpt-4o",
		"choices": [{
			"message": {"content": "", "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{}"}}
			]},
			"finish_reason": "tool_calls"
		}]
	}`)
	var buf bytes.Buffer
	if err := streamFromCached(bufio.NewWriter(&buf), cached); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `"tool_calls"`) {
		t.Fatalf("expected a tool_calls delta chunk, got: %s", out)
	}
	if !strings.Contains(out, `get_weather`) {
		t.Fatalf("expected the cached tool call to survive synthesis, got: %s", out)
	}
	// Empty text content shouldn't produce a spurious empty content chunk.
	if strings.Count(out, `"content":""`) > 0 {
		t.Fatalf("did not expect an empty content chunk when there's no text, got: %s", out)
	}
}

func TestNonStreamOpenAIResponse_IncludesToolCalls(t *testing.T) {
	toolCalls := []map[string]any{
		{"id": "call_1", "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}},
	}
	b := nonStreamOpenAIResponse("id1", "gpt-4o", "", "tool_calls", Usage{PromptTokens: 5, CompletionTokens: 3}, toolCalls)

	var parsed map[string]any
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	choices := parsed["choices"].([]any)
	message := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != nil {
		t.Fatalf("expected nil content when only tool_calls are present, got %v", message["content"])
	}
	if _, ok := message["tool_calls"]; !ok {
		t.Fatalf("expected tool_calls to be present in reconstructed response: %+v", message)
	}
}

func TestNonStreamOpenAIResponse_PlainText(t *testing.T) {
	b := nonStreamOpenAIResponse("id1", "gpt-4o", "hello", "stop", Usage{PromptTokens: 1, CompletionTokens: 1}, nil)
	var parsed map[string]any
	json.Unmarshal(b, &parsed)
	choices := parsed["choices"].([]any)
	message := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "hello" {
		t.Fatalf("got content %v, want \"hello\"", message["content"])
	}
	if _, ok := message["tool_calls"]; ok {
		t.Fatalf("did not expect a tool_calls key when there are none: %+v", message)
	}
}
