(function() {
  /* --- Debug Log Beautification --- */
  var body = document.getElementById('debug-log-body');
  var wrap = document.querySelector('.debug-log-wrap');
  var autoScroll = document.getElementById('log-autoscroll');
  var filterEl = document.getElementById('log-filter');
  var maxRows = 1000;

  // Subsystem ? color group
  var groupMap = {
    kafka: 'messaging',
    protocol: 'protocol', outbox: 'protocol',
    edge_handler: 'edge', heartbeat: 'edge',
    engine: 'core',
    dispatch: 'lifecycle', completion: 'lifecycle', release: 'lifecycle',
    bin_pickup: 'lifecycle', demand: 'lifecycle',
    plc: 'plant', reporter: 'plant', station: 'plant',
    reconcile: 'sync', backfill: 'sync', changeover: 'sync',
    side_cycle: 'sync', warlink: 'sync'
  };

  function badgeClass(s) { return 'badge-' + (s || ''); }
  function groupOf(s) { return groupMap[s || ''] || ''; }
  function groupClass(s) { var g = groupOf(s); return g ? 'group-' + g : ''; }
  function esc(s) { return ShingoEdge.escapeHtml(s); }

  // Strip the first <td>...</td> from an HTML string (time cell)
  function stripFirstTd(html) {
    var end = html.indexOf('</' + 'td>');
    if (end < 0) return html;
    return html.slice(end + 5);
  }

  // Severity detection from message content
  function detectSeverity(msg) {
    if (!msg) return '';
    var u = msg.toUpperCase();
    if (/\b(FAILED|ERROR|DEAD[\s\-]?LETTER|TIMEOUT|REFUSED|INVALID)\b/.test(u)) return 'error';
    if (/\bWARN(?:ING)?:?\b/i.test(msg)) return 'warn';
    return '';
  }

  // Message tokenizer: UUID | prefix | key=value | dotted name | error keyword
  var TOKEN_RE = /([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})|((?:RECV|DISPATCH|RAW|SEND|ACK|RETRY|SKIP|WARN|ERR):)|([A-Z_]+=)(\S*)|([A-Z][A-Z0-9_]*(?:\.[A-Z][A-Z0-9_]*)+)|(\b(?:FAILED|DEAD[\s\-]?LETTER|TIMEOUT|REFUSED|INVALID|ERROR)\b)/gi;

  function formatMessage(msg) {
    if (!msg) return '';
    var parts = [];
    var last = 0, m;
    TOKEN_RE.lastIndex = 0;
    while ((m = TOKEN_RE.exec(msg)) !== null) {
      if (m.index > last) parts.push(esc(msg.slice(last, m.index)));
      if (m[1])        parts.push('<span class="log-uuid">' + esc(m[1]) + '</span>');
      else if (m[2])   parts.push('<span class="log-prefix">' + esc(m[2]) + '</span>');
      else if (m[3]) {
        parts.push('<span class="log-key">' + esc(m[3]) + '</span>' +
          (m[4] ? '<span class="log-value">' + esc(m[4]) + '</span>' : ''));
      }
      else if (m[5])   parts.push('<span class="log-name">' + esc(m[5]) + '</span>');
      else if (m[6])   parts.push('<span class="log-err-kw">' + esc(m[6]) + '</span>');
      last = TOKEN_RE.lastIndex;
    }
    if (last < msg.length) parts.push(esc(msg.slice(last)));
    return parts.join('');
  }

  // --- JSON expand/collapse ---

  function extractJson(msg) {
    if (!msg) return null;
    var rawIdx = msg.indexOf('RAW:');
    var jsonStart;
    if (rawIdx >= 0) {
      jsonStart = msg.indexOf('{', rawIdx);
    } else {
      jsonStart = msg.indexOf('{');
    }
    if (jsonStart < 0) return null;
    var candidate = msg.slice(jsonStart);
    try {
      var obj = JSON.parse(candidate);
      return { prefix: msg.slice(0, jsonStart), json: obj, raw: candidate };
    } catch (e) {
      return null;
    }
  }

  // summarizeValue produces a compact human-readable string for any JSON
  // value. Default-stringifying nested objects yields "[object Object]"
  // which is the source of the SRC=[object Object] / DST=[object Object]
  // bug — we now recognise the routing-endpoint shape ({role, station})
  // and render it as role/station, matching the header: log line. Other
  // objects fall back to JSON.stringify so at least the keys are visible.
  function summarizeValue(v) {
    if (v === null || v === undefined) return String(v);
    if (typeof v !== 'object') return String(v);
    // Endpoint shape: {role: "edge", station: "plant-a.line-1"} → "edge/plant-a.line-1"
    if (typeof v.role === 'string' && typeof v.station === 'string') {
      return v.role + '/' + v.station;
    }
    try {
      return JSON.stringify(v);
    } catch (e) {
      return '[unserializable]';
    }
  }

  function jsonSummary(obj) {
    var parts = [];
    var fields = ['type', 'subject', 'src', 'dst', 'size'];
    for (var i = 0; i < fields.length; i++) {
      if (obj[fields[i]] !== undefined) {
        parts.push('<span class="log-key">' + esc(fields[i]).toUpperCase() + '=</span>' +
          '<span class="log-value">' + esc(summarizeValue(obj[fields[i]])) + '</span>');
      }
    }
    if (obj.data && typeof obj.data === 'object') {
      var keys = Object.keys(obj.data);
      for (var j = 0; j < keys.length && j < 6; j++) {
        parts.push('<span class="log-key">' + esc(keys[j]).toUpperCase() + '=</span>' +
          '<span class="log-value">' + esc(summarizeValue(obj.data[keys[j]])) + '</span>');
      }
      if (keys.length > 6) parts.push('<span class="log-key">+' + (keys.length - 6) + ' more</span>');
    }
    return parts.join(' ');
  }

  function highlightJson(obj, indent) {
    indent = indent || 0;
    var pad = '';
    for (var p = 0; p < indent; p++) pad += '  ';
    var nextPad = pad + '  ';
    if (obj === null) return '<span class="json-null">null</span>';
    if (typeof obj === 'boolean') return '<span class="json-bool">' + obj + '</span>';
    if (typeof obj === 'number') return '<span class="json-number">' + obj + '</span>';
    if (typeof obj === 'string') return '<span class="json-string">&quot;' + esc(obj) + '&quot;</span>';
    if (Array.isArray(obj)) {
      if (obj.length === 0) return '<span class="json-brace">[]</span>';
      var items = obj.map(function(v) { return nextPad + highlightJson(v, indent + 1); });
      return '<span class="json-brace">[</span>\n' +
        items.join('<span class="json-comma">,</span>\n') + '\n' +
        pad + '<span class="json-brace">]</span>';
    }
    if (typeof obj === 'object') {
      var keys = Object.keys(obj);
      if (keys.length === 0) return '<span class="json-brace">{}</span>';
      var entries = keys.map(function(k) {
        return nextPad + '<span class="json-key">&quot;' + esc(k) + '&quot;</span>' +
          '<span class="json-colon">: </span>' + highlightJson(obj[k], indent + 1);
      });
      return '<span class="json-brace">{</span>\n' +
        entries.join('<span class="json-comma">,</span>\n') + '\n' +
        pad + '<span class="json-brace">}</span>';
    }
    return esc(String(obj));
  }

  function buildMessageHtml(msg) {
    var parsed = extractJson(msg);
    if (!parsed) return formatMessage(msg);
    var html = formatMessage(parsed.prefix);
    html += '<span class="json-summary">' + jsonSummary(parsed.json) + '</span>';
    html += '<span class="json-toggle" data-state="collapsed">[+json]</span>';
    html += '<div class="raw-expand">' + highlightJson(parsed.json) + '</div>';
    return html;
  }

  function handleJsonToggle(e) {
    var toggle = e.target;
    if (!toggle.classList.contains('json-toggle')) return;
    var expand = toggle.nextElementSibling;
    if (!expand || !expand.classList.contains('raw-expand')) return;
    var open = expand.classList.toggle('open');
    toggle.textContent = open ? '[-json]' : '[+json]';
    toggle.setAttribute('data-state', open ? 'open' : 'collapsed');
  }
  body.addEventListener('click', handleJsonToggle);

  // --- Row building ---

  function buildRowHtml(entry) {
    var sub = entry.subsystem || '';
    var sev = detectSeverity(entry.message);
    var cls = 'debug-row' +
      (sev ? ' debug-' + sev : '') +
      (groupClass(sub) ? ' ' + groupClass(sub) : '');
    var ts = entry.time ? new Date(entry.time) : new Date();
    var timeStr = ts.toTimeString().slice(0, 8) + '.' + String(ts.getMilliseconds()).padStart(3, '0');
    var badge = '<span class="subsystem-badge ' + badgeClass(sub) + '">' + esc(sub) + '</span>';
    return { cls: cls, sub: sub, sev: sev, html: '<td>' + timeStr + '</td><td>' + badge + '</td><td>' + buildMessageHtml(entry.message) + '</td>' };
  }

  function buildRow(entry) {
    var tr = document.createElement('tr');
    var r = buildRowHtml(entry);
    tr.className = r.cls;
    tr.setAttribute('data-subsystem', r.sub);
    tr.innerHTML = r.html;
    return tr;
  }

  // --- Row grouping ---

  // groupHeaderCells builds the three <td>s for a group-header row.
  // The leading ▶ chevron is the visual affordance for "click to expand"
  // — previously the only hint was `cursor: pointer`, which operators
  // missed and read as "the rest of the run is just gone". CSS flips
  // the chevron to ▼ when the header carries .expanded.
  function groupHeaderCells(origTime, badgeHtml, countBadgeHtml, messageHtml) {
    return '<td><span class="group-chevron">&#9656;</span> ' + esc(origTime) + '</td>' +
      '<td>' + badgeHtml + ' ' + countBadgeHtml + '</td>' +
      '<td class="group-msg">' + messageHtml + '</td>';
  }

  // Toggle a group header open/closed
  function toggleGroup(header) {
    var expanded = header.classList.toggle('expanded');
    var chevron = header.querySelector('.group-chevron');
    if (chevron) chevron.innerHTML = expanded ? '&#9662;' : '&#9656;';
    var n = header.nextElementSibling;
    while (n && n.classList.contains('debug-group-child')) {
      n.style.display = expanded ? 'table-row' : 'none';
      n = n.nextElementSibling;
    }
  }

  body.addEventListener('click', function(e) {
    var header = e.target.closest ? e.target.closest('.debug-group-header') : null;
    if (header) toggleGroup(header);
  });

  // Group consecutive server-rendered rows after formatting them
  function groupExistingRows() {
    var rows = Array.prototype.slice.call(body.querySelectorAll('tr.debug-row'));
    if (rows.length === 0) return;

    var entries = [];
    for (var i = 0; i < rows.length; i++) {
      var tr = rows[i];
      var sub = tr.getAttribute('data-subsystem') || '';
      var cells = tr.querySelectorAll('td');
      if (cells.length < 3) continue;
      var msg = cells[2].textContent || '';
      var sev = detectSeverity(msg);
      entries.push({ sub: sub, sev: sev, group: groupOf(sub), badgeHtml: '<span class="subsystem-badge ' + badgeClass(sub) + '">' + esc(sub) + '</span>', msgHtml: buildMessageHtml(msg), cls: 'debug-row' + (sev ? ' debug-' + sev : '') + (groupClass(sub) ? ' ' + groupClass(sub) : ''), origTime: cells[0].textContent || '' });
    }

    body.innerHTML = '';
    var i = 0;
    while (i < entries.length) {
      var e = entries[i];
      if (e.sev === 'error' || e.sev === 'warn' || !e.group) {
        var tr = document.createElement('tr');
        tr.className = e.cls;
        tr.setAttribute('data-subsystem', e.sub);
        tr.innerHTML = '<td>' + esc(e.origTime) + '</td><td>' + e.badgeHtml + '</td><td>' + e.msgHtml + '</td>';
        body.appendChild(tr);
        i++;
        continue;
      }
      var runStart = i;
      // Collapse consecutive rows by EXACT subsystem, not by group-name.
      // Previously runs spanned a whole group (e.g. dispatch/completion/release
      // all collapsed under the first row's badge as "lifecycle"), which
      // misrepresented the count. Subsystem-level grouping makes
      // "protocol 3" mean exactly 3 protocol lines.
      while (i < entries.length && entries[i].sub === e.sub && entries[i].sev === e.sev) i++;
      var runLen = i - runStart;
      if (runLen === 1) {
        var tr = document.createElement('tr');
        tr.className = e.cls;
        tr.setAttribute('data-subsystem', e.sub);
        tr.innerHTML = '<td>' + esc(e.origTime) + '</td><td>' + e.badgeHtml + '</td><td>' + e.msgHtml + '</td>';
        body.appendChild(tr);
        continue;
      }
      var first = entries[runStart];
      var header = document.createElement('tr');
      header.className = 'debug-row debug-group-header ' + groupClass(first.sub);
      header.setAttribute('data-subsystem', first.sub);
      var headerBadge = '<span class="subsystem-badge ' + badgeClass(first.sub) + '">' + esc(first.sub) + '</span>';
      var countBadge = '<span class="group-count">' + runLen + '</span>';
      header.innerHTML = groupHeaderCells(first.origTime, headerBadge, countBadge, first.msgHtml);
      body.appendChild(header);

      for (var c = runStart; c < runStart + runLen; c++) {
        var child = document.createElement('tr');
        child.className = entries[c].cls + ' debug-group-child';
        child.setAttribute('data-subsystem', entries[c].sub);
        child.style.display = 'none';
        child.innerHTML = '<td>' + esc(entries[c].origTime) + '</td><td>' + entries[c].badgeHtml + '</td><td>' + entries[c].msgHtml + '</td>';
        body.appendChild(child);
      }
    }
  }

  groupExistingRows();

  // --- Live SSE: buffer and group incoming rows ---
  var pendingBuffer = [];
  var flushTimer = null;

  function flushPending() {
    if (pendingBuffer.length === 0) return;
    var entries = pendingBuffer.slice();
    pendingBuffer = [];

    for (var i = 0; i < entries.length; i++) {
      var entry = entries[i];
      var sub = entry.subsystem || '';
      var sev = detectSeverity(entry.message);
      var grp = groupOf(sub);

      // Error/warn: always standalone (don't hide failures inside a collapsed
      // group). Subsystems without a known group also render standalone —
      // grouping only kicks in for subsystems we recognise, so unknown
      // entries stay maximally visible.
      if (sev === 'error' || sev === 'warn' || !grp) {
        appendStandalone(entry);
        continue;
      }

      // Match on EXACT subsystem, not group-name (see comment in
      // groupExistingRows for the rationale). Bursts of the same subsystem
      // collapse; lifecycle bounces across dispatch/completion/release
      // render as separate rows.
      var lastRow = body.lastElementChild;
      if (lastRow && lastRow.classList.contains('debug-group-header')) {
        if ((lastRow.getAttribute('data-subsystem') || '') === sub) {
          appendChildToGroup(lastRow, entry);
          continue;
        }
      }

      if (lastRow && lastRow.classList.contains('debug-row') && !lastRow.classList.contains('debug-group-header') && !lastRow.classList.contains('debug-group-child')) {
        if ((lastRow.getAttribute('data-subsystem') || '') === sub) {
          convertToGroup(lastRow, entry);
          continue;
        }
      }

      // Otherwise standalone
      appendStandalone(entry);
    }

    pruneRows();
    if (autoScroll.checked) wrap.scrollTop = wrap.scrollHeight;
  }

  function appendStandalone(entry) {
    var tr = buildRow(entry);
    body.appendChild(tr);
  }

  function appendChildToGroup(header, entry) {
    var child = document.createElement('tr');
    var r = buildRowHtml(entry);
    child.className = r.cls + ' debug-group-child';
    child.setAttribute('data-subsystem', r.sub);
    child.style.display = header.classList.contains('expanded') ? 'table-row' : 'none';
    child.innerHTML = r.html;
    header.after(child);
    // Update count
    var countBadge = header.querySelector('.group-count');
    if (countBadge) {
      var count = header.parentElement.querySelectorAll('.debug-group-child').length;
      countBadge.textContent = count;
    }
  }

  function convertToGroup(standalone, entry) {
    var sub = standalone.getAttribute('data-subsystem') || '';
    // Snapshot the standalone's content BEFORE replacing it. The original
    // code referenced an undefined `savedHtml`, so the first child of a
    // freshly-converted group rendered as the literal string "undefined" —
    // operators saw a count of 2 but only one row's worth of content on
    // expand. Capture row state up-front.
    var standaloneClass = standalone.className;
    var origTime = standalone.children[0].textContent;
    var savedMessageHtml = standalone.children[2].innerHTML;

    var header = document.createElement('tr');
    header.className = standaloneClass + ' debug-group-header';
    header.setAttribute('data-subsystem', sub);
    var badge = '<span class="subsystem-badge ' + badgeClass(sub) + '">' + esc(sub) + '</span>';
    var countBadge = '<span class="group-count">2</span>';
    header.innerHTML = groupHeaderCells(origTime, badge, countBadge, savedMessageHtml);
    standalone.replaceWith(header);

    // First child is the original standalone, re-rendered as a hidden
    // child of the new group. badge cell is rebuilt without the count
    // marker so children look like normal rows when expanded.
    var firstChild = document.createElement('tr');
    firstChild.className = standaloneClass + ' debug-group-child';
    firstChild.setAttribute('data-subsystem', sub);
    firstChild.style.display = 'none';
    firstChild.innerHTML = '<td>' + esc(origTime) + '</td><td>' + badge + '</td><td>' + savedMessageHtml + '</td>';
    header.after(firstChild);

    appendChildToGroup(header, entry);
  }

  function pruneRows() {
    while (body.children.length > maxRows) {
      body.removeChild(body.firstChild);
    }
  }

  // --- Public API ---

  window.debugAppendRow = function(entry) {
    pendingBuffer.push(entry);
    if (!flushTimer) {
      flushTimer = setTimeout(function() {
        flushTimer = null;
        flushPending();
      }, 100);
    }
  };

  window.debugClear = function() {
    body.innerHTML = '';
    pendingBuffer = [];
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
  ShingoEdge.createSSE('/events', {
    onDebugLog: function(entry) {
      debugAppendRow(entry);
    }
  });
})();
