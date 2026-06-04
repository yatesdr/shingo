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
const KINDS = [{ value: 'task-board', label: 'Task Board' }];

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
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(url)
      .then(function () { toast('Link copied: ' + url, 'success'); })
      .catch(function () { toast(url, 'info', { sticky: true }); });
  } else {
    toast(url, 'info', { sticky: true });
  }
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
  var stationsInput = el('input', {
    className: 'form-input', type: 'text',
    value: (d && d.stations) ? d.stations.join(', ') : '',
    placeholder: 'e.g. ALN_001, ALN_002 — leave empty for whole plant'
  });
  var enabledInput = el('input', { type: 'checkbox' });
  enabledInput.checked = d ? !!d.enabled : true;

  var overlay = el('div', { className: 'modal-overlay active' }, [
    el('div', { className: 'modal', style: { maxWidth: '480px' } }, [
      el('h2', {}, isEdit ? 'Edit Dashboard' : 'New Dashboard'),
      field('Name', nameInput),
      field('Kind', kindSelect),
      field('Area — station IDs (comma-separated)', stationsInput),
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
      stations: stationsInput.value.split(/[,\n]/).map(function (s) { return s.trim(); }).filter(Boolean),
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
