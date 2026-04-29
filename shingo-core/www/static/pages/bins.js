// ===== STATE =====
var currentBinId = null;
var currentBinData = null;
var ccState = { step: 0, bins: [], index: 0, results: [] };

// ===== FILTERING =====
function filterBins() {
  var q = document.getElementById('bin-search').value.toLowerCase();
  var binType = document.getElementById('bin-type-filter').value;
  var status = document.getElementById('bin-status-filter').value;
  var contents = document.getElementById('bin-contents-filter').value;
  var locked = document.getElementById('bin-locked-filter').value;
  var rows = document.querySelectorAll('#bin-table tbody tr');
  var shown = 0;
  rows.forEach(function(row) {
    var d = row.dataset;
    var matchQ = !q || (d.label && d.label.toLowerCase().indexOf(q) >= 0) ||
                 (d.node && d.node.toLowerCase().indexOf(q) >= 0) ||
                 (d.payload && d.payload.toLowerCase().indexOf(q) >= 0);
    var matchType = !binType || d.type === binType;
    var matchStatus = !status || d.status === status;
    var matchContents = !contents || d.contents === contents;
    var matchLocked = !locked || d.locked === locked;
    var vis = matchQ && matchType && matchStatus && matchContents && matchLocked;
    row.style.display = vis ? '' : 'none';
    if (vis) shown++;
  });
  document.getElementById('bin-count').textContent = shown + ' bins';
}

// ===== DETAIL MODAL =====
function openBinDetail(id) {
  currentBinId = id;
  apiGet('/api/bins/detail?id=' + id)
    .then(function(resp) {
      currentBinData = resp;
      document.getElementById('bd-title').textContent = resp.bin.label;
      document.getElementById('bd-subtitle').textContent =
        resp.bin.bin_type_code + (resp.bin.node_name ? ' \u2022 ' + resp.bin.node_name : ' \u2022 unassigned');
      renderOverview(resp);
      renderContents(resp);
      renderActions(resp);
      renderJournal(resp);
      switchTab('overview');
      showModal('bin-detail-modal');
    })
    .catch(function(e) {
      console.error('openBinDetail', id, e);
      alert('Error loading bin: ' + ((typeof e === 'string' && e) || (e && e.error) || 'unknown'));
    });
}

function closeBinDetail() {
  hideModal('bin-detail-modal');
  currentBinId = null;
  currentBinData = null;
}

function switchTab(name) {
  var tabs = ['overview', 'contents', 'actions', 'journal'];
  tabs.forEach(function(t) {
    var panel = document.getElementById('bd-' + t);
    var btn = document.querySelector('.tab-btn[onclick*="' + t + '"]');
    if (t === name) {
      if (panel) { panel.classList.remove('hide'); panel.style.display = ''; }
      if (btn) btn.classList.add('active');
    } else {
      if (panel) { panel.classList.add('hide'); panel.style.display = 'none'; }
      if (btn) btn.classList.remove('active');
    }
  });
}

function renderOverview(data) {
  var b = data.bin;
  var html = '<div class="bd-fields">';
  html += bdField('Location', b.node_name || '<span class="text-muted">unassigned</span>');
  html += bdField('Status', '<span class="badge badge-' + esc(b.status) + '">' + esc(b.status) + '</span>');
  html += bdField('Payload', b.payload_code ? '<code>' + esc(b.payload_code) + '</code>' : '<span class="text-muted">empty</span>');
  html += bdField('UOP Remaining', b.payload_code ? b.uop_remaining + uopBar(b.uop_remaining, data.template) : '<span class="text-muted">-</span>');
  html += bdField('Manifest', b.manifest_confirmed ? '<span style="color:var(--success)">Confirmed</span>' :
    (b.payload_code ? '<span style="color:var(--warning)">Unconfirmed</span>' : '<span class="text-muted">-</span>'));
  html += bdField('Locked', b.locked ? '<span style="color:var(--danger)">' + esc(b.locked_by) + '</span>' : 'No');
  html += bdField('Description', b.description || '<span class="text-muted">-</span>');
  html += bdField('Bin Type', esc(b.bin_type_code));
  if (b.claimed_by) {
    html += bdField('Claimed By', 'Order #' + b.claimed_by);
  }
  if (b.last_counted_at) {
    html += bdField('Last Counted', timeAgo(b.last_counted_at) + ' by ' + esc(b.last_counted_by));
  }
  html += bdField('Created', timeAgo(b.created_at));
  html += bdField('Updated', timeAgo(b.updated_at));
  html += '</div>';

  if (data.current_order) {
    var o = data.current_order;
    html += '<div class="action-group"><h4>Current Order</h4>';
    html += '<p>Order #' + o.id + ' &mdash; <span class="badge badge-' + esc(o.status) + '">' + esc(o.status) + '</span></p>';
    html += '</div>';
  }

  document.getElementById('bd-overview').innerHTML = html;
}

