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
	"github.com/pterm/pterm"
	"github.com/valyala/fasthttp"

	"github.com/Oluiy/ai-cost-guard/internal/budget"
	"github.com/Oluiy/ai-cost-guard/internal/cache"
	"github.com/Oluiy/ai-cost-guard/internal/config"
	"github.com/Oluiy/ai-cost-guard/internal/cost"
	"github.com/Oluiy/ai-cost-guard/internal/logging"
)

// backgroundOpTimeout bounds work that outlives the triggering request in
// spirit — cache writes and cost-log inserts are record-keeping that
// should complete even if the original caller disconnects, so they don't
// inherit the request's context (which cancels on disconnect). They still
// need *some* bound so a stuck disk or Redis can't hang forever; this is it.
const backgroundOpTimeout = 5 * time.Second

// Handler wires together auth, caching, budgets, routing/fallback, provider
// translation and cost logging for the chat completions endpoint.
type Handler struct {
	Cfg       *config.Config
	Cache     cache.Cache
	Budget    budget.Enforcer
	Store     *logging.Store
	Providers map[string]Provider
}

// New builds a Handler, constructing one Provider per configured entry in
// cfg.Providers.
func New(cfg *config.Config, c cache.Cache, enforcer budget.Enforcer, store *logging.Store) *Handler {
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
	return &Handler{Cfg: cfg, Cache: c, Budget: enforcer, Store: store, Providers: providers}
}

func errorJSON(message, typ string) fiber.Map {
	return fiber.Map{"error": fiber.Map{"message": message, "type": typ}}
}

