# Architecture

For using FitGuard, see [README.md](./README.md) and the
[full documentation](https://ai-cost-guard-ruddy.vercel.app/). This file is for reading or
changing the code itself: what each package does, and the paths that
touch config on every request.

## Package layout

```
cmd/fitguard/       CLI entrypoint (cobra: init, run, reset-dashboard-password)
internal/config/    YAML config loading + validation + live-editable settings
internal/proxy/     /v1/chat/completions handler, provider routing/translation, fallback
internal/cache/     in-memory + Redis response cache
internal/budget/    per-user daily spend enforcement
internal/cost/      model price table + cost calculation
internal/logging/   SQLite request/cost log
internal/auth/      password hashing + signed-cookie dashboard sessions
internal/dashboard/ embedded live cost dashboard (HTML + SSE + settings API)
internal/cli/       interactive init + run commands
```

## The one thing worth understanding before you touch `internal/config`

`config.Config` (loaded once at startup from `config.yaml`) and
`config.Settings` (the live, mutex-guarded subset the dashboard can edit
while running) are two different types on purpose, not an accident of
naming.

- `Cfg *config.Config` — read directly. Nothing mutates it after startup:
  provider credentials, virtual keys, the session secret, the port.
- `Settings *config.Settings` — **never** read the underlying `Config`
  fields directly for these. Go through the accessor methods
  (`CacheEnabled()`, `CacheTTLSeconds()`, `Fallback()`, `Budget(userID)`).
  These are the four things `PUT /dashboard/api/settings` can change while
  request goroutines are reading them concurrently; reading the raw struct
  field instead of the accessor is a data race, and `go test -race` will
  say so.

If you're adding a new config field, the deciding question is: does this
carry a credential, or does changing it while running require restarting
something else (a listener, a DB connection)? If yes, it belongs on
`Config` and stays terminal-only. If it's a pure behavioral knob read
per-request, it can go on `Settings` — but adding it there means also
adding it to `config.Editable` (the wire type) and to `ValidateEditable`.
`config.Editable` is deliberately not `*Config` serialized directly:
that boundary is what stops a future field on `Config` from being
silently exposed to the dashboard API just by existing.

## Build and test

```bash
go build ./...
go vet ./...
go test ./...
go test ./... -race   # required for internal/config and internal/budget —
                       # both have concurrency guarantees that only a race
                       # build actually verifies
```
