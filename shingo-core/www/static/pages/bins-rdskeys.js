// bins-rdskeys.js — the RDS keys adapter over the picker's shell (style guide
// §34), for the Lift and Wait fields of each family tab's key strip in the Bin
// Types section (bins-keystrip.js mounts it when Change is pressed). One pick
// each.
//
// The server hands each field whole (bins_family_keys.go, keyField, in the
// host's data-field): its form name, its first view's label, the operations
// that view keeps, and its rows: the default first (value '', the key it
// sends), a saved key Core's scene copy no longer lists ("not in RDS"), then
// every key the copy lists, each with RDS's operation as its note.
//
// TWO VIEWS, THE FIELD'S OWN FIRST: Lift keys · All, Wait keys · All. A view
// keeps a key by the operation RDS gives it, never by its name: the operations
// the copy gives the default key the field sends (seerrds.BinTaskForAction,
// read by the server; no list of its own here). The default row and the saved
// row are in every view; a key RDS gives no operation is under All only. The
// save takes any key the copy lists, so All is the whole choice.

import { mountPicker } from '/static/shared/node-picker.js';

// rdsKeyRows is the shell's rows for one field: each option as a row, its note
// the operation (or "not in RDS"), searchable by its operations too.
export function rdsKeyRows(field) {
    const options = (field && Array.isArray(field.options)) ? field.options : [];
    return options.map((o) => {
        const ops = Array.isArray(o.ops) ? o.ops.map(String) : [];
        const row = { value: String(o.value || ''), label: String(o.label || o.value || ''), ops, pinned: !!o.pinned };
        if (o.note) row.note = String(o.note);
        if (ops.length) row.search = ops.join(' ');
        return row;
    });
}

// rdsKeyViews is the field's two views: its own (kept by operation) and All.
export function rdsKeyViews(field) {
    const keepOps = new Set(((field && field.viewOps) || []).map(String));
    return [
        { key: 'kind', label: (field && field.view) || 'Keys',
            keep: (row) => !!row.pinned || (row.ops || []).some((op) => keepOps.has(op)) },
        { key: 'all', label: 'All', keep: () => true },
    ];
}

// rdsKeySelected is the field's saved value: '' for the default.
export function rdsKeySelected(field) {
    const options = (field && Array.isArray(field.options)) ? field.options : [];
    const saved = options.find((o) => o.selected);
    return saved ? [String(saved.value || '')] : [''];
}

// rdsKeyPicker draws one field's picker into host, opening on its first view.
// The key strip (bins-keystrip.js) mounts it when Change is pressed and owns the
// field's value: opts are the shell's own (selected, the value to open on;
// chips: false, as the strip shows the value; onChange, the pick). label names
// the box for a screen reader.
export function rdsKeyPicker(host, field, label, opts) {
    const rows = rdsKeyRows(field);
    const picker = mountPicker(host, null, () => rows, Object.assign({
        selected: rdsKeySelected(field),
        views: rdsKeyViews(field),
        placeholder: 'Search the RDS keys',
        empty: (q) => (q ? 'No key is called that' : 'No keys'),
    }, opts));
    if (label) picker.input.setAttribute('aria-label', label);
    return picker;
}
