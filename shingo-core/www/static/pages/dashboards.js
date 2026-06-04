// dashboards.js — admin management for the dashboard platform. Lists saved
// dashboards and provides create / edit / delete plus a one-click way to grab
// the chromeless display link to put on a wall monitor.
//
// Interactive elements are built with el(...) and real onclick handlers (the
// app.js DOM builder), not data-action strings — simpler here and it sidesteps
// the data-action delegation plumbing entirely.

import { el, apiGet, apiPost, apiPut, apiDelete, toast, uiConfirm } from '/static/app.js';

// Known dashboard kinds. Adding a kind here (plus its renderer template +
// dashboard.js branch) is all the front-end needs to host a new display type.
const KINDS = [
  { value: 'task-board', label: 'Task Board' },
  { value: 'robot-map', label: 'Robot Map' }
];

function kindLabel(k) {
  var m = KINDS.find(function (x) { return x.value === k; });
  return m ? m.label : k;
}

var dashboards = [];

function load() {
  apiGet('/api/dashboards').then(function (list) {
    dashboards = list || [];
    renderTable();
  }).catch(function (e) {
    toast('Load failed: ' + e, 'error');
  });
}

function renderTable() {
  var tbody = document.getElementById('dash-tbody');
  tbody.innerHTML = '';
  if (!dashboards.length) {
    tbody.appendChild(el('tr', {}, el('td', { colspan: 6, className: 'muted' },
      'No dashboards yet. Create one to put on a board.')));
    return;
  }
  dashboards.forEach(function (d) { tbody.appendChild(rowFor(d)); });
}

function rowFor(d) {
  var stations = (d.stations && d.stations.length) ? d.stations.join(', ') : 'Whole plant';
  var displayPath = '/dashboard/' + d.id;
  return el('tr', {}, [
    el('td', {}, d.name),
    el('td', {}, kindLabel(d.kind)),
    el('td', { className: 'muted' }, stations),
    el('td', {}, d.enabled ? 'Yes' : 'No'),
    el('td', {}, [
      el('a', { href: displayPath, target: '_blank', className: 'btn btn-sm' }, 'Open'),
      ' ',
      el('button', { className: 'btn btn-sm', onclick: function () { copyLink(displayPath); } }, 'Copy link')
    ]),
    el('td', {}, [
      el('button', { className: 'btn btn-sm', onclick: function () { openModal(d); } }, 'Edit'),
      ' ',
      el('button', { className: 'btn btn-sm btn-danger', onclick: function () { removeDashboard(d); } }, 'Delete')
    ])
  ]);
}

function copyLink(path) {
  var url = location.origin + path;
  function done() { toast('Link copied: ' + url, 'success'); }
  function show() { toast(url, 'info', { sticky: true }); }
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(url)
      .then(done)
      .catch(function () { if (legacyCopy(url)) { done(); } else { show(); } });
  } else if (legacyCopy(url)) {
    done();
  } else {
    show();
  }
}

// navigator.clipboard only exists in secure contexts (HTTPS / localhost).
// Admin pages are typically served over plain HTTP on the plant LAN, where
// the legacy textarea + execCommand path is the only way to actually copy —
// without it, "Copy link" silently degraded to a toast showing the URL.
function legacyCopy(text) {
  var ta = document.createElement('textarea');
  ta.value = text;
  ta.setAttribute('readonly', '');
  ta.style.position = 'fixed';
  ta.style.left = '-9999px';
  document.body.appendChild(ta);
  ta.select();
  var ok = false;
  try { ok = document.execCommand('copy'); } catch (_) { ok = false; }
  ta.remove();
  return ok;
}

function removeDashboard(d) {
  uiConfirm('Delete dashboard "' + d.name + '"? Its display link will stop working.').then(function (ok) {
    if (!ok) return;
    apiDelete('/api/dashboards/' + d.id)
      .then(function () { toast('Deleted', 'success'); load(); })
      .catch(function (e) { toast('Delete failed: ' + e, 'error'); });
  });
}

