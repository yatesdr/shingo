// part-picker.js — the part adapter over the picker's shell (style guide §34).
// A page reads its part list ONCE (partSource: Core's payloads, or the Edge's
// mirror of Core's catalog) and every part field on it draws from that one
// list through partPicker, each with its own rule.
//
// A field that may take only some parts names them (field.allowed: a claim's
// codes, a bin type's admitted parts), in the order it wants them; a code the
// list does not carry is still offered, by its code alone, because the field
// says it is allowed and the list may be the one that is behind. A part the
// field's rule refuses is drawn greyed with the reason.

import { mountPicker, pickerSource } from '/static/shared/node-picker.js';

// partSource is the page's part list: rows of {code, name?, description?}
// (Core's payloads, the Edge's catalog) or bare codes.
export function partSource(read, initial) {
    return pickerSource(read, asPart, initial);
}

function asPart(raw) {
    const r = typeof raw === 'string' ? { code: raw } : (raw || {});
    const code = String(r.code || r.name || '');
    const name = r.name && String(r.name) !== code ? String(r.name) : '';
    return { code, name, description: String(r.description || ''), id: r.id === undefined ? null : r.id, raw: r };
}

function byCode(a, b) {
    return a.code < b.code ? -1 : a.code > b.code ? 1 : 0;
}

// partChoices is the shell's rows for one field over the page's part list.
// field:
//   allowed     the codes this field may take, in its order (null: all)
//   rule(part)  why this field may not take the part, or ''
//   note(part)  a quiet line on a part it may take
//   none        a first row that picks no part ('' value), with this label
export function partChoices(parts, field) {
    const f = field || {};
    const known = (Array.isArray(parts) ? parts : []).filter((p) => p && p.code);
    let list;
    if (Array.isArray(f.allowed)) {
        const byIt = new Map(known.map((p) => [p.code, p]));
        const seen = new Set();
        list = [];
        for (const c of f.allowed.map(String)) {
            if (!c || seen.has(c)) continue;
            seen.add(c);
            list.push(byIt.get(c) || asPart(c));
        }
    } else {
        list = known.slice().sort(byCode);
    }
    const rows = f.none ? [{ value: '', label: f.none }] : [];
    for (const p of list) {
        const why = f.rule ? f.rule(p) : '';
        const note = [p.description || p.name, !why && f.note ? f.note(p) : ''].filter(Boolean).join(' · ');
        const row = { value: p.code, label: p.code, note, search: [p.name, p.description].filter(Boolean).join(' ') };
        if (why) row.reason = why;
        rows.push(row);
    }
    return rows;
}

// partPicker draws one part field into host from the page's source (null: a
// field whose allowed codes are all it offers). opts are the shell's, plus
// name (a hidden input for a form post), as nodePicker's.
export function partPicker(host, source, field, opts) {
    return mountPicker(host, source, (rows) => partChoices(rows, field), Object.assign({
        empty: (q) => (q ? 'No part is called that' : 'No parts'),
        reading: 'Reading the part list…',
        failed: 'Could not read the part list',
    }, opts));
}
