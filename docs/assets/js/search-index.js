// Hand-maintained search index for the docs search overlay (Cmd+K).
// Kept as plain data, separate from app.js, so adding a new page or
// section is a one-line addition here, not a change to search logic.
//
// Paths are root-relative to the docs/ site (no "./" or "../"): app.js
// resolves each one against whatever depth the current page happens to
// be at, so this file doesn't need to know or care where it's loaded from.
window.FITGUARD_SEARCH_INDEX = [
  {
    title: "FitGuard",
    section: "Home",
    url: "index.html",
    keywords: "overview landing home stop bills cost gateway proxy llm",
  },

  // ---------- Start ----------
  {
    title: "Getting started",
    section: "Start",
    url: "guide/getting-started.html",
    keywords: "install init run quickstart first request curl openai anthropic sdk virtual key setup",
  },
  {
    title: "Install",
    section: "Start · Getting started",
    url: "guide/getting-started.html#install",
    keywords: "install download binary install.sh curl npm npx go install docker build release homebrew brew linux macos windows",
  },
  {
    title: "Point your app at fitguard",
    section: "Start · Getting started",
    url: "guide/getting-started.html#point-your-app",
    keywords: "baseurl base_url integration openai sdk python typescript switch migrate",
  },
  {
    title: "Using it with an existing Anthropic SDK integration",
    section: "Start · Getting started",
    url: "guide/getting-started.html#anthropic-sdk",
    keywords: "anthropic native sdk messages.create claude switch openai client",
  },
  {
    title: "Full example: dev to production",
    section: "Start · Getting started",
    url: "guide/getting-started-example.html",
    keywords: "full example migration walkthrough anthropic openai dependency pip install venv virtualenv requirements.txt production deploy company existing project python typescript dotnet java go rust",
  },
  {
    title: "Budgets and virtual keys",
    section: "Start · Getting started",
    url: "guide/getting-started.html#budgets",
    keywords: "budget daily limit virtual key sk-guard authorization bearer",
  },
  {
    title: "How it works",
    section: "Start",
    url: "guide/how-it-works.html",
    keywords: "architecture design internals lifecycle diagram overview",
  },
  {
    title: "The request lifecycle",
    section: "Start · How it works",
    url: "guide/how-it-works.html#lifecycle",
    keywords: "lifecycle order steps authenticate validate cache reserve upstream log release",
  },
  {
    title: "Budget enforcement",
    section: "Start · How it works",
    url: "guide/how-it-works.html#budget",
    keywords: "budget reservation race condition concurrent estimate ceiling max_tokens local redis enforcer",
  },
  {
    title: "Caching",
    section: "Start · How it works",
    url: "guide/how-it-works.html#caching",
    keywords: "cache key sha256 ttl exact match semantic memory redis hit miss",
  },
  {
    title: "Model routing",
    section: "Start · How it works",
    url: "guide/how-it-works.html#routing",
    keywords: "routing model prefix gpt claude gemini llama provider dispatch which provider",
  },
  {
    title: "Fallback routing",
    section: "Start · How it works",
    url: "guide/how-it-works.html#fallback",
    keywords: "fallback retry outage 429 5xx degrade model-used header",
  },

  // ---------- Reference ----------
  {
    title: "Configuration reference",
    section: "Reference",
    url: "guide/configuration.html",
    keywords: "config.yaml providers cache budget redis keys fallback yaml fields schema",
  },
  {
    title: "Redis backend",
    section: "Reference · Configuration",
    url: "guide/configuration.html#redis",
    keywords: "redis cache backend budget backend multi instance horizontal scale",
  },
  {
    title: "Failing closed",
    section: "Reference · Configuration",
    url: "guide/configuration.html#fail-closed",
    keywords: "fail_closed fail closed unlimited budget typo user_id missing entry refuse deny unmetered",
  },
  {
    title: "Dashboard login",
    section: "Reference · Configuration",
    url: "guide/configuration.html#dashboard-auth",
    keywords: "dashboard login password session_secret bcrypt auth admin account",
  },
  {
    title: "Providers",
    section: "Reference",
    url: "guide/providers.html",
    keywords: "openai anthropic gemini groq together provider setup api key base_url",
  },
  {
    title: "OpenAI",
    section: "Reference · Providers",
    url: "guide/providers.html#openai",
    keywords: "openai gpt-4o gpt api key setup example",
  },
  {
    title: "Anthropic",
    section: "Reference · Providers",
    url: "guide/providers.html#anthropic",
    keywords: "anthropic claude messages api translation setup example",
  },
  {
    title: "Gemini",
    section: "Reference · Providers",
    url: "guide/providers.html#gemini",
    keywords: "gemini google generativelanguage flash pro setup example",
  },
  {
    title: "Groq",
    section: "Reference · Providers",
    url: "guide/providers.html#groq",
    keywords: "groq llama mixtral fast inference setup example",
  },
  {
    title: "Together AI",
    section: "Reference · Providers",
    url: "guide/providers.html#together",
    keywords: "together ai llama meta-llama open source models setup example",
  },
  {
    title: "API reference",
    section: "Reference",
    url: "guide/api-reference.html",
    keywords: "api endpoints rest http reference request response",
  },
  {
    title: "POST /v1/chat/completions",
    section: "Reference · API",
    url: "guide/api-reference.html#chat-completions",
    keywords: "chat completions endpoint messages model max_tokens tools request body",
  },
  {
    title: "Chat completions response",
    section: "Reference · API",
    url: "guide/api-reference.html#chat-response",
    keywords: "response shape response_model chat.completion choices message usage finish_reason",
  },
  {
    title: "Response headers",
    section: "Reference · API",
    url: "guide/api-reference.html#response-headers",
    keywords: "x-cache x-fitguard-model-used x-fitguard-warning headers hit miss truncated",
  },
  {
    title: "Streaming",
    section: "Reference · API",
    url: "guide/api-reference.html#streaming",
    keywords: "stream true sse server sent events chunks done fallback cache",
  },
  {
    title: "Error codes",
    section: "Reference · API",
    url: "guide/api-reference.html#errors",
    keywords: "error 400 401 429 502 budget_exceeded invalid_api_key upstream_error invalid_request_error",
  },
  {
    title: "POST /v1/embeddings",
    section: "Reference · API",
    url: "guide/api-reference.html#embeddings",
    keywords: "embeddings vector embed text-embedding input",
  },
  {
    title: "GET /healthz",
    section: "Reference · API",
    url: "guide/api-reference.html#healthz",
    keywords: "health healthz liveness probe uptime monitor kubernetes",
  },
  {
    title: "Dashboard API",
    section: "Reference · API",
    url: "guide/api-reference.html#dashboard-api",
    keywords: "dashboard api data requests report csv export whoami events sse json",
  },
  {
    title: "Live settings API",
    section: "Reference · API",
    url: "guide/api-reference.html#settings-api",
    keywords: "settings live dashboard cache ttl budget fallback put patch config write issued_keys providers available_models",
  },
  {
    title: "CLI reference",
    section: "Reference",
    url: "guide/commands.html",
    keywords: "fitguard init add-provider run reset-dashboard-password version config flag command terminal",
  },
  {
    title: "fitguard add-provider",
    section: "Reference · CLI",
    url: "guide/commands.html#add-provider",
    keywords: "add provider groq together gemini fallback unlock new provider without overwrite",
  },

  // ---------- Operate ----------
  {
    title: "Deployment",
    section: "Operate",
    url: "guide/deployment.html",
    keywords: "deploy production ship hosting server",
  },
  {
    title: "What goes where",
    section: "Operate · Deployment",
    url: "guide/deployment.html#what-goes-where",
    keywords: "what goes where git commit config.yaml config.production.yaml fitguard.db dockerfile gitignore secrets",
  },
  {
    title: "Docker",
    section: "Operate · Deployment",
    url: "guide/deployment-docker.html",
    keywords: "docker container image volume build run compose",
  },
  {
    title: "The Dockerfile, annotated",
    section: "Operate · Deployment",
    url: "guide/deployment-docker.html#dockerfile",
    keywords: "dockerfile multi-stage scratch alpine cgo static binary annotated example",
  },
  {
    title: "docker-compose.yml",
    section: "Operate · Deployment",
    url: "guide/deployment-docker.html#compose",
    keywords: "docker compose redis volumes .env local development",
  },
  {
    title: "Already have a Dockerfile for your own app?",
    section: "Operate · Deployment",
    url: "guide/deployment-docker.html#alongside",
    keywords: "existing dockerfile python dotnet node java two services separate container multi-service render railway",
  },
  {
    title: "VPS deployment",
    section: "Operate · Deployment",
    url: "guide/deployment-vps.html",
    keywords: "digitalocean azure aws droplet ec2 virtual machine vm systemd vps",
  },
  {
    title: "Confirm it actually worked",
    section: "Operate · Deployment",
    url: "guide/deployment-vps.html#verify",
    keywords: "verify checklist healthz dashboard acceptance test production confirm",
  },
  {
    title: "Render, Heroku, and Railway",
    section: "Operate · Deployment",
    url: "guide/deployment-paas.html",
    keywords: "heroku railway render paas port env cloud platform",
  },
  {
    title: "Persistence and operations",
    section: "Operate · Deployment",
    url: "guide/deployment-operations.html",
    keywords: "ephemeral filesystem disk persistence lost data reset deploy volume sqlite render disk railway volume heroku fitguard_config redis",
  },
  {
    title: "Running it on its own domain",
    section: "Operate · Deployment",
    url: "guide/deployment-operations.html#proxy",
    keywords: "domain subdomain reverse proxy caddy nginx tls https dns microservice",
  },
  {
    title: "Trusted proxies",
    section: "Operate · Deployment",
    url: "guide/deployment.html#trusted-proxies",
    keywords: "trusted_proxies x-forwarded-for x-forwarded-proto reverse proxy rate limit secure cookie nginx caddy load balancer client ip",
  },
  {
    title: "One instance vs. many",
    section: "Operate · Deployment",
    url: "guide/deployment-operations.html#proxy",
    keywords: "scale horizontal multiple instances load balancer redis budget per-process",
  },
  {
    title: "Production checklist",
    section: "Operate · Deployment",
    url: "guide/deployment-operations.html#backup",
    keywords: "checklist production ready launch review secure",
  },
  {
    title: "Troubleshooting",
    section: "Operate",
    url: "guide/troubleshooting.html",
    keywords: "troubleshoot debug error problem broken fix help",
  },
  {
    title: "Startup problems",
    section: "Operate · Troubleshooting",
    url: "guide/troubleshooting.html#startup",
    keywords: "wont start address already in use port conflict redis connection provider",
  },
  {
    title: "Request problems",
    section: "Operate · Troubleshooting",
    url: "guide/troubleshooting.html#requests",
    keywords: "401 429 unauthorized budget no effect wrong provider truncated 404",
  },
  {
    title: "Dashboard problems",
    section: "Operate · Troubleshooting",
    url: "guide/troubleshooting.html#dashboard-issues",
    keywords: "dashboard login forgot password empty database locked sse proxy buffering",
  },
  {
    title: "Known limitations",
    section: "Operate · Troubleshooting",
    url: "guide/troubleshooting.html#limitations",
    keywords: "limitations missing not supported roadmap caveats assistants api rbac",
  },
];
