// --- Click-to-edit ---
function startEdit(span, field) {
  var td = span.parentElement;
  var input = td.querySelector('.cell-input');
  input.classList.add('editing');
  input.focus();
  input.select();
}

function stopEdit(input, field) {
  var td = input.parentElement;
  var span = td.querySelector('.cell-val');
  var tr = input.closest('tr');
  span.textContent = input.value;
  input.classList.remove('editing');
  checkDirty(tr);
}

function stopEditProduced(input) {
  var td = input.parentElement;
  var span = td.querySelector('.cell-val');
  var tr = input.closest('tr');
  span.textContent = input.value;
  input.classList.remove('editing');
  checkDirty(tr);
}

function checkDirty(tr) {
  var demandInput = tr.querySelector('[name="demand_qty"]');
  var producedInput = tr.querySelector('[name="produced_qty"]');
  var demandDirty = (parseInt(demandInput.value) || 0) !== (parseInt(tr.dataset.origDemand) || 0);
  var producedDirty = (parseInt(producedInput.value) || 0) !== (parseInt(tr.dataset.origProduced) || 0);
  demandInput.parentElement.classList.toggle('cell-dirty', demandDirty);
  producedInput.parentElement.classList.toggle('cell-dirty', producedDirty);
}

// --- API calls ---
function showAddRow() {
  document.getElementById('add-cat-id').value = '';
  document.getElementById('add-description').value = '';
  document.getElementById('add-demand-qty').value = '0';
  showModal('add-modal');
}

async function addMaterial() {
  var catId = document.getElementById('add-cat-id').value.trim();
  if (!catId) { alert('Cat-ID is required'); return; }
  var body = {
    cat_id: catId,
    description: document.getElementById('add-description').value,
    demand_qty: parseInt(document.getElementById('add-demand-qty').value) || 0
  };
  try {
    var res = await fetch('/api/demands', { method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify(body) });
    var data = await res.json();
    if (!res.ok) { alert(data.error || 'Error creating demand'); return; }
    hideModal('add-modal');
    location.reload();
  } catch(e) { alert('Error: ' + e); }
}

function openEditModal(btn) {
  var tr = btn.closest('tr');
  document.getElementById('edit-id').value = tr.dataset.id;
  document.getElementById('edit-cat-id').value = tr.dataset.cat;
  document.getElementById('edit-description').value = tr.dataset.origDesc;
  showModal('edit-modal');
}

async function saveEdit() {
  var id = document.getElementById('edit-id').value;
  var tr = document.querySelector('tr[data-id="' + id + '"]');
  var body = {
    cat_id: document.getElementById('edit-cat-id').value.trim(),
    description: document.getElementById('edit-description').value,
    demand_qty: parseInt(tr.querySelector('[name="demand_qty"]').value) || 0,
    produced_qty: parseInt(tr.querySelector('[name="produced_qty"]').value) || 0
  };
  if (!body.cat_id) { alert('Cat-ID is required'); return; }
  try {
    var res = await fetch('/api/demands/' + id, { method:'PUT', headers:{'Content-Type':'application/json'}, body:JSON.stringify(body) });
    if (!res.ok) { var err = await res.json(); alert(err.error || 'Error'); return; }
    hideModal('edit-modal');
    location.reload();
  } catch(e) { alert('Error: ' + e); }
}

function getRowData(tr) {
  return {
    id: parseInt(tr.dataset.id),
    cat_id: tr.dataset.cat,
    description: tr.dataset.origDesc,
    demand_qty: parseInt(tr.querySelector('[name="demand_qty"]').value) || 0,
    produced_qty: parseInt(tr.querySelector('[name="produced_qty"]').value) || 0
  };
}

async function applyRow(btn) {
  var tr = btn.closest('tr');
  var d = getRowData(tr);
  try {
    var res = await fetch('/api/demands/' + d.id, { method:'PUT', headers:{'Content-Type':'application/json'}, body:JSON.stringify({cat_id:d.cat_id, description:d.description, demand_qty:d.demand_qty, produced_qty:d.produced_qty}) });
    if (!res.ok) { var err = await res.json(); alert(err.error || 'Error'); return; }
    location.reload();
  } catch(e) { alert('Error: ' + e); }
}

async function deleteRow(btn) {
  if (!confirm('Delete this tracked material?')) return;
  var tr = btn.closest('tr');
  var id = tr.dataset.id;
  try {
    var res = await fetch('/api/demands/' + id, { method:'DELETE' });
    if (!res.ok) { var err = await res.json(); alert(err.error || 'Error'); return; }
    location.reload();
  } catch(e) { alert('Error: ' + e); }
}

async function applyAll() {
  if (!confirm('Apply all changes?')) return;
  var rows = document.querySelectorAll('#demand-body tr[data-id]');
  try {
    for (var i = 0; i < rows.length; i++) {
      var d = getRowData(rows[i]);
      var res = await fetch('/api/demands/' + d.id, { method:'PUT', headers:{'Content-Type':'application/json'}, body:JSON.stringify({cat_id:d.cat_id, description:d.description, demand_qty:d.demand_qty, produced_qty:d.produced_qty}) });
      if (!res.ok) { var err = await res.json(); alert(err.error || 'Error'); return; }
    }
    location.reload();
  } catch(e) { alert('Error: ' + e); }
}

async function clearAllProduced() {
  if (!confirm('Zero all produced counts? This will not change demand quantities.')) return;
  try {
    var res = await fetch('/api/demands/clear-all', { method:'POST' });
    if (!res.ok) { var err = await res.json(); alert(err.error || 'Error'); return; }
    location.reload();
  } catch(e) { alert('Error: ' + e); }
}

async function viewLog(id) {
  var tr = document.querySelector('tr[data-id="' + id + '"]');
  var catId = tr ? tr.dataset.cat : '';
  document.getElementById('log-cat-id').textContent = catId;
  document.getElementById('log-body').innerHTML = '<p style="color:#888;">Loading...</p>';
  showModal('log-modal');
  try {
    var res = await fetch('/api/demands/' + id + '/log');
    if (!res.ok) { document.getElementById('log-body').innerHTML = '<p style="color:red;">Error loading log (HTTP ' + res.status + ')</p>'; return; }
    var entries = await res.json();
    if (!entries || entries.length === 0) {
      document.getElementById('log-body').innerHTML = '<p style="color:#888;">No production reports yet.</p>';
      return;
    }
    var html = '<table class="table"><thead><tr><th>Station</th><th>Quantity</th><th>Reported At</th></tr></thead><tbody>';
    for (var i = 0; i < entries.length; i++) {
      var e = entries[i];
      html += '<tr><td>' + escapeHtml(e.station_id) + '</td><td>' + e.quantity + '</td><td>' + escapeHtml(e.reported_at) + '</td></tr>';
    }
    html += '</tbody></table>';
    document.getElementById('log-body').innerHTML = html;
  } catch(e) { document.getElementById('log-body').innerHTML = '<p style="color:red;">Error: ' + escapeHtml(String(e)) + '</p>'; }
}
