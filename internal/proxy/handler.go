package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
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
			providers[name] = NewOpenAICompatProvider(baseURL, p.APIKey)
		case "together":
			if baseURL == "" {
				baseURL = "https://api.together.xyz/v1"
			}
			providers[name] = NewOpenAICompatProvider(baseURL, p.APIKey)
		default: // "openai" and any custom OpenAI-compatible provider
			if baseURL == "" {
				baseURL = "https://api.openai.com/v1"
			}
			providers[name] = NewOpenAICompatProvider(baseURL, p.APIKey)
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
	if len(h.Cfg.Keys) == 0 {
		return "default", true
	}
	auth := c.Get("Authorization")
	token, hasBearer := strings.CutPrefix(auth, "Bearer ")
	if !hasBearer || token == "" {
		return "", false
	}
	// Constant-time comparison against every configured key, not just
	// until the first match, so timing can't leak which keys are close.
	tokenBytes := []byte(token)
	for key, uid := range h.Cfg.Keys {
		if len(key) == len(token) && subtle.ConstantTimeCompare([]byte(key), tokenBytes) == 1 {
			userID, ok = uid, true
		}
	}
	return userID, ok
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

	if stream, _ := parsed["stream"].(bool); stream {
		return h.chatCompletionsStream(c, userID, model, rawBody, parsed, cacheKey, start)
	}

	// 1. Cache lookup, before budget: a hit costs nothing, so there's
	// nothing to gate.
	if h.Settings.CacheEnabled() && h.Cache != nil {
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
			warnf("provider %s (model %s) failed: %v (status %d), trying fallback", providerName, attemptModel, err, sc)
			continue
		}
		respBody, usage, finishReason, statusCode, usedModel = b, u, fr, sc, attemptModel
		lastErr = nil
		break
	}

	if lastErr != nil || usedModel == "" {
		h.logRequest(userID, model, 0, 0, 0, time.Since(start), false, "", fiber.StatusBadGateway)
		return c.Status(fiber.StatusBadGateway).JSON(errorJSON("all providers/fallbacks failed", "upstream_error"))
	}

	// 4. finish_reason: "length" means the response was truncated.
	if finishReason == "length" {
		warnf("model %s hit finish_reason=length for user %s (possible truncation/runaway loop)", usedModel, userID)
		c.Set("X-AI-Guard-Warning", "finish_reason=length: response was truncated, check max_tokens")
	}

	requestCost := cost.Calculate(usedModel, usage.PromptTokens, usage.CompletionTokens)
	actualCost = requestCost

	// 5. Log + cache the successful response.
	h.logRequest(userID, usedModel, usage.PromptTokens, usage.CompletionTokens, requestCost, time.Since(start), false, finishReason, statusCode)

	if h.Settings.CacheEnabled() && h.Cache != nil {
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
func (h *Handler) chatCompletionsStream(c *fiber.Ctx, userID, model string, rawBody []byte, parsed map[string]any, cacheKey string, start time.Time) error {
	if h.Settings.CacheEnabled() && h.Cache != nil {
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
	for _, attemptModel := range attempts {
		providerName := RouteProvider(attemptModel)
		provider, ok := h.Providers[providerName]
		if !ok {
			continue
		}
		s, sc, err := provider.OpenStream(c.Context(), attemptModel, rawBody)
		if err != nil || sc >= 500 || sc == 429 {
			lastErr = err
			warnf("provider %s (model %s) failed to open stream: %v (status %d), trying fallback", providerName, attemptModel, err, sc)
			continue
		}
		session, usedModel, lastErr = s, attemptModel, nil
		break
	}

	if lastErr != nil || session == nil {
		release(0)
		h.logRequest(userID, model, 0, 0, 0, time.Since(start), false, "", fiber.StatusBadGateway)
		return c.Status(fiber.StatusBadGateway).JSON(errorJSON("all providers/fallbacks failed", "upstream_error"))
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

		requestCost := cost.Calculate(usedModel, usage.PromptTokens, usage.CompletionTokens)
		release(requestCost)
		h.logRequest(userID, usedModel, usage.PromptTokens, usage.CompletionTokens, requestCost, time.Since(start), false, finishReason, fiber.StatusOK)

		if h.Settings.CacheEnabled() && h.Cache != nil {
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
	if h.Store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), backgroundOpTimeout)
	defer cancel()
	_, err := h.Store.Insert(ctx, logging.Record{
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
	if err != nil {
		warnf("failed to log request: %v", err)
	}
}
