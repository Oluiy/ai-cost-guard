package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/Oluiy/ai-cost-guard/internal/cost"
	"github.com/Oluiy/ai-cost-guard/internal/logging"
)

// The Files and Batches APIs, for OpenAI and Groq.
//
// What FitGuard adds over a plain proxy, because the upstream account is
// shared by every FitGuard key:
//
//   - Ownership. Files and batches are recorded against the key that made
//     them; any id that isn't yours answers 404, so one user can't read,
//     cancel or download another's data.
//   - A budget gate. The input file is priced when uploaded (worst case,
//     at the batch discount) and creating a batch is refused if that
//     wouldn't fit today's budget.
//   - Cost settlement. A batch finishes hours later, so its real cost is
//     read from the output file when it completes and logged then, on the
//     day it settles.
//
// Not supported: listing batches, and any file purpose other than "batch".

// batchDiscount is the price of batch work relative to the synchronous
// rate. OpenAI and Groq both bill batches at half price.
const batchDiscount = 0.5

// batchEndpoints are the request URLs a batch line may target, each with
// its worst-case estimator.
var batchEstimators = map[string]func(model string, body map[string]any) float64{
	"/v1/chat/completions": cost.EstimateWorstCaseCost,
	"/v1/embeddings":       cost.EstimateEmbeddingCost,
	"/v1/responses":        cost.EstimateResponsesCost,
}

func (h *Handler) batchUser(c *fiber.Ctx) (string, bool) {
	userID, ok := h.authenticate(c)
	if !ok {
		_ = c.Status(fiber.StatusUnauthorized).JSON(errorJSON(
			"missing or invalid API key; pass Authorization: Bearer <fitguard key> (see `fitguard init`)", "invalid_api_key"))
	}
	return userID, ok
}

func notFound(c *fiber.Ctx, what string) error {
	return c.Status(fiber.StatusNotFound).JSON(errorJSON("no such "+what, "invalid_request_error"))
}

func (h *Handler) batchProviderFor(name string) (BatchProvider, bool) {
	p, ok := h.Providers[name]
	if !ok {
		return nil, false
	}
	bp, ok := p.(BatchProvider)
	if !ok {
		return nil, false
	}
	if oc, ok := p.(*OpenAICompatProvider); ok && !oc.supportsBatch() {
		return nil, false
	}
	return bp, true
}

// reply relays an upstream answer unchanged.
func reply(c *fiber.Ctx, status int, contentType string, body []byte) error {
	if contentType != "" {
		c.Set("Content-Type", contentType)
	}
	return c.Status(status).Send(body)
}

// parsedBatchFile is what pricing an input file found.
type parsedBatchFile struct {
	provider     string
	models       []string
	estimatedUSD float64
}

func parseBatchInput(data []byte) (parsedBatchFile, error) {
	var out parsedBatchFile
	seen := map[string]bool{}
	lineNo := 0
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		lineNo++
		var l struct {
			URL  string         `json:"url"`
			Body map[string]any `json:"body"`
		}
		if err := json.Unmarshal(line, &l); err != nil {
			return out, fmt.Errorf("line %d is not valid JSON", lineNo)
		}
		model, _ := l.Body["model"].(string)
		if model == "" {
			return out, fmt.Errorf("line %d has no body.model", lineNo)
		}
		est, ok := batchEstimators[l.URL]
		if !ok {
			return out, fmt.Errorf("line %d targets %q; supported: /v1/chat/completions, /v1/embeddings, /v1/responses", lineNo, l.URL)
		}
		provider := RouteProvider(model)
		if out.provider == "" {
			out.provider = provider
		} else if provider != out.provider {
			return out, fmt.Errorf("line %d uses %q (provider %s) but earlier lines use provider %s: a batch file must target one provider", lineNo, model, provider, out.provider)
		}
		out.estimatedUSD += est(model, l.Body) * batchDiscount
		if !seen[model] {
			seen[model] = true
			out.models = append(out.models, model)
		}
	}
	if lineNo == 0 {
		return out, fmt.Errorf("the file has no requests")
	}
	return out, nil
}

