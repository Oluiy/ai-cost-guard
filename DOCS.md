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
  `fitguard.db` is a local SQLite file, so each instance's dashboard only shows
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

## Shipping the first release

The repo is public now, and the distribution tooling is in place and tested
(`.goreleaser.yaml`, `install.sh`, `npm/`, scratch `Dockerfile`, GoReleaser
workflow). What's left is actually cutting the release:

1. **Commit everything.** Nothing in the working tree is committed yet.
2. **Push a `v0.1.0` tag.** `.github/workflows/release.yml` fires on `v*` and
   runs GoReleaser, which produces the archives and `checksums.txt` that
   `install.sh` and the npm postinstall both download. Until that tag exists,
   both of those installers fail with a 404: they have nothing to fetch.
3. **Publish the npm package.** `cd npm && npm publish`. Its version and the
   git tag have to stay in lockstep (`0.1.0` <-> `v0.1.0`), because
   postinstall derives the download URL from the package version.

Order matters: tag first, then `npm publish`. Publishing npm before the
release assets exist means anyone who installs in that window gets a failed
postinstall.

## Still not built

- **Homebrew.** No formula and no tap. Needs a separate `homebrew-fitguard`
  repo with a formula pointing at the release assets. `brew install` is
  advertised nowhere right now, which is correct until that exists.
- **Published container image.** The `Dockerfile` builds a 15.5MB scratch
  image and works, but nothing pushes it to a registry. The docs tell people
  to `docker build` locally, which is accurate. Adding a `dockers:` block to
  `.goreleaser.yaml` plus GHCR login in the workflow would change that.

## Notes to self

- The public limitations list on `docs/guide/troubleshooting.html#limitations`
  is still live and says most of the above. Decide whether that stays. It's the
  section an evaluating engineer looks for, but it's also public.
- `ENTERPRISE.md` is still tracked and still enumerates gaps (TLS, secrets
  management, HA observability). Same decision applies to it.
