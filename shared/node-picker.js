// node-picker.js — the node adapter over the picker's shell (style guide §34).
// A page reads its node list ONCE (nodeSource) and every node field on it
// draws from that one list through nodePicker, each with its own rule.
//
// THERE IS NO BLANKET RULE. A field says what it may not take and why
// (field.rule, built from nodeWhy's pieces), and the picker draws the excluded
// node greyed with that reason. The one node no field ever offers is
// _TRANSIT: it is where a bin is while a robot carries it, not a place.
//
// THE TREE IS THE PLANT'S. Core's list names a parent by id (parent_id); the
// Edge's names a member's group (group) or a group's members (members). A
// container is drawn first in its section with every node under it, at any
// depth, indented beneath it; a node two levels down reads "↳ name". A
// container with no zone of its own sits with the first node under it that
// has one. Names sort in code-unit order, the order Go's sort.Strings gives,
// so a list drawn by Go and one drawn here agree.
//
// A ROW'S VALUE is the node's name, or its id (field.value: 'id') for a door
// that takes ids.

import { el } from '/static/shared/dom.js';
import { createPicker } from '/static/shared/picker.js';

const TRANSIT = '_TRANSIT';
const ROBOT = '_ROBOT:';

// pickerSource is a page's one copy of a list a picker draws from: read once,
// shared by every picker on the page, re-asked after a failure (a failed read
// is not an empty plant, and is not kept as one). read() resolves an array, or
// the HTTP door's answer ({ok, body, error}); a rejection is a failure too.
// initial, when the page was rendered with the list, is the list already read.
//
// The source: { rows() (null: not read yet), error() ('' or why the last read
// failed), load(force) (one read in flight; a list already read is not read
// again unless forced), set(list) (a list pushed to the page), watch(fn) → off }.
export function pickerSource(read, normal, initial) {
    const shape = typeof normal === 'function' ? normal : (x) => x;
    let rows = initial === undefined || initial === null ? null : take(initial);
    let error = '';
    let req = null;
    const subs = [];
    function take(list) { return (Array.isArray(list) ? list : []).map(shape); }
    function notify() { for (const fn of subs.slice()) fn(); }
    return {
        rows: () => rows,
        error: () => error,
        load(force) {
            if (rows !== null && !force) return Promise.resolve(rows);
            if (req) return req;
            error = '';
            notify();
            let asked;
            try { asked = Promise.resolve(read()); } catch (e) { asked = Promise.reject(e); }
            req = asked
                .then((ans) => { rows = take(bodyOf(ans)); })
                .catch((e) => { error = String((e && e.message) || e || 'no answer'); })
                .then(() => { req = null; notify(); return rows; });
            return req;
        },
        set(list) { rows = take(list); error = ''; notify(); },
        watch(fn) {
            subs.push(fn);
            return () => { const i = subs.indexOf(fn); if (i >= 0) subs.splice(i, 1); };
        },
    };
}

// bodyOf is what a read answered: an array as it came, or the door's body.
function bodyOf(ans) {
    if (Array.isArray(ans) || ans === null || ans === undefined) return ans || [];
    if (typeof ans === 'object' && 'ok' in ans && 'status' in ans) {
        if (!ans.ok) throw new Error(ans.error || 'no answer');
        return Array.isArray(ans.body) ? ans.body : [];
    }
    throw new Error('not a list');
}

// nodeSource is the page's node list: pickerSource over Core's or the Edge's
// node rows, each turned into one shape (asNode).
export function nodeSource(read, initial) {
    return pickerSource(read, asNode, initial);
}

// asNode is one node as every rule reads it, from any surface's row: Core's
// /api/nodes (id, node_type_code, is_synthetic, parent_id, zone, enabled,
// display_name), the Edge's core-node rows (node_type, group, members), or a
// bare name.
function asNode(raw) {
    const r = typeof raw === 'string' ? { name: raw } : (raw || {});
    const name = String(r.name || '');
    const type = String(r.node_type_code || r.node_type || '').toUpperCase();
    return {
        name,
        id: r.id === undefined || r.id === null ? null : r.id,
        type,
        synthetic: !!r.is_synthetic || type === 'NGRP' || type === 'LANE',
        parentId: r.parent_id === undefined || r.parent_id === null ? null : r.parent_id,
        parentName: String(r.parent_name || r.group || ''),
        memberNames: Array.isArray(r.members) ? r.members.map(String) : [],
        zone: String(r.zone || ''),
        enabled: r.enabled === undefined ? true : !!r.enabled,
        display: String(r.display_name || ''),
        desc: String(r.desc || ''),
        local: r.source === 'local',
        robot: name.indexOf(ROBOT) === 0,
        raw: r,
        // Filled by plantOf from the whole list: the container this node is
        // in (its name, '' for none) and the nodes directly under it.
        up: '',
        kids: [],
    };
}

