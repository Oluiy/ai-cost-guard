// AI Guard dashboard login screen. No framework, no build step, same
// conventions as dashboard.js.
(function () {
  "use strict";

  initTheme();

  var form = document.getElementById("login-form");
  var errorEl = document.getElementById("login-error");
  var submitBtn = document.getElementById("login-submit");

  form.addEventListener("submit", function (e) {
    e.preventDefault();
    setError("");
    submitBtn.disabled = true;

    fetch("/dashboard/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        username: document.getElementById("login-username").value,
        password: document.getElementById("login-password").value,
      }),
    })
      .then(function (res) {
        if (res.ok) {
          window.location.href = "/dashboard";
          return;
        }
        return res.json().then(function (body) {
          throw new Error((body.error && body.error.message) || "Login failed");
        });
      })
      .catch(function (err) {
        setError(err.message || "Login failed. Try again.");
        submitBtn.disabled = false;
      });
  });

  function setError(msg) {
    errorEl.textContent = msg;
    errorEl.classList.toggle("visible", !!msg);
  }

  // Mirrors dashboard.js's initTheme exactly (toggle-only, no
  // persistence) — this page and the dashboard should behave the same
  // way, and the dashboard doesn't persist the choice either.
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
})();