function renderContents(data) {
  var b = data.bin;
  var html = '';

  if (data.manifest && data.manifest.items && data.manifest.items.length > 0) {
    html += h`<table class="table-compact"><thead><tr><th>Cat ID</th><th>Qty</th><th>Notes</th></tr></thead><tbody>${
      data.manifest.items.map(function(item) {
        return h`<tr><td><code>${item.catid}</code></td><td>${item.qty}</td><td>${item.notes || ''}</td></tr>`;
      })
    }</tbody></table>`;
  } else {
    html += '<p class="text-muted mb-1">No manifest items.</p>';
  }

  if (PAGE_AUTH) {
    html += '<hr style="margin:1rem 0;border:none;border-top:1px solid var(--border)">';

    // Load Payload
    html += '<div class="action-group"><h4>Load Payload</h4>';
    html += '<div class="inline-form">';
    html += '<div class="form-group"><label>Payload</label><select id="bd-load-payload" style="width:200px"><option value="">-- Select --</option>';
    (PAGE_PAYLOADS || []).forEach(function(p) {
      html += '<option value="' + esc(p.code) + '">' + esc(p.code) + '</option>';
    });
    html += '</select></div>';
    html += '<div class="form-group"><label>UOP Override</label><input type="number" id="bd-load-uop" min="0" style="width:80px" placeholder="auto"></div>';
    html += '<button class="btn btn-primary btn-sm" onclick="loadPayload()">Load</button>';
    html += '</div></div>';

    // Clear / Confirm
    html += '<div style="display:flex;gap:0.5rem">';
    if (b.payload_code) {
      html += '<button class="btn btn-sm btn-danger" onclick="clearBin()">Clear Bin</button>';
      if (b.manifest_confirmed) {
        html += '<button class="btn btn-sm" onclick="toggleManifest()">Unconfirm</button>';
      } else {
        html += '<button class="btn btn-sm" onclick="toggleManifest()">Confirm Manifest</button>';
      }
    }
    html += '</div>';
  }

  document.getElementById('bd-contents').innerHTML = html;
}

