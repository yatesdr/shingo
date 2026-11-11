// settings.js — the settings page's behaviour, shared by Core and Edge:
// settingsPage (the draft, the save bar and the server's answers), the
// duration box both ways (durationFromText, durationToText), and collectList.
//
// ES module, served at /static/shared/settings.js; a settings page imports
// each helper from here. Nothing runs at import. It takes el from dom.js, and
// apiResult (its default save door) and toast from utils.js.

import { el } from '/static/shared/dom.js';
import { apiResult, toast } from '/static/shared/utils.js';

// ─── Settings pages ──────────────────────────────────────────────────────
//
// The house settings shape (docs/ui-style-guide/33-settings-pages.md): one
// page, one Save in the foot bar, a draft that is never applied half-typed.
// The CSS is shared/components.css's .set- rules; this is the behaviour.

const _DURATION_UNITS = {
    ns: 1e-6, us: 1e-3, 'µs': 1e-3,
    ms: 1, msec: 1, msecs: 1, millisecond: 1, milliseconds: 1,
    s: 1000, sec: 1000, secs: 1000, second: 1000, seconds: 1000,
    m: 60000, min: 60000, mins: 60000, minute: 60000, minutes: 60000,
    h: 3600000, hr: 3600000, hrs: 3600000, hour: 3600000, hours: 3600000,
};

// durationMs reads a duration as typed or as Go wrote it ("45 min", "10 s",
// "1 h 30 min", "45m0s", "1h30m", "1.5h", "500ms") into whole milliseconds.
// A bare 0 is zero. Anything else, including a negative, is null.
function durationMs(text) {
    const s = String(text === null || text === undefined ? '' : text).trim().toLowerCase();
    if (s === '') return null;
    if (/^0+(\.0+)?$/.test(s)) return 0;
    const re = /(\d+(?:\.\d+)?|\.\d+)\s*([a-zµ]+)\s*/gy;
    let total = 0;
    let at = 0;
    let m;
    while ((m = re.exec(s)) !== null) {
        const unit = _DURATION_UNITS[m[2]];
        if (unit === undefined) return null;
        total += parseFloat(m[1]) * unit;
        at = re.lastIndex;
    }
    if (at !== s.length) return null;
    return Math.round(total);
}

function _trimNum(n) {
    return String(Math.round(n * 1000) / 1000);
}

// goDuration writes milliseconds the way Go's time.Duration.String does
// ("45m0s", "1h0m0s", "10s", "1m30.5s", "500ms"), so the server's
// time.ParseDuration reads it and a value saved and read back is unchanged.
function goDuration(ms) {
    if (ms === 0) return '0s';
    if (ms < 1000) return _trimNum(ms) + 'ms';
    const h = Math.floor(ms / 3600000);
    const m = Math.floor((ms % 3600000) / 60000);
    const sec = (ms % 60000) / 1000;
    let out = '';
    if (h) out += h + 'h';
    if (h || m) out += m + 'm';
    return out + _trimNum(sec) + 's';
}

// durationFromText is what the page sends for a duration box: the typed text
// ("45 min", or a Go string typed in) as a Go duration string. Blank is ''
// (the page decides what blank means); text that is not a duration is null,
// which the page shows as a field error.
export function durationFromText(text) {
    if (String(text === null || text === undefined ? '' : text).trim() === '') return '';
    const ms = durationMs(text);
    return ms === null ? null : goDuration(ms);
}

// durationToText is what the box shows for a stored Go duration: one box
// with its unit in it ("45 min", "10 s", "1 h", "1 h 30 min"), never "45m0s".
// A value that does not parse is shown as stored, so nothing is hidden.
export function durationToText(goStr) {
    const ms = durationMs(goStr);
    if (ms === null) return goStr === null || goStr === undefined ? '' : String(goStr);
    if (ms === 0) return '0 s';
    const parts = [];
    const h = Math.floor(ms / 3600000);
    const m = Math.floor((ms % 3600000) / 60000);
    const s = Math.floor((ms % 60000) / 1000);
    const rest = ms % 1000;
    if (h) parts.push(h + ' h');
    if (m) parts.push(m + ' min');
    if (s) parts.push(s + ' s');
    if (rest) parts.push(_trimNum(rest) + ' ms');
    return parts.join(' ');
}

// collectList reads a list section from the rows that are on the page, in
// order, NOT by index: removing row 0 must never lose the rows after it (an
// index walk stops at the first missing index). read(row) returns the
// row's value; null, undefined and '' are left out (a blank new row).
export function collectList(container, rowSelector, read) {
    if (!container) return [];
    const rows = container.querySelectorAll(rowSelector);
    const out = [];
    for (let i = 0; i < rows.length; i++) {
        const v = read(rows[i]);
        if (v === null || v === undefined || v === '') continue;
        out.push(v);
    }
    return out;
}

