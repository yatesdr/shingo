(function() {
  // Tab switching
  window.switchDiagTab = function(tab) {
    document.getElementById('tab-debug').style.display = tab === 'debug' ? '' : 'none';
    document.getElementById('tab-cms').style.display = tab === 'cms' ? '' : 'none';
    var tabs = document.querySelectorAll('.diag-tab');
    tabs.forEach(function(t) { t.classList.remove('active'); });
    if (tab === 'debug') tabs[0].classList.add('active');
    else tabs[1].classList.add('active');

    if (tab === 'cms' && !cmsLoaded) {
      cmsLoaded = true;
      loadCMSTransactions();
    }
  };

  // --- Debug Log ---
  var body = document.getElementById('debug-log-body');
  var wrap = document.querySelector('#tab-debug .debug-log-wrap');
  var autoScroll = document.getElementById('log-autoscroll');
  var filterEl = document.getElementById('log-filter');
  var maxRows = 1000;

  window.debugAppendRow = function(entry) {
    var tr = document.createElement('tr');
    tr.className = 'debug-row';
    tr.setAttribute('data-subsystem', entry.subsystem || '');
    var ts = entry.time ? new Date(entry.time) : new Date();
    var timeStr = ts.toTimeString().slice(0, 8) + '.' + String(ts.getMilliseconds()).padStart(3, '0');
    tr.innerHTML = '<td>' + timeStr + '</td><td>' + (entry.subsystem || '') + '</td><td>' + escapeHtml(entry.message || '') + '</td>';
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

  if (autoScroll.checked) {
    wrap.scrollTop = wrap.scrollHeight;
  }

  // --- CMS Transactions ---
  var cmsBody = document.getElementById('cms-log-body');
  var cmsLoaded = false;

  function formatCMSTime(ts) {
    var d = new Date(ts);
    if (isNaN(d.getTime())) return ts;
    return d.toTimeString().slice(0, 8) + '.' + String(d.getMilliseconds()).padStart(3, '0');
  }

  function makeCMSRow(t) {
    var tr = document.createElement('tr');
    var isPos = t.delta >= 0;
    tr.className = isPos ? 'cms-pos' : 'cms-neg';
    tr.setAttribute('data-node', (t.node_name || '').toLowerCase());
    tr.setAttribute('data-src', t.source_type);
    var qtyClass = isPos ? 'cms-qty-pos' : 'cms-qty-neg';
    var deltaLabel = isPos ? '+' + t.delta : String(t.delta);
    tr.innerHTML =
      '<td>' + formatCMSTime(t.created_at) + '</td>' +
      '<td>' + escapeHtml(t.node_name || '') + '</td>' +
      '<td>' + escapeHtml(t.txn_type || '') + '</td>' +
      '<td>' + escapeHtml(t.cat_id || '') + '</td>' +
      '<td class="' + qtyClass + '">' + deltaLabel + '</td>' +
      '<td>' + t.qty_before + '</td>' +
      '<td>' + t.qty_after + '</td>' +
      '<td>' + escapeHtml(t.bin_label || '-') + '</td>' +
      '<td>' + escapeHtml(t.blueprint_code || '') + '</td>' +
      '<td>' + escapeHtml(t.source_type || '') + '</td>' +
      '<td>' + escapeHtml(t.notes || '') + '</td>';
    return tr;
  }

  function loadCMSTransactions() {
    fetch('/api/cms-transactions?limit=200')
      .then(function(r) { return r.json(); })
      .then(function(data) {
        cmsBody.innerHTML = '';
        if (!data) return;
        data.forEach(function(t) {
          cmsBody.appendChild(makeCMSRow(t));
        });
        cmsFilter();
      });
  }

  window.cmsAppendRows = function(txns) {
    if (!txns) return;
    txns.forEach(function(t) {
      var tr = makeCMSRow(t);
      cmsBody.insertBefore(tr, cmsBody.firstChild);
    });
    while (cmsBody.children.length > maxRows) {
      cmsBody.removeChild(cmsBody.lastChild);
    }
    cmsFilter();
  };

  window.cmsFilter = function() {
    var nodeF = (document.getElementById('cms-filter-node').value || '').toLowerCase();
    var srcF = document.getElementById('cms-filter-src').value;
    var rows = cmsBody.querySelectorAll('tr');
    for (var i = 0; i < rows.length; i++) {
      var show = true;
      if (nodeF && rows[i].getAttribute('data-node').indexOf(nodeF) === -1) show = false;
      if (srcF && rows[i].getAttribute('data-src') !== srcF) show = false;
      rows[i].style.display = show ? '' : 'none';
    }
  };
})();
