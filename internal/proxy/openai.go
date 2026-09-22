package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
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
	// Kind distinguishes OpenAI from Groq/Together for the endpoints that
	// aren't uniformly OpenAI-shaped across all three (images, audio) —
	// chat/completions and embeddings don't need it, since those genuinely
	// are identical across all three. Empty/"openai" is the default (full
	// pass-through on every endpoint); see ImageGeneration/AudioSpeech/
	// AudioTranscription below for what each Kind actually supports.
	Kind string
}

// NewOpenAICompatProvider builds a provider for an OpenAI-compatible
// endpoint. kind is "openai", "groq", or "together" — see Kind's doc comment.
func NewOpenAICompatProvider(baseURL, apiKey, kind string) *OpenAICompatProvider {
	return &OpenAICompatProvider{
		BaseURL: baseURL,
		APIKey:  apiKey,
		Client:  &http.Client{Timeout: 120 * time.Second},
		Kind:    kind,
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

// ImageGeneration is a pass-through for OpenAI and Together (both serve
// /images/generations at this same path with an OpenAI-compatible body —
// Together's exact response field names weren't independently confirmed
// during research; verify against a live key before relying on this).
// Groq has no image generation API.
func (p *OpenAICompatProvider) ImageGeneration(ctx context.Context, model string, rawBody []byte) ([]byte, int, int, error) {
	if p.Kind == "groq" {
		return nil, 0, 0, fmt.Errorf("groq has no image generation API; route image requests to an openai/gemini/together model instead")
	}
	body, err := setModel(rawBody, model)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("rewriting model in request: %w", err)
	}
	if p.Kind == "together" {
		// Together's response_format enum is "base64"/"url", not
		// OpenAI's "b64_json"/"url" — confirmed against Together's API
		// reference. Forwarding "b64_json" verbatim gets rejected
		// upstream as an invalid enum value.
		body, err = translateTogetherImageRequest(body)
		if err != nil {
			return nil, 0, 0, fmt.Errorf("translating request for together: %w", err)
		}
	}
	n := requestedImageCount(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/images/generations", bytes.NewReader(body))
	if err != nil {
		return nil, 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.APIKey)

	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	defer resp.Body.Close()

	respBody, err := readUpstreamBody(resp.Body)
	if err != nil {
		return nil, 0, resp.StatusCode, err
	}
	if resp.StatusCode >= 400 {
		return respBody, 0, resp.StatusCode, fmt.Errorf("upstream returned status %d", resp.StatusCode)
	}
	return respBody, n, resp.StatusCode, nil
}

// translateTogetherImageRequest rewrites the two OpenAI image parameters
// Together spells differently. Both verified against Together's own API
// reference; `prompt`, `model`, `n`, and `seed` are already identical in
// both and pass through untouched.
//
//   - response_format: OpenAI's "b64_json" is Together's "base64". "url"
//     is the same word in both. Absent stays absent.
//   - size: OpenAI takes one "1024x1024" string; Together takes separate
//     numeric width/height. Left untranslated, Together ignores `size`
//     entirely and silently renders at its own default dimensions, which
//     is worse than an error — the caller gets a differently-sized image
//     than it asked for with nothing to indicate why.
func translateTogetherImageRequest(body []byte) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if payload["response_format"] == "b64_json" {
		payload["response_format"] = "base64"
	}
	if size, ok := payload["size"].(string); ok {
		w, h, err := parseImageSize(size)
		if err != nil {
			return nil, err
		}
		delete(payload, "size")
		payload["width"], payload["height"] = w, h
	}
	return json.Marshal(payload)
}

// parseImageSize splits OpenAI's "<width>x<height>" size string.
func parseImageSize(size string) (int, int, error) {
	wStr, hStr, ok := strings.Cut(size, "x")
	if !ok {
		return 0, 0, fmt.Errorf("size %q is not in <width>x<height> form", size)
	}
	w, err := strconv.Atoi(wStr)
	if err != nil {
		return 0, 0, fmt.Errorf("size %q has a non-numeric width", size)
	}
	h, err := strconv.Atoi(hStr)
	if err != nil {
		return 0, 0, fmt.Errorf("size %q has a non-numeric height", size)
	}
	return w, h, nil
}

func requestedImageCount(body []byte) int {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return 1
	}
	if v, ok := payload["n"].(float64); ok && v > 0 {
		return int(v)
	}
	return 1
}

