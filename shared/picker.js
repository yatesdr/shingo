// picker.js — the picker's shell (style guide §34): a search box, the rows it
// filters, the selection as chips, and an "Add and use" slot. It knows
// nothing about nodes or parts. An adapter hands it rows and decides what each
// row may be; the shell draws them, says why an excluded one is excluded, and
// moves the selection. Extracted from the Processes page's own node picker,
// which it replaced (S3): every node and part picker in the apps is this one.
//
// A ROW is { value, label, section?, note?, reason?, members?, values?,
// search? }:
//   section  a heading drawn above the first row of each run of the same one
//   note     a quiet line on a row that can be picked
//   reason   why this row cannot be picked: it is drawn greyed with the
//            reason beside it, and neither a click nor the keyboard reaches it
//            (an exclusion is shown, never silently dropped)
//   members  rows drawn indented under this one (a group's nodes)
//   values   what picking the row picks, when it is not [value] (a group that
//            stands for its members); the row is on when all of them are
//   search   more text the search box matches besides label and value
//
// ONE LIST OPEN AT A TIME. A dropdown picker's list is a popover on the layer
// (layer.js), so Escape and a press outside close it before the sheet under
// it, and opening one picker's list closes another's. A picker made with
// open: true (the positions checklist) keeps its list up and is not a layer.
//
// THE SEARCH BOX IS NEVER REDRAWN: a keystroke redraws the rows and the chips,
// never the field under the caret. It is marked data-scratch, so what is typed
// into it is not work a sheet asks before discarding; a selection that moved
// marks the picker data-dirty, which is.

import { el } from '/static/shared/dom.js';
import { openLayer } from '/static/shared/layer.js';

let seq = 0;
let dropped = null;   // the picker whose dropdown list is open

