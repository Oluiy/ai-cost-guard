package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Wire-contract tests for the image/audio paths that require real
// translation (Gemini's Interactions API, Together's differently-spelled
// image parameters). A stub server stands in for the provider and asserts
// the outbound request matches what that provider's own docs specify,
// then replies with a documented-shape response so the parsing side is
// exercised too.
//
// This is deliberately not a substitute for one real call against a live
// key — a stub can only prove the code matches the documented contract,
// not that the docs match the provider's actual behavior. What it does
// buy is that the contract can't silently drift later: a regression in
// the translation fails here instead of at a customer's first request.

func stubProvider(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// TestGeminiImageGeneration_WireContract pins the request/response shape
// documented at ai.google.dev/api/interactions-api (post-May-2026 schema).
func TestGeminiImageGeneration_WireContract(t *testing.T) {
	var gotPath, gotRevision string
	var gotBody map[string]any

	srv := stubProvider(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRevision = r.Header.Get("Api-Revision")
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"steps":[
			{"type":"user_input","content":[{"type":"text","text":"a cat"}]},
			{"type":"model_output","content":[{"type":"image","data":"aW1hZ2UtYnl0ZXM=","mime_type":"image/png"}]}
		]}`)
	})

	p := NewGeminiProvider(srv.URL, "test-key")
	out, n, status, err := p.ImageGeneration(context.Background(), "gemini-3.1-flash-image",
		[]byte(`{"model":"gemini-3.1-flash-image","prompt":"a cat"}`))
	if err != nil {
		t.Fatalf("ImageGeneration: %v", err)
	}
	if status != http.StatusOK || n != 1 {
		t.Fatalf("got status=%d n=%d, want 200/1", status, n)
	}
	if gotPath != "/interactions" {
		t.Errorf("posted to %q, want /interactions", gotPath)
	}
	if gotRevision != geminiAPIRevision {
		t.Errorf("Api-Revision = %q, want %q (required to opt into the new schema)", gotRevision, geminiAPIRevision)
	}
	if gotBody["input"] != "a cat" {
		t.Errorf(`input = %v, want the prompt as a plain string`, gotBody["input"])
	}
	rf, _ := gotBody["response_format"].(map[string]any)
	if rf == nil || rf["type"] != "image" {
		t.Errorf(`response_format = %v, want {"type":"image"}`, gotBody["response_format"])
	}

	// Response side: the base64 payload must be lifted out of
	// steps[].content[] and re-shaped into OpenAI's images response.
	var openaiShaped struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &openaiShaped); err != nil {
		t.Fatalf("unmarshaling our own response: %v", err)
	}
	if len(openaiShaped.Data) != 1 || openaiShaped.Data[0].B64JSON != "aW1hZ2UtYnl0ZXM=" {
		t.Errorf("got %s, want the image data lifted out of the steps array", out)
	}
}

// TestGeminiAudioSpeech_WireContract pins the speech request shape
// (response_format + generation_config.speech_config) and confirms the
// returned base64 audio is decoded to raw bytes for the client.
func TestGeminiAudioSpeech_WireContract(t *testing.T) {
	var gotBody map[string]any

	srv := stubProvider(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"steps":[{"type":"model_output","content":[
			{"type":"audio","data":"`+base64.StdEncoding.EncodeToString([]byte("audio-bytes"))+`","mime_type":"audio/wav"}
		]}]}`)
	})

	p := NewGeminiProvider(srv.URL, "test-key")
	out, chars, status, err := p.AudioSpeech(context.Background(), "gemini-3.1-flash-tts-preview",
		[]byte(`{"model":"gemini-3.1-flash-tts-preview","input":"hello","voice":"Puck"}`))
	if err != nil {
		t.Fatalf("AudioSpeech: %v", err)
	}
	if status != http.StatusOK || chars != len("hello") {
		t.Fatalf("got status=%d chars=%d, want 200/5", status, chars)
	}
	rf, _ := gotBody["response_format"].(map[string]any)
	if rf == nil || rf["type"] != "audio" {
		t.Errorf(`response_format = %v, want {"type":"audio"}`, gotBody["response_format"])
	}
	genCfg, _ := gotBody["generation_config"].(map[string]any)
	voices, _ := genCfg["speech_config"].([]any)
	if len(voices) != 1 {
		t.Fatalf("generation_config.speech_config = %v, want one voice entry", genCfg["speech_config"])
	}
	if v, _ := voices[0].(map[string]any); v["voice"] != "Puck" {
		t.Errorf("voice = %v, want the caller's requested voice", voices[0])
	}
	if string(out) != "audio-bytes" {
		t.Errorf("got %q, want the base64 decoded to raw audio bytes", out)
	}
}

