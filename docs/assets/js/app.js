// fitguard docs — shared site behavior. No framework, no build step: this
// runs as-is in every page via a plain <script> tag. Split into small
// named functions (not one big IIFE dump) so any one piece is easy to
// find and review on its own.
(function () {
  "use strict";

  initThemeToggle();
  initActiveNav();
  initSearch();
  initCodeTabs();
  initCopyButtons();

  // ---------- Theme ----------
  // Same persisted-preference pattern as the live /dashboard, so a
  // visitor's choice feels consistent whether they're reading docs or
  // looking at their own spend data.
  function initThemeToggle() {
    var STORAGE_KEY = "fitguard-docs-theme";
    var root = document.documentElement;
    var saved = localStorage.getItem(STORAGE_KEY);
    if (saved === "light" || saved === "dark") {
      root.setAttribute("data-theme", saved);
    }

    var toggle = document.querySelector("[data-theme-toggle]");
    if (!toggle) return;

    toggle.addEventListener("click", function () {
      var current = root.getAttribute("data-theme");
      var isLight =
        current === "light" ||
        (!current && matchMedia("(prefers-color-scheme: light)").matches);
      var next = isLight ? "dark" : "light";
      root.setAttribute("data-theme", next);
      localStorage.setItem(STORAGE_KEY, next);
    });
  }

  // ---------- Active nav link ----------
  function initActiveNav() {
    var path = location.pathname.replace(/\/index\.html$/, "/");
    document.querySelectorAll("nav.top-links a, .guide-sidebar a").forEach(function (link) {
      var href = link.getAttribute("href");
      if (!href) return;
      var resolved = new URL(href, location.href).pathname.replace(/\/index\.html$/, "/");
      if (resolved === path) link.classList.add("active");
    });
  }

  // ---------- Search ----------
  // FITGUARD_SEARCH_INDEX is defined in search-index.js, loaded before
  // this file. Matching is deliberately simple (substring, not fuzzy) —
  // the whole docs set is small enough that "does the title or a keyword
  // contain this" finds the right page every time.
  function initSearch() {
    var overlay = document.getElementById("search-overlay");
    var input = document.getElementById("search-input");
    var results = document.getElementById("search-results");
    if (!overlay || !input || !results) return;

    // Every entry in search-index.js is root-relative ("guide/foo.html").
    // Resolve that against however deep the current page is, once, here
    // — the index itself never needs to know where it's being loaded from.
    var siteRoot = document.body.getAttribute("data-site-root") || "./";
    var index = (window.FITGUARD_SEARCH_INDEX || []).map(function (item) {
      return Object.assign({}, item, { url: siteRoot + item.url });
    });

    function open() {
      overlay.classList.add("open");
      input.value = "";
      render(index.slice(0, 6));
      setTimeout(function () {
        input.focus();
      }, 0);
    }

    function close() {
      overlay.classList.remove("open");
    }

    function render(items) {
      if (items.length === 0) {
        results.innerHTML = '<div class="search-empty">No matches</div>';
        return;
      }
      results.innerHTML = items
        .map(function (item, i) {
          return (
            '<a href="' + item.url + '" class="' + (i === 0 ? "active" : "") + '">' +
            '<div class="r-title">' + escapeHtml(item.title) + "</div>" +
            '<div class="r-section">' + escapeHtml(item.section) + "</div>" +
            "</a>"
          );
        })
        .join("");
    }

    function search(query) {
      var q = query.trim().toLowerCase();
      if (!q) return index.slice(0, 6);
      return index.filter(function (item) {
        return (
          item.title.toLowerCase().indexOf(q) !== -1 ||
          item.section.toLowerCase().indexOf(q) !== -1 ||
          (item.keywords || "").toLowerCase().indexOf(q) !== -1
        );
      });
    }

    document.querySelectorAll("[data-search-open]").forEach(function (btn) {
      btn.addEventListener("click", open);
    });

    document.addEventListener("keydown", function (e) {
      var isCmdK = (e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k";
      if (isCmdK) {
        e.preventDefault();
        overlay.classList.contains("open") ? close() : open();
      }
      if (e.key === "Escape" && overlay.classList.contains("open")) close();
    });

    overlay.addEventListener("click", function (e) {
      if (e.target === overlay) close();
    });

    input.addEventListener("input", function () {
      render(search(input.value));
    });
  }

  function escapeHtml(s) {
    var div = document.createElement("div");
    div.textContent = s;
    return div.innerHTML;
  }

  // ---------- Code language tabs ----------
  // The chosen language (curl / python / typescript) is remembered in
  // localStorage and applied to every .code-tabs block on the page (and,
  // via the shared key, every page you visit next) — the same "pick once,
  // it sticks" pattern Stripe's docs use so you're not re-selecting your
  // language on every single page.
  //
  // Blocks that aren't picking a *language* opt into their own key with
  // data-tab-group (the install-method tabs use "install"). Without that
  // they'd share the language preference, and choosing "npm" to install
  // would silently discard someone's saved choice of Python for every
  // API example on the site.
  function initCodeTabs() {
    document.querySelectorAll(".code-tabs").forEach(function (block) {
      var group = block.getAttribute("data-tab-group") || "lang";
      var storageKey = "fitguard-docs-" + group;
      var saved = localStorage.getItem(storageKey);
      var tabs = block.querySelectorAll(".tab");
      var panels = block.querySelectorAll(".panel");

      function activate(lang) {
        tabs.forEach(function (t) {
          t.classList.toggle("active", t.dataset.lang === lang);
        });
        panels.forEach(function (p) {
          p.classList.toggle("active", p.dataset.lang === lang);
        });
      }

      tabs.forEach(function (tab) {
        tab.addEventListener("click", function () {
          activate(tab.dataset.lang);
          localStorage.setItem(storageKey, tab.dataset.lang);
        });
      });

      var initial = saved && block.querySelector('.tab[data-lang="' + saved + '"]')
        ? saved
        : tabs[0] && tabs[0].dataset.lang;
      if (initial) activate(initial);
    });
  }

  // ---------- Copy-to-clipboard on code blocks ----------
  function initCopyButtons() {
    document.querySelectorAll(".code-tabs pre, .guide-content pre").forEach(function (pre) {
      var btn = document.createElement("button");
      btn.className = "copy-btn";
      btn.type = "button";
      btn.textContent = "Copy";
      btn.addEventListener("click", function () {
        var text = pre.innerText.replace(/^Copy\n?/, "");
        navigator.clipboard.writeText(text).then(function () {
          btn.textContent = "Copied";
          btn.classList.add("copied");
          setTimeout(function () {
            btn.textContent = "Copy";
            btn.classList.remove("copied");
          }, 1400);
        });
      });
      pre.style.position = "relative";
      pre.appendChild(btn);
    });
  }
  // ---------- Dashboard screenshot carousel ----------
  // Transform-based slider (not scroll-snap): the active slide is fully
  // opaque, the rest dim, so it reads as one deliberate slide changing
  // rather than a scrollable filmstrip. Autoplays, but any manual
  // interaction (dot, arrow, swipe, or just parking the mouse over it)
  // stops it, since nothing is more annoying than a screenshot changing
  // out from under you while you're actually reading it.
  function initCarousel() {
    var carousel = document.getElementById("dashboard-carousel");
    var track = document.getElementById("carousel-track");
    if (!carousel || !track) return;
    var slides = Array.prototype.slice.call(track.querySelectorAll(".carousel-slide"));
    var dotsWrap = document.getElementById("carousel-dots");
    var dots = [];
    var index = 0;
    var autoplayTimer = null;
    var autoplayMs = 4500;

    slides.forEach(function (slide, i) {
      var dot = document.createElement("button");
      dot.type = "button";
      dot.className = "carousel-dot" + (i === 0 ? " active" : "");
      dot.setAttribute("aria-label", "Go to slide " + (i + 1));
      dot.addEventListener("click", function () {
        stopAutoplay();
        goTo(i);
      });
      dotsWrap.appendChild(dot);
      dots.push(dot);

      var img = slide.querySelector("img");
      img.addEventListener("click", function () {
        openLightbox(img.src, img.alt);
      });
    });

    function goTo(i) {
      index = (i + slides.length) % slides.length;
      track.style.transform = "translateX(-" + index * 100 + "%)";
      slides.forEach(function (s, si) {
        s.classList.toggle("is-active", si === index);
      });
      dots.forEach(function (d, di) {
        d.classList.toggle("active", di === index);
      });
    }

    function startAutoplay() {
      stopAutoplay();
      autoplayTimer = window.setInterval(function () {
        goTo(index + 1);
      }, autoplayMs);
    }
    function stopAutoplay() {
      if (autoplayTimer) window.clearInterval(autoplayTimer);
      autoplayTimer = null;
    }

    document.getElementById("carousel-prev").addEventListener("click", function () {
      stopAutoplay();
      goTo(index - 1);
    });
    document.getElementById("carousel-next").addEventListener("click", function () {
      stopAutoplay();
      goTo(index + 1);
    });
    carousel.addEventListener("mouseenter", stopAutoplay);
    carousel.addEventListener("focusin", stopAutoplay);
    carousel.addEventListener("keydown", function (e) {
      if (e.key === "ArrowLeft") {
        stopAutoplay();
        goTo(index - 1);
      } else if (e.key === "ArrowRight") {
        stopAutoplay();
        goTo(index + 1);
      }
    });

    // Basic touch swipe.
    var touchStartX = null;
    track.addEventListener("touchstart", function (e) {
      stopAutoplay();
      touchStartX = e.touches[0].clientX;
    });
    track.addEventListener("touchend", function (e) {
      if (touchStartX === null) return;
      var dx = e.changedTouches[0].clientX - touchStartX;
      if (Math.abs(dx) > 40) goTo(dx < 0 ? index + 1 : index - 1);
      touchStartX = null;
    });

    goTo(0);
    startAutoplay();
  }

  function openLightbox(src, alt) {
    var lightbox = document.getElementById("carousel-lightbox");
    if (!lightbox) return;
    var img = document.getElementById("lightbox-img");
    img.src = src;
    img.alt = alt;
    lightbox.hidden = false;
  }
  function initLightbox() {
    var lightbox = document.getElementById("carousel-lightbox");
    if (!lightbox) return;
    function close() {
      lightbox.hidden = true;
      document.getElementById("lightbox-img").src = "";
    }
    lightbox.addEventListener("click", close);
    document.getElementById("lightbox-close").addEventListener("click", function (e) {
      e.stopPropagation();
      close();
    });
    document.addEventListener("keydown", function (e) {
      if (e.key === "Escape") close();
    });
  }

  initCarousel();
  initLightbox();
})();
