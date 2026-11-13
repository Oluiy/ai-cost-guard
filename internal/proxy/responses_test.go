package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/Oluiy/ai-cost-guard/internal/cache"
	"github.com/Oluiy/ai-cost-guard/internal/config"
	"github.com/Oluiy/ai-cost-guard/internal/cost"
	"github.com/Oluiy/ai-cost-guard/internal/logging"
)

const responsesJSON = `{"id":"resp_1","object":"response","status":"completed",
	"output":[{"type":"web_search_call"},{"type":"message","content":[{"type":"output_text","text":"hi"}]}],
	"usage":{"input_tokens":1000,"output_tokens":100,"input_tokens_details":{"cached_tokens":800},"output_tokens_details":{"reasoning_tokens":40}}}`

const responsesSSE = "event: response.created\n" +
	"data: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\",\"usage\":null}}\n\n" +
	"event: response.output_text.delta\n" +
	"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
	"event: response.completed\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\"}]," +
	"\"usage\":{\"input_tokens\":1000,\"output_tokens\":100,\"input_tokens_details\":{\"cached_tokens\":800}}}}\n\n"

type responsesFixture struct {
	app   *fiber.App
	store *logging.Store
	hits  *int32
	path  *atomic.Value
}

func newResponsesFixture(t *testing.T, providers func(url string) map[string]config.Provider) responsesFixture {
	t.Helper()
	var hits int32
	var path atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		path.Store(r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, responsesSSE)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, responsesJSON)
	}))
	t.Cleanup(srv.Close)

	store, err := logging.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	cfg := &config.Config{Providers: providers(srv.URL), Cache: config.CacheConfig{Enabled: true, TTL: 60}}
	h := New(cfg, config.NewSettings("", cfg), cache.NewMemoryCache(), nil, store)
	app := fiber.New()
	app.Post("/v1/responses", h.Responses)
	return responsesFixture{app: app, store: store, hits: &hits, path: &path}
}

func openAIOnly(url string) map[string]config.Provider {
	return map[string]config.Provider{"openai": {APIKey: "k", BaseURL: url}}
}

