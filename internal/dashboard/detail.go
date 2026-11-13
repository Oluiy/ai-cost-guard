package dashboard

import (
	"github.com/Oluiy/ai-cost-guard/internal/cost"
	"github.com/Oluiy/ai-cost-guard/internal/logging"
)

// costDetail is what the dashboard needs to explain a single cost when its
// row is expanded. It is embedded in both row types that carry a cost (the
// overview's top requests and the full request log), so the two can't
// drift apart; encoding/json flattens it into the parent object.
//
// The component costs are the ones priced when the request happened, not
// recomputed from today's price table, so an old row keeps explaining the
// number it actually billed.
type costDetail struct {
	ID                int64   `json:"id"` // lets the UI keep a row expanded across live refreshes
	Provider          string  `json:"provider"`
	Kind              string  `json:"kind"`
	CachedTokens      int     `json:"cached_tokens"`
	CacheWriteTokens  int     `json:"cache_write_tokens"`
	InputCostUSD      float64 `json:"input_cost_usd"`
	CachedCostUSD     float64 `json:"cached_cost_usd"`
	CacheWriteCostUSD float64 `json:"cache_write_cost_usd"`
	OutputCostUSD     float64 `json:"output_cost_usd"`
	ToolUses          int     `json:"tool_uses"`
	Units             float64 `json:"units"`
}

func detailOf(r logging.Record) costDetail {
	provider, _ := cost.ProviderFor(r.Model)
	return costDetail{
		ID:                r.ID,
		Provider:          provider,
		Kind:              r.Kind,
		CachedTokens:      r.CachedTokens,
		CacheWriteTokens:  r.CacheWriteTokens,
		InputCostUSD:      r.InputCostUSD,
		CachedCostUSD:     r.CachedCostUSD,
		CacheWriteCostUSD: r.CacheWriteCostUSD,
		OutputCostUSD:     r.OutputCostUSD,
		ToolUses:          r.ToolUses,
		Units:             r.Units,
	}
}