// multipartFields pulls the "purpose" value and "file" contents out of an
// upload without disturbing the original body, which is forwarded as is.
func multipartFields(contentType string, body []byte) (purpose string, file []byte, err error) {
	mt, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.HasPrefix(mt, "multipart/") {
		return "", nil, fmt.Errorf("expected a multipart/form-data upload")
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", nil, fmt.Errorf("reading the upload: %w", err)
		}
		data, _ := io.ReadAll(part)
		switch part.FormName() {
		case "purpose":
			purpose = strings.TrimSpace(string(data))
		case "file":
			file = data
		}
	}
	return purpose, file, nil
}

// UploadFile handles POST /v1/files (batch input only).
func (h *Handler) UploadFile(c *fiber.Ctx) error {
	userID, ok := h.batchUser(c)
	if !ok {
		return nil
	}
	body := append([]byte(nil), c.Body()...)
	ct := c.Get("Content-Type")
	purpose, file, err := multipartFields(ct, body)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errorJSON(err.Error(), "invalid_request_error"))
	}
	if purpose != "batch" {
		return c.Status(fiber.StatusBadRequest).JSON(errorJSON(
			"FitGuard only handles file uploads with purpose=batch, so every file can be priced and attributed to a key", "invalid_request_error"))
	}
	parsed, err := parseBatchInput(file)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(errorJSON("invalid batch file: "+err.Error(), "invalid_request_error"))
	}
	bp, ok := h.batchProviderFor(parsed.provider)
	if !ok {
		return c.Status(fiber.StatusBadRequest).JSON(errorJSON(
			"batches are only available for OpenAI and Groq models; this file targets "+parsed.provider, "invalid_request_error"))
	}

	status, rct, resp, err := bp.Forward(c.Context(), http.MethodPost, "/files", ct, body)
	if err != nil || status >= 500 {
		logUpstreamFailure(parsed.provider, "-", "file upload", status, err, resp)
		return c.Status(fiber.StatusBadGateway).JSON(errorJSON(clientFailureMessage(parsed.provider, status, err), "upstream_error"))
	}
	if status < 300 {
		var f struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(resp, &f) == nil && f.ID != "" {
			models, _ := json.Marshal(parsed.models)
			if err := h.Store.SaveBatchFile(c.Context(), logging.BatchFile{
				FileID: f.ID, UserID: userID, Provider: parsed.provider, Models: string(models), EstimatedUSD: parsed.estimatedUSD,
			}); err != nil {
				warnf("recording batch file %s for %s: %v", f.ID, userID, err)
			}
		}
	}
	return reply(c, status, rct, resp)
}