// nodeWhy is the pieces a field's rule is built from: each answers why a node
// may not be picked, or ''. any(...) is the first piece that answers.
export const nodeWhy = {
    robot: (n) => (n.robot ? 'a robot, not a place' : ''),
    disabled: (n) => (n.enabled ? '' : 'disabled'),
    lane: (n) => (n.type === 'LANE' ? 'a lane' : ''),
    group: (n) => (isContainer(n) ? 'a group: pick one of its nodes' : ''),
    member: (n) => (n.up ? 'in ' + n.up + ': Core picks inside the group' : ''),
    synthetic: (n) => (n.synthetic && !isContainer(n) ? 'synthetic, not a place' : ''),
    any(...pieces) {
        return (n) => {
            for (const p of pieces) {
                const why = p(n);
                if (why) return why;
            }
            return '';
        };
    },
};

function isContainer(n) {
    return n.kids.length > 0 || n.type === 'NGRP' || n.type === 'LANE';
}

function byName(a, b) {
    return a.name < b.name ? -1 : a.name > b.name ? 1 : 0;
}

// plantOf links each node to its container, once per list (the source swaps
// the array when the list changes, so the array is the key).
const plants = new WeakMap();
function plantOf(nodes) {
    let p = plants.get(nodes);
    if (p) return p;
    const all = nodes.filter((n) => n && n.name && n.name !== TRANSIT);
    const named = new Map(all.map((n) => [n.name, n]));
    const byId = new Map();
    for (const n of all) if (n.id !== null) byId.set(String(n.id), n);
    for (const n of all) { n.up = ''; n.kids = []; }
    for (const n of all) {
        let up = null;
        if (n.parentId !== null) up = byId.get(String(n.parentId)) || null;
        if (!up && n.parentName) up = named.get(n.parentName) || null;
        if (up && up !== n) { n.up = up.name; up.kids.push(n); }
    }
    for (const g of all) {
        for (const m of g.memberNames) {
            const n = named.get(m);
            if (n && !n.up && n !== g) { n.up = g.name; g.kids.push(n); }
        }
    }
    for (const n of all) n.kids.sort(byName);
    p = { all, tops: all.filter((n) => !n.up || !named.has(n.up)) };
    // A loop in the list (a parent under its own child) has no top: draw its
    // nodes flat rather than not at all.
    const reached = new Set();
    const walk = (n) => { if (reached.has(n)) return; reached.add(n); n.kids.forEach(walk); };
    p.tops.forEach(walk);
    for (const n of all) if (!reached.has(n)) { n.up = ''; n.kids = []; p.tops.push(n); reached.add(n); }
    plants.set(nodes, p);
    return p;
}

// under is every node beneath n, depth first, each with its depth.
function under(n, depth, out, seen) {
    for (const k of n.kids) {
        if (seen.has(k)) continue;
        seen.add(k);
        out.push({ n: k, depth });
        under(k, depth + 1, out, seen);
    }
    return out;
}

function zoneOf(n) {
    if (n.zone) return n.zone;
    for (const { n: k } of under(n, 1, [], new Set())) if (k.zone) return k.zone;
    return '';
}

function kindOf(n) {
    if (n.type === 'NGRP') return 'group';
    if (n.type === 'LANE') return 'lane';
    if (n.robot) return 'robot';
    return n.synthetic ? 'synthetic' : '';
}

