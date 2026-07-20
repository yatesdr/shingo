import { api, delegateActions, el, escapeHtml, hideModal, removeParentElement, showModal, toast } from '/static/app.js';

/* --- Manifest builder --- */
function addManifestRow(containerId, catid, qty) {
  var container = document.getElementById(containerId);
  var row = document.createElement('div');
  row.className = 'manifest-row';
  row.style.cssText = 'display:flex;gap:0.4rem;align-items:center;margin-top:0.3rem';
  row.innerHTML =
    '<input type="text" placeholder="CATID" value="' + escapeHtml(catid || '') + '" style="flex:2;font-size:0.85rem;padding:0.3rem" class="mr-catid">' +
    '<input type="number" placeholder="Qty" value="' + (qty || '') + '" step="1" min="0" style="flex:1;font-size:0.85rem;padding:0.3rem" class="mr-qty">' +
    '<button type="button" class="btn btn-danger btn-sm" data-action="removeParentElement" style="padding:0.15rem 0.4rem">&times;</button>';
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
// loadRobotGroups fills the shared <datalist> with robot-group suggestions from
// the live fleet scene. Best-effort: on an RDS outage or the sim backend
// (available:false) the datalist is simply left empty — the input stays
// free-text and the server-rendered saved value (pre-filled on edit) is
// submitted regardless, so a configured robot_group is never lost.
function loadRobotGroups() {
  var dl = document.getElementById('robot-groups-list');
  if (!dl) return;
  fetch('/api/fleet/robot-groups')
    .then(function(r) { return r.json(); })
    .then(function(resp) {
      var data = (resp && resp.data) || resp || {};
      var groups = data.groups || [];
      dl.innerHTML = '';
      groups.forEach(function(g) {
        var opt = document.createElement('option');
        opt.value = g.name;
        if (g.desc) opt.label = g.desc;
        dl.appendChild(opt);
      });
    })
    .catch(function() { /* no suggestions; free-text entry still works */ });
}

// loadLoadSequences fills an advanced-load-sequence <select> with the registered
// sequence names and selects `selected` (the saved value on edit). Best-effort,
// like loadRobotGroups: on an error the select keeps just the "(normal load)"
// option. A saved value that isn't in the registry (renamed, or the list failed
// to load) is added as its own option so editing never silently drops it.
function loadLoadSequences(selectId, selected) {
  var sel = document.getElementById(selectId);
  if (!sel) return;
  function render(names) {
    sel.innerHTML = '<option value="">(normal load)</option>';
    var seen = false;
    names.forEach(function(n) {
      var opt = document.createElement('option');
      opt.value = n; opt.textContent = n;
      if (n === selected) { opt.selected = true; seen = true; }
      sel.appendChild(opt);
    });
    if (selected && !seen) {
      var opt = document.createElement('option');
      opt.value = selected; opt.textContent = selected + ' (unregistered)';
      opt.selected = true;
      sel.appendChild(opt);
    }
    if (!selected) sel.value = '';
  }
  fetch('/api/payloads/templates/sequences')
    .then(function(r) { return r.json(); })
    .then(function(resp) {
      var data = (resp && resp.data) || resp || {};
      render(data.names || []);
    })
    .catch(function() { render([]); });
}

// checkSequence runs config-time validation on demand (the Check button) and
// reports the outcome: a missing key at a real location is an error, an
// un-checkable case (no RDS / no assigned nodes) is an info "unverified", and a
// clean pass is a success. It never changes the form — it only reports.
function checkSequence(id, sequence) {
  if (!sequence) { toast('Normal load (no advanced sequence)', 'info'); return; }
  var url = '/api/payloads/templates/check-sequence?id=' + (id || 0) +
    '&sequence=' + encodeURIComponent(sequence);
  fetch(url)
    .then(function(r) { return r.json(); })
    .then(function(resp) {
      var c = (resp && resp.data) || resp || {};
      if (c.missing && c.missing.length) { toast('Missing: ' + c.missing.join('; '), 'error'); return; }
      if (c.warnings && c.warnings.length) { toast('Unverified — ' + c.warnings.join('; '), 'info'); return; }
      if (c.verified) { toast('Sequence verified at all load locations', 'success'); return; }
      toast('Check complete', 'info');
    })
    .catch(function(err) { toast('Check failed: ' + err, 'error'); });
}
function checkPLCreateSequence() {
  checkSequence(0, document.getElementById('plc-load-sequence').value);
}
function checkPLEditSequence() {
  checkSequence(parseInt(document.getElementById('pl-edit-id').value) || 0,
    document.getElementById('pl-edit-load-sequence').value);
}

// surfaceSaveWarnings toasts any "saved but unverified" warnings the server
// returned, then navigates. Warnings delay the redirect briefly so they're
// readable; a clean save navigates immediately.
function surfaceSaveWarnings(data) {
  var warnings = (data && data.data && data.data.warnings) || (data && data.warnings) || [];
  if (warnings.length) {
    toast('Saved unverified — ' + warnings.join('; '), 'info');
    setTimeout(function() { location.href = '/payloads'; }, 2000);
  } else {
    location.href = '/payloads';
  }
}

function openCreatePayloadModal() {
  document.getElementById('plc-code').value = '';
  document.getElementById('plc-uop').value = '0';
  document.getElementById('plc-notes').value = '';
  document.getElementById('plc-robot-group').value = '';
  document.getElementById('plc-manifest-rows').innerHTML = '';
  var sel = document.getElementById('plc-bin-types');
  for (var i = 0; i < sel.options.length; i++) sel.options[i].selected = false;
  loadRobotGroups();
  loadLoadSequences('plc-load-sequence', '');
  showModal('pl-create-modal');
}
function closePLCreateModal() {
  hideModal('pl-create-modal');
}

function submitPLCreate(el, evt) {
  if (evt) evt.preventDefault();
  var body = {
    code: document.getElementById('plc-code').value,
    description: document.getElementById('plc-notes').value,
    uop_capacity: parseInt(document.getElementById('plc-uop').value) || 0,
    robot_group: document.getElementById('plc-robot-group').value.trim(),
    advanced_load_sequence: document.getElementById('plc-load-sequence').value,
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
    if (data.error) { toast('Save failed: ' + data.error, 'error'); return; }
    surfaceSaveWarnings(data);
  })
  .catch(function(err) { toast('Save error: ' + err, 'error'); });
  return false;
}

