// Package dashboard serves the embedded ai-guard cost dashboard.
package dashboard

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/valyala/fasthttp"

	"github.com/aicostguard/ai-cost-guard/internal/logging"
)

//go:embed static/index.html
var staticFS embed.FS

// Handler serves the dashboard UI and its JSON/SSE data feeds.
type Handler struct {
	Store *logging.Store
}

func New(store *logging.Store) *Handler {
	return &Handler{Store: store}
}

// Register mounts the dashboard routes on app.
func (h *Handler) Register(app *fiber.App) {
	app.Get("/dashboard", h.Index)
	app.Get("/dashboard/api/data", h.Data)
	app.Get("/dashboard/events", h.Events)
}

func (h *Handler) Index(c *fiber.Ctx) error {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		return err
	}
	c.Set("Content-Type", "text/html; charset=utf-8")
	return c.Send(b)
}

// snapshot is the JSON shape served by both /dashboard/api/data and the SSE feed.
type snapshot struct {
	GeneratedAt string          `json:"generated_at"`
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

func (h *Handler) buildSnapshot(ctx context.Context) (snapshot, error) {
	now := time.Now().UTC()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	last24h := now.Add(-24 * time.Hour)

	summaries, err := h.Store.SummarySince(ctx, startOfDay)
	if err != nil {
		return snapshot{}, err
	}
	top, err := h.Store.TopExpensive(ctx, startOfDay, 10)
	if err != nil {
		return snapshot{}, err
	}
	buckets, err := h.Store.TimeSeriesSince(ctx, last24h)
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
		Today:       today,
		Users:       users,
		Top:         topRows,
		Timeseries:  tsRows,
	}, nil
}

// snapshotQueryTimeout bounds each dashboard query so a stuck disk can't
// hang a request (Data) or wedge the streaming loop open forever (Events).
const snapshotQueryTimeout = 5 * time.Second

// Data serves a single JSON snapshot.
func (h *Handler) Data(c *fiber.Ctx) error {
	ctx, cancel := context.WithTimeout(c.Context(), snapshotQueryTimeout)
	defer cancel()
	snap, err := h.buildSnapshot(ctx)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(snap)
}

// Events streams the same snapshot as Server-Sent Events every few seconds
// so the dashboard updates live without a manual refresh.
func (h *Handler) Events(c *fiber.Ctx) error {
	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")

	c.Context().SetBodyStreamWriter(fasthttp.StreamWriter(func(w *bufio.Writer) {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			ctx, cancel := context.WithTimeout(context.Background(), snapshotQueryTimeout)
			snap, err := h.buildSnapshot(ctx)
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
