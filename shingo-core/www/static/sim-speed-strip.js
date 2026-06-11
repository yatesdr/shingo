// Dev-only sim speed top-strip. Injected by the layout under -tags sim only.
// Shows the current sim multiplier + sim clock and sets BOTH the core (same
// origin) and edge (:8081) sim clocks live via /api/sim/speed.
(function () {
  "use strict";

  var EDGE = location.protocol + "//" + location.hostname + ":8081";
  var PRESETS = [1, 2, 5, 10, 60, 300, 1000];

  var css = document.createElement("style");
  css.textContent = [
    "#sim-strip{position:sticky;top:0;z-index:9999;display:flex;align-items:center;gap:8px;",
    "padding:6px 14px;background:#161d29;color:#cfe3ff;border-bottom:1px solid #34435c;",
    "font:600 12px/1.5 system-ui,'Segoe UI',sans-serif}",
    "#sim-strip .tag{background:#caa11a;color:#161d29;padding:1px 7px;border-radius:3px;letter-spacing:.4px}",
    "#sim-strip .spd{color:#fff;font-size:15px;min-width:56px}",
    "#sim-strip .clk{color:#8fb3da;font-weight:500}",
    "#sim-strip button{background:#222c3c;border:1px solid #3a4a63;color:#cfe3ff;border-radius:4px;",
    "padding:3px 9px;cursor:pointer;font:inherit}",
    "#sim-strip button:hover{background:#33455f}",
    "#sim-strip button.on{background:#2d6cdf;border-color:#2d6cdf;color:#fff}",
    "#sim-strip .gap{flex:1}",
  ].join("");
  document.head.appendChild(css);

  var bar = document.createElement("div");
  bar.id = "sim-strip";
  var html = '<span class="tag">SIM</span><span>speed</span><span class="spd" id="sim-spd">—</span>';
  PRESETS.forEach(function (p) {
    html += '<button data-spd="' + p + '">' + p + "×</button>";
  });
  html += '<span class="gap"></span><span class="clk" id="sim-clk"></span>';
  bar.innerHTML = html;
  document.body.insertBefore(bar, document.body.firstChild);

  function fmt(n) { return (n >= 1 ? n : n.toFixed(2)) + "×"; }

  // j = the /api/sim/{status,speed} payload. speed is the EFFECTIVE rate the
  // clock actually runs; requested_speed is what was asked. When a crank was
  // clamped (requested > effective) we show "M× (max — asked N×)" so the readout
  // is honest about what the box can sustain.
  function render(j) {
    if (!j) return;
    var speed = j.speed, requested = j.requested_speed;
    var capped = typeof requested === "number" && typeof speed === "number" && requested > speed + 0.001;
    var s = document.getElementById("sim-spd");
    if (s && typeof speed === "number") {
      s.textContent = capped ? fmt(speed) + " (max — asked " + fmt(requested) + ")" : fmt(speed);
      s.style.color = capped ? "#ffd166" : "#fff";
      s.title = capped ? "Capped at sim.max_speed; the integration sim can't process faster" : "";
    }
    // Highlight the button the user picked (requested), even when it was capped.
    var sel = capped ? requested : speed;
    bar.querySelectorAll("button").forEach(function (b) {
      b.classList.toggle("on", parseFloat(b.dataset.spd) === sel);
    });
    if (j.sim_now) {
      var c = document.getElementById("sim-clk");
      if (c) c.textContent = "sim clock " + j.sim_now.replace("T", " ").replace("Z", " UTC");
    }
  }

  function setSpeed(n) {
    // Core: same origin — read the response to confirm and update the readout.
    fetch("/api/sim/speed?speed=" + n, { method: "POST" })
      .then(function (r) { return r.json(); })
      .then(function (j) { render(j); })
      .catch(function () {});
    // Edge: cross-origin simple POST (no body) — executes without a preflight;
    // fire-and-forget, we don't need to read the opaque response.
    fetch(EDGE + "/api/sim/speed?speed=" + n, { method: "POST", mode: "no-cors" }).catch(function () {});
  }

  function poll() {
    fetch("/api/sim/status")
      .then(function (r) { return r.json(); })
      .then(function (j) { if (j && j.has_clock) render(j); })
      .catch(function () {});
  }

  bar.addEventListener("click", function (e) {
    var b = e.target.closest("button");
    if (b) setSpeed(parseFloat(b.dataset.spd));
  });

  poll();
  setInterval(poll, 2000);
})();
