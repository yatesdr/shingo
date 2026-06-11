// dashboard-landing.js — the "/" Dashboard page, now the dashboard HUB
// (consolidation refactor #3). You see, make, and open every dashboard here,
// in-core; the old standalone /dashboards Manage table is retired. Also wires
// the "Re-sync edges" button (Q-034: ask edges to re-send their catalog).
//
// Create/edit logic is ported from the old dashboards.js so the modal behaves
// identically (kind picker + station-scope checkboxes). Edit/Delete only render
// when authenticated — detected from the presence of the auth-gated "+ New"
// button the template emits.

import { el, apiGet, apiPost, apiPut, apiDelete, toast, uiConfirm } from '/static/app.js';

// Known kinds. A kind needs a renderer template + dashboard.js branch to display.
const KINDS = [
    { value: 'heartbeat', label: 'Heartbeat' },
    { value: 'task-board', label: 'Flight Board' },
    { value: 'robot-map', label: 'Robot Map' },
];
const kindLabel = (k) => (KINDS.find((x) => x.value === k) || {}).label || k;

const canEdit = !!document.getElementById('dash-new');
let dashboards = [];

// ── Re-sync edges button (Q-034) ────────────────────────────────────────────
const resync = document.getElementById('resync-edges');
if (resync) {
    resync.addEventListener('click', () => {
        resync.disabled = true;
        apiPost('/api/edges/reregister', {})
            .then(() => toast('Re-sync requested — edges will re-send their catalog', 'success'))
            .catch((e) => toast('Re-sync failed: ' + (e && e.message ? e.message : e), 'error'))
            .finally(() => setTimeout(() => { resync.disabled = false; }, 1500));
    });
}

// ── Hub: dashboard cards ─────────────────────────────────────────────────────
function load() {
    apiGet('/api/dashboards')
        .then((list) => { dashboards = list || []; render(); })
        .catch((e) => toast('Load dashboards failed: ' + e, 'error'));
}

function render() {
    const grid = document.getElementById('dash-cards');
    if (!grid) return;
    grid.innerHTML = '';

    // Overview is a built-in analytical view, not a stored dashboard — surface it
    // here so the hub is the one place for "all dashboards".
    grid.appendChild(card({
        name: 'Overview', kindText: 'Built-in', meta: 'Plant-wide mission analytics',
        open: '/overview', fullscreen: null, edit: null,
    }));

    if (!dashboards.length) {
        grid.appendChild(el('div', { className: 'dash-empty' },
            canEdit ? 'No saved dashboards yet — “+ New dashboard” to make one.' : 'No saved dashboards yet.'));
        return;
    }
    dashboards.forEach((d) => {
        const scope = (d.stations && d.stations.length) ? d.stations.join(', ') : 'Whole plant';
        grid.appendChild(card({
            name: d.name, kindText: kindLabel(d.kind), meta: scope + (d.enabled ? '' : ' · disabled'),
            open: '/dashboard/' + d.id, fullscreen: '/dashboard/' + d.id + '?kiosk=1',
            edit: canEdit ? d : null,
        }));
    });
}

function card(o) {
    const actions = [el('a', { className: 'btn btn-sm btn-primary', href: o.open }, 'Open')];
    if (o.fullscreen) {
        actions.push(el('a', { className: 'btn btn-sm', href: o.fullscreen, target: '_blank', rel: 'noopener' }, 'Fullscreen'));
    }
    if (o.edit) {
        actions.push(el('button', { className: 'btn btn-sm', onclick: () => openModal(o.edit) }, 'Edit'));
        actions.push(el('button', { className: 'btn btn-sm btn-danger', onclick: () => removeDashboard(o.edit) }, 'Delete'));
    }
    return el('div', { className: 'dash-card' }, [
        el('div', { className: 'dash-card-kind' }, o.kindText),
        el('h3', {}, o.name),
        el('div', { className: 'dash-card-meta' }, o.meta),
        el('div', { className: 'dash-card-actions' }, actions),
    ]);
}

function removeDashboard(d) {
    uiConfirm('Delete dashboard "' + d.name + '"? Its display link will stop working.').then((ok) => {
        if (!ok) return;
        apiDelete('/api/dashboards/' + d.id)
            .then(() => { toast('Deleted', 'success'); load(); })
            .catch((e) => toast('Delete failed: ' + e, 'error'));
    });
}

// ── Create / edit modal ──────────────────────────────────────────────────────
function field(label, control) {
    return el('div', { style: { marginBottom: '0.75rem' } }, [
        el('label', { style: { display: 'block', marginBottom: '0.25rem', fontWeight: '600' } }, label),
        control,
    ]);
}

