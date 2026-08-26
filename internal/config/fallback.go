package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Oluiy/ai-cost-guard/internal/cost"
)

// ValidateFallback rejects any fallback model that isn't served by one of
// the configured providers. A fallback naming an unconfigured provider is
// silently skipped at request time, so the chain degrades to nothing
// exactly when it's needed; failing at config time makes that visible.
func ValidateFallback(fallback []string, providers map[string]Provider) error {
	if len(providers) == 0 {
		return nil
	}
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)

	for i, model := range fallback {
		model = strings.TrimSpace(model)
		if model == "" {
			return fmt.Errorf("fallback[%d] cannot be empty", i)
		}
		provider, known := cost.ProviderFor(model)
		if !known {
			return fmt.Errorf("fallback model %q is not a known model — configured providers are %s; "+
				"use `provider/model` (e.g. %s/my-model) for a custom or self-hosted model",
				model, strings.Join(names, ", "), names[0])
		}
		if _, ok := providers[provider]; !ok {
			return fmt.Errorf("fallback model %q needs the %q provider, which isn't configured — "+
				"add it with `fitguard add-provider`, or pick a model from: %s",
				model, provider, strings.Join(cost.ChatModelsFor(names), ", "))
		}
	}
	return nil
}
