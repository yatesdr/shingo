// set-pick.js — one list, several boxes (style guide §34): a process's
// positions and its routing set picked off one node list. Each box is one
// selection in its own colour, showing what is in it; click a box, then tick
// nodes into it in the list below. Moved out of the Processes page, where it
// was setInit (2026-11-12, from HOP's round and the owner's sketch: "click the
// bucket above, use the same picker, and see what is selected under each
// bucket"), so the Add process sheet and Settings draw the one piece.
//
// THE BOXES SHOW THEIR PICKS. Each is its name, its count and its picks as
// chips (× takes one out); one the host says is still needed has a dashed
// outline in its colour and says "needed", an optional empty one says
// "optional". The colour is the box's data-role (shared/components.css, the
// --role-* tokens) and always comes with the box's name.
//
// THE LIST IS THE PICKER'S SHELL over the page's node list (node-picker.js),
// always open, with the field the host gives for the box that is open: its
// rule, notes, which rows wait under a group's "show" and which go behind
// "In use by other processes". A row in another box carries that box's name
// as a tag, in that box's colour.
//
// IT HOLDS A DRAFT. Nothing here writes: the host reads value(box) on its own
// Save, and onChange tells it a box moved (a host may move names between
// boxes then: a node made a position leaves the roles).

import { el } from '/static/shared/dom.js';
import { mountPicker, nodeRows } from '/static/shared/node-picker.js';

// How many chips a box shows before "+ N more".
const BOX_CHIPS = 8;

