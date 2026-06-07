// cells.js — admin management for production cells (Phase E, Q-025). Lists
// cell_config rows and provides create / edit / delete. A cell groups the
// production Processes on a station; the Process picker is populated live from
// the production-tick stream (/api/cells/processes), since a raw process_id is
// opaque — each option carries a style/payload/tick-count hint.
//
// Built with el(...) + real handlers (the app.js DOM builder), matching
// dashboards.js. Upsert is POST /api/cells for both create and edit (the
// backend keys on cell_id via ON CONFLICT).

import { el, apiGet, apiPost, apiDelete, toast, uiConfirm } from '/static/app.js';

var cells = [];

function load() {
  apiGet('/api/cells').then(function (list) {
    cells = list || [];
    renderTable();
  }).catch(function (e) {
    toast('Load failed: ' + e, 'error');
  });
}

function renderTable() {
  var tbody = document.getElementById('cell-tbody');
  tbody.innerHTML = '';
  if (!cells.length) {
    tbody.appendChild(el('tr', {}, el('td', { colspan: 5, className: 'muted' },
      'No cells configured. Create one to group a station’s Processes.')));
    return;
  }
  cells.forEach(function (c) { tbody.appendChild(rowFor(c)); });
}

function rowFor(c) {
  var subs = (c.sub_process_ids && c.sub_process_ids.length)
    ? c.sub_process_ids.join(', ') : '—';
  return el('tr', {}, [
    el('td', {}, [el('strong', {}, c.cell_id), c.display_name ? el('div', { className: 'muted' }, c.display_name) : '']),
    el('td', { className: 'muted' }, c.station),
    el('td', {}, String(c.primary_process_id)),
    el('td', { className: 'muted' }, subs),
    el('td', {}, [
      el('button', { className: 'btn btn-sm', onclick: function () { openModal(c); } }, 'Edit'),
      ' ',
      el('button', { className: 'btn btn-sm btn-danger', onclick: function () { removeCell(c); } }, 'Delete')
    ])
  ]);
}

function removeCell(c) {
  uiConfirm('Delete cell "' + c.cell_id + '"? It will disappear from Missions and the kiosk.').then(function (ok) {
    if (!ok) return;
    apiDelete('/api/cells/' + encodeURIComponent(c.cell_id))
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

function procLabel(p) {
  var bits = ['Process ' + p.process_id];
  if (p.style_id) bits.push('style ' + p.style_id);
  if (p.payload_code) bits.push(p.payload_code);
  bits.push((p.ticks || 0) + ' ticks');
  return bits.join(' · ');
}

function openModal(c) {
  var isEdit = !!c;

  var cellIdInput = el('input', { className: 'form-input', type: 'text', value: c ? c.cell_id : '' });
  if (isEdit) { cellIdInput.readOnly = true; cellIdInput.style.opacity = '0.6'; }
  var displayInput = el('input', { className: 'form-input', type: 'text', value: c ? (c.display_name || '') : '' });

  // Station picker — datalist of known stations, but free-text so a station
  // that hasn't been seen by the order path yet can still be typed.
  var stationList = el('datalist', { id: 'cell-stations' });
  var stationInput = el('input', { className: 'form-input', type: 'text', value: c ? c.station : '', list: 'cell-stations' });
  apiGet('/api/stations').then(function (list) {
    (list || []).forEach(function (s) { stationList.appendChild(el('option', { value: s })); });
  }).catch(function () {});

  var primarySelect = el('select', { className: 'form-input' });
  var subsBox = el('div', {
    className: 'form-input',
    style: { maxHeight: '180px', overflowY: 'auto', padding: '0.4rem 0.6rem' }
  }, el('span', { className: 'muted' }, 'Enter a station to load its Processes…'));

  var wantPrimary = c ? c.primary_process_id : 0;
  var wantSubs = new Set((c && c.sub_process_ids) ? c.sub_process_ids : []);

  function loadProcs() {
    var station = stationInput.value.trim();
    primarySelect.innerHTML = '';
    subsBox.innerHTML = '';
    if (!station) {
      subsBox.appendChild(el('span', { className: 'muted' }, 'Enter a station to load its Processes…'));
      return;
    }
    subsBox.appendChild(el('span', { className: 'muted' }, 'Loading…'));
    apiGet('/api/cells/processes?station=' + encodeURIComponent(station))
      .then(function (procs) { renderProcs(procs || []); })
      .catch(function () { renderProcs([]); });
  }

  function renderProcs(procs) {
    primarySelect.innerHTML = '';
    subsBox.innerHTML = '';
    if (!procs.length) {
      primarySelect.appendChild(el('option', { value: '' }, 'No Processes ticking yet'));
      subsBox.appendChild(el('span', { className: 'muted' },
        'No Processes seen for this station yet — start production, then configure.'));
      return;
    }
    procs.forEach(function (p) {
      var opt = el('option', { value: String(p.process_id) }, procLabel(p));
      primarySelect.appendChild(opt);
    });
    if (wantPrimary) primarySelect.value = String(wantPrimary);
    procs.forEach(function (p) {
      var cb = el('input', { type: 'checkbox', value: String(p.process_id) });
      cb.checked = wantSubs.has(p.process_id);
      cb.addEventListener('change', function () {
        var id = Number(p.process_id);
        if (cb.checked) { wantSubs.add(id); } else { wantSubs.delete(id); }
      });
      subsBox.appendChild(el('label', {
        style: { display: 'flex', alignItems: 'center', gap: '0.4rem', padding: '0.15rem 0', fontWeight: '400' }
      }, [cb, ' ' + procLabel(p)]));
    });
  }

  stationInput.addEventListener('change', loadProcs);
  if (c) loadProcs();

  var overlay = el('div', { className: 'modal-overlay active' }, [
    el('div', { className: 'modal', style: { maxWidth: '520px' } }, [
      el('h2', {}, isEdit ? 'Edit Cell' : 'New Cell'),
      stationList,
      field('Cell ID (e.g. SNF2)', cellIdInput),
      field('Display name', displayInput),
      field('Station', stationInput),
      field('Primary Process', primarySelect),
      field('Sub-processes (feed the primary)', subsBox),
      el('div', { style: { display: 'flex', gap: '0.5rem', justifyContent: 'flex-end', marginTop: '1rem' } }, [
        el('button', { className: 'btn', onclick: close }, 'Cancel'),
        el('button', { className: 'btn btn-primary', onclick: doSave }, isEdit ? 'Save' : 'Create')
      ])
    ])
  ]);
  document.body.appendChild(overlay);
  setTimeout(function () { (isEdit ? stationInput : cellIdInput).focus(); }, 0);

  function close() { overlay.remove(); }

  function doSave() {
    var cellID = cellIdInput.value.trim();
    var station = stationInput.value.trim();
    var primary = Number(primarySelect.value);
    if (!cellID) { toast('Cell ID is required', 'error'); return; }
    if (!station) { toast('Station is required', 'error'); return; }
    if (!primary) { toast('Pick a primary Process', 'error'); return; }
    // A primary can't also be a sub of itself.
    var subs = Array.from(wantSubs).filter(function (s) { return s !== primary; });
    apiPost('/api/cells', {
      cell_id: cellID,
      station: station,
      primary_process_id: primary,
      sub_process_ids: subs,
      display_name: displayInput.value.trim()
    }).then(function () {
      toast(isEdit ? 'Saved' : 'Created', 'success');
      close();
      load();
    }).catch(function (e) {
      toast('Save failed: ' + e, 'error');
    });
  }
}

document.getElementById('cell-new').addEventListener('click', function () { openModal(null); });
load();
