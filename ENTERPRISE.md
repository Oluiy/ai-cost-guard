# FitGuard: Enterprise Readiness

This document is for anyone evaluating fitguard for use inside an
organization, not a solo/local deployment. It says plainly what's solid
today, what's missing, and what you'd need to add or wrap around it before
it belongs in a production enterprise environment. For how the system
works day to day, see the [documentation](https://ai-cost-guard-ruddy.vercel.app/); for the
quickstart, see [README.md](./README.md).

## Bottom line

**Not enterprise-ready today.** The cost-control mechanics, the actual
reason this project exists, are solid and verified: budget enforcement,
caching, fallback, and streaming all work, including correctly across
multiple instances when configured with a shared Redis backend. What's
missing is everything *around* that core: transport security, secrets
management, key lifecycle, multi-instance observability, and the
operational hardening an enterprise environment assumes by default. None
of it is exotic to add, but none of it exists yet either. This section
lists exactly what, so you can decide whether to wait, contribute it, or
wrap it yourself.

## What you can rely on today

- **Per-user budget enforcement that holds under concurrency**, including
  across multiple fitguard instances behind a load balancer, when
  `budget.backend: redis` is set. This is not a claim taken on faith: it's
  covered by a `-race`-tested concurrency test
  (`TestRedisEnforcer_TwoInstancesShareOneBudget`) that runs two independent
  enforcer instances against a shared Redis and asserts the combined total
  never exceeds the configured limit, and was separately verified live over
  real HTTP against two separate running processes.
- **Authentication via gateway-issued virtual keys**, not a client-supplied
  identity, so a budget can't be evaded by a caller simply omitting or
  changing a header. See [Authentication](https://ai-cost-guard-ruddy.vercel.app/guide/api-reference.html#auth)
  for why this distinction matters.
- **Response caching, including for streaming clients**: a cache hit is
  synthesized into a valid stream rather than silently falling back to a
  non-streaming response shape.
- **Provider fallback that never hands a client a broken partial response**,
  confirmed for streaming: fallback works *before* anything is
  written to the client (see
  [Streaming](https://ai-cost-guard-ruddy.vercel.app/guide/api-reference.html#streaming)).
- **Full request/cost audit trail** in SQLite: every request's user, model,
  tokens, cost, latency, cache hit, and finish_reason. Prompt and response
  *content* is never written to this log — only the metadata above.
- **Timing-safe login.** The dashboard's login compares against a decoy
  bcrypt hash when the submitted username doesn't exist, so a failed
  attempt takes the same time whether or not that username is real —
  closing a username-enumeration side channel that existed until this was
  specifically tested for and fixed.
- **Concurrency-tested under real load, not just unit tests.** A live
  `-race`-instrumented run under concurrent proxy traffic and concurrent
  settings writes (see below) surfaced and fixed a genuine data race in
  the logging library's shared printer — the kind of bug unit tests alone
  do not catch. That run is why this list makes a concurrency claim at
  all, rather than an assumption.

## Dashboard settings: read-only no longer

As of this version, the dashboard's **Settings** page can change
`cache.enabled`, `cache.ttl_seconds`, per-user `daily_limit_usd`, and the
`fallback` list live, with no restart, writing back to `config.yaml`. This
changes the enterprise risk calculus for the dashboard: a compromised
dashboard session used to be read-only (spend visibility only); it can now
change what the gateway does.

What limits that:

- **Provider `api_key`, `base_url`, virtual `keys:`, and `session_secret`
  are not reachable through this API at all** — not hidden in the UI, not
  present in the wire format (`config.Editable`) the endpoint accepts or
  returns. There is no code path from a dashboard session to a provider
  credential.
- **It refuses to orphan a live virtual key.** Removing a user's budget
  while a `keys:` entry still maps to them would otherwise silently make
  that key spend without a cap; the server rejects the request and names
  the affected user rather than allowing it.
- **Adding a new `user_id` mints a virtual key.** A compromised dashboard
  session can now create a new authenticated caller against your gateway,
  not just change budgets for existing ones — the key is capped by
  whatever `daily_limit_usd` is set in the same request, and is returned
  once in the response body (`issued_keys`), never logged or retrievable
  again.
- **`fallback` entries are rejected unless their provider is configured.**
  A write naming a model whose provider isn't in `providers:` fails
  validation rather than being accepted and only failing later, at request
  time. Providers themselves are still terminal-only, added via
  `fitguard add-provider`, not through this API.
- Every write is validated before anything is mutated (bad values leave
  the running config and the file untouched), and the on-disk file is
  tightened to owner-only permissions on every write, including one that
  arrived some other way at looser permissions.
- It's gated behind the same session-cookie auth as the rest of the
  dashboard — everything in "What's missing" below about that auth
  (no RBAC, no SSO, one admin account) applies here too, now with
  slightly higher stakes than a read-only view.

## What's missing

### Security & compliance

- **No TLS.** fitguard listens on plain HTTP. It must sit behind a reverse
  proxy or load balancer that terminates TLS. This isn't optional for any
  network you don't fully trust, and it isn't documented anywhere as a
  deployment requirement today, which it should be.
- **No secrets management integration.** Provider API keys and virtual
  keys live in `config.yaml`, with `${ENV_VAR}` expansion as the only
  indirection. There's no Vault, AWS Secrets Manager, or similar
  integration. You're responsible for how that file/those env vars reach
  the process.
- **No key lifecycle.** Virtual keys are a static map in the config file.
  Rotating or revoking one means editing the file and restarting the
  process. There's no API, no expiry, no scoped permissions.
- **The dashboard's login is one admin account, not RBAC.** `fitguard init`
  (or `fitguard reset-dashboard-password` any time after) sets up a single
  username/password gating `/dashboard`: session cookies are a signed
  HMAC, not a database-backed session store, and login attempts are
  rate-limited. There's no separate read-only/viewer account, no SSO, and
  no audit trail of who logged in when. Skipping that setup step leaves
  the dashboard reachable with no login at all, same as before this
  existed, fine on a private network, not acceptable exposed any wider
  without your own reverse-proxy auth in front regardless.
  - Sessions default to a 7-day lifetime (`dashboard.session_ttl_hours`,
    configurable) and there is no server-side revocation list: a leaked
    cookie is valid until it expires. The one immediate revocation is
    rotating `session_secret` via `fitguard reset-dashboard-password`,
    which invalidates every outstanding session at once.
  - If fitguard runs behind a reverse proxy, set `trusted_proxies` to its
    address. This is what makes the login rate limiter key on the real
    caller instead of the proxy (otherwise every request behind the proxy
    looks like one client and a single attacker can exhaust the limiter
    for everyone), and what lets the session cookie be marked `Secure`.
    It's opt-in and must name the actual proxy: honoring these headers
    from an untrusted source would let a caller spoof its own address and
    bypass the rate limiter entirely.
- **No formal security audit.** This codebase has had a careful manual
  security-minded review during development (closing an auth-bypass class
  of bug where budgets could be evaded by a client-controlled identity,
  checking the dashboard's DOM-insertion points for XSS, fixing silent
  failure modes that could mask misconfiguration), but that is not
  equivalent to a penetration test or a dependency vulnerability scan, and
  neither has been done.
- **No compliance posture.** No SOC2, no audit logging of *configuration*
  changes (only of proxied requests), no data residency controls.
- **A misconfigured budget mapping fails open by default.** A `keys:`
  entry whose `user_id` has no matching `users:` entry is treated as
  unlimited spend rather than refused — usually a typo, but a silent one.
  `fitguard run` warns loudly about it at every startup, and setting
  `budget.fail_closed: true` refuses the request instead; it defaults to
  off because it's the wrong default for solo/local use, where budgets
  are often not configured at all.

### High availability & scale

- **Cost-log analytics don't scale horizontally.** `fitguard.db` is a local
  SQLite file. Even with `budget.backend: redis` making *enforcement*
  correct across instances, each instance's `/dashboard` only reflects
  requests it personally handled. There's no unified fleet-wide view.
- **No HA story for Redis.** If you run `budget.backend: redis` or
  `cache.backend: redis` and that Redis becomes unreachable, the affected
  requests fail rather than degrading gracefully to a fallback mode.
- **No load testing has been done.** Correctness has been verified under
  modest concurrency: dozens of concurrent requests in tests and live
  smoke tests. Nothing has been measured at the throughput an enterprise
  deployment would actually need to plan capacity around.
- **Single point of failure by default.** fitguard has no built-in
  clustering, leader election, or request queuing. Running it reliably at
  scale means putting standard infrastructure (a load balancer, a process
  supervisor with restart policies, health-check-based routing) around it
  yourself. None of that is bundled or documented as a reference
  architecture today.

### Observability

- **No metrics export.** No Prometheus endpoint, no OpenTelemetry traces.
  The only visibility is the built-in `/dashboard` and its
  `/dashboard/api/data`/`/dashboard/events` feeds, which are aggregate
  views, not per-request tracing or alerting primitives.
- **No alerting.** Nothing pages anyone when spend spikes, a provider
  starts failing, or the budget-exceeded rate climbs. You'd need to poll
  the dashboard API and build that yourself.

### Provider integration confidence

- **Verified against mock servers, not the real provider APIs.** Every
  test in this project, including streaming, fallback, and tool-calling,
  has been run against local mocks built to match each provider's
  *documented* API shape. That's meaningfully weaker than verification
  against live OpenAI/Anthropic/Groq/Together endpoints, which can surface
  quirks (exact error response shapes, streaming keep-alive behavior, rate
  limit response formats) no mock will catch. Before trusting this in
  front of production traffic, run it against your actual provider
  accounts and watch it under real conditions.

## What you can do today to close the gap yourself

None of the above requires waiting on this project. Most of it is
standard infrastructure you likely already run:

- Put fitguard behind a TLS-terminating reverse proxy or load balancer
  (nginx, Caddy, your cloud LB); don't expose it directly. Set
  `trusted_proxies` to that proxy's address once you do.
- Set `budget.fail_closed: true` so a misconfigured `keys:`/`users:`
  mapping refuses a request instead of silently granting unlimited spend.
- Inject `config.yaml`'s `${ENV_VAR}` values from your existing secrets
  manager instead of a plain file.
- Put the `/dashboard` path behind the same authenticating proxy, or don't
  expose it beyond a private network.
- Run one fitguard instance per logical unit you're willing to have
  degrade independently, with `budget.backend: redis` and
  `cache.backend: redis` pointed at infrastructure you already operate
  with HA.
- Ship fitguard's structured pterm/log output to whatever log aggregation
  you already run, and poll `/dashboard/api/data` on an interval into your
  existing metrics pipeline until a native exporter exists.
- Load-test it yourself against your actual traffic shape before trusting
  a capacity number.

## Who this is for right now

- **Good fit today:** a single team, a single fitguard instance (or a
  small Redis-backed cluster) in front of a moderate-traffic internal or
  early-stage product, run by people comfortable operating the
  infrastructure around it themselves (TLS, secrets, log shipping).
- **Not a good fit yet:** anything requiring compliance sign-off, a
  managed/hands-off deployment, unified multi-instance dashboards, or
  guaranteed behavior at high throughput without your own load testing
  first.
