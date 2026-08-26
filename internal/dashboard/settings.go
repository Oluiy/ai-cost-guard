package dashboard

import (
	"github.com/gofiber/fiber/v2"

	"github.com/Oluiy/ai-cost-guard/internal/config"
)

// Settings serves the subset of configuration the dashboard may change,
// as config.Editable rather than *config.Config, so provider keys and the
// session secret can never end up in the response.
func (h *Handler) Settings(c *fiber.Ctx) error {
	if h.Live == nil {
		return c.Status(fiber.StatusNotImplemented).JSON(fiber.Map{
			"error": fiber.Map{
				"message": "live settings are unavailable in this build",
				"type":    "unsupported",
			},
		})
	}
	return c.JSON(h.Live.Snapshot())
}

// UpdateSettings validates and applies a settings change. Not a PATCH:
// the client sends the complete editable set and gets the applied result
// back, avoiding ambiguity between "omitted" and "set to zero/false".
func (h *Handler) UpdateSettings(c *fiber.Ctx) error {
	if h.Live == nil {
		return c.Status(fiber.StatusNotImplemented).JSON(fiber.Map{
			"error": fiber.Map{
				"message": "live settings are unavailable in this build",
				"type":    "unsupported",
			},
		})
	}

	var req config.Editable
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": fiber.Map{"message": "invalid request body", "type": "invalid_request_error"},
		})
	}

	// Apply validates before touching live state, so a rejected update
	// leaves both the running config and the file exactly as they were.
	issuedKeys, err := h.Live.Apply(req)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": fiber.Map{"message": err.Error(), "type": "invalid_request_error"},
		})
	}

	resp := h.Live.Snapshot()
	if len(issuedKeys) > 0 {
		return c.JSON(fiber.Map{
			"cache_enabled":     resp.CacheEnabled,
			"cache_ttl_seconds": resp.CacheTTLSeconds,
			"fallback":          resp.Fallback,
			"users":             resp.Users,
			// Shown once: keys aren't retrievable again after this response.
			"issued_keys": issuedKeys,
		})
	}
	return c.JSON(resp)
}