// authenticate resolves the caller's user_id from their Authorization
// header against configured virtual keys. Budgets are only meaningful if
// they're tied to an identity the caller can't choose for themselves — see
// internal/budget's package doc for why a client-supplied header isn't
// good enough. With no keys configured, ai-guard is single-tenant: every
// caller is "default" and there's no gate (local/solo use only).
func (h *Handler) authenticate(c *fiber.Ctx) (userID string, ok bool) {
	if len(h.Cfg.Keys) == 0 {
		return "default", true
	}
	auth := c.Get("Authorization")
	token, hasBearer := strings.CutPrefix(auth, "Bearer ")
	if !hasBearer || token == "" {
		return "", false
	}
	// Constant-time, and deliberately checks every configured key rather
	// than returning on the first match: standard practice for comparing
	// a caller-supplied secret against a set of valid ones, so the
	// comparison itself can't become a side channel regardless of how
	// many keys are configured or where a near-match sits in the map.
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
			"missing or invalid API key; pass Authorization: Bearer <ai-guard key> (see `ai-guard init`)", "invalid_api_key"))
	}

	var parsed map[string]any
	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errorJSON("invalid JSON body", "invalid_request_error"))
	}

	model, _ := parsed["model"].(string)
	if model == "" {
		return c.Status(fiber.StatusBadRequest).JSON(errorJSON("\"model\" is required", "invalid_request_error"))
	}

	// Cache key intentionally excludes "user" and "stream": neither
	// affects the answer, and a streaming vs. non-streaming request for
	// the same prompt should be able to hit the same cache entry (see
	// chatCompletionsStream, which re-serves a non-streaming cache hit as
	// a synthesized stream).
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

	// 1. Cache lookup. Cache hits cost nothing, so they're served before
	// (and regardless of) budget — no reason to gate a free response.
	if h.Cfg.Cache.Enabled && h.Cache != nil {
		if cached, ok := h.Cache.Get(c.Context(), cacheKey); ok {
			h.logRequest(userID, model, 0, 0, 0, time.Since(start), true, "", fiber.StatusOK)
			c.Set("X-Cache", "HIT")
			c.Set("Content-Type", "application/json")
			return c.Status(fiber.StatusOK).Send(cached)
		}
	}

	// 2. Budget: reserve the request's worst-case cost *before* calling
	// upstream, so a concurrent burst or a single huge max_tokens can't
	// slip past a check that only looked at already-settled spend.
	// actualCost is set below once the real cost is known (it stays 0 if
	// every provider/fallback attempt fails, since nothing was spent);
	// the deferred release reconciles the reservation against whatever
	// it ends up being.
	var actualCost float64
	if h.Budget != nil {
		estimatedCost := cost.EstimateWorstCaseCost(model, parsed)
		result, release, err := h.Budget.Reserve(c.Context(), userID, estimatedCost)
		if err != nil {
			pterm.Warning.Printfln("budget check failed for %s: %v", userID, err)
		} else if !result.Allowed {
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"error": fiber.Map{
					"message": pterm.Sprintf("daily budget of $%.2f exceeded or would be exceeded by this request (spent $%.2f)", result.LimitUSD, result.SpentUSD),
					"type":    "budget_exceeded",
				},
			})
		} else {
			defer func() { release(actualCost) }()
		}
	}

	// 3. Attempt primary model, then configured fallbacks on failure.
	attempts := append([]string{model}, h.Cfg.Fallback...)
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
			pterm.Warning.Printfln("provider %s (model %s) failed: %v (status %d), trying fallback", providerName, attemptModel, err, sc)
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

	// 4. Finish-reason guard: warn loudly on truncated / runaway generations.
	if finishReason == "length" {
		pterm.Warning.Printfln("model %s hit finish_reason=length for user %s (possible truncation/runaway loop)", usedModel, userID)
		c.Set("X-AI-Guard-Warning", "finish_reason=length: response was truncated, check max_tokens")
	}

	requestCost := cost.Calculate(usedModel, usage.PromptTokens, usage.CompletionTokens)
	actualCost = requestCost

	// 5. Log + cache the successful response.
	h.logRequest(userID, usedModel, usage.PromptTokens, usage.CompletionTokens, requestCost, time.Since(start), false, finishReason, statusCode)

	if h.Cfg.Cache.Enabled && h.Cache != nil {
		ttl := time.Duration(h.Cfg.Cache.TTL) * time.Second
		setCtx, cancel := context.WithTimeout(context.Background(), backgroundOpTimeout)
		err := h.Cache.Set(setCtx, cacheKey, respBody, ttl)
		cancel()
		if err != nil {
			pterm.Warning.Printfln("failed to write cache entry: %v", err)
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

// chatCompletionsStream handles stream:true requests. It mirrors
// ChatCompletions' cache/budget/fallback logic, but has to work
// differently in three ways streaming forces on it:
//
//  1. A cache hit still needs to come back as a stream (some clients
//     always request one), so it's synthesized from the cached
//     non-streaming JSON rather than sent back verbatim.
//  2. Provider fallback has to happen *before* anything is written to the
//     client: once a byte of the stream is sent, there's no way to hand a
//     partially-streamed response to a different provider. So this
//     establishes a working StreamSession first (via Provider.OpenStream,
//     which — like ChatCompletion — commits to nothing on failure) and
//     only then commits to the client-visible stream.
//  3. If every provider/fallback attempt fails, the HTTP status is
//     already committed to 200 by the time that's discovered *if* it
//     happened after committing the stream — which is exactly what step 2
//     avoids: total failure here still returns a normal 502, matching the
//     non-streaming path, because it's detected before SetBodyStreamWriter
//     is ever called.
func (h *Handler) chatCompletionsStream(c *fiber.Ctx, userID, model string, rawBody []byte, parsed map[string]any, cacheKey string, start time.Time) error {
	// Cache hit: free, so served before (and regardless of) budget, same
	// reasoning as the non-streaming path.
	if h.Cfg.Cache.Enabled && h.Cache != nil {
		if cached, ok := h.Cache.Get(c.Context(), cacheKey); ok {
			h.logRequest(userID, model, 0, 0, 0, time.Since(start), true, "", fiber.StatusOK)
			var buf bytes.Buffer
			if err := streamFromCached(bufio.NewWriter(&buf), cached); err != nil {
				pterm.Warning.Printfln("failed to synthesize cached stream for %s: %v", userID, err)
			} else {
				setSSEHeaders(c)
				c.Set("X-Cache", "HIT")
				return c.Status(fiber.StatusOK).Send(buf.Bytes())
			}
		}
	}

	// Budget: identical semantics to the non-streaming path (reserve the
	// worst-case estimate before calling upstream; the reservation is
	// reconciled against the real cost once the stream finishes, inside
	// the stream writer below — there's no defer here since release is
	// called explicitly at the one point that matters).
	release := budget.Release(func(float64) {})
	if h.Budget != nil {
		estimatedCost := cost.EstimateWorstCaseCost(model, parsed)
		result, rel, err := h.Budget.Reserve(c.Context(), userID, estimatedCost)
		if err != nil {
			pterm.Warning.Printfln("budget check failed for %s: %v", userID, err)
		} else if !result.Allowed {
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"error": fiber.Map{
					"message": pterm.Sprintf("daily budget of $%.2f exceeded or would be exceeded by this request (spent $%.2f)", result.LimitUSD, result.SpentUSD),
					"type":    "budget_exceeded",
				},
			})
		} else {
			release = rel
		}
	}

	// Establish a working session before committing to a client-visible
	// stream, trying fallbacks exactly like the non-streaming path.
	attempts := append([]string{model}, h.Cfg.Fallback...)
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
			pterm.Warning.Printfln("provider %s (model %s) failed to open stream: %v (status %d), trying fallback", providerName, attemptModel, err, sc)
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
			pterm.Warning.Printfln("streaming relay for %s (model %s) ended early: %v", userID, usedModel, err)
		}

		// Finish-reason guard: streaming can't add a response header at
		// this point (headers are long since sent), so a truncated
		// streamed answer is only ever flagged server-side, not to the
		// client — unlike the non-streaming path's X-AI-Guard-Warning.
		if finishReason == "length" {
			pterm.Warning.Printfln("model %s hit finish_reason=length for user %s (possible truncation/runaway loop)", usedModel, userID)
		}

		requestCost := cost.Calculate(usedModel, usage.PromptTokens, usage.CompletionTokens)
		release(requestCost)
		h.logRequest(userID, usedModel, usage.PromptTokens, usage.CompletionTokens, requestCost, time.Since(start), false, finishReason, fiber.StatusOK)

		if h.Cfg.Cache.Enabled && h.Cache != nil {
			reconstructed := nonStreamOpenAIResponse("chatcmpl-"+randomSuffix(), usedModel, text, finishReason, usage, toolCalls)
			ttl := time.Duration(h.Cfg.Cache.TTL) * time.Second
			setCtx, cancel := context.WithTimeout(context.Background(), backgroundOpTimeout)
			cacheErr := h.Cache.Set(setCtx, cacheKey, reconstructed, ttl)
			cancel()
			if cacheErr != nil {
				pterm.Warning.Printfln("failed to write cache entry: %v", cacheErr)
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
		pterm.Warning.Printfln("failed to log request: %v", err)
	}
}