// AudioSpeech is a pass-through for OpenAI and Together. Groq's only TTS
// offering (Orpheus) is explicitly preview/evaluation-only per Groq's own
// docs, so it's deliberately excluded rather than wired as if GA.
func (p *OpenAICompatProvider) AudioSpeech(ctx context.Context, model string, rawBody []byte) ([]byte, int, int, error) {
	// GROQ-TTS-PREVIEW: Groq's Orpheus TTS (canopylabs/orpheus-*) is
	// allowed through here. Groq itself labels these models preview /
	// "for evaluation purposes only", with a 200-character input limit
	// per request. To remove: restore the block below, which rejects all
	// Groq speech requests.
	//
	// if p.Kind == "groq" {
	// 	return nil, 0, 0, fmt.Errorf("groq's text-to-speech models are preview/evaluation-only and not wired into this gateway; route audio/speech requests to an openai/gemini/together model instead")
	// }
	body, err := setModel(rawBody, model)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("rewriting model in request: %w", err)
	}
	var payload map[string]any
	charCount := 0
	if json.Unmarshal(body, &payload) == nil {
		if s, ok := payload["input"].(string); ok {
			charCount = len(s)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/audio/speech", bytes.NewReader(body))
	if err != nil {
		return nil, 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.APIKey)

	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	defer resp.Body.Close()

	// Audio, not JSON, on success — readUpstreamBody just size-caps the
	// read, it doesn't assume a content type.
	respBody, err := readUpstreamBody(resp.Body)
	if err != nil {
		return nil, 0, resp.StatusCode, err
	}
	if resp.StatusCode >= 400 {
		return respBody, 0, resp.StatusCode, fmt.Errorf("upstream returned status %d", resp.StatusCode)
	}
	return respBody, charCount, resp.StatusCode, nil
}

// AudioTranscription is a pass-through for all three (OpenAI Whisper,
// Groq's whisper-large-v3(-turbo), Together's hosted Whisper) — all three
// serve /audio/transcriptions as multipart/form-data with an
// OpenAI-compatible response shape.
func (p *OpenAICompatProvider) AudioTranscription(ctx context.Context, model string, audio io.Reader, filename string, formFields map[string]string) ([]byte, float64, int, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return nil, 0, 0, err
	}
	if _, err := io.Copy(part, audio); err != nil {
		return nil, 0, 0, err
	}
	if err := mw.WriteField("model", togetherUpstreamModel(p.Kind, model)); err != nil {
		return nil, 0, 0, err
	}
	for k, v := range formFields {
		if k == "model" || k == "file" {
			continue // "model" is set above (already rewritten to the resolved model); "file" was consumed above
		}
		if err := mw.WriteField(k, v); err != nil {
			return nil, 0, 0, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, 0, 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/audio/transcriptions", &buf)
	if err != nil {
		return nil, 0, 0, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+p.APIKey)

	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	defer resp.Body.Close()

	respBody, err := readUpstreamBody(resp.Body)
	if err != nil {
		return nil, 0, resp.StatusCode, err
	}
	if resp.StatusCode >= 400 {
		return respBody, 0, resp.StatusCode, fmt.Errorf("upstream returned status %d", resp.StatusCode)
	}

	var parsed struct {
		Duration float64 `json:"duration"`
	}
	json.Unmarshal(respBody, &parsed) // duration is best-effort; 0 falls back to the pre-flight byte-size estimate
	return respBody, parsed.Duration, resp.StatusCode, nil
}

// togetherUpstreamModel maps FitGuard's routing-safe alias
// "together/whisper-large-v3" to Together's actual catalog id
// ("openai/whisper-large-v3") before it goes upstream — see the alias's
// doc comment in internal/cost/cost.go for why the alias exists at all
// (Together's real id would otherwise be misrouted to the openai
// provider). Every other model name passes through unchanged.
func togetherUpstreamModel(kind, model string) string {
	if kind == "together" && model == "together/whisper-large-v3" {
		return "openai/whisper-large-v3"
	}
	return model
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
// streamed deltas: id/name arrive once, arguments arrive split across
// many deltas that must be concatenated, not replaced.
type toolCallAccumulator struct {
	id, name string
	args     strings.Builder
}

// prepareStreamBody rewrites rawBody's model, forces stream:true, and
// requests a trailing usage chunk so cost tracking can use real token
// counts instead of estimating from streamed text.
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
			// Client disconnected; return what's accumulated so far so it's still logged.
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
		// No usage chunk from the provider; estimate rather than bill as free.
		var payload map[string]any
		if err := json.Unmarshal(s.requestBody, &payload); err == nil {
			usage.PromptTokens = cost.EstimatePromptTokens(payload)
		}
		usage.CompletionTokens = len(text.String()) / 4
	}

	return text.String(), finalizeToolCalls(toolCallOrder, toolCalls), usage, finishReason, nil
}

// finalizeToolCalls converts accumulated per-index tool call state into
// the same tool_calls shape used across providers and streaming modes.
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
