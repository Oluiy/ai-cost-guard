package proxy

import (
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/Oluiy/ai-cost-guard/internal/cost"
)

// modelObject is one entry of the OpenAI /v1/models response. Tools such as
// LibreChat, Open WebUI, Cursor and Continue call this endpoint to fill
// their model picker, and the official SDKs expose it as models.list(), so
// without it they show an empty list or fail outright.
type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// servedModels is every priced model whose provider is configured here.
// Models of a provider with no API key are left out: listing them would
// put choices in a picker that can only fail.
//
// This is the catalogue FitGuard can price and budget, not a limit on what
// it will forward. A model that isn't listed still works if the provider
// serves it (it's costed at a conservative default rate), which is how a
// brand-new model name keeps working before the price table learns it.
func (h *Handler) servedModels() []modelObject {
	providers := make([]string, 0, len(h.Cfg.Providers))
	for name := range h.Cfg.Providers {
		providers = append(providers, name)
	}
	names := cost.ModelsFor(providers)
	out := make([]modelObject, 0, len(names))
	for _, n := range names {
		out = append(out, toModelObject(n))
	}
	return out
}

func toModelObject(id string) modelObject {
	provider, _ := cost.ProviderFor(id)
	// created is required by the OpenAI SDK's types but FitGuard has no
	// release date for a model; 0 is honest, and clients that sort on it
	// fall back to name order.
	return modelObject{ID: id, Object: "model", Created: 0, OwnedBy: provider}
}

// Models handles GET /v1/models.
func (h *Handler) Models(c *fiber.Ctx) error {
	if _, ok := h.authenticate(c); !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(errorJSON(
			"missing or invalid API key; pass Authorization: Bearer <fitguard key> (see `fitguard init`)", "invalid_api_key"))
	}
	return c.JSON(fiber.Map{"object": "list", "data": h.servedModels()})
}

// Model handles GET /v1/models/{model}. The model id is a wildcard because
// ids contain slashes ("meta-llama/Llama-3.3-70B-Instruct-Turbo").
func (h *Handler) Model(c *fiber.Ctx) error {
	if _, ok := h.authenticate(c); !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(errorJSON(
			"missing or invalid API key; pass Authorization: Bearer <fitguard key> (see `fitguard init`)", "invalid_api_key"))
	}
	id := strings.TrimPrefix(c.Params("*"), "/")
	for _, m := range h.servedModels() {
		if m.ID == id {
			return c.JSON(m)
		}
	}
	return c.Status(fiber.StatusNotFound).JSON(errorJSON(
		"The model '"+id+"' is not in FitGuard's catalogue for the providers configured here. "+
			"It may still work if its provider serves it: the catalogue lists priced models, not every model a provider offers.",
		"invalid_request_error"))
}