function renderActions(data) {
  var b = data.bin;
  if (!PAGE_AUTH) {
    document.getElementById('bd-actions').innerHTML = '<p class="text-muted">Login required for actions.</p>';
    return;
  }
  var html = '';

  // Status transitions
  html += '<div class="action-group"><h4>Status</h4>';
  if (b.status !== 'available') html += '<button class="btn btn-sm" onclick="doBinAction(\'activate\')">Activate</button> ';
  if (b.status !== 'flagged') html += '<button class="btn btn-sm" onclick="doBinAction(\'flag\')">Flag</button> ';
  if (b.status !== 'quality_hold') html += '<button class="btn btn-sm" onclick="doQualityHold()">Quality Hold</button> ';
  if (b.status !== 'maintenance') html += '<button class="btn btn-sm" onclick="doBinAction(\'maintenance\')">Maintenance</button> ';
  // Staged toggle: blue when active, default otherwise. Available ↔ staged only.
  if (b.status === 'available' || b.status === 'staged') {
    var stagedActive = (b.status === 'staged');
    html += '<button class="btn btn-sm' + (stagedActive ? ' btn-primary' : '') +
      '" onclick="doBinAction(\'' + (stagedActive ? 'release' : 'stage') + '\')">Staged</button> ';
  }
  if (b.status !== 'retired') html += '<button class="btn btn-sm btn-danger" onclick="doBinAction(\'retire\')">Retire</button> ';
  html += '</div>';

  // Lock/Unlock
  html += '<div class="action-group"><h4>Lock</h4>';
  if (b.locked) {
    html += '<p class="text-muted mb-1">Locked by <strong>' + esc(b.locked_by) + '</strong></p>';
    html += '<button class="btn btn-sm" onclick="doBinAction(\'unlock\')">Unlock</button>';
  } else {
    html += '<div class="inline-form">';
    html += '<div class="form-group"><label>Locked By</label><input type="text" id="bd-lock-actor" placeholder="Name" style="width:150px"></div>';
    html += '<button class="btn btn-sm" onclick="lockBin()">Lock</button>';
    html += '</div>';
  }
  html += '</div>';

  // Move
  html += '<div class="action-group"><h4>Move</h4>';
  html += '<div class="inline-form">';
  html += '<div class="form-group"><label>Destination</label><select id="bd-move-node" style="width:200px"><option value="">-- Select --</option>';
  (PAGE_NODES || []).forEach(function(n) {
    if (b.node_id && n.id === b.node_id) return; // skip current location
    html += '<option value="' + n.id + '">' + esc(n.name) + '</option>';
  });
  html += '</select></div>';
  html += '<button class="btn btn-sm" onclick="moveBin()">Move</button>';
  html += '<button class="btn btn-sm" onclick="requestTransport()">Request Transport</button>';
  html += '</div></div>';

  // Record Count
  html += '<div class="action-group"><h4>Record Count</h4>';
  html += '<div class="inline-form">';
  html += '<div class="form-group"><label>Actual UOP</label><input type="number" id="bd-count-uop" min="0" style="width:80px" value="' + b.uop_remaining + '"></div>';
  html += '<div class="form-group"><label>Counter</label><input type="text" id="bd-count-actor" placeholder="Name" style="width:150px"></div>';
  html += '<button class="btn btn-sm" onclick="recordCount()">Record</button>';
  html += '</div></div>';

  // Edit Properties
  html += '<div class="action-group"><h4>Properties</h4>';
  html += '<div class="inline-form">';
  html += '<div class="form-group"><label>Label</label><input type="text" id="bd-edit-label" value="' + esc(b.label) + '" style="width:150px"></div>';
  html += '<div class="form-group"><label>Description</label><input type="text" id="bd-edit-desc" value="' + esc(b.description || '') + '" style="width:200px"></div>';
  html += '<button class="btn btn-sm" onclick="updateBinProps()">Save</button>';
  html += '</div></div>';

  // Delete
  html += '<div class="action-group">';
  html += '<form method="POST" action="/bins/delete" onsubmit="return confirm(\'Delete this bin permanently?\')">';
  html += '<input type="hidden" name="id" value="' + b.id + '">';
  html += '<button type="submit" class="btn btn-sm btn-danger">Delete Bin</button>';
  html += '</form></div>';

  document.getElementById('bd-actions').innerHTML = html;
}

function renderJournal(data) {
  var html = '';

  // Add Note form
  if (PAGE_AUTH) {
    html += '<div class="action-group"><h4>Add Note</h4>';
    html += '<div class="inline-form mb-1">';
    html += '<div class="form-group"><label>Type</label><select id="bd-note-type" style="width:120px">';
    html += '<option value="general">General</option><option value="issue">Issue</option>';
    html += '<option value="quality">Quality</option><option value="resolution">Resolution</option>';
    html += '</select></div>';
    html += '<div class="form-group"><label>Actor</label><input type="text" id="bd-note-actor" placeholder="Name" style="width:120px"></div>';
    html += '</div>';
    html += '<div class="form-group"><textarea id="bd-note-msg" placeholder="Note text..." rows="2" style="font-size:0.85rem"></textarea></div>';
    html += '<button class="btn btn-sm btn-primary" onclick="addNote()">Add Note</button>';
    html += '</div>';
    html += '<hr style="margin:1rem 0;border:none;border-top:1px solid var(--border)">';
  }

  // Audit entries
  if (data.audit && data.audit.length > 0) {
    html += h`<div class="timeline">${
      data.audit.map(function(e) {
        return h`<div class="timeline-item">
          <div class="time">${{__html:true, value: timeAgo(e.created_at)}} &middot; ${e.actor}</div>
          <div>${e.action}${(e.old_value || e.new_value) ? {__html:true, value: h`: ${e.old_value} &rarr; ${e.new_value}`} : ''}${e.detail ? {__html:true, value: h` &mdash; ${e.detail}`} : ''}</div>
        </div>`;
      })
    }</div>`;
  } else {
    html += '<p class="text-muted">No audit entries.</p>';
  }

  // Recent orders
  if (data.recent_orders && data.recent_orders.length > 0) {
    html += '<h4 style="margin-top:1rem;font-size:0.8rem;text-transform:uppercase;color:var(--text-muted)">Recent Orders</h4>';
    html += h`<table class="table-compact"><thead><tr><th>ID</th><th>Type</th><th>Status</th><th>Created</th></tr></thead><tbody>${
      data.recent_orders.map(function(o) {
        return h`<tr><td>${o.id}</td><td>${o.order_type || ''}</td>
          <td><span class="badge badge-${o.status}">${o.status}</span></td>
          <td>${{__html:true, value: timeAgo(o.created_at)}}</td></tr>`;
      })
    }</tbody></table>`;
  }

  document.getElementById('bd-journal').innerHTML = html;
}

