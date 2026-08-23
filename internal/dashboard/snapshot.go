package dashboard

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/valyala/fasthttp"

	"github.com/Oluiy/ai-cost-guard/internal/logging"
)

// snapshot is the JSON shape served by both /dashboard/api/data and the SSE feed.
type snapshot struct {
	GeneratedAt string          `json:"generated_at"`
	Range       string          `json:"range"`
	User        string          `json:"user"`
	AllUsers    []string        `json:"all_users"`
	Today       todaySummary    `json:"today"`
	Users       []userRow       `json:"users"`
	Top         []requestRow    `json:"top"`
	Timeseries  []timeseriesRow `json:"timeseries"`
}

type todaySummary struct {
	TotalCostUSD float64 `json:"total_cost_usd"`
	Requests     int64   `json:"requests"`
	CacheHits    int64   `json:"cache_hits"`
	CacheHitRate float64 `json:"cache_hit_rate"`
	ActiveUsers  int     `json:"active_users"`
}

type userRow struct {
	UserID       string  `json:"user_id"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Requests     int64   `json:"requests"`
	CacheHits    int64   `json:"cache_hits"`
}

type requestRow struct {
	Timestamp    string  `json:"timestamp"`
	UserID       string  `json:"user_id"`
	Model        string  `json:"model"`
	CostUSD      float64 `json:"cost_usd"`
	PromptTokens int     `json:"prompt_tokens"`
	CompTokens   int     `json:"completion_tokens"`
	LatencyMS    int64   `json:"latency_ms"`
	FinishReason string  `json:"finish_reason"`
}

type timeseriesRow struct {
	Hour     string  `json:"hour"`
	CostUSD  float64 `json:"cost_usd"`
	Requests int64   `json:"requests"`
}

// rangeWindow resolves a `range` query value into how far back to look and
// how coarsely to bucket the spend-over-time chart. Hourly buckets over a
// multi-day range would mean hundreds of bars in a chart meant to be read
// at a glance, so anything longer than a day buckets by day instead.
// Unrecognized values fall back to "today" rather than erroring — a
// dashboard query param is not worth failing a page load over.
func rangeWindow(rng string) (since time.Time, granularity logging.Granularity, normalized string) {
	now := time.Now().UTC()
	switch rng {
	case "7d":
		return now.Add(-7 * 24 * time.Hour), logging.GranularityDay, "7d"
	case "30d":
		return now.Add(-30 * 24 * time.Hour), logging.GranularityDay, "30d"
	default:
		startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		return startOfDay, logging.GranularityHour, "today"
	}
}

const dateOnlyLayout = "2006-01-02"

// customDateRange parses ?from=&to= (YYYY-MM-DD, as produced by an
// <input type="date">) into an inclusive [since, until] window: since is
// midnight at the start of `from`, until is the last instant of `to` —
// picking "to: Aug 15" without this would silently exclude every request
// that happened on the 15th itself, since a bare date parses to that
// day's midnight. ok is false if either date is missing/malformed, or if
// from is after to, so callers can fall back to the regular range preset
// instead of erroring on a stray/partial query string.
func customDateRange(fromStr, toStr string) (since, until time.Time, ok bool) {
	from, err1 := time.ParseInLocation(dateOnlyLayout, fromStr, time.UTC)
	to, err2 := time.ParseInLocation(dateOnlyLayout, toStr, time.UTC)
	if err1 != nil || err2 != nil || to.Before(from) {
		return time.Time{}, time.Time{}, false
	}
	return from, to.Add(24*time.Hour - time.Millisecond), true
}

func (h *Handler) buildSnapshot(ctx context.Context, rng, userFilter string) (snapshot, error) {
	since, granularity, normalizedRange := rangeWindow(rng)
	now := time.Now().UTC()

	summaries, err := h.Store.SummarySince(ctx, since, userFilter)
	if err != nil {
		return snapshot{}, err
	}
	top, err := h.Store.TopExpensive(ctx, since, 10, userFilter)
	if err != nil {
		return snapshot{}, err
	}
	buckets, err := h.Store.TimeSeriesSince(ctx, since, granularity, userFilter)
	if err != nil {
		return snapshot{}, err
	}
	// Always drawn from a fixed, generous window, unfiltered by the
	// current selection, so switching the range or user filter never
	// makes an option disappear from its own dropdown.
	allUsers, err := h.Store.DistinctUsers(ctx, now.Add(-30*24*time.Hour))
	if err != nil {
		return snapshot{}, err
	}

	today := todaySummary{ActiveUsers: len(summaries)}
	users := make([]userRow, 0, len(summaries))
	for _, s := range summaries {
		today.TotalCostUSD += s.TotalCostUSD
		today.Requests += s.Requests
		today.CacheHits += s.CacheHits
		users = append(users, userRow{
			UserID: s.UserID, TotalCostUSD: s.TotalCostUSD, Requests: s.Requests, CacheHits: s.CacheHits,
		})
	}
	if today.Requests > 0 {
		today.CacheHitRate = float64(today.CacheHits) / float64(today.Requests)
	}

	topRows := make([]requestRow, 0, len(top))
	for _, r := range top {
		topRows = append(topRows, requestRow{
			Timestamp: r.Timestamp.Format(time.RFC3339), UserID: r.UserID, Model: r.Model,
			CostUSD: r.CostUSD, PromptTokens: r.PromptTokens, CompTokens: r.CompletionTokens,
			LatencyMS: r.LatencyMS, FinishReason: r.FinishReason,
		})
	}

	tsRows := make([]timeseriesRow, 0, len(buckets))
	for _, b := range buckets {
		tsRows = append(tsRows, timeseriesRow{
			Hour: b.Hour.Format(time.RFC3339), CostUSD: b.CostUSD, Requests: b.Requests,
		})
	}

	return snapshot{
		GeneratedAt: now.Format(time.RFC3339),
		Range:       normalizedRange,
		User:        userFilter,
		AllUsers:    allUsers,
		Today:       today,
		Users:       users,
		Top:         topRows,
		Timeseries:  tsRows,
	}, nil
}

// Data serves a single JSON snapshot. Accepts ?range=today|7d|30d and
// ?user=<id> to scope it, both optional.
func (h *Handler) Data(c *fiber.Ctx) error {
	ctx, cancel := context.WithTimeout(c.Context(), snapshotQueryTimeout)
	defer cancel()
	snap, err := h.buildSnapshot(ctx, c.Query("range"), c.Query("user"))
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(snap)
}

// Events streams the same snapshot as Server-Sent Events every few seconds
// so the dashboard updates live without a manual refresh. The range/user
// filter is captured once, from the query string the client connected
// with — a client that changes the filter reconnects with a new request
// rather than pushing a filter change down an open stream.
func (h *Handler) Events(c *fiber.Ctx) error {
	rng := c.Query("range")
	userFilter := c.Query("user")

	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")

	c.Context().SetBodyStreamWriter(fasthttp.StreamWriter(func(w *bufio.Writer) {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			ctx, cancel := context.WithTimeout(context.Background(), snapshotQueryTimeout)
			snap, err := h.buildSnapshot(ctx, rng, userFilter)
			cancel()
			if err == nil {
				b, _ := json.Marshal(snap)
				fmt.Fprintf(w, "data: %s\n\n", b)
				if w.Flush() != nil {
					return
				}
			}
			<-ticker.C
		}
	}))
	return nil
}
