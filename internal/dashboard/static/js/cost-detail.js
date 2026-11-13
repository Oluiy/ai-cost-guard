// Expandable cost detail. Click (or Enter/Space on) a request row to see
// what that cost was made of: tokens by kind with the rate each was billed
// at, cache savings, and any caveats. Loaded before dashboard.js, which
// calls CostDetail.attach() once per rendered row.
//
// Everything shown is built from numbers stored with the request when it
// happened, never recomputed from today's prices, so an old row keeps
// explaining the figure it actually billed.
(function () {
  "use strict";

  // Open rows, by request id. The overview table is rebuilt on every live
  // update (every few seconds), so open state can't live in the DOM.
  var open = new Set();

  function money(v) {
    if (!v) return "$0";
    if (v < 0.01) return "$" + v.toFixed(6).replace(/0+$/, "").replace(/\.$/, "");
    return "$" + v.toFixed(4).replace(/0+$/, "").replace(/\.$/, "");
  }
  function int(v) {
    return Number(v).toLocaleString();
  }
  function perMillion(costUSD, tokens) {
    return tokens > 0 && costUSD > 0 ? money((costUSD / tokens) * 1e6) + " / 1M tokens" : "—";
  }

  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined) n.textContent = text;
    return n;
  }

  // The itemised lines: [what, quantity, rate, cost].
  function lines(r) {
    var out = [];
    var kind = r.kind || "chat";
    if (kind === "image") {
      out.push(["Images generated", int(r.units), r.units > 0 ? money(r.cost_usd / r.units) + " each" : "—", r.cost_usd]);
    } else if (kind === "speech") {
      out.push(["Characters spoken", int(r.units), r.units > 0 ? money((r.cost_usd / r.units) * 1000) + " / 1K chars" : "—", r.cost_usd]);
    } else if (kind === "transcription") {
      var mins = r.units / 60;
      out.push(["Audio transcribed", mins.toFixed(2) + " min", mins > 0 ? money(r.cost_usd / mins) + " / min" : "—", r.cost_usd]);
    } else if (kind === "embedding") {
      out.push(["Input tokens", int(r.prompt_tokens), perMillion(r.cost_usd, r.prompt_tokens), r.cost_usd]);
    } else {
      var uncached = Math.max(0, r.prompt_tokens - r.cached_tokens - r.cache_write_tokens);
      out.push(["Input", int(uncached) + " tokens", perMillion(r.input_cost_usd, uncached), r.input_cost_usd]);
      if (r.cached_tokens > 0) {
        out.push(["Cached input", int(r.cached_tokens) + " tokens", perMillion(r.cached_cost_usd, r.cached_tokens), r.cached_cost_usd]);
      }
      if (r.cache_write_tokens > 0) {
        out.push(["Cache writes", int(r.cache_write_tokens) + " tokens", perMillion(r.cache_write_cost_usd, r.cache_write_tokens), r.cache_write_cost_usd]);
      }
      out.push(["Output", int(r.completion_tokens) + " tokens", perMillion(r.output_cost_usd, r.completion_tokens), r.output_cost_usd]);
    }
    return out;
  }

  // True for chat rows logged before itemised costs were stored: they have
  // tokens and a total, but every component is zero.
  function lacksBreakdown(r) {
    var kind = r.kind || "";
    if (kind !== "" && kind !== "chat") return false;
    var parts = r.input_cost_usd + r.cached_cost_usd + r.cache_write_cost_usd + r.output_cost_usd;
    return !r.cache_hit && r.cost_usd > 0 && parts === 0;
  }

  function note(cls, text) {
    return el("div", "cd-note " + cls, text);
  }

  function notes(r) {
    var out = [];
    if (r.cache_hit) {
      out.push(note("good", "Served from FitGuard's response cache: nothing was sent to the provider, so this cost $0."));
    }
    if (r.tool_uses > 0) {
      out.push(note("warn",
        "This request used " + r.tool_uses + " server-side tool call" + (r.tool_uses > 1 ? "s" : "") +
        " (such as web search). The provider bills those separately, per use, and that fee is not included here, so the cost shown is a minimum."));
    }
    if (r.cached_tokens > 0) {
      var uncached = r.prompt_tokens - r.cached_tokens - r.cache_write_tokens;
      if (uncached > 0 && r.input_cost_usd > 0) {
        var saved = r.cached_tokens * (r.input_cost_usd / uncached) - r.cached_cost_usd;
        if (saved > 0) out.push(note("good", "The provider's prompt cache saved about " + money(saved) + " on this request."));
      }
    }
    if (r.finish_reason === "length") {
      out.push(note("warn", "The response was cut off at the token limit. Repeated, this is a common sign of a runaway generation loop."));
    }
    if (lacksBreakdown(r)) {
      out.push(note("muted", "An itemised breakdown wasn't recorded for requests logged before this feature existed, so only the total is known."));
    }
    return out;
  }

  function meta(r) {
    var items = [
      ["Provider", r.provider || "unknown"],
      ["Model", r.model],
    ];
    // A batch runs for hours and is logged when it settles, so it has no
    // request latency to show.
    if (r.kind !== "batch") items.push(["Latency", int(r.latency_ms) + " ms"]);
    if (r.status_code !== undefined) items.push(["Status", String(r.status_code)]);
    if (r.finish_reason) items.push(["Finish reason", r.finish_reason]);
    items.push(["Tokens", int(r.prompt_tokens) + " in / " + int(r.completion_tokens) + " out"]);
    var dl = el("dl", "cd-meta");
    items.forEach(function (kv) {
      dl.appendChild(el("dt", null, kv[0]));
      dl.appendChild(el("dd", null, kv[1]));
    });
    return dl;
  }

  function breakdownTable(r) {
    var table = el("table", "cd-table");
    var head = el("tr");
    ["Item", "Quantity", "Rate billed", "Cost"].forEach(function (h, i) {
      head.appendChild(el("th", i === 0 ? "" : "num", h));
    });
    table.appendChild(el("thead")).appendChild(head);
    var body = el("tbody");
    lines(r).forEach(function (l) {
      var tr = el("tr");
      tr.appendChild(el("td", null, l[0]));
      tr.appendChild(el("td", "num", l[1]));
      tr.appendChild(el("td", "num", l[2]));
      tr.appendChild(el("td", "num", money(l[3])));
      body.appendChild(tr);
    });
    var total = el("tr", "cd-total");
    total.appendChild(el("td", null, "Total"));
    total.appendChild(el("td", "num", ""));
    total.appendChild(el("td", "num", ""));
    total.appendChild(el("td", "num", money(r.cost_usd)));
    body.appendChild(total);
    table.appendChild(body);
    return table;
  }

  function panel(r) {
    var wrap = el("div", "cd-panel");
    wrap.appendChild(el("div", "cd-title", "Where this cost came from"));
    if (!r.cache_hit && !lacksBreakdown(r)) wrap.appendChild(breakdownTable(r));
    notes(r).forEach(function (n) {
      wrap.appendChild(n);
    });
    wrap.appendChild(meta(r));
    return wrap;
  }

  function show(tr, r, cols) {
    var d = el("tr", "detail-row");
    var td = el("td");
    td.colSpan = cols;
    td.appendChild(panel(r));
    d.appendChild(td);
    tr.after(d);
    tr._detail = d;
    tr.setAttribute("aria-expanded", "true");
  }

  function hide(tr) {
    if (tr._detail) tr._detail.remove();
    tr._detail = null;
    tr.setAttribute("aria-expanded", "false");
  }

  function attach(tr, r, cols) {
    tr.classList.add("expandable");
    tr.tabIndex = 0;
    tr.setAttribute("role", "button");
    tr.setAttribute("aria-expanded", "false");

    function toggle() {
      if (tr._detail) {
        open.delete(r.id);
        hide(tr);
      } else {
        open.add(r.id);
        show(tr, r, cols);
      }
    }
    tr.addEventListener("click", toggle);
    tr.addEventListener("keydown", function (e) {
      if (e.key === "Enter" || e.key === " ") {
        e.preventDefault();
        toggle();
      }
    });
    // The row is being re-created by a refresh; reopen it if it was open.
    // Called before the row is in the DOM, so the detail row is inserted
    // by the caller's append order: see afterInsert.
    tr._wantOpen = open.has(r.id);
    tr._row = r;
    tr._cols = cols;
  }

  // afterInsert opens rows that were open before a refresh. Separate from
  // attach() because tr.after() needs the row already in the table.
  function afterInsert(tr) {
    if (tr._wantOpen && !tr._detail) show(tr, tr._row, tr._cols);
  }

  window.CostDetail = { attach: attach, afterInsert: afterInsert };
})();
