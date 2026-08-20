# AI Cost Guard

A self-hostable, OpenAI-compatible AI gateway that stops runaway LLM bills
before they happen — the kind of incident where a stuck loop or an
unbounded `max_tokens` turns into an $8K bill in 11 days.

Single Go binary. No required external dependencies (Redis is optional).
Drop it in front of OpenAI, Anthropic, or Groq by changing one `baseURL`.

📖 **[Full documentation](./DOCS.md)** — configuration reference, API
reference, how budget enforcement and caching actually work, deployment
tradeoffs, and known limitations.

🏢 **[Enterprise readiness](./ENTERPRISE.md)** — an honest look at what's
solid today (budget enforcement across instances, verified live) versus
what's missing (TLS, secrets management, HA observability) before this
belongs in a production enterprise environment.

## Why

Most teams calling LLM APIs directly have none of:

- **Caching** — identical prompts hit the API (and the bill) every time.
- **Per-user budgets** — one bad actor or bug can spend the whole month's budget in an hour.
- **Loop detection** — a `finish_reason: "length"` (truncated response) is a common sign of
  a runaway generation loop, and it's usually silently ignored.
- **Fallback routing** — a provider outage takes your app down instead of degrading to a cheaper model.
- **Visibility** — no per-request cost log to see where the money actually went.

AI Cost Guard adds all of this as a transparent proxy, in front of an
OpenAI-compatible API surface, so existing SDKs work unmodified.

## Quickstart

```bash
git clone https://github.com/<your-org>/ai-cost-guard.git
cd ai-cost-guard
go install ./cmd/ai-guard   # builds from source, installs to $GOPATH/bin

ai-guard init   # interactive: pick providers, paste real API keys, add budgeted users
ai-guard run    # starts the gateway on http://localhost:8787
```

(Once you push this repo to your own GitHub org, `go install github.com/<your-org>/ai-cost-guard/cmd/ai-guard@latest`
works too — the module path in `go.mod` is a placeholder until then.)

`ai-guard init` asks you to add one or more **budgeted users** and issues a
virtual API key for each — that's what your app authenticates to ai-guard
with, *not* your real OpenAI/Anthropic key (ai-guard holds those and attaches
them upstream; your app never sees them). Each key gets its own daily budget,
and — importantly — that budget can't be bypassed by an app bug or a caller
sending a different name, because it's tied to the key, not to anything the
caller self-reports.

Then point your app at ai-guard, using the issued key in place of your real one:

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8787/v1", api_key="sk-guard-...")  # key from `ai-guard init`
client.chat.completions.create(
    model="gpt-4o",
    messages=[{"role": "user", "content": "hello"}],
)
```

```ts
import OpenAI from "openai";

const client = new OpenAI({ baseURL: "http://localhost:8787/v1", apiKey: "sk-guard-..." });
```

Anthropic and Groq models (`claude-*`, `llama-*`, `mixtral-*`, ...) are
routed automatically based on the `model` field — no client changes needed
beyond the `baseURL`/`apiKey`.

Open `http://localhost:8787/dashboard` for live spend, cache hit rate, and
top expensive requests.

If you run `ai-guard init` with no budgeted users, it falls back to
single-tenant mode — every caller shares one unauthenticated "default"
identity. That's fine for solo local use; it is **not** a substitute for
issued keys once more than one caller can reach the gateway.

## Features

