import { createSSE, delegateActions, escapeHtml } from '/static/js/shingoedge.js';

(function() {
  // Debug-log rendering.
  //
  // Plain rows, matching Core's shingo-core/www/static/pages/diagnostics.js.
  // This page used to carry an Edge-only formatting layer from May 2026 — a
  // message tokenizer painting uuids, key=value pairs and error words into
  // spans, a JSON pretty-printer with per-value colouring and collapsible field
  // summaries, and run-detection that folded consecutive same-subsystem entries
  // into expandable groups. All of it was removed on 2026-08-19: it was ~380
  // lines of browser-side presentation that existed on one of the two surfaces
  // reading the same protocol/debuglog ring buffer, and the two pages showing
  // the same entries differently is a cost paid by whoever has to compare them
  // during an incident.
  //
  // The ring buffer itself is untouched and always was shared.
  var body = document.getElementById('debug-log-body');
  var wrap = document.querySelector('.debug-log-wrap');
  var autoScroll = document.getElementById('log-autoscroll');
  var filterEl = document.getElementById('log-filter');
  var maxRows = 1000;

  window.debugAppendRow = function(entry) {
    var tr = document.createElement('tr');
    tr.className = 'debug-row';
    tr.setAttribute('data-subsystem', entry.subsystem || '');
    var ts = entry.time ? new Date(entry.time) : new Date();
    var timeStr = ts.toTimeString().slice(0, 8) + '.' + String(ts.getMilliseconds()).padStart(3, '0');
    tr.innerHTML = '<td>' + timeStr + '</td><td>' + escapeHtml(entry.subsystem || '') + '</td><td>' +
      escapeHtml(entry.message || '') + '</td>';
    var f = filterEl.value;
    if (f && entry.subsystem !== f) {
      tr.style.display = 'none';
    }
    body.appendChild(tr);
    while (body.children.length > maxRows) {
      body.removeChild(body.firstChild);
    }
    if (autoScroll.checked) {
      wrap.scrollTop = wrap.scrollHeight;
    }
  };

  window.debugClear = function() {
    body.innerHTML = '';
  };

  window.debugFilter = function() {
    var f = filterEl.value;
    var rows = body.querySelectorAll('tr.debug-row');
    for (var i = 0; i < rows.length; i++) {
      if (!f || rows[i].getAttribute('data-subsystem') === f) {
        rows[i].style.display = '';
      } else {
        rows[i].style.display = 'none';
      }
    }
  };

  // Auto-scroll to bottom on load
  if (autoScroll.checked) {
    wrap.scrollTop = wrap.scrollHeight;
  }

  // SSE listener for live debug entries
  createSSE('/events', {
    onDebugLog: function(entry) {
      debugAppendRow(entry);
    }
  });

  // data-action wiring. The verbs are attached to window (for SSE
  // callbacks); delegated dispatch needs its own handler map so the
  // Clear / Replay / Sync Order Status buttons fire.
  delegateActions(document.body, {
    debugClear: window.debugClear,
    debugFilter: window.debugFilter,
    requestOrderStatusSync: window.requestOrderStatusSync,
    replayDeadLetter: window.replayDeadLetter
  }, { events: ['click', 'change', 'input'] });
})();
