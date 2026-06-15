import { api, delegateActions, formatDuration, uiConfirm } from '/static/app.js';
import { renderBarList } from '/static/components/BarList.js';

var allRows = [];
var filteredRows = [];
var sortCol = '';
var sortAsc = true;

(function() {
  loadInventory();
  loadBuckets();
  loadProduction();
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
    renderPartRollup(allRows);
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

// P3.2/P3.3 — part-number rollup + three-way bin lifecycle, composed
// client-side from the bin rows already loaded (no new endpoint). One
// physical bin can surface as several rows (the manifest-items LATERAL in
// inventorySQL), so dedupe by bin_id first; uop_remaining and payload_code
// are per-bin columns, identical across a bin's rows. Plant-wide: this is
// the headline, decoupled from the detail-table filters below.
function renderPartRollup(rows) {
  var bins = {};
  for (var i = 0; i < (rows || []).length; i++) {
    var r = rows[i];
    if (r.bin_id && !bins[r.bin_id]) {
      bins[r.bin_id] = { payload: r.payload_code || '', cat: r.cat_id || '', uop: r.uop_remaining || 0 };
    }
  }

  // Three-way lifecycle (P3.3, Q-032 tail): stocked (UoP>0) / in-production
  // but empty (manifest present, UoP=0 — the refill signal) / idle (no
  // manifest). On-hand rollup (P3.2) counts only bins actually holding
  // material (UoP>0) — depleted-but-assigned bins live in the in-production
  // bucket, not the on-hand stock line.
  var stocked = 0, prodEmpty = 0, idle = 0;
  var byPart = {};
  Object.keys(bins).forEach(function(id) {
    var b = bins[id];
    if (b.uop > 0) {
      stocked++;
      if (b.payload) {
        if (!byPart[b.payload]) byPart[b.payload] = { part: b.payload, cat: b.cat, uop: 0, bins: 0 };
        byPart[b.payload].uop += b.uop;
        byPart[b.payload].bins += 1;
        if (!byPart[b.payload].cat && b.cat) byPart[b.payload].cat = b.cat;
      }
    } else if (b.payload) {
      prodEmpty++;
    } else {
      idle++;
    }
  });

  var lc = document.getElementById('inv-lifecycle');
  if (lc) {
    // Dots match the bins-table vocabulary (green stocked / amber refill /
    // muted idle). No health-warn class exists, so amber is inline.
    lc.innerHTML =
      '<span class="health health-ok"></span>' + stocked + ' stocked' +
      ' &nbsp; <span class="health" style="background:var(--warning)"></span>' + prodEmpty + ' in-production (empty)' +
      ' &nbsp; <span class="health" style="background:var(--text-muted)"></span>' + idle + ' idle';
  }

  var parts = Object.keys(byPart).map(function(k) { return byPart[k]; });
  parts.sort(function(a, b) { return b.uop - a.uop; });
  var body = document.getElementById('inv-rollup-body');
  if (!body) return;
  if (!parts.length) {
    body.innerHTML = '<tr><td colspan="4" class="dash-empty">No on-hand inventory.</td></tr>';
    return;
  }
  var html = '';
  for (var j = 0; j < parts.length; j++) {
    var p = parts[j];
    html += '<tr>';
    html += '<td><code>' + esc(p.part) + '</code></td>';
    html += '<td><code>' + esc(p.cat) + '</code></td>';
    html += '<td style="text-align:right">' + p.uop + '</td>';
    html += '<td style="text-align:right">' + p.bins + '</td>';
    html += '</tr>';
  }
  body.innerHTML = html;
}

// Production cluster — produced / cycle time / consumption, folded from the
// old Missions Parts section (P3.1 → P3.2). Plant-wide, recent window (top 10),
// same /api/parts/* endpoints the Missions page used.
async function loadProduction() {
  try {
    var pr = await (await fetch('/api/parts/produced?top=10')).json();
    renderBarList(document.getElementById('inv-parts-produced'), (pr && pr.rows) || [], {
      label: function(r) { return r.part_number; }, raw: function(r) { return r.qty; },
      value: function(r) { return r.qty + ' · ' + r.missions; }, color: 'var(--info)',
    });
  } catch (e) { /* leave empty */ }
  try {
    var cy = await (await fetch('/api/parts/cycle-time?top=10')).json();
    renderBarList(document.getElementById('inv-parts-cycle'), (cy && cy.rows) || [], {
      label: function(r) { return r.part_number; }, raw: function(r) { return r.avg_duration_ms; },
      value: function(r) { return formatDuration(r.avg_duration_ms); }, color: 'var(--text-muted)',
    });
  } catch (e) { /* leave empty */ }
  try {
    var co = await (await fetch('/api/parts/consumption?top=10')).json();
    renderBarList(document.getElementById('inv-parts-consume'), (co && co.rows) || [], {
      label: function(r) { return r.part_number; }, raw: function(r) { return r.uop; },
      value: function(r) { return r.uop + ' UoP'; }, color: 'var(--info)',
    });
  } catch (e) { /* leave empty */ }
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
    window.toast('Delete failed: ' + (e.message || e), 'error');
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
