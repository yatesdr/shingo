import { api, delegateActions, el, escapeHtml, hideModal, showModal, toast, uiConfirm } from '/static/app.js';

// --- Click-to-edit ---
//
// Signatures match the data-action dispatch convention: any literal
// args from the action string come first, then the matched element
// (`el`), then the event (`evt`). The dispatcher binds `this` to
// the matched element too, but using positional `el` makes the
// dependency explicit.

// data-action="startEdit:demand_qty" on the span.cell-val.
function startEdit(field, el) {
  var span = el;
  var td = span.parentElement;
  var input = td.querySelector('.cell-input');
  input.classList.add('editing');
  input.focus();
  input.select();
}

// data-action-blur="stopEdit:demand_qty" on the .cell-input.
function stopEdit(field, el) {
  var input = el;
  var td = input.parentElement;
  var span = td.querySelector('.cell-val');
  var tr = input.closest('tr');
  span.textContent = input.value;
  input.classList.remove('editing');
  checkDirty(tr);
}

// data-action-blur="stopEditProduced" on the produced cell input.
function stopEditProduced(el) {
  var input = el;
  var td = input.parentElement;
  var span = td.querySelector('.cell-val');
  var tr = input.closest('tr');
  span.textContent = input.value;
  input.classList.remove('editing');
  checkDirty(tr);
}

// data-action-keydown="demandCellKeydown:demand_qty"  (or :produced_qty)
// Enter blurs to commit; Escape resets to the row's original value
// and blurs.
function demandCellKeydown(field, el, evt) {
  if (!evt) return;
  if (evt.key === 'Enter') {
    el.blur();
  } else if (evt.key === 'Escape') {
    var tr = el.closest('tr');
    var orig = field === 'demand_qty' ? tr.dataset.origDemand : tr.dataset.origProduced;
    el.value = orig;
    el.blur();
  }
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
  if (!catId) { toast('Cat-ID is required', 'info'); return; }
  var body = {
    cat_id: catId,
    description: document.getElementById('add-description').value,
    demand_qty: parseInt(document.getElementById('add-demand-qty').value) || 0
  };
  try {
    var res = await fetch('/api/demands', { method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify(body) });
    var data = await res.json();
    if (!res.ok) { toast(data.error || 'Error creating demand', 'error'); return; }
    hideModal('add-modal');
    location.reload();
  } catch(e) { toast('Error: ' + e, 'error'); }
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
  if (!body.cat_id) { toast('Cat-ID is required', 'info'); return; }
  try {
    var res = await fetch('/api/demands/' + id, { method:'PUT', headers:{'Content-Type':'application/json'}, body:JSON.stringify(body) });
    if (!res.ok) { var err = await res.json(); toast(err.error || 'Error', 'error'); return; }
    hideModal('edit-modal');
    location.reload();
  } catch(e) { toast('Error: ' + e, 'error'); }
}

// Synchronous: builds the row's edit payload from its dataset/inputs. Must
// NOT be async — applyRow / applyAll consume the result directly (d.id, …),
// so returning a Promise here silently sent PUT /api/demands/undefined.
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
    if (!res.ok) { var err = await res.json(); toast(err.error || 'Error', 'error'); return; }
    location.reload();
  } catch(e) { toast('Error: ' + e, 'error'); }
}

async function deleteRow(btn) {
  if (!await uiConfirm('Delete this tracked material?')) return;
  var tr = btn.closest('tr');
  var id = tr.dataset.id;
  try {
    var res = await fetch('/api/demands/' + id, { method:'DELETE' });
    if (!res.ok) { var err = await res.json(); toast(err.error || 'Error', 'error'); return; }
    location.reload();
  } catch(e) { toast('Error: ' + e, 'error'); }
}

async function applyAll() {
  if (!await uiConfirm('Apply all changes?')) return;
  var rows = document.querySelectorAll('#demand-body tr[data-id]');
  try {
    for (var i = 0; i < rows.length; i++) {
      var d = getRowData(rows[i]);
      var res = await fetch('/api/demands/' + d.id, { method:'PUT', headers:{'Content-Type':'application/json'}, body:JSON.stringify({cat_id:d.cat_id, description:d.description, demand_qty:d.demand_qty, produced_qty:d.produced_qty}) });
      if (!res.ok) { var err = await res.json(); toast(err.error || 'Error', 'error'); return; }
    }
    location.reload();
  } catch(e) { toast('Error: ' + e, 'error'); }
}

async function clearAllProduced() {
  if (!await uiConfirm('Zero all produced counts? This will not change demand quantities.')) return;
  try {
    var res = await fetch('/api/demands/clear-all', { method:'POST' });
    if (!res.ok) { var err = await res.json(); toast(err.error || 'Error', 'error'); return; }
    location.reload();
  } catch(e) { toast('Error: ' + e, 'error'); }
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

// ─── delegated event handlers ─────────────────────────
// All page-level data-action verbs route through delegateActions
// on document.body. Multiple event types share the same handler
// map — most handlers are click-only but a few (e.g. updatePreview)
// are referenced via data-action-change / data-action-input too,
// so binding the map across every event type keeps the page wiring
// single-source.
delegateActions(document.body, {
    addMaterial,
    applyAll,
    applyRow,
    checkDirty,
    clearAllProduced,
    deleteRow,
    demandCellKeydown,
    getRowData,
    openEditModal,
    saveEdit,
    showAddRow,
    startEdit,
    stopEdit,
    stopEditProduced,
    viewLog
}, { events: ['click', 'change', 'input', 'blur', 'keydown', 'submit'] });
