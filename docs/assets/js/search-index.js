// Hand-maintained search index for the docs search overlay (Cmd+K).
// Kept as plain data, separate from app.js, so adding a new page or
// section is a one-line addition here, not a change to search logic.
//
// Paths are root-relative to the docs/ site (no "./" or "../"): app.js
// resolves each one against whatever depth the current page happens to
// be at, so this file doesn't need to know or care where it's loaded from.
window.AI_GUARD_SEARCH_INDEX = [
  {
    title: "AI Guard",
    section: "Home",
    url: "index.html",
    keywords: "overview landing home stop bills cost gateway",
  },
  {
    title: "Getting started",
    section: "Guide",
    url: "guide/getting-started.html",
    keywords: "install init run quickstart first request curl openai anthropic sdk virtual key",
  },
  {
    title: "Point your app at ai-guard",
    section: "Guide · Getting started",
    url: "guide/getting-started.html#point-your-app",
    keywords: "baseurl base_url integration openai sdk python typescript switch",
  },
  {
    title: "Using it with an existing Anthropic SDK integration",
    section: "Guide · Getting started",
    url: "guide/getting-started.html#anthropic-sdk",
    keywords: "anthropic native sdk messages.create claude switch openai client",
  },
  {
    title: "Streaming",
    section: "Guide · Getting started",
    url: "guide/getting-started.html#streaming",
    keywords: "stream true sse chunks fallback cache",
  },
  {
    title: "Budgets and virtual keys",
    section: "Guide · Getting started",
    url: "guide/getting-started.html#budgets",
    keywords: "budget daily limit virtual key sk-guard authorization",
  },
  {
    title: "The dashboard",
    section: "Guide · Getting started",
    url: "guide/getting-started.html#dashboard",
    keywords: "dashboard spend chart cache hit rate top expensive requests",
  },
  {
    title: "Configuration reference",
    section: "Guide",
    url: "guide/configuration.html",
    keywords: "config.yaml providers cache budget redis keys fallback",
  },
  {
    title: "Redis backend",
    section: "Guide · Configuration",
    url: "guide/configuration.html#redis",
    keywords: "redis cache backend budget backend multi instance horizontal scale",
  },
  {
    title: "CLI reference",
    section: "Guide",
    url: "guide/commands.html",
    keywords: "ai-guard init run version config flag command",
  },
];