// settingsPage binds a settings page's draft, save bar and answers.
//
//   const page = settingsPage(root, {
//       url: '/api/config',            // the shared save door (PUT unless opts.method)
//       restart: ['Station UID'],      // the server's notice at render, if any
//       sections: {
//           fleet: {
//               read()        -> the section's draft as an object (from the page),
//               write(values) -> put values back on the page (Discard),
//               validate(d)   -> {field: msg} or null, before anything is sent,
//               body(d)       -> the section's wire body in the shared request
//                                (default: d itself),
//           },
//           shifts: {
//               label: 'shifts',  // how the outcome line names it (default: the key)
//               read, write, validate as above,
//               save(d)       -> Promise<{ok, errors?, error?}>: a section with
//                                its own door; it is left out of the shared request
//           },
//       },
//       onSaved(body, sections) -> after the shared request saved, e.g. to redraw live words
//   });
//
// Dirty is decided per section by comparing read() with what the page last
// loaded or saved, after every input, change and click inside root, and on
// page.update() (call it after changing the draft from code).
//
// A save validates every dirty section first; any error and nothing is sent.
// Then it sends ONE shared request, {section: body(d)} for the dirty sections
// without their own save, through apiResult, and reads the answer:
// {ok, errors{field: msg}, error} on a refusal, {ok, applied, restart, failed}
// on a save. Then each dirty section with its own save(d) is saved, in order.
// Each part stands alone: a section is clean again only if its own part saved,
// and a partial outcome is said plainly ("Settings saved; shifts failed: …").
//
// A field error is drawn under the row holding [data-field="<field>"]; one with
// no such field goes in the refusal box above the bar. `restart` lists the
// fields waiting on a restart, shown in the notice above the bar; `failed`
// names what was saved but did not apply.
//
// The bar is root's .set-savebar if the page drew one (with a .prov and two
// buttons [data-set="discard"] / [data-set="save"]); otherwise it is built at
// the end of root. The notice and refusal boxes go directly above it.
export function settingsPage(root, opts) {
    opts = opts || {};
    const sections = opts.sections || {};
    const names = Object.keys(sections);
    const send = opts.send || apiResult;
    const method = opts.method || 'PUT';
    const state = {
        baseline: {},
        saving: false,
        restart: Array.isArray(opts.restart) ? opts.restart.slice() : [],
        failed: [],
        refusal: '',
        errLines: [],
    };
    const snap = (n) => JSON.stringify(sections[n].read());
    names.forEach((n) => { state.baseline[n] = snap(n); });

    let bar = root.querySelector('.set-savebar');
    if (!bar) {
        bar = el('div', { className: 'set-savebar' }, [
            el('span', { className: 'prov' }, 'No unsaved changes'),
            el('button', { type: 'button', className: 'set-btn quiet', dataset: { set: 'discard' } }, 'Discard'),
            el('button', { type: 'button', className: 'set-btn primary', dataset: { set: 'save' } }, 'Save'),
        ]);
        root.appendChild(bar);
    }
    const prov = bar.querySelector('.prov');
    const discardBtn = bar.querySelector('[data-set="discard"]');
    const saveBtn = bar.querySelector('[data-set="save"]');
    const notice = el('div', { className: 'set-notice', role: 'status' });
    const refusal = el('div', { className: 'set-refusal', role: 'alert' });
    notice.hidden = true;
    refusal.hidden = true;
    bar.parentNode.insertBefore(notice, bar);
    bar.parentNode.insertBefore(refusal, bar);

    function dirtySections() {
        return names.filter((n) => snap(n) !== state.baseline[n]);
    }

    function render() {
        const dirty = dirtySections().length > 0;
        if (prov) {
            prov.textContent = state.saving ? 'Saving…' : (dirty ? 'Unsaved changes' : 'No unsaved changes');
            prov.classList.toggle('dirty', dirty);
        }
        if (saveBtn) saveBtn.disabled = !dirty || state.saving;
        if (discardBtn) discardBtn.disabled = !dirty || state.saving;
        notice.hidden = state.restart.length === 0;
        notice.textContent = state.restart.length
            ? 'Applies after a restart: ' + state.restart.join(', ') + '.'
            : '';
        let ref = state.refusal;
        if (state.failed.length) {
            ref = (ref ? ref + ' ' : '') + 'Saved, but not applied: ' + state.failed.join(', ') + '.';
        }
        refusal.hidden = !ref;
        refusal.textContent = ref;
    }

    function clearErrors() {
        state.errLines.forEach((line) => { if (line.parentNode) line.parentNode.removeChild(line); });
        state.errLines = [];
        state.refusal = '';
    }

    function fieldFor(key) {
        const q = String(key).replace(/["\\]/g, '\\$&');
        return root.querySelector('[data-field="' + q + '"]');
    }

    // showErrors draws each field error under its row and returns the
    // messages that had no field to sit under.
    function showErrors(errors) {
        const loose = [];
        Object.keys(errors || {}).forEach((key) => {
            const msg = String(errors[key]);
            const field = fieldFor(key);
            if (!field) { loose.push(msg); return; }
            const row = (field.closest && field.closest('.set-fld')) || field;
            const line = el('div', { className: 'set-err', role: 'alert', dataset: { errFor: key } }, msg);
            row.parentNode.insertBefore(line, row.nextSibling);
            state.errLines.push(line);
        });
        return loose;
    }

    // failureText says why one part did not save: its messages with no field
    // to sit under, else its top-level error, else that the marked fields are why.
    function failureText(answer, loose, placed, status) {
        if (loose.length) return loose.join(' ');
        if (!placed) return (answer && answer.error) || ('the server answered ' + status + '.');
        return 'see the marked fields.';
    }

    async function save() {
        const dirty = dirtySections();
        if (!dirty.length || state.saving) return null;
        clearErrors();
        state.failed = [];
        const errors = {};
        const drafts = {};
        dirty.forEach((n) => {
            const s = sections[n];
            drafts[n] = s.read();
            const e = s.validate ? s.validate(drafts[n]) : null;
            if (e) Object.assign(errors, e);
        });
        if (Object.keys(errors).length) {
            const loose = showErrors(errors);
            state.refusal = 'Not saved.' + (loose.length ? ' ' + loose.join(' ') : ' Fix the marked fields.');
            render();
            return { ok: false, shared: null, own: {}, errors };
        }

        const shared = dirty.filter((n) => typeof sections[n].save !== 'function');
        const own = dirty.filter((n) => typeof sections[n].save === 'function');
        const parts = [];   // {label, ok, why}
        const out = { ok: true, shared: null, own: {} };
        state.saving = true;
        render();
        try {
            if (shared.length) {
                const body = {};
                shared.forEach((n) => {
                    const s = sections[n];
                    body[n] = s.body ? s.body(drafts[n]) : drafts[n];
                });
                let res;
                try {
                    res = await send(method, opts.url, body);
                } catch (e) {
                    res = { ok: false, status: 0, body: { error: String(e && e.message ? e.message : e) } };
                }
                out.shared = res;
                const b = (res && res.body && typeof res.body === 'object') ? res.body : {};
                if (!res || !res.ok || b.ok === false) {
                    const loose = showErrors(b.errors);
                    const placed = Object.keys(b.errors || {}).length - loose.length;
                    parts.push({ label: 'Settings', ok: false, why: failureText(b, loose, placed, res ? res.status : 0) });
                } else {
                    shared.forEach((n) => { state.baseline[n] = JSON.stringify(drafts[n]); });
                    if (Array.isArray(b.restart)) state.restart = b.restart.slice();
                    state.failed = Array.isArray(b.failed) ? b.failed.slice() : [];
                    parts.push({ label: 'Settings', ok: true });
                    if (opts.onSaved) opts.onSaved(b, shared);
                }
            }
            for (const n of own) {
                const s = sections[n];
                const label = s.label || n;
                let r;
                try {
                    r = await s.save(drafts[n]);
                } catch (e) {
                    r = { ok: false, error: String(e && e.message ? e.message : e) };
                }
                r = r || { ok: false, error: 'no answer.' };
                out.own[n] = r;
                if (r.ok) {
                    state.baseline[n] = JSON.stringify(drafts[n]);
                    parts.push({ label, ok: true });
                } else {
                    const loose = showErrors(r.errors);
                    const placed = Object.keys(r.errors || {}).length - loose.length;
                    parts.push({ label, ok: false, why: failureText(r, loose, placed, 0) });
                }
            }
        } finally {
            state.saving = false;
        }

        out.ok = parts.every((p) => p.ok);
        if (out.ok) {
            if (!state.failed.length) toast('Saved', 'success');
        } else if (parts.length === 1) {
            const why = parts[0].why;
            state.refusal = 'Not saved. ' + why.charAt(0).toUpperCase() + why.slice(1);
        } else {
            // A partial outcome, said plainly: "Settings saved; shifts failed: …".
            const line = parts.map((p) => p.label + (p.ok ? ' saved' : ' failed: ' + p.why.replace(/\.$/, ''))).join('; ');
            state.refusal = line.charAt(0).toUpperCase() + line.slice(1) + '.';
        }
        render();
        return out;
    }

    function discard() {
        dirtySections().forEach((n) => {
            if (sections[n].write) sections[n].write(JSON.parse(state.baseline[n]));
        });
        clearErrors();
        render();
    }

    let pending = false;
    function later() {
        if (pending) return;
        pending = true;
        Promise.resolve().then(() => { pending = false; render(); });
    }
    root.addEventListener('input', later);
    root.addEventListener('change', later);
    root.addEventListener('click', later);
    if (saveBtn) saveBtn.addEventListener('click', () => { save(); });
    if (discardBtn) discardBtn.addEventListener('click', discard);
    render();

    return {
        save,
        discard,
        update: render,
        isDirty: () => dirtySections().length > 0,
        dirtySections,
        state,
    };
}
