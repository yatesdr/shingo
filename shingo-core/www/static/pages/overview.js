// Operations Overview (/overview) — exec/GM dashboard (plan §15).
//
// This module is the orchestrator: it owns the global filter store, the SSE
// liveness pill, the filter-bar wiring, and a registry of sections. Each
// section is a self-contained factory under pages/overview/ that exports
// { mount(), refresh(state) }. Sections are registered in buildSections()
// as the slice-0 sub-steps land; the orchestrator never has to be rewritten
// to add one. The shared drill modal (components/DrillModal.js) is opened by
// a delegated data-action="openDrill:<metric>" handler.

import { createStore, onSSE, debounce } from '/static/shared/utils.js';
import { apiGet, delegateActions } from '/static/app.js';
import { openDrillModal } from '/static/components/DrillModal.js';
import { createHeroSection } from '/static/pages/overview/hero.js';
import { createTrendsSection } from '/static/pages/overview/trends.js';
import { createFleetSection } from '/static/pages/overview/fleet.js';
import { createFootprintSection } from '/static/pages/overview/footprint.js';

const params = new URLSearchParams(location.search);
const DEMO = params.get('demo') === 'on';

// Global filter store (§2). range is the hero/footprint snapshot window;
// station/robot scope every section. Trends keep their own local window.
const filters = createStore({ range: 'today', station: '', robot: '', demo: DEMO });

// ─── sections ─────────────────────────────────────────────────────────────
const sections = [];
function buildSections() {
    sections.push(createHeroSection(filters));
    sections.push(createTrendsSection(filters));
    sections.push(createFleetSection(filters));
    sections.push(createFootprintSection(filters));
}

function refreshAll(state) {
    for (const s of sections) {
        try { s.refresh(state); } catch (e) { console.error('section refresh', e); }
    }
}

// Debounced so one filter toggle fans out as a single wave of fetches (§6).
const onFilterChange = debounce((state) => refreshAll(state), 150);

// ─── SSE liveness (foundation validation) ───────────────────────────────
function setLive(on) {
    const pill = document.getElementById('ops-live');
    if (!pill) return;
    pill.classList.toggle('is-live', on);
    pill.innerHTML = on ? '&#9679; live' : '&#9675; offline';
}
// 'connected' fires on every (re)connect of the shared bus — proves the SSE
// pipeline end-to-end and lets live sections re-fetch after a reconnect.
onSSE('connected', () => setLive(true));

// ─── filter bar ─────────────────────────────────────────────────────────
function initFilterBar() {
    const range = document.getElementById('ops-range');
    if (range) {
        range.value = filters.get().range;
        range.addEventListener('change', () => filters.set({ range: range.value }));
    }
    const station = document.getElementById('ops-station');
    if (station) station.addEventListener('change', () => filters.set({ station: station.value }));
    const robot = document.getElementById('ops-robot');
    if (robot) robot.addEventListener('change', () => filters.set({ robot: robot.value }));
    const refresh = document.getElementById('ops-refresh');
    if (refresh) refresh.addEventListener('click', () => refreshAll(filters.get()));
    if (DEMO) {
        const badge = document.getElementById('ops-demo-badge');
        if (badge) badge.style.display = '';
    }
}

// Populate station/robot dropdowns from existing public endpoints.
async function loadFilterOptions() {
    try {
        const stations = await apiGet('/api/stations');
        const sel = document.getElementById('ops-station');
        const list = Array.isArray(stations) ? stations : (stations && stations.stations) || [];
        if (sel) addOptions(sel, list, (s) => (typeof s === 'string' ? s : (s.id || s.station_id || s.name)));
    } catch (e) { /* non-fatal: filter still usable as "all" */ }
    try {
        const robots = await apiGet('/api/robots');
        const sel = document.getElementById('ops-robot');
        const list = Array.isArray(robots) ? robots : (robots && robots.robots) || [];
        if (sel) addOptions(sel, list, (r) => (typeof r === 'string' ? r : (r.vehicle_id || r.VehicleID || r.id)));
    } catch (e) { /* non-fatal */ }
}

function addOptions(sel, list, pick) {
    const seen = new Set();
    for (const item of list) {
        const id = pick(item);
        if (!id || seen.has(id)) continue;
        seen.add(id);
        const opt = document.createElement('option');
        opt.value = id; opt.textContent = id;
        sel.appendChild(opt);
    }
}

// ─── boot ─────────────────────────────────────────────────────────────────
function init() {
    // Drill-modal opener for any element with data-action="openDrill:<metric>".
    delegateActions(document.body, {
        openDrill: (metric, el) => openDrillModal(metric, filters.get()),
    });

    buildSections();
    initFilterBar();
    loadFilterOptions();
    filters.subscribe(onFilterChange);

    for (const s of sections) {
        try { s.mount(); } catch (e) { console.error('section mount', e); }
    }
    refreshAll(filters.get());
}

if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
else init();
