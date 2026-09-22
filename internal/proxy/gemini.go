package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Oluiy/ai-cost-guard/internal/cost"
)

// GeminiProvider translates OpenAI-shaped chat/completions and embeddings
// requests to and from Google's Generative Language API, including
// multimodal content, tool calling, and streaming.
type GeminiProvider struct {
	BaseURL string // e.g. "https://generativelanguage.googleapis.com/v1beta"
	APIKey  string
	Client  *http.Client
}

func NewGeminiProvider(baseURL, apiKey string) *GeminiProvider {
	return &GeminiProvider{
		BaseURL: baseURL,
		APIKey:  apiKey,
		Client:  &http.Client{Timeout: 120 * time.Second},
	}
}

// --- Gemini-side request shapes ---

type geminiRequest struct {
	Contents          []geminiContent         `json:"contents"`
	SystemInstruction *geminiContent          `json:"systemInstruction,omitempty"`
	GenerationConfig  *geminiGenerationConfig `json:"generationConfig,omitempty"`
	Tools             []geminiTool            `json:"tools,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	InlineData       *geminiInlineData       `json:"inlineData,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
}

type geminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type geminiFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type geminiFunctionResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type geminiGenerationConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFunctionDeclaration `json:"functionDeclarations"`
}

type geminiFunctionDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// buildGeminiRequest converts an OpenAI-shaped chat/completions request
// into Gemini's generateContent shape: system messages become
// systemInstruction, "assistant" becomes role "model", image_url parts
// become inlineData blocks, and tool_calls/results become
// functionCall/functionResponse parts.
func buildGeminiRequest(rawBody []byte, model string) (geminiRequest, error) {
	var oaiReq openAIChatRequest
	if err := json.Unmarshal(rawBody, &oaiReq); err != nil {
		return geminiRequest{}, fmt.Errorf("parsing request: %w", err)
	}

	gReq := geminiRequest{}
	if oaiReq.Temperature != nil || oaiReq.MaxTokens != nil {
		gReq.GenerationConfig = &geminiGenerationConfig{
			Temperature:     oaiReq.Temperature,
			MaxOutputTokens: oaiReq.MaxTokens,
		}
	}

	// Gemini correlates a function result to its call by name, not by
	// call ID like OpenAI does, so tool_call_id has to be resolved to a
	// name from the assistant message that issued it.
	toolCallNames := map[string]string{}
	for _, m := range oaiReq.Messages {
		for _, tc := range m.ToolCalls {
			toolCallNames[tc.ID] = tc.Function.Name
		}
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
			response := json.RawMessage(text)
			if !json.Valid(response) {
				b, _ := json.Marshal(map[string]string{"result": text})
				response = b
			}
			gReq.Contents = append(gReq.Contents, geminiContent{
				Role: "user",
				Parts: []geminiPart{{FunctionResponse: &geminiFunctionResponse{
					Name: toolCallNames[m.ToolCallID], Response: response,
				}}},
			})
			continue
		}

		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}

		var parts []geminiPart
		text, contentParts := parseOpenAIContent(m.Content)
		if text != "" {
			parts = append(parts, geminiPart{Text: text})
		}
		for _, part := range contentParts {
			switch part.Type {
			case "text":
				parts = append(parts, geminiPart{Text: part.Text})
			case "image_url":
				if part.ImageURL == nil {
					continue
				}
				if inline := parseGeminiInlineImage(part.ImageURL.URL); inline != nil {
					parts = append(parts, geminiPart{InlineData: inline})
				}
				// Remote URLs are dropped, not fetched: fitguard doesn't
				// make server-side requests to caller-supplied URLs.
			}
		}
		for _, tc := range m.ToolCalls {
			args := json.RawMessage(tc.Function.Arguments)
			if !json.Valid(args) {
				args = json.RawMessage("{}")
			}
			parts = append(parts, geminiPart{FunctionCall: &geminiFunctionCall{Name: tc.Function.Name, Args: args}})
		}
		if len(parts) == 0 {
			continue
		}
		gReq.Contents = append(gReq.Contents, geminiContent{Role: role, Parts: parts})
	}
	if len(systemParts) > 0 {
		gReq.SystemInstruction = &geminiContent{Parts: []geminiPart{{Text: strings.Join(systemParts, "\n\n")}}}
	}

	var funcDecls []geminiFunctionDeclaration
	for _, t := range oaiReq.Tools {
		if t.Type != "function" {
			continue
		}
		funcDecls = append(funcDecls, geminiFunctionDeclaration{
			Name: t.Function.Name, Description: t.Function.Description, Parameters: t.Function.Parameters,
		})
	}
	if len(funcDecls) > 0 {
		gReq.Tools = []geminiTool{{FunctionDeclarations: funcDecls}}
	}
	// tool_choice has no direct Gemini equivalent; left untranslated.

	return gReq, nil
}

