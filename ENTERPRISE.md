# AI Cost Guard: Enterprise Readiness

This document is for anyone evaluating ai-guard for use inside an
organization, not a solo/local deployment. It says plainly what's solid
today, what's missing, and what you'd need to add or wrap around it before
it belongs in a production enterprise environment. For how the system
works day to day, see the [documentation](./docs/index.html); for the
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
  across multiple ai-guard instances behind a load balancer, when
  `budget.backend: redis` is set. This is not a claim taken on faith: it's
  covered by a `-race`-tested concurrency test
  (`TestRedisEnforcer_TwoInstancesShareOneBudget`) that runs two independent
  enforcer instances against a shared Redis and asserts the combined total
  never exceeds the configured limit, and was separately verified live over
  real HTTP against two separate running processes.
- **Authentication via gateway-issued virtual keys**, not a client-supplied
  identity, so a budget can't be evaded by a caller simply omitting or
  changing a header. See [Authentication](./docs/guide/api-reference.html#auth)
  for why this distinction matters.
- **Response caching, including for streaming clients**: a cache hit is
  synthesized into a valid stream rather than silently falling back to a
  non-streaming response shape.
- **Provider fallback that never hands a client a broken partial response**,
  confirmed for streaming: fallback works *before* anything is
  written to the client (see
  [Streaming](./docs/guide/api-reference.html#streaming)).
- **Full request/cost audit trail** in SQLite: every request's user, model,
  tokens, cost, latency, cache hit, and finish_reason.

## What's missing

### Security & compliance

- **No TLS.** ai-guard listens on plain HTTP. It must sit behind a reverse
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
- **The dashboard's login is one admin account, not RBAC.** `ai-guard init`
  (or `ai-guard reset-dashboard-password` any time after) sets up a single
  username/password gating `/dashboard`: session cookies are a signed
  HMAC, not a database-backed session store, and login attempts are
  rate-limited. There's no separate read-only/viewer account, no SSO, and
  no audit trail of who logged in when. Skipping that setup step leaves
  the dashboard reachable with no login at all, same as before this
  existed, fine on a private network, not acceptable exposed any wider
  without your own reverse-proxy auth in front regardless.
- **No formal security audit.** This codebase has had a careful manual
  security-minded review during development (closing an auth-bypass class
  of bug where budgets could be evaded by a client-controlled identity,
  checking the dashboard's DOM-insertion points for XSS, fixing silent
  failure modes that could mask misconfiguration), but that is not
  equivalent to a penetration test or a dependency vulnerability scan, and
  neither has been done.
- **No compliance posture.** No SOC2, no audit logging of *configuration*
  changes (only of proxied requests), no data residency controls.

### High availability & scale

- **Cost-log analytics don't scale horizontally.** `aiguard.db` is a local
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
- **Single point of failure by default.** ai-guard has no built-in
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

- Put ai-guard behind a TLS-terminating reverse proxy or load balancer
  (nginx, Caddy, your cloud LB); don't expose it directly.
- Inject `config.yaml`'s `${ENV_VAR}` values from your existing secrets
  manager instead of a plain file.
- Put the `/dashboard` path behind the same authenticating proxy, or don't
  expose it beyond a private network.
- Run one ai-guard instance per logical unit you're willing to have
  degrade independently, with `budget.backend: redis` and
  `cache.backend: redis` pointed at infrastructure you already operate
  with HA.
- Ship ai-guard's structured pterm/log output to whatever log aggregation
  you already run, and poll `/dashboard/api/data` on an interval into your
  existing metrics pipeline until a native exporter exists.
- Load-test it yourself against your actual traffic shape before trusting
  a capacity number.

## Who this is for right now

- **Good fit today:** a single team, a single ai-guard instance (or a
  small Redis-backed cluster) in front of a moderate-traffic internal or
  early-stage product, run by people comfortable operating the
  infrastructure around it themselves (TLS, secrets, log shipping).
- **Not a good fit yet:** anything requiring compliance sign-off, a
  managed/hands-off deployment, unified multi-instance dashboards, or
  guaranteed behavior at high throughput without your own load testing
  first.
