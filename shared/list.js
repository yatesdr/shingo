// list.js — lists on a page, shared by Core and Edge: click-to-sort tables
// (installTableSort), keyed reconciling for lists a live stream updates
// (reconcileList), and the small store behind a filter bar (createStore).
//
// ES module, served at /static/shared/list.js; a page imports each helper from
// here. No imports, and nothing runs at import: loading it only defines.

// ─── Table sort ─────────────────────────────────────────────────────────
//
// installTableSort(root) wires click-to-sort onto any
// <table data-sortable> under `root` (default: document). Single-column,
// client-side, no persistence — first click on a header sorts ascending,
// repeat clicks toggle direction.
//
// Markup contract:
//   <table data-sortable>                      — opt in for the table
//     <thead>
//       <tr><th data-sort>Col</th>             — sortable column
//           <th>Actions</th></tr>               — not sortable (no data-sort)
//     </thead>
//     <tbody>
//       <tr><td>foo</td><td>…</td></tr>
//       <tr data-no-sort>…</tr>                 — pinned to bottom (e.g.
//                                                 "no rows" placeholder)
//     </tbody>
//   </table>
//
// Cell value selection (in order):
//   1. <td data-sort-value="X">  — explicit override; use for ISO time
//                                  cells whose visible text is localized.
//   2. The cell's textContent (trimmed).
//
// Comparison uses Intl.Collator with numeric:true so mixed text/number
// columns ("Order #2", "Order #10") sort intuitively without per-column
// type hints.
export function installTableSort(root) {
    root = root || document;
    const tables = root.querySelectorAll('table[data-sortable]');
    for (const table of tables) wireSortableTable(table);
}

function wireSortableTable(table) {
    if (table.__tableSortInstalled) return;
    table.__tableSortInstalled = true;

    const headers = Array.from(table.querySelectorAll('thead th[data-sort]'));
    let activeIdx = -1;
    let activeDir = 'asc';

    for (const th of headers) {
        th.style.cursor = 'pointer';
        th.style.userSelect = 'none';
        // Reserve space for the indicator so the header doesn't jump
        // width when it activates. The space is non-breaking: a plain one
        // let a narrow column wrap the empty indicator onto a second line,
        // and that two-line header sat higher than its neighbours.
        if (!th.querySelector('.sort-indicator')) {
            th.appendChild(document.createTextNode(' '));
            const span = document.createElement('span');
            span.className = 'sort-indicator';
            span.setAttribute('aria-hidden', 'true');
            th.appendChild(span);
        }
        th.addEventListener('click', () => {
            const idx = Array.prototype.indexOf.call(th.parentNode.children, th);
            if (idx === activeIdx) {
                activeDir = activeDir === 'asc' ? 'desc' : 'asc';
            } else {
                activeIdx = idx;
                activeDir = 'asc';
            }
            for (const h of headers) {
                const ind = h.querySelector('.sort-indicator');
                if (ind) ind.textContent = '';
            }
            const ai = th.querySelector('.sort-indicator');
            if (ai) ai.textContent = activeDir === 'asc' ? '▲' : '▼';
            sortTbodyRows(table, idx, activeDir);
        });
    }
}

function sortTbodyRows(table, columnIndex, dir) {
    const tbody = table.querySelector('tbody');
    if (!tbody) return;
    const all = Array.from(tbody.children).filter(n => n.tagName === 'TR');
    const sortable = [];
    const fixed = [];
    for (const r of all) {
        if (r.hasAttribute('data-no-sort')) fixed.push(r);
        else sortable.push(r);
    }
    const collator = new Intl.Collator(undefined, { numeric: true, sensitivity: 'base' });
    const sign = dir === 'desc' ? -1 : 1;
    sortable.sort((a, b) => sign * collator.compare(
        cellSortValue(a, columnIndex),
        cellSortValue(b, columnIndex)
    ));
    for (const r of sortable) tbody.appendChild(r);
    for (const r of fixed) tbody.appendChild(r);
}

function cellSortValue(row, columnIndex) {
    const td = row.children[columnIndex];
    if (!td) return '';
    if (td.hasAttribute('data-sort-value')) return td.getAttribute('data-sort-value');
    return (td.textContent || '').trim();
}