// ===== BIN ACTIONS =====
function doBinAction(action, params) {
  apiPost('/api/bins/action', { id: currentBinId, action: action, params: params || {} })
    .then(function() { openBinDetail(currentBinId); })
    .catch(function(e) { alert('Error: ' + (e.error || e)); });
}

function loadPayload() {
  var code = document.getElementById('bd-load-payload').value;
  if (!code) { alert('Select a payload'); return; }
  var uop = parseInt(document.getElementById('bd-load-uop').value) || 0;
  doBinAction('load_payload', { payload_code: code, uop_override: uop });
}

function clearBin() {
  if (!confirm('Clear this bin\'s payload and manifest?')) return;
  doBinAction('clear');
}

function toggleManifest() {
  var b = currentBinData.bin;
  doBinAction(b.manifest_confirmed ? 'unconfirm_manifest' : 'confirm_manifest');
}

function lockBin() {
  var actor = document.getElementById('bd-lock-actor').value.trim();
  if (!actor) { alert('Enter who is locking this bin'); return; }
  doBinAction('lock', { actor: actor });
}

function moveBin() {
  var nodeId = parseInt(document.getElementById('bd-move-node').value);
  if (!nodeId) { alert('Select a destination'); return; }
  if (currentBinData && currentBinData.bin.node_id && nodeId === currentBinData.bin.node_id) {
    alert('Bin is already at this location'); return;
  }
  doBinAction('move', { node_id: nodeId });
}

function requestTransport() {
  var nodeId = parseInt(document.getElementById('bd-move-node').value);
  if (!nodeId) { alert('Select a destination'); return; }
  if (currentBinData && currentBinData.bin.node_id && nodeId === currentBinData.bin.node_id) {
    alert('Bin is already at this location'); return;
  }
  apiPost('/api/bins/request-transport', { bin_id: currentBinId, destination_node_id: nodeId })
    .then(function(data) { alert(data.message || 'Transport requested'); openBinDetail(currentBinId); })
    .catch(function(e) { alert('Error: ' + (e.error || e)); });
}

function recordCount() {
  var uop = parseInt(document.getElementById('bd-count-uop').value) || 0;
  var actor = document.getElementById('bd-count-actor').value.trim();
  doBinAction('record_count', { actual_uop: uop, actor: actor });
}

function updateBinProps() {
  var label = document.getElementById('bd-edit-label').value.trim();
  var desc = document.getElementById('bd-edit-desc').value.trim();
  doBinAction('update', { label: label, description: desc });
}

function doQualityHold() {
  var reason = prompt('Reason for quality hold:');
  if (reason === null) return;
  doBinAction('quality_hold', { reason: reason, actor: 'ui' });
}

function addNote() {
  var noteType = document.getElementById('bd-note-type').value;
  var msg = document.getElementById('bd-note-msg').value.trim();
  var actor = document.getElementById('bd-note-actor').value.trim();
  if (!msg) { alert('Enter a note message'); return; }
  doBinAction('add_note', { note_type: noteType, message: msg, actor: actor });
}

// ===== BULK OPERATIONS =====
function toggleAllBins(cb) {
  var boxes = document.querySelectorAll('.bin-cb');
  boxes.forEach(function(box) {
    var row = box.closest('tr');
    if (row && row.style.display !== 'none') {
      box.checked = cb.checked;
    }
  });
  updateBulkBar();
}

