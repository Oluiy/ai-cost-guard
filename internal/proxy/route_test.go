package proxy

import "testing"

func TestRouteProvider(t *testing.T) {
	cases := map[string]string{
		"gpt-4o":                         "openai",
		"gpt-4o-mini":                    "openai",
		"o1-mini":                        "openai",
		"o3-mini":                        "openai",
		"text-embedding-3-small":         "openai", // no recognized prefix -> default
		"claude-5-sonnet":                "anthropic",
		"claude-3-haiku":                 "anthropic",
		"llama-3.1-8b-instant":           "groq",
		"mixtral-8x7b-32768":             "groq",
		"gemma2-9b-it":                   "groq",
		"deepseek-r1":                    "groq",
		"some-unrecognized-model":        "openai", // default fallback
		"groq/my-custom-model":           "groq",
		"anthropic/some-model":           "anthropic",
		"openai/some-model":              "openai",
		"together/some-model":            "together",
		"meta-llama/Llama-3-70b-chat-hf": "together", // unrecognized prefix before "/" -> together
	}
	for model, want := range cases {
		if got := RouteProvider(model); got != want {
			t.Errorf("RouteProvider(%q) = %q, want %q", model, got, want)
		}
	}
}
