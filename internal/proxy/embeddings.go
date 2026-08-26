package proxy

import (
	"context"
	"encoding/json"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/Oluiy/ai-cost-guard/internal/cache"
	"github.com/Oluiy/ai-cost-guard/internal/cost"
)

// Embeddings handles POST /v1/embeddings, sharing ChatCompletions'
// auth/cache/budget shape but with no fallback: `fallback:` names chat
// models, and a silent model swap mid-indexing-run would corrupt
// downstream data rather than just fail cleanly.
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

	// Deterministic for a given input+model, so caching is a clean win.
	if h.Settings.CacheEnabled() && h.Cache != nil {
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

	providerName := RouteProvider(model)
	provider, ok := h.Providers[providerName]
	if !ok {
		h.logRequest(userID, model, 0, 0, 0, time.Since(start), false, "", fiber.StatusBadGateway)
		return c.Status(fiber.StatusBadGateway).JSON(errorJSON(
			"no provider configured for model \""+model+"\"", "upstream_error"))
	}

	respBody, usage, statusCode, err := provider.Embeddings(c.Context(), model, rawBody)
	if err != nil {
		warnf("provider %s (model %s) embeddings failed: %v (status %d)", providerName, model, err, statusCode)
		h.logRequest(userID, model, 0, 0, 0, time.Since(start), false, "", fiber.StatusBadGateway)
		// Only one attempt here (no fallback), so the error is
		// unambiguous and worth surfacing directly.
		return c.Status(fiber.StatusBadGateway).JSON(errorJSON(err.Error(), "upstream_error"))
	}

	requestCost := cost.Calculate(model, usage.PromptTokens, 0)
	actualCost = requestCost

	h.logRequest(userID, model, usage.PromptTokens, 0, requestCost, time.Since(start), false, "", statusCode)

	if h.Settings.CacheEnabled() && h.Cache != nil {
		ttl := time.Duration(h.Settings.CacheTTLSeconds()) * time.Second
		setCtx, cancel := context.WithTimeout(context.Background(), backgroundOpTimeout)
		cacheErr := h.Cache.Set(setCtx, cacheKey, respBody, ttl)
		cancel()
		if cacheErr != nil {
			warnf("failed to write cache entry: %v", cacheErr)
		}
	}

	c.Set("X-Cache", "MISS")
	c.Set("Content-Type", "application/json")
	return c.Status(fiber.StatusOK).Send(respBody)
}
