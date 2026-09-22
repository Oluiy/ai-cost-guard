package proxy

import (
	"encoding/json"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/Oluiy/ai-cost-guard/internal/cost"
)

// ImageGenerations handles POST /v1/images/generations, sharing
// ChatCompletions'/Embeddings' auth/budget shape — but not their cache:
// image generation is not deterministic (no fixed seed in the OpenAI
// request shape), so caching a prompt would silently return the same
// stale image on every repeat instead of a fresh one. No fallback either,
// same reasoning as Embeddings: a silent model swap changes the actual
// output, not just how it's produced.
func (h *Handler) ImageGenerations(c *fiber.Ctx) error {
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

	var actualCost float64
	if h.Budget != nil {
		estimatedCost := cost.EstimateImageCost(model, parsed)
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

	respBody, n, statusCode, err := provider.ImageGeneration(c.Context(), model, rawBody)
	if err != nil {
		warnf("provider %s (model %s) image generation failed: %v (status %d)", providerName, model, err, statusCode)
		h.logRequest(userID, model, 0, 0, 0, time.Since(start), false, "", fiber.StatusBadGateway)
		return c.Status(fiber.StatusBadGateway).JSON(errorJSON(err.Error(), "upstream_error"))
	}

	requestCost := cost.CalculateImageCost(model, n)
	actualCost = requestCost

	h.logRequest(userID, model, 0, 0, requestCost, time.Since(start), false, "", statusCode)

	c.Set("Content-Type", "application/json")
	return c.Status(fiber.StatusOK).Send(respBody)
}