| Feature | How it works |
|---|---|
| **Semantic cache** | Hashes `model` + request payload; identical requests within the TTL are served from cache with `X-Cache: HIT` and cost $0 — served before any budget check, since it costs nothing. |
| **Per-user budgets** | Each virtual API key maps to a `daily_limit_usd`. Before calling upstream, ai-guard estimates the request's worst-case cost (from `max_tokens`) and reserves it against the budget — so a burst of concurrent requests, or one request with a huge `max_tokens`, can't slip past a check that only looked at already-settled spend. Over budget → `429`. Reservations live in-process by default (correct for one instance); set `budget.backend: redis` to share enforcement across multiple ai-guard instances behind a load balancer. |
| **finish_reason guard** | Every response with `finish_reason: "length"` is logged and flagged with an `X-AI-Guard-Warning` header — the #1 signal of a truncation/loop bug. |
| **Auto fallback** | If the primary model's request errors or rate-limits, ai-guard retries against the next model in your configured `fallback` list. |
| **Cost logging** | Every request is logged to SQLite: user, model, tokens, cost, latency, cache hit, finish reason. |
| **Live dashboard** | `/dashboard` — spend today, cache hit rate, spend-by-hour chart, spend by user, and top expensive requests, all updating live over SSE. |
| **Streaming** | `stream: true` is fully supported — including caching (a cache hit still returns a real stream) and fallback (a failed provider is retried *before* anything reaches the client, not mid-stream). |
| **Embeddings** | `/v1/embeddings` — cached and budget-checked the same way as chat completions. |
| **Vision & tool calling** | Multimodal (image) content and function/tool calling are translated for Anthropic; OpenAI-compatible providers pass them through natively. |

## Configuration

`ai-guard init` writes a `config.yaml` for you interactively. See
[`config.example.yaml`](./config.example.yaml) for the full shape, including
`${ENV_VAR}` expansion for keeping API keys out of the file:

```yaml
providers:
  openai:
    api_key: ${OPENAI_API_KEY}
users:
  user_123:
    daily_limit_usd: 5
keys:
  sk-guard-abc123...: user_123   # what the caller sends; never your real provider key
fallback:
  - gpt-4o-mini
```

Run with a specific config path:

```bash
ai-guard run --config /path/to/config.yaml
```

## Docker

```bash
docker build -t ai-guard .
docker run -p 8787:8787 -v $(pwd)/config.yaml:/data/config.yaml ai-guard
```

## Known limitations (v0.1)

- **No Assistants/Responses API** — just `/v1/chat/completions` and
  `/v1/embeddings`.
- **Embeddings requests don't use `fallback:`** — a single attempt against
  the requested model's provider, deliberately (chat's fallback models
  aren't valid embeddings models).
- **A dropped upstream connection mid-stream ends the client's stream with
  no formal error event** — fallback only covers failures *before* the
  first byte is relayed to the client; once a provider's stream is
  committed, there's no way to hand it to a fallback if it dies partway
  through.
- **Anthropic tool-call argument streaming is batched, not incremental**,
  and `tool_choice: "none"` has no exact Anthropic equivalent (translated
  as a best-effort `"auto"`).
- **Budget enforcement is per-process, not per-cluster.** Reservations that
  close the concurrent-request race (see above) are held in memory, and
  SQLite is a single file — if you run more than one ai-guard instance
  (e.g. one per pod behind a load balancer), each has its own view of
  spend and its own in-flight reservations, so a single user's budget can
  be exceeded by roughly (number of instances)×. Single-instance
  deployments (a single container/VM in front of your app) enforce budgets
  correctly; horizontally-scaled deployments currently don't.
- Worst-case cost estimation (for the pre-flight budget reservation) sizes
  the prompt from message text length (~4 chars/token) and uses the
  request's `max_tokens` (or a 4096-token default if unset) — it's a
  ceiling, not the model's actual token count, so it can reserve somewhat
  more than a request ends up costing. The reservation is released and
  reconciled against actual logged cost immediately after the request.
- The pricing table in `internal/cost/cost.go` is a manually maintained
  snapshot of public provider pricing; verify against current provider
  pricing pages for anything cost-sensitive, and update the table if it
  drifts.

## Development

```bash
go build ./...
go test ./...
```

Project layout:

```
cmd/ai-guard/       CLI entrypoint (cobra: init, run)
internal/config/    YAML config loading + validation
internal/proxy/      /v1/chat/completions handler, provider routing/translation, fallback
internal/cache/      in-memory + Redis response cache
internal/budget/     per-user daily spend enforcement
internal/cost/       model price table + cost calculation
internal/logging/    SQLite request/cost log
internal/dashboard/  embedded live cost dashboard (HTML + SSE)
internal/cli/        interactive init + run commands
```

## License

MIT
