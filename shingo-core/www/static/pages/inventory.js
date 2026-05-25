import { api, delegateActions, uiConfirm } from '/static/app.js';

var allRows = [];
var filteredRows = [];
var sortCol = '';
var sortAsc = true;

(function() {
  loadInventory();
  loadBuckets();
})();

async function loadInventory() {
  var loading = document.getElementById('inv-loading');
  var errDiv = document.getElementById('inv-error');
  var tableWrap = document.getElementById('inv-table-wrap');
  var exportBtn = document.getElementById('export-btn');

  loading.style.display = 'flex';
  errDiv.style.display = 'none';
  tableWrap.style.display = 'none';

  try {
    var resp = await fetch('/api/inventory');
    if (!resp.ok) throw new Error('HTTP ' + resp.status);
    var data = await resp.json();
    allRows = data || [];
    populateFilterDropdowns();
    applyFilters();
    loading.style.display = 'none';
    tableWrap.style.display = 'block';
    if (exportBtn) exportBtn.disabled = false;
  } catch (e) {
    loading.style.display = 'none';
    errDiv.style.display = 'block';
    errDiv.textContent = 'Failed to load inventory: ' + e.message;
  }
}

function populateFilterDropdowns() {
  var zones = new Set();
  var groups = new Set();
  allRows.forEach(function(r) {
    if (r.zone) zones.add(r.zone);
    if (r.group_name) groups.add(r.group_name);
  });

  var zoneSel = document.getElementById('inv-zone');
  zones.forEach(function(z) {
    var opt = document.createElement('option');
    opt.value = z;
    opt.textContent = z;
    zoneSel.appendChild(opt);
  });

  var grpSel = document.getElementById('inv-group');
  groups.forEach(function(g) {
    var opt = document.createElement('option');
    opt.value = g;
    opt.textContent = g;
    grpSel.appendChild(opt);
  });
}

function applyFilters() {
  var search = document.getElementById('inv-search').value.toLowerCase();
  var zone = document.getElementById('inv-zone').value;
  var group = document.getElementById('inv-group').value;
  var status = document.getElementById('inv-status').value;
  var transitOnly = document.getElementById('inv-transit').checked;

  filteredRows = allRows.filter(function(r) {
    if (zone && r.zone !== zone) return false;
    if (group && r.group_name !== group) return false;
    if (status && r.status !== status) return false;
    if (transitOnly && !r.in_transit) return false;
    if (search) {
      var hay = [r.group_name, r.lane_name, r.node_name, r.zone, r.bin_label, r.bin_type, r.status, r.payload_code, r.cat_id, r.destination].join(' ').toLowerCase();
      if (hay.indexOf(search) === -1) return false;
    }
    return true;
  });

  if (sortCol) doSort();
  renderTable();
}

function sortBy(col) {
  if (sortCol === col) {
    sortAsc = !sortAsc;
  } else {
    sortCol = col;
    sortAsc = true;
  }
  // Update header indicators
  document.querySelectorAll('#inv-table th.sortable').forEach(function(th) {
    th.classList.remove('sort-asc', 'sort-desc');
    if (th.dataset.col === sortCol) {
      th.classList.add(sortAsc ? 'sort-asc' : 'sort-desc');
    }
  });
  doSort();
  renderTable();
}

function doSort() {
  filteredRows.sort(function(a, b) {
    var va = a[sortCol], vb = b[sortCol];
    if (typeof va === 'number' && typeof vb === 'number') {
      return sortAsc ? va - vb : vb - va;
    }
    va = (va || '').toString().toLowerCase();
    vb = (vb || '').toString().toLowerCase();
    if (va < vb) return sortAsc ? -1 : 1;
    if (va > vb) return sortAsc ? 1 : -1;
    return 0;
  });
}

function renderTable() {
  var tbody = document.getElementById('inv-body');
  var countEl = document.getElementById('row-count');

  if (filteredRows.length === 0) {
    tbody.innerHTML = '<tr><td colspan="13" class="text-muted" style="text-align:center;padding:2rem;">No inventory data</td></tr>';
    countEl.textContent = '0 rows';
    return;
  }

  countEl.textContent = filteredRows.length + ' row' + (filteredRows.length !== 1 ? 's' : '') +
    (filteredRows.length !== allRows.length ? ' of ' + allRows.length : '');

  var html = '';
  for (var i = 0; i < filteredRows.length; i++) {
    var r = filteredRows[i];
    html += '<tr>';
    html += '<td>' + esc(r.group_name) + '</td>';
    html += '<td>' + esc(r.lane_name) + '</td>';
    html += '<td>' + esc(r.node_name) + '</td>';
    html += '<td>' + esc(r.zone) + '</td>';
    html += '<td><code>' + esc(r.bin_label || '\u2014') + '</code></td>';
    html += '<td>' + esc(r.bin_type) + '</td>';
    html += '<td><span class="badge badge-' + r.status + '">' + r.status + '</span></td>';
    html += '<td>';
    if (r.in_transit) {
      html += '<span class="badge badge-in_transit" title="' + esc(r.destination) + '">In Transit</span>';
    }
    html += '</td>';
    html += '<td>' + esc(r.payload_code) + '</td>';
    html += '<td><code>' + esc(r.cat_id) + '</code></td>';
    html += '<td style="text-align:right">' + (r.qty || '') + '</td>';
    html += '<td style="text-align:right">' + (r.uop_remaining || '') + '</td>';
    html += '<td>' + (r.confirmed ? '<span class="health health-ok"></span>Yes' : (r.payload_code ? '<span class="health health-fail"></span>No' : '')) + '</td>';
    html += '</tr>';
  }
  tbody.innerHTML = html;
}