function field(label, control) {
  return el('div', { style: { marginBottom: '0.75rem' } }, [
    el('label', { style: { display: 'block', marginBottom: '0.25rem', fontWeight: '600' } }, label),
    control
  ]);
}

function openModal(d) {
  var isEdit = !!d;

  var nameInput = el('input', { className: 'form-input', type: 'text', value: d ? d.name : '' });
  var kindSelect = el('select', { className: 'form-input' },
    KINDS.map(function (k) { return el('option', { value: k.value }, k.label); }));
  if (d) kindSelect.value = d.kind;

  // Area picker — selectable station IDs from /api/stations instead of a
  // free-text field. The board filter is an exact station_id match, so a
  // typed name that matches nothing silently scopes the board to empty.
  // Values already saved on the dashboard but absent from the live list are
  // shown flagged, so stale entries can be unchecked rather than lingering
  // invisibly.
  var selected = new Set((d && d.stations) ? d.stations : []);
  var pickerBox = el('div', {
    className: 'form-input',
    style: { maxHeight: '180px', overflowY: 'auto', padding: '0.4rem 0.6rem' }
  }, el('span', { className: 'muted' }, 'Loading stations…'));

  apiGet('/api/stations')
    .then(function (list) { renderPicker(list || []); })
    .catch(function () { renderPicker([]); });

  function renderPicker(available) {
    pickerBox.innerHTML = '';
    var all = available.slice();
    selected.forEach(function (s) { if (all.indexOf(s) === -1) all.push(s); });
    all.sort();
    if (!all.length) {
      pickerBox.appendChild(el('span', { className: 'muted' },
        'No stations seen yet — leave empty for whole plant.'));
      return;
    }
    all.forEach(function (s) {
      var cb = el('input', { type: 'checkbox' });
      cb.checked = selected.has(s);
      cb.addEventListener('change', function () {
        if (cb.checked) { selected.add(s); } else { selected.delete(s); }
      });
      var parts = [cb, ' ' + s];
      if (available.indexOf(s) === -1) {
        parts.push(el('span', { className: 'muted' }, ' — not a known station; uncheck to drop'));
      }
      pickerBox.appendChild(el('label', {
        style: { display: 'flex', alignItems: 'center', gap: '0.4rem', padding: '0.15rem 0', fontWeight: '400' }
      }, parts));
    });
  }

  var enabledInput = el('input', { type: 'checkbox' });
  enabledInput.checked = d ? !!d.enabled : true;

  var overlay = el('div', { className: 'modal-overlay active' }, [
    el('div', { className: 'modal', style: { maxWidth: '480px' } }, [
      el('h2', {}, isEdit ? 'Edit Dashboard' : 'New Dashboard'),
      field('Name', nameInput),
      field('Kind', kindSelect),
      field('Area — stations (none selected = whole plant)', pickerBox),
      el('label', { style: { display: 'flex', alignItems: 'center', gap: '0.5rem', marginBottom: '0.75rem' } },
        [enabledInput, 'Enabled']),
      el('div', { style: { display: 'flex', gap: '0.5rem', justifyContent: 'flex-end', marginTop: '1rem' } }, [
        el('button', { className: 'btn', onclick: close }, 'Cancel'),
        el('button', { className: 'btn btn-primary', onclick: doSave }, isEdit ? 'Save' : 'Create')
      ])
    ])
  ]);
  document.body.appendChild(overlay);
  setTimeout(function () { nameInput.focus(); }, 0);

  function close() { overlay.remove(); }

  function doSave() {
    var name = nameInput.value.trim();
    if (!name) { toast('Name is required', 'error'); return; }
    var payload = {
      name: name,
      kind: kindSelect.value,
      stations: Array.from(selected),
      enabled: enabledInput.checked
    };
    var req = isEdit
      ? apiPut('/api/dashboards/' + d.id, payload)
      : apiPost('/api/dashboards', payload);
    req.then(function () {
      toast(isEdit ? 'Saved' : 'Created', 'success');
      close();
      load();
    }).catch(function (e) {
      toast('Save failed: ' + e, 'error');
    });
  }
}

document.getElementById('dash-new').addEventListener('click', function () { openModal(null); });
load();
