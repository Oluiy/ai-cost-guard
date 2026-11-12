package proxy

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"

	"github.com/pterm/pterm"

	"github.com/google/uuid"

	"github.com/Oluiy/ai-cost-guard/internal/budget"
)

func randomSuffix() string {
	return uuid.NewString()
}

// maxUpstreamResponseBytes caps how much of a provider response body gets
// read into memory, since base_url can point at any endpoint including a
// misconfigured or unbounded one. 32MB covers any real response.
const maxUpstreamResponseBytes = 32 * 1024 * 1024

// readUpstreamBody reads resp.Body capped at maxUpstreamResponseBytes.
func readUpstreamBody(body io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(body, maxUpstreamResponseBytes+1))
}

// budgetDenialMessage renders a budget refusal for the client. A
// fail-closed refusal carries its own Reason, since there's no real
// limit to quote.
func budgetDenialMessage(r budget.Result) string {
	if r.Reason != "" {
		return r.Reason
	}
	return fmt.Sprintf("daily budget of $%.2f exceeded or would be exceeded by this request (spent $%.2f)",
		r.LimitUSD, r.SpentUSD)
}

// secretShaped matches things that look like credentials: well-known key
// prefixes, or any long unbroken token. Upstream error bodies are
// attacker-influenced text from a third party, and are about to be written
// to a log that people paste into issues, so anything key-shaped is masked
// before it is printed. Providers already mask the keys they echo; this is
// the second line of defence, not the first.
var secretShaped = regexp.MustCompile(`(?i)\b(?:sk|gsk|tgp|xai|aiza|aq)[-_.][A-Za-z0-9_.-]{8,}|[A-Za-z0-9_-]{32,}`)

// upstreamDetail returns a short, single-line, key-free excerpt of an
// upstream error body, or "" if there's nothing useful to show. This is
// what makes "upstream returned status 400" diagnosable: the status alone
// says nothing, while the body says "model requires terms acceptance" or
// "Incorrect API key provided".
func upstreamDetail(body []byte) string {
	s := strings.Join(strings.Fields(string(body)), " ")
	if s == "" {
		return ""
	}
	s = secretShaped.ReplaceAllString(s, "[redacted]")
	const max = 300
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}

// logUpstreamFailure is the one place a failed provider call is logged, so
// every endpoint reports the upstream's own reason and not just a status.
func logUpstreamFailure(provider, model, what string, status int, err error, body []byte) {
	detail := ""
	if d := upstreamDetail(body); d != "" {
		detail = ": " + d
	}
	// err can embed the upstream body too (stream-open errors do), so it
	// gets the same masking as the body itself.
	errText := "unknown error"
	if err != nil {
		errText = upstreamDetail([]byte(err.Error()))
	}
	warnf("provider %s (model %s) %s failed: %s (status %d)%s", provider, model, what, errText, status, detail)
}

// describeFailure is what the CALLER is told about a failed provider call:
// which provider and what HTTP status, plus the one fix that applies to
// the most common first-deployment mistake. It deliberately excludes the
// upstream body, which stays in the server log.
func describeFailure(provider string, status int) string {
	switch {
	case status == 0:
		return fmt.Sprintf("%s could not be reached", provider)
	case status == 401 || status == 403:
		return fmt.Sprintf("%s rejected the API key (HTTP %d); check the %s api_key in your FitGuard config", provider, status, provider)
	default:
		return fmt.Sprintf("%s returned HTTP %d (see the FitGuard server log for the provider's message)", provider, status)
	}
}

// clientFailureMessage is the 502 body for the single-attempt endpoints
// (embeddings, images, audio). An upstream HTTP failure names the provider
// and status; anything else (e.g. "groq has no image generation API") is
// already a clear, FitGuard-authored sentence and is passed through as is.
func clientFailureMessage(provider string, status int, err error) string {
	if status >= 400 {
		return describeFailure(provider, status)
	}
	return err.Error()
}

// requestError is a problem with what the CALLER sent, found before any
// provider is contacted. Handlers answer it with a 400 and the message,
// rather than the 502 reserved for upstream failures.
type requestError struct{ msg string }

func (e *requestError) Error() string { return e.msg }

// allFailedMessage is the 502 body when every provider/fallback failed.
func allFailedMessage(lastFailure string) string {
	if lastFailure == "" {
		return "all providers/fallbacks failed"
	}
	return "all providers/fallbacks failed; last attempt: " + lastFailure
}

// warnLogMu serializes warning output from request handlers. pterm's
// printers are package-level singletons that mutate shared state per
// call, so concurrent Printfln calls are a data race without this.
var warnLogMu sync.Mutex

// warnf logs a request-path warning safely from any goroutine.
func warnf(format string, a ...any) {
	warnLogMu.Lock()
	defer warnLogMu.Unlock()
	pterm.Warning.Printfln(format, a...)
}
