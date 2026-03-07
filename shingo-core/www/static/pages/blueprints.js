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
    location.href = '/blueprints';
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
    location.href = '/blueprints';
  })
  .catch(function(err) { alert('Error: ' + err); });
  return false;
}

/* --- Keyboard shortcuts --- */
document.addEventListener('keydown', function(e) {
  if (e.key === 'Escape') {
    closeBPCreateModal(); closeBPEditModal();
  }
});
