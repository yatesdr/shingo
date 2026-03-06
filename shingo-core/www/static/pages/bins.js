function filterBins() {
  var q = document.getElementById('bin-search').value.toLowerCase();
  var binType = document.getElementById('bin-type-filter').value;
  var status = document.getElementById('bin-status-filter').value;
  var rows = document.querySelectorAll('#bin-table tbody tr');
  var shown = 0;
  rows.forEach(function(row) {
    var d = row.dataset;
    var matchQ = !q || (d.label && d.label.toLowerCase().indexOf(q) >= 0) || (d.node && d.node.toLowerCase().indexOf(q) >= 0);
    var matchType = !binType || d.type === binType;
    var matchStatus = !status || d.status === status;
    var vis = matchQ && matchType && matchStatus;
    row.style.display = vis ? '' : 'none';
    if (vis) shown++;
  });
  document.getElementById('bin-count').textContent = shown + ' bins';
}

/* --- Bin Type modals --- */
function openCreateBTModal() {
  showModal('bt-create-modal');
}
function closeBTCreateModal() {
  hideModal('bt-create-modal');
}

function openEditBTModal(btn) {
  var d = btn.dataset;
  document.getElementById('bt-edit-id').value = d.id;
  document.getElementById('bt-edit-code').value = d.code;
  document.getElementById('bt-edit-desc').value = d.desc || '';
  document.getElementById('bt-edit-w').value = d.width && d.width !== '0' ? d.width : '';
  document.getElementById('bt-edit-h').value = d.height && d.height !== '0' ? d.height : '';
  showModal('bt-edit-modal');
}
function closeBTEditModal() {
  hideModal('bt-edit-modal');
}

/* --- Bin modals --- */
function openCreateBinModal() {
  showModal('bin-create-modal');
}
function closeCreateBinModal() {
  hideModal('bin-create-modal');
}

function openEditBinModal(btn) {
  var d = btn.dataset;
  document.getElementById('be-id').value = d.id;
  document.getElementById('be-label').value = d.label;
  document.getElementById('be-type').value = d.typeId;
  document.getElementById('be-node').value = d.node || '';
  document.getElementById('be-status').value = d.status;
  showModal('bin-edit-modal');
}
function closeEditBinModal() {
  hideModal('bin-edit-modal');
}

/* --- Bin lifecycle actions --- */
function binAction(id, action) {
  if (!confirm(action.charAt(0).toUpperCase() + action.slice(1) + ' bin #' + id + '?')) return;
  fetch('/api/bins/action', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({id: id, action: action})
  })
  .then(function(r) { return r.json(); })
  .then(function(data) {
    if (data.error) { alert(data.error); return; }
    location.reload();
  })
  .catch(function(e) { alert('Error: ' + e); });
}

document.addEventListener('keydown', function(e) {
  if (e.key === 'Escape') {
    closeBTCreateModal(); closeBTEditModal();
    closeCreateBinModal(); closeEditBinModal();
  }
});