// nodeRows is the shell's rows for one field over the page's node list.
// field:
//   rule(node)   why this field may not take the node, or '' (nodeWhy's pieces)
//   note(node)   a quiet line on a node it may take
//   value        'name' (default) or 'id'
//   expand       picking a container picks the nodes under it (positions)
//   sections     zone headings; default: on when any node has a zone
export function nodeRows(nodes, field) {
    const f = field || {};
    if (!Array.isArray(nodes)) return [];
    const plant = plantOf(nodes);
    const zoned = f.sections !== undefined ? !!f.sections : plant.all.some((n) => n.zone);
    const valueOf = f.value === 'id' ? (n) => String(n.id) : (n) => n.name;
    const rowOf = (n, depth, section) => {
        const why = f.rule ? f.rule(n) : '';
        const notes = [kindOf(n), n.display && n.display !== n.name ? n.display : '', n.desc,
            n.local ? 'this Edge only' : '', !why && f.note ? f.note(n) : ''];
        const row = {
            value: valueOf(n),
            label: (depth >= 2 ? '↳ ' : '') + n.name,
            note: notes.filter(Boolean).join(' · '),
            search: [n.display, n.desc].filter(Boolean).join(' '),
        };
        if (section !== undefined) row.section = section;
        if (why) row.reason = why;
        if (f.expand && n.kids.length) {
            row.values = under(n, 1, [], new Set()).filter((x) => !x.n.kids.length).map((x) => valueOf(x.n));
        }
        return row;
    };
    const tops = plant.tops.slice().sort((a, b) =>
        (zoned ? cmpZone(zoneOf(a), zoneOf(b)) : 0) ||
        (isContainer(b) ? 1 : 0) - (isContainer(a) ? 1 : 0) ||
        byName(a, b));
    return tops.map((n) => {
        const section = zoned ? (zoneOf(n) || 'Other') : undefined;
        const row = rowOf(n, 0, section);
        const members = under(n, 1, [], new Set()).map((x) => rowOf(x.n, x.depth));
        if (members.length) row.members = members;
        return row;
    });
}

// Zones in code-unit order; a node with none ("Other") last.
function cmpZone(a, b) {
    if (a === b) return 0;
    if (!a) return 1;
    if (!b) return -1;
    return a < b ? -1 : 1;
}

// nodePicker draws one node field into host from the page's source.
// field is nodeRows' (rule, note, value, expand, sections); opts are the
// shell's (multi, open, selected, placeholder, chips, onChange, onPick, add,
// filters), plus:
//   name   a hidden input of that name carries the value, for a form the
//          browser posts
// The list is read on the first open (or first focus) if the page has not
// read it, and again on the next one after a failure, which the list says.
export function nodePicker(host, source, field, opts) {
    return mount(host, source, (rows) => nodeRows(rows, field), Object.assign({
        empty: (q) => (q ? 'No node is called that' : 'No nodes'),
        reading: 'Reading the node list…',
        failed: 'Could not read the node list',
    }, opts));
}

// mount is a picker over a source, for both adapters.
export function mountPicker(host, source, rowsFor, opts) {
    return mount(host, source, rowsFor, opts);
}

function mount(host, source, rowsFor, opts) {
    const o = Object.assign({}, opts);
    const reading = o.reading || 'Reading…';
    const failed = o.failed || 'Could not read the list';
    const userChange = o.onChange;
    delete o.reading;
    delete o.failed;
    const name = o.name;
    delete o.name;
    let hidden = null;
    const sync = () => { if (hidden) hidden.value = picker.value(); };
    o.rows = () => {
        const list = source ? source.rows() : [];
        return list === null ? null : rowsFor(list);
    };
    Object.defineProperty(o, 'loading', {
        enumerable: true,
        get: () => (source && source.error() ? failed + ': ' + source.error() + '. Open the list again to retry.' : reading),
    });
    o.onChange = (value, p) => { sync(); if (userChange) userChange(value, p); };
    const picker = createPicker(host, o);
    if (name) {
        hidden = el('input', { type: 'hidden', name });
        host.appendChild(hidden);
        sync();
    }
    const off = source ? source.watch(() => picker.refresh()) : () => {};
    const ask = () => { if (source && source.rows() === null) source.load(); };
    picker.input.addEventListener('focus', ask);
    picker.input.addEventListener('click', ask);
    const set = picker.set;
    picker.set = (values, setOpts) => { set(values, setOpts); sync(); };
    const destroy = picker.destroy;
    picker.destroy = () => { off(); destroy(); };
    return picker;
}
