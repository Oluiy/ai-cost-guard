package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/valyala/fasthttp"

	"github.com/Oluiy/ai-cost-guard/internal/budget"
	"github.com/Oluiy/ai-cost-guard/internal/cache"
	"github.com/Oluiy/ai-cost-guard/internal/cost"
)

// Responses handles POST /v1/responses (create). Pass-through for OpenAI
// and Groq, the providers that serve the API natively; there is no
// `fallback:` because a response carries server-side state (ids,
// conversations) that a different provider can't continue.
//
// Not supported, each refused up front rather than priced wrongly:
// background:true (the call returns before any usage exists), and the
// retrieve/cancel/delete/input_items endpoints (they're keyed by response
// id, not model, so there's no provider to route to or cost to meter).
func (h *Handler) Responses(c *fiber.Ctx) error {
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
	if bg, _ := parsed["background"].(bool); bg {
		return c.Status(fiber.StatusBadRequest).JSON(errorJSON(
			"background responses are not supported through FitGuard: the call returns before any token usage exists, so it could not be priced or budgeted. Send the request without background:true", "invalid_request_error"))
	}

	providerName := RouteProvider(model)
	provider, ok := h.Providers[providerName]
	if !ok {
		h.logRequest(userID, model, 0, 0, 0, time.Since(start), false, "", fiber.StatusBadGateway)
		return c.Status(fiber.StatusBadGateway).JSON(errorJSON(
			"no provider configured for model \""+model+"\"", "upstream_error"))
	}
	rp, ok := provider.(ResponsesProvider)
	if !ok || !providerServesResponses(provider) {
		return c.Status(fiber.StatusBadRequest).JSON(errorJSON(
			"the Responses API is only available for OpenAI and Groq models; use /v1/chat/completions for "+providerName, "invalid_request_error"))
	}

	// Stateful or tool-running requests aren't a pure function of the body.
	toolsRequested := serverToolsRequested(model, parsed)
	cacheKey := ""
	if !toolsRequested && parsed["previous_response_id"] == nil && parsed["conversation"] == nil {
		payload := map[string]any{}
		for k, v := range parsed {
			if k == "user" || k == "stream" {
				continue
			}
			payload[k] = v
		}
		cacheKey = cache.Key("responses:"+model, payload)
	}
	stream, _ := parsed["stream"].(bool)

	if cacheKey != "" && !stream && h.Settings.CacheEnabled() && h.Cache != nil {
		if cached, ok := h.Cache.Get(c.Context(), cacheKey); ok {
			h.logRequest(userID, model, 0, 0, 0, time.Since(start), true, "", fiber.StatusOK)
			c.Set("X-Cache", "HIT")
			c.Set("Content-Type", "application/json")
			return c.Status(fiber.StatusOK).Send(cached)
		}
	}

	release := budget.Release(func(float64) {})
	if h.Budget != nil {
		result, rel, err := h.Budget.Reserve(c.Context(), userID, cost.EstimateResponsesCost(model, parsed))
		if err != nil {
			warnf("budget check failed for %s: %v", userID, err)
		} else if !result.Allowed {
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"error": fiber.Map{"message": budgetDenialMessage(result), "type": "budget_exceeded"},
			})
		} else {
			release = rel
		}
	}

	if stream {
		return h.responsesStream(c, rp, providerName, userID, model, rawBody, toolsRequested, release, start)
	}

	respBody, usage, finish, status, err := rp.Responses(c.Context(), model, rawBody)
	if err != nil || status >= 500 || status == 429 {
		release(0)
		logUpstreamFailure(providerName, model, "responses", status, err, respBody)
		h.logRequest(userID, model, 0, 0, 0, time.Since(start), false, "", fiber.StatusBadGateway)
		return c.Status(fiber.StatusBadGateway).JSON(errorJSON(clientFailureMessage(providerName, status, err), "upstream_error"))
	}

	bd := cost.Compute(model, usage.tokenUsage())
	release(bd.Total())
	toolUses := serverToolUses(providerName, toolsRequested, usage.ToolUses)
	if toolUses > 0 {
		warnHeader(c, "server-side tool fees are not included in the logged cost")
	}
	if finish == "length" {
		warnf("model %s hit max_output_tokens for user %s (possible truncation/runaway loop)", model, userID)
		warnHeader(c, "max_output_tokens reached: response was truncated")
	}
	rec := chatRecord(userID, model, usage, bd, toolUses, time.Since(start), finish, status)
	rec.Kind = "response"
	h.logRecord(rec)

	if cacheKey != "" && finish == "completed" && h.Settings.CacheEnabled() && h.Cache != nil {
		ttl := time.Duration(h.Settings.CacheTTLSeconds()) * time.Second
		setCtx, cancel := context.WithTimeout(context.Background(), backgroundOpTimeout)
		if err := h.Cache.Set(setCtx, cacheKey, respBody, ttl); err != nil {
			warnf("failed to write cache entry: %v", err)
		}
		cancel()
	}

	c.Set("X-Cache", "MISS")
	c.Set("Content-Type", "application/json")
	return c.Status(status).Send(respBody)
}

func (h *Handler) responsesStream(c *fiber.Ctx, rp ResponsesProvider, providerName, userID, model string, rawBody []byte, toolsRequested bool, release budget.Release, start time.Time) error {
	session, status, err := rp.OpenResponsesStream(c.Context(), model, rawBody)
	if err != nil {
		release(0)
		logUpstreamFailure(providerName, model, "responses-stream", status, err, nil)
		h.logRequest(userID, model, 0, 0, 0, time.Since(start), false, "", fiber.StatusBadGateway)
		return c.Status(fiber.StatusBadGateway).JSON(errorJSON(clientFailureMessage(providerName, status, err), "upstream_error"))
	}

	setSSEHeaders(c)
	c.Set("X-Cache", "MISS")
	c.Status(fiber.StatusOK)
	c.Context().SetBodyStreamWriter(fasthttp.StreamWriter(func(w *bufio.Writer) {
		usage, finish, err := session.Relay(w)
		if err != nil {
			warnf("responses stream for %s (model %s) ended early: %v", userID, model, err)
		}
		bd := cost.Compute(model, usage.tokenUsage())
		release(bd.Total())
		rec := chatRecord(userID, model, usage, bd, serverToolUses(providerName, toolsRequested, usage.ToolUses), time.Since(start), finish, fiber.StatusOK)
		rec.Kind = "response"
		h.logRecord(rec)
	}))
	return nil
}

// providerServesResponses filters out OpenAI-compatible providers (Together)
// that don't expose /responses even though they share the adapter.
func providerServesResponses(p Provider) bool {
	oc, ok := p.(*OpenAICompatProvider)
	return !ok || oc.supportsResponses()
}
