import { api, delegateActions, el, escapeHtml, hideModal, removeParentElement, showModal, toast } from '/static/app.js';

/* --- Manifest builder ---
 *
 * The editable number is PER CYCLE — how many of the part one production
 * cycle consumes, usually 1. The full-bin count is that times the payload's
 * UoP capacity, and is shown read-only beside it so the person typing a
 * ratio can see the number they used to type.
 *
 * The derived cell is not decoration. The box was labelled Qty and holds a
 * ratio — nearly always 1 against capacities in the thousands — which reads
 * like an unfilled field until you see what it multiplies out to. The
 * full-bin figure is how someone checks they typed the right thing.
 */
function uopCapacityFor(containerId) {
  var input = document.getElementById(
    containerId === 'plc-manifest-rows' ? 'plc-uop' : 'pl-edit-uop');
  return (input && parseInt(input.value)) || 0;
}

function renderFullBin(row, containerId) {
  var cell = row.querySelector('.mr-fullbin');
  if (!cell) return;
  var per = parseInt(row.querySelector('.mr-per-cycle').value) || 0;
  var cap = uopCapacityFor(containerId);
  cell.textContent = cap > 0 ? '= ' + (per * cap) + ' / bin' : '';
}

function addManifestRow(containerId, catid, perCycle) {
  var container = document.getElementById(containerId);
  var row = document.createElement('div');
  row.className = 'manifest-row';
  row.style.cssText = 'display:flex;gap:0.4rem;align-items:center;margin-top:0.3rem';
  row.innerHTML =
    '<input type="text" placeholder="CATID" value="' + escapeHtml(catid || '') + '" style="flex:2;font-size:0.85rem;padding:0.3rem" class="mr-catid">' +
    '<input type="number" placeholder="Per cycle *" value="' + (perCycle === undefined || perCycle === null ? '' : perCycle) + '" step="1" min="1" required style="flex:1;font-size:0.85rem;padding:0.3rem" class="mr-per-cycle">' +
    '<span class="text-muted mr-fullbin" style="font-size:0.75rem;min-width:6rem"></span>' +
    '<button type="button" class="btn btn-danger btn-sm" data-action="removeParentElement" style="padding:0.15rem 0.4rem">&times;</button>';
  container.appendChild(row);
  renderFullBin(row, containerId);
  row.querySelector('.mr-per-cycle').addEventListener('input', function() {
    renderFullBin(row, containerId);
  });
}

// refreshFullBinCells re-derives every row's full-bin figure. Bound to the two
// UoP capacity inputs: the capacity is the other half of the product, so
// editing it silently staled every row before this existed.
function refreshFullBinCells(containerId) {
  document.querySelectorAll('#' + containerId + ' .manifest-row').forEach(function(row) {
    renderFullBin(row, containerId);
  });
}

// collectManifestRows returns {items, error}. A row with a part number and no
// usable per-cycle value is an ERROR, not a zero.
//
// It used to read `parseInt(...) || 0` and push the row anyway, so leaving the
// box empty submitted parts_per_cycle: 0 — a template line declaring that a bin
// of this payload contains none of that part. Nothing on screen said so and
// nothing downstream could tell that value from a deliberate one, which is how
// rows reached a plant at zero. Blank now refuses at the door.
//
// ZERO IS REFUSED TOO, and that is the same decision one step further: the
// count a bin ships to the inventory ledger is uop_remaining x this number, so
// a zero line contributes nothing to any count while looking configured. A
// part that genuinely is not in the carrier is a line that should not be on the
// manifest at all. (A DB-level CHECK is the eventual backstop; it must not ship
// before this validation is deployed, or the plant UI's own inserts fail.)
function collectManifestRows(containerId) {
  var rows = document.querySelectorAll('#' + containerId + ' .manifest-row');
  var items = [];
  var bad = [];
  rows.forEach(function(row) {
    var catid = row.querySelector('.mr-catid').value.trim();
    if (!catid) return;
    var raw = row.querySelector('.mr-per-cycle').value.trim();
    var perCycle = parseInt(raw, 10);
    if (raw === '' || isNaN(perCycle) || perCycle < 1) {
      bad.push(catid);
      return;
    }
    items.push({part_number: catid, parts_per_cycle: perCycle, description: ''});
  });
  if (bad.length > 0) {
    return {items: items, error: 'Per cycle is required and must be 1 or more. Fix: ' +
      bad.join(', ') + '. It is how many of the part ONE production cycle uses — usually 1, ' +
      'not the number in a full bin.'};
  }
  return {items: items, error: ''};
}