func (f responsesFixture) post(t *testing.T, body string) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.app.Test(req, 5000)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func (f responsesFixture) rows(t *testing.T) []logging.Record {
	t.Helper()
	rows, _, err := f.store.ListRequests(context.Background(), time.Now().Add(-time.Hour), time.Time{}, "", "", "", "time", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestResponses_PassThroughPricesAndFlagsTools(t *testing.T) {
	f := newResponsesFixture(t, openAIOnly)
	resp, body := f.post(t, `{"model":"gpt-4o","input":"hello"}`)
	if resp.StatusCode != 200 || !strings.Contains(body, `"resp_1"`) {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if f.path.Load() != "/responses" {
		t.Fatalf("upstream path = %v, want /responses", f.path.Load())
	}
	if !strings.Contains(resp.Header.Get("X-AI-Guard-Warning"), "tool fees") {
		t.Errorf("a web_search_call in the output must raise the tool-fee warning, got %q", resp.Header.Get("X-AI-Guard-Warning"))
	}
	rows := f.rows(t)
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	r := rows[0]
	want := cost.Compute("gpt-4o", cost.TokenUsage{Prompt: 1000, Cached: 800, Completion: 100})
	if r.Kind != "response" || r.CostUSD != want.Total() || r.CachedTokens != 800 || r.ToolUses != 1 {
		t.Fatalf("record %+v, want kind=response cost=%v cached=800 tooluses=1", r, want.Total())
	}
}

func TestResponses_CacheOnlyForPureRequests(t *testing.T) {
	f := newResponsesFixture(t, openAIOnly)
	plain := `{"model":"gpt-4o","input":"same"}`
	f.post(t, plain)
	if resp, _ := f.post(t, plain); resp.Header.Get("X-Cache") != "HIT" || atomic.LoadInt32(f.hits) != 1 {
		t.Fatalf("control: repeat should hit the cache (X-Cache=%q, upstream calls=%d)", resp.Header.Get("X-Cache"), atomic.LoadInt32(f.hits))
	}

	for name, body := range map[string]string{
		"server tool":          `{"model":"gpt-4o","input":"s1","tools":[{"type":"web_search"}]}`,
		"previous_response_id": `{"model":"gpt-4o","input":"s2","previous_response_id":"resp_0"}`,
		"conversation":         `{"model":"gpt-4o","input":"s3","conversation":"conv_1"}`,
	} {
		atomic.StoreInt32(f.hits, 0)
		f.post(t, body)
		resp, _ := f.post(t, body)
		if resp.Header.Get("X-Cache") == "HIT" || atomic.LoadInt32(f.hits) != 2 {
			t.Errorf("%s: must never be served from cache (X-Cache=%q, upstream calls=%d)", name, resp.Header.Get("X-Cache"), atomic.LoadInt32(f.hits))
		}
	}
}

func TestResponses_StreamRelaysVerbatimAndLogsFinalUsage(t *testing.T) {
	f := newResponsesFixture(t, openAIOnly)
	resp, body := f.post(t, `{"model":"gpt-4o","input":"hello","stream":true}`)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content type %q", resp.Header.Get("Content-Type"))
	}
	if body != responsesSSE {
		t.Fatalf("stream was altered:\n%s", body)
	}
	rows := f.rows(t)
	want := cost.Compute("gpt-4o", cost.TokenUsage{Prompt: 1000, Cached: 800, Completion: 100})
	if len(rows) != 1 || rows[0].CostUSD != want.Total() || rows[0].Kind != "response" {
		t.Fatalf("rows %+v, want one response row costing %v", rows, want.Total())
	}
}

func TestResponses_RefusesWhatItCannotPrice(t *testing.T) {
	f := newResponsesFixture(t, func(url string) map[string]config.Provider {
		return map[string]config.Provider{
			"openai":    {APIKey: "k", BaseURL: url},
			"anthropic": {APIKey: "k", BaseURL: url},
			"together":  {APIKey: "k", BaseURL: url},
		}
	})
	cases := map[string]string{
		"background": `{"model":"gpt-4o","input":"x","background":true}`,
		"anthropic":  `{"model":"claude-sonnet-5","input":"x"}`,
		"together":   `{"model":"meta-llama/Llama-3-70b","input":"x"}`,
		"no model":   `{"input":"x"}`,
	}
	for name, body := range cases {
		resp, out := f.post(t, body)
		if resp.StatusCode != 400 {
			t.Errorf("%s: status %d (%s), want 400", name, resp.StatusCode, out)
		}
	}
	if atomic.LoadInt32(f.hits) != 0 {
		t.Fatalf("refused requests must not reach any provider, got %d upstream calls", atomic.LoadInt32(f.hits))
	}
}

func TestResponses_TruncationIsFlagged(t *testing.T) {
	srv := jsonStub(t, `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":5,"output_tokens":9}}`)
	p := NewOpenAICompatProvider(srv.URL, "k", "openai")
	_, u, finish, _, err := p.Responses(context.Background(), "gpt-4o", []byte(`{"input":"x"}`))
	if err != nil || finish != "length" || u.CompletionTokens != 9 {
		t.Fatalf("finish=%q usage=%+v err=%v", finish, u, err)
	}
}

func TestAudioSpeech_GroqBadVoiceIs400NotBadGateway(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		io.WriteString(w, "RIFF")
	}))
	t.Cleanup(srv.Close)
	cfg := &config.Config{Providers: map[string]config.Provider{"groq": {APIKey: "k", BaseURL: srv.URL}}}
	store, err := logging.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	h := New(cfg, config.NewSettings("", cfg), cache.NewMemoryCache(), nil, store)
	app := fiber.New()
	app.Post("/v1/audio/speech", h.AudioSpeech)

	post := func(body string) (*http.Response, string) {
		req := httptest.NewRequest("POST", "/v1/audio/speech", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req, 5000)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	resp, out := post(`{"model":"canopylabs/orpheus-v1-english","input":"hi","voice":"alloy"}`)
	if resp.StatusCode != 400 || !strings.Contains(out, "hannah") || atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("got %d %s (upstream calls %d), want a 400 naming Groq's voices and no upstream call", resp.StatusCode, out, calls)
	}
	resp, _ = post(`{"model":"canopylabs/orpheus-v1-english","input":"hi","voice":"troy"}`)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/wav" {
		t.Fatalf("valid request: %d, content type %q, want 200 audio/wav", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}