// CreateBatch handles POST /v1/batches.
func (h *Handler) CreateBatch(c *fiber.Ctx) error {
	userID, ok := h.batchUser(c)
	if !ok {
		return nil
	}
	body := append([]byte(nil), c.Body()...)
	var req struct {
		InputFileID string `json:"input_file_id"`
		Endpoint    string `json:"endpoint"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.InputFileID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(errorJSON("\"input_file_id\" is required", "invalid_request_error"))
	}
	file, found, err := h.Store.GetBatchFile(c.Context(), req.InputFileID, userID)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(errorJSON("looking up the input file failed", "server_error"))
	}
	if !found {
		return notFound(c, "file (upload it through FitGuard with purpose=batch first)")
	}
	bp, ok := h.batchProviderFor(file.Provider)
	if !ok {
		return c.Status(fiber.StatusBadRequest).JSON(errorJSON("provider "+file.Provider+" is not configured for batches", "invalid_request_error"))
	}

	// A gate, not a hold: the batch runs for hours, so nothing is kept
	// reserved. The real cost is logged when it settles.
	if h.Budget != nil {
		result, release, err := h.Budget.Reserve(c.Context(), userID, file.EstimatedUSD)
		if err != nil {
			warnf("budget check failed for %s: %v", userID, err)
		} else if !result.Allowed {
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"error": fiber.Map{"message": budgetDenialMessage(result), "type": "budget_exceeded"},
			})
		} else {
			release(0)
		}
	}

	status, rct, resp, err := bp.Forward(c.Context(), http.MethodPost, "/batches", "application/json", body)
	if err != nil || status >= 500 {
		logUpstreamFailure(file.Provider, "-", "batch create", status, err, resp)
		return c.Status(fiber.StatusBadGateway).JSON(errorJSON(clientFailureMessage(file.Provider, status, err), "upstream_error"))
	}
	if status < 300 {
		if b, ok := batchFromResponse(resp); ok {
			endpoint := b.Endpoint
			if endpoint == "" {
				endpoint = req.Endpoint
			}
			if err := h.Store.SaveBatch(c.Context(), logging.Batch{
				BatchID: b.ID, UserID: userID, Provider: file.Provider, Endpoint: endpoint, Status: b.Status,
				OutputFileID: b.OutputFileID, ErrorFileID: b.ErrorFileID, EstimatedUSD: file.EstimatedUSD,
			}); err != nil {
				warnf("recording batch %s for %s: %v", b.ID, userID, err)
			}
		}
	}
	return reply(c, status, rct, resp)
}

type batchObject struct {
	ID           string `json:"id"`
	Endpoint     string `json:"endpoint"`
	Status       string `json:"status"`
	OutputFileID string `json:"output_file_id"`
	ErrorFileID  string `json:"error_file_id"`
}

func batchFromResponse(resp []byte) (batchObject, bool) {
	var b batchObject
	if json.Unmarshal(resp, &b) != nil || b.ID == "" {
		return b, false
	}
	return b, true
}

// GetBatch handles GET /v1/batches/:id.
func (h *Handler) GetBatch(c *fiber.Ctx) error {
	userID, ok := h.batchUser(c)
	if !ok {
		return nil
	}
	b, found, err := h.Store.GetBatch(c.Context(), c.Params("id"), userID)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(errorJSON("looking up the batch failed", "server_error"))
	}
	if !found {
		return notFound(c, "batch")
	}
	bp, ok := h.batchProviderFor(b.Provider)
	if !ok {
		return c.Status(fiber.StatusBadRequest).JSON(errorJSON("provider "+b.Provider+" is not configured for batches", "invalid_request_error"))
	}
	status, rct, resp, err := bp.Forward(c.Context(), http.MethodGet, "/batches/"+b.BatchID, "", nil)
	if err != nil || status >= 500 {
		logUpstreamFailure(b.Provider, "-", "batch get", status, err, resp)
		return c.Status(fiber.StatusBadGateway).JSON(errorJSON(clientFailureMessage(b.Provider, status, err), "upstream_error"))
	}
	if status < 300 {
		if obj, ok := batchFromResponse(resp); ok {
			h.observeBatch(c.Context(), b, obj)
		}
	}
	return reply(c, status, rct, resp)
}

// CancelBatch handles POST /v1/batches/:id/cancel.
func (h *Handler) CancelBatch(c *fiber.Ctx) error {
	userID, ok := h.batchUser(c)
	if !ok {
		return nil
	}
	b, found, err := h.Store.GetBatch(c.Context(), c.Params("id"), userID)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(errorJSON("looking up the batch failed", "server_error"))
	}
	if !found {
		return notFound(c, "batch")
	}
	bp, ok := h.batchProviderFor(b.Provider)
	if !ok {
		return c.Status(fiber.StatusBadRequest).JSON(errorJSON("provider "+b.Provider+" is not configured for batches", "invalid_request_error"))
	}
	status, rct, resp, err := bp.Forward(c.Context(), http.MethodPost, "/batches/"+b.BatchID+"/cancel", "application/json", []byte("{}"))
	if err != nil || status >= 500 {
		logUpstreamFailure(b.Provider, "-", "batch cancel", status, err, resp)
		return c.Status(fiber.StatusBadGateway).JSON(errorJSON(clientFailureMessage(b.Provider, status, err), "upstream_error"))
	}
	if status < 300 {
		if obj, ok := batchFromResponse(resp); ok {
			h.observeBatch(c.Context(), b, obj)
		}
	}
	return reply(c, status, rct, resp)
}

// ListBatches is deliberately unsupported: the upstream list is account
// wide, and returning it would expose other users' batches.
func (h *Handler) ListBatches(c *fiber.Ctx) error {
	return c.Status(fiber.StatusNotImplemented).JSON(errorJSON(
		"listing batches is not supported through FitGuard; fetch a batch by id (GET /v1/batches/{id})", "invalid_request_error"))
}

// GetFile handles GET /v1/files/:id and GET /v1/files/:id/content, for the
// caller's own uploads and the output/error files of their own batches.
func (h *Handler) GetFile(c *fiber.Ctx) error {
	userID, ok := h.batchUser(c)
	if !ok {
		return nil
	}
	id := c.Params("id")
	provider := ""
	if f, found, err := h.Store.GetBatchFile(c.Context(), id, userID); err == nil && found {
		provider = f.Provider
	} else if b, found, err := h.Store.BatchByOutputFile(c.Context(), id, userID); err == nil && found {
		provider = b.Provider
	}
	if provider == "" {
		return notFound(c, "file")
	}
	bp, ok := h.batchProviderFor(provider)
	if !ok {
		return c.Status(fiber.StatusBadRequest).JSON(errorJSON("provider "+provider+" is not configured for batches", "invalid_request_error"))
	}
	path := "/files/" + id
	if strings.HasSuffix(c.Path(), "/content") {
		path += "/content"
	}
	status, rct, resp, err := bp.Forward(c.Context(), http.MethodGet, path, "", nil)
	if err != nil || status >= 500 {
		logUpstreamFailure(provider, "-", "file get", status, err, resp)
		return c.Status(fiber.StatusBadGateway).JSON(errorJSON(clientFailureMessage(provider, status, err), "upstream_error"))
	}
	return reply(c, status, rct, resp)
}

// observeBatch records the latest status and, once the batch has reached an
// end state, settles its cost.
func (h *Handler) observeBatch(ctx context.Context, b logging.Batch, obj batchObject) {
	if err := h.Store.UpdateBatchStatus(ctx, b.BatchID, obj.Status, obj.OutputFileID, obj.ErrorFileID); err != nil {
		warnf("updating batch %s: %v", b.BatchID, err)
	}
	b.Status, b.OutputFileID, b.ErrorFileID = obj.Status, obj.OutputFileID, obj.ErrorFileID
	switch obj.Status {
	case "completed", "failed", "expired", "cancelled":
		if err := h.settleBatch(ctx, b); err != nil {
			warnf("settling batch %s: %v", b.BatchID, err)
		}
	}
}

// StartBatchSettler polls unfinished batches until ctx ends, so their cost
// is logged even when nobody asks for the batch again.
func (h *Handler) StartBatchSettler(ctx context.Context, every time.Duration) {
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				h.pollBatches(ctx)
			}
		}
	}()
}

func (h *Handler) pollBatches(ctx context.Context) {
	pending, err := h.Store.PendingBatches(ctx)
	if err != nil {
		warnf("listing unsettled batches: %v", err)
		return
	}
	for _, b := range pending {
		bp, ok := h.batchProviderFor(b.Provider)
		if !ok {
			continue
		}
		rc, cancel := context.WithTimeout(ctx, 30*time.Second)
		status, _, resp, err := bp.Forward(rc, http.MethodGet, "/batches/"+b.BatchID, "", nil)
		cancel()
		if err != nil || status >= 300 {
			continue
		}
		if obj, ok := batchFromResponse(resp); ok {
			h.observeBatch(ctx, b, obj)
		}
	}
}

// settleBatch prices a finished batch from its output file and logs one row
// per model. Claiming the batch comes before logging, so two instances
// settling at once can't both log it.
func (h *Handler) settleBatch(ctx context.Context, b logging.Batch) error {
	if b.Settled {
		return nil
	}
	perModel := map[string]cost.TokenUsage{}
	if b.OutputFileID != "" {
		bp, ok := h.batchProviderFor(b.Provider)
		if !ok {
			return fmt.Errorf("provider %s is not configured", b.Provider)
		}
		rc, cancel := context.WithTimeout(ctx, 2*time.Minute)
		status, _, data, err := bp.Forward(rc, http.MethodGet, "/files/"+b.OutputFileID+"/content", "", nil)
		cancel()
		if err != nil {
			return err
		}
		if status >= 300 {
			return fmt.Errorf("reading the output file returned status %d", status)
		}
		perModel = sumBatchOutput(data)
	}

	type row struct {
		model string
		u     cost.TokenUsage
		bd    cost.Breakdown
	}
	var rows []row
	total := 0.0
	for m, u := range perModel {
		bd := cost.Compute(m, u).Scaled(batchDiscount)
		rows = append(rows, row{m, u, bd})
		total += bd.Total()
	}

	claimed, err := h.Store.SettleBatch(ctx, b.BatchID, total)
	if err != nil || !claimed {
		return err
	}
	for _, r := range rows {
		usage := Usage{PromptTokens: r.u.Prompt, CompletionTokens: r.u.Completion, CachedTokens: r.u.Cached}
		rec := chatRecord(b.UserID, r.model, usage, r.bd, 0, 0, b.Status, http.StatusOK)
		rec.Kind = "batch"
		h.logRecord(rec)
		h.chargeSettledCost(ctx, b.UserID, r.bd.Total())
	}
	return nil
}

// chargeSettledCost makes a settled batch count against the budget
// backend. The in-process backend reads spend from the request log, which
// already has the row; a shared (Redis) backend keeps its own counter and
// needs telling. It's a charge, not a check: the work already happened.
func (h *Handler) chargeSettledCost(ctx context.Context, userID string, usd float64) {
	if h.Budget == nil || usd <= 0 {
		return
	}
	if _, release, err := h.Budget.Reserve(ctx, userID, usd); err == nil {
		release(usd)
	}
}

// sumBatchOutput totals token usage per model across the lines of a batch
// output file. Failed lines carry no usage and add nothing.
func sumBatchOutput(data []byte) map[string]cost.TokenUsage {
	out := map[string]cost.TokenUsage{}
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var l struct {
			Response struct {
				StatusCode int `json:"status_code"`
				Body       struct {
					Model string `json:"model"`
					Usage struct {
						PromptTokens        int `json:"prompt_tokens"`
						CompletionTokens    int `json:"completion_tokens"`
						InputTokens         int `json:"input_tokens"`
						OutputTokens        int `json:"output_tokens"`
						PromptTokensDetails struct {
							CachedTokens int `json:"cached_tokens"`
						} `json:"prompt_tokens_details"`
						InputTokensDetails struct {
							CachedTokens int `json:"cached_tokens"`
						} `json:"input_tokens_details"`
					} `json:"usage"`
				} `json:"body"`
			} `json:"response"`
		}
		if json.Unmarshal(line, &l) != nil || l.Response.StatusCode != http.StatusOK || l.Response.Body.Model == "" {
			continue
		}
		u := l.Response.Body.Usage
		prompt, completion := u.PromptTokens, u.CompletionTokens
		cached := u.PromptTokensDetails.CachedTokens
		if prompt == 0 && completion == 0 { // the Responses API spells them input/output
			prompt, completion = u.InputTokens, u.OutputTokens
			cached = u.InputTokensDetails.CachedTokens
		}
		t := out[l.Response.Body.Model]
		t.Prompt += prompt
		t.Completion += completion
		t.Cached += cached
		out[l.Response.Body.Model] = t
	}
	return out
}
