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

/* --- Payload modals --- */
function openCreatePayloadModal() {
  document.getElementById('plc-code').value = '';
  document.getElementById('plc-uop').value = '0';
  document.getElementById('plc-notes').value = '';
  document.getElementById('plc-manifest-rows').innerHTML = '';
  var sel = document.getElementById('plc-bin-types');
  for (var i = 0; i < sel.options.length; i++) sel.options[i].selected = false;
  showModal('pl-create-modal');
}
function closePLCreateModal() {
  hideModal('pl-create-modal');
}

function submitPLCreate(e) {
  e.preventDefault();
  var body = {
    code: document.getElementById('plc-code').value,
    description: document.getElementById('plc-notes').value,
    uop_capacity: parseInt(document.getElementById('plc-uop').value) || 0,
    bin_type_ids: getSelectedBinTypes('plc-bin-types'),
    manifest: collectManifestRows('plc-manifest-rows')
  };
  console.log('Creating payload:', JSON.stringify(body));
  fetch('/api/payloads/templates/create', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify(body)
  })
  .then(function(r) { return r.json(); })
  .then(function(data) {
    if (data.error) { alert('Save failed: ' + data.error); return; }
    location.href = '/payloads';
  })
  .catch(function(err) { alert('Save error: ' + err); });
  return false;
}

function openEditPayloadModal(btn) {
  var d = btn.dataset;
  var plId = parseInt(d.id);
  document.getElementById('pl-edit-id').value = d.id;
  document.getElementById('pl-edit-code').value = d.code;
  document.getElementById('pl-edit-uop').value = d.uop || '0';
  document.getElementById('pl-edit-notes').value = d.notes || '';
  document.getElementById('ple-manifest-rows').innerHTML = '<span class="text-muted" style="font-size:0.8rem">Loading...</span>';
  showModal('pl-edit-modal');

  fetch('/api/payloads/templates/manifest?id=' + plId)
    .then(function(r) {
      if (!r.ok) throw new Error('HTTP ' + r.status);
      return r.json();
    })
    .then(function(resp) {
      var items = resp.data || resp || [];
      var container = document.getElementById('ple-manifest-rows');
      container.innerHTML = '';
      if (items && items.length > 0) {
        items.forEach(function(item) {
          addManifestRow('ple-manifest-rows', item.part_number, item.quantity);
        });
      }
    })
    .catch(function(err) {
      console.error('Manifest load failed:', err);
      document.getElementById('ple-manifest-rows').innerHTML =
        '<span class="text-muted" style="font-size:0.8rem">No manifest items (load failed: ' + err.message + ')</span>';
    });

  fetch('/api/payloads/templates/bin-types?id=' + plId)
    .then(function(r) {
      if (!r.ok) throw new Error('HTTP ' + r.status);
      return r.json();
    })
    .then(function(resp) {
      var ids = resp.data || resp || [];
      var sel = document.getElementById('ple-bin-types');
      for (var i = 0; i < sel.options.length; i++) {
        sel.options[i].selected = ids.indexOf(parseInt(sel.options[i].value)) >= 0;
      }
    })
    .catch(function(err) {
      console.error('Bin types load failed:', err);
    });
}
function closePLEditModal() {
  hideModal('pl-edit-modal');
}

function submitPLEdit(e) {
  e.preventDefault();
  var body = {
    id: parseInt(document.getElementById('pl-edit-id').value),
    code: document.getElementById('pl-edit-code').value,
    description: document.getElementById('pl-edit-notes').value,
    uop_capacity: parseInt(document.getElementById('pl-edit-uop').value) || 0,
    bin_type_ids: getSelectedBinTypes('ple-bin-types'),
    manifest: collectManifestRows('ple-manifest-rows')
  };
  console.log('Saving payload:', JSON.stringify(body));
  fetch('/api/payloads/templates/update', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify(body)
  })
  .then(function(r) { return r.json(); })
  .then(function(data) {
    if (data.error) { alert('Save failed: ' + data.error); return; }
    location.href = '/payloads';
  })
  .catch(function(err) { alert('Save error: ' + err); });
  return false;
}

/* --- Keyboard shortcuts --- */
document.addEventListener('keydown', function(e) {
  if (e.key === 'Escape') {
    closePLCreateModal(); closePLEditModal();
  }
});
