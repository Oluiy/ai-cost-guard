# Working notes (private)

Not published. This file is gitignored, so it's the place to be blunt about
what isn't finished without it being the first thing a visitor reads.

Public docs live in [`docs/`](./docs/). Open
[`docs/index.html`](./docs/index.html) in a browser, or serve it with
`cd docs && python3 -m http.server 8000`.

## Known limitations

Moved out of README.md, where it was the last thing a prospective user saw
before deciding whether to try this.

### API surface

- **No Assistants or Responses API.** Only `/v1/chat/completions` and
  `/v1/embeddings`.
- **Embeddings requests don't use `fallback:`.** A single attempt against the
  requested model's provider. This one is deliberate: chat fallback models
  aren't valid embedding models, and swapping models partway through an
  indexing run writes mismatched dimensions into a vector store.

### Streaming

- **A dropped upstream connection mid-stream ends the client's stream with no
  formal error event.** Fallback only covers failures before the first byte is
  relayed. Once a provider's stream is committed to the client there's no way
  to hand it to a fallback if it dies partway through.
- **Anthropic tool-call argument streaming is batched, not incremental.**
  `input_json_delta` events are accumulated server-side and relayed as one
  complete `tool_calls` delta when the block closes.
- **`tool_choice: "none"` has no exact Anthropic equivalent** short of omitting
  `tools` entirely, so it's translated as a best-effort `"auto"` and doesn't
  actually prevent tool use.

### Scale and accuracy

- **Budget enforcement is per-process unless `budget.backend: redis` is set.**
  Reservations are held in memory. Multiple instances behind a load balancer
  each get their own view of spend, so a user's budget can be exceeded by
  roughly (instance count) x. Redis fixes this; the default doesn't.
- **Cost logging is per-process regardless of the budget backend.**
  `aiguard.db` is a local SQLite file, so each instance's dashboard only shows
  requests it handled itself. This one has no fix short of a shared database.
- **Worst-case cost estimation is a heuristic.** Prompt size comes from message
  text length at ~4 chars/token, completion size from `max_tokens` or a 4096
  default. It's a ceiling, not a token count. Multimodal content is
  undercounted because only text parts are sized.
- **The pricing table in `internal/cost/cost.go` is a hand-maintained snapshot**
  and drifts from real provider pricing. Embedding models are priced input-only.

### Access control

- **The dashboard has one admin account, not role-based access control.** No
  read-only or viewer role. Anyone with the password can see everything.
- **No TLS.** Serves plain HTTP by design and needs a reverse proxy in front.

## Blockers before this is genuinely installable

1. **The repo is private.** `go install github.com/Oluiy/ai-cost-guard/cmd/ai-guard@latest`
   is shown in the README and the docs, and it does not work for anyone but me.
   Making the repo public is what makes that instruction true.
2. **No tagged release.** `.github/workflows/release.yml` only fires on a `v*`
   tag and none has been pushed, so there are no prebuilt binaries for anyone
   who doesn't have a Go toolchain.

## Notes to self

- The public limitations list on `docs/guide/troubleshooting.html#limitations`
  is still live and says most of the above. Decide whether that stays. It's the
  section an evaluating engineer looks for, but it's also public.
- `ENTERPRISE.md` is still tracked and still enumerates gaps (TLS, secrets
  management, HA observability). Same decision applies to it.
- Nothing in the working tree is committed yet.
