(function() {
  // Tab switching
  window.switchDiagTab = function(tab) {
    document.getElementById('tab-debug').style.display = tab === 'debug' ? 'block' : 'none';
    document.getElementById('tab-cms').style.display = tab === 'cms' ? 'block' : 'none';
    document.getElementById('tab-recon').style.display = tab === 'recon' ? 'block' : 'none';
    document.getElementById('tab-recovery').style.display = tab === 'recovery' ? 'block' : 'none';
    var tabs = document.querySelectorAll('.diag-tab');
    tabs.forEach(function(t) { t.classList.remove('active'); });
    if (tab === 'debug') tabs[0].classList.add('active');
    else if (tab === 'cms') tabs[1].classList.add('active');
    else if (tab === 'recon') tabs[2].classList.add('active');
    else tabs[3].classList.add('active');

    if (tab === 'cms' && !cmsLoaded) {
      cmsLoaded = true;
      loadCMSTransactions();
    }
    if (tab === 'recon' && !reconLoaded) {
      reconLoaded = true;
      loadReconciliation();
    }
    if (tab === 'recovery' && !recoveryLoaded) {
      recoveryLoaded = true;
      loadDeadLetters();
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
  var reconBody = document.getElementById('recon-body');
  var reconLoaded = false;
  var recoveryBody = document.getElementById('recovery-body');
  var recoveryActionsBody = document.getElementById('recovery-actions-body');
  var recoveryLoaded = false;

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
      '<td>' + escapeHtml(t.payload_code || '') + '</td>' +
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

  function formatRecoveryTime(ts) {
    var d = new Date(ts);
    if (isNaN(d.getTime())) return ts || '';
    return d.toTimeString().slice(0, 8) + '.' + String(d.getMilliseconds()).padStart(3, '0');
  }

  function renderReconciliation(resp) {
    var items = resp && resp.anomalies ? resp.anomalies : [];
    reconBody.innerHTML = '';
    if (!items.length) {
      reconBody.innerHTML = '<tr><td colspan="9" class="text-muted">No anomalies detected.</td></tr>';
      return;
    }
    items.forEach(function(item) {
      var actionHtml = '<span class="text-muted">Manual review</span>';
      if (item.recommended_action === 'reapply_completion') {
        actionHtml = '<button class="btn btn-sm" onclick="repairAnomaly(\'reapply_completion\',' + item.order_id + ',0)">Reapply Completion</button>';
      } else if (item.recommended_action === 'release_terminal_claim' && item.bin_id) {
        actionHtml = '<button class="btn btn-sm" onclick="repairAnomaly(\'release_terminal_claim\',' + item.order_id + ',' + item.bin_id + ')">Release Claim</button>';
      } else if (item.recommended_action === 'release_staged_bin' && item.bin_id) {
        actionHtml = '<button class="btn btn-sm" onclick="repairAnomaly(\'release_staged_bin\',0,' + item.bin_id + ')">Release Staged Bin</button>';
      } else if (item.recommended_action === 'cancel_stuck_order' && item.order_id) {
        actionHtml = '<button class="btn btn-sm" onclick="repairAnomaly(\'cancel_stuck_order\',' + item.order_id + ',0)">Cancel Stuck Order</button>';
      }
      var tr = document.createElement('tr');
      tr.innerHTML =
        '<td>' + escapeHtml(item.category || '') + '</td>' +
        '<td>' + escapeHtml(item.severity || '') + '</td>' +
        '<td>' + (item.order_id || '-') + '</td>' +
        '<td>' + (item.bin_id || '-') + '</td>' +
        '<td>' + escapeHtml(item.station_id || '-') + '</td>' +
        '<td>' + escapeHtml(item.order_status || '') + '</td>' +
        '<td>' + escapeHtml(item.bin_status || '-') + '</td>' +
        '<td>' + escapeHtml(item.issue || '') + '</td>' +
        '<td>' + actionHtml + '</td>';
      reconBody.appendChild(tr);
    });
  }

  function loadReconciliation() {
    fetch('/api/reconciliation')
      .then(function(r) { return r.json(); })
      .then(renderReconciliation);
  }

  function renderDeadLetters(items) {
    recoveryBody.innerHTML = '';
    if (!items || !items.length) {
      recoveryBody.innerHTML = '<tr><td colspan="6" class="text-muted">No dead-lettered outbox messages.</td></tr>';
      return;
    }
    items.forEach(function(msg) {
      var tr = document.createElement('tr');
      tr.innerHTML =
        '<td>' + msg.id + '</td>' +
        '<td>' + formatRecoveryTime(msg.created_at) + '</td>' +
        '<td>' + escapeHtml(msg.msg_type || '') + '</td>' +
        '<td>' + escapeHtml(msg.station_id || '') + '</td>' +
        '<td>' + msg.retries + '</td>' +
        '<td><button class="btn btn-sm" onclick="replayDeadLetter(' + msg.id + ')">Replay</button></td>';
      recoveryBody.appendChild(tr);
    });
  }

  function renderRecoveryActions(items) {
    if (!recoveryActionsBody) return;
    recoveryActionsBody.innerHTML = '';
    if (!items || !items.length) {
      recoveryActionsBody.innerHTML = '<tr><td colspan="7" class="text-muted">No recovery actions recorded yet.</td></tr>';
      return;
    }
    items.forEach(function(item) {
      var tr = document.createElement('tr');
      tr.innerHTML =
        '<td>' + item.id + '</td>' +
        '<td>' + formatRecoveryTime(item.created_at) + '</td>' +
        '<td>' + escapeHtml(item.action || '') + '</td>' +
        '<td>' + escapeHtml(item.target_type || '') + '</td>' +
        '<td>' + item.target_id + '</td>' +
        '<td>' + escapeHtml(item.actor || '') + '</td>' +
        '<td>' + escapeHtml(item.detail || '') + '</td>';
      recoveryActionsBody.appendChild(tr);
    });
  }

  function loadDeadLetters() {
    fetch('/api/outbox/deadletters')
      .then(function(r) { return r.json(); })
      .then(renderDeadLetters);
    fetch('/api/recovery/actions')
      .then(function(r) { return r.json(); })
      .then(renderRecoveryActions);
  }

  window.replayDeadLetter = function(id) {
    fetch('/api/outbox/replay?id=' + encodeURIComponent(id), { method: 'POST' })
      .then(function(r) {
        if (!r.ok) throw new Error('replay failed');
        return r.json();
      })
      .then(function() {
        loadDeadLetters();
      });
  };

  window.repairAnomaly = function(action, orderID, binID) {
    fetch('/api/recovery/repair', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        action: action,
        order_id: orderID || 0,
        bin_id: binID || 0
      })
    })
      .then(function(r) {
        if (!r.ok) throw new Error('repair failed');
        return r.json();
      })
      .then(function() {
        loadReconciliation();
        loadDeadLetters();
      });
  };
})();
