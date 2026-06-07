// BarList — horizontal label / bar / value list (plan §3.F breakdowns, §3.E
// parts). Small lists (≤10 rows) rebuilt on each render — the bar widths
// depend on the whole set's max, so a full rebuild is simpler and correct
// than reconciling. Rows are XSS-safe (textContent, not innerHTML).
//
//   renderBarList(container, rows, {
//     label: r => r.label,        // left text
//     value: r => r.count + '',   // right text
//     raw:   r => r.count,        // numeric magnitude for the bar width
//     onClick: r => {...},        // optional → clickable rows
//     color: 'var(--info)',       // optional fill color
//   });

import { el } from '/static/app.js';

export function renderBarList(container, rows, opts) {
    if (!container) return;
    container.innerHTML = '';
    if (!rows || !rows.length) {
        container.appendChild(el('div', { className: 'dash-empty' }, 'No data in this window.'));
        return;
    }
    let max = 0;
    for (const r of rows) max = Math.max(max, opts.raw(r) || 0);
    if (max <= 0) max = 1;

    for (const r of rows) {
        const row = el('div', { className: 'bar-row' + (opts.onClick ? ' bar-row--clickable' : '') });
        const labelText = String(opts.label(r));
        row.appendChild(el('span', { className: 'bar-row__label', title: labelText }, labelText));
        const track = el('span', { className: 'bar-row__track' });
        const fill = el('span', { className: 'bar-row__fill' });
        fill.style.width = (Math.max(0, Math.min(1, (opts.raw(r) || 0) / max)) * 100).toFixed(1) + '%';
        if (opts.color) fill.style.background = opts.color;
        track.appendChild(fill);
        row.appendChild(track);
        row.appendChild(el('span', { className: 'bar-row__value' }, String(opts.value(r))));
        if (opts.onClick) row.addEventListener('click', () => opts.onClick(r));
        container.appendChild(row);
    }
}
