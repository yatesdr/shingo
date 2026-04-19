(function() {
  // Tab switching
  window.switchDiagTab = function(tab) {
    document.getElementById('tab-debug').style.display = tab === 'debug' ? 'block' : 'none';
    document.getElementById('tab-cms').style.display = tab === 'cms' ? 'block' : 'none';
    document.getElementById('tab-recon').style.display = tab === 'recon' ? 'block' : 'none';
    document.getElementById('tab-recovery').style.display = tab === 'recovery' ? 'block' : 'none';
    var tabFire = document.getElementById('tab-fire');
    if (tabFire) tabFire.style.display = tab === 'fire' ? 'block' : 'none';
    var tabEmaint = document.getElementById('tab-emaint');
    if (tabEmaint) tabEmaint.style.display = tab === 'emaint' ? 'block' : 'none';
    var tabs = document.querySelectorAll('.diag-tab');
    tabs.forEach(function(t) { t.classList.remove('active'); });
    if (tab === 'debug') tabs[0].classList.add('active');
    else if (tab === 'cms') tabs[1].classList.add('active');
    else if (tab === 'recon') tabs[2].classList.add('active');
    else if (tab === 'recovery') tabs[3].classList.add('active');
    var fireTab = document.querySelector('.diag-tab[data-tab="fire"]');
    if (fireTab) {
      if (tab === 'fire') fireTab.classList.add('active');
      else fireTab.classList.remove('active');
    }

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
    if (tab === 'fire' && !fireLoaded) {
      fireLoaded = true;
      loadFireAlarmStatus();
    }
    var emaintTab = document.querySelector('.diag-tab[data-tab="emaint"]');
    if (emaintTab) {
      if (tab === 'emaint') emaintTab.classList.add('active');
      else emaintTab.classList.remove('active');
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
  var fireLoaded = false;

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

  // ── Fire Alarm ─────────────────────────────────────────

  function loadFireAlarmStatus() {
    fetch('/api/fire-alarm/status')
      .then(function(r) {
        if (!r.ok) throw new Error('status query failed');
        return r.json();
      })
      .then(function(data) {
        updateFireAlarmUI(data.is_fire, data.changed_at);
      })
      .catch(function() {
        var el = document.getElementById('fa-status');
        if (el) {
          el.textContent = 'Unavailable';
          el.style.color = 'var(--text-muted)';
        }
      });
  }

  function updateFireAlarmUI(isFire, changedAt) {
    var statusEl = document.getElementById('fa-status');
    var changedEl = document.getElementById('fa-changed-at');
    var btnActivate = document.getElementById('fa-btn-activate');
    var btnClear = document.getElementById('fa-btn-clear');

    if (!statusEl) return;

    if (isFire) {
      statusEl.textContent = 'ACTIVE';
      statusEl.style.color = '#dc2626';
      if (btnActivate) btnActivate.style.display = 'none';
      if (btnClear) btnClear.style.display = 'inline-block';
    } else {
      statusEl.textContent = 'Clear';
      statusEl.style.color = '#16a34a';
      if (btnActivate) btnActivate.style.display = 'inline-block';
      if (btnClear) btnClear.style.display = 'none';
    }

    if (changedEl && changedAt) {
      var d = new Date(changedAt);
      if (!isNaN(d.getTime())) {
        changedEl.textContent = 'since ' + d.toLocaleString();
      } else {
        changedEl.textContent = '';
      }
    } else if (changedEl) {
      changedEl.textContent = '';
    }
  }

  window.fireAlarmTrigger = function(on) {
    var autoResume = false;
    var cb = document.getElementById('fa-auto-resume');
    if (cb) autoResume = cb.checked;

    var action = on ? 'ACTIVATE' : 'CLEAR';
    if (!confirm('Are you sure you want to ' + action + ' the fire alarm?')) {
      return;
    }

    fetch('/api/fire-alarm/trigger', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ on: on, autoResume: autoResume })
    })
      .then(function(r) {
        if (!r.ok) return r.json().then(function(d) { throw new Error(d.error || 'trigger failed'); });
        return r.json();
      })
      .then(function() {
        loadFireAlarmStatus();
      })
      .catch(function(err) {
        alert('Fire alarm command failed: ' + err.message);
      });
  };

  // SSE callback — wired from app.js es.addEventListener('fire-alarm', ...)
  window.onFireAlarmUpdate = function(data) {
    updateFireAlarmUI(data.is_fire, null);
  };

  // ── E-Maint Robot Telemetry ──────────────────────────────
  var emaintBody = document.getElementById('emaint-body');
  var emaintSummary = document.getElementById('emaint-summary');
  var emaintDownload = document.getElementById('emaint-download');

  window.loadEMaintReport = function() {
    emaintBody.innerHTML = '<tr><td colspan="10" class="text-muted">Loading...</td></tr>';
    fetch('/api/telemetry/e-maint')
      .then(function(r) { return r.json(); })
      .then(function(data) {
        emaintSummary.textContent = 'Report ' + data.report_id + ' | Generated ' + data.generated_at + ' | ' + data.robot_count + ' robot(s)';
        emaintDownload.style.display = 'inline-block';
        emaintBody.innerHTML = '';
        if (!data.robots || !data.robots.length) {
          emaintBody.innerHTML = '<tr><td colspan="10" class="text-muted">No robots found in cache.</td></tr>';
          return;
        }
        data.robots.forEach(function(r) {
          var tr = document.createElement('tr');
          var runtimeH = r.runtime && r.runtime.total_ms ? (r.runtime.total_ms / 3600000).toFixed(1) : '-';
          var batteryPct = r.battery ? r.battery.level_pct.toFixed(0) + '%' : '-';
          var voltage = r.battery && r.battery.voltage_v ? r.battery.voltage_v.toFixed(1) : '-';
          var odoTotal = r.odometer ? r.odometer.total_m.toFixed(0) : '-';
          var odoToday = r.odometer ? r.odometer.today_m.toFixed(0) : '-';
          var lifts = r.lifts ? r.lifts.total_count : '-';
          var ctrlTemp = r.controller && r.controller.temp_c ? r.controller.temp_c.toFixed(1) : '-';
          var station = r.position ? r.position.current_station || '-' : '-';
          var status = r.task ? r.task.status : '-';
          var statusColor = '';
          if (status === 'ready') statusColor = 'color:#16a34a;';
          else if (status === 'busy') statusColor = 'color:#2563eb;';
          else if (status === 'offline') statusColor = 'color:#9ca3af;';
          else if (status === 'error') statusColor = 'color:#dc2626;';
          tr.innerHTML =
            '<td><strong>' + escapeHtml(r.vehicle_id || '') + '</strong></td>' +
            '<td style="' + statusColor + '">' + escapeHtml(status) + '</td>' +
            '<td>' + batteryPct + '</td>' +
            '<td>' + voltage + '</td>' +
            '<td>' + odoTotal + '</td>' +
            '<td>' + odoToday + '</td>' +
            '<td>' + runtimeH + '</td>' +
            '<td>' + lifts + '</td>' +
            '<td>' + ctrlTemp + '</td>' +
            '<td>' + escapeHtml(station) + '</td>';
          emaintBody.appendChild(tr);
        });
      })
      .catch(function(err) {
        emaintBody.innerHTML = '<tr><td colspan="10" class="text-muted">Error: ' + escapeHtml(err.message) + '</td></tr>';
      });
  };
})();