function updateBulkBar() {
  var ids = getSelectedIds();
  var bar = document.getElementById('bulk-bar');
  if (!bar) return;
  if (ids.length > 0) {
    bar.style.display = 'flex';
    document.getElementById('bulk-count').textContent = ids.length + ' selected';
  } else {
    bar.style.display = 'none';
  }
}

function getSelectedIds() {
  var ids = [];
  document.querySelectorAll('.bin-cb:checked').forEach(function(cb) {
    ids.push(parseInt(cb.value));
  });
  return ids;
}

function bulkAction(action) {
  var ids = getSelectedIds();
  if (ids.length === 0) return;
  var params = {};
  if (action === 'lock') {
    var actor = prompt('Lock by (name):');
    if (!actor) return;
    params = { actor: actor };
  }
  if (action === 'quality_hold') {
    var reason = prompt('Reason for quality hold:');
    if (reason === null) return;
    params = { reason: reason, actor: 'ui' };
  }
  if (!confirm(action + ' ' + ids.length + ' bin(s)?')) return;
  apiPost('/api/bins/bulk-action', { ids: ids, action: action, params: params })
    .then(function(data) {
      var failed = (data.results || []).filter(function(r) { return !r.ok; });
      if (failed.length > 0) {
        alert(failed.length + ' failed: ' + failed.map(function(f) { return '#' + f.id + ': ' + f.error; }).join(', '));
      }
      ids.forEach(refreshBinRow);
      clearSelection();
    })
    .catch(function(e) { alert('Error: ' + (e.error || e)); });
}

function refreshBinRow(id) {
  apiGet('/api/bins/detail?id=' + id)
    .then(function(resp) {
      var row = document.querySelector('#bin-table tbody tr[data-id="' + id + '"]');
      if (!row) return;
      var b = resp.bin;
      var contents = b.payload_code
        ? (b.manifest_confirmed ? (b.uop_remaining > 0 ? 'loaded' : 'depleted') : 'unconfirmed')
        : 'empty';
      row.dataset.status = b.status;
      row.dataset.node = b.node_name || '';
      row.dataset.payload = b.payload_code || '';
      row.dataset.uop = b.uop_remaining || 0;
      row.dataset.locked = b.locked ? '1' : '0';
      row.dataset.claimed = b.claimed_by ? '1' : '0';
      row.dataset.confirmed = b.manifest_confirmed ? '1' : '0';
      row.dataset.contents = contents;

      var labelCell = row.querySelector('td:nth-child(' + (row.querySelector('.bin-cb') ? 2 : 1) + ')');
      if (labelCell) {
        labelCell.innerHTML = '<span class="bin-dot bin-dot-' + contents + '"></span>'
          + '<strong><code>' + escapeHtml(b.label) + '</code></strong>';
      }
      var tds = row.querySelectorAll('td');
      var off = row.querySelector('.bin-cb') ? 1 : 0;
      // Location, Payload, UOP, Status, Flags
      tds[off + 2].innerHTML = b.node_name ? escapeHtml(b.node_name) : '<span class="text-muted">-</span>';
      tds[off + 3].innerHTML = b.payload_code ? '<code>' + escapeHtml(b.payload_code) + '</code>' : '<span class="text-muted">-</span>';
      tds[off + 4].innerHTML = b.payload_code ? String(b.uop_remaining) : '<span class="text-muted">-</span>';
      tds[off + 5].innerHTML = '<span class="badge badge-' + escapeHtml(b.status) + '">' + escapeHtml(b.status) + '</span>';
      var flags = '';
      if (b.locked) flags += '<span title="Locked by ' + escapeHtml(b.locked_by || '') + '">&#128274;</span>';
      if (b.claimed_by) flags += '<span title="Claimed by order #' + b.claimed_by + '">&#128230;</span>';
      if (!b.manifest_confirmed && b.payload_code) flags += '<span title="Manifest unconfirmed">&#9888;</span>';
      tds[off + 6].innerHTML = flags;
    })
    .catch(function(err) { console.error('refreshBinRow', id, err); });
}

function clearSelection() {
  document.querySelectorAll('.bin-cb').forEach(function(cb) { cb.checked = false; });
  var hdr = document.querySelector('#bin-table thead input[type=checkbox]');
  if (hdr) hdr.checked = false;
  updateBulkBar();
}

