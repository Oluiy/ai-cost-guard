package proxy

import (
	"bufio"
	"bytes"
	"context"

	"encoding/json"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/valyala/fasthttp"

	"github.com/Oluiy/ai-cost-guard/internal/budget"
	"github.com/Oluiy/ai-cost-guard/internal/cache"
	"github.com/Oluiy/ai-cost-guard/internal/config"
	"github.com/Oluiy/ai-cost-guard/internal/cost"
	"github.com/Oluiy/ai-cost-guard/internal/logging"
)

// backgroundOpTimeout bounds cache writes and cost-log inserts that run
// after the response is sent, so a stuck disk or Redis can't hang forever.
const backgroundOpTimeout = 5 * time.Second

// Handler wires together auth, caching, budgets, routing/fallback,
// provider translation, and cost logging for the chat completions
// endpoint.
type Handler struct {
	Cfg *config.Config
	// Settings holds the knobs the dashboard can change while running
	// (cache, fallback, per-user budgets). Read only through its
	// accessors — Cfg's copies of these fields are not safe to read
	// directly once Settings exists.
	Settings  *config.Settings
	Cache     cache.Cache
	Budget    budget.Enforcer
	Store     *logging.Store
	Providers map[string]Provider
}

// New builds a Handler, constructing one Provider per configured entry in
// cfg.Providers.
func New(cfg *config.Config, settings *config.Settings, c cache.Cache, enforcer budget.Enforcer, store *logging.Store) *Handler {
	providers := make(map[string]Provider, len(cfg.Providers))
	for name, p := range cfg.Providers {
		baseURL := p.BaseURL
		switch name {
		case "anthropic":
			if baseURL == "" {
				baseURL = "https://api.anthropic.com/v1"
			}
			providers[name] = NewAnthropicProvider(baseURL, p.APIKey)
		case "gemini":
			if baseURL == "" {
				baseURL = "https://generativelanguage.googleapis.com/v1beta"
			}
			providers[name] = NewGeminiProvider(baseURL, p.APIKey)
		case "groq":
			if baseURL == "" {
				baseURL = "https://api.groq.com/openai/v1"
			}
			providers[name] = NewOpenAICompatProvider(baseURL, p.APIKey, "groq")
		case "together":
			if baseURL == "" {
				baseURL = "https://api.together.xyz/v1"
			}
			providers[name] = NewOpenAICompatProvider(baseURL, p.APIKey, "together")
		default: // "openai" and any custom OpenAI-compatible provider
			if baseURL == "" {
				baseURL = "https://api.openai.com/v1"
			}
			providers[name] = NewOpenAICompatProvider(baseURL, p.APIKey, "openai")
		}
	}
	return &Handler{Cfg: cfg, Settings: settings, Cache: c, Budget: enforcer, Store: store, Providers: providers}
}

func errorJSON(message, typ string) fiber.Map {
	return fiber.Map{"error": fiber.Map{"message": message, "type": typ}}
}

// authenticate resolves the caller's user_id from their Authorization
// header against configured virtual keys. With no keys configured,
// fitguard is single-tenant: every caller is "default".
func (h *Handler) authenticate(c *fiber.Ctx) (userID string, ok bool) {
	// Read through Settings, not h.Cfg: keys are runtime-mutable (the
	// dashboard issues them, another instance may have), and reading the
	// map directly raced with those writes and never saw a reload.
	if !h.Settings.HasKeys() {
		return "default", true
	}
	auth := c.Get("Authorization")
	token, hasBearer := strings.CutPrefix(auth, "Bearer ")
	if !hasBearer || token == "" {
		return "", false
	}
	return h.Settings.UserForKey(token)
}