function openModal(d) {
    const isEdit = !!d;
    const nameInput = el('input', { className: 'form-input', type: 'text', value: d ? d.name : '' });
    const kindSelect = el('select', { className: 'form-input' }, KINDS.map((k) => el('option', { value: k.value }, k.label)));
    if (d) kindSelect.value = d.kind;

    // Station scope: real station IDs as checkboxes (a typo silently scopes to
    // nothing). Already-saved values absent from the live list are shown flagged.
    const selected = new Set((d && d.stations) ? d.stations : []);
    const pickerBox = el('div', { className: 'form-input', style: { maxHeight: '180px', overflowY: 'auto', padding: '0.4rem 0.6rem' } },
        el('span', { className: 'muted' }, 'Loading stations…'));
    apiGet('/api/stations').then((list) => renderPicker(list || [])).catch(() => renderPicker([]));

    function renderPicker(available) {
        pickerBox.innerHTML = '';
        const all = available.slice();
        selected.forEach((s) => { if (all.indexOf(s) === -1) all.push(s); });
        all.sort();
        if (!all.length) {
            pickerBox.appendChild(el('span', { className: 'muted' }, 'No stations seen yet — leave empty for whole plant.'));
            return;
        }
        all.forEach((s) => {
            const cb = el('input', { type: 'checkbox' });
            cb.checked = selected.has(s);
            cb.addEventListener('change', () => { if (cb.checked) selected.add(s); else selected.delete(s); });
            const parts = [cb, ' ' + s];
            if (available.indexOf(s) === -1) parts.push(el('span', { className: 'muted' }, ' — not a known station; uncheck to drop'));
            pickerBox.appendChild(el('label', { style: { display: 'flex', alignItems: 'center', gap: '0.4rem', padding: '0.15rem 0', fontWeight: '400' } }, parts));
        });
    }

    // Heartbeat cell setup (refactor #4): which catalog cells show on this board
    // + optional rename. Stored in the dashboard's config — this is "cell config
    // tied to the dashboard menu" rather than a separate /admin/cells page. Only
    // shown for the heartbeat kind.
    const cellRows = []; // { label, showCb, nameInput }
    const cellsBox = el('div', { className: 'form-input', style: { maxHeight: '200px', overflowY: 'auto', padding: '0.4rem 0.6rem' } },
        el('span', { className: 'muted' }, 'Loading cells…'));
    const cellsField = field('Cells (uncheck to hide · rename optional)', cellsBox);
    let existingCells = {};
    try {
        const raw = d && d.config ? (typeof d.config === 'string' ? JSON.parse(d.config) : d.config) : {};
        existingCells = (raw && raw.cells) || {};
    } catch (_) { existingCells = {}; }
    apiGet('/api/cells/catalog').then((list) => renderCells(list || [])).catch(() => renderCells([]));

    function renderCells(cats) {
        cellsBox.innerHTML = '';
        cellRows.length = 0;
        if (!cats.length) {
            cellsBox.appendChild(el('span', { className: 'muted' }, 'No cells reported yet — they appear once an edge registers.'));
            return;
        }
        cats.forEach((c) => {
            const label = c.cell_label;
            const ov = existingCells[label] || {};
            const showCb = el('input', { type: 'checkbox' });
            showCb.checked = !ov.hide;
            const nameInput = el('input', { className: 'form-input', type: 'text', placeholder: label, value: ov.name || '', style: { flex: '1', minWidth: '0' } });
            cellRows.push({ label, showCb, nameInput });
            cellsBox.appendChild(el('label', { style: { display: 'flex', alignItems: 'center', gap: '0.4rem', padding: '0.15rem 0', fontWeight: '400' } },
                [showCb, el('span', { style: { minWidth: '90px' } }, label), nameInput]));
        });
    }
    function buildConfig() {
        const cells = {};
        cellRows.forEach((row) => {
            const hide = !row.showCb.checked;
            const name = row.nameInput.value.trim();
            if (hide || name) cells[row.label] = Object.assign({}, hide ? { hide: true } : {}, name ? { name } : {});
        });
        return { cells };
    }

    const enabledInput = el('input', { type: 'checkbox' });
    enabledInput.checked = d ? !!d.enabled : true;

    const overlay = el('div', { className: 'modal-overlay active' }, [
        el('div', { className: 'modal', style: { maxWidth: '480px' } }, [
            el('h2', {}, isEdit ? 'Edit Dashboard' : 'New Dashboard'),
            field('Name', nameInput),
            field('Type', kindSelect),
            field('Area — stations (none selected = whole plant)', pickerBox),
            cellsField,
            el('label', { style: { display: 'flex', alignItems: 'center', gap: '0.5rem', marginBottom: '0.75rem' } }, [enabledInput, 'Enabled']),
            el('div', { style: { display: 'flex', gap: '0.5rem', justifyContent: 'flex-end', marginTop: '1rem' } }, [
                el('button', { className: 'btn', onclick: close }, 'Cancel'),
                el('button', { className: 'btn btn-primary', onclick: doSave }, isEdit ? 'Save' : 'Create'),
            ]),
        ]),
    ]);
    document.body.appendChild(overlay);
    syncKindUI();
    setTimeout(() => nameInput.focus(), 0);

    function syncKindUI() { cellsField.style.display = (kindSelect.value === 'heartbeat') ? '' : 'none'; }
    kindSelect.addEventListener('change', syncKindUI);

    function close() { overlay.remove(); }

    function doSave() {
        const name = nameInput.value.trim();
        if (!name) { toast('Name is required', 'error'); return; }
        const payload = { name, kind: kindSelect.value, stations: Array.from(selected), enabled: enabledInput.checked };
        if (kindSelect.value === 'heartbeat') payload.config = buildConfig();
        const req = isEdit ? apiPut('/api/dashboards/' + d.id, payload) : apiPost('/api/dashboards', payload);
        req.then(() => { toast(isEdit ? 'Saved' : 'Created', 'success'); close(); load(); })
            .catch((e) => toast('Save failed: ' + e, 'error'));
    }
}

const newBtn = document.getElementById('dash-new');
if (newBtn) newBtn.addEventListener('click', () => openModal(null));

load();
