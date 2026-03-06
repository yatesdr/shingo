var currentPayloadID = 0;
var isAuth = document.getElementById('page-data').dataset.authenticated === 'true';

/* --- Filtering --- */
function filterPayloads() {
  var q = document.getElementById('pl-search').value.toLowerCase();
  var blueprint = document.getElementById('pl-blueprint-filter').value;
  var status = document.getElementById('pl-status-filter').value;
  var rows = document.querySelectorAll('#pl-table tbody tr');
  var shown = 0;
  rows.forEach(function(row) {
    var d = row.dataset;
    var matchQ = !q || (d.bin && d.bin.toLowerCase().indexOf(q) >= 0) || (d.blueprint && d.blueprint.toLowerCase().indexOf(q) >= 0) || (d.node && d.node.toLowerCase().indexOf(q) >= 0);
    var matchBP = !blueprint || d.blueprint === blueprint;
    var matchSt = !status || d.status === status;
    var vis = matchQ && matchBP && matchSt;
    row.style.display = vis ? '' : 'none';
    if (vis) shown++;
  });
  document.getElementById('pl-count').textContent = shown + ' payloads';
}

/* --- Payload lifecycle actions --- */
function payloadAction(id, action, reason) {
  if (!confirm(action.charAt(0).toUpperCase() + action.slice(1) + ' payload #' + id + '?')) return;
  fetch('/api/payloads/action', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({id: id, action: action, reason: reason})
  })
  .then(function(r) { return r.json(); })
  .then(function(data) {
    if (data.error) { alert(data.error); return; }
    location.reload();
  })
  .catch(function(e) { alert('Error: ' + e); });
}

/* --- Bulk Register --- */
function bulkRegister() {
  var blueprintID = parseInt(document.getElementById('bulk-blueprint').value);
  var count = parseInt(document.getElementById('bulk-count').value);
  var binTypeID = document.getElementById('bulk-bin-type').value;
  var status = document.getElementById('bulk-status').value;
  var result = document.getElementById('bulk-result');
  result.textContent = 'Registering...';
  var body = {blueprint_id: blueprintID, count: count, status: status};
  if (binTypeID) body.bin_type_id = parseInt(binTypeID);
  fetch('/api/payloads/bulk-register', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify(body)
  })
  .then(function(r) { return r.json(); })
  .then(function(data) {
    if (data.error) { result.innerHTML = '<span style="color:var(--danger)">' + data.error + '</span>'; return; }
    result.innerHTML = '<span style="color:var(--success)">Created ' + data.created + ' payloads</span>';
    setTimeout(function() { location.reload(); }, 1000);
  })
  .catch(function(e) { result.innerHTML = '<span style="color:var(--danger)">Error: ' + e + '</span>'; });
}

/* --- Manifest builder --- */
function addManifestRow(containerId, catid, qty) {
  var container = document.getElementById(containerId);
  var row = document.createElement('div');
  row.className = 'manifest-row';
  row.style.cssText = 'display:flex;gap:0.4rem;align-items:center;margin-top:0.3rem';
  row.innerHTML =
    '<input type="text" placeholder="CATID" value="' + escapeHtml(catid || '') + '" style="flex:2;font-size:0.85rem;padding:0.3rem" class="mr-catid">' +
    '<input type="number" placeholder="Qty" value="' + (qty || '') + '" step="1" min="0" style="flex:1;font-size:0.85rem;padding:0.3rem" class="mr-qty">' +
    '<button type="button" class="btn btn-danger btn-sm" onclick="this.parentElement.remove()" style="padding:0.15rem 0.4rem">&times;</button>';
  container.appendChild(row);
}

function collectManifestRows(containerId) {
  var rows = document.querySelectorAll('#' + containerId + ' .manifest-row');
  var items = [];
  rows.forEach(function(row) {
    var catid = row.querySelector('.mr-catid').value.trim();
    var qty = parseInt(row.querySelector('.mr-qty').value) || 0;
    if (catid) items.push({part_number: catid, quantity: qty, description: ''});
  });
  return items;
}

function getSelectedBinTypes(selectId) {
  var sel = document.getElementById(selectId);
  var ids = [];
  for (var i = 0; i < sel.options.length; i++) {
    if (sel.options[i].selected) ids.push(parseInt(sel.options[i].value));
  }
  return ids;
}

/* --- Blueprint modals --- */
function openCreateBlueprintModal() {
  document.getElementById('bpc-code').value = '';
  document.getElementById('bpc-uop').value = '0';
  document.getElementById('bpc-notes').value = '';
  document.getElementById('bpc-manifest-rows').innerHTML = '';
  var sel = document.getElementById('bpc-bin-types');
  for (var i = 0; i < sel.options.length; i++) sel.options[i].selected = false;
  showModal('bp-create-modal');
}
function closeBPCreateModal() {
  hideModal('bp-create-modal');
}

