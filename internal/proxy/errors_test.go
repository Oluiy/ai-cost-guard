package proxy

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/Oluiy/ai-cost-guard/internal/cache"
	"github.com/Oluiy/ai-cost-guard/internal/config"
	"github.com/Oluiy/ai-cost-guard/internal/logging"
)

// The log line is where a first-time deployer finds out why a provider
// rejected them, and it's also what gets pasted into issues, so it has to
// be both informative and free of anything key-shaped.
func TestUpstreamDetail(t *testing.T) {
	cases := []struct {
		name, in string
		wantHas  []string
		wantNot  []string
	}{
		{"keeps the provider's reason", `{"error":{"message":"The model requires terms acceptance","code":"model_terms_required"}}`,
			[]string{"requires terms acceptance", "model_terms_required"}, nil},
		{"masks an echoed OpenAI key", `Incorrect API key provided: sk-proj-AbCdEf1234567890xyz`,
			[]string{"Incorrect API key provided", "[redacted]"}, []string{"AbCdEf1234567890"}},
		{"masks Groq and Anthropic key shapes", `bad keys gsk_abcdefghijklmnop1234 and sk-ant-api03-abcdefghijkl`,
			[]string{"[redacted]"}, []string{"abcdefghijklmnop", "api03"}},
		{"masks any long unbroken token", `token 0123456789abcdef0123456789abcdef0123 rejected`,
			[]string{"rejected", "[redacted]"}, []string{"0123456789abcdef0123456789abcdef"}},
		{"collapses whitespace to one line", "line one\n\n   line   two", []string{"line one line two"}, nil},
	}
	for _, c := range cases {
		got := upstreamDetail([]byte(c.in))
		for _, w := range c.wantHas {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q should contain %q", c.name, got, w)
			}
		}
		for _, n := range c.wantNot {
			if strings.Contains(got, n) {
				t.Errorf("%s: %q leaked %q", c.name, got, n)
			}
		}
	}
	if upstreamDetail(nil) != "" || upstreamDetail([]byte("  \n ")) != "" {
		t.Error("an empty body should produce no detail")
	}
	if got := upstreamDetail([]byte(strings.Repeat("word ", 400))); len([]rune(got)) > 301 {
		t.Errorf("detail must be capped, got %d runes", len([]rune(got)))
	}
}

func TestClientFailureMessage(t *testing.T) {
	if got := clientFailureMessage("openai", 401, errors.New("x")); !strings.Contains(got, "openai rejected the API key") || !strings.Contains(got, "api_key in your FitGuard config") {
		t.Errorf("a 401 should say whose key to fix, got %q", got)
	}
	if got := clientFailureMessage("groq", 400, errors.New("x")); !strings.Contains(got, "groq returned HTTP 400") || !strings.Contains(got, "server log") {
		t.Errorf("other statuses should name the provider and point at the log, got %q", got)
	}
	// FitGuard's own explanatory errors (status 0) must pass through intact.
	own := errors.New("groq has no image generation API; route image requests to an openai/gemini/together model instead")
	if got := clientFailureMessage("groq", 0, own); got != own.Error() {
		t.Errorf("a FitGuard-authored error was rewritten: %q", got)
	}
}

// End to end: a provider that rejects the key must produce a 502 telling
// the caller which provider and status, without echoing the upstream body.
func TestChat_UpstreamRejectionIsExplainedWithoutLeakingTheBody(t *testing.T) {
	srv := stubProvider(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(401)
		io.WriteString(w, `{"error":{"message":"Incorrect API key provided: sk-proj-SECRETVALUE1234567890"}}`)
	})
	store, err := logging.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	cfg := &config.Config{Providers: map[string]config.Provider{"openai": {APIKey: "k", BaseURL: srv.URL}}}
	h := New(cfg, config.NewSettings("", cfg), cache.NewMemoryCache(), nil, store)
	app := fiber.New()
	app.Post("/v1/chat/completions", h.ChatCompletions)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	resp, err := app.Test(req, 5000)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 502 {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
	if !strings.Contains(string(body), "openai rejected the API key (HTTP 401)") {
		t.Errorf("the caller should be told which provider rejected which key: %s", body)
	}
	if strings.Contains(string(body), "SECRETVALUE") || strings.Contains(string(body), "Incorrect API key") {
		t.Errorf("the upstream body must stay in the server log, not the response: %s", body)
	}
}