// ===== CYCLE COUNT =====
function openCycleCount() {
  // Preview: count visible rows with payloads
  var rows = document.querySelectorAll('#bin-table tbody tr');
  var count = 0;
  rows.forEach(function(row) {
    if (row.style.display !== 'none' && row.dataset.payload) count++;
  });
  document.getElementById('cc-preview-count').textContent = count + ' bins to count';
  document.getElementById('cc-step1').style.display = '';
  document.getElementById('cc-step2').style.display = 'none';
  document.getElementById('cc-step3').style.display = 'none';
  showModal('cc-modal');
}

function closeCycleCount() {
  hideModal('cc-modal');
  ccState = { step: 0, bins: [], index: 0, results: [] };
}

function ccStart() {
  var rows = document.querySelectorAll('#bin-table tbody tr');
  ccState.bins = [];
  rows.forEach(function(row) {
    if (row.style.display !== 'none' && row.dataset.payload) {
      ccState.bins.push({
        id: parseInt(row.dataset.id),
        label: row.dataset.label,
        node: row.dataset.node,
        payload: row.dataset.payload,
        uop: parseInt(row.dataset.uop) || 0
      });
    }
  });
  if (ccState.bins.length === 0) { alert('No bins with payloads to count'); return; }
  ccState.index = 0;
  ccState.results = [];
  document.getElementById('cc-step1').style.display = 'none';
  document.getElementById('cc-step2').style.display = '';
  document.getElementById('cc-total').textContent = ccState.bins.length;
  ccShowBin();
}

function ccShowBin() {
  var bin = ccState.bins[ccState.index];
  document.getElementById('cc-index').textContent = ccState.index + 1;
  var pct = ((ccState.index) / ccState.bins.length * 100);
  document.getElementById('cc-progress-bar').style.width = pct + '%';
  document.getElementById('cc-bin-card').innerHTML =
    '<div class="cc-label">' + esc(bin.label) + '</div>' +
    '<div class="text-muted">' + esc(bin.node || 'unassigned') + ' &middot; ' + esc(bin.payload) + '</div>' +
    '<div style="margin-top:0.5rem">Expected UOP: <strong>' + bin.uop + '</strong></div>';
  document.getElementById('cc-actual').value = bin.uop;
  document.getElementById('cc-actual').focus();
}

function ccConfirm() {
  var bin = ccState.bins[ccState.index];
  var actor = document.getElementById('cc-actor').value.trim() || 'cycle_count';
  apiPost('/api/bins/action', { id: bin.id, action: 'record_count', params: { actual_uop: bin.uop, actor: actor } })
    .catch(function(e) { console.error('ccConfirm record_count', bin.id, e); });
  ccState.results.push({ id: bin.id, label: bin.label, result: 'match', expected: bin.uop, actual: bin.uop });
  ccAdvance();
}

function ccDiscrepancy() {
  var bin = ccState.bins[ccState.index];
  var actual = parseInt(document.getElementById('cc-actual').value) || 0;
  var actor = document.getElementById('cc-actor').value.trim() || 'cycle_count';
  apiPost('/api/bins/action', { id: bin.id, action: 'record_count', params: { actual_uop: actual, actor: actor } })
    .catch(function(e) { console.error('ccDiscrepancy record_count', bin.id, e); });
  ccState.results.push({ id: bin.id, label: bin.label, result: 'discrepancy', expected: bin.uop, actual: actual });
  ccAdvance();
}

function ccSkip() {
  var bin = ccState.bins[ccState.index];
  ccState.results.push({ id: bin.id, label: bin.label, result: 'skipped' });
  ccAdvance();
}

function ccFlag() {
  var bin = ccState.bins[ccState.index];
  apiPost('/api/bins/action', { id: bin.id, action: 'flag' })
    .catch(function(e) { console.error('ccFlag', bin.id, e); });
  ccState.results.push({ id: bin.id, label: bin.label, result: 'flagged' });
  ccAdvance();
}

function ccAdvance() {
  ccState.index++;
  if (ccState.index >= ccState.bins.length) {
    ccSummary();
  } else {
    ccShowBin();
  }
}