function submitBPCreate(e) {
  e.preventDefault();
  var body = {
    code: document.getElementById('bpc-code').value,
    description: document.getElementById('bpc-notes').value,
    uop_capacity: parseInt(document.getElementById('bpc-uop').value) || 0,
    bin_type_ids: getSelectedBinTypes('bpc-bin-types'),
    manifest: collectManifestRows('bpc-manifest-rows')
  };
  fetch('/api/blueprints/create', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify(body)
  })
  .then(function(r) { return r.json(); })
  .then(function(data) {
    if (data.error) { alert(data.error); return; }
    location.href = '/payloads';
  })
  .catch(function(err) { alert('Error: ' + err); });
  return false;
}

function openEditBlueprintModal(btn) {
  var d = btn.dataset;
  var bpId = parseInt(d.id);
  document.getElementById('bp-edit-id').value = d.id;
  document.getElementById('bp-edit-code').value = d.code;
  document.getElementById('bp-edit-uop').value = d.uop || '0';
  document.getElementById('bp-edit-notes').value = d.notes || '';
  document.getElementById('bpe-manifest-rows').innerHTML = '<span class="text-muted" style="font-size:0.8rem">Loading...</span>';
  showModal('bp-edit-modal');

  // Load existing manifest items
  fetch('/api/blueprints/manifest?id=' + bpId)
    .then(function(r) { return r.json(); })
    .then(function(resp) {
      var items = resp.data || resp || [];
      var container = document.getElementById('bpe-manifest-rows');
      container.innerHTML = '';
      if (items && items.length > 0) {
        items.forEach(function(item) {
          addManifestRow('bpe-manifest-rows', item.part_number, item.quantity);
        });
      }
    })
    .catch(function() {
      document.getElementById('bpe-manifest-rows').innerHTML = '<span class="text-muted" style="font-size:0.8rem">Error loading manifest</span>';
    });

  // Load existing bin type associations
  fetch('/api/blueprints/bin-types?id=' + bpId)
    .then(function(r) { return r.json(); })
    .then(function(resp) {
      var ids = resp.data || resp || [];
      var sel = document.getElementById('bpe-bin-types');
      for (var i = 0; i < sel.options.length; i++) {
        sel.options[i].selected = ids.indexOf(parseInt(sel.options[i].value)) >= 0;
      }
    })
    .catch(function() {});
}
function closeBPEditModal() {
  hideModal('bp-edit-modal');
}

function submitBPEdit(e) {
  e.preventDefault();
  var body = {
    id: parseInt(document.getElementById('bp-edit-id').value),
    code: document.getElementById('bp-edit-code').value,
    description: document.getElementById('bp-edit-notes').value,
    uop_capacity: parseInt(document.getElementById('bp-edit-uop').value) || 0,
    bin_type_ids: getSelectedBinTypes('bpe-bin-types'),
    manifest: collectManifestRows('bpe-manifest-rows')
  };
  fetch('/api/blueprints/update', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify(body)
  })
  .then(function(r) { return r.json(); })
  .then(function(data) {
    if (data.error) { alert(data.error); return; }
    location.href = '/payloads';
  })
  .catch(function(err) { alert('Error: ' + err); });
  return false;
}

/* --- Payload create/edit modals --- */
function openCreatePayloadModal() {
  showModal('pl-create-modal');
}
function closePLCreateModal() {
  hideModal('pl-create-modal');
}
function openEditPayloadModal(btn) {
  var d = btn.dataset;
  document.getElementById('pe-id').value = d.id;
  document.getElementById('pe-blueprint').value = d.blueprint;
  document.getElementById('pe-bin').value = d.bin || '';
  document.getElementById('pe-status').value = d.status;
  document.getElementById('pe-notes').value = d.notes;
  showModal('pl-edit-modal');
}
function closePLEditModal() {
  hideModal('pl-edit-modal');
}

