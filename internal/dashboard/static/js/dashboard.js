// AI Guard dashboard client. No framework, no build step — this is loaded
// as a plain <script> alongside dashboard.css. Split into named functions
// by concern (chart, tables, filters, nav, account, live connection) so
// any one piece is easy to find and change without reading the whole file.
(function () {
  "use strict";

  var STORAGE_KEY = "ai-guard-dashboard-filters";
  var tooltip = document.getElementById("tooltip");
  var state = loadFilters();
  var eventSource = null;

  // Requests-view pagination — not persisted, always starts fresh: a
  // stale offset from a previous session pointing past the end of a now-
  // smaller filtered set would just render an empty page.
  var requestsOffset = 0;
  var requestsLimit = 25;
  var requestsTotal = 0;

  initTheme();
  initFilters();
  initNav();
  initAccount();
  initLoadMore();
  initReport();
  initialLoad();
  reconnect();
  window.addEventListener("resize", function () {
    if (state.view !== "requests") {
      fetchSnapshot().then(function (s) {
        if (s) renderChart(s.timeseries || []);
      });
    }
  });

  // ---------- Formatting ----------

  function fmtUSD(v) {
    if (v < 0.01 && v > 0) return "$" + v.toFixed(4);
    return "$" + v.toFixed(2);
  }
  function fmtInt(v) {
    return Number(v).toLocaleString();
  }
  function fmtPct(v) {
    return Math.round(v * 100) + "%";
  }
  function fmtHour(iso) {
    return new Date(iso).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  }
  function fmtDay(iso) {
    return new Date(iso).toLocaleDateString([], { month: "short", day: "numeric" });
  }
  function fmtTime(iso) {
    return new Date(iso).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
  }
  // Time-only (fmtTime) is ambiguous once the range spans more than a
  // day — two requests 24h apart at the same time of day are otherwise
  // indistinguishable in a table row. Falls back to date+time beyond
  // "today", same threshold the chart already uses for its own labels.
  function fmtRequestTime(iso) {
    var isTodayOnly = state.range === "today" && !(state.reportFrom && state.reportTo);
    if (isTodayOnly) return fmtTime(iso);
    var d = new Date(iso);
    return (
      d.toLocaleDateString([], { month: "short", day: "numeric" }) +
      ", " +
      d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })
    );
  }

  // ---------- Filters: range + user + view, persisted across reloads ----------

  function loadFilters() {
    var defaults;
    try {
      var saved = JSON.parse(localStorage.getItem(STORAGE_KEY) || "{}");
      defaults = {
        range: saved.range || "today",
        user: saved.user || "",
        view: saved.view === "requests" ? "requests" : "overview",
        model: "",
        status: "",
        sort: "time",
        reportFrom: "",
        reportTo: "",
      };
    } catch (e) {
      defaults = { range: "today", user: "", view: "overview", model: "", status: "", sort: "time", reportFrom: "", reportTo: "" };
    }
    // A URL carrying its own filters (a shared/bookmarked link) takes
    // priority over whatever's saved in this browser's local storage —
    // otherwise opening a teammate's link would just show your own last
    // session instead of the view they meant to share.
    return applyURLParams(defaults);
  }

  function applyURLParams(filters) {
    var params = new URLSearchParams(window.location.search);
    if (params.has("view")) filters.view = params.get("view") === "requests" ? "requests" : "overview";
    if (params.has("range")) filters.range = params.get("range");
    if (params.has("user")) filters.user = params.get("user");
    if (params.has("model")) filters.model = params.get("model");
    if (params.has("status")) filters.status = params.get("status");
    if (params.has("sort")) filters.sort = params.get("sort");
    if (params.has("from") && params.has("to")) {
      filters.reportFrom = params.get("from");
      filters.reportTo = params.get("to");
    }
    return filters;
  }

  // Mirrors the current filters into the URL (via replaceState, so every
  // filter tweak doesn't spam the browser's back button) so the page can
  // be bookmarked or handed to a teammate exactly as it looks right now.
  function syncURL() {
    var params = new URLSearchParams();
    if (state.view === "requests") params.set("view", "requests");
    if (state.reportFrom && state.reportTo) {
      params.set("from", state.reportFrom);
      params.set("to", state.reportTo);
    } else if (state.range) {
      params.set("range", state.range);
    }
    if (state.user) params.set("user", state.user);
    if (state.model) params.set("model", state.model);
    if (state.status) params.set("status", state.status);
    if (state.sort && state.sort !== "time") params.set("sort", state.sort);
    var qs = params.toString();
    history.replaceState(null, "", window.location.pathname + (qs ? "?" + qs : ""));
  }

  function saveFilters() {
    localStorage.setItem(STORAGE_KEY, JSON.stringify({ range: state.range, user: state.user, view: state.view }));
    syncURL();
  }

  function initFilters() {
    var rangePicker = document.getElementById("range-picker");
    rangePicker.querySelectorAll("button").forEach(function (btn) {
      btn.classList.toggle("active", btn.dataset.range === state.range);
      btn.addEventListener("click", function () {
        state.range = btn.dataset.range;
        rangePicker.querySelectorAll("button").forEach(function (b) {
          b.classList.toggle("active", b === btn);
        });
        clearReport(); // a preset range and a custom report window are mutually exclusive
        saveFilters();
        refresh();
      });
    });

    var userPicker = document.getElementById("user-picker");
    userPicker.addEventListener("change", function () {
      state.user = userPicker.value;
      saveFilters();
      refresh();
    });

    var modelPicker = document.getElementById("model-picker");
    modelPicker.addEventListener("change", function () {
      state.model = modelPicker.value;
      syncURL();
      loadRequests(true);
    });

    var statusPicker = document.getElementById("status-picker");
    statusPicker.addEventListener("change", function () {
      state.status = statusPicker.value;
      syncURL();
      loadRequests(true);
    });

    var sortPicker = document.getElementById("sort-picker");
    sortPicker.addEventListener("change", function () {
      state.sort = sortPicker.value;
      syncURL();
      loadRequests(true);
    });
  }

  // Populates the user dropdown from the snapshot's all_users list without
  // discarding the current selection if it's still valid.
  function syncUserOptions(allUsers) {
    var picker = document.getElementById("user-picker");
    var current = state.user;
    picker.innerHTML = '<option value="">All users</option>';
    (allUsers || []).forEach(function (u) {
      var opt = document.createElement("option");
      opt.value = u;
      opt.textContent = u;
      picker.appendChild(opt);
    });
    picker.value = allUsers && allUsers.indexOf(current) !== -1 ? current : "";
    if (picker.value !== current) {
      state.user = picker.value;
      saveFilters();
    }
  }

  function syncModelOptions(allModels) {
    var picker = document.getElementById("model-picker");
    var current = state.model;
    picker.innerHTML = '<option value="">All models</option>';
    (allModels || []).forEach(function (m) {
      var opt = document.createElement("option");
      opt.value = m;
      opt.textContent = m;
      picker.appendChild(opt);
    });
    picker.value = allModels && allModels.indexOf(current) !== -1 ? current : "";
    state.model = picker.value;
  }

  function refresh() {
    // The live connection always tracks the current range/user filter,
    // even while the Requests view is active and the SSE-driven overview
    // data isn't visible — otherwise switching back to Dashboard would
    // show stale, wrongly-filtered data until the next filter change.
    if (eventSource) eventSource.close();
    reconnect();

    if (state.view === "requests") {
      loadRequests(true);
    } else {
      fetchSnapshot().then(function (s) {
        if (s) render(s);
      });
    }
  }

  function queryString() {
    var params = new URLSearchParams();
    if (state.range) params.set("range", state.range);
    if (state.user) params.set("user", state.user);
    var s = params.toString();
    return s ? "?" + s : "";
  }

  function fetchSnapshot() {
    return fetch("/dashboard/api/data" + queryString())
      .then(function (r) {
        return r.json();
      })
      .catch(function () {
        return null;
      });
  }

  // Static <select>s (status, sort) aren't populated from a server
  // response the way user/model are, so nothing else sets their DOM
  // value to match a filter restored from the URL or a fresh default —
  // do that once, up front, rather than leaving them showing "All
  // statuses"/"Newest first" while state.status/state.sort say otherwise.
  function restoreStaticPickers() {
    document.getElementById("status-picker").value = state.status || "";
    document.getElementById("sort-picker").value = state.sort || "time";
  }

  function initialLoad() {
    restoreStaticPickers();
    fetchSnapshot().then(function (s) {
      if (s) render(s);
    });
    if (state.view === "requests") {
      switchView("requests");
    }
    // A shared/bookmarked report link (?from=&to=) should reproduce the
    // full report — summary panel and CSV button included, not just the
    // underlying table filter — so whoever opens it sees the same thing
    // the sender did.
    if (state.reportFrom && state.reportTo) {
      document.getElementById("report-from").value = state.reportFrom;
      document.getElementById("report-to").value = state.reportTo;
      document.querySelectorAll("#range-picker button").forEach(function (b) {
        b.classList.remove("active");
      });
      runReport();
    }
  }

  // ---------- Sidebar nav: Dashboard / Requests ----------

  function initNav() {
    document.querySelectorAll(".nav-item[data-view]").forEach(function (btn) {
      btn.addEventListener("click", function () {
        switchView(btn.dataset.view);
      });
    });
    document.querySelectorAll("[data-view-link]").forEach(function (btn) {
      btn.addEventListener("click", function () {
        switchView(btn.dataset.viewLink);
      });
    });
  }

  function switchView(view) {
    state.view = view;
    saveFilters();

    document.querySelectorAll(".nav-item[data-view]").forEach(function (btn) {
      btn.classList.toggle("active", btn.dataset.view === view);
    });
    document.getElementById("view-overview").hidden = view !== "overview";
    document.getElementById("view-requests").hidden = view !== "requests";
    document.getElementById("model-picker").hidden = view !== "requests";
    document.getElementById("status-picker").hidden = view !== "requests";
    document.getElementById("sort-picker").hidden = view !== "requests";
    document.getElementById("view-title").textContent = view === "requests" ? "All requests" : "Cost dashboard";

    if (view === "requests") {
      loadRequests(true);
    } else {
      fetchSnapshot().then(function (s) {
        if (s) render(s);
      });
    }
  }

  // ---------- Chart ----------

  function renderChart(rows) {
    var svg = document.getElementById("chart");
    var width = svg.clientWidth || 1000;
    var height = 220;
    var padLeft = 48,
      padRight = 8,
      padTop = 14,
      padBottom = 22;
    var plotW = width - padLeft - padRight;
    var plotH = height - padTop - padBottom;

    svg.setAttribute("viewBox", "0 0 " + width + " " + height);
    while (svg.firstChild) svg.removeChild(svg.firstChild);
    if (!rows.length) return;

    var isDaily = state.range !== "today";
    var fmtBucket = isDaily ? fmtDay : fmtHour;

    var max = 0;
    rows.forEach(function (r) {
      if (r.cost_usd > max) max = r.cost_usd;
    });
    if (max <= 0) max = 0.01;

    var n = rows.length;
    var slot = plotW / n;
    var barW = Math.min(24, slot * 0.6);
    var gap = 2;
    var ns = "http://www.w3.org/2000/svg";

    // Y-axis context (just the floor and the scale's ceiling) — without
    // this, an empty-looking chart and a $500/day chart render as the
    // exact same shape, since bars are always scaled to fill the plot.
    var maxLabel = document.createElementNS(ns, "text");
    maxLabel.setAttribute("class", "axis-value-label");
    maxLabel.setAttribute("x", padLeft - 8);
    maxLabel.setAttribute("y", padTop + 4);
    maxLabel.setAttribute("text-anchor", "end");
    maxLabel.textContent = fmtUSD(max);
    svg.appendChild(maxLabel);

    var zeroLabel = document.createElementNS(ns, "text");
    zeroLabel.setAttribute("class", "axis-value-label");
    zeroLabel.setAttribute("x", padLeft - 8);
    zeroLabel.setAttribute("y", padTop + plotH);
    zeroLabel.setAttribute("text-anchor", "end");
    zeroLabel.textContent = "$0";
    svg.appendChild(zeroLabel);

    var baseline = document.createElementNS(ns, "line");
    baseline.setAttribute("class", "baseline");
    baseline.setAttribute("x1", padLeft);
    baseline.setAttribute("x2", width - padRight);
    baseline.setAttribute("y1", padTop + plotH);
    baseline.setAttribute("y2", padTop + plotH);
    svg.appendChild(baseline);

    // Sparser labels as the bucket count grows, so a 30-day view doesn't
    // print 30 overlapping date labels.
    var labelEvery = n > 20 ? Math.ceil(n / 8) : 4;

    rows.forEach(function (r, i) {
      var cx = padLeft + slot * i + slot / 2;
      var hasSpend = r.cost_usd > 0;
      var h = hasSpend ? Math.max(2, (r.cost_usd / max) * (plotH - 4)) : 0;
      var y = padTop + plotH - h;
      var rect = null;

      // Buckets with no spend draw no bar at all, rather than a 2px
      // rounded pill sitting on the baseline — across a day's worth of
      // hourly buckets, those pills read as visual noise/clutter, not
      // "zero." The hit-target below still covers the space so hovering
      // an empty hour shows "$0.00 · 0 req" in the tooltip.
      if (hasSpend) {
        rect = document.createElementNS(ns, "rect");
        rect.setAttribute("class", "bar");
        rect.setAttribute("x", cx - barW / 2 + gap / 2);
        rect.setAttribute("y", y);
        rect.setAttribute("width", Math.max(1, barW - gap));
        rect.setAttribute("height", h);
        var radius = Math.min(3, h / 2);
        rect.setAttribute("rx", radius);
        rect.setAttribute("ry", radius);
        svg.appendChild(rect);
      }

      var hit = document.createElementNS(ns, "rect");
      hit.setAttribute("class", "hit-target");
      hit.setAttribute("x", cx - slot / 2);
      hit.setAttribute("y", padTop);
      hit.setAttribute("width", slot);
      hit.setAttribute("height", plotH);
      hit.tabIndex = 0;
      hit.addEventListener("pointermove", function (e) {
        if (rect) rect.classList.add("hover");
        showTooltip(
          e.clientX,
          e.clientY,
          '<div class="tt-value">' + fmtUSD(r.cost_usd) + "</div>" +
            "<div>" + fmtBucket(r.hour) + " &middot; " + fmtInt(r.requests) + " req</div>"
        );
      });
      hit.addEventListener("pointerleave", function () {
        if (rect) rect.classList.remove("hover");
        hideTooltip();
      });
      hit.addEventListener("focus", function () {
        showTooltip(cx, padTop + 40, '<div class="tt-value">' + fmtUSD(r.cost_usd) + "</div><div>" + fmtBucket(r.hour) + "</div>");
      });
      hit.addEventListener("blur", hideTooltip);
      svg.appendChild(hit);

      if (i % labelEvery === 0 || i === n - 1) {
        var label = document.createElementNS(ns, "text");
        label.setAttribute("class", "axis-label");
        label.setAttribute("x", cx);
        label.setAttribute("y", height - 6);
        label.setAttribute("text-anchor", "middle");
        label.textContent = fmtBucket(r.hour);
        svg.appendChild(label);
      }
    });
  }

  function showTooltip(x, y, html) {
    tooltip.innerHTML = "";
    var node = document.createElement("div");
    node.innerHTML = html; // built from trusted, escaped fragments only
    tooltip.appendChild(node);
    tooltip.style.display = "block";
    tooltip.style.left = Math.min(x + 12, window.innerWidth - 180) + "px";
    tooltip.style.top = Math.max(y - 40, 8) + "px";
  }

  function hideTooltip() {
    tooltip.style.display = "none";
  }

  // ---------- Tiles + tables (overview) ----------

  function renderTiles(today) {
    document.getElementById("tile-spend").textContent = fmtUSD(today.total_cost_usd);
    document.getElementById("tile-requests").textContent = fmtInt(today.requests);
    document.getElementById("tile-cache").textContent = fmtPct(today.cache_hit_rate);
    document.getElementById("tile-users").textContent = fmtInt(today.active_users);

    // A low cache hit rate is wasted spend, worth flagging at a glance.
    // No signal yet on a day with zero requests to judge against.
    var cacheTile = document.getElementById("tile-cache-wrap");
    if (today.requests === 0) {
      cacheTile.removeAttribute("data-level");
    } else if (today.cache_hit_rate >= 0.5) {
      cacheTile.setAttribute("data-level", "good");
    } else if (today.cache_hit_rate >= 0.2) {
      cacheTile.setAttribute("data-level", "warning");
    } else {
      cacheTile.setAttribute("data-level", "critical");
    }
  }

  function renderUsers(users) {
    var tbody = document.querySelector("#users-table tbody");
    tbody.innerHTML = "";
    document.getElementById("users-empty").style.display = users.length ? "none" : "block";
    users.forEach(function (u) {
      var tr = document.createElement("tr");
      addCell(tr, u.user_id, "primary");
      addCell(tr, fmtInt(u.requests), "num");
      addCell(tr, fmtInt(u.cache_hits), "num");
      addCell(tr, fmtUSD(u.total_cost_usd), "num");
      tbody.appendChild(tr);
    });
  }

  function renderTop(rows) {
    var tbody = document.querySelector("#top-table tbody");
    tbody.innerHTML = "";
    document.getElementById("top-empty").style.display = rows.length ? "none" : "block";
    rows.forEach(function (r) {
      var tr = document.createElement("tr");
      addCell(tr, fmtRequestTime(r.timestamp));
      addCell(tr, r.user_id);
      var modelTd = document.createElement("td");
      modelTd.textContent = r.model;
      if (r.finish_reason === "length") {
        var badge = document.createElement("span");
        badge.className = "badge";
        badge.textContent = "truncated";
        badge.style.marginLeft = "6px";
        modelTd.appendChild(badge);
      }
      tr.appendChild(modelTd);
      addCell(tr, fmtInt(r.prompt_tokens + r.completion_tokens), "num");
      addCell(tr, fmtInt(r.latency_ms) + "ms", "num");
      addCell(tr, fmtUSD(r.cost_usd), "num");
      tbody.appendChild(tr);
    });
  }

  function addCell(tr, text, cls) {
    var td = document.createElement("td");
    if (cls) td.className = cls;
    td.textContent = text;
    tr.appendChild(td);
  }

  function updateSectionTitles(rangeLabel, userLabel) {
    var suffix = userLabel ? " · " + userLabel : "";
    document.getElementById("chart-title").textContent = "Spend " + (state.range === "today" ? "by hour" : "by day") + " (" + rangeLabel + ")" + suffix;
    document.getElementById("users-title").textContent = "Spend by user (" + rangeLabel + ")";
    document.getElementById("top-title").textContent = "Top expensive requests (" + rangeLabel + ")" + suffix;
  }

  var RANGE_LABELS = { today: "today", "7d": "last 7 days", "30d": "last 30 days" };

  function render(snap) {
    syncUserOptions(snap.all_users);
    updateSectionTitles(RANGE_LABELS[snap.range] || "today", snap.user);
    renderTiles(snap.today);
    renderChart(snap.timeseries || []);
    renderUsers(snap.users || []);
    renderTop(snap.top || []);
  }

  // ---------- Requests view: full paginated, filterable log ----------

  function loadRequests(reset) {
    if (reset) requestsOffset = 0;

    var params = new URLSearchParams();
    // A custom report window (both dates set) takes over from the
    // Today/7d/30d preset entirely — they're mutually exclusive ways of
    // specifying the same thing, and clicking a preset already clears
    // the custom dates (see clearReport()).
    if (state.reportFrom && state.reportTo) {
      params.set("from", state.reportFrom);
      params.set("to", state.reportTo);
    } else if (state.range) {
      params.set("range", state.range);
    }
    if (state.user) params.set("user", state.user);
    if (state.model) params.set("model", state.model);
    if (state.status) params.set("status", state.status);
    if (state.sort) params.set("sort", state.sort);
    params.set("limit", requestsLimit);
    params.set("offset", requestsOffset);

    var btn = document.getElementById("load-more-btn");
    btn.disabled = true;

    fetch("/dashboard/api/requests?" + params.toString())
      .then(function (r) {
        return r.json();
      })
      .then(function (page) {
        if (!page) return;
        syncModelOptions(page.all_models);
        renderRequests(page.requests || [], reset);
        requestsTotal = page.total || 0;
        requestsOffset += (page.requests || []).length;

        var count = document.getElementById("requests-count");
        count.textContent = requestsTotal
          ? "Showing " + fmtInt(Math.min(requestsOffset, requestsTotal)) + " of " + fmtInt(requestsTotal) +
            " · " + fmtUSD(page.total_cost_usd || 0) + " total"
          : "";

        var hasMore = requestsOffset < requestsTotal;
        btn.hidden = !hasMore;
        btn.disabled = !hasMore;
        document.getElementById("requests-empty").style.display = requestsTotal ? "none" : "block";
      })
      .catch(function () {
        btn.disabled = false;
      });
  }

  function renderRequests(rows, reset) {
    var tbody = document.querySelector("#requests-table tbody");
    if (reset) tbody.innerHTML = "";
    rows.forEach(function (r) {
      var tr = document.createElement("tr");
      addCell(tr, fmtRequestTime(r.timestamp));
      addCell(tr, r.user_id);

      var modelTd = document.createElement("td");
      modelTd.textContent = r.model;
      if (r.finish_reason === "length") {
        var truncBadge = document.createElement("span");
        truncBadge.className = "badge";
        truncBadge.textContent = "truncated";
        truncBadge.style.marginLeft = "6px";
        modelTd.appendChild(truncBadge);
      }
      tr.appendChild(modelTd);

      var statusTd = document.createElement("td");
      var statusBadge = document.createElement("span");
      var isError = r.status_code >= 400;
      statusBadge.className = "badge " + (isError ? "error" : "ok");
      statusBadge.textContent = String(r.status_code);
      statusTd.appendChild(statusBadge);
      tr.appendChild(statusTd);

      var cacheTd = document.createElement("td");
      var cacheBadge = document.createElement("span");
      cacheBadge.className = "badge " + (r.cache_hit ? "hit" : "miss");
      cacheBadge.textContent = r.cache_hit ? "hit" : "miss";
      cacheTd.appendChild(cacheBadge);
      tr.appendChild(cacheTd);

      addCell(tr, fmtInt(r.prompt_tokens + r.completion_tokens), "num");
      addCell(tr, fmtInt(r.latency_ms) + "ms", "num");
      addCell(tr, fmtUSD(r.cost_usd), "num");
      tbody.appendChild(tr);
    });
  }

  function initLoadMore() {
    document.getElementById("load-more-btn").addEventListener("click", function () {
      loadRequests(false);
    });
  }

  // ---------- Custom report: arbitrary date range, summary + CSV export ----------

  function initReport() {
    document.getElementById("report-run-btn").addEventListener("click", runReport);
    document.getElementById("report-clear-btn").addEventListener("click", function () {
      clearReport();
      saveFilters();
      refresh();
    });
    document.getElementById("report-csv-btn").addEventListener("click", function () {
      if (!state.reportFrom || !state.reportTo) return;
      window.location.href = "/dashboard/api/report?" + reportQueryString() + "&format=csv";
    });
  }

  function reportQueryString() {
    var params = new URLSearchParams();
    params.set("from", document.getElementById("report-from").value);
    params.set("to", document.getElementById("report-to").value);
    if (state.user) params.set("user", state.user);
    if (state.model) params.set("model", state.model);
    if (state.status) params.set("status", state.status);
    return params.toString();
  }

  function setReportError(msg) {
    var el = document.getElementById("report-error");
    el.textContent = msg;
    el.classList.toggle("visible", !!msg);
  }

  function runReport() {
    var from = document.getElementById("report-from").value;
    var to = document.getElementById("report-to").value;
    setReportError("");

    if (!from || !to) {
      setReportError("Pick both a start and an end date.");
      return;
    }
    if (from > to) {
      setReportError("The start date has to be before the end date.");
      return;
    }

    fetch("/dashboard/api/report?" + reportQueryString())
      .then(function (r) {
        return r.json().then(function (body) {
          return { ok: r.ok, body: body };
        });
      })
      .then(function (result) {
        if (!result.ok) {
          setReportError((result.body.error && result.body.error.message) || "Couldn't generate the report.");
          return;
        }

        state.reportFrom = from;
        state.reportTo = to;
        syncURL();

        // A custom window replaces the preset range picker — visually
        // deselect it so it's clear which one is actually driving the
        // table below.
        document.querySelectorAll("#range-picker button").forEach(function (b) {
          b.classList.remove("active");
        });

        document.getElementById("report-requests").textContent = fmtInt(result.body.requests);
        document.getElementById("report-spend").textContent = fmtUSD(result.body.total_cost_usd);
        document.getElementById("report-cache").textContent = fmtPct(result.body.cache_hit_rate);
        document.getElementById("report-summary").hidden = false;
        document.getElementById("report-clear-btn").hidden = false;
        document.getElementById("report-csv-btn").disabled = false;

        loadRequests(true);
      })
      .catch(function () {
        setReportError("Couldn't reach the server. Try again.");
      });
  }

  function clearReport() {
    state.reportFrom = "";
    state.reportTo = "";
    setReportError("");
    document.getElementById("report-summary").hidden = true;
    document.getElementById("report-clear-btn").hidden = true;
    document.getElementById("report-csv-btn").disabled = true;
  }

  // ---------- Live connection (overview only) ----------

  function setLive(on) {
    document.getElementById("live-dot").classList.toggle("off", !on);
    document.getElementById("live-text").textContent = on ? "live" : "reconnecting…";
  }

  function reconnect() {
    eventSource = new EventSource("/dashboard/events" + queryString());
    eventSource.onopen = function () {
      setLive(true);
    };
    eventSource.onerror = function () {
      setLive(false);
    };
    eventSource.onmessage = function (e) {
      try {
        render(JSON.parse(e.data));
      } catch (err) {
        /* ignore malformed frame */
      }
    };
  }

  // ---------- Theme ----------

  function initTheme() {
    var btn = document.getElementById("theme-toggle");
    btn.addEventListener("click", function () {
      var root = document.documentElement;
      var current = root.getAttribute("data-theme");
      if (current === "dark") root.setAttribute("data-theme", "light");
      else if (current === "light") root.removeAttribute("data-theme");
      else root.setAttribute("data-theme", "dark");
    });
  }

  // ---------- Account: who's logged in, log out ----------

  function doLogout() {
    fetch("/dashboard/logout", { method: "POST" }).then(function () {
      window.location.href = "/dashboard";
    });
  }

  function initAccount() {
    fetch("/dashboard/api/whoami")
      .then(function (r) {
        return r.json();
      })
      .then(function (info) {
        if (!info || !info.auth_enabled || !info.username) return;

        var block = document.getElementById("account-block");
        block.hidden = false;
        document.getElementById("account-username").textContent = info.username;
        document.getElementById("account-avatar").textContent = info.username.charAt(0);

        var trigger = document.getElementById("account-trigger");
        var menu = document.getElementById("account-menu");
        trigger.addEventListener("click", function (e) {
          e.stopPropagation();
          menu.hidden = !menu.hidden;
        });
        document.addEventListener("click", function () {
          menu.hidden = true;
        });

        document.getElementById("logout-btn").addEventListener("click", doLogout);

        // Same action, also reachable straight from the sidebar — no
        // login is configured in single-tenant mode, so this stays
        // hidden (matching the header account block) rather than
        // offering a logout with nothing to log out of.
        var sidebarLogout = document.getElementById("sidebar-logout-btn");
        sidebarLogout.hidden = false;
        sidebarLogout.addEventListener("click", doLogout);
      })
      .catch(function () {
        /* no dashboard login configured, or not reachable — leave the account block hidden */
      });
  }
})();
