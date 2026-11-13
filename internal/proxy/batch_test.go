package proxy

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/Oluiy/ai-cost-guard/internal/budget"
	"github.com/Oluiy/ai-cost-guard/internal/cache"
	"github.com/Oluiy/ai-cost-guard/internal/config"
	"github.com/Oluiy/ai-cost-guard/internal/cost"
	"github.com/Oluiy/ai-cost-guard/internal/logging"
)

// One chat request and one response line, 1M prompt + 1M completion tokens
// so the expected cost is easy to reason about.
const batchInput = `{"custom_id":"a","method":"POST","url":"/v1/chat/completions","body":{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":100}}` + "\n"

const batchOutput = `{"id":"x","custom_id":"a","response":{"status_code":200,"body":{"model":"gpt-4o-2024-08-06","usage":{"prompt_tokens":1000000,"completion_tokens":1000000}}}}` + "\n" +
	`{"id":"y","custom_id":"b","response":{"status_code":400,"body":{"model":"gpt-4o","usage":{"prompt_tokens":999,"completion_tokens":999}}}}` + "\n"

type batchFixture struct {
	app       *fiber.App
	h         *Handler
	store     *logging.Store
	mu        sync.Mutex
	status    string // what the stub reports for the batch
	uploaded  []byte // multipart body the stub received, verbatim
	outReads  int
	createHit int
}