function ccSummary() {
  document.getElementById('cc-step2').style.display = 'none';
  document.getElementById('cc-step3').style.display = '';
  var matched = 0, disc = 0, skipped = 0, flagged = 0;
  ccState.results.forEach(function(r) {
    if (r.result === 'match') matched++;
    else if (r.result === 'discrepancy') disc++;
    else if (r.result === 'skipped') skipped++;
    else if (r.result === 'flagged') flagged++;
  });
  var html = '<div class="grid grid-4 mb-2">';
  html += '<div class="stat"><div class="value">' + matched + '</div><div class="label">Matched</div></div>';
  html += '<div class="stat"><div class="value" style="color:var(--warning)">' + disc + '</div><div class="label">Discrepancies</div></div>';
  html += '<div class="stat"><div class="value">' + skipped + '</div><div class="label">Skipped</div></div>';
  html += '<div class="stat"><div class="value" style="color:var(--danger)">' + flagged + '</div><div class="label">Flagged</div></div>';
  html += '</div>';
  if (disc > 0) {
    html += '<h4 style="font-size:0.85rem;margin-bottom:0.5rem">Discrepancies</h4>';
    html += '<table class="table-compact"><thead><tr><th>Bin</th><th>Expected</th><th>Actual</th><th>Diff</th></tr></thead><tbody>';
    ccState.results.forEach(function(r) {
      if (r.result === 'discrepancy') {
        var diff = r.actual - r.expected;
        html += '<tr><td><code>' + esc(r.label) + '</code></td><td>' + r.expected + '</td><td>' + r.actual + '</td>';
        html += '<td style="color:' + (diff < 0 ? 'var(--danger)' : 'var(--success)') + '">' + (diff > 0 ? '+' : '') + diff + '</td></tr>';
      }
    });
    html += '</tbody></table>';
  }
  document.getElementById('cc-summary').innerHTML = html;
}

// ===== SSE =====
window.onBinUpdate = debounce(function(e) {
  var data = JSON.parse(e.data);
  if (currentBinId && currentBinId === data.bin_id) {
    openBinDetail(currentBinId);
  }
}, 500);

// ===== HELPERS =====
function esc(s) { return escapeHtml(s); }

function bdField(label, value) {
  return '<div class="bd-field"><label>' + label + '</label><span>' + value + '</span></div>';
}

function uopBar(remaining, template) {
  var capacity = (template && template.uop_capacity) ? template.uop_capacity : remaining;
  if (capacity <= 0) return '';
  var pct = Math.min(100, Math.round(remaining / capacity * 100));
  var cls = pct > 25 ? 'uop-ok' : (pct > 5 ? 'uop-low' : 'uop-empty');
  return ' <span class="uop-bar"><span class="uop-bar-fill ' + cls + '" style="width:' + pct + '%"></span></span>';
}

// ===== BIN TYPE MODALS =====
function openCreateBTModal() { showModal('bt-create-modal'); }
function closeBTCreateModal() { hideModal('bt-create-modal'); }

function openEditBTModal(btn) {
  var d = btn.dataset;
  document.getElementById('bt-edit-id').value = d.id;
  document.getElementById('bt-edit-code').value = d.code;
  document.getElementById('bt-edit-desc').value = d.desc || '';
  document.getElementById('bt-edit-w').value = d.width && d.width !== '0' ? d.width : '';
  document.getElementById('bt-edit-h').value = d.height && d.height !== '0' ? d.height : '';
  showModal('bt-edit-modal');
}
function closeBTEditModal() { hideModal('bt-edit-modal'); }

// ===== CREATE BIN MODAL =====
function openCreateBinModal() { showModal('bin-create-modal'); }
function closeCreateBinModal() { hideModal('bin-create-modal'); }

// ===== KEYBOARD =====
document.addEventListener('keydown', function(e) {
  if (e.key === 'Escape') {
    closeBinDetail();
    closeBTCreateModal(); closeBTEditModal();
    closeCreateBinModal(); closeCycleCount();
  }
  // Cycle count shortcuts
  var step2 = document.getElementById('cc-step2');
  if (step2 && step2.style.display !== 'none') {
    if (e.key === 'Enter' && !e.ctrlKey && !e.metaKey) {
      e.preventDefault();
      ccConfirm();
    } else if (e.key === 'Tab') {
      e.preventDefault();
      ccSkip();
    } else if (e.key === 'f' || e.key === 'F') {
      if (document.activeElement && document.activeElement.tagName !== 'INPUT' && document.activeElement.tagName !== 'TEXTAREA') {
        e.preventDefault();
        ccFlag();
      }
    }
  }
});
