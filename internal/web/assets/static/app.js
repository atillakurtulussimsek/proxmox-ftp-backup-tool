// PVE FTP Yedek — küçük arayüz yardımcıları (CSP: yalnızca 'self' betikleri).
(function () {
  "use strict";

  // ---- VM tablosu filtresi
  var filter = "all";
  var query = "";

  function applyFilter() {
    var table = document.querySelector("table.vms");
    if (!table) return;
    var shown = 0;
    table.querySelectorAll("tbody tr").forEach(function (tr) {
      var ok = true;
      if (filter === "sel") ok = tr.hasAttribute("data-sel");
      if (filter === "issue") ok = tr.hasAttribute("data-issue");
      if (ok && query) ok = (tr.getAttribute("data-search") || "").toLocaleLowerCase("tr").indexOf(query) !== -1;
      tr.hidden = !ok;
      if (ok) shown++;
    });
    var none = document.querySelector("[data-no-match]");
    if (none) none.hidden = shown !== 0;
  }

  function updateCounts() {
    var box = document.querySelector("[data-counts]");
    document.querySelectorAll(".chip[data-filter]").forEach(function (chip) {
      var n = chip.querySelector(".n");
      if (!box) { if (n) n.remove(); return; }
      if (!n) { n = document.createElement("span"); n.className = "n"; chip.appendChild(n); }
      n.textContent = box.getAttribute("data-" + chip.getAttribute("data-filter")) || "0";
    });
  }

  document.addEventListener("click", function (e) {
    var chip = e.target.closest(".chip[data-filter]");
    if (chip) {
      filter = chip.getAttribute("data-filter");
      document.querySelectorAll(".chip[data-filter]").forEach(function (c) {
        c.classList.toggle("on", c === chip);
        c.setAttribute("aria-pressed", c === chip ? "true" : "false");
      });
      applyFilter();
      return;
    }
    var x = e.target.closest("[data-dismiss]");
    if (x) x.closest(".toast").remove();
  });

  document.addEventListener("input", function (e) {
    if (e.target.matches("[data-filter-search]")) {
      query = e.target.value.trim().toLocaleLowerCase("tr");
      applyFilter();
    }
  });

  // ---- çalışan iş: log sona kaydırılır, iş bitince "jobFinished" olayı yayılır
  var jobRunning = null;
  document.addEventListener("htmx:afterSwap", function (e) {
    var t = e.detail.target;
    if (!t || t.id !== "job-status") return;
    var log = t.querySelector(".job-log");
    if (log) log.scrollTop = log.scrollHeight;
    var running = !!t.querySelector(".job");
    if (jobRunning === true && !running) htmx.trigger(document.body, "jobFinished");
    jobRunning = running;
  });

  // ---- bildirimler: başarılı olanlar 6 sn sonra kapanır
  document.addEventListener("htmx:afterSwap", function (e) {
    var t = e.detail.target;
    if (t && t.id === "vm-table") { updateCounts(); applyFilter(); }
    if (t && t.id === "flash") {
      var toast = t.querySelector(".toast.ok");
      if (toast) setTimeout(function () { toast.remove(); }, 6000);
    }
  });
})();