// parseGeminiInlineImage returns an inline image block for a base64 data
// URI ("data:<mime>;base64,<data>"), or nil for a remote URL.
func parseGeminiInlineImage(url string) *geminiInlineData {
	mediaType, data, ok := strings.Cut(strings.TrimPrefix(url, "data:"), ";base64,")
	if !ok || !strings.Contains(url, "data:") {
		return nil
	}
	return &geminiInlineData{MimeType: mediaType, Data: data}
}

// --- Gemini-side response shapes ---

type geminiResponse struct {
	Candidates    []geminiCandidate   `json:"candidates"`
	UsageMetadata geminiUsageMetadata `json:"usageMetadata"`
}

type geminiCandidate struct {
	Content      geminiContent `json:"content"`
	FinishReason string        `json:"finishReason"`
	Index        int           `json:"index"`
}

type geminiUsageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

func mapGeminiFinishReason(reason string) string {
	switch reason {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return "content_filter"
	case "":
		return ""
	default:
		return "stop"
	}
}

// splitGeminiParts separates a Gemini response's first candidate into
// text and OpenAI-shaped tool_calls. Gemini has no call-ID concept, so
// an ID is synthesized to fill the OpenAI shape.
func splitGeminiParts(candidates []geminiCandidate) (text string, toolCalls []map[string]any) {
	if len(candidates) == 0 {
		return "", nil
	}
	var sb strings.Builder
	for i, part := range candidates[0].Content.Parts {
		if part.Text != "" {
			sb.WriteString(part.Text)
		}
		if part.FunctionCall != nil {
			args := part.FunctionCall.Args
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   fmt.Sprintf("call_%s_%d", randomSuffix(), i),
				"type": "function",
				"function": map[string]any{
					"name": part.FunctionCall.Name, "arguments": string(args),
				},
			})
		}
	}
	return sb.String(), toolCalls
}

// post is the shared request/response plumbing for Gemini's non-streaming
// endpoints: auth header, size-capped body read, common error shape.
func (p *GeminiProvider) post(ctx context.Context, url string, body []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Header form, not "?key=": a query-string key ends up in access logs.
	req.Header.Set("x-goog-api-key", p.APIKey)

	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	respBody, err := readUpstreamBody(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode >= 400 {
		return respBody, resp.StatusCode, fmt.Errorf("upstream returned status %d", resp.StatusCode)
	}
	return respBody, resp.StatusCode, nil
}

func (p *GeminiProvider) ChatCompletion(ctx context.Context, model string, rawBody []byte) ([]byte, Usage, string, int, error) {
	gReq, err := buildGeminiRequest(rawBody, model)
	if err != nil {
		return nil, Usage{}, "", 0, err
	}
	body, err := json.Marshal(gReq)
	if err != nil {
		return nil, Usage{}, "", 0, err
	}

	url := fmt.Sprintf("%s/models/%s:generateContent", p.BaseURL, model)
	respBody, statusCode, err := p.post(ctx, url, body)
	if err != nil {
		return respBody, Usage{}, "", statusCode, err
	}

	var gResp geminiResponse
	if err := json.Unmarshal(respBody, &gResp); err != nil {
		return nil, Usage{}, "", statusCode, fmt.Errorf("parsing gemini response: %w", err)
	}

	text, toolCalls := splitGeminiParts(gResp.Candidates)
	var finishReason string
	if len(gResp.Candidates) > 0 {
		finishReason = mapGeminiFinishReason(gResp.Candidates[0].FinishReason)
	}
	usage := Usage{PromptTokens: gResp.UsageMetadata.PromptTokenCount, CompletionTokens: gResp.UsageMetadata.CandidatesTokenCount}

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
		return nil, Usage{}, "", statusCode, err
	}
	return outBody, usage, finishReason, statusCode, nil
}

