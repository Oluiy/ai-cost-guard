# FitGuard

A self-hostable, OpenAI-compatible AI gateway that stops runaway LLM bills
before they happen: the kind of incident where a stuck loop or an
unbounded `max_tokens` turns into an $8K bill in 11 days.

Single Go binary. No required external dependencies (Redis is optional).
Drop it in front of OpenAI, Anthropic, Gemini, Groq, or Together by
changing one `baseURL`.

📖 **[Full documentation](https://ai-cost-guard-ruddy.vercel.app/)**: getting
started, how it works, the complete API reference with examples in five
languages, provider setup, deployment, and troubleshooting.

🏢 **[Enterprise deployments](./ENTERPRISE.md)**: budget enforcement across
instances, hardening, and what to put in front of FitGuard when it runs
in a production environment.

💚 **[Sponsor this project](./GITHUB-SPONSORS-AI-COST-GUARD.md)**: if FitGuard
is saving you real money on your provider bill, sponsoring keeps it
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

FitGuard adds all of this as a transparent proxy, in front of an
OpenAI-compatible API surface, so existing SDKs work unmodified.

## Install

**Install script** (macOS/Linux, downloads a prebuilt binary and verifies
its checksum):

```bash
curl -fsSL https://raw.githubusercontent.com/Oluiy/ai-cost-guard/main/install.sh | sh
```

**npm**:

```bash
npm install -g fitguard
```

**Go** (any platform with a Go toolchain):

```bash
go install github.com/Oluiy/ai-cost-guard/cmd/fitguard@latest
```

Prebuilt binaries for Linux, macOS, and Windows are also on the
[releases page](https://github.com/Oluiy/ai-cost-guard/releases).

## Quickstart

```bash
fitguard init   # interactive: pick providers, paste real API keys, add budgeted users
fitguard run    # starts the gateway on http://localhost:8787
```

`fitguard init` asks you to add one or more **budgeted users** and issues a
virtual API key for each: that's what your app authenticates to fitguard
with, *not* your real OpenAI/Anthropic key (fitguard holds those and attaches
them upstream; your app never sees them). Each key gets its own daily budget,
and, importantly, that budget can't be bypassed by an app bug or a caller
sending a different name, because it's tied to the key, not to anything the
caller self-reports.

Then point your app at fitguard, using the issued key in place of your real one:

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8787/v1", api_key="sk-guard-...")  # key from `fitguard init`
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

Open `http://localhost:8787/dashboard` for live spend, cache hit rate, top
expensive requests, and a **Settings** page for changing cache/budget/fallback
behavior without a restart. `fitguard init` sets up a login for it
(skippable, but skipping leaves it reachable with no login).

Running `fitguard init` with no budgeted users falls back to single-tenant
mode — fine for solo local use, not once more than one caller can reach the
gateway. Both are covered in [**Getting started**](https://ai-cost-guard-ruddy.vercel.app/guide/getting-started.html).

## Features

| Feature | |
|---|---|
| **Exact-match cache** | Identical requests within the TTL cost $0. Not semantic — a one-character rewording is a miss. |
| **Per-user budgets** | Worst-case cost is reserved *before* calling upstream, so concurrent requests can't jointly overspend. Over budget → `429`. |
| **finish_reason guard** | Flags truncated responses (`finish_reason: "length"`) — the #1 signal of a runaway loop. |
| **Auto fallback** | Errors or rate-limits retry against your configured `fallback` list, before anything reaches the client. |
| **Cost logging** | Every request logged to SQLite: user, model, tokens, cost, latency, cache hit. No prompt content stored. |
| **Live dashboard** | Spend, cache hit rate, top expensive requests, updating live over SSE. Login-protected. |
| **Streaming** | `stream: true` fully supported, including through caching and fallback. |
| **Embeddings** | `/v1/embeddings`, cached and budget-checked the same way as chat completions. |
| **Vision & tool calling** | Translated for Anthropic; OpenAI-compatible providers pass them through natively. |

How each of these actually works, mechanism by mechanism, is in
[**How it works**](https://ai-cost-guard-ruddy.vercel.app/guide/how-it-works.html).

## Configuration

`fitguard init` writes a `config.yaml` for you interactively. See
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
fitguard run --config /path/to/config.yaml
```

Every field, including the two security-relevant ones worth knowing before
you deploy beyond your own machine (`budget.fail_closed`,
`trusted_proxies`), is documented in the
[**Configuration reference**](https://ai-cost-guard-ruddy.vercel.app/guide/configuration.html).

## Docker

```bash
docker build -t fitguard .
docker run -p 8787:8787 -v $(pwd)/config.yaml:/data/config.yaml fitguard
```

## Development

```bash
go build ./...
go test ./...
```

Package layout and the config concurrency model are in
[**ARCHITECTURE.md**](./ARCHITECTURE.md).

## License

Apache License 2.0. See [LICENSE](./LICENSE).