// ChatCompletions handles POST /v1/chat/completions.
func (h *Handler) ChatCompletions(c *fiber.Ctx) error {
	start := time.Now()
	rawBody := c.Body()

	userID, authOK := h.authenticate(c)
	if !authOK {
		return c.Status(fiber.StatusUnauthorized).JSON(errorJSON(
			"missing or invalid API key; pass Authorization: Bearer <fitguard key> (see `fitguard init`)", "invalid_api_key"))
	}

	var parsed map[string]any
	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errorJSON("invalid JSON body", "invalid_request_error"))
	}

	model, _ := parsed["model"].(string)
	if model == "" {
		return c.Status(fiber.StatusBadRequest).JSON(errorJSON("\"model\" is required", "invalid_request_error"))
	}

	// "user" and "stream" don't affect the response, so a streaming and
	// non-streaming request for the same prompt hit the same cache entry.
	cacheKeyPayload := map[string]any{}
	for k, v := range parsed {
		if k == "user" || k == "stream" {
			continue
		}
		cacheKeyPayload[k] = v
	}
	cacheKey := cache.Key(model, cacheKeyPayload)
	// A request that runs a provider-side tool (web search, code
	// execution...) isn't a pure function of its body: serving a cached
	// answer would hand back old search results as current. An empty key
	// means "don't read or write the cache" for this request.
	toolsRequested := serverToolsRequested(model, parsed)
	if toolsRequested {
		cacheKey = ""
	}

	if stream, _ := parsed["stream"].(bool); stream {
		return h.chatCompletionsStream(c, userID, model, rawBody, parsed, cacheKey, toolsRequested, start)
	}

	// 1. Cache lookup, before budget: a hit costs nothing, so there's
	// nothing to gate.
	if cacheKey != "" && h.Settings.CacheEnabled() && h.Cache != nil {
		if cached, ok := h.Cache.Get(c.Context(), cacheKey); ok {
			h.logRequest(userID, model, 0, 0, 0, time.Since(start), true, "", fiber.StatusOK)
			c.Set("X-Cache", "HIT")
			c.Set("Content-Type", "application/json")
			return c.Status(fiber.StatusOK).Send(cached)
		}
	}

	// 2. Reserve the worst-case cost before calling upstream, so
	// concurrent requests can't jointly overspend a budget that only
	// checks settled spend. actualCost is filled in once the real cost
	// is known; it stays 0 if every attempt fails.
	var actualCost float64
	if h.Budget != nil {
		estimatedCost := cost.EstimateWorstCaseCost(model, parsed)
		result, release, err := h.Budget.Reserve(c.Context(), userID, estimatedCost)
		if err != nil {
			warnf("budget check failed for %s: %v", userID, err)
		} else if !result.Allowed {
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"error": fiber.Map{
					"message": budgetDenialMessage(result),
					"type":    "budget_exceeded",
				},
			})
		} else {
			defer func() { release(actualCost) }()
		}
	}

	// 3. Try the primary model, then each configured fallback in order.
	attempts := append([]string{model}, h.Settings.Fallback()...)
	var (
		respBody     []byte
		usage        Usage
		finishReason string
		statusCode   int
		usedModel    string
		lastErr      error
		lastFailure  string
	)
	for _, attemptModel := range attempts {
		providerName := RouteProvider(attemptModel)
		provider, ok := h.Providers[providerName]
		if !ok {
			continue
		}
		b, u, fr, sc, err := provider.ChatCompletion(c.Context(), attemptModel, rawBody)
		if err != nil || sc >= 500 || sc == 429 {
			lastErr = err
			lastFailure = describeFailure(providerName, sc)
			logUpstreamFailure(providerName, attemptModel, "request", sc, err, b)
			continue
		}
		respBody, usage, finishReason, statusCode, usedModel = b, u, fr, sc, attemptModel
		lastErr = nil
		break
	}

	if lastErr != nil || usedModel == "" {
		h.logRequest(userID, model, 0, 0, 0, time.Since(start), false, "", fiber.StatusBadGateway)
		return c.Status(fiber.StatusBadGateway).JSON(errorJSON(allFailedMessage(lastFailure), "upstream_error"))
	}

	// 4. finish_reason: "length" means the response was truncated.
	if finishReason == "length" {
		warnf("model %s hit finish_reason=length for user %s (possible truncation/runaway loop)", usedModel, userID)
		c.Set("X-AI-Guard-Warning", "finish_reason=length: response was truncated, check max_tokens")
	}

	bd := cost.Compute(usedModel, usage.tokenUsage())
	requestCost := bd.Total()
	actualCost = requestCost

	toolUses := serverToolUses(RouteProvider(usedModel), toolsRequested, usage.ToolUses)
	if toolUses > 0 {
		warnHeader(c, "server-side tool fees are not included in the logged cost")
	}

	// 5. Log + cache the successful response.
	h.logRecord(chatRecord(userID, usedModel, usage, bd, toolUses, time.Since(start), finishReason, statusCode))

	if cacheKey != "" && h.Settings.CacheEnabled() && h.Cache != nil {
		ttl := time.Duration(h.Settings.CacheTTLSeconds()) * time.Second
		setCtx, cancel := context.WithTimeout(context.Background(), backgroundOpTimeout)
		err := h.Cache.Set(setCtx, cacheKey, respBody, ttl)
		cancel()
		if err != nil {
			warnf("failed to write cache entry: %v", err)
		}
	}

	c.Set("X-Cache", "MISS")
	c.Set("X-AI-Guard-Model-Used", usedModel)
	c.Set("Content-Type", "application/json")
	return c.Status(fiber.StatusOK).Send(respBody)
}