// TestTogetherImageGeneration_WireContract pins the two parameters
// Together spells differently from OpenAI, verified against Together's
// own API reference: response_format "base64" (not "b64_json") and
// numeric width/height (not a single "1024x768" size string).
func TestTogetherImageGeneration_WireContract(t *testing.T) {
	var gotPath string
	var gotBody map[string]any

	srv := stubProvider(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"img-1","model":"black-forest-labs/FLUX.1-schnell","object":"list",
			"data":[{"index":0,"b64_json":"aW1n","type":"b64_json"}]}`)
	})

	p := NewOpenAICompatProvider(srv.URL, "test-key", "together")
	_, n, status, err := p.ImageGeneration(context.Background(), "black-forest-labs/FLUX.1-schnell",
		[]byte(`{"model":"flux","prompt":"a cat","size":"1024x768","response_format":"b64_json","n":2}`))
	if err != nil {
		t.Fatalf("ImageGeneration: %v", err)
	}
	if status != http.StatusOK || n != 2 {
		t.Fatalf("got status=%d n=%d, want 200/2", status, n)
	}
	if gotPath != "/images/generations" {
		t.Errorf("posted to %q, want /images/generations", gotPath)
	}
	if gotBody["response_format"] != "base64" {
		t.Errorf(`response_format = %v, want "base64" (Together's spelling)`, gotBody["response_format"])
	}
	if gotBody["width"] != float64(1024) || gotBody["height"] != float64(768) {
		t.Errorf("width/height = %v/%v, want 1024/768 split out of size", gotBody["width"], gotBody["height"])
	}
	if _, stillThere := gotBody["size"]; stillThere {
		t.Error(`the OpenAI-only "size" key was forwarded to Together, which ignores it`)
	}
	if gotBody["model"] != "black-forest-labs/FLUX.1-schnell" {
		t.Errorf("model = %v, want the resolved model, not the caller's alias", gotBody["model"])
	}
}

// TestTogetherAudioTranscription_WireContract confirms the multipart
// upload carries Together's real catalog id, not FitGuard's routing-safe
// alias — the alias would 404 upstream.
func TestTogetherAudioTranscription_WireContract(t *testing.T) {
	var gotPath, gotModel, gotFilename string

	srv := stubProvider(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			switch part.FormName() {
			case "model":
				b, _ := io.ReadAll(part)
				gotModel = string(b)
			case "file":
				gotFilename = part.FileName()
			}
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"text":"hello world"}`)
	})

	p := NewOpenAICompatProvider(srv.URL, "test-key", "together")
	out, _, status, err := p.AudioTranscription(context.Background(), "together/whisper-large-v3",
		strings.NewReader("fake-audio"), "clip.mp3", map[string]string{})
	if err != nil {
		t.Fatalf("AudioTranscription: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("got status %d, want 200", status)
	}
	if gotPath != "/audio/transcriptions" {
		t.Errorf("posted to %q, want /audio/transcriptions", gotPath)
	}
	if gotModel != "openai/whisper-large-v3" {
		t.Errorf("model = %q, want Together's real catalog id", gotModel)
	}
	if gotFilename != "clip.mp3" {
		t.Errorf("filename = %q, want the caller's filename preserved", gotFilename)
	}
	if !strings.Contains(string(out), "hello world") {
		t.Errorf("got %s, want the provider's response relayed through", out)
	}
}

// GROQ-TTS-PREVIEW: routing + wire contract for Groq's Orpheus TTS.
// Delete this whole test if you remove GROQ-TTS-PREVIEW.
func TestGroqAudioSpeech_WireContract(t *testing.T) {
	if got := RouteProvider("canopylabs/orpheus-v1-english"); got != "groq" {
		t.Fatalf(`RouteProvider("canopylabs/orpheus-v1-english") = %q, want "groq"`, got)
	}

	var gotPath string
	var gotBody map[string]any
	srv := stubProvider(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "audio/wav")
		io.WriteString(w, "RIFF-fake-wav")
	})

	p := NewOpenAICompatProvider(srv.URL, "test-key", "groq")
	out, chars, status, err := p.AudioSpeech(context.Background(), "canopylabs/orpheus-v1-english",
		[]byte(`{"model":"canopylabs/orpheus-v1-english","input":"hello there","voice":"troy"}`))
	if err != nil {
		t.Fatalf("AudioSpeech: %v", err)
	}
	if status != http.StatusOK || chars != len("hello there") {
		t.Fatalf("got status=%d chars=%d, want 200/%d", status, chars, len("hello there"))
	}
	if gotPath != "/audio/speech" {
		t.Errorf("posted to %q, want /audio/speech", gotPath)
	}
	if gotBody["model"] != "canopylabs/orpheus-v1-english" || gotBody["voice"] != "troy" || gotBody["input"] != "hello there" {
		t.Errorf("got body %v, want model/voice/input forwarded unchanged", gotBody)
	}
	if string(out) != "RIFF-fake-wav" {
		t.Errorf("got %q, want the audio bytes relayed unchanged", out)
	}
}
