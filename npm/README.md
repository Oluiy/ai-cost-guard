# fitguard

Self-hosted, OpenAI-compatible AI gateway that stops runaway LLM bills
before they happen. This package is a thin installer: `postinstall`
downloads the matching prebuilt `fitguard` binary from [GitHub
Releases](https://github.com/Oluiy/ai-cost-guard/releases) and verifies it
against the published checksums. No Node dependencies — the binary itself
is a single Go executable.

## Install

```bash
npm install -g fitguard
```

## Use

```bash
fitguard init   # interactive: pick providers, paste real API keys, add budgeted users
fitguard run    # starts the gateway on http://localhost:8787
```

Full documentation: https://ai-cost-guard-ruddy.vercel.app/