func newBatchFixture(t *testing.T, limitUSD float64) *batchFixture {
	t.Helper()
	f := &batchFixture{status: "validating"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/files":
			f.uploaded, _ = io.ReadAll(r.Body)
			io.WriteString(w, `{"id":"file-in","purpose":"batch"}`)
		case r.Method == "POST" && r.URL.Path == "/batches":
			f.createHit++
			io.WriteString(w, `{"id":"batch_1","endpoint":"/v1/chat/completions","status":"validating"}`)
		case r.Method == "GET" && r.URL.Path == "/batches/batch_1":
			out := ""
			if f.status == "completed" {
				out = "file-out"
			}
			io.WriteString(w, `{"id":"batch_1","endpoint":"/v1/chat/completions","status":"`+f.status+`","output_file_id":"`+out+`"}`)
		case r.URL.Path == "/batches/batch_1/cancel" || (r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/files/file-in")):
			io.WriteString(w, `{"id":"batch_1","status":"cancelling"}`)
		case r.Method == "GET" && r.URL.Path == "/files/file-out/content":
			f.outReads++
			io.WriteString(w, batchOutput)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	store, err := logging.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	cfg := &config.Config{
		Providers: map[string]config.Provider{"openai": {APIKey: "k", BaseURL: srv.URL}, "together": {APIKey: "k", BaseURL: srv.URL}},
		Keys:      map[string]string{"key-alice": "alice", "key-bob": "bob"},
		Users:     map[string]config.Budget{"alice": {DailyLimitUSD: limitUSD}, "bob": {DailyLimitUSD: limitUSD}},
	}
	settings := config.NewSettings("", cfg)
	h := New(cfg, settings, cache.NewMemoryCache(), budget.NewFailClosed(store, settings, false), store)
	f.h = h
	f.app = fiber.New()
	f.app.Post("/v1/files", h.UploadFile)
	f.app.Get("/v1/files/:id", h.GetFile)
	f.app.Get("/v1/files/:id/content", h.GetFile)
	f.app.Post("/v1/batches", h.CreateBatch)
	f.app.Get("/v1/batches", h.ListBatches)
	f.app.Get("/v1/batches/:id", h.GetBatch)
	f.app.Post("/v1/batches/:id/cancel", h.CancelBatch)
	f.store = store
	return f
}

func (f *batchFixture) do(t *testing.T, key, method, path, contentType string, body []byte) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := f.app.Test(req, 5000)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func multipartUpload(t *testing.T, purpose, content string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("purpose", purpose)
	fw, _ := mw.CreateFormFile("file", "in.jsonl")
	io.WriteString(fw, content)
	mw.Close()
	return buf.Bytes(), mw.FormDataContentType()
}

func (f *batchFixture) upload(t *testing.T, key, purpose, content string) (*http.Response, string) {
	body, ct := multipartUpload(t, purpose, content)
	return f.do(t, key, "POST", "/v1/files", ct, body)
}

func (f *batchFixture) batchRows(t *testing.T) []logging.Record {
	t.Helper()
	rows, _, err := f.store.ListRequests(context.Background(), time.Now().Add(-time.Hour), time.Time{}, "", "", "", "time", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestBatch_UploadValidatesAndForwardsUnchanged(t *testing.T) {
	f := newBatchFixture(t, 100)
	body, ct := multipartUpload(t, "batch", batchInput)
	resp, out := f.do(t, "key-alice", "POST", "/v1/files", ct, body)
	if resp.StatusCode != 200 || !strings.Contains(out, "file-in") {
		t.Fatalf("upload: %d %s", resp.StatusCode, out)
	}
	if !bytes.Equal(f.uploaded, body) {
		t.Fatal("the upload must reach the provider byte for byte")
	}

	for name, c := range map[string]struct{ purpose, content string }{
		"wrong purpose":  {"assistants", batchInput},
		"not json":       {"batch", "nope\n"},
		"empty":          {"batch", "\n"},
		"unknown url":    {"batch", `{"url":"/v1/images","body":{"model":"gpt-4o"}}` + "\n"},
		"mixed provider": {"batch", batchInput + `{"url":"/v1/chat/completions","body":{"model":"claude-sonnet-5"}}` + "\n"},
		"no batch api":   {"batch", `{"url":"/v1/chat/completions","body":{"model":"meta-llama/Llama-3-70b"}}` + "\n"},
	} {
		if r, o := f.upload(t, "key-alice", c.purpose, c.content); r.StatusCode != 400 {
			t.Errorf("%s: status %d (%s), want 400", name, r.StatusCode, o)
		}
	}
	if r, _ := f.upload(t, "", "batch", batchInput); r.StatusCode != 401 {
		t.Errorf("no key: status %d, want 401", r.StatusCode)
	}
}

func TestBatch_NobodyElsesBatchOrFileIsVisible(t *testing.T) {
	f := newBatchFixture(t, 100)
	f.upload(t, "key-alice", "batch", batchInput)
	f.do(t, "key-alice", "POST", "/v1/batches", "application/json", []byte(`{"input_file_id":"file-in","endpoint":"/v1/chat/completions","completion_window":"24h"}`))

	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/batches/batch_1"},
		{"POST", "/v1/batches/batch_1/cancel"},
		{"GET", "/v1/files/file-in"},
		{"GET", "/v1/files/file-in/content"},
	} {
		if r, _ := f.do(t, "key-bob", c.method, c.path, "", nil); r.StatusCode != 404 {
			t.Errorf("bob %s %s = %d, want 404 (alice's data)", c.method, c.path, r.StatusCode)
		}
		if r, _ := f.do(t, "key-alice", c.method, c.path, "", nil); r.StatusCode == 404 {
			t.Errorf("alice %s %s = 404, her own data must be reachable", c.method, c.path)
		}
	}
	// Bob can't build a batch on Alice's file either.
	if r, _ := f.do(t, "key-bob", "POST", "/v1/batches", "application/json", []byte(`{"input_file_id":"file-in"}`)); r.StatusCode != 404 {
		t.Errorf("bob creating a batch from alice's file = %d, want 404", r.StatusCode)
	}
	if r, _ := f.do(t, "key-alice", "GET", "/v1/batches", "", nil); r.StatusCode != 501 {
		t.Errorf("list = %d, want 501", r.StatusCode)
	}
}

func TestBatch_CreationIsGatedByTheBudget(t *testing.T) {
	// The file's estimate is a few cents at most; a $0 limit can't hold it.
	// A limit of 0 means unlimited, so use a vanishingly small one.
	f := newBatchFixture(t, 0.0000001)
	f.upload(t, "key-alice", "batch", batchInput)
	resp, out := f.do(t, "key-alice", "POST", "/v1/batches", "application/json", []byte(`{"input_file_id":"file-in"}`))
	if resp.StatusCode != 429 || !strings.Contains(out, "budget") {
		t.Fatalf("got %d %s, want 429 budget_exceeded", resp.StatusCode, out)
	}
	if f.createHit != 0 {
		t.Fatal("a refused batch must never reach the provider")
	}
}

func TestBatch_CompletionIsPricedAtHalfRateAndLoggedOnce(t *testing.T) {
	f := newBatchFixture(t, 1000)
	f.upload(t, "key-alice", "batch", batchInput)
	f.do(t, "key-alice", "POST", "/v1/batches", "application/json", []byte(`{"input_file_id":"file-in","endpoint":"/v1/chat/completions","completion_window":"24h"}`))

	// Still running: nothing to log yet.
	f.do(t, "key-alice", "GET", "/v1/batches/batch_1", "", nil)
	if n := len(f.batchRows(t)); n != 0 {
		t.Fatalf("a running batch logged %d rows", n)
	}

	f.status = "completed"
	for i := 0; i < 3; i++ { // polling a finished batch repeatedly must not re-log it
		if r, _ := f.do(t, "key-alice", "GET", "/v1/batches/batch_1", "", nil); r.StatusCode != 200 {
			t.Fatalf("get: %d", r.StatusCode)
		}
	}
	rows := f.batchRows(t)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want exactly 1 (the failed line adds nothing, repeats add nothing)", len(rows))
	}
	r := rows[0]
	want := cost.Compute("gpt-4o-2024-08-06", cost.TokenUsage{Prompt: 1000000, Completion: 1000000}).Scaled(0.5)
	if r.Kind != "batch" || r.UserID != "alice" || r.CostUSD != want.Total() || want.Total() == 0 {
		t.Fatalf("row %+v, want kind=batch user=alice cost=%v (half of the synchronous price)", r, want.Total())
	}
	if f.outReads != 1 {
		t.Fatalf("output file read %d times, want 1", f.outReads)
	}
	// Alice can now download the output file; Bob still cannot.
	if resp, _ := f.do(t, "key-alice", "GET", "/v1/files/file-out/content", "", nil); resp.StatusCode != 200 {
		t.Errorf("alice's output file: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "key-bob", "GET", "/v1/files/file-out/content", "", nil); resp.StatusCode != 404 {
		t.Errorf("bob read alice's output file: %d", resp.StatusCode)
	}
}

func TestBatch_BackgroundPollSettlesWithoutAnyoneAsking(t *testing.T) {
	f := newBatchFixture(t, 1000)
	f.upload(t, "key-alice", "batch", batchInput)
	f.do(t, "key-alice", "POST", "/v1/batches", "application/json", []byte(`{"input_file_id":"file-in"}`))
	f.status = "completed"

	h := f.h
	h.pollBatches(context.Background())
	if n := len(f.batchRows(t)); n != 1 {
		t.Fatalf("poller logged %d rows, want 1", n)
	}
	h.pollBatches(context.Background())
	if n := len(f.batchRows(t)); n != 1 {
		t.Fatalf("a second poll re-logged the batch (%d rows)", n)
	}
}