/* --- Payload detail modal (manifest + events) --- */
function openDetailModal(id) {
  currentPayloadID = id;
  document.getElementById('pd-id').textContent = id;
  document.getElementById('pd-manifest').innerHTML = '<span class="text-muted" style="font-size:0.8rem">Loading...</span>';
  document.getElementById('pd-events').innerHTML = '<span class="text-muted">Loading...</span>';
  showModal('pl-detail-modal');

  fetch('/api/payloads/detail?id=' + id)
    .then(function(r) { return r.json(); })
    .then(function(resp) {
      var p = resp.data || resp;
      document.getElementById('pd-bin').textContent = p.bin_label || '-';
      document.getElementById('pd-blueprint').textContent = p.blueprint_code || '-';
      document.getElementById('pd-node').textContent = p.node_name || 'unassigned';
      document.getElementById('pd-status').textContent = p.status || '-';
      document.getElementById('pd-uop').textContent = p.uop_remaining != null ? p.uop_remaining : '-';
      document.getElementById('pd-notes').textContent = p.notes || '-';
    })
    .catch(function() {});

  loadManifest(id);

  fetch('/api/payloads/events?id=' + id)
    .then(function(r) { return r.json(); })
    .then(function(resp) {
      var events = resp.data || resp || [];
      if (!events || events.length === 0) {
        document.getElementById('pd-events').innerHTML = '<span class="text-muted">No events recorded</span>';
        return;
      }
      var html = '<table><thead><tr><th>Event</th><th>Detail</th><th>Actor</th><th>When</th></tr></thead><tbody>';
      events.forEach(function(e) {
        html += '<tr><td>' + escapeHtml(e.event_type) + '</td><td>' + escapeHtml(e.detail) + '</td><td>' + escapeHtml(e.actor) + '</td><td>' + timeAgo(e.created_at) + '</td></tr>';
      });
      html += '</tbody></table>';
      document.getElementById('pd-events').innerHTML = html;
    })
    .catch(function() {
      document.getElementById('pd-events').innerHTML = '<span class="text-muted">Error loading events</span>';
    });
}

function closeDetailModal() {
  hideModal('pl-detail-modal');
}

function loadManifest(payloadID) {
  var list = document.getElementById('pd-manifest');
  fetch('/api/payloads/manifest?id=' + payloadID)
    .then(function(r) { if (!r.ok) throw new Error('HTTP ' + r.status); return r.json(); })
    .then(function(resp) {
      var items = resp.data || resp || [];
      if (!items || items.length === 0) {
        list.innerHTML = '<span class="text-muted" style="font-size:0.8rem">No manifest items</span>';
        return;
      }
      var html = '<table style="font-size:0.8rem"><thead><tr><th>Part Number</th><th>Qty</th><th>Prod Date</th><th>Lot Code</th><th>Notes</th>';
      if (isAuth) html += '<th></th>';
      html += '</tr></thead><tbody>';
      items.forEach(function(item) {
        html += '<tr><td>' + escapeHtml(item.part_number) + '</td><td>' + item.quantity + '</td><td>' + (item.production_date || '-') + '</td><td>' + (item.lot_code || '-') + '</td><td>' + escapeHtml(item.notes) + '</td>';
        if (isAuth) html += '<td><button class="btn btn-danger btn-sm" onclick="deleteManifestItem(' + item.id + ')" style="font-size:0.7rem;padding:0.1rem 0.3rem">X</button></td>';
        html += '</tr>';
      });
      html += '</tbody></table>';
      list.innerHTML = html;
    })
    .catch(function() {
      list.innerHTML = '<span class="text-muted" style="font-size:0.8rem">Error loading manifest</span>';
    });
}

function addManifestItem() {
  var body = {
    payload_id: currentPayloadID,
    part_number: document.getElementById('mi-part').value,
    quantity: parseInt(document.getElementById('mi-qty').value) || 0,
    production_date: document.getElementById('mi-date').value,
    lot_code: document.getElementById('mi-lot').value,
    notes: document.getElementById('mi-notes').value
  };
  fetch('/api/payloads/manifest/create', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify(body)})
    .then(function(r) { if (!r.ok) throw new Error('HTTP ' + r.status); return r.json(); })
    .then(function() {
      document.getElementById('mi-part').value = '';
      document.getElementById('mi-qty').value = '';
      document.getElementById('mi-date').value = '';
      document.getElementById('mi-lot').value = '';
      document.getElementById('mi-notes').value = '';
      loadManifest(currentPayloadID);
    })
    .catch(function(e) { alert('Error: ' + e); });
}

function deleteManifestItem(id) {
  if (!confirm('Delete this manifest item?')) return;
  fetch('/api/payloads/manifest/delete', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({id:id})})
    .then(function(r) { if (!r.ok) throw new Error('HTTP ' + r.status); return r.json(); })
    .then(function() { loadManifest(currentPayloadID); })
    .catch(function(e) { alert('Error: ' + e); });
}

/* --- Keyboard shortcuts --- */
document.addEventListener('keydown', function(e) {
  if (e.key === 'Escape') {
    closeBPCreateModal(); closeBPEditModal();
    closePLCreateModal(); closePLEditModal();
    closeDetailModal();
  }
});