// ─── Dashboard primitives (mission-telemetry plan §6) ─────────────────────
//
// Four additions take the micro-framework from "good for page-scoped
// scripts" to "good for cross-page shared components driven by live event
// streams": reconcileList (keyed list diffing), createStore (observable for
// the global filter bar), and onSSE (single-connection multi-subscriber SSE
// bus). The fourth piece — the static/components/ directory — lives in Core.

// reconcileList — keyed create-or-update-or-remove for SSE-driven lists.
//
// Manages ALL direct children of `container` as a keyed set. For each item
// (in order) it looks up the existing node by key, creates one via
// create(item, key) if absent or updates it via update(node, item, key) if
// present, and reorders nodes to match `items`. Nodes whose key is absent
// from `items` are removed, and destroy(node, key) is called first so the
// node can release resources it owns — Chart.js instances, intervals,
// animation loops, onSSE unsubscribes. The destroy callback is mandatory
// for any item that owns such resources.
//
//   reconcileList(grid, robots, {
//     key:     r => r.vehicle_id,
//     create:  r => createRobotTile(r),
//     update:  (node, r) => updateRobotTile(node, r),
//     destroy: node => { if (node.__chart) node.__chart.destroy(); },
//     nodeKey: node => node.dataset.name,   // optional: adopt server DOM
//   });
//
// `nodeKey` (optional) lets the first reconcile adopt server-rendered
// children in place instead of rebuilding them (avoids a flash on pages
// whose initial list is template-rendered, e.g. /robots).
//
// NOT for the /heartbeat plant-rhythm strip — that is a streaming animation
// over a fixed pre-allocated ring buffer, not a keyed list (see §13).
//
// Assumes every direct child of `container` is a managed list item; give it
// a dedicated container, not one that also holds headers or other chrome.
export function reconcileList(container, items, opts) {
    if (!container || !opts) return;
    const key = opts.key;
    const create = opts.create;
    const update = opts.update;
    const destroy = opts.destroy;
    let map = container.__reconcileMap;
    if (!map) {
        map = new Map();
        container.__reconcileMap = map;
        if (typeof opts.nodeKey === 'function') {
            const kids = Array.prototype.slice.call(container.children);
            for (let i = 0; i < kids.length; i++) {
                const k = opts.nodeKey(kids[i]);
                if (k !== null && k !== undefined) map.set(String(k), kids[i]);
            }
        }
    }
    const seen = new Set();
    let prev = null;
    const list = items || [];
    for (let i = 0; i < list.length; i++) {
        const item = list[i];
        const k = String(key(item));
        if (seen.has(k)) continue; // defensive de-dupe on duplicate keys
        seen.add(k);
        let node = map.get(k);
        if (!node) {
            node = create(item, k);
            if (!node) continue;
            map.set(k, node);
        } else if (update) {
            update(node, item, k);
        }
        const ref = prev ? prev.nextSibling : container.firstChild;
        if (node !== ref) container.insertBefore(node, ref);
        prev = node;
    }
    map.forEach((node, k) => {
        if (seen.has(k)) return;
        map.delete(k);
        if (destroy) {
            try { destroy(node, k); } catch (e) { console.error('reconcileList destroy', e); }
        }
        if (node.parentNode) node.parentNode.removeChild(node);
    });
}

// createStore — minimal observable backing the global filter bar.
//
//   const store = createStore({ range: 'today', station: '', robot: '' });
//   const off = store.subscribe(s => rerender(s));   // returns unsubscribe
//   store.set({ station: 'STN-A' });                 // shallow-merge + notify
//   store.get();                                     // current snapshot
//
// set() shallow-merges the patch and notifies every subscriber with the new
// state. subscribe() returns an unsubscribe fn; sections MUST call it on
// teardown/re-render or the subscriber set grows on every filter change
// (slow leak). Wrap re-fetching subscribers in debounce() so one filter
// toggle doesn't fire N simultaneous HTTP requests.
export function createStore(initial) {
    let state = Object.assign({}, initial || {});
    const subs = new Set();
    return {
        get() { return state; },
        set(patch) {
            state = Object.assign({}, state, patch);
            const snapshot = state;
            Array.prototype.forEach.call(Array.from(subs), (fn) => {
                try { fn(snapshot); } catch (e) { console.error('store subscriber', e); }
            });
        },
        subscribe(fn) {
            subs.add(fn);
            return () => { subs.delete(fn); };
        },
    };
}