// --- Embeddings ---

type geminiEmbedContentRequest struct {
	Content geminiContent `json:"content"`
}

type geminiEmbedContentResponse struct {
	Embedding struct {
		Values []float64 `json:"values"`
	} `json:"embedding"`
}

type geminiBatchEmbedRequest struct {
	Requests []geminiBatchEmbedItem `json:"requests"`
}

type geminiBatchEmbedItem struct {
	Model   string        `json:"model"`
	Content geminiContent `json:"content"`
}

type geminiBatchEmbedResponse struct {
	Embeddings []struct {
		Values []float64 `json:"values"`
	} `json:"embeddings"`
}

func (p *GeminiProvider) Embeddings(ctx context.Context, model string, rawBody []byte) ([]byte, Usage, int, error) {
	var payload map[string]any
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		return nil, Usage{}, 0, fmt.Errorf("parsing request: %w", err)
	}

	var texts []string
	switch input := payload["input"].(type) {
	case string:
		texts = []string{input}
	case []any:
		for _, item := range input {
			if s, ok := item.(string); ok {
				texts = append(texts, s)
			}
		}
	}
	if len(texts) == 0 {
		return nil, Usage{}, 0, fmt.Errorf(`"input" must be a string or array of strings`)
	}

	var embeddings [][]float64
	var statusCode int
	var err error
	if len(texts) == 1 {
		var vals []float64
		vals, statusCode, err = p.embedOne(ctx, model, texts[0])
		embeddings = [][]float64{vals}
	} else {
		embeddings, statusCode, err = p.embedBatch(ctx, model, texts)
	}
	if err != nil {
		return nil, Usage{}, statusCode, err
	}

	data := make([]map[string]any, len(embeddings))
	for i, e := range embeddings {
		data[i] = map[string]any{"object": "embedding", "index": i, "embedding": e}
	}

	// Gemini's embedding endpoints don't return token usage, so it's
	// estimated from the request text rather than billed as free.
	promptTokens := cost.EstimateEmbeddingTokens(payload)
	usage := Usage{PromptTokens: promptTokens}

	respBody, err := json.Marshal(map[string]any{
		"object": "list", "data": data, "model": model,
		"usage": map[string]any{"prompt_tokens": promptTokens, "total_tokens": promptTokens},
	})
	if err != nil {
		return nil, Usage{}, statusCode, err
	}
	return respBody, usage, statusCode, nil
}

func (p *GeminiProvider) embedOne(ctx context.Context, model, text string) ([]float64, int, error) {
	reqBody, err := json.Marshal(geminiEmbedContentRequest{Content: geminiContent{Parts: []geminiPart{{Text: text}}}})
	if err != nil {
		return nil, 0, err
	}
	url := fmt.Sprintf("%s/models/%s:embedContent", p.BaseURL, model)
	respBody, statusCode, err := p.post(ctx, url, reqBody)
	if err != nil {
		return nil, statusCode, err
	}
	var parsed geminiEmbedContentResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, statusCode, fmt.Errorf("parsing gemini embedding response: %w", err)
	}
	return parsed.Embedding.Values, statusCode, nil
}

