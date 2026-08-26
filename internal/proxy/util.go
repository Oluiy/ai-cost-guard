package proxy

import (
	"fmt"
	"io"
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
