// bins-keystrip.js — a family tab's key strip on the Bins page (owner,
// 2026-11-18). One line at rest: Lift and its key, Wait and its key, each with
// a small Change, the scene's sync time at the end. Change opens that field's
// shared picker in place, through the page's RDS keys adapter
// (bins-rdskeys.js); a pick closes it and the field shows the new key marked
// "not saved"; Escape or a press away closes it with nothing changed. One field
// open at a time. Save keys and Cancel show only while a key is not saved: Save
// posts the form as it always did (its hidden inputs, one Save for the two),
// Cancel puts both fields back. The picker's shell is unchanged: the strip
// mounts it, opens it, and reads its own aria-expanded to know it closed.
//
// The state is one object per strip, and what the strip shows is computed from
// it (style guide 13): keyStripState, openKey, pickKey, closeKey, cancelKeys
// and keyStripView are pure, held by bins.keystrip.test.js.

import { rdsKeyPicker } from '/static/pages/bins-rdskeys.js';

// keyStripState is a strip at rest: each field's saved value, as the server
// drew it, and no field open. fields is { lift: field, wait: field }, each the
// server's keyField (bins_family_keys.go).
export function keyStripState(fields) {
    const keys = {};
    for (const k of Object.keys(fields || {})) {
        const saved = savedValue(fields[k]);
        keys[k] = { field: fields[k], saved, value: saved };
    }
    return { open: '', keys };
}

function savedValue(field) {
    const options = (field && Array.isArray(field.options)) ? field.options : [];
    const o = options.find((x) => x.selected);
    return o ? String(o.value || '') : '';
}

function withKeys(st, fn) {
    const keys = {};
    for (const k of Object.keys(st.keys)) keys[k] = fn(k, st.keys[k]);
    return keys;
}

// openKey opens one field's picker; any other closes, as it was.
export function openKey(st, key) {
    return { open: st.keys[key] ? key : '', keys: st.keys };
}

// pickKey is a pick in the open field: the field takes the key, not saved yet,
// and the picker closes.
export function pickKey(st, key, value) {
    return {
        open: '',
        keys: withKeys(st, (k, f) => (k === key ? Object.assign({}, f, { value: String(value || '') }) : f)),
    };
}

// closeKey closes the open picker with nothing changed: Escape, a press away.
export function closeKey(st) {
    return { open: '', keys: st.keys };
}

// cancelKeys puts both fields back to what is saved.
export function cancelKeys(st) {
    return { open: '', keys: withKeys(st, (_k, f) => Object.assign({}, f, { value: f.saved })) };
}

// keyStripView is what the strip shows: per field its key's label and the
// note beside it ("not saved" on a picked key, "not in RDS" on a saved key the
// scene copy no longer lists, else nothing), and whether its picker is open;
// and dirty, which shows Save keys and Cancel.
export function keyStripView(st) {
    let dirty = false;
    const keys = {};
    for (const k of Object.keys(st.keys)) {
        const f = st.keys[k];
        const options = (f.field && Array.isArray(f.field.options)) ? f.field.options : [];
        const o = options.find((x) => String(x.value || '') === f.value) || { label: f.value };
        const notSaved = f.value !== f.saved;
        if (notSaved) dirty = true;
        keys[k] = {
            label: String(o.label || o.value || ''),
            note: notSaved ? 'not saved' : (o.missing ? 'not in RDS' : ''),
            notSaved,
            open: st.open === k,
            value: f.value,
        };
    }
    return { keys, dirty };
}

// mountKeyStrip wires one strip's form: the state, the picker a Change mounts,
// and render. It returns the strip's two verbs for the page's delegated
// actions.
export function mountKeyStrip(form) {
    const parts = {};
    const fields = {};
    form.querySelectorAll('.bt-key').forEach((span) => {
        const key = span.dataset.key;
        const host = span.querySelector('.bt-key-pick');
        let field = null;
        try { field = JSON.parse(host.dataset.field || 'null'); } catch (e) { field = null; }
        if (!key || !field) return;
        fields[key] = field;
        parts[key] = {
            span, host, field,
            code: span.querySelector('.bt-key-value code'),
            note: span.querySelector('.bt-key-note'),
            value: span.querySelector('.bt-key-value'),
            change: span.querySelector('.bt-key-change'),
            input: span.querySelector('input[type=hidden]'),
            label: host.dataset.label || '',
        };
    });
    const save = form.querySelector('.bt-key-save');
    const cancel = form.querySelector('.bt-key-cancel');
    let st = keyStripState(fields);
    let open = null;   // { key, picker, watch, picked } while a picker is up

    function unmount() {
        if (!open) return;
        open.watch.disconnect();
        open.picker.destroy();
        open.part.host.textContent = '';
        open = null;
    }

    function mount(key) {
        const part = parts[key];
        let picked;
        const picker = rdsKeyPicker(part.host, part.field, part.label, {
            selected: [st.keys[key].value],
            chips: false,
            onChange: (value) => { picked = value; },
        });
        // The shell closes its own list on a pick, on Escape and on a press
        // away, and says so on its box (aria-expanded). Read after the shell
        // has finished its own work.
        const watch = new MutationObserver(() => {
            if (picker.input.getAttribute('aria-expanded') !== 'false') return;
            setTimeout(() => {
                if (!open || open.picker !== picker) return;
                st = picked === undefined ? closeKey(st) : pickKey(st, key, picked);
                render();
            }, 0);
        });
        watch.observe(picker.input, { attributes: true, attributeFilter: ['aria-expanded'] });
        open = { key, picker, watch, part };
        picker.open();
        picker.focus();
    }

    function render() {
        const view = keyStripView(st);
        if (open && open.key !== st.open) unmount();
        for (const key of Object.keys(parts)) {
            const p = parts[key];
            const v = view.keys[key];
            p.code.textContent = v.label;
            p.note.textContent = v.note;
            p.note.classList.toggle('is-not-saved', v.notSaved);
            p.input.value = v.value;
            p.value.hidden = v.open;
            p.change.hidden = v.open;
            p.host.hidden = !v.open;
        }
        save.hidden = !view.dirty;
        cancel.hidden = !view.dirty;
        if (st.open && !open) mount(st.open);
    }

    return {
        change(key) { st = openKey(closeKey(st), key); render(); },
        cancel() { st = cancelKeys(st); render(); },
    };
}