function esc(s) {
  if (!s) return '';
  var d = document.createElement('div');
  d.textContent = s;
  return d.innerHTML;
}

function exportInventory() {
  window.location.href = '/api/inventory/export';
}

async function loadBuckets() {
  var tbody = document.getElementById('buckets-body');
  var countEl = document.getElementById('buckets-count');
  if (!tbody) return;
  try {
    var resp = await fetch('/api/buckets');
    if (!resp.ok) throw new Error('HTTP ' + resp.status);
    var rows = (await resp.json()) || [];
    renderBuckets(rows);
    countEl.textContent = rows.length + ' row' + (rows.length !== 1 ? 's' : '');
  } catch (e) {
    tbody.innerHTML = '<tr><td colspan="8" class="text-muted" style="text-align:center;padding:1rem;">Failed to load lineside buckets: ' + esc(e.message) + '</td></tr>';
    countEl.textContent = '';
  }
}

async function renderBuckets(rows) {
  var tbody = document.getElementById('buckets-body');
  if (!rows || rows.length === 0) {
    tbody.innerHTML = '<tr><td colspan="9" class="text-muted" style="text-align:center;padding:1rem;">No lineside buckets — no parts currently kitted at any node.</td></tr>';
    return;
  }
  var html = '';
  for (var i = 0; i < rows.length; i++) {
    var r = rows[i];
    html += '<tr data-bucket-id="' + (r.id || 0) + '">';
    html += '<td>' + esc(r.group_name) + '</td>';
    html += '<td>' + esc(r.lane_name) + '</td>';
    html += '<td>' + esc(r.station) + '</td>';
    html += '<td>' + esc(r.node_name) + '</td>';
    html += '<td>' + (r.style_id || '') + '</td>';
    html += '<td><code>' + esc(r.part_number) + '</code></td>';
    html += '<td><span class="badge">' + esc(r.state || 'active') + '</span></td>';
    html += '<td style="text-align:right">' + (r.qty || 0) + '</td>';
    // Round-3 Obs 10: admin Delete button. Confirm dialog includes
    // enough identifying info that the operator doesn't nuke the
    // wrong row when several stations have similar parts.
    var confirmMsg = 'Delete bucket #' + r.id + ' (' + r.node_name + ' / ' + r.part_number +
        ' / qty=' + (r.qty || 0) + ')?\\n\\nUse this only for Core-only orphan rows. Active buckets re-emit from Edge after deletion.';
    html += '<td><button class="btn btn-sm btn-danger" data-action="deleteBucket"' +
        ' data-id="' + (r.id || 0) +
        '" data-confirm-msg="' + esc(confirmMsg) + '">Delete</button></td>';
    html += '</tr>';
  }
  tbody.innerHTML = html;
}

async function deleteBucket() {
  // Invoked via data-action="deleteBucket" with data-id + data-confirm-msg.
  var id = parseInt(this.dataset.id, 10);
  var confirmMsg = this.dataset.confirmMsg || 'Delete bucket?';
  if (!id) return;
  if (!await uiConfirm(confirmMsg)) return;
  try {
    var resp = await fetch('/api/buckets/delete', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: id })
    });
    if (!resp.ok) {
      var msg = 'HTTP ' + resp.status;
      try {
        var j = await resp.json();
        if (j && j.error) { msg = j.error; }
      } catch (e) { /* keep status code */ }
      window.toast('Delete failed: ' + msg, 'error');
      return;
    }
    await loadBuckets();
  } catch (e) {
    window.toast('Delete failed: ' + (e.message || e, 'error'));
  }
}

// ─── delegated event handlers ─────────────────────────
// All page-level data-action verbs route through delegateActions
// on document.body. Multiple event types share the same handler
// map — most handlers are click-only but a few (e.g. updatePreview)
// are referenced via data-action-change / data-action-input too,
// so binding the map across every event type keeps the page wiring
// single-source.
delegateActions(document.body, {
    applyFilters,
    deleteBucket,
    doSort,
    esc,
    exportInventory,
    loadBuckets,
    loadInventory,
    populateFilterDropdowns,
    renderBuckets,
    renderTable,
    sortBy
}, { events: ['click', 'change', 'input', 'blur', 'keydown', 'submit'] });
