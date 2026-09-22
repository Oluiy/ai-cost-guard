package proxy

import (
	"context"
	"strings"
	"testing"
)

// TestTranslateTogetherImageRequest covers the two confirmed Together
// parameter mismatches: response_format's enum is "base64"/"url" (not
// OpenAI's "b64_json"), and dimensions are separate numeric width/height
// (not OpenAI's single "1024x1024" size string). Both verified against
// Together's own API reference.
func TestTranslateTogetherImageRequest(t *testing.T) {
	got, err := translateTogetherImageRequest([]byte(`{"model":"flux","prompt":"x","response_format":"b64_json","size":"1024x768"}`))
	if err != nil {
		t.Fatalf("translateTogetherImageRequest: %v", err)
	}
	for _, want := range []string{`"response_format":"base64"`, `"width":1024`, `"height":768`} {
		if !strings.Contains(string(got), want) {
			t.Errorf("got %s, want it to contain %s", got, want)
		}
	}
	if strings.Contains(string(got), `"size"`) {
		t.Errorf("got %s, want the OpenAI-only \"size\" key removed", got)
	}

	// "url" and an absent size are both left alone.
	got, err = translateTogetherImageRequest([]byte(`{"model":"flux","prompt":"x","response_format":"url"}`))
	if err != nil {
		t.Fatalf("translateTogetherImageRequest: %v", err)
	}
	if !strings.Contains(string(got), `"response_format":"url"`) {
		t.Errorf("got %s, want response_format left as \"url\"", got)
	}

	// A malformed size is a loud error, not a silently dropped parameter.
	if _, err := translateTogetherImageRequest([]byte(`{"model":"flux","prompt":"x","size":"big"}`)); err == nil {
		t.Error("expected an error for a malformed size string")
	}
}

// TestTogetherUpstreamModel is the fix for a real bug: the FitGuard-facing
// alias "together/whisper-large-v3" (chosen so RouteProvider doesn't
// misread an "openai/" prefix as "route to openai") was never actually
// translated back to Together's real catalog id before going upstream.
func TestTogetherUpstreamModel(t *testing.T) {
	if got := togetherUpstreamModel("together", "together/whisper-large-v3"); got != "openai/whisper-large-v3" {
		t.Errorf(`togetherUpstreamModel("together", "together/whisper-large-v3") = %q, want "openai/whisper-large-v3"`, got)
	}
	// Every other case passes through unchanged.
	if got := togetherUpstreamModel("openai", "whisper-1"); got != "whisper-1" {
		t.Errorf("expected non-together kind to pass through unchanged, got %q", got)
	}
	if got := togetherUpstreamModel("together", "some-other-model"); got != "some-other-model" {
		t.Errorf("expected an unrelated together model to pass through unchanged, got %q", got)
	}
}

func TestRequestedImageCount(t *testing.T) {
	cases := map[string]int{
		`{"model":"gpt-image-1","prompt":"a cat"}`:   1, // "n" unset -> default 1
		`{"model":"gpt-image-1","prompt":"x","n":3}`: 3,
		`{"model":"gpt-image-1","prompt":"x","n":0}`: 1, // zero/invalid -> default 1
		`not valid json`: 1, // unparsable -> default 1
	}
	for body, want := range cases {
		if got := requestedImageCount([]byte(body)); got != want {
			t.Errorf("requestedImageCount(%s) = %d, want %d", body, got, want)
		}
	}
}

// TestOpenAICompatProvider_GroqExcludesImages: Groq has no image
// generation API, so this must error rather than forward to an endpoint
// Groq doesn't serve. (Speech used to be excluded here too; see
// GROQ-TTS-PREVIEW. If you remove that, restore the AudioSpeech check.)
func TestOpenAICompatProvider_GroqExcludesImages(t *testing.T) {
	p := NewOpenAICompatProvider("https://api.groq.com/openai/v1", "test-key", "groq")
	if _, _, _, err := p.ImageGeneration(context.Background(), "some-model", []byte(`{}`)); err == nil {
		t.Error("expected ImageGeneration to error for groq")
	}
}

// TestOpenAICompatProvider_GroqTranscriptionNotExcluded confirms
// transcription (unlike images/speech) is NOT gated by Kind — Groq's
// Whisper models are GA, so AudioTranscription must build a request
// rather than short-circuit to a "not supported" error the way
// ImageGeneration/AudioSpeech do above.
func TestOpenAICompatProvider_GroqTranscriptionNotExcluded(t *testing.T) {
	p := NewOpenAICompatProvider("http://127.0.0.1:0", "test-key", "groq") // unreachable on purpose
	ctx := context.Background()

	_, _, _, err := p.AudioTranscription(ctx, "whisper-large-v3-turbo", strings.NewReader("fake audio"), "a.mp3", nil)
	if err == nil {
		t.Fatal("expected a connection error against the unreachable base URL")
	}
	if strings.Contains(err.Error(), "not supported") || strings.Contains(err.Error(), "preview") {
		t.Errorf("transcription should not be gated by Kind like images/speech are, got: %v", err)
	}
}
