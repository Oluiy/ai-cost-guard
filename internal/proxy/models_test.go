package proxy

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/Oluiy/ai-cost-guard/internal/cache"
	"github.com/Oluiy/ai-cost-guard/internal/config"
)

func modelsApp(t *testing.T, keys map[string]string, providers ...string) *fiber.App {
	t.Helper()
	cfg := &config.Config{Providers: map[string]config.Provider{}, Keys: keys}
	for _, p := range providers {
		cfg.Providers[p] = config.Provider{APIKey: "k"}
	}
	h := New(cfg, config.NewSettings("", cfg), cache.NewMemoryCache(), nil, nil)
	app := fiber.New()
	app.Get("/v1/models", h.Models)
	app.Get("/v1/models/*", h.Model)
	return app
}

func get(t *testing.T, app *fiber.App, path, key string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := app.Test(req, 5000)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

type modelList struct {
	Object string        `json:"object"`
	Data   []modelObject `json:"data"`
}

// The shape the OpenAI SDKs and picker UIs depend on, and the rule that
// only models of configured providers are offered.
func TestModels_ListsOnlyConfiguredProvidersInOpenAIShape(t *testing.T) {
	app := modelsApp(t, nil, "openai", "anthropic")
	status, body := get(t, app, "/v1/models", "")
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	var list modelList
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	if list.Object != "list" || len(list.Data) == 0 {
		t.Fatalf("want object=list with data, got %s", body)
	}
	seen := map[string]modelObject{}
	for i, m := range list.Data {
		if m.Object != "model" || m.OwnedBy == "" {
			t.Errorf("entry %+v is not a complete model object", m)
		}
		if i > 0 && list.Data[i-1].ID >= m.ID {
			t.Errorf("list must be sorted by id, %q came before %q", list.Data[i-1].ID, m.ID)
		}
		seen[m.ID] = m
	}
	for _, want := range []string{"gpt-4o", "claude-sonnet-5", "text-embedding-3-small", "gpt-image-1", "whisper-1"} {
		if _, ok := seen[want]; !ok {
			t.Errorf("%s should be listed: every kind of model of a configured provider belongs in the picker", want)
		}
	}
	for _, unwanted := range []string{"gemini-2.5-flash", "llama-3.3-70b-versatile"} {
		if _, ok := seen[unwanted]; ok {
			t.Errorf("%s is listed but its provider has no key here, so it could only fail", unwanted)
		}
	}
	if seen["gpt-4o"].OwnedBy != "openai" || seen["claude-sonnet-5"].OwnedBy != "anthropic" {
		t.Errorf("owned_by should name the provider, got %q / %q", seen["gpt-4o"].OwnedBy, seen["claude-sonnet-5"].OwnedBy)
	}
}

func TestModels_RetrieveHandlesSlashedIDsAndMissingModels(t *testing.T) {
	app := modelsApp(t, nil, "together")
	status, body := get(t, app, "/v1/models/meta-llama/Llama-3.3-70B-Instruct-Turbo", "")
	var m modelObject
	if status != 200 || json.Unmarshal(body, &m) != nil || m.ID != "meta-llama/Llama-3.3-70B-Instruct-Turbo" || m.OwnedBy != "together" {
		t.Fatalf("a slashed id must resolve: %d %s", status, body)
	}

	status, body = get(t, app, "/v1/models/gpt-4o", "") // openai isn't configured
	var e struct {
		Error struct{ Message, Type string } `json:"error"`
	}
	if status != 404 || json.Unmarshal(body, &e) != nil || e.Error.Type != "invalid_request_error" || e.Error.Message == "" {
		t.Fatalf("an unavailable model should be an OpenAI-style 404, got %d %s", status, body)
	}
}

func TestModels_RequireAKeyWhenKeysAreConfigured(t *testing.T) {
	app := modelsApp(t, map[string]string{"sk-guard-real": "alice"}, "openai")
	for _, path := range []string{"/v1/models", "/v1/models/gpt-4o"} {
		if status, _ := get(t, app, path, ""); status != 401 {
			t.Errorf("%s without a key: status %d, want 401", path, status)
		}
		if status, _ := get(t, app, path, "sk-guard-wrong"); status != 401 {
			t.Errorf("%s with a wrong key: status %d, want 401", path, status)
		}
	}
	if status, _ := get(t, app, "/v1/models", "sk-guard-real"); status != 200 {
		t.Errorf("a valid key should list models, got %d", status)
	}
}