func setSSEHeaders(c *fiber.Ctx) {
	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
}

// chatCompletionsStream is the stream:true path. It mirrors
// ChatCompletions' cache/budget/fallback logic, with three differences
// streaming requires:
//
//  1. A cache hit is synthesized into a stream (via streamFromCached)
//     rather than returned verbatim, since some clients always request one.
//  2. Fallback has to happen before any byte reaches the client — once a
//     stream starts, there's no way to hand it to a different provider.
//     Provider.OpenStream commits to nothing on failure, so every
//     fallback attempt is tried before the client-visible stream starts.
//  3. Because of (2), total failure still returns a normal 502, matching
//     the non-streaming path, instead of a stream that dies partway through.
func (h *Handler) chatCompletionsStream(c *fiber.Ctx, userID, model string, rawBody []byte, parsed map[string]any, cacheKey string, toolsRequested bool, start time.Time) error {
	if cacheKey != "" && h.Settings.CacheEnabled() && h.Cache != nil {
		if cached, ok := h.Cache.Get(c.Context(), cacheKey); ok {
			h.logRequest(userID, model, 0, 0, 0, time.Since(start), true, "", fiber.StatusOK)
			var buf bytes.Buffer
			if err := streamFromCached(bufio.NewWriter(&buf), cached); err != nil {
				warnf("failed to synthesize cached stream for %s: %v", userID, err)
			} else {
				setSSEHeaders(c)
				c.Set("X-Cache", "HIT")
				return c.Status(fiber.StatusOK).Send(buf.Bytes())
			}
		}
	}

	// Same budget semantics as the non-streaming path; release is called
	// explicitly inside the stream writer once the real cost is known.
	release := budget.Release(func(float64) {})
	if h.Budget != nil {
		estimatedCost := cost.EstimateWorstCaseCost(model, parsed)
		result, rel, err := h.Budget.Reserve(c.Context(), userID, estimatedCost)
		if err != nil {
			warnf("budget check failed for %s: %v", userID, err)
		} else if !result.Allowed {
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"error": fiber.Map{
					"message": budgetDenialMessage(result),
					"type":    "budget_exceeded",
				},
			})
		} else {
			release = rel
		}
	}

	attempts := append([]string{model}, h.Settings.Fallback()...)
	var session StreamSession
	var usedModel string
	var lastErr error
	var lastFailure string
	for _, attemptModel := range attempts {
		providerName := RouteProvider(attemptModel)
		provider, ok := h.Providers[providerName]
		if !ok {
			continue
		}
		s, sc, err := provider.OpenStream(c.Context(), attemptModel, rawBody)
		if err != nil || sc >= 500 || sc == 429 {
			lastErr = err
			lastFailure = describeFailure(providerName, sc)
			// An open-stream error already carries the upstream body in
			// its text, so it goes through the same masking.
			logUpstreamFailure(providerName, attemptModel, "stream", sc, err, nil)
			continue
		}
		session, usedModel, lastErr = s, attemptModel, nil
		break
	}

	if lastErr != nil || session == nil {
		release(0)
		h.logRequest(userID, model, 0, 0, 0, time.Since(start), false, "", fiber.StatusBadGateway)
		return c.Status(fiber.StatusBadGateway).JSON(errorJSON(allFailedMessage(lastFailure), "upstream_error"))
	}

	setSSEHeaders(c)
	c.Set("X-Cache", "MISS")
	c.Set("X-AI-Guard-Model-Used", usedModel)
	c.Status(fiber.StatusOK)

	c.Context().SetBodyStreamWriter(fasthttp.StreamWriter(func(w *bufio.Writer) {
		defer session.Close()

		text, toolCalls, usage, finishReason, err := session.Relay(w)
		if err != nil {
			warnf("streaming relay for %s (model %s) ended early: %v", userID, usedModel, err)
		}

		// Headers are already sent, so a truncated stream can only be
		// flagged server-side, unlike the non-streaming X-AI-Guard-Warning.
		if finishReason == "length" {
			warnf("model %s hit finish_reason=length for user %s (possible truncation/runaway loop)", usedModel, userID)
		}

		bd := cost.Compute(usedModel, usage.tokenUsage())
		requestCost := bd.Total()
		release(requestCost)
		toolUses := serverToolUses(RouteProvider(usedModel), toolsRequested, usage.ToolUses)
		h.logRecord(chatRecord(userID, usedModel, usage, bd, toolUses, time.Since(start), finishReason, fiber.StatusOK))

		if cacheKey != "" && h.Settings.CacheEnabled() && h.Cache != nil {
			reconstructed := nonStreamOpenAIResponse("chatcmpl-"+randomSuffix(), usedModel, text, finishReason, usage, toolCalls)
			ttl := time.Duration(h.Settings.CacheTTLSeconds()) * time.Second
			setCtx, cancel := context.WithTimeout(context.Background(), backgroundOpTimeout)
			cacheErr := h.Cache.Set(setCtx, cacheKey, reconstructed, ttl)
			cancel()
			if cacheErr != nil {
				warnf("failed to write cache entry: %v", cacheErr)
			}
		}
	}))
	return nil
}

