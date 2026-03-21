var allRows = [];
var filteredRows = [];
var sortCol = '';
var sortAsc = true;

(function() {
  loadInventory();
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