func (p *GeminiProvider) embedBatch(ctx context.Context, model string, texts []string) ([][]float64, int, error) {
	items := make([]geminiBatchEmbedItem, len(texts))
	for i, t := range texts {
		items[i] = geminiBatchEmbedItem{Model: "models/" + model, Content: geminiContent{Parts: []geminiPart{{Text: t}}}}
	}
	reqBody, err := json.Marshal(geminiBatchEmbedRequest{Requests: items})
	if err != nil {
		return nil, 0, err
	}
	url := fmt.Sprintf("%s/models/%s:batchEmbedContents", p.BaseURL, model)
	respBody, statusCode, err := p.post(ctx, url, reqBody)
	if err != nil {
		return nil, statusCode, err
	}
	var parsed geminiBatchEmbedResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, statusCode, fmt.Errorf("parsing gemini batch embedding response: %w", err)
	}
	out := make([][]float64, len(parsed.Embeddings))
	for i, e := range parsed.Embeddings {
		out[i] = e.Values
	}
	return out, statusCode, nil
}

// --- Streaming ---

type geminiStreamSession struct {
	resp  *http.Response
	model string
}

func (p *GeminiProvider) OpenStream(ctx context.Context, model string, rawBody []byte) (StreamSession, int, error) {
	gReq, err := buildGeminiRequest(rawBody, model)
	if err != nil {
		return nil, 0, err
	}
	body, err := json.Marshal(gReq)
	if err != nil {
		return nil, 0, err
	}

	url := fmt.Sprintf("%s/models/%s:streamGenerateContent?alt=sse", p.BaseURL, model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", p.APIKey)
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
	return &geminiStreamSession{resp: resp, model: model}, resp.StatusCode, nil
}

func (s *geminiStreamSession) Close() error {
	return s.resp.Body.Close()
}

// Relay reads Gemini's SSE stream, where each "data:" line is a complete
// GenerateContentResponse chunk, and translates each into an
// OpenAI-shaped chunk as it arrives.
func (s *geminiStreamSession) Relay(w *bufio.Writer) (string, []map[string]any, Usage, string, error) {
	defer s.resp.Body.Close()

	id := "chatcmpl-" + randomSuffix()
	created := time.Now().Unix()
	usage := Usage{}
	finishReason := ""
	var text strings.Builder
	var toolCallOrder []int
	toolCalls := map[int]*toolCallAccumulator{}
	nextToolIndex := 0

	scanner := bufio.NewScanner(s.resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || payload == "" {
			continue
		}
		var chunk geminiResponse
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if chunk.UsageMetadata.PromptTokenCount > 0 {
			usage.PromptTokens = chunk.UsageMetadata.PromptTokenCount
		}
		if chunk.UsageMetadata.CandidatesTokenCount > 0 {
			usage.CompletionTokens = chunk.UsageMetadata.CandidatesTokenCount
		}
		if len(chunk.Candidates) == 0 {
			continue
		}
		cand := chunk.Candidates[0]
		if cand.FinishReason != "" {
			finishReason = mapGeminiFinishReason(cand.FinishReason)
		}
		for _, part := range cand.Content.Parts {
			if part.Text != "" {
				text.WriteString(part.Text)
				if err := writeSSEChunk(w, openAIChunk(id, s.model, created, map[string]any{"content": part.Text}, nil)); err != nil {
					return text.String(), finalizeToolCalls(toolCallOrder, toolCalls), usage, finishReason, err
				}
			}
			if part.FunctionCall != nil {
				idx := nextToolIndex
				nextToolIndex++
				args := part.FunctionCall.Args
				if len(args) == 0 {
					args = json.RawMessage("{}")
				}
				acc := &toolCallAccumulator{id: fmt.Sprintf("call_%s_%d", randomSuffix(), idx), name: part.FunctionCall.Name}
				acc.args.WriteString(string(args))
				toolCalls[idx] = acc
				toolCallOrder = append(toolCallOrder, idx)
				delta := map[string]any{"tool_calls": []map[string]any{{
					"index": idx, "id": acc.id, "type": "function",
					"function": map[string]any{"name": acc.name, "arguments": string(args)},
				}}}
				if err := writeSSEChunk(w, openAIChunk(id, s.model, created, delta, nil)); err != nil {
					return text.String(), finalizeToolCalls(toolCallOrder, toolCalls), usage, finishReason, err
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return text.String(), finalizeToolCalls(toolCallOrder, toolCalls), usage, finishReason, err
	}

	if finishReason == "" {
		finishReason = "stop"
	}
	if err := writeSSEChunk(w, openAIChunk(id, s.model, created, map[string]any{}, &finishReason)); err != nil {
		return text.String(), finalizeToolCalls(toolCallOrder, toolCalls), usage, finishReason, err
	}
	if err := writeSSEDone(w); err != nil {
		return text.String(), finalizeToolCalls(toolCallOrder, toolCalls), usage, finishReason, err
	}
	return text.String(), finalizeToolCalls(toolCallOrder, toolCalls), usage, finishReason, nil
}

// --- Images and audio, via Gemini's unified /interactions endpoint ---
//
// Distinct from generateContent (used by ChatCompletion/Embeddings above):
// image and speech generation live behind this newer surface. Schema
// confirmed against Google's own current docs (ai.google.dev/api/interactions-api
// and .../docs/interactions-breaking-changes-may-2026, checked 2026-09):
// requests need the "Api-Revision: 2026-05-20" header and a "response_format"
// discriminator; responses are a "steps" array (the May 2026 schema — the
// prior flat "outputs"/"output_image" shape this replaced is gone as of
// that migration). Still not exercised against a live API key — the shapes
// below are correct per the docs, not proven by a real response yet.

const geminiAPIRevision = "2026-05-20"

type geminiInteractionRequest struct {
	Model            string                   `json:"model"`
	Input            string                   `json:"input"`
	ResponseFormat   *geminiResponseFormat    `json:"response_format,omitempty"`
	GenerationConfig *geminiInteractionGenCfg `json:"generation_config,omitempty"`
}

type geminiResponseFormat struct {
	Type string `json:"type"` // "image" or "audio"
}

type geminiInteractionGenCfg struct {
	SpeechConfig []geminiSpeechVoice `json:"speech_config,omitempty"`
}

type geminiSpeechVoice struct {
	Voice string `json:"voice"`
}

type geminiInteractionResponse struct {
	Steps []geminiInteractionStep `json:"steps"`
}

type geminiInteractionStep struct {
	Type    string                   `json:"type"` // "user_input", "model_output", ...
	Content []geminiInteractionMedia `json:"content"`
}

type geminiInteractionMedia struct {
	Type     string `json:"type"`           // "text", "image", "audio"
	Data     string `json:"data,omitempty"` // base64, for image/audio
	MimeType string `json:"mime_type,omitempty"`
}

// firstMediaOfType scans every step's content (not just "model_output"
// steps specifically — the docs don't guarantee media never appears
// elsewhere, and matching on content type directly is more robust than
// assuming step-type placement) for the first block of the given type.
func firstMediaOfType(steps []geminiInteractionStep, mediaType string) *geminiInteractionMedia {
	for _, step := range steps {
		for _, c := range step.Content {
			if c.Type == mediaType && c.Data != "" {
				return &c
			}
		}
	}
	return nil
}

func (p *GeminiProvider) ImageGeneration(ctx context.Context, model string, rawBody []byte) ([]byte, int, int, error) {
	var payload map[string]any
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		return nil, 0, 0, fmt.Errorf("parsing request: %w", err)
	}
	prompt, _ := payload["prompt"].(string)
	if prompt == "" {
		return nil, 0, 0, fmt.Errorf(`"prompt" is required`)
	}

	iReq := geminiInteractionRequest{
		Model:          model,
		Input:          prompt,
		ResponseFormat: &geminiResponseFormat{Type: "image"},
	}
	body, err := json.Marshal(iReq)
	if err != nil {
		return nil, 0, 0, err
	}

	respBody, statusCode, err := p.postInteraction(ctx, body)
	if err != nil {
		return respBody, 0, statusCode, err
	}

	var parsed geminiInteractionResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, 0, statusCode, fmt.Errorf("parsing gemini image response: %w", err)
	}
	media := firstMediaOfType(parsed.Steps, "image")
	if media == nil {
		return nil, 0, statusCode, fmt.Errorf("gemini response had no image output")
	}

	openaiShaped := map[string]any{
		"created": time.Now().Unix(),
		"data":    []map[string]any{{"b64_json": media.Data}},
	}
	outBody, err := json.Marshal(openaiShaped)
	if err != nil {
		return nil, 0, statusCode, err
	}
	return outBody, 1, statusCode, nil
}

func (p *GeminiProvider) AudioSpeech(ctx context.Context, model string, rawBody []byte) ([]byte, int, int, error) {
	var payload map[string]any
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		return nil, 0, 0, fmt.Errorf("parsing request: %w", err)
	}
	input, _ := payload["input"].(string)
	if input == "" {
		return nil, 0, 0, fmt.Errorf(`"input" is required`)
	}
	voice, _ := payload["voice"].(string)
	if voice == "" {
		voice = "Kore" // Gemini's documented default voice
	}

	iReq := geminiInteractionRequest{
		Model:            model,
		Input:            input,
		ResponseFormat:   &geminiResponseFormat{Type: "audio"},
		GenerationConfig: &geminiInteractionGenCfg{SpeechConfig: []geminiSpeechVoice{{Voice: voice}}},
	}
	body, err := json.Marshal(iReq)
	if err != nil {
		return nil, 0, 0, err
	}

	respBody, statusCode, err := p.postInteraction(ctx, body)
	if err != nil {
		return nil, 0, statusCode, err
	}

	var parsed geminiInteractionResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, 0, statusCode, fmt.Errorf("parsing gemini speech response: %w", err)
	}
	media := firstMediaOfType(parsed.Steps, "audio")
	if media == nil {
		return nil, 0, statusCode, fmt.Errorf("gemini response had no audio output")
	}
	audioBytes, err := base64.StdEncoding.DecodeString(media.Data)
	if err != nil {
		return nil, 0, statusCode, fmt.Errorf("decoding gemini audio response: %w", err)
	}
	return audioBytes, len(input), statusCode, nil
}

// postInteraction is like post, but targets /interactions and sets the
// Api-Revision header the new (May 2026) schema requires — generateContent
// (used by ChatCompletion/Embeddings) doesn't need it, so this doesn't
// touch the shared post() helper those use.
func (p *GeminiProvider) postInteraction(ctx context.Context, body []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/interactions", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", p.APIKey)
	req.Header.Set("Api-Revision", geminiAPIRevision)

	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	respBody, err := readUpstreamBody(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode >= 400 {
		return respBody, resp.StatusCode, fmt.Errorf("upstream returned status %d", resp.StatusCode)
	}
	return respBody, resp.StatusCode, nil
}

// AudioTranscription: Gemini understands audio as multimodal chat input
// (generateContent), but has no dedicated /v1/audio/transcriptions-shaped
// endpoint confirmed at time of writing — left unsupported rather than
// guessed at. Route transcription requests to openai/groq/together.
func (p *GeminiProvider) AudioTranscription(ctx context.Context, model string, audio io.Reader, filename string, formFields map[string]string) ([]byte, float64, int, error) {
	return nil, 0, 0, fmt.Errorf("gemini transcription is not supported by this gateway yet; route audio/transcriptions requests to an openai/groq/together model instead")
}
