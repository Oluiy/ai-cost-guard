package proxy

import "testing"

func TestServerToolsRequested(t *testing.T) {
	cases := []struct {
		name   string
		model  string
		parsed map[string]any
		want   bool
	}{
		{"plain chat", "gpt-4o", map[string]any{"messages": []any{}}, false},
		{"function tool is the caller's own", "gpt-4o", map[string]any{"tools": []any{map[string]any{"type": "function"}}}, false},
		{"web_search_options", "gpt-4o", map[string]any{"web_search_options": map[string]any{}}, true},
		{"search-preview model", "gpt-4o-search-preview", map[string]any{}, true},
		{"groq compound system", "groq/compound", map[string]any{}, true},
		{"groq browser_search tool", "openai/gpt-oss-120b", map[string]any{"tools": []any{map[string]any{"type": "browser_search"}}}, true},
		{"mixed function + code_interpreter", "gpt-4o", map[string]any{"tools": []any{
			map[string]any{"type": "function"}, map[string]any{"type": "code_interpreter"}}}, true},
		{"untyped tool entry is ignored", "gpt-4o", map[string]any{"tools": []any{map[string]any{}}}, false},
	}
	for _, c := range cases {
		if got := serverToolsRequested(c.model, c.parsed); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestServerToolUses(t *testing.T) {
	if got := serverToolUses("groq", false, 3); got != 3 {
		t.Errorf("provider-reported count must win, got %d", got)
	}
	if got := serverToolUses("openai", true, 0); got != 1 {
		t.Errorf("requested on a forwarding provider counts as 1, got %d", got)
	}
	for _, p := range []string{"anthropic", "gemini"} {
		if got := serverToolUses(p, true, 0); got != 0 {
			t.Errorf("%s drops server tools, so there is no fee to flag; got %d", p, got)
		}
	}
	if got := serverToolUses("openai", false, 0); got != 0 {
		t.Errorf("nothing requested, got %d", got)
	}
}
