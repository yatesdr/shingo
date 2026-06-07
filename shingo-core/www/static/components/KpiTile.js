// KpiTile — a hero KPI tile: big tabular value, label, delta arrow, optional
// sparkline along the bottom edge (plan §3.A, §4). Returns a .card.kpi-tile
// node; updateKpiTile() refreshes one in place so SSE/filter updates don't
// rebuild the DOM.
//
//   const tile = KpiTile({ id:'success', label:'Success', value:'98.4%',
//       delta:{ dir:'up', text:'1.2pt', good:true }, spark:[...], drill:'success_rate' });
//   updateKpiTile(tile, { ...newSpec });
//
// spec:
//   id        stable key (used as data-kpi and for reconcileList)
//   label     small uppercase caption
//   value     big number; null/''/undefined renders as the em-dash cold state (§8 #19)
//   sub       small muted subtitle (e.g. "P50 4m 02s")
//   delta     { dir:'up'|'down'|'flat', text:'1.2pt', good:true|false }
//             dir picks the arrow; good picks the color (up≠always-good — a
//             falling duration is good). Omit good for a neutral delta.
//   spark     array of numbers for the bottom sparkline
//   sparkColor / tone ('good'|'bad') optional styling
//   drill     metric key → makes the tile clickable (data-action="openDrill:<key>")

import { el, h } from '/static/app.js';
import { Sparkline } from '/static/components/Sparkline.js';

export function KpiTile(spec) {
    const tile = el('div', { className: 'card kpi-tile' });
    if (spec.id) tile.dataset.kpi = spec.id;
    if (spec.drill) {
        tile.classList.add('kpi-tile--clickable');
        tile.dataset.action = 'openDrill:' + spec.drill;
        tile.setAttribute('role', 'button');
        tile.setAttribute('tabindex', '0');
    }
    tile.innerHTML = h`
        <div class="kpi-label">${spec.label || ''}</div>
        <div class="kpi-value">—</div>
        <div class="kpi-sub"></div>
        <div class="kpi-delta"></div>
        <div class="kpi-spark"></div>`;
    updateKpiTile(tile, spec);
    return tile;
}

export function updateKpiTile(tile, spec) {
    if (!tile) return;
    const labelEl = tile.querySelector('.kpi-label');
    if (labelEl) labelEl.textContent = spec.label || '';

    const valueEl = tile.querySelector('.kpi-value');
    if (valueEl) {
        const v = spec.value;
        valueEl.textContent = (v === null || v === undefined || v === '') ? '—' : String(v);
    }

    const subEl = tile.querySelector('.kpi-sub');
    if (subEl) {
        subEl.textContent = spec.sub || '';
        subEl.style.display = spec.sub ? '' : 'none';
    }

    const deltaEl = tile.querySelector('.kpi-delta');
    if (deltaEl) {
        deltaEl.className = 'kpi-delta';
        if (spec.delta && spec.delta.text) {
            const dir = spec.delta.dir || 'flat';
            const arrow = dir === 'up' ? '▲' : dir === 'down' ? '▼' : '→';
            deltaEl.textContent = arrow + ' ' + spec.delta.text;
            if (spec.delta.good === true) deltaEl.classList.add('kpi-delta--up');
            else if (spec.delta.good === false) deltaEl.classList.add('kpi-delta--down');
            deltaEl.style.display = '';
        } else {
            deltaEl.textContent = '';
            deltaEl.style.display = 'none';
        }
    }

    tile.classList.remove('kpi-tile--good', 'kpi-tile--bad');
    if (spec.tone === 'good') tile.classList.add('kpi-tile--good');
    else if (spec.tone === 'bad') tile.classList.add('kpi-tile--bad');

    const sparkHolder = tile.querySelector('.kpi-spark');
    if (sparkHolder) {
        sparkHolder.textContent = '';
        if (Array.isArray(spec.spark) && spec.spark.length >= 2) {
            sparkHolder.appendChild(Sparkline(spec.spark, {
                color: spec.sparkColor || 'var(--primary)', width: 120, height: 18,
            }));
        }
    }
}
