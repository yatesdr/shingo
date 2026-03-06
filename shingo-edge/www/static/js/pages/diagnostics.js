(function() {
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
    tr.innerHTML = '<td>' + timeStr + '</td><td>' + (entry.subsystem || '') + '</td><td>' + ShingoEdge.escapeHtml(entry.message || '') + '</td>';
    // Hide if filter is active and doesn't match
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
    var rows = body.querySelectorAll('tr');
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
  ShingoEdge.createSSE('/events', {
    onDebugLog: function(entry) {
      debugAppendRow(entry);
    }
  });
})();