function openEditPayloadModal(btn) {
  var d = btn.dataset;
  var plId = parseInt(d.id);
  document.getElementById('pl-edit-id').value = d.id;
  document.getElementById('pl-edit-code').value = d.code;
  document.getElementById('pl-edit-uop').value = d.uop || '0';
  document.getElementById('pl-edit-notes').value = d.notes || '';
  // Pre-fill from the saved value (server-rendered data attribute), NOT from
  // RDS — so editing works and the group is preserved even if RDS is down.
  document.getElementById('pl-edit-robot-group').value = d.robotGroup || '';
  loadLoadSequences('pl-edit-load-sequence', d.loadSequence || '');
  document.getElementById('ple-manifest-rows').innerHTML = '<span class="text-muted" style="font-size:0.8rem">Loading...</span>';
  // Clear any stale bin-type selection synchronously so the modal opens with
  // nothing selected (matches the create modal); the async fetch below sets the
  // real selection, and a fetch failure then leaves it cleared rather than a
  // bogus option[0]. Fixes "bin type resets to 0 on edit".
  var pleBinTypes = document.getElementById('ple-bin-types');
  for (var bi = 0; bi < pleBinTypes.options.length; bi++) pleBinTypes.options[bi].selected = false;
  loadRobotGroups();
  showModal('pl-edit-modal');

  fetch('/api/payloads/templates/manifest?id=' + plId)
    .then(function(r) {
      if (!r.ok) throw new Error('HTTP ' + r.status);
      return r.json();
    })
    .then(function(resp) {
      var items = (resp && resp.data) || resp || [];
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
      var ids = (resp && resp.data) || resp || [];
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

function submitPLEdit(el, evt) {
  if (evt) evt.preventDefault();
  var body = {
    id: parseInt(document.getElementById('pl-edit-id').value),
    code: document.getElementById('pl-edit-code').value,
    description: document.getElementById('pl-edit-notes').value,
    uop_capacity: parseInt(document.getElementById('pl-edit-uop').value) || 0,
    robot_group: document.getElementById('pl-edit-robot-group').value.trim(),
    advanced_load_sequence: document.getElementById('pl-edit-load-sequence').value,
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
    if (data.error) { toast('Save failed: ' + data.error, 'error'); return; }
    surfaceSaveWarnings(data);
  })
  .catch(function(err) { toast('Save error: ' + err, 'error'); });
  return false;
}

/* --- Keyboard shortcuts --- */
document.addEventListener('keydown', function(e) {
  if (e.key === 'Escape') {
    closePLCreateModal(); closePLEditModal();
  }
});

// ─── delegated event handlers ─────────────────────────
// All page-level data-action verbs route through delegateActions
// on document.body. Multiple event types share the same handler
// map — most handlers are click-only but a few (e.g. updatePreview)
// are referenced via data-action-change / data-action-input too,
// so binding the map across every event type keeps the page wiring
// single-source.
delegateActions(document.body, {
    addManifestRow,
    checkPLCreateSequence,
    checkPLEditSequence,
    closePLCreateModal,
    closePLEditModal,
    collectManifestRows,
    getSelectedBinTypes,
    openCreatePayloadModal,
    openEditPayloadModal,
    removeParentElement,
    submitPLCreate,
    submitPLEdit
}, { events: ['click', 'change', 'input', 'blur', 'keydown', 'submit'] });