// createPicker draws a picker into host (its content replaced) and returns it.
// opts:
//   rows         an array of rows, or a function returning one; null means
//                "not read yet" and draws the loading line
//   multi        a multi-select: a pick toggles, the list stays up
//   open         true: the list is always up (a checklist)
//   selected     the values picked at the start
//   placeholder  the search box's hint
//   filters      [{label, query}]: buttons that put query in the search box
//                (a second press clears it)
//   chips        false: draw no chips (the control that opened it shows the
//                value)
//   empty(q)     the line when nothing matches; loading, the not-read-yet line
//   add          the "Add and use" slot: { label(q), run(q) }. run makes the
//                thing typed and resolves its value or row (null: nothing was
//                made, the door already said why); the shell then picks it
//   onChange(value, picker)  after the engineer moved the selection
//   onPick(next, row, picker)  WRITE-THROUGH: a pick (a row, a chip's ×) is
//                the caller's write, not the picker's. next is the selection
//                it would make; the picked row shows pending and the chips do
//                not move until what onPick returns (or resolves to) answers:
//                the selection as the server now holds it (an array, or
//                {values}), drawn as clean; or {error} (or nothing), a
//                refusal, and the chips stay as they were with the reason
//                beside them. One pick at a time; no timer.
//
// The picker: { el, input, value(), set(values, {clean}), markClean(),
// refresh(), open(), close(), focus(), destroy() }. value() is an array for a
// multi-select and a string ('' for none) for a single one.
export function createPicker(host, opts) {
    const o = opts || {};
    const id = 'picker-' + (++seq);
    const multi = !!o.multi;
    const always = !!o.open;
    const st = { sel: list(o.selected), was: [], q: '', active: -1, extra: [], adding: false, layer: null, keys: [], pending: null, refused: '' };
    st.was = st.sel.slice();

    const root = el('div', { className: 'picker' + (multi ? ' picker--multi' : '') + (always ? ' picker--open' : '') });
    const chips = o.chips === false ? null : el('div', { className: 'picker-chips' });
    const input = el('input', {
        className: 'picker-q', type: 'text', autocomplete: 'off', role: 'combobox',
        'aria-autocomplete': 'list', 'aria-controls': id, 'aria-expanded': always ? 'true' : 'false',
        'data-scratch': '', placeholder: o.placeholder || null,
    });
    const filters = o.filters && o.filters.length ? el('div', { className: 'picker-filters' }) : null;
    const box = el('div', { className: 'picker-list', id, role: 'listbox', 'aria-multiselectable': multi ? 'true' : null });
    box.hidden = !always;
    // A write-through pick's refusal, beside the chips it left as they were.
    const refusal = o.onPick ? el('div', { className: 'picker-refused', role: 'status' }) : null;
    if (refusal) refusal.hidden = true;
    for (const part of [chips, refusal, input, filters, box]) if (part) root.appendChild(part);
    host.textContent = '';
    host.appendChild(root);

    const picker = {
        el: root,
        input,
        value: () => (multi ? st.sel.slice() : (st.sel[0] || '')),
        set(values, setOpts) {
            st.sel = list(values);
            if (!multi) st.sel = st.sel.slice(0, 1);
            if (setOpts && setOpts.clean) st.was = st.sel.slice();
            draw();
        },
        markClean() { st.was = st.sel.slice(); dirty(); },
        refresh: () => draw(),
        open: () => openList(),
        close: () => closeList(),
        focus: () => input.focus(),
        destroy() { closeList(); root.remove(); },
    };

    input.addEventListener('input', () => {
        st.q = input.value;
        st.active = st.q.trim() ? 0 : -1;
        openList();
        drawRows();
    });
    input.addEventListener('focus', () => openList());
    input.addEventListener('keydown', (ev) => {
        if (ev.key === 'ArrowDown' || ev.key === 'ArrowUp') {
            ev.preventDefault();
            openList();
            move(ev.key === 'ArrowDown' ? 1 : -1);
        } else if (ev.key === 'Enter') {
            const k = st.keys[st.active];
            if (!k || box.hidden) return;
            ev.preventDefault();
            choose(k);
        }
    });
    // A press on a row must not take focus out of the search box: the blur
    // would be the engineer leaving the picker, and the row's click would land
    // on a list that had gone.
    box.addEventListener('mousedown', (ev) => ev.preventDefault());
    box.addEventListener('click', (ev) => {
        const hit = ev.target.closest ? ev.target.closest('.picker-row, .picker-add') : null;
        const k = hit && st.keys.find((x) => x.node === hit);
        if (k) choose(k);
    });
    if (chips) {
        chips.addEventListener('click', (ev) => {
            const b = ev.target.closest ? ev.target.closest('[data-drop]') : null;
            if (!b) return;
            const v = b.getAttribute('data-drop');
            commit(st.sel.filter((x) => x !== v), v);
        });
    }
    if (filters) {
        filters.addEventListener('click', (ev) => {
            const b = ev.target.closest ? ev.target.closest('[data-query]') : null;
            if (!b) return;
            const q = b.getAttribute('data-query');
            st.q = st.q === q ? '' : q;
            input.value = st.q;
            st.active = -1;
            openList();
            draw();
        });
    }

    // ─── drawing ────────────────────────────────────────────────────────────

    function rowsNow() {
        const r = typeof o.rows === 'function' ? o.rows() : o.rows;
        return r === null || r === undefined ? null : r;
    }

    function everyRow() {
        const out = [];
        for (const r of (rowsNow() || []).concat(st.extra)) {
            out.push(r);
            for (const m of r.members || []) out.push(m);
        }
        return out;
    }

    function labelOf(value) {
        const r = everyRow().find((x) => String(x.value) === value);
        return r && r.label !== undefined ? String(r.label) : value;
    }

    function draw() {
        dirty();
        if (refusal) { refusal.textContent = st.refused; refusal.hidden = !st.refused; }
        drawChips();
        drawFilters();
        drawRows();
    }

    function drawChips() {
        if (!chips) return;
        chips.textContent = '';
        for (const v of st.sel) {
            chips.appendChild(el('span', { className: 'picker-chip' }, [
                labelOf(v),
                el('button', { type: 'button', 'data-drop': v, 'aria-label': 'Remove ' + labelOf(v) }, '×'),
            ]));
        }
    }

    function drawFilters() {
        if (!filters) return;
        filters.textContent = '';
        for (const f of o.filters) {
            filters.appendChild(el('button', {
                type: 'button', className: st.q === f.query ? 'is-on' : null, 'data-query': f.query,
            }, f.label));
        }
    }

    function drawRows() {
        st.keys = [];
        if (box.hidden) return;
        box.textContent = '';
        const all = rowsNow();
        if (all === null) {
            box.appendChild(el('div', { className: 'picker-none is-loading' }, o.loading || 'Loading…'));
            return;
        }
        const q = st.q.trim().toLowerCase();
        const seen = new Set(all.map((r) => String(r.value)));
        let section = null;
        let shown = 0;
        for (const r of all.concat(st.extra.filter((x) => !seen.has(String(x.value))))) {
            const self = hits(r, q);
            const members = (r.members || []).filter((m) => self || hits(m, q));
            if (!self && !members.length) continue;
            if (r.section && r.section !== section) {
                section = r.section;
                box.appendChild(el('div', { className: 'picker-sect', role: 'presentation' }, r.section));
            }
            row(r, false);
            for (const m of members) row(m, true);
            shown++;
        }
        if (o.add && q && !everyRow().some((r) => String(r.label === undefined ? r.value : r.label).toLowerCase() === q
            || String(r.value).toLowerCase() === q)) {
            const words = st.adding ? 'Adding…' : (o.add.label ? o.add.label(st.q.trim()) : 'Add “' + st.q.trim() + '” and use it');
            const node = el('div', {
                className: 'picker-add' + (st.adding ? ' is-busy' : ''), role: 'option',
                id: id + '-add', 'aria-disabled': st.adding ? 'true' : null,
            }, words);
            box.appendChild(node);
            if (!st.adding) st.keys.push({ add: true, node });
            shown++;
        }
        if (!shown) box.appendChild(el('div', { className: 'picker-none' }, o.empty ? o.empty(st.q.trim()) : 'Nothing matches'));
        if (st.active >= st.keys.length) st.active = st.keys.length - 1;
        mark();
    }

    function row(r, member) {
        const vals = valuesOf(r);
        const on = vals.length > 0 && vals.every((v) => st.sel.indexOf(v) >= 0);
        const off = !!r.reason;
        const node = el('div', {
            className: 'picker-row' + (member ? ' picker-row--member' : '') + (on ? ' is-on' : '') + (off ? ' is-off' : '') +
                (st.pending !== null && st.pending === String(r.value) ? ' is-pending' : ''),
            'aria-busy': st.pending !== null && st.pending === String(r.value) ? 'true' : null,
            role: 'option', id: id + '-' + (box.children.length + 1),
            'aria-selected': on ? 'true' : 'false', 'aria-disabled': off ? 'true' : null,
            'data-value': String(r.value),
        }, [
            multi ? el('i', { className: 'picker-box', 'aria-hidden': 'true' }) : null,
            el('span', { className: 'picker-label' }, r.label === undefined ? String(r.value) : String(r.label)),
            off ? el('small', { className: 'picker-why' }, String(r.reason))
                : (r.note ? el('small', { className: 'picker-note' }, String(r.note)) : null),
        ]);
        box.appendChild(node);
        if (!off) st.keys.push({ row: r, node });
    }

    function mark() {
        st.keys.forEach((k, i) => {
            if (i === st.active) k.node.classList.add('is-active'); else k.node.classList.remove('is-active');
        });
        const cur = st.keys[st.active];
        if (cur) {
            input.setAttribute('aria-activedescendant', cur.node.id);
            if (typeof cur.node.scrollIntoView === 'function') cur.node.scrollIntoView({ block: 'nearest' });
        } else {
            input.removeAttribute('aria-activedescendant');
        }
    }

    function dirty() {
        const same = st.sel.length === st.was.length && st.sel.every((v) => st.was.indexOf(v) >= 0);
        if (same) root.removeAttribute('data-dirty'); else root.setAttribute('data-dirty', '');
    }

    // ─── choosing ───────────────────────────────────────────────────────────

    function move(d) {
        if (!st.keys.length) return;
        st.active = st.active < 0 ? (d > 0 ? 0 : st.keys.length - 1)
            : Math.max(0, Math.min(st.keys.length - 1, st.active + d));
        mark();
    }

    function choose(k) {
        if (k.add) { void addAndUse(); return; }
        pick(valuesOf(k.row), String(k.row.value));
    }

    function pick(vals, value) {
        let next;
        if (multi) {
            const allOn = vals.every((v) => st.sel.indexOf(v) >= 0);
            next = allOn ? st.sel.filter((v) => vals.indexOf(v) < 0)
                : st.sel.concat(vals.filter((v) => st.sel.indexOf(v) < 0));
        } else {
            next = vals.slice(0, 1);
        }
        commit(next, value);
        if (!multi && !o.onPick) closeList();
    }

    // commit moves the selection to next: at once, or, write-through, when
    // the caller's write answers (see onPick). A pick made while one is out
    // is not taken: the first answer has not said where the selection is.
    function commit(next, value) {
        if (!o.onPick) { st.sel = next; changed(); return; }
        if (st.pending !== null) return;
        st.pending = value === undefined ? '' : String(value);
        st.refused = '';
        draw();
        const done = (ans) => {
            st.pending = null;
            const vals = Array.isArray(ans) ? ans : (ans && Array.isArray(ans.values) ? ans.values : null);
            if (vals) {
                st.sel = list(vals);
                if (!multi) st.sel = st.sel.slice(0, 1);
                st.was = st.sel.slice();
            } else {
                st.refused = ans && ans.error ? String(ans.error) : '';
            }
            draw();
            if (vals && o.onChange) o.onChange(picker.value(), picker);
            if (vals && !multi) closeList();
        };
        let ans;
        try { ans = o.onPick(next.slice(), { value: st.pending }, picker); } catch (e) { done({ error: String((e && e.message) || e) }); return; }
        if (ans && typeof ans.then === 'function') ans.then(done, (e) => done({ error: String((e && e.message) || e) }));
        else done(ans);
    }

    async function addAndUse() {
        if (st.adding || !o.add || typeof o.add.run !== 'function') return;
        const q = st.q.trim();
        st.adding = true;
        drawRows();
        let got = null;
        try {
            got = await o.add.run(q);
        } finally {
            st.adding = false;
        }
        if (got === null || got === undefined || got === false) { drawRows(); return; }
        const r = typeof got === 'object' ? got : { value: got, label: String(got) };
        if (!everyRow().some((x) => String(x.value) === String(r.value))) st.extra.push(r);
        st.q = '';
        input.value = '';
        st.active = -1;
        const v = String(r.value);
        if (multi) { if (st.sel.indexOf(v) < 0) st.sel = st.sel.concat([v]); } else st.sel = [v];
        changed();
        if (!multi) closeList();
    }

    function changed() {
        draw();
        if (o.onChange) o.onChange(picker.value(), picker);
    }

    // ─── the list as a popover ──────────────────────────────────────────────

    function openList() {
        if (always || st.layer) return;
        if (dropped && dropped !== picker) dropped.close();
        dropped = picker;
        st.layer = openLayer(box, {
            kind: 'popover', anchor: root, focus: false,
            onClose: () => {
                st.layer = null;
                st.active = -1;
                if (dropped === picker) dropped = null;
                input.setAttribute('aria-expanded', 'false');
                input.removeAttribute('aria-activedescendant');
            },
        });
        input.setAttribute('aria-expanded', 'true');
        drawRows();
    }

    function closeList() {
        if (st.layer) st.layer.close('close');
    }

    draw();
    return picker;
}

function list(v) {
    if (v === null || v === undefined || v === '') return [];
    return (Array.isArray(v) ? v : [v]).map(String);
}

function valuesOf(r) {
    return (Array.isArray(r.values) ? r.values : [r.value]).map(String);
}

function hits(r, q) {
    if (!q) return true;
    return [r.label, r.value, r.search].some((t) => t !== undefined && t !== null && String(t).toLowerCase().indexOf(q) >= 0);
}
