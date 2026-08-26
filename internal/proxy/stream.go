package proxy

import (
	"bufio"
	"encoding/json"
	"time"
)

// writeSSEChunk writes one SSE "data: <payload>" event and flushes
// immediately rather than waiting for fasthttp's own buffering.
func writeSSEChunk(w *bufio.Writer, payload []byte) error {
	if _, err := w.Write([]byte("data: ")); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	if _, err := w.Write([]byte("\n\n")); err != nil {
		return err
	}
	return w.Flush()
}

// writeSSEDone writes the terminal "data: [DONE]" event OpenAI-compatible
// clients look for to know the stream has ended.
func writeSSEDone(w *bufio.Writer) error {
	if _, err := w.Write([]byte("data: [DONE]\n\n")); err != nil {
		return err
	}
	return w.Flush()
}

// openAIChunk builds one OpenAI chat.completion.chunk payload. delta is
// the incremental content for this chunk (e.g. {"content": "Hello"});
// finishReason is nil until the final content chunk.
func openAIChunk(id, model string, created int64, delta map[string]any, finishReason *string) []byte {
	choice := map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason}
	chunk := map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
		"choices": []map[string]any{choice},
	}
	b, _ := json.Marshal(chunk)
	return b
}

// openAIUsageChunk builds the trailing usage-only chunk some providers
// send when stream_options.include_usage is set.
func openAIUsageChunk(id, model string, created int64, usage Usage) []byte {
	chunk := map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
		"choices": []any{},
		"usage": map[string]any{
			"prompt_tokens":     usage.PromptTokens,
			"completion_tokens": usage.CompletionTokens,
			"total_tokens":      usage.PromptTokens + usage.CompletionTokens,
		},
	}
	b, _ := json.Marshal(chunk)
	return b
}

// nonStreamOpenAIResponse rebuilds a ChatCompletion-shaped response from
// a completed stream's accumulated text/tool calls/usage, used to
// populate the cache after streaming finishes.
func nonStreamOpenAIResponse(id, model, text, finishReason string, usage Usage, toolCalls []map[string]any) []byte {
	message := map[string]any{"role": "assistant", "content": text}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		if text == "" {
			message["content"] = nil
		}
	}
	resp := map[string]any{
		"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": model,
		"choices": []map[string]any{
			{"index": 0, "message": message, "finish_reason": finishReason},
		},
		"usage": map[string]any{
			"prompt_tokens":     usage.PromptTokens,
			"completion_tokens": usage.CompletionTokens,
			"total_tokens":      usage.PromptTokens + usage.CompletionTokens,
		},
	}
	b, _ := json.Marshal(resp)
	return b
}

// cachedChatResponse is the subset of a cached response needed to
// synthesize a stream from it.
type cachedChatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content   string           `json:"content"`
			ToolCalls []map[string]any `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// streamFromCached synthesizes an SSE stream from a cached response for
// clients that request streaming even on a cache hit. Delivered as one
// content chunk (and one tool_calls delta if applicable) rather than
// reproducing the original timing.
func streamFromCached(w *bufio.Writer, cached []byte) error {
	var parsed cachedChatResponse
	if err := json.Unmarshal(cached, &parsed); err != nil {
		return err
	}
	text, finishReason := "", "stop"
	var toolCalls []map[string]any
	if len(parsed.Choices) > 0 {
		text = parsed.Choices[0].Message.Content
		toolCalls = parsed.Choices[0].Message.ToolCalls
		if parsed.Choices[0].FinishReason != "" {
			finishReason = parsed.Choices[0].FinishReason
		}
	}

	id := "chatcmpl-" + randomSuffix()
	created := time.Now().Unix()

	if text != "" {
		if err := writeSSEChunk(w, openAIChunk(id, parsed.Model, created, map[string]any{"role": "assistant", "content": text}, nil)); err != nil {
			return err
		}
	}
	for i, tc := range toolCalls {
		tc["index"] = i
		if err := writeSSEChunk(w, openAIChunk(id, parsed.Model, created, map[string]any{"tool_calls": []map[string]any{tc}}, nil)); err != nil {
			return err
		}
	}
	if err := writeSSEChunk(w, openAIChunk(id, parsed.Model, created, map[string]any{}, &finishReason)); err != nil {
		return err
	}
	return writeSSEDone(w)
}
