# AI Cost Guard: Documentation

This is the full reference. For a 30-second pitch and quickstart, see
[README.md](./README.md). This document covers configuration, the request
lifecycle, the API surface, deployment tradeoffs, and what's not built yet.

## Contents

- [How it fits together](#how-it-fits-together)
- [Installation](#installation)
- [Configuration reference](#configuration-reference)
- [Authentication (virtual keys)](#authentication-virtual-keys)
- [The request lifecycle](#the-request-lifecycle)
- [API reference](#api-reference)
- [Streaming](#streaming)
- [Model routing](#model-routing)
- [Budget enforcement, in detail](#budget-enforcement-in-detail)
- [Caching, in detail](#caching-in-detail)
- [Fallback routing](#fallback-routing)
- [Cost pricing table](#cost-pricing-table)
- [Dashboard](#dashboard)
- [CLI reference](#cli-reference)
- [Deployment](#deployment)
- [Development](#development)
- [Known limitations](#known-limitations)
- [Troubleshooting](#troubleshooting)

## How it fits together

```
Your app (OpenAI SDK)
      │  POST /v1/chat/completions
      │  Authorization: Bearer sk-guard-...
      ▼
┌─────────────────────────────────────────────────────────┐
│ ai-guard                                                 │
│                                                           │
│  1. authenticate  → resolve virtual key to a user_id     │
│  2. cache lookup  → serve from cache if hit ($0, no gate)│
│  3. budget reserve→ estimate worst-case cost, reserve it │
│  4. call upstream → try model, then configured fallbacks │
│  5. finish_reason guard → warn on truncated responses    │
│  6. log + release → persist cost, release the reservation│
│                                                           │
└──────────────┬───────────────────────┬──────────────────┘
               │                       │
     provider APIs               SQLite (aiguard.db)
   (OpenAI/Anthropic/                  │
    Groq/Together)              /dashboard (live UI)
```

ai-guard is a single Go binary that terminates OpenAI-compatible chat
requests, applies cost controls, and forwards to the real provider. Your
application only ever talks to ai-guard. It never sees your real provider
API keys.

## Installation

```bash
git clone https://github.com/Oluiy/ai-cost-guard.git
cd ai-cost-guard
go install ./cmd/ai-guard
```

This builds from source and installs the `ai-guard` binary to
`$(go env GOPATH)/bin` (make sure that's on your `PATH`). Requires Go 1.22+.

`go install github.com/Oluiy/ai-cost-guard/cmd/ai-guard@latest` will work
directly, no clone needed, once the repo is made public. It's currently
private, and `go install` can't resolve a private module without local
`GOPRIVATE`/git credential setup. `git clone` (above) works regardless.

Prebuilt cross-platform binaries are produced by
[`.github/workflows/release.yml`](.github/workflows/release.yml) once a
`v*` tag is pushed. None has been tagged yet, so for now, `git clone` +
`go install` or [Docker](#deployment) are the ways to get a binary.

## Configuration reference

`ai-guard init` writes `config.yaml` interactively. Full schema:

```yaml
port: 8787              # HTTP port ai-guard listens on
data_dir: .              # where aiguard.db (SQLite log) is written

providers:
  openai:
    api_key: ${OPENAI_API_KEY}       # ${ENV_VAR} is expanded at load time
    base_url: https://api.openai.com/v1   # optional, shown are the defaults
  anthropic:
    api_key: ${ANTHROPIC_API_KEY}
    base_url: https://api.anthropic.com/v1
  groq:
    api_key: ${GROQ_API_KEY}
    base_url: https://api.groq.com/openai/v1
  together:
    api_key: ${TOGETHER_API_KEY}
    base_url: https://api.together.xyz/v1

cache:
  enabled: true
  ttl_seconds: 300
  backend: memory                    # "memory" or "redis"
  redis_url: redis://localhost:6379/0   # only used when backend: redis

users:
  user_123:
    daily_limit_usd: 5      # 0 or omitted = unlimited

keys:
  sk-guard-...: user_123    # Authorization: Bearer value -> user_id

fallback:                   # tried in order if the primary model fails/429s/5xxs
  - gpt-4o-mini
  - claude-3-haiku

dashboard:                   # written by `ai-guard init` or `reset-dashboard-password`
  session_secret: ...         # generated; signs session cookies, don't hand-edit
  users:
    - username: admin
      password_hash: $2a$10$...   # bcrypt; never edit by hand, use reset-dashboard-password
```

| Field | Type | Default | Notes |
|---|---|---|---|
| `port` | int | `8787` | |
| `data_dir` | string | `.` | Directory for `aiguard.db` |
| `providers.<name>.api_key` | string | — | Required per configured provider. Supports `${ENV_VAR}` |
| `providers.<name>.base_url` | string | provider-specific | Override for self-hosted/proxied endpoints |
| `cache.enabled` | bool | `false` | |
| `cache.ttl_seconds` | int | `300` | |
| `cache.backend` | `memory`\|`redis` | `memory` | |
| `cache.redis_url` | string | — | Required if `backend: redis` |
| `budget.backend` | `local`\|`redis` | `local` | See [Budget enforcement](#budget-enforcement-in-detail) |
| `budget.redis_url` | string | — | Falls back to `cache.redis_url` if that's also `redis` and this is unset |
| `users.<id>.daily_limit_usd` | float | — | Omit the user entirely for unlimited |
| `keys.<token>` | string (user_id) | — | See [Authentication](#authentication-virtual-keys) |
| `fallback` | []string | `[]` | Model names, tried in order |
| `dashboard.session_secret` | string | — | Generated by `init`/`reset-dashboard-password`; signs session cookies |
| `dashboard.users[].username` | string | — | Dashboard login username (currently: one account) |
| `dashboard.users[].password_hash` | string | — | bcrypt hash; set via `reset-dashboard-password`, never by hand |

Provider config keys that aren't `openai`, `anthropic`, `gemini`, `groq`, or
`together` are still accepted and treated as OpenAI-compatible endpoints
(useful for self-hosted/other OpenAI-shaped APIs), but note [model
routing](#model-routing) only dispatches to those four names by default.

**Validation vs. warnings.** `ai-guard run` refuses to start (hard error)
for configs that are unsafe or nonsensical to run at all: no providers, a
port outside 1–65535, or `cache.backend: redis` with no `redis_url`. Other
problems don't block startup but print as warnings, because they're
footguns rather than fatal, most notably a `keys:` entry whose user_id
has no matching `users:` entry, which silently means *unlimited* budget for
that key (there's nothing to cap it against); and no `dashboard.users`
entries, which means `/dashboard` has no login at all. If you hand-edit
`config.yaml` after `ai-guard init` wrote it, check the startup warnings.

## Authentication (virtual keys)

Your application authenticates to ai-guard with a **virtual key**, not your
real provider key:

```
Authorization: Bearer sk-guard-a1b2c3...
```

`ai-guard init` generates one of these per budgeted user and writes the
mapping to `config.yaml`'s `keys:` section. ai-guard resolves the key to a
`user_id` server-side, enforces that user's budget, and attaches the *real*
provider credentials (from `providers:`) only on the upstream leg. Your
client code and logs never contain them.

This exists because a client-supplied identifier (an `X-User-Id` header, or
the OpenAI `user` field) can't be trusted for budget enforcement: any bug or
inconsistency in the caller (not even malice, just an app that doesn't set
the header on every code path) lets it evade its budget entirely, or pool
against the wrong one. A per-key budget can't be evaded by a caller
choosing what to send; the caller doesn't get to choose what the key means.

**Single-tenant mode.** If `keys:` is empty, every request is authenticated
automatically as `"default"` with no bearer token required. This is a
deliberate fallback for local/solo use where there's exactly one caller (you)
and per-caller attribution is pointless. It is not a safe default once more
than one caller (another service, another team, an untrusted client) can
reach the gateway.

This is entirely separate from the [dashboard login](#dashboard): virtual
keys authenticate *API callers* (your app, hitting `/v1/...`); the
dashboard login authenticates the *human* looking at `/dashboard` in a
browser. Neither one implies or affects the other.

## The request lifecycle

Every call to `POST /v1/chat/completions` goes through, in order:

1. **Authenticate.** Resolve the bearer token to a `user_id` (or `default`
   in single-tenant mode). Missing/invalid token → `401`.
2. **Validate.** `model` must be present; `stream: true` is rejected (see
   [limitations](#known-limitations)).
3. **Cache lookup.** The request is hashed (model + full payload, minus
   `user`/`stream`) and checked against the cache. A hit is served
   immediately, logged at `$0` cost, and skips budget entirely: a cache
   hit can't overspend, so there's nothing to gate.
4. **Budget reservation.** On a cache miss, ai-guard estimates the worst
   case this request could cost (see [below](#budget-enforcement-in-detail))
   and reserves that amount against the user's daily budget. If the
   projected total (already-spent + other in-flight reservations + this
   estimate) would exceed the limit, the request is rejected with `429`
   before any upstream call is made.
5. **Upstream call, with fallback.** The primary model is tried first; on
   error, `5xx`, or `429` from the provider, ai-guard tries each model in
   `fallback:` in order.
6. **finish_reason guard.** If the response's `finish_reason` is `"length"`
   (the completion was cut off), ai-guard sets `X-AI-Guard-Warning` and logs
   a warning: this is the strongest signal of a truncation/runaway-loop bug
   in the calling application.
7. **Log + release.** The actual cost (from real token usage) is written to
   SQLite, the cache is populated, and the budget reservation from step 4 is
   released. The next request's budget check will see the real persisted
   cost instead.

## API reference

### `POST /v1/chat/completions`

OpenAI-compatible, including `stream: true` (see [Streaming](#streaming)),
multimodal (image) content, and tool/function calling. Body is passed
through mostly as-is to the resolved provider (with translation for
Anthropic, see below). Fields ai-guard itself reads: `model` (required),
`messages`, `max_tokens`, `stream`, `temperature`.

**Response headers (non-streaming):**

| Header | Meaning |
|---|---|
| `X-Cache` | `HIT` or `MISS` |
| `X-AI-Guard-Model-Used` | The model that actually served the request (may differ from the requested one if fallback triggered) |
| `X-AI-Guard-Warning` | Present and set to a truncation notice when `finish_reason: "length"` |

Streaming responses set `X-Cache` and `X-AI-Guard-Model-Used` too, but
**not** `X-AI-Guard-Warning`: headers can't be added once the stream has
started, so a truncated streamed answer is only ever flagged server-side
(a log line), not to the client. See [Streaming](#streaming).

**Error responses** (OpenAI-shaped `{"error": {"message", "type"}}`):

| Status | `type` | Cause |
|---|---|---|
| 400 | `invalid_request_error` | Malformed JSON, or missing `model` |
| 401 | `invalid_api_key` | Missing/unrecognized bearer token (multi-tenant mode only) |
| 429 | `budget_exceeded` | This request's worst-case cost would exceed the user's remaining daily budget |
| 502 | `upstream_error` | The primary model and every configured fallback failed |

For streaming requests, all of the above happen, and are returned with
their normal status codes, *before* the stream is committed to the
client (see [Streaming](#streaming) for what happens if a provider fails
after that point).

### `POST /v1/embeddings`

OpenAI-compatible. Routed, cached, and budget-checked the same way as chat
completions, with two differences: there's no `fallback:` (a single
attempt against the requested model's provider, since chat's fallback
models aren't valid embeddings models), and Anthropic always returns a
`502` with a clear message (`anthropic has no embeddings API...`) rather
than silently no-op'ing, since it has no embeddings API to translate to.
Errors return the underlying reason directly rather than a generic
message, since there's no fallback ambiguity to hide it behind.

### `GET /healthz`

Liveness check: `{"status": "ok"}`.

### `GET /dashboard`, `/dashboard/api/data`, `/dashboard/events`

See [Dashboard](#dashboard).

## Streaming

`stream: true` is fully supported on `/v1/chat/completions`, including
across provider fallback, caching, and budget enforcement, with a few
mechanics worth knowing:

- **Fallback happens before the client sees anything.** Establishing a
  streaming connection is split into two steps internally
  (`Provider.OpenStream`, then `StreamSession.Relay`): `OpenStream` makes
  the upstream request and checks its status *without* writing to the
  client, so on failure ai-guard can still try the next `fallback:` model
  (exactly like the non-streaming path) before anything is committed.
  Only once a provider's stream is confirmed to have started successfully
  does relaying (and the client-visible response) begin. If every
  attempt fails before that point, the client gets a normal `502`, not a
  half-open stream.
- **A cache hit still returns a stream.** Some clients always request
  `stream: true`; re-sending a non-streaming cached response would break
  their parser. A cache hit is instead synthesized into a minimal valid
  SSE stream (one content chunk, one finish_reason chunk, `[DONE]`) from
  the cached JSON, so caching still pays off for streaming clients, which
  in practice is most of them.
- **A cache miss is cached after it finishes.** ai-guard doesn't know the
  full answer until the stream ends, so caching happens then, from the
  accumulated text/tool-calls/usage, reconstructed into the same shape a
  non-streaming response would have, so either a later streaming or
  non-streaming request for the same prompt can hit it.
- **Budget reservation and reconciliation work exactly like non-streaming**
  (see [Budget enforcement](#budget-enforcement-in-detail)): the
  worst-case estimate is reserved before `OpenStream` is attempted, and
  released against the real cost (from the stream's actual usage, or a
  token-estimate fallback if a provider didn't report it) once the stream
  ends.
- **Usage isn't always in the stream.** ai-guard requests
  `stream_options.include_usage: true` for OpenAI-compatible providers so
  the trailing chunk carries exact token counts; if a provider ignores
  that (some do), cost falls back to estimating completion tokens from
  the accumulated text length. Anthropic always reports exact usage
  in-stream, so no fallback is needed there.

## Model routing

ai-guard picks which configured provider serves a request from the `model`
name (`internal/proxy/route.go`):

| Model prefix | Routed to |
|---|---|
| `gpt-*`, `o1*`, `o3*`, `text-*` | `openai` |
| `claude*` | `anthropic` |
| `llama*`, `mixtral*`, `gemma*`, `deepseek*` | `groq` |
| anything else | `openai` (default) |
| `<provider>/<model>` (e.g. `groq/my-model`) | that literal provider name if it's one of `openai`/`anthropic`/`groq`/`together`; otherwise `together` |

If the resolved provider isn't configured in `providers:`, that attempt is
skipped (falling through to the next entry in `fallback:`, if any).

**Anthropic translation.** Anthropic's Messages API doesn't match the
OpenAI shape, so ai-guard translates both directions:

- The `system` role message becomes Anthropic's top-level `system` field.
- `stop_reason` maps to an OpenAI-style `finish_reason`
  (`end_turn`/`stop_sequence` → `stop`, `max_tokens` → `length`,
  `tool_use` → `tool_calls`).
- **Multimodal content**: `image_url` content parts become Anthropic image
  blocks: a `data:<mime>;base64,<data>` URI becomes a base64 source,
  anything else is passed through as a remote URL source.
- **Tool/function calling**: OpenAI `tools`/`tool_choice` become
  Anthropic's `tools`/`input_schema` form; `tool_calls` on an assistant
  message become `tool_use` content blocks; a `role: "tool"` result
  message becomes a `user` message with a `tool_result` block; and
  `tool_use` blocks in Anthropic's response become OpenAI `tool_calls` on
  the way back (see [Known limitations](#known-limitations) for the two
  edge cases that aren't fully mapped: `tool_choice: "none"`, and
  incremental argument streaming).
- The response is reassembled into an OpenAI-shaped `chat.completion`
  object either way. `max_tokens` defaults to 1024 if the request doesn't
  set one (Anthropic requires it; OpenAI doesn't).

OpenAI, Groq, and Together are all OpenAI-shaped already, so those requests
are forwarded close to verbatim (only the `model` field is rewritten, for
the fallback case where it differs from what the client requested).

## Budget enforcement, in detail

Budgets are enforced by reserving a request's estimated cost *before*
calling upstream, not by checking already-logged spend alone. Logged
spend only reflects requests that have *finished*, so a naive "check
spend so far" gate has a window: two concurrent requests can both read
"under budget" before either is logged. There are two implementations of
this reservation, chosen by `budget.backend`:

- **`local`** (`internal/budget.MemoryEnforcer`, default): closes the race
  with an in-memory, mutex-guarded map of `user_id → sum of
  currently-reserved-but-unsettled cost`, so a concurrent request in the
  *same process* sees a sibling's reservation immediately, not after a
  round trip through SQLite. Correct for a single ai-guard instance;
  **not** correct across multiple, because that in-flight map (and each
  instance's SQLite file) is invisible to every other instance, see
  [Deployment](#deployment).
- **`redis`** (`internal/budget.RedisEnforcer`): the same reservation, but
  the running total lives in one Redis key per user per day, mutated
  atomically by a Lua script (`EVAL` executes single-threaded in Redis, so
  it's effectively a mutex shared by every instance, not just one
  process's memory). Reserve adds the estimate to that key immediately;
  Release trues it up to the real cost once known. There's no separate
  SQLite spend query in this path. The Redis key is the sole source of
  truth for "how much of today's budget is committed," which is what
  makes it correct across instances: two `RedisEnforcer`s pointed at the
  same Redis, in different processes, can never jointly admit more than
  the configured limit (verified in
  `internal/budget/redis_enforcer_test.go`'s
  `TestRedisEnforcer_TwoInstancesShareOneBudget`, which runs two
  independent `RedisEnforcer` instances concurrently and asserts the
  admitted total never exceeds what the shared limit allows).

**How the estimate is computed** (`internal/cost/estimate.go`):

- Prompt size: sum of message content length ÷ 4 (a standard rough
  chars-per-token heuristic). Only text content is sized; image/audio parts
  in multimodal messages are not counted, so multimodal requests are
  undercounted.
- Completion size: the request's `max_tokens` if set, otherwise a
  4096-token default ceiling.
- Cost: run both through the same price table used for real billing.

This is deliberately a **ceiling**, not a prediction. It will usually
reserve more than the request ends up actually costing, and that's the
point: it stops a request from being *admitted* if its worst case would
blow the budget, rather than waiting to find out after the fact. Once the
real response comes back, its actual cost is what gets permanently logged;
the reservation is released the moment the request finishes, regardless of
how the estimate compared to reality.

**What `local` does not do:** enforce a budget across multiple ai-guard
processes. Use `budget.backend: redis` for that. See
[Deployment](#deployment).

## Caching, in detail

The cache key is a SHA-256 hash of the model name plus the full request
payload (minus the `user` and `stream` fields, which don't affect the
response). Identical requests within the configured TTL are served from
cache with zero cost and zero upstream latency.

Two backends:
- **`memory`** (default): in-process, per-instance. A background janitor
  evicts expired entries once a minute. Lost on restart; not shared across
  multiple ai-guard instances.
- **`redis`**: shared across instances, survives restarts. Set
  `cache.backend: redis` and `cache.redis_url`.

Caching is exact-match on the full payload, not semantic/fuzzy similarity.
"cache hit" here means "this exact request was made before within the TTL,"
not "a similar-meaning request was made before."

## Fallback routing

`fallback:` is a flat list of model names, tried in order after the
originally requested model, only when a request errors, or the upstream
returns `5xx` or `429`. Each fallback attempt goes through the same
[model routing](#model-routing) as the primary. A fallback to
`claude-3-haiku` after a `gpt-4o` failure will hit the `anthropic` provider,
if configured. The response's `X-AI-Guard-Model-Used` header tells you which
one actually served the request. If every attempt fails, the client gets a
single `502 upstream_error`.

## Cost pricing table

`internal/cost/cost.go` hardcodes USD-per-1K-token pricing for a fixed set
of OpenAI, Anthropic, Gemini, Groq, and Together models (see the file for the exact
list and prices). Lookup falls back to a prefix match (so
`claude-3-5-sonnet-20241022` matches a `claude-3-5-sonnet` entry), and
finally to a conservative default price for anything unrecognized, so an
unknown model degrades to "probably overestimated" rather than silently
`$0`.

**This table is a manually maintained snapshot and will drift.** Verify
against current provider pricing pages before relying on it for anything
budget-critical, and update `cost.Table` when it's stale.

## Dashboard

`GET /dashboard` serves a self-contained HTML page (no build step, no
external JS) showing:

- Spend today, requests today, cache hit rate, active users (stat tiles)
- Spend by hour, last 24h (bar chart)
- Spend by user, today (table)
- Top 10 most expensive requests, today (table, flags `finish_reason: length` rows)

It updates live via `GET /dashboard/events` (Server-Sent Events, a fresh
snapshot pushed every 3 seconds); `GET /dashboard/api/data` returns the same
snapshot as a one-shot JSON `GET` if you want to pull the numbers into
something else. Both aggregate directly from the SQLite log. There's no
separate metrics store.

The dashboard is protected by a login (one admin account, set up during
`ai-guard init` or any time after via `ai-guard reset-dashboard-password`)
unless that step was skipped, in which case anyone who can reach the port
can view aggregate spend/usage, same as before this existed. `config.yaml`
warns loudly at every `ai-guard run` if no dashboard login is configured.
Sessions are a signed cookie (HMAC-SHA256, secret stored in `config.yaml`),
not a server-side session store: no database growth, and logins survive
restarts. `POST /dashboard/login` is rate-limited to 5 attempts/minute per
IP. Put it behind your own network boundary or reverse-proxy auth as well
if that's a concern. This is one admin account, not multi-user
role-based access control (viewer/editor accounts aren't supported yet).

## CLI reference

```
ai-guard [--config|-c <path>] <command>
```

Global flag `--config`/`-c` (default `config.yaml`) applies to both
commands and selects the config file.

- **`ai-guard init`**: interactive setup (`internal/cli/init.go`). Pick
  providers, enter real provider API keys, choose cache settings, add
  budgeted users (each gets a generated `sk-guard-...` key, shown once,
  copy it immediately), writes `config.yaml`. Numeric prompts (port, daily
  budget) are validated and re-prompt on bad input rather than silently
  substituting a default or a zero value. A malformed budget can never
  turn into a silent unlimited budget. If `config.yaml` already exists,
  you're asked to confirm before it's overwritten (existing virtual keys
  aren't recoverable once that happens, since they're never stored
  anywhere but the config file itself). Entering `0` for a budget asks you
  to confirm you meant "unlimited," since it's easy to mistype. Last step:
  optionally set up the dashboard login (username + masked, confirmed
  password), skippable, but skipping leaves `/dashboard` reachable with
  no login.
- **`ai-guard reset-dashboard-password`**: sets or resets the dashboard's
  one admin account: username, then a masked/confirmed password. Doesn't
  need the server running or the current password. It's the recovery
  path, the same as every comparable self-hosted tool provides since
  there's no email system here to send a reset link through. Also doubles
  as first-time setup if the dashboard login was skipped during `init`.
  Rotates the session secret, so it also signs out any existing session.
- **`ai-guard run`**: loads the config, prints any non-fatal [config
  warnings](#configuration-reference) (e.g. a key mapped to an undefined
  user, an empty provider API key, single-tenant mode), creates `data_dir`
  if it doesn't exist, opens `aiguard.db`, starts the HTTP server, and
  prints a startup banner with the configured providers/cache/budget count
  and the dashboard URL. Ctrl+C or `SIGTERM` triggers a graceful shutdown:
  stops accepting new connections, waits up to 10s for in-flight requests,
  closes the database cleanly, then exits. A port already in use or an
  unreachable Redis produce a specific, actionable error instead of a bare
  Go error string.
- **`ai-guard --version`**: prints the build version (set via
  `-ldflags -X main.version=...` in the release workflow; `dev` otherwise).
- **`ai-guard <command> --help`**: every command has a full description
  and usage examples, not just a one-line summary.

Runtime errors (bad config, port conflicts, unreachable providers) print
just the error, styled consistently with the rest of the CLI, not a
command usage/flags dump, which cobra would otherwise print by default for
every error regardless of whether it was actually a usage mistake.

## Deployment

### Docker

```bash
docker build -t ai-guard .
docker run -p 8787:8787 -v $(pwd)/config.yaml:/data/config.yaml ai-guard
```

The image is a static (`CGO_ENABLED=0`) binary on Alpine, running as a
non-root user, with `/data` as the working directory (mount your
`config.yaml` and, if you want persistence, a volume for `aiguard.db`).

### Single instance vs. horizontal scale

With the default `budget.backend: local`, budget reservations are
per-process. The in-flight reservation map lives in the Go process's
memory. Running **one** ai-guard instance in front of your app (a single
container/VM) enforces budgets correctly, including under concurrent load
(see [Budget enforcement](#budget-enforcement-in-detail)).

Running **multiple** ai-guard instances (e.g., one per pod, behind a load
balancer, for throughput or availability) with `budget.backend: local`
means each instance has its own view of spend: a user's real budget can be
exceeded by up to roughly (number of instances)×, since a request routed
to instance B has no idea what instance A has already reserved or spent.

Set `budget.backend: redis` to fix this: every instance reserves and
reconciles against the same Redis key per user per day, atomically, so a
user's budget holds regardless of which instance a request lands on, see
[Budget enforcement](#budget-enforcement-in-detail) for how, and
`TestRedisEnforcer_TwoInstancesShareOneBudget` for the concurrency proof.
The `cache` can be shared the same way (`cache.backend: redis`); the two
are independent settings; you can share one, both, or neither.

**Cost logging (SQLite, for the dashboard and `/dashboard/api/data`)
remains per-instance** even with `budget.backend: redis`: `aiguard.db` is
a local file, so each instance's dashboard only reflects requests it
personally handled. Budget enforcement no longer needs this data (Redis is
now its own source of truth), but the dashboard does; point every
instance's `data_dir` at the same *shared, concurrent-safe* filesystem if
you want one unified dashboard view, or accept per-instance dashboards and
aggregate externally.

## Development

```bash
go build ./...
go vet ./...
go test ./...
go test ./... -race     # budget's concurrency guarantees are only meaningful under -race
```

Project layout:

```
cmd/ai-guard/        CLI entrypoint (cobra: init, run)
internal/config/     YAML config loading + validation
internal/proxy/      chat/completions handler, auth, provider routing/translation, fallback
internal/cache/      in-memory + Redis response cache
internal/budget/     per-user daily spend reservation/enforcement
internal/cost/       model price table, cost calculation, worst-case estimation
internal/logging/    SQLite request/cost log
internal/dashboard/  embedded live cost dashboard (HTML + SSE)
internal/cli/        interactive init + run commands
```

## Known limitations

- **A dropped upstream connection mid-stream ends the client's stream
  without a formal error event.** Fallback only covers failures before
  the first byte is relayed (see [Streaming](#streaming)); once a
  provider's stream is committed to the client, there's no way to signal
  failure retroactively if it dies partway through. Whatever was streamed
  is still logged/costed for what it actually delivered.
- **Embeddings requests don't use the `fallback:` list.** It's a single
  attempt against the requested model's provider (chat's fallback models
  aren't valid embeddings models, so applying the same list would be
  nonsensical, and a clean error beats a silent model swap mid-indexing-run.
- **Anthropic tool-call argument streaming is batched, not incremental.**
  OpenAI's own streaming format sends a tool call's arguments across many
  small deltas; Anthropic's `input_json_delta` events are accumulated
  server-side and relayed as one complete `tool_calls` delta when the
  block closes, rather than mirrored delta-for-delta.
- **`tool_choice: "none"` has no exact Anthropic equivalent** short of
  omitting `tools` entirely, so it's translated as a best-effort `"auto"`
  rather than actually preventing tool use.
- **Budget state is per-process unless you set `budget.backend: redis`**
  (see [Deployment](#deployment)). **Cost logging (the dashboard) is
  per-process regardless of that setting**: `aiguard.db` is a local
  SQLite file, so each instance's `/dashboard` only reflects requests it
  personally handled.
- **Worst-case cost estimation is a heuristic**, not exact token counting
  (see [Budget enforcement](#budget-enforcement-in-detail)); it can reserve
  more than a request actually costs, though never less than the ceiling it
  computed. Multimodal (image) content is undercounted in this estimate:
  only text parts are sized.
- **The dashboard's login is one admin account, not role-based access
  control.** There's no separate read-only/viewer account yet. Anyone
  with the one password can see and do everything the dashboard exposes.
- **The pricing table is a manually maintained snapshot** and will drift
  from providers' actual current pricing. Embedding models are priced
  input-only (no completion cost).

## Troubleshooting

**"config must define at least one provider"**: `config.yaml` has no
entries under `providers:`. Run `ai-guard init` or edit the file directly.

**401 on every request**: `keys:` is non-empty in your config (multi-tenant
mode), so a valid `Authorization: Bearer <key>` is required; check the key
matches an entry in `keys:` exactly.

**429 immediately, even on a fresh budget**: the worst-case estimate for
the request (driven by `max_tokens`, or the 4096-token default if unset)
exceeds the user's `daily_limit_usd` on its own. Either raise the budget,
lower `max_tokens`, or both.

**A user's budget seems to have no effect (unlimited spend)**: check
`ai-guard run`'s startup output for a warning. The most common cause is a
`keys:` entry whose user_id doesn't match anything under `users:` (typo, or
the user block was removed later): `Enforcer.Reserve` treats "no budget
entry for this user" as "no limit," by design, so this fails open rather
than closed. `ai-guard init` can't produce this (it always writes both
together), but hand-edited configs can.

**"starting server on port N: ... address already in use"**: another
process (possibly another `ai-guard run`) already has that port. Stop it,
or set a different `port:` in `config.yaml`.

**"connecting to redis: ..." on startup**: `cache.backend: redis` is set
but ai-guard couldn't reach `cache.redis_url` at startup. Check Redis is
running and the URL/credentials are correct; ai-guard won't start with a
configured-but-unreachable cache rather than silently falling back to
in-memory.

**ai-guard doesn't exit on Ctrl+C**: it should, within ~10s. `run`
installs a `SIGINT`/`SIGTERM` handler that stops accepting new connections
and waits for in-flight requests to finish before exiting. If a request is
genuinely hung against a slow/unresponsive upstream, that's the 10s
grace period expiring, not a bug. The process will still exit once it
does.

**"database is locked"**: multiple processes pointed at the same
`data_dir`/`aiguard.db`. Each ai-guard instance needs its own database file
(and, per the scaling caveat above, its own view of budget).

**Requests to a model I configured 404 or route to the wrong provider**:
check [Model routing](#model-routing); routing is prefix-based on the model
name, not configurable per-model yet. Use a `<provider>/<model>` prefix to
force routing.