// createSetPick draws the set pick into host (its content replaced).
// opts:
//   boxes      [{ key, label, sub, role, need }] in order; role names the
//              colour (positions, inbound, staging, outbound); need is
//              'needed' (dashed until it holds one) or 'optional'
//   selected   { key: [names] } at the start
//   active     the box open at the start (the first by default)
//   source     the page's node source (nodeSource)
//   field(key) the node field for that box: nodeRows' rule, note, only,
//              members, later
//   closed(key) why a box cannot be opened (a routing set that could not be
//              read), or ''
//   note(key)  a short line drawn in the box under its chips, or ''
//   filters    the list's prefix filters, or a function returning them
//   later      the shell's { label } for the rows field.later sends behind
//   placeholder the list's search hint
//   onChange(key, set) after a box's picks moved
//
// The set: { el, value(key), values(), set(key, names, {clean}), active(),
// open(key), dirty(), markClean(), refresh(), destroy() }.
export function createSetPick(host, opts) {
    const o = opts || {};
    const boxes = o.boxes || [];
    const sel = {};
    for (const b of boxes) sel[b.key] = list((o.selected || {})[b.key]);
    let base = JSON.parse(JSON.stringify(sel));
    let active = boxes.some((b) => b.key === o.active) ? o.active : (boxes[0] || {}).key;
    const more = new Set();   // boxes whose chips are all shown

    const root = el('div', { className: 'setpick' });
    const row = el('div', { className: 'setpick-boxes' });
    const pane = el('div', { className: 'setpick-list' });
    const head = el('div', { className: 'setpick-head' });
    const listHost = el('div', { className: 'setpick-picker' });
    pane.appendChild(head);
    pane.appendChild(listHost);
    root.appendChild(row);
    root.appendChild(pane);
    host.textContent = '';
    host.appendChild(root);

    const boxOf = (key) => boxes.find((b) => b.key === key) || null;
    const closedWhy = (key) => (o.closed ? o.closed(key) || '' : '');

    // THE PREFIX FILTERS ARE THE LIST'S OWN NAMES, so a pick drawn before the
    // node list lands is drawn again once it does, with them.
    let picker = null;
    let filtered = 0;
    function mountList() {
        if (picker) picker.destroy();
        const filters = typeof o.filters === 'function' ? o.filters() : (o.filters || null);
        filtered = filters ? filters.length : 0;
        picker = mountPicker(listHost, o.source, (rows) => nodeRows(rows, fieldFor(active)), {
            multi: true,
            open: true,
            chips: false,
            selected: sel[active] || [],
            placeholder: o.placeholder || 'filter by name',
            filters,
            later: o.later || undefined,
            onChange: (v) => {
                sel[active] = list(v);
                moved(active);
            },
        });
        picker.set(sel[active] || [], { was: base[active] || [] });
    }
    mountList();
    const unwatch = typeof o.filters === 'function' && o.source
        ? o.source.watch(() => { if (!filtered && o.source.rows()) mountList(); }) : () => {};

    // fieldFor is the host's field for a box, with the other boxes' tags.
    function fieldFor(key) {
        const f = Object.assign({}, o.field ? o.field(key) : {});
        const own = f.tags;
        f.tags = (n) => boxes.filter((b) => b.key !== key && sel[b.key].indexOf(n.name) >= 0)
            .map((b) => ({ label: b.label, tone: b.role || b.key }))
            .concat(own ? own(n) || [] : []);
        return f;
    }

    function moved(key) {
        if (o.onChange) o.onChange(key, api);
        drawBoxes();
    }

    function drawBoxes() {
        row.textContent = '';
        for (const b of boxes) {
            const names = sel[b.key];
            const why = closedWhy(b.key);
            const needed = b.need === 'needed' && !names.length;
            const shown = more.has(b.key) || names.length <= BOX_CHIPS ? names : names.slice(0, BOX_CHIPS);
            const chips = el('div', { className: 'setpick-chips' }, shown.map((n) => el('span', { className: 'setpick-chip' }, [
                n, why ? null : el('button', { type: 'button', 'data-drop': n, 'data-box': b.key, 'aria-label': 'Take ' + n + ' out of ' + b.label }, '×'),
            ])));
            if (names.length > BOX_CHIPS) {
                chips.appendChild(el('button', { type: 'button', className: 'setpick-more', 'data-more': b.key },
                    more.has(b.key) ? 'fold them' : '+ ' + (names.length - BOX_CHIPS) + ' more'));
            }
            const mark = needed ? el('span', { className: 'setpick-need is-needed' }, 'needed')
                : b.need === 'optional' && !names.length ? el('span', { className: 'setpick-need' }, 'optional') : null;
            const note = !why && o.note ? o.note(b.key) : '';
            row.appendChild(el('div', {
                className: 'setpick-box' + (b.key === active ? ' is-on' : '') + (needed ? ' is-needed' : '') + (why ? ' is-closed' : ''),
                'data-role': b.role || b.key, 'data-box': b.key,
            }, [
                el('button', {
                    type: 'button', className: 'setpick-t', 'aria-pressed': b.key === active ? 'true' : 'false',
                    disabled: why ? 'disabled' : null,
                }, [el('i', { className: 'setpick-dot', 'aria-hidden': 'true' }), b.label, mark,
                    el('b', { className: 'setpick-n' }, why ? '–' : String(names.length))]),
                el('small', { className: 'setpick-sub' }, why || b.sub || ''),
                names.length ? chips : (why ? null : el('small', { className: 'setpick-none' }, needed ? 'nothing picked yet' : 'none')),
                note ? el('small', { className: 'setpick-note' }, note) : null,
            ]));
        }
        const b = boxOf(active);
        head.textContent = '';
        if (b) {
            head.appendChild(el('i', { className: 'setpick-dot', 'aria-hidden': 'true' }));
            head.appendChild(el('b', null, 'Picking ' + b.label));
            head.appendChild(el('small', null, b.sub || ''));
        }
        pane.setAttribute('data-role', b ? (b.role || b.key) : '');
        root.setAttribute('data-active', active || '');
        dirtyMark();
    }

    function dirtyMark() {
        if (api.dirty()) root.setAttribute('data-dirty', ''); else root.removeAttribute('data-dirty');
    }

    function open(key) {
        if (!boxOf(key) || closedWhy(key)) return;
        active = key;
        picker.set(sel[key], { was: base[key] });
        drawBoxes();
    }

    row.addEventListener('click', (ev) => {
        const t = ev.target;
        const drop = t.closest ? t.closest('[data-drop]') : null;
        if (drop) {
            const key = drop.getAttribute('data-box');
            sel[key] = sel[key].filter((n) => n !== drop.getAttribute('data-drop'));
            if (key === active) picker.set(sel[key], { was: base[key] });
            moved(key);
            return;
        }
        const m = t.closest ? t.closest('[data-more]') : null;
        if (m) {
            const key = m.getAttribute('data-more');
            if (more.has(key)) more.delete(key); else more.add(key);
            drawBoxes();
            return;
        }
        const box = t.closest ? t.closest('.setpick-box') : null;
        if (box) open(box.getAttribute('data-box'));
    });

    const api = {
        el: root,
        value: (key) => list(sel[key]),
        values: () => JSON.parse(JSON.stringify(sel)),
        active: () => active,
        open,
        set(key, names, setOpts) {
            if (!(key in sel)) return;
            sel[key] = list(names);
            if (setOpts && setOpts.clean) base[key] = sel[key].slice();
            if (key === active) picker.set(sel[key], { was: base[key] });
            drawBoxes();
        },
        dirty: () => JSON.stringify(sel) !== JSON.stringify(base),
        markClean() {
            base = JSON.parse(JSON.stringify(sel));
            picker.set(sel[active], { clean: true });
            drawBoxes();
        },
        refresh() { picker.refresh(); drawBoxes(); },
        destroy() { unwatch(); picker.destroy(); root.remove(); },
    };
    drawBoxes();
    return api;
}

function list(v) {
    if (v === null || v === undefined || v === '') return [];
    return (Array.isArray(v) ? v : [v]).map(String);
}