func (h *Handler) logRequest(userID, model string, promptTokens, completionTokens int, costUSD float64, latency time.Duration, cacheHit bool, finishReason string, statusCode int) {
	h.logRecord(logging.Record{
		UserID:           userID,
		Model:            model,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		CostUSD:          costUSD,
		LatencyMS:        latency.Milliseconds(),
		CacheHit:         cacheHit,
		FinishReason:     finishReason,
		StatusCode:       statusCode,
	})
}

// logRecord persists one request. Everything that logs goes through here.
func (h *Handler) logRecord(rec logging.Record) {
	if h.Store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), backgroundOpTimeout)
	defer cancel()
	if _, err := h.Store.Insert(ctx, rec); err != nil {
		warnf("failed to log request: %v", err)
	}
}

// chatRecord builds the log row for a completed chat request, itemising
// the cost so the dashboard can show what each part of it was charged for.
func chatRecord(userID, model string, u Usage, bd cost.Breakdown, toolUses int, latency time.Duration, finishReason string, statusCode int) logging.Record {
	return logging.Record{
		UserID:            userID,
		Model:             model,
		PromptTokens:      u.PromptTokens,
		CompletionTokens:  u.CompletionTokens,
		CachedTokens:      u.CachedTokens,
		CacheWriteTokens:  u.CacheWriteTokens,
		CostUSD:           bd.Total(),
		InputCostUSD:      bd.InputUSD,
		CachedCostUSD:     bd.CachedUSD,
		CacheWriteCostUSD: bd.CacheWriteUSD,
		OutputCostUSD:     bd.OutputUSD,
		ToolUses:          toolUses,
		Kind:              "chat",
		LatencyMS:         latency.Milliseconds(),
		FinishReason:      finishReason,
		StatusCode:        statusCode,
	}
}

// unitRecord builds the log row for a request billed per unit rather than
// per token: images generated, characters spoken, seconds transcribed.
func unitRecord(userID, model, kind string, units, costUSD float64, latency time.Duration, statusCode int) logging.Record {
	return logging.Record{
		UserID:     userID,
		Model:      model,
		CostUSD:    costUSD,
		Kind:       kind,
		Units:      units,
		LatencyMS:  latency.Milliseconds(),
		StatusCode: statusCode,
	}
}

// warnHeader sets X-AI-Guard-Warning, joining with any warning already set
// (finish_reason=length uses the same header).
func warnHeader(c *fiber.Ctx, msg string) {
	if prev := string(c.Response().Header.Peek("X-AI-Guard-Warning")); prev != "" {
		msg = prev + "; " + msg
	}
	c.Set("X-AI-Guard-Warning", msg)
}
