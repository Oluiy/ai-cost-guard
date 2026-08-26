package dashboard

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
)

// requestsPage is the JSON shape served by /dashboard/api/requests.
type requestsPage struct {
	Requests     []requestListRow `json:"requests"`
	Total        int64            `json:"total"`
	TotalCostUSD float64          `json:"total_cost_usd"`
	Limit        int              `json:"limit"`
	Offset       int              `json:"offset"`
	AllModels    []string         `json:"all_models"`
	Range        string           `json:"range"`
}

type requestListRow struct {
	Timestamp    string  `json:"timestamp"`
	UserID       string  `json:"user_id"`
	Model        string  `json:"model"`
	CostUSD      float64 `json:"cost_usd"`
	PromptTokens int     `json:"prompt_tokens"`
	CompTokens   int     `json:"completion_tokens"`
	LatencyMS    int64   `json:"latency_ms"`
	FinishReason string  `json:"finish_reason"`
	CacheHit     bool    `json:"cache_hit"`
	StatusCode   int     `json:"status_code"`
}

const (
	requestsDefaultLimit = 25
	requestsMaxLimit     = 100
)

// Requests serves a paginated, filterable page of the full request log,
// newest first. Filters: ?range=, ?user=, ?model=,
// ?status=success|error, ?limit=/?offset=. ?from=&to= (YYYY-MM-DD)
// override ?range= with an explicit date range.
func (h *Handler) Requests(c *fiber.Ctx) error {
	ctx, cancel := context.WithTimeout(c.Context(), snapshotQueryTimeout)
	defer cancel()

	userFilter := c.Query("user")
	modelFilter := c.Query("model")
	statusFilter := c.Query("status")
	sortBy := c.Query("sort")

	limit := c.QueryInt("limit", requestsDefaultLimit)
	if limit <= 0 || limit > requestsMaxLimit {
		limit = requestsDefaultLimit
	}
	offset := c.QueryInt("offset", 0)
	if offset < 0 {
		offset = 0
	}

	var since, until time.Time
	var normalizedRange string
	if customSince, customUntil, ok := customDateRange(c.Query("from"), c.Query("to")); ok {
		since, until, normalizedRange = customSince, customUntil, "custom"
	} else {
		since, _, normalizedRange = rangeWindow(c.Query("range"))
	}

	records, total, err := h.Store.ListRequests(ctx, since, until, userFilter, modelFilter, statusFilter, sortBy, limit, offset)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	periodSummary, err := h.Store.PeriodSummary(ctx, since, until, userFilter, modelFilter, statusFilter)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	allModels, err := h.Store.DistinctModels(ctx, since)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}

	rows := make([]requestListRow, 0, len(records))
	for _, r := range records {
		rows = append(rows, requestListRow{
			Timestamp: r.Timestamp.Format(time.RFC3339), UserID: r.UserID, Model: r.Model,
			CostUSD: r.CostUSD, PromptTokens: r.PromptTokens, CompTokens: r.CompletionTokens,
			LatencyMS: r.LatencyMS, FinishReason: r.FinishReason, CacheHit: r.CacheHit, StatusCode: r.StatusCode,
		})
	}

	return c.JSON(requestsPage{
		Requests: rows, Total: total, TotalCostUSD: periodSummary.TotalCostUSD, Limit: limit, Offset: offset,
		AllModels: allModels, Range: normalizedRange,
	})
}

// csvSafe defuses formula/CSV injection (CWE-1236): a cell starting with
// =, +, -, or @ opens as a formula in Excel/Sheets. `model` is
// attacker-controlled, so it's logged and exported verbatim otherwise.
// Prefixing with a quote forces plain-text interpretation.
func csvSafe(s string) string {
	if s != "" && strings.ContainsAny(s[:1], "=+-@") {
		return "'" + s
	}
	return s
}

// reportMaxRows caps how many rows a CSV export includes. Hitting it is a
// sign to narrow the filters, not a reason to build streamed pagination.
const reportMaxRows = 10000

type reportSummary struct {
	Since        string  `json:"since"`
	Until        string  `json:"until"`
	Requests     int64   `json:"requests"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	CacheHits    int64   `json:"cache_hits"`
	CacheHitRate float64 `json:"cache_hit_rate"`
}

// Report generates a summary, and with ?format=csv a downloadable export,
// for an explicit ?from=&to= date range (YYYY-MM-DD, inclusive), scoped
// by the same filters as Requests.
func (h *Handler) Report(c *fiber.Ctx) error {
	ctx, cancel := context.WithTimeout(c.Context(), snapshotQueryTimeout)
	defer cancel()

	since, until, ok := customDateRange(c.Query("from"), c.Query("to"))
	if !ok {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": fiber.Map{"message": "from and to (YYYY-MM-DD, from <= to) are required", "type": "invalid_request_error"},
		})
	}
	userFilter := c.Query("user")
	modelFilter := c.Query("model")
	statusFilter := c.Query("status")

	summary, err := h.Store.PeriodSummary(ctx, since, until, userFilter, modelFilter, statusFilter)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	var cacheHitRate float64
	if summary.Requests > 0 {
		cacheHitRate = float64(summary.CacheHits) / float64(summary.Requests)
	}

	if c.Query("format") != "csv" {
		return c.JSON(reportSummary{
			Since: since.Format(time.RFC3339), Until: until.Format(time.RFC3339),
			Requests: summary.Requests, TotalCostUSD: summary.TotalCostUSD,
			CacheHits: summary.CacheHits, CacheHitRate: cacheHitRate,
		})
	}

	records, _, err := h.Store.ListRequests(ctx, since, until, userFilter, modelFilter, statusFilter, "time", reportMaxRows, 0)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}

	var buf bytes.Buffer
	buf.WriteString(fmt.Sprintf("# FitGuard report: %s to %s\n", c.Query("from"), c.Query("to")))
	buf.WriteString(fmt.Sprintf("# Requests: %d, Total spend: $%.4f, Cache hit rate: %.1f%%\n",
		summary.Requests, summary.TotalCostUSD, cacheHitRate*100))
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"timestamp", "user_id", "model", "status_code", "cache_hit", "prompt_tokens", "completion_tokens", "latency_ms", "cost_usd"})
	for _, r := range records {
		_ = w.Write([]string{
			r.Timestamp.Format(time.RFC3339), csvSafe(r.UserID), csvSafe(r.Model), strconv.Itoa(r.StatusCode), strconv.FormatBool(r.CacheHit),
			strconv.Itoa(r.PromptTokens), strconv.Itoa(r.CompletionTokens), strconv.FormatInt(r.LatencyMS, 10),
			strconv.FormatFloat(r.CostUSD, 'f', -1, 64),
		})
	}
	w.Flush()

	c.Set("Content-Type", "text/csv; charset=utf-8")
	c.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="fitguard-report-%s-to-%s.csv"`, c.Query("from"), c.Query("to")))
	return c.Send(buf.Bytes())
}
