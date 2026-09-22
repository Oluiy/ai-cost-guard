package proxy

import (
	"encoding/json"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/Oluiy/ai-cost-guard/internal/cost"
)

// AudioSpeech handles POST /v1/audio/speech (text-to-speech), sharing
// ChatCompletions'/Embeddings' auth/budget shape. Cache and fallback are
// both skipped, same reasoning as ImageGenerations: TTS output isn't
// guaranteed byte-identical across calls even for the same input, and a
// silent model swap would change the actual voice produced.
func (h *Handler) AudioSpeech(c *fiber.Ctx) error {
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
		estimatedCost := cost.EstimateAudioSpeechCost(model, parsed)
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

	respBody, charCount, statusCode, err := provider.AudioSpeech(c.Context(), model, rawBody)
	if err != nil {
		warnf("provider %s (model %s) audio speech failed: %v (status %d)", providerName, model, err, statusCode)
		h.logRequest(userID, model, 0, 0, 0, time.Since(start), false, "", fiber.StatusBadGateway)
		return c.Status(fiber.StatusBadGateway).JSON(errorJSON(err.Error(), "upstream_error"))
	}

	requestCost := cost.CalculateAudioSpeechCost(model, charCount)
	actualCost = requestCost

	h.logRequest(userID, model, 0, 0, requestCost, time.Since(start), false, "", statusCode)

	c.Set("Content-Type", "audio/mpeg")
	return c.Status(fiber.StatusOK).Send(respBody)
}

// AudioTranscriptions handles POST /v1/audio/transcriptions
// (speech-to-text). multipart/form-data, not JSON — the one endpoint
// shape that breaks from every other handler in this package. Cost is a
// ceiling (EstimateAudioTranscriptionCost, from the uploaded file's byte
// size) reconciled to the provider's actual reported duration afterward,
// same "estimate then reconcile" pattern ChatCompletions uses for
// max_tokens. No cache (each upload is effectively unique) and no
// fallback (same reasoning as Embeddings/ImageGenerations).
func (h *Handler) AudioTranscriptions(c *fiber.Ctx) error {
	start := time.Now()

	userID, authOK := h.authenticate(c)
	if !authOK {
		return c.Status(fiber.StatusUnauthorized).JSON(errorJSON(
			"missing or invalid API key; pass Authorization: Bearer <fitguard key> (see `fitguard init`)", "invalid_api_key"))
	}

	fileHeader, err := c.FormFile("file")
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errorJSON("\"file\" is required (multipart/form-data)", "invalid_request_error"))
	}
	model := c.FormValue("model")
	if model == "" {
		return c.Status(fiber.StatusBadRequest).JSON(errorJSON("\"model\" is required", "invalid_request_error"))
	}

	formFields := map[string]string{}
	if form, err := c.MultipartForm(); err == nil {
		for k, vals := range form.Value {
			if len(vals) > 0 {
				formFields[k] = vals[0]
			}
		}
	}

	var actualCost float64
	if h.Budget != nil {
		estimatedCost := cost.EstimateAudioTranscriptionCost(model, fileHeader.Size)
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

	f, err := fileHeader.Open()
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errorJSON("could not read uploaded file", "invalid_request_error"))
	}
	defer f.Close()

	respBody, durationSeconds, statusCode, err := provider.AudioTranscription(c.Context(), model, f, fileHeader.Filename, formFields)
	if err != nil {
		warnf("provider %s (model %s) transcription failed: %v (status %d)", providerName, model, err, statusCode)
		h.logRequest(userID, model, 0, 0, 0, time.Since(start), false, "", fiber.StatusBadGateway)
		return c.Status(fiber.StatusBadGateway).JSON(errorJSON(err.Error(), "upstream_error"))
	}

	requestCost := cost.CalculateAudioTranscriptionCost(model, durationSeconds)
	actualCost = requestCost

	h.logRequest(userID, model, 0, 0, requestCost, time.Since(start), false, "", statusCode)

	c.Set("Content-Type", "application/json")
	return c.Status(fiber.StatusOK).Send(respBody)
}
