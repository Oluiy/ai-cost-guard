package proxy

import (
	"context"
	"encoding/json"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/pterm/pterm"

	"github.com/aicostguard/ai-cost-guard/internal/cache"
	"github.com/aicostguard/ai-cost-guard/internal/cost"
)

// Embeddings handles POST /v1/embeddings. It shares ChatCompletions'
// auth/cache/budget shape, simplified where embeddings genuinely differ:
// there's no completion side to a budget estimate (the prompt size *is*
// the worst case), and — deliberately — no fallback list. Chat's
// `fallback:` config names chat models; an embeddings request falling
// back to one would be nonsensical, and embeddings models are typically
// used for indexing pipelines where a silent model swap mid-index is far
// more likely to corrupt data than a clean error asking you to retry.
func (h *Handler) Embeddings(c *fiber.Ctx) error {
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

	cacheKeyPayload := map[string]any{}
	for k, v := range parsed {
		if k == "user" {
			continue
		}
		cacheKeyPayload[k] = v
	}
	cacheKey := cache.Key(model, cacheKeyPayload)

	// Embeddings are deterministic for a given input+model, so this is an
	// even better caching candidate than chat completions — same free,
	// budget-exempt cache-hit reasoning as ChatCompletions.
	if h.Cfg.Cache.Enabled && h.Cache != nil {
		if cached, ok := h.Cache.Get(c.Context(), cacheKey); ok {
			h.logRequest(userID, model, 0, 0, 0, time.Since(start), true, "", fiber.StatusOK)
			c.Set("X-Cache", "HIT")
			c.Set("Content-Type", "application/json")
			return c.Status(fiber.StatusOK).Send(cached)
		}
	}

	var actualCost float64
	if h.Budget != nil {
		estimatedCost := cost.EstimateEmbeddingCost(model, parsed)
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

	providerName := RouteProvider(model)
	provider, ok := h.Providers[providerName]
	if !ok {
		h.logRequest(userID, model, 0, 0, 0, time.Since(start), false, "", fiber.StatusBadGateway)
		return c.Status(fiber.StatusBadGateway).JSON(errorJSON(
			"no provider configured for model \""+model+"\"", "upstream_error"))
	}

	respBody, usage, statusCode, err := provider.Embeddings(c.Context(), model, rawBody)
	if err != nil {
		pterm.Warning.Printfln("provider %s (model %s) embeddings failed: %v (status %d)", providerName, model, err, statusCode)
		h.logRequest(userID, model, 0, 0, 0, time.Since(start), false, "", fiber.StatusBadGateway)
		// Unlike chat completions' fallback loop, there's exactly one
		// attempt here, so the error is unambiguous — worth surfacing
		// directly rather than a generic message, especially for the
		// common case of hitting a provider (e.g. Anthropic) with no
		// embeddings API at all. Provider error strings never embed raw
		// upstream response bodies (see openai.go/anthropic.go), so this
		// doesn't leak anything sensitive.
		return c.Status(fiber.StatusBadGateway).JSON(errorJSON(err.Error(), "upstream_error"))
	}

	requestCost := cost.Calculate(model, usage.PromptTokens, 0)
	actualCost = requestCost

	h.logRequest(userID, model, usage.PromptTokens, 0, requestCost, time.Since(start), false, "", statusCode)

	if h.Cfg.Cache.Enabled && h.Cache != nil {
		ttl := time.Duration(h.Cfg.Cache.TTL) * time.Second
		setCtx, cancel := context.WithTimeout(context.Background(), backgroundOpTimeout)
		cacheErr := h.Cache.Set(setCtx, cacheKey, respBody, ttl)
		cancel()
		if cacheErr != nil {
			pterm.Warning.Printfln("failed to write cache entry: %v", cacheErr)
		}
	}

	c.Set("X-Cache", "MISS")
	c.Set("Content-Type", "application/json")
	return c.Status(fiber.StatusOK).Send(respBody)
}
