# AI Cost Guard

A self-hostable, OpenAI-compatible AI gateway that stops runaway LLM bills
before they happen: the kind of incident where a stuck loop or an
unbounded `max_tokens` turns into an $8K bill in 11 days.

Single Go binary. No required external dependencies (Redis is optional).
Drop it in front of OpenAI, Anthropic, Gemini, Groq, or Together by
changing one `baseURL`.

📖 **[Full documentation](./docs/index.html)**: getting started, how it
works, the complete API reference with examples in five languages, provider
setup, deployment, and troubleshooting. Open `docs/index.html` in a browser.

🏢 **[Enterprise deployments](./ENTERPRISE.md)**: budget enforcement across
instances, hardening, and what to put in front of AI Cost Guard when it runs
in a production environment.

💚 **[Sponsor this project](./GITHUB-SPONSORS-AI-COST-GUARD.md)**: if AI Cost
Guard is saving you real money on your provider bill, sponsoring keeps it
maintained. Tiers and what your sponsorship funds are in that doc, or use
the **Sponsor** button at the top of this repo.

## Why

Most teams calling LLM APIs directly have none of:

- **Caching**: identical prompts hit the API (and the bill) every time.
- **Per-user budgets**: one bad actor or bug can spend the whole month's budget in an hour.
- **Loop detection**: a `finish_reason: "length"` (truncated response) is a common sign of
  a runaway generation loop, and it's usually silently ignored.
- **Fallback routing**: a provider outage takes your app down instead of degrading to a cheaper model.
- **Visibility**: no per-request cost log to see where the money actually went.

AI Cost Guard adds all of this as a transparent proxy, in front of an
OpenAI-compatible API surface, so existing SDKs work unmodified.

## Quickstart

```bash
git clone https://github.com/Oluiy/ai-cost-guard.git
cd ai-cost-guard
go install ./cmd/ai-guard   # builds from source, installs to $GOPATH/bin

ai-guard init   # interactive: pick providers, paste real API keys, add budgeted users
ai-guard run    # starts the gateway on http://localhost:8787
```

`go install github.com/Oluiy/ai-cost-guard/cmd/ai-guard@latest` will also
work directly, no clone needed, once the repo is public. It's currently
private, which `go install` can't resolve without local `GOPRIVATE` + git
credential setup, so `git clone` (above) is the reliable path for now.

`ai-guard init` asks you to add one or more **budgeted users** and issues a
virtual API key for each: that's what your app authenticates to ai-guard
with, *not* your real OpenAI/Anthropic key (ai-guard holds those and attaches
them upstream; your app never sees them). Each key gets its own daily budget,
and, importantly, that budget can't be bypassed by an app bug or a caller
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

Anthropic, Gemini, and Groq models (`claude-*`, `gemini-*`, `llama-*`,
`mixtral-*`, ...) are routed automatically based on the `model` field. No
client changes needed beyond the `baseURL`/`apiKey`.

Open `http://localhost:8787/dashboard` for live spend, cache hit rate, and
top expensive requests. `ai-guard init` also sets up a login for it (one
admin account), skippable, but skipping leaves it reachable with no login.

If you run `ai-guard init` with no budgeted users, it falls back to
single-tenant mode: every caller shares one unauthenticated "default"
identity. That's fine for solo local use; it is **not** a substitute for
issued keys once more than one caller can reach the gateway.

## Features

| Feature | How it works |
|---|---|
| **Semantic cache** | Hashes `model` + request payload; identical requests within the TTL are served from cache with `X-Cache: HIT` and cost $0, served before any budget check since it costs nothing. |
| **Per-user budgets** | Each virtual API key maps to a `daily_limit_usd`. Before calling upstream, ai-guard estimates the request's worst-case cost (from `max_tokens`) and reserves it against the budget, so a burst of concurrent requests, or one request with a huge `max_tokens`, can't slip past a check that only looked at already-settled spend. Over budget → `429`. Reservations live in-process by default (correct for one instance); set `budget.backend: redis` to share enforcement across multiple ai-guard instances behind a load balancer. |
| **finish_reason guard** | Every response with `finish_reason: "length"` is logged and flagged with an `X-AI-Guard-Warning` header: the #1 signal of a truncation/loop bug. |
| **Auto fallback** | If the primary model's request errors or rate-limits, ai-guard retries against the next model in your configured `fallback` list. |
| **Cost logging** | Every request is logged to SQLite: user, model, tokens, cost, latency, cache hit, finish reason. |
| **Live dashboard** | `/dashboard`: spend today, cache hit rate, spend-by-hour chart, spend by user, and top expensive requests, all updating live over SSE. Protected by a login; `ai-guard reset-dashboard-password` recovers it any time. |
| **Streaming** | `stream: true` is fully supported, including caching (a cache hit still returns a real stream) and fallback (a failed provider is retried *before* anything reaches the client, not mid-stream). |
| **Embeddings** | `/v1/embeddings`: cached and budget-checked the same way as chat completions. |
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

Apache License 2.0. See [LICENSE](./LICENSE).
