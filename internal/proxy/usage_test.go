package proxy

import (
	"bufio"
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

// --- Usage parsing: each provider reports cache and thinking tokens
// differently, and getting any of them wrong silently mis-prices requests.

const chatReq = `{"model":"x","messages":[{"role":"user","content":"hi"}]}`

func jsonStub(t *testing.T, body string) *httptest.Server {
	return stubProvider(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	})
}

func TestOpenAIUsage_CachedTokensAreASubsetOfPrompt(t *testing.T) {
	srv := jsonStub(t, `{"choices":[{"finish_reason":"stop","message":{}}],
		"usage":{"prompt_tokens":1000,"completion_tokens":100,"prompt_tokens_details":{"cached_tokens":800}}}`)
	p := NewOpenAICompatProvider(srv.URL, "k", "openai")
	_, u, _, _, err := p.ChatCompletion(context.Background(), "gpt-4o", []byte(chatReq))
	if err != nil {
		t.Fatal(err)
	}
	if u.PromptTokens != 1000 || u.CachedTokens != 800 || u.CompletionTokens != 100 {
		t.Fatalf("got %+v, want prompt 1000 (cached included), cached 800, completion 100", u)
	}
}

// Groq lists each built-in tool it ran on the message.
func TestOpenAIUsage_GroqExecutedToolsCounted(t *testing.T) {
	srv := jsonStub(t, `{"choices":[{"finish_reason":"stop","message":{"executed_tools":[{"type":"search"},{"type":"search"}]}}],
		"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	p := NewOpenAICompatProvider(srv.URL, "k", "groq")
	_, u, _, _, err := p.ChatCompletion(context.Background(), "openai/gpt-oss-120b", []byte(chatReq))
	if err != nil {
		t.Fatal(err)
	}
	if u.ToolUses != 2 {
		t.Fatalf("ToolUses = %d, want 2", u.ToolUses)
	}
}

// The Anthropic trap: input_tokens is only the UNCACHED remainder. Reading
// it alone dropped every cached token from the count.
func TestAnthropicUsage_InputTokensExcludeCacheBuckets(t *testing.T) {
	srv := jsonStub(t, `{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":100,"cache_creation_input_tokens":50,"cache_read_input_tokens":800,"output_tokens":10}}`)
	p := NewAnthropicProvider(srv.URL, "k")
	_, u, _, _, err := p.ChatCompletion(context.Background(), "claude-sonnet-5", []byte(chatReq))
	if err != nil {
		t.Fatal(err)
	}
	if u.PromptTokens != 950 || u.CachedTokens != 800 || u.CacheWriteTokens != 50 || u.CompletionTokens != 10 {
		t.Fatalf("got %+v, want prompt 950 = 100+50+800, cached 800, write 50", u)
	}
}

func TestAnthropicStreamUsage_CacheFieldsFromMessageStart(t *testing.T) {
	sse := strings.Join([]string{
		event("message_start", `{"message":{"usage":{"input_tokens":20,"cache_read_input_tokens":500,"cache_creation_input_tokens":0}}}`),
		event("content_block_start", `{"index":0,"content_block":{"type":"text"}}`),
		event("content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"hi"}}`),
		event("message_delta", `{"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`),
	}, "")
	s := &anthropicStreamSession{resp: &http.Response{Body: io.NopCloser(strings.NewReader(sse))}, model: "claude-sonnet-5"}
	var sink strings.Builder
	_, _, u, _, err := s.Relay(bufio.NewWriter(&sink))
	if err != nil {
		t.Fatal(err)
	}
	if u.PromptTokens != 520 || u.CachedTokens != 500 || u.CompletionTokens != 7 {
		t.Fatalf("got %+v, want prompt 520, cached 500, completion 7", u)
	}
}

// Google bills thinking tokens as output, but reports them separately from
// candidatesTokenCount.
func TestGeminiUsage_ThinkingTokensBillAsOutputAndCacheIsSubset(t *testing.T) {
	srv := jsonStub(t, `{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":1000,"cachedContentTokenCount":600,"candidatesTokenCount":40,"thoughtsTokenCount":160}}`)
	p := NewGeminiProvider(srv.URL, "k")
	_, u, _, _, err := p.ChatCompletion(context.Background(), "gemini-2.5-flash", []byte(chatReq))
	if err != nil {
		t.Fatal(err)
	}
	if u.PromptTokens != 1000 || u.CachedTokens != 600 || u.CompletionTokens != 200 {
		t.Fatalf("got %+v, want prompt 1000, cached 600, completion 40+160=200", u)
	}
}

// --- Handler level: the response cache must never serve a request that
// runs a provider-side tool, and every logged cost must be itemised.

type handlerFixture struct {
	app    *fiber.App
	store  *logging.Store
	hits   *int32
	server *httptest.Server
}

func newHandlerFixture(t *testing.T) handlerFixture {
	t.Helper()
	var hits int32
	srv := jsonStub(t, `{"choices":[{"finish_reason":"stop","message":{"content":"hi"}}],
		"usage":{"prompt_tokens":1000,"completion_tokens":100,"prompt_tokens_details":{"cached_tokens":800}}}`)
	// jsonStub's handler has no counter, so wrap one in front of it.
	counted := stubProvider(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		resp, err := http.Post(srv.URL, "application/json", r.Body)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		io.Copy(w, resp.Body)
	})

	store, err := logging.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	cfg := &config.Config{
		Providers: map[string]config.Provider{"openai": {APIKey: "k", BaseURL: counted.URL}},
		Cache:     config.CacheConfig{Enabled: true, TTL: 60},
	}
	h := New(cfg, config.NewSettings("", cfg), cache.NewMemoryCache(), nil, store)
	app := fiber.New()
	app.Post("/v1/chat/completions", h.ChatCompletions)
	return handlerFixture{app: app, store: store, hits: &hits, server: counted}
}

func (f handlerFixture) post(t *testing.T, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.app.Test(req, 5000)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	return resp
}

func TestChatCache_ServesRepeatsButBypassesServerTools(t *testing.T) {
	f := newHandlerFixture(t)
	plain := `{"model":"gpt-4o","messages":[{"role":"user","content":"same"}]}`
	f.post(t, plain)
	second := f.post(t, plain)
	if second.Header.Get("X-Cache") != "HIT" || atomic.LoadInt32(f.hits) != 1 {
		t.Fatalf("control: an identical plain request should be a cache hit (X-Cache=%q, upstream calls=%d)",
			second.Header.Get("X-Cache"), atomic.LoadInt32(f.hits))
	}

	atomic.StoreInt32(f.hits, 0)
	withTool := `{"model":"gpt-4o","messages":[{"role":"user","content":"news?"}],"tools":[{"type":"browser_search"}]}`
	f.post(t, withTool)
	again := f.post(t, withTool)
	if again.Header.Get("X-Cache") == "HIT" {
		t.Fatal("a request running a server-side tool was served from cache: that returns stale search results as current")
	}
	if got := atomic.LoadInt32(f.hits); got != 2 {
		t.Fatalf("both tool requests must reach the provider, got %d upstream calls", got)
	}
}

func TestChatLog_ItemisesCostAndFlagsUnpricedTools(t *testing.T) {
	f := newHandlerFixture(t)
	resp := f.post(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"x"}],"tools":[{"type":"browser_search"}]}`)
	if !strings.Contains(resp.Header.Get("X-AI-Guard-Warning"), "tool fees") {
		t.Errorf("expected a tool-fee warning header, got %q", resp.Header.Get("X-AI-Guard-Warning"))
	}

	rows, _, err := f.store.ListRequests(context.Background(), time.Now().Add(-time.Hour), time.Time{}, "", "", "", "time", 10, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	r := rows[0]
	want := cost.Compute("gpt-4o", cost.TokenUsage{Prompt: 1000, Cached: 800, Completion: 100})
	if r.Kind != "chat" || r.CachedTokens != 800 || r.ToolUses != 1 {
		t.Fatalf("detail not recorded: %+v", r)
	}
	if diff := r.InputCostUSD + r.CachedCostUSD + r.CacheWriteCostUSD + r.OutputCostUSD - r.CostUSD; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("the itemised parts must sum to the logged cost: parts differ from total by %v (%+v)", diff, r)
	}
	if r.CostUSD != want.Total() || r.CachedCostUSD != want.CachedUSD {
		t.Fatalf("cost %v / cached part %v, want %v / %v", r.CostUSD, r.CachedCostUSD, want.Total(), want.CachedUSD)
	}
}