// The bin-type pickers are CHECKBOX LISTS, not <select multiple>. Multi-select
// needs ctrl-click to add and drops the whole selection on a plain click, which
// is an easy way to clear a payload's carrier types by accident with nothing on
// screen saying you did.
function getSelectedBinTypes(hostId) {
  var host = document.getElementById(hostId);
  if (!host) return [];
  var ids = [];
  host.querySelectorAll('input[type=checkbox]').forEach(function(cb) {
    if (cb.checked) ids.push(parseInt(cb.value));
  });
  return ids;
}

// setSelectedBinTypes checks exactly the given ids and clears the rest. Passing
// an empty list is how both modals clear a stale selection.
function setSelectedBinTypes(hostId, ids) {
  var host = document.getElementById(hostId);
  if (!host) return;
  ids = ids || [];
  host.querySelectorAll('input[type=checkbox]').forEach(function(cb) {
    cb.checked = ids.indexOf(parseInt(cb.value)) >= 0;
  });
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
  setSelectedBinTypes('plc-bin-types', []);
  loadRobotGroups();
  loadLoadSequences('plc-load-sequence', '');
  showModal('pl-create-modal');
}
function closePLCreateModal() {
  hideModal('pl-create-modal');
}

function submitPLCreate(el, evt) {
  if (evt) evt.preventDefault();
  var manifest = collectManifestRows('plc-manifest-rows');
  if (manifest.error) { toast(manifest.error, 'error'); return; }
  var body = {
    code: document.getElementById('plc-code').value,
    description: document.getElementById('plc-notes').value,
    uop_capacity: parseInt(document.getElementById('plc-uop').value) || 0,
    robot_group: document.getElementById('plc-robot-group').value.trim(),
    advanced_load_sequence: document.getElementById('plc-load-sequence').value,
    bin_type_ids: getSelectedBinTypes('plc-bin-types'),
    manifest: manifest.items
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
  setSelectedBinTypes('ple-bin-types', []);
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
          addManifestRow('ple-manifest-rows', item.part_number, item.parts_per_cycle);
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
      setSelectedBinTypes('ple-bin-types', ids);
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
  var manifest = collectManifestRows('ple-manifest-rows');
  if (manifest.error) { toast(manifest.error, 'error'); return; }
  var body = {
    id: parseInt(document.getElementById('pl-edit-id').value),
    code: document.getElementById('pl-edit-code').value,
    description: document.getElementById('pl-edit-notes').value,
    uop_capacity: parseInt(document.getElementById('pl-edit-uop').value) || 0,
    robot_group: document.getElementById('pl-edit-robot-group').value.trim(),
    advanced_load_sequence: document.getElementById('pl-edit-load-sequence').value,
    bin_type_ids: getSelectedBinTypes('ple-bin-types'),
    manifest: manifest.items
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

/* --- Import (.csv / .xlsx) --- */
// One row per manifest part, Payload Code repeated; the server groups,
// validates, skips duplicates, and reports per-row results. The file is
// POSTed raw (multipart) — the server owns parsing so both formats share
// one code path.

// Set when an import created payloads; the page reloads when the results
// modal CLOSES, not when it opens — reloading immediately would tear the
// modal down before the operator has read the report it exists to show.
var _importReloadPending = false;

function openPayloadImport() {
  var input = document.getElementById('pl-import-file');
  if (!input) return;
  input.value = '';
  input.click();
}

function closePLImportModal() {
  // Only the close that actually hid the modal triggers the pending
  // reload — a backdrop-close already hid it, and a later Escape must
  // not fire a surprise reload for a report the operator closed hours ago.
  var overlay = document.getElementById('pl-import-modal');
  var wasActive = overlay && overlay.classList.contains('active');
  hideModal('pl-import-modal');
  if (wasActive && _importReloadPending) {
    _importReloadPending = false;
    window.location.reload();
  }
}

// importStatusColor maps a report row status to its display color.
function importStatusColor(status) {
  if (status === 'created') return 'var(--success, #198754)';
  if (status === 'failed') return 'var(--danger, #dc3545)';
  if (status === 'warning') return 'var(--warning, #b58900)';
  return 'var(--text-muted)';
}

function showImportResults(data) {
  var s = (data && data.summary) || {};
  var rows = (data && data.rows) || [];
  document.getElementById('pl-import-summary').textContent =
    (s.created || 0) + ' created, ' +
    (s.skipped || 0) + ' skipped, ' +
    (s.failed || 0) + ' failed' +
    (s.warnings ? ', ' + s.warnings + ' warnings' : '');
  var host = document.getElementById('pl-import-results');
  if (rows.length === 0) {
    host.innerHTML = '<div class="text-muted" style="padding:0.4rem 0">Nothing to report — the file had no payload rows.</div>';
  } else {
    var html = '<table style="width:100%;font-size:0.85rem;border-collapse:collapse">';
    rows.forEach(function(r) {
      html += '<tr>' +
        '<td style="padding:0.2rem 0.5rem 0.2rem 0;color:var(--text-muted);white-space:nowrap">line ' + r.line + '</td>' +
        '<td style="padding:0.2rem 0.5rem"><code>' + escapeHtml(r.code || '') + '</code></td>' +
        '<td style="padding:0.2rem 0.5rem;font-weight:600;color:' + importStatusColor(r.status) + '">' + r.status + '</td>' +
        '<td style="padding:0.2rem 0.5rem">' + escapeHtml(r.reason || '') + '</td>' +
        '</tr>';
    });
    html += '</table>';
    host.innerHTML = html;
  }
  showModal('pl-import-modal');
}

function uploadPayloadImport(file) {
  var fd = new FormData();
  fd.append('file', file, file.name);
  toast('Importing ' + file.name + '…', 'info');
  fetch('/api/payloads/templates/import', { method: 'POST', body: fd })
    .then(function(r) { return r.json(); })
    .then(function(data) {
      if (data && data.error) { toast('Import failed: ' + data.error, 'error'); return; }
      showImportResults(data);
      // The list is server-rendered; reload when the modal closes (see
      // _importReloadPending) so created payloads appear.
      if (data && data.summary && data.summary.created > 0) _importReloadPending = true;
    })
    .catch(function(err) { toast('Import error: ' + err, 'error'); });
}

// Wire the hidden file input once. The change listener (rather than a
// delegated data-action-change) is deliberate: file inputs do not fire
// change through data-action delegation reliably across browsers.
(function initPayloadImport() {
  var input = document.getElementById('pl-import-file');
  if (!input) return;
  input.addEventListener('change', function() {
    if (input.files && input.files.length > 0) uploadPayloadImport(input.files[0]);
  });
})();

/* --- Keyboard shortcuts --- */
document.addEventListener('keydown', function(e) {
  if (e.key === 'Escape') {
    closePLCreateModal(); closePLEditModal(); closePLImportModal();
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
    closePLImportModal,
    getSelectedBinTypes,
    openCreatePayloadModal,
    openEditPayloadModal,
    openPayloadImport,
    refreshFullBinCells,
    removeParentElement,
    submitPLCreate,
    submitPLEdit
}, { events: ['click', 'change', 'input', 'blur', 'keydown', 'submit'] });
