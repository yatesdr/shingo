// pins.js — what the UI helpers do TODAY, per surface, before S2 of the
// shared-UI brief replaces the three copies with one set. Run by
// shared/helper_pins_test.go (TestHelperPins); prints one JSON object
// {golden file name: text} on stdout, compared with testdata/helper-pins/.
//
// THE THREE COPIES, each pinned on its own even where two read the same, so the
// swap behind a name shows up as that surface's golden moving:
//
//   core-hub  shingo-core/www/static/app.js        showModal hideModal uiConfirm
//             uiConfirmWithInput uiPrompt toast api apiGet/Post/Put/Delete, and
//             the bus subscription app.js makes on import (onSSE 'system-status')
//   edge-hub  shingo-edge/www/static/js/shingoedge.js  createSSE showModal
//             hideModal confirm prompt api handleResponse toast window.ShingoEdge
//   shared    shared/utils.js  api apiResult createSSE showRefreshBanner showModal
//             hideModal confirm prompt toast installBackdropClose; shared/bus.js
//             the onSSE bus
//
// plus htmx-sse.js (the htmx SSE extension Edge's changeover and orders pages
// connect through) and a census of htmx request-error handling on Edge admin.
//
// A hub loads as a page loads it: the hub module and the shared modules in one
// realm (world.js), so a hub's import-time side effects (installBackdropClose,
// app.js's bus subscription) are part of what its helpers do. The shared
// surface is shared/utils.js loaded with no hub, as on the operator station
// (which reaches it through bus.js); a case that depends on a page having
// called installBackdropClose says so.
//
// Every case starts a fresh page unless its label says it continues one. A
// golden records behaviour, bugs included, and a bug is pinned as it is, never
// fixed here.
//
// BUGS PINNED AS IS (golden: case label — source):
//   modal.edge-hub.txt "m1 has data-backdrop-close, show, press and click the
//     overlay": the modal closes but body overflow stays "hidden" (the scroll
//     lock is never released) — shingoedge.js:129 locks, shared/utils.js:412
//     (installBackdropClose) only strips .active.
//   modal.shared.txt "show m1 {closeOnBackdrop:true}, press in the card,
//     release on the overlay": closes on a drag that began in the card, the
//     case installBackdropClose guards — shared/utils.js:272.
//   modal.shared.txt "show m2, show m1 (m1 first in the document), Escape":
//     closes m2, the last open overlay in document order, not the last opened
//     — shared/utils.js:263.
//   dialog.shared.txt "confirm: the page opened and hid a modal with this
//     surface's showModal first, then Escape on body": Escape strips .active
//     from the dialog's overlay, which stays in the DOM, and the promise never
//     resolves — shared/utils.js:263 (the document Escape) over confirm's
//     "modal-overlay active" class.
//   dialog.shared.txt "prompt: over an open modal (this surface's showModal),
//     Escape in the input": resolves null AND closes the modal underneath (the
//     input's Escape, then the document's) — shared/utils.js:324 then :263.
//   sse.edge-hub.txt / sse.shared-createSSE.txt "#n error, then client.close()
//     before the reconnect fires": a stream opens after close() — the pending
//     timer is not cancelled — shingoedge.js:97, shared/utils.js:165.
//   sse.edge-hub.txt handler names: "onfoo" listens for "oo", "onSSEPing" for
//     "s-s-e-ping" — shingoedge.js:82.
//   api.core-hub.txt / api.edge-hub.txt "204 no content", "200 empty body":
//     a 2xx with no body rejects (SyntaxError) — app.js:257, shingoedge.js:349.
//   api.core-hub.txt "400 JSON null": rejects TypeError (reads null.error) —
//     app.js:253.
//   toast.shared.txt 'lifetime, "error", {sticky:false}': an error toast
//     lives 3200 ms, not 5000 — shared/utils.js:375.
//
// FIXED SINCE (the golden shows the fixed behaviour):
//   sse.shared-bus.txt "as #n opens (its watchdog armed): setSSEWatchdog(0)":
//     0 disarms the watchdog already running, so the stream stays up; it
//     used to be dropped once more.

'use strict';

const fs = require('fs');
const path = require('path');
const { REPO, Event, Element, makeRealm, dump, describeThrown } = require('./world.js');

const goldens = {};
function golden(name, lines) { goldens[name] = lines.join('\n').replace(/[ \t]+$/gm, '') + '\n'; }

const SURFACES = [
    {
        name: 'core-hub', root: 'core', entry: 'shared/testdata/helper-pins-harness/core-page.js',
        file: 'a Core page (shingo-core/www/static/app.js loaded; the components under Core\'s names)',
        pick: (m) => ({ showModal: m.showModal, hideModal: m.hideModal, confirm: m.uiConfirm, prompt: m.uiPrompt, toast: m.toast }),
        words: { confirm: 'uiConfirm', prompt: 'uiPrompt' },
        // Its modals and dialogs are layer.js's, its toast notice.js's, its door
        // verbs app.js's (wave C, s2-hubs-core): core-page.js holds them as a
        // Core page imports them. No Core file calls utils.js's
        // installBackdropClose any more (the layer needs none), so a case's
        // "page called installBackdropClose()" is the components' no-op here.
        sharedEntry: 'shared/testdata/components/surface.js',
        sel: { dialog: '.layer-scrim', toastBox: '.notice-stack', toast: '.notice-toast' },
    },
    {
        name: 'edge-hub', root: 'edge', entry: '/static/js/shingoedge.js',
        file: 'shingo-edge/www/static/js/shingoedge.js',
        pick: (m) => ({ showModal: m.showModal, hideModal: m.hideModal, confirm: m.confirm, prompt: m.prompt, toast: m.toast }),
        words: { confirm: 'confirm', prompt: 'prompt' },
    },
    {
        name: 'shared', root: 'core', entry: '/static/shared/utils.js',
        file: 'shared/utils.js',
        pick: (m) => ({ showModal: m.showModal, hideModal: m.hideModal, confirm: m.confirm, prompt: m.prompt, toast: m.toast }),
        words: { confirm: 'confirm', prompt: 'prompt' },
    },
];

// A surface outside this file (shared/testdata/components/cases.js runs these
// case lists on the shared components) may name its own shared module and the
// selectors its dialogs and toasts carry; the three below use the defaults.
function page(surface, opts) {
    const r = makeRealm(Object.assign({ surface: surface.root }, opts || {}));
    r.sel = surface.sel || {};
    const m = r.load(surface.entry);
    const shared = r.load(surface.sharedEntry || '/static/shared/utils.js');
    return { r, m, s: surface.pick(m), shared };
}

// ── small DOM verbs ──────────────────────────────────────────────────────────

function mk(r, tag, attrs, kids) {
    const e = r.doc.createElement(tag);
    Object.keys(attrs || {}).forEach((k) => e.setAttribute(k, attrs[k]));
    (kids || []).forEach((k) => e.appendChild(typeof k === 'string' ? r.doc.createTextNode(k) : k));
    return e;
}
function ev(type, extra) { return new Event(type, Object.assign({ bubbles: true, cancelable: true }, extra || {})); }
function key(target, k) { target.dispatchEvent(ev('keydown', { key: k })); }
// pressClick is a pointer pressed on `down` and released on `up` (the click
// lands on `up`, as the browser retargets a drag's click).
function pressClick(down, up) {
    down.dispatchEvent(ev('mousedown'));
    (up || down).dispatchEvent(ev('click'));
}
function desc(e, r) {
    if (!e) return 'null';
    if (r && e === r.doc.body) return 'body';
    if (e.id) return '#' + e.id;
    return e.tagName.toLowerCase() + (e.className ? '.' + e.className.trim().split(/\s+/).join('.') : '');
}
function attempt(fn) {
    try { const v = fn(); return v && typeof v.then === 'function' ? 'returns a promise' : 'ok'; } catch (e) { return 'throws ' + describeThrown(e); }
}
function watch(p) {
    const w = { state: 'pending', value: undefined };
    p.then((v) => { w.state = 'resolved'; w.value = v; }, (e) => { w.state = 'rejected'; w.value = e; });
    return w;
}
function said(w) {
    if (w.state === 'pending') return 'pending';
    if (w.state === 'resolved') return 'resolves ' + JSON.stringify(w.value === undefined ? '(undefined)' : w.value);
    return 'rejects ' + describeThrown(w.value);
}

// ═══ modal ═══════════════════════════════════════════════════════════════════

function modalFixture(r, m1attrs) {
    const opener = mk(r, 'button', { id: 'opener' }, ['Open']);
    const field = (tag, attrs, kids) => mk(r, tag, attrs, kids);
    const m1inner = mk(r, 'div', { class: 'modal', id: 'm1-card' }, [
        field('input', { id: 'm1-text', type: 'text', value: 'dflt' }),
        field('input', { id: 'm1-check', type: 'checkbox' }),
        field('select', { id: 'm1-sel' }, [mk(r, 'option', { value: 'a' }, ['A']), mk(r, 'option', { value: 'b' }, ['B']), mk(r, 'option', { value: 'c' }, ['C'])]),
        field('textarea', { id: 'm1-area' }, ['area-dflt']),
        field('input', { id: 'm1-hidden', type: 'hidden', value: 'keep' }),
    ]);
    const m1 = mk(r, 'div', Object.assign({ id: 'm1', class: 'modal-overlay' }, m1attrs || {}), [m1inner]);
    const m2 = mk(r, 'div', { id: 'm2', class: 'modal-overlay' }, [mk(r, 'div', { class: 'modal' }, [mk(r, 'input', { id: 'm2-text' })])]);
    [opener, m1, m2].forEach((e) => r.doc.body.appendChild(e));
    opener.focus();
    let backdropCloses = 0;
    m1.addEventListener('backdropclose', () => { backdropCloses++; });
    return { opener, m1, m2, m1inner, backdropCloses: () => backdropCloses };
}
function modalState(r, f) {
    const a = (e) => (e.classList.contains('active') ? 'open' : 'closed');
    return 'm1 ' + a(f.m1) + ', m2 ' + a(f.m2) + ', body overflow ' + JSON.stringify(r.doc.body.style.overflow)
        + ', focus ' + desc(r.doc.activeElement, r);
}
function fieldState(r) {
    const g = (id) => r.doc.getElementById(id);
    return 'text ' + JSON.stringify(g('m1-text').value) + ', checkbox ' + g('m1-check').checked
        + ', select index ' + g('m1-sel').selectedIndex + ', textarea ' + JSON.stringify(g('m1-area').value)
        + ', hidden ' + JSON.stringify(g('m1-hidden').value);
}

function modalPins(surface) {
    const out = [
        '# modal — ' + surface.name + ': showModal / hideModal of ' + surface.file,
        '# a fresh page per case: #m1 and #m2 are .modal-overlay (m1 holds a text input with',
        '# value attribute "dflt", a checkbox, a 3-option select, a textarea "area-dflt" and a',
        '# hidden input "keep"); #opener has focus before every case.',
        '',
    ];
    const run = (label, setup, fn) => {
        const p = page(surface);
        const f = modalFixture(p.r, setup);
        const line = fn(p, f);
        out.push(label + ': ' + line);
    };
    const S = (p) => p.s;

    run('show m1', null, (p, f) => { S(p).showModal('m1'); return modalState(p.r, f); });
    run('show m1, hide m1', null, (p, f) => { S(p).showModal('m1'); S(p).hideModal('m1'); return modalState(p.r, f); });
    run('hide on a modal never shown', null, (p, f) => { S(p).hideModal('m1'); return modalState(p.r, f); });
    run('show missing id', null, (p) => attempt(() => S(p).showModal('nope')));
    run('hide missing id', null, (p) => attempt(() => S(p).hideModal('nope')));
    run('show unknown opts are ignored ({bogus:1})', null, (p, f) => { S(p).showModal('m1', { bogus: 1 }); return modalState(p.r, f); });
    out.push('');
    out.push('## fields on hide');
    run('typed, then hide m1', null, (p) => { S(p).showModal('m1'); fixtureType(p); S(p).hideModal('m1'); return fieldState(p.r); });
    run('typed, then hide m1 {preserveState:true}', null, (p) => { S(p).showModal('m1'); fixtureType(p); S(p).hideModal('m1', { preserveState: true }); return fieldState(p.r); });
    out.push('');
    out.push('## Escape');
    run('show m1, Escape on body', null, (p, f) => { S(p).showModal('m1'); key(p.r.doc.body, 'Escape'); return modalState(p.r, f); });
    run('show m1, Escape in its text input', null, (p, f) => { S(p).showModal('m1'); key(p.r.doc.getElementById('m1-text'), 'Escape'); return modalState(p.r, f); });
    run('show m1, typed, Escape: fields', null, (p) => { S(p).showModal('m1'); fixtureType(p); key(p.r.doc.body, 'Escape'); return fieldState(p.r); });
    run('show m1, show m2, Escape', null, (p, f) => { S(p).showModal('m1'); S(p).showModal('m2'); key(p.r.doc.body, 'Escape'); return modalState(p.r, f); });
    run('show m1, show m2, Escape, Escape', null, (p, f) => { S(p).showModal('m1'); S(p).showModal('m2'); key(p.r.doc.body, 'Escape'); key(p.r.doc.body, 'Escape'); return modalState(p.r, f); });
    run('show m2, show m1 (m1 first in the document), Escape', null, (p, f) => { S(p).showModal('m2'); S(p).showModal('m1'); key(p.r.doc.body, 'Escape'); return modalState(p.r, f); });
    run('another key (Enter) on body', null, (p, f) => { S(p).showModal('m1'); key(p.r.doc.body, 'Enter'); return modalState(p.r, f); });
    out.push('');
    out.push('## backdrop');
    const bd = (p, f) => modalState(p.r, f) + ', backdropclose events ' + f.backdropCloses();
    run('show m1, press and click the overlay', null, (p, f) => { S(p).showModal('m1'); pressClick(f.m1); return bd(p, f); });
    run('m1 has data-backdrop-close, show, press and click the overlay', { 'data-backdrop-close': '' }, (p, f) => { S(p).showModal('m1'); pressClick(f.m1); return bd(p, f); });
    run('m1 has data-backdrop-close, page called installBackdropClose(), press and click the overlay', { 'data-backdrop-close': '' },
        (p, f) => { p.shared.installBackdropClose(); S(p).showModal('m1'); pressClick(f.m1); return bd(p, f); });
    run('m1 has data-backdrop-close, page called installBackdropClose(), press in the card, release on the overlay', { 'data-backdrop-close': '' },
        (p, f) => { p.shared.installBackdropClose(); S(p).showModal('m1'); pressClick(f.m1inner, f.m1); return bd(p, f); });
    run('m1 has data-backdrop-close, page called installBackdropClose(), click in the card', { 'data-backdrop-close': '' },
        (p, f) => { p.shared.installBackdropClose(); S(p).showModal('m1'); pressClick(f.m1inner); return bd(p, f); });
    run('m1 has data-backdrop-close, page called installBackdropClose(), typed, backdrop close: fields', { 'data-backdrop-close': '' },
        (p, f) => { p.shared.installBackdropClose(); S(p).showModal('m1'); fixtureType(p); pressClick(f.m1); return fieldState(p.r); });
    run('show m1 {closeOnBackdrop:true}, press and click the overlay', null, (p, f) => { S(p).showModal('m1', { closeOnBackdrop: true }); pressClick(f.m1); return bd(p, f); });
    run('show m1 {closeOnBackdrop:true}, click in the card', null, (p, f) => { S(p).showModal('m1', { closeOnBackdrop: true }); pressClick(f.m1inner); return bd(p, f); });
    run('show m1 {closeOnBackdrop:true}, press in the card, release on the overlay', null,
        (p, f) => { S(p).showModal('m1', { closeOnBackdrop: true }); pressClick(f.m1inner, f.m1); return bd(p, f); });
    out.push('');
    out.push('## scroll lock (body overflow)');
    run('show m1, show m2, hide m1', null, (p, f) => { S(p).showModal('m1'); S(p).showModal('m2'); S(p).hideModal('m1'); return modalState(p.r, f); });
    run('show m1, show m2, hide m1, hide m2', null, (p, f) => { S(p).showModal('m1'); S(p).showModal('m2'); S(p).hideModal('m1'); S(p).hideModal('m2'); return modalState(p.r, f); });
    run('show m1 twice, hide once', null, (p, f) => { S(p).showModal('m1'); S(p).showModal('m1'); S(p).hideModal('m1'); return modalState(p.r, f); });
    run('page body overflow "auto" before: show m1, hide m1', null, (p, f) => { p.r.doc.body.style.overflow = 'auto'; S(p).showModal('m1'); S(p).hideModal('m1'); return modalState(p.r, f); });
    return out;
}
function fixtureType(p) {
    const g = (id) => p.r.doc.getElementById(id);
    g('m1-text').value = 'typed';
    g('m1-check').checked = true;
    g('m1-sel').selectedIndex = 2;
    g('m1-area').value = 'typed-area';
    g('m1-hidden').value = 'changed';
}

// ═══ confirm / prompt ════════════════════════════════════════════════════════

function overlays(r) { return r.doc.querySelectorAll(r.sel.dialog || '.confirm-overlay'); }
function buttonsOf(ov) { return ov.querySelectorAll('button'); }
function cancelOf(ov) { return buttonsOf(ov).find((b) => b.textContent === 'Cancel'); }
function okOf(ov) { return buttonsOf(ov).find((b) => b.textContent !== 'Cancel'); }
function inputOf(ov) { return ov.querySelector('input'); }
function left(r) {
    const ovs = overlays(r);
    if (!ovs.length) return 'no overlay left';
    return ovs.length + ' overlay(s) left: ' + ovs.map((o) => JSON.stringify(o.className)).join(', ');
}

async function dialogPins(surface) {
    const C = surface.words.confirm;
    const P = surface.words.prompt;
    const out = [
        '# confirm / prompt — ' + surface.name + ': ' + C + ' / ' + P + ' of ' + surface.file,
        '# a fresh page per case; #opener has focus when the dialog opens. "after 60 s" is the',
        '# fake clock moved a minute on: a dialog still pending then never resolves by itself.',
        '',
    ];
    const MSG = 'Delete <b>BIN-1</b> & "its" tags?';
    const run = async (label, fn) => {
        const p = page(surface);
        const opener = mk(p.r, 'button', { id: 'opener' }, ['Open']);
        p.r.doc.body.appendChild(opener);
        opener.focus();
        out.push(label + ': ' + await fn(p));
    };
    const settle = (p) => p.r.advance(60000);

    out.push('## ' + C);
    await run(C + '(msg): the DOM it writes', async (p) => {
        p.s.confirm(MSG);
        await p.r.advance(0);
        const ov = overlays(p.r)[0];
        return 'focus ' + desc(p.r.doc.activeElement, p.r) + '\n' + dump(ov, '    ');
    });
    await run(C + ': OK', async (p) => { const w = watch(p.s.confirm(MSG)); okOf(overlays(p.r)[0]).click(); await p.r.advance(0); return said(w) + ', ' + left(p.r) + ', focus ' + desc(p.r.doc.activeElement, p.r); });
    await run(C + ': Cancel', async (p) => { const w = watch(p.s.confirm(MSG)); cancelOf(overlays(p.r)[0]).click(); await p.r.advance(0); return said(w) + ', ' + left(p.r) + ', focus ' + desc(p.r.doc.activeElement, p.r); });
    await run(C + ': Escape on body, after 60 s', async (p) => { const w = watch(p.s.confirm(MSG)); key(p.r.doc.body, 'Escape'); await settle(p); return said(w) + ', ' + left(p.r); });
    await run(C + ': Escape on its OK button, after 60 s', async (p) => { const w = watch(p.s.confirm(MSG)); key(okOf(overlays(p.r)[0]), 'Escape'); await settle(p); return said(w) + ', ' + left(p.r); });
    await run(C + ': the page opened and hid a modal with this surface\'s showModal first, then Escape on body, after 60 s', async (p) => {
        const m = mk(p.r, 'div', { id: 'm1', class: 'modal-overlay' });
        p.r.doc.body.appendChild(m);
        p.s.showModal('m1'); p.s.hideModal('m1');
        const w = watch(p.s.confirm(MSG)); key(p.r.doc.body, 'Escape'); await settle(p); return said(w) + ', ' + left(p.r);
    });
    await run(C + ': press and click the overlay (backdrop), after 60 s', async (p) => { const w = watch(p.s.confirm(MSG)); pressClick(overlays(p.r)[0]); await settle(p); return said(w) + ', ' + left(p.r); });
    await run(C + ': a second one opened while the first is open', async (p) => {
        const a = watch(p.s.confirm('first')); const b = watch(p.s.confirm('second'));
        const n = overlays(p.r).length;
        okOf(overlays(p.r)[1]).click(); await p.r.advance(0);
        const mid = 'first ' + said(a) + ', second ' + said(b);
        cancelOf(overlays(p.r)[0]).click(); await p.r.advance(0);
        return n + ' overlays; OK on the second: ' + mid + '; then Cancel on the first: first ' + said(a) + ', ' + left(p.r);
    });
    await run(C + ': OK clicked twice', async (p) => { const w = watch(p.s.confirm(MSG)); const ok = okOf(overlays(p.r)[0]); ok.click(); ok.click(); await p.r.advance(0); return said(w) + ', ' + left(p.r); });
    await run(C + '(non-string message 42): the message node', async (p) => { p.s.confirm(42); return '\n' + dump(overlays(p.r)[0].firstElementChild.firstChild, '    '); });

    out.push('');
    out.push('## ' + P);
    await run(P + '(msg): the DOM it writes, focus after a tick', async (p) => {
        p.s.prompt(MSG);
        const before = desc(p.r.doc.activeElement, p.r);
        await p.r.advance(0);
        const ov = overlays(p.r)[0];
        return 'focus at open ' + before + ', after a tick ' + desc(p.r.doc.activeElement, p.r) + '\n' + dump(ov, '    ');
    });
    await run(P + '(msg, {type:"number", min:1, max:9, value:5}): the input', async (p) => {
        p.s.prompt(MSG, { type: 'number', min: 1, max: 9, value: 5 });
        const i = inputOf(overlays(p.r)[0]);
        return 'type ' + JSON.stringify(i.type) + ', min ' + JSON.stringify(i.min) + ', max ' + JSON.stringify(i.max) + ', value ' + JSON.stringify(i.value);
    });
    await run(P + '(msg, {type:"date"}): the input type', async (p) => { p.s.prompt(MSG, { type: 'date' }); return JSON.stringify(inputOf(overlays(p.r)[0]).type); });
    await run(P + ': OK untouched, no default', async (p) => { const w = watch(p.s.prompt(MSG)); okOf(overlays(p.r)[0]).click(); await p.r.advance(0); return said(w) + ', ' + left(p.r) + ', focus ' + desc(p.r.doc.activeElement, p.r); });
    await run(P + ': OK untouched, {value:"5"}', async (p) => { const w = watch(p.s.prompt(MSG, { value: '5' })); okOf(overlays(p.r)[0]).click(); await p.r.advance(0); return said(w); });
    await run(P + ': OK untouched, {value:0}', async (p) => { const w = watch(p.s.prompt(MSG, { value: 0 })); okOf(overlays(p.r)[0]).click(); await p.r.advance(0); return said(w); });
    await run(P + ': {value:"5"}, cleared to empty, OK', async (p) => { const w = watch(p.s.prompt(MSG, { value: '5' })); inputOf(overlays(p.r)[0]).value = ''; okOf(overlays(p.r)[0]).click(); await p.r.advance(0); return said(w); });
    await run(P + ': typed "abc", OK', async (p) => { const w = watch(p.s.prompt(MSG)); inputOf(overlays(p.r)[0]).value = 'abc'; okOf(overlays(p.r)[0]).click(); await p.r.advance(0); return said(w); });
    await run(P + ': typed "abc", Enter in the input', async (p) => { const w = watch(p.s.prompt(MSG)); const i = inputOf(overlays(p.r)[0]); i.value = 'abc'; key(i, 'Enter'); await p.r.advance(0); return said(w) + ', ' + left(p.r); });
    await run(P + ': typed "abc", Escape in the input', async (p) => { const w = watch(p.s.prompt(MSG)); const i = inputOf(overlays(p.r)[0]); i.value = 'abc'; key(i, 'Escape'); await p.r.advance(0); return said(w) + ', ' + left(p.r); });
    await run(P + ': typed "abc", Cancel', async (p) => { const w = watch(p.s.prompt(MSG)); inputOf(overlays(p.r)[0]).value = 'abc'; cancelOf(overlays(p.r)[0]).click(); await p.r.advance(0); return said(w) + ', ' + left(p.r); });
    await run(P + ': Escape on body, after 60 s', async (p) => { const w = watch(p.s.prompt(MSG)); key(p.r.doc.body, 'Escape'); await settle(p); return said(w) + ', ' + left(p.r); });
    await run(P + ': press and click the overlay (backdrop), after 60 s', async (p) => { const w = watch(p.s.prompt(MSG)); pressClick(overlays(p.r)[0]); await settle(p); return said(w) + ', ' + left(p.r); });
    await run(P + ': over an open modal (this surface\'s showModal), Escape in the input', async (p) => {
        const m = mk(p.r, 'div', { id: 'm1', class: 'modal-overlay' });
        p.r.doc.body.appendChild(m);
        p.s.showModal('m1');
        const w = watch(p.s.prompt(MSG));
        key(inputOf(overlays(p.r)[0]), 'Escape'); await p.r.advance(0);
        return said(w) + ', the modal underneath ' + (m.classList.contains('active') ? 'open' : 'closed');
    });
    await run(P + ': a second one opened while the first is open', async (p) => {
        const a = watch(p.s.prompt('first')); const b = watch(p.s.prompt('second'));
        await p.r.advance(0);
        const n = overlays(p.r).length;
        const focus = desc(p.r.doc.activeElement, p.r) + ' in overlay ' + (overlays(p.r)[1].contains(p.r.doc.activeElement) ? 2 : overlays(p.r)[0].contains(p.r.doc.activeElement) ? 1 : 'none');
        inputOf(overlays(p.r)[0]).value = 'one';
        key(inputOf(overlays(p.r)[0]), 'Enter'); await p.r.advance(0);
        return n + ' overlays, focus ' + focus + '; Enter in the first: first ' + said(a) + ', second ' + said(b) + ', ' + left(p.r);
    });

    if (surface.name === 'core-hub') {
        out.push('');
        out.push('## uiConfirmWithInput (core-hub only)');
        const W = (p, opts) => p.m.uiConfirmWithInput(MSG, opts);
        await run('uiConfirmWithInput(msg, {placeholder:"Reason"}): the DOM it writes, focus after a tick', async (p) => {
            W(p, { placeholder: 'Reason' }); await p.r.advance(0);
            return 'focus ' + desc(p.r.doc.activeElement, p.r) + '\n' + dump(overlays(p.r)[0], '    ');
        });
        await run('uiConfirmWithInput: typed "dent", Confirm', async (p) => { const w = watch(W(p)); inputOf(overlays(p.r)[0]).value = 'dent'; okOf(overlays(p.r)[0]).click(); await p.r.advance(0); return said(w) + ', ' + left(p.r); });
        await run('uiConfirmWithInput: untouched, Confirm', async (p) => { const w = watch(W(p)); okOf(overlays(p.r)[0]).click(); await p.r.advance(0); return said(w); });
        await run('uiConfirmWithInput: typed "dent", Enter', async (p) => { const w = watch(W(p)); const i = inputOf(overlays(p.r)[0]); i.value = 'dent'; key(i, 'Enter'); await p.r.advance(0); return said(w); });
        await run('uiConfirmWithInput: typed "dent", Escape in the input', async (p) => { const w = watch(W(p)); const i = inputOf(overlays(p.r)[0]); i.value = 'dent'; key(i, 'Escape'); await p.r.advance(0); return said(w) + ', ' + left(p.r); });
        await run('uiConfirmWithInput: typed "dent", Cancel', async (p) => { const w = watch(W(p)); inputOf(overlays(p.r)[0]).value = 'dent'; cancelOf(overlays(p.r)[0]).click(); await p.r.advance(0); return said(w); });
        await run('uiConfirmWithInput: Escape on body, after 60 s', async (p) => { const w = watch(W(p)); key(p.r.doc.body, 'Escape'); await settle(p); return said(w) + ', ' + left(p.r); });
    }
    return out;
}

// ═══ toast ═══════════════════════════════════════════════════════════════════

function toastBox(r) {
    if (r.sel.toastBox) return r.doc.querySelector(r.sel.toastBox);
    return r.doc.getElementById('toast-container') || r.doc.querySelector('.toast-container');
}
function toastsIn(r) { const b = toastBox(r); return b ? b.children : []; }

// lifetime steps the fake clock 100 ms at a time and says when the toast went
// transparent and when it left the DOM.
async function lifetime(r, t, maxMs) {
    let faded = null, gone = null;
    for (let ms = 100; ms <= maxMs && gone === null; ms += 100) {
        await r.advance(100);
        if (faded === null && t.style.opacity === '0') faded = ms;
        if (gone === null && !t.isConnected) gone = ms;
    }
    return (faded !== null ? 'transparent at ' + faded + ' ms, ' : '') + (gone !== null ? 'removed at ' + gone + ' ms' : 'still there after ' + maxMs + ' ms');
}

async function toastPins(surface) {
    const out = [
        '# toast — ' + surface.name + ': toast of ' + surface.file,
        '# a fresh page per case; nothing in the page markup holds a toast container.',
        '',
    ];
    const run = async (label, fn) => { const p = page(surface); out.push(label + ': ' + await fn(p)); };
    const last = (p) => { const k = toastsIn(p.r); return k[k.length - 1]; };

    await run('toast("Saved", "success"): returns, and the DOM it writes', async (p) => {
        const ret = p.s.toast('Saved', 'success');
        return (ret === undefined ? 'returns undefined' : 'returns the toast element (' + (ret === last(p) ? 'same node' : 'another node') + ')') + '\n' + dump(toastBox(p.r), '    ');
    });
    for (const lvl of [undefined, 'info', 'success', 'warning', 'error', 'bogus']) {
        await run('level ' + (lvl === undefined ? '(none)' : JSON.stringify(lvl)) + ': the toast node', async (p) => { p.s.toast('Msg', lvl); return '\n' + dump(last(p), '    '); });
    }
    for (const lvl of [undefined, 'success', 'warning', 'error', 'bogus']) {
        await run('lifetime, level ' + (lvl === undefined ? '(none)' : JSON.stringify(lvl)), async (p) => { p.s.toast('Msg', lvl); return lifetime(p.r, last(p), 8000); });
    }
    await run('lifetime, "error", {sticky:false}', async (p) => { p.s.toast('Msg', 'error', { sticky: false }); return lifetime(p.r, last(p), 8000); });
    await run('{sticky:true}, "error": the node, after 60 s, then a click on it', async (p) => {
        p.s.toast('Msg', 'error', { sticky: true });
        const t = last(p);
        const node = dump(t, '    ');
        await p.r.advance(60000);
        if (!t.isConnected) return 'gone within 60 s (sticky ignored), so no click\n' + node;
        t.click();
        return 'still there after 60 s; a click ' + (t.isConnected ? 'leaves it' : 'removes it') + '\n' + node;
    });
    await run('three toasts: the container\'s children, in order', async (p) => {
        p.s.toast('one'); p.s.toast('two', 'error'); p.s.toast('three', 'success');
        return toastsIn(p.r).map((t) => JSON.stringify(t.textContent) + ' ' + t.className).join(' | ') + ' (' + p.r.doc.body.children.length + ' child(ren) of body)';
    });
    await run('twelve toasts at once: how many are in the DOM', async (p) => {
        for (let i = 0; i < 12; i++) p.s.toast('n' + i);
        return toastsIn(p.r).length + ' toasts, first ' + JSON.stringify(toastsIn(p.r)[0].textContent);
    });
    await run('message with markup <img src=x onerror=alert(1)> & "q": the toast\'s children', async (p) => {
        p.s.toast('<img src=x onerror=alert(1)> & "q"', 'error');
        return '\n' + dump(last(p), '    ');
    });
    await run('toast(null)', async (p) => { p.s.toast(null); return JSON.stringify(last(p).textContent); });
    await run('toast(42)', async (p) => { p.s.toast(42); return JSON.stringify(last(p).textContent); });
    await run('toast(an Error("boom"))', async (p) => { p.s.toast(new p.r.TypeError('boom')); return JSON.stringify(last(p).textContent); });
    await run('toast({error:"x"})', async (p) => { p.s.toast({ error: 'x' }); return JSON.stringify(last(p).textContent); });
    return out;
}

// ═══ api ═════════════════════════════════════════════════════════════════════
//
// The bodies are what the servers write: Edge writeError / writeAdvisoryRefusal
// / writeErrorWithExit (shingo-edge/www/handlers_api.go), Core jsonError
// (shingo-core/www/helpers.go), configRefusal (shingo-core/www/handlers_config.go)
// and kanbanHeld (shingo-core/www/handlers_kanban.go) as json.Encoder writes
// them (map keys sorted, a trailing newline), net/http's http.Error (text plus a
// newline), and a proxy's HTML page.

const ANSWERS = [
    ['200 JSON object', 200, '{"id":7,"name":"PRESS-1"}\n'],
    ['200 JSON {"ok":false,"error":"refused"}', 200, '{"error":"refused","ok":false}\n'],
    ['200 empty body', 200, ''],
    ['204 no content', 204, ''],
    ['200 not JSON ("ok")', 200, 'ok\n'],
    ['400 Edge writeError / Core jsonError {"error"}', 400, '{"error":"process not found"}\n'],
    ['400 Edge writeAdvisoryRefusal {"error","notice"}', 400, '{"error":"changeover already running","notice":true}\n'],
    ['409 Edge writeErrorWithExit {"error","exit"}', 409, '{"error":"lane held","exit":{"kind":"release","url":"/api/x/release","label":"Release"}}\n'],
    ['400 Core configRefusal {"error","errors","ok"}', 400, '{"error":"Port must be a number.","errors":{"port":"Port must be a number."},"ok":false}\n'],
    ['409 Core kanbanHeld {"drain","error","needs_force"}', 409, '{"drain":[],"error":"position holds bins","needs_force":true}\n'],
    ['400 JSON {"error":""}', 400, '{"error":""}\n'],
    ['400 JSON with no error key', 400, '{"needs_force":true}\n'],
    ['400 JSON whose error is an object', 400, '{"error":{"code":7}}\n'],
    ['400 JSON null', 400, 'null\n'],
    ['400 plain text (http.Error)', 400, 'invalid bin type\n'],
    ['400 empty body', 400, ''],
    ['500 JSON {"error"}', 500, '{"error":"database not answering"}\n'],
    ['502 HTML from a proxy', 502, '<html><body>Bad Gateway</body></html>'],
    ['no answer (fetch rejects TypeError "Failed to fetch")', 0, null],
];

function answerFor(status, body) {
    return () => (body === null ? { reject: 'Failed to fetch' } : { status, body, headers: { 'content-type': 'application/json' } });
}
function requestLine(req) {
    if (!req) return 'no request';
    const i = req.init;
    if (i === undefined) return req.url + ' (no init: GET)';
    const parts = [JSON.stringify(i.method || '(none: GET)')];
    parts.push('headers ' + (i.headers ? JSON.stringify(i.headers) : 'none'));
    parts.push('body ' + (i.body === undefined ? 'none' : JSON.stringify(i.body)));
    return req.url + ' ' + parts.join(', ');
}
function consoleNotes(r) { return r.notes.filter((n) => / console\./.test(n)).map((n) => n.replace(/^t=\d+ /, '')); }

async function callDoor(surface, door, status, body) {
    const p = page(surface);
    p.r.answer = answerFor(status, body);
    const realFetch = p.r.ctx.fetch;
    // the stub's own rejection carries the harness's message; mark it so the
    // golden keeps it (engine messages are dropped, see describeThrown).
    p.r.ctx.fetch = (u, i) => realFetch(u, i).catch((e) => { e.__harness = true; throw e; });
    await p.r.advance(0);
    p.r.notes = [];
    p.r.requests = [];
    let w;
    try { w = watch(Promise.resolve(door(p))); } catch (e) { return { line: 'throws synchronously ' + describeThrown(e), req: null }; }
    await p.r.advance(0);
    const toasts = p.r.doc.querySelectorAll(p.r.sel.toast || '.toast').length;
    const cons = consoleNotes(p.r);
    return {
        line: said(w) + (toasts ? ', toasts ' + toasts : ', no toast') + (cons.length ? ', ' + cons.join('; ') : ''),
        req: p.r.requests[0],
    };
}

async function apiPins(surface) {
    const out = [
        '# api — ' + surface.name + ': the HTTP helpers of ' + surface.file,
        '# a fresh page per call; "resolves"/"rejects" is the promise the page awaits.',
        '',
    ];
    const probe = page(surface);
    const doors = surface.doors ? surface.doors(probe) : {
        'core-hub': {
            shape: () => 'verbs apiGet apiPost apiPut apiDelete (app.js, over shared/http.js): ' + ['apiGet', 'apiPost', 'apiPut', 'apiDelete'].map((n) => n + ' ' + typeof probe.m[n]).join(', ') + '; no non-throwing door',
            requests: [
                ['apiGet(url)', (p) => p.m.apiGet('/api/x')],
                ['apiPost(url, {a:1})', (p) => p.m.apiPost('/api/x', { a: 1 })],
                ['apiPost(url)', (p) => p.m.apiPost('/api/x')],
                ['apiPut(url, {a:1})', (p) => p.m.apiPut('/api/x', { a: 1 })],
                ['apiDelete(url)', (p) => p.m.apiDelete('/api/x')],
            ],
            // The hub's rejection is an Error carrying the door's sentence (its
            // String() is that sentence), not the engine's words: keep it.
            throwing: [['apiGet', (p) => p.m.apiGet('/api/x')], ['apiPost', (p) => p.m.apiPost('/api/x', { a: 1 })], ['apiDelete', (p) => p.m.apiDelete('/api/x')]]
                .map(([n, door]) => [n, (p) => door(p).catch((e) => { if (e && typeof e === 'object' && String(e) === e.message) e.__harness = true; throw e; })]),
            safe: [],
        },
        'edge-hub': {
            shape: () => 'api is an ' + typeof probe.m.api + ' {' + Object.keys(probe.m.api).join(', ') + '}; handleResponse is module-private',
            requests: [
                ['api.get(url)', (p) => p.m.api.get('/api/x')],
                ['api.post(url, {a:1})', (p) => p.m.api.post('/api/x', { a: 1 })],
                ['api.post(url)', (p) => p.m.api.post('/api/x')],
                ['api.put(url, {a:1})', (p) => p.m.api.put('/api/x', { a: 1 })],
                ['api.patch(url, {a:1})', (p) => p.m.api.patch('/api/x', { a: 1 })],
                ['api.del(url)', (p) => p.m.api.del('/api/x')],
                ['api.postDetailed(url, {a:1})', (p) => p.m.api.postDetailed('/api/x', { a: 1 })],
            ],
            throwing: [['api.get', (p) => p.m.api.get('/api/x')], ['api.post', (p) => p.m.api.post('/api/x', { a: 1 })], ['api.del', (p) => p.m.api.del('/api/x')]],
            safe: [['api.postDetailed', (p) => p.m.api.postDetailed('/api/x', { a: 1 })]],
        },
        'shared': {
            shape: () => 'api is an ' + typeof probe.m.api + ' {' + Object.keys(probe.m.api).join(', ') + '}; apiResult is a ' + typeof probe.m.apiResult + ' (method, url, body)',
            requests: [
                ['api.get(url)', (p) => p.m.api.get('/api/x')],
                ['api.post(url, {a:1})', (p) => p.m.api.post('/api/x', { a: 1 })],
                ['api.post(url)', (p) => p.m.api.post('/api/x')],
                ['api.put(url, {a:1})', (p) => p.m.api.put('/api/x', { a: 1 })],
                ['api.del(url)', (p) => p.m.api.del('/api/x')],
                ['api.patch', (p) => p.m.api.patch('/api/x', { a: 1 })],
                ['apiResult("GET", url)', (p) => p.m.apiResult('GET', '/api/x')],
                ['apiResult("POST", url, {a:1})', (p) => p.m.apiResult('POST', '/api/x', { a: 1 })],
            ],
            throwing: [['api.get', (p) => p.m.api.get('/api/x')], ['api.post', (p) => p.m.api.post('/api/x', { a: 1 })], ['api.del', (p) => p.m.api.del('/api/x')]],
            safe: [['apiResult GET', (p) => p.m.apiResult('GET', '/api/x')]],
        },
    }[surface.name];

    out.push('shape: ' + doors.shape());
    out.push('');
    out.push('## the request each verb sends (answered 200 {})');
    for (const [label, door] of doors.requests) {
        const res = await callDoor(surface, door, 200, '{}');
        out.push(label + ': ' + (res.req ? requestLine(res.req) : res.line));
    }
    for (const [name, door] of doors.throwing) {
        out.push('');
        out.push('## ' + name + ': every answer');
        for (const [label, status, body] of ANSWERS) {
            const res = await callDoor(surface, door, status, body);
            out.push(label + ': ' + res.line);
        }
    }
    for (const [name, door] of doors.safe) {
        out.push('');
        out.push('## ' + name + ' (the door that does not throw): every answer');
        for (const [label, status, body] of ANSWERS) {
            const res = await callDoor(surface, door, status, body);
            out.push(label + ': ' + res.line);
        }
    }
    if (!doors.safe.length) { out.push(''); out.push('## the door that does not throw: none on this surface'); }
    return out;
}

// ═══ SSE clients ═════════════════════════════════════════════════════════════

// A timeline: each step, then what the page did (EventSource opened/closed,
// handler calls, console errors, reloads, the refresh banner coming or going),
// stamped with the fake clock, then any summary line the step returns.
function timeline(r, out) {
    const banner = () => !!r.doc.getElementById('shingo-refresh-banner');
    return async (label, fn) => {
        r.notes = [];
        const had = banner();
        const extra = await fn();
        await r.advance(0);
        if (banner() !== had) r.note(banner() ? 'refresh banner shown' : 'refresh banner gone');
        out.push(label);
        (r.notes.length ? r.notes : ['(nothing)']).forEach((n) => out.push('    ' + n));
        [].concat(extra === undefined ? [] : extra).forEach((x) => out.push('    = ' + x));
    };
}
// waitForOpen runs the clock timer by timer until a new stream opens (or
// maxMs passes) and says how long after the last close it opened.
async function waitForOpen(r, maxMs) {
    const start = r.clock.now;
    const before = r.streams.length;
    for (;;) {
        if (r.streams.length > before) {
            const s = r.streams[r.streams.length - 1];
            const shut = r.streams.slice(0, -1).filter((x) => x.closedAt !== null).sort((a, b) => b.closedAt - a.closedAt)[0];
            return shut ? '#' + s.n + ' opened ' + (s.openedAt - shut.closedAt) + ' ms after #' + shut.n + ' closed'
                : '#' + s.n + ' opened, no stream closed before it';
        }
        const next = r.clock.timers.map((t) => t.at).filter((at) => at <= start + maxMs).sort((a, b) => a - b)[0];
        if (next === undefined) { await r.advance(start + maxMs - r.clock.now); return 'no new stream within ' + maxMs + ' ms'; }
        await r.advance(Math.max(0, next - r.clock.now));
    }
}

function handlerNote(r, label) {
    return function () {
        const args = Array.prototype.slice.call(arguments);
        r.note('handler ' + label + '(' + args.map((a, i) => (i === 0 ? JSON.stringify(a) : (a && typeof a === 'object' && 'lastEventId' in a ? '<event>' : JSON.stringify(a)))).join(', ') + ')');
    };
}

async function createSSEPins(surfaceName) {
    const isEdge = surfaceName === 'edge-hub';
    const surface = SURFACES.find((s) => s.name === surfaceName);
    const out = [
        '# createSSE — ' + surfaceName + ': createSSE of ' + surface.file,
        '# one page, steps in order; "#n" is the n-th EventSource the page opened, "←" a frame',
        '# the server sends on it. The clock is fake: t= is ms since the page loaded.',
        '',
    ];
    const p = page(surface);
    const r = p.r;
    const step = timeline(r, out);
    const handlers = isEdge
        ? { onOrderUpdate: handlerNote(r, 'onOrderUpdate'), onCoreNodes: handlerNote(r, 'onCoreNodes'), onfoo: handlerNote(r, 'onfoo'), onSSEPing: handlerNote(r, 'onSSEPing'), refresh: handlerNote(r, 'refresh'), onNotAFunction: 7 }
        : { 'order-update': handlerNote(r, 'order-update'), 'core-nodes': handlerNote(r, 'core-nodes'), onOrderUpdate: handlerNote(r, 'onOrderUpdate'), notAFunction: 7 };
    out.push('handlers given: ' + Object.keys(handlers).join(', '));
    let client;
    await step('page loaded (the module imported, no createSSE yet)', async () => {});
    await step('createSSE("/events", handlers)', async () => { client = p.m.createSSE('/events', handlers); });
    out.push('    returns {' + Object.keys(client).join(', ') + '}; #1 listens for: ' + r.streams[0].types().join(', '));
    const s1 = r.streams[0];
    await step('#1 open (readyState OPEN)', async () => s1.open());
    await step('#1 ← connected {"build":"b1"}', async () => s1.emit('connected', { build: 'b1' }));
    await step('#1 ← order-update {"id":1}', async () => s1.emit('order-update', { id: 1 }));
    await step('#1 ← core-nodes {"nodes":[]}', async () => s1.emit('core-nodes', { nodes: [] }));
    await step('#1 ← oo {"x":1}  (the type a handler named "onfoo" maps to)', async () => s1.emit('oo', { x: 1 }));
    await step('#1 ← order-update "not json"', async () => s1.emit('order-update', 'not json'));
    await step('#1 ← heartbeat {"build":"b1"}', async () => s1.emit('heartbeat', { build: 'b1' }));
    await step('#1 ← heartbeat {"build":"b2"}  (a new build, seen on a heartbeat)', async () => s1.emit('heartbeat', { build: 'b2' }));
    if (r.doc.getElementById('shingo-refresh-banner')) {
        out.push('    the banner:');
        out.push(dump(r.doc.getElementById('shingo-refresh-banner'), '      '));
        await step('click the banner\'s Dismiss', async () => { r.doc.getElementById('shingo-refresh-banner').querySelectorAll('button')[1].click(); });
    }
    out.push('');
    out.push('## reconnect');
    await step('#1 error', async () => { s1.fail(0); return waitForOpen(r, 70000); });
    for (let i = 0; i < 5; i++) {
        const s = r.lastStream();
        await step('#' + s.n + ' error (no connected since the last error)', async () => { s.fail(0); return waitForOpen(r, 70000); });
    }
    {
        const s = r.lastStream();
        await step('#' + s.n + ' ← connected {"build":"b1"}, then #' + s.n + ' error', async () => { s.emit('connected', { build: 'b1' }); s.fail(0); return waitForOpen(r, 70000); });
    }
    out.push('    #' + r.lastStream().n + ' listens for: ' + r.lastStream().types().join(', '));
    out.push('');
    out.push('## build change');
    {
        const s = r.lastStream();
        await step('#' + s.n + ' ← connected {"build":"b2"}  (a new build, on a connect)', async () => s.emit('connected', { build: 'b2' }));
        await step('#' + s.n + ' ← connected {"build":"b2"} again', async () => s.emit('connected', { build: 'b2' }));
        await step('#' + s.n + ' ← connected {"build":"b1"}  (the first build again)', async () => s.emit('connected', { build: 'b1' }));
        await step('#' + s.n + ' ← connected {}  (no build)', async () => s.emit('connected', {}));
        await step('#' + s.n + ' ← connected "garbage"', async () => s.emit('connected', 'garbage'));
        if (r.doc.getElementById('shingo-refresh-banner')) {
            await step('click the banner\'s Refresh', async () => { r.doc.getElementById('shingo-refresh-banner').querySelectorAll('button')[0].click(); });
        }
    }
    out.push('');
    out.push('## close and unload');
    {
        const s = r.lastStream();
        await step('#' + s.n + ' error, then client.close() before the reconnect fires', async () => { s.fail(0); client.close(); return waitForOpen(r, 70000); });
        const t = r.lastStream();
        await step('client.close() with #' + t.n + ' open', async () => { client.close(); });
    }
    {
        const q = page(surface);
        const qs = [];
        const qstep = timeline(q.r, qs);
        q.m.createSSE('/events?x=1', {});
        await qstep('a second page: createSSE("/events?x=1", {}), then window beforeunload', async () => { q.r.window.dispatchEvent(new Event('beforeunload')); });
        out.push(...qs);
    }
    {
        const q = page(surface, { readyState: 'loading' });
        const qs = [];
        const qstep = timeline(q.r, qs);
        await qstep('a page still loading (readyState "loading"): createSSE("/events", {})', async () => { q.m.createSSE('/events', {}); });
        await qstep('then DOMContentLoaded', async () => { q.r.doc.readyState = 'interactive'; q.r.doc.dispatchEvent(new Event('DOMContentLoaded')); });
        out.push(...qs);
    }
    out.push('');
    out.push('## disconnected / connected signals');
    out.push('a second page, handlers for "connected" and "disconnected" given as ' + (isEdge ? 'onConnected, onDisconnected' : '"connected", "disconnected"') + ':');
    {
        const q = page(surface);
        const qs = [];
        const qstep = timeline(q.r, qs);
        const hs = isEdge ? { onDisconnected: handlerNote(q.r, 'onDisconnected'), onConnected: handlerNote(q.r, 'onConnected') }
            : { disconnected: handlerNote(q.r, 'disconnected'), connected: handlerNote(q.r, 'connected') };
        q.m.createSSE('/events', hs);
        await qstep('#1 ← connected {"build":"b1"}, then #1 error', async () => { q.r.streams[0].emit('connected', { build: 'b1' }); q.r.streams[0].fail(0); });
        out.push(...qs);
    }
    return out;
}

async function busPins() {
    const out = [
        '# onSSE bus — shared: onSSE setSSEQuery setSSEWatchdog setSSEReloadOnBuild closeSSEBus',
        '# of shared/bus.js; it dispatches build-changed, and BC shows showRefreshBanner of shared/utils.js',
        '# (a subscriber; Core\'s hub shows notice.js\'s banner, sse.core-hub.txt). One page (shared/bus.js, as the',
        '# operator station loads it), steps in order; a "second page" is a fresh one.',
        '',
    ];
    const surface = SURFACES.find((s) => s.name === 'shared');
    // busPage is a page with the bus loaded beside utils.js, one instance of
    // each. The bus shows no banner: a page subscribes build-changed to show it.
    const busPage = () => { const q = page(surface); q.bus = q.r.load('/static/shared/bus.js'); return q; };
    const p = busPage();
    const r = p.r;
    const m = p.m;
    const bus = p.bus;
    const step = timeline(r, out);
    let offA;
    await step('onSSE("order-update", A); onSSE("order-update", B); onSSE("connected", C); onSSE("disconnected", D) — before a microtask', async () => {
        offA = bus.onSSE('order-update', handlerNote(r, 'A'));
        bus.onSSE('order-update', handlerNote(r, 'B'));
        bus.onSSE('connected', handlerNote(r, 'C'));
        bus.onSSE('disconnected', handlerNote(r, 'D'));
        r.note('streams open before the microtask: ' + r.streams.length);
    });
    out.push('    #1 listens for: ' + r.streams[0].types().join(', '));
    const s1 = r.streams[0];
    await step('onSSE("order-update", "not a function")', async () => { const off = bus.onSSE('order-update', 'nope'); r.note('returns a ' + typeof off); });
    await step('#1 ← connected {"build":"b1","seq":5}', async () => s1.emit('connected', { build: 'b1', seq: 5 }));
    await step('#1 ← order-update {"id":1} id 6', async () => s1.emit('order-update', { id: 1 }, '6'));
    await step('#1 ← order-update "not json"', async () => s1.emit('order-update', 'not json'));
    await step('onSSE("order-update", T that throws) then #1 ← order-update {"id":2}, then T unsubscribed', async () => {
        const offT = bus.onSSE('order-update', () => { throw new Error('page bug'); });
        s1.emit('order-update', { id: 2 });
        offT();
    });
    await step('onSSE("bins", E) after the stream is up', async () => { bus.onSSE('bins', handlerNote(r, 'E')); });
    const s2 = r.lastStream();
    await step('A unsubscribed (offA())', async () => { offA(); });
    await step('#' + s2.n + ' ← connected {"build":"b1","seq":10}; ← order-update {"id":3}', async () => { s2.emit('connected', { build: 'b1', seq: 10 }); s2.emit('order-update', { id: 3 }); });
    await step('onSSE("stream-gap", G); onSSE("heartbeat", H)', async () => { bus.onSSE('stream-gap', handlerNote(r, 'G')); bus.onSSE('heartbeat', handlerNote(r, 'H')); });
    await step('#' + s2.n + ' ← heartbeat {"build":"b1","seq":10}', async () => s2.emit('heartbeat', { build: 'b1', seq: 10 }));
    await step('#' + s2.n + ' ← heartbeat {"build":"b1","seq":12}', async () => s2.emit('heartbeat', { build: 'b1', seq: 12 }));
    await step('#' + s2.n + ' ← heartbeat {"build":"b1"}  (no seq: Core)', async () => s2.emit('heartbeat', { build: 'b1' }));
    await step('onSSE("build-changed", BC), BC calling showRefreshBanner()', async () => {
        const note = handlerNote(r, 'BC');
        bus.onSSE('build-changed', function () { note.apply(null, arguments); m.showRefreshBanner(); });
    });
    await step('#' + s2.n + ' ← heartbeat {"build":"b2","seq":12}  (a new build)', async () => s2.emit('heartbeat', { build: 'b2', seq: 12 }));
    if (r.doc.getElementById('shingo-refresh-banner')) {
        out.push('    the banner:');
        out.push(dump(r.doc.getElementById('shingo-refresh-banner'), '      '));
    }
    await step('showRefreshBanner() again while it is shown', async () => { m.showRefreshBanner(); r.note('banners in the DOM: ' + r.doc.querySelectorAll('#shingo-refresh-banner').length); });
    await step('click Dismiss', async () => { r.doc.getElementById('shingo-refresh-banner').querySelectorAll('button')[1].click(); });
    await step('#' + s2.n + ' ← connected {"build":"b2"}  (the new build again, after Dismiss)', async () => s2.emit('connected', { build: 'b2' }));
    await step('click Refresh', async () => { r.doc.getElementById('shingo-refresh-banner').querySelectorAll('button')[0].click(); });
    out.push('');
    out.push('## reconnect');
    await step('#' + s2.n + ' error', async () => { s2.fail(0); return waitForOpen(r, 70000); });
    for (let i = 0; i < 5; i++) {
        const s = r.lastStream();
        await step('#' + s.n + ' error (no connected since the last error)', async () => { s.fail(0); return waitForOpen(r, 70000); });
    }
    {
        const s = r.lastStream();
        await step('#' + s.n + ' ← connected {"build":"b1"}, then #' + s.n + ' error', async () => { s.emit('connected', { build: 'b1' }); s.fail(0); return waitForOpen(r, 70000); });
        out.push('    #' + r.lastStream().n + ' url ' + r.lastStream().url);
    }
    out.push('');
    out.push('## watchdog');
    {
        const s = r.lastStream();
        await step('setSSEWatchdog(5000) on the open #' + s.n + ', then 6 s with no frame', async () => { bus.setSSEWatchdog(5000); await r.advance(6000); });
        await step('#' + s.n + ' ← heartbeat {"build":"b1"} (re-arms), then 4.9 s, a frame, 4.9 s', async () => { s.emit('heartbeat', { build: 'b1' }); await r.advance(4900); s.emit('order-update', { id: 9 }); await r.advance(4900); });
        await step('then 0.2 s more (5.1 s since the last frame)', async () => { await r.advance(200); return waitForOpen(r, 70000); });
        await step('as #' + (s.n + 1) + ' opens (its watchdog armed): setSSEWatchdog(0), #' + (s.n + 1) + ' ← connected {"build":"b1"}, then 60 s with no frame',
            async () => { bus.setSSEWatchdog(0); r.lastStream().emit('connected', { build: 'b1' }); await r.advance(60000); });
    }
    out.push('');
    out.push('## reload on build');
    {
        const s = r.lastStream();
        await step('setSSEReloadOnBuild(true); #' + s.n + ' ← connected {"build":"b3"}', async () => { bus.setSSEReloadOnBuild(true); s.emit('connected', { build: 'b3' }); });
        await step('#' + s.n + ' ← heartbeat {"build":"b3"}', async () => s.emit('heartbeat', { build: 'b3' }));
        await step('#' + s.n + ' ← heartbeat {"build":"b1"}', async () => s.emit('heartbeat', { build: 'b1' }));
    }
    out.push('');
    out.push('## close');
    {
        const s = r.lastStream();
        await step('#' + s.n + ' error, then closeSSEBus() before the reconnect fires', async () => { s.fail(0); bus.closeSSEBus(); return waitForOpen(r, 70000); });
        await step('onSSE("later", F) after closeSSEBus()', async () => { bus.onSSE('later', handlerNote(r, 'F')); });
    }
    out.push('');
    out.push('## the URL');
    for (const [label, fn] of [
        ['only stream frames subscribed: onSSE("connected", C)', (q) => q.bus.onSSE('connected', () => {})],
        ['setSSEQuery({station:7, a:"x y"}) then onSSE("b"), onSSE("a")', (q) => { q.bus.setSSEQuery({ station: 7, a: 'x y' }); q.bus.onSSE('b', () => {}); q.bus.onSSE('a', () => {}); }],
        ['onSSE("a"), stream up, then setSSEQuery({station:7}) and onSSE("b")', async (q) => { q.bus.onSSE('a', () => {}); await q.r.advance(0); q.bus.setSSEQuery({ station: 7 }); q.bus.onSSE('b', () => {}); }],
        ['onSSE("a"), stream up, then setSSEQuery({station:7}) and the stream errors', async (q) => { q.bus.onSSE('a', () => {}); await q.r.advance(0); q.bus.setSSEQuery({ station: 7 }); q.r.streams[0].fail(0); await q.r.advance(2000); }],
        ['window beforeunload with the stream up', async (q) => { q.bus.onSSE('a', () => {}); await q.r.advance(0); q.r.window.dispatchEvent(new Event('beforeunload')); }],
        ['onSSE("a"), stream up, ← connected {"build":"b1"} then {"build":"b2"}, nothing subscribed to build-changed', async (q) => { q.bus.onSSE('a', () => {}); await q.r.advance(0); q.r.streams[0].emit('connected', { build: 'b1' }); q.r.streams[0].emit('connected', { build: 'b2' }); q.r.note('refresh banner in the DOM: ' + !!q.r.doc.getElementById('shingo-refresh-banner')); }],
    ]) {
        const q = busPage();
        const qs = [];
        await timeline(q.r, qs)('a second page: ' + label, async () => { await fn(q); });
        out.push(...qs);
    }
    return out;
}

async function coreHubBusPins() {
    const out = [
        '# SSE — core-hub: what shingo-core/www/static/app.js does with the shared bus on import',
        '# (its onSSE("system-status") subscription keeps one stream open on every Core page).',
        '',
    ];
    const r = makeRealm({ surface: 'core' });
    const step = timeline(r, out);
    await step('a Core page imports app.js', async () => { r.load('/static/app.js'); });
    const bus = r.load('/static/shared/bus.js');
    out.push('    #1 listens for: ' + r.streams[0].types().join(', '));
    const s1 = r.streams[0];
    await step('#1 ← connected {"build":"b1"}', async () => s1.emit('connected', { build: 'b1' }));
    await step('#1 ← system-status {"fleet":"ok"}', async () => s1.emit('system-status', { fleet: 'ok' }));
    await step('#1 error', async () => { s1.fail(0); return waitForOpen(r, 70000); });
    const s2 = r.lastStream();
    // The new-build banner is the notice's, under the key "build" (wave C,
    // s2-hubs-core).
    const shown = () => r.doc.querySelectorAll('[data-banner="build"]');
    const said = () => '"build" banners in the page: ' + shown().length;
    await step('#2 ← connected {"build":"b2"}  (Core redeployed)', async () => { s2.emit('connected', { build: 'b2' }); await r.advance(0); return said(); });
    if (shown().length) {
        out.push('    the banner:');
        out.push(dump(shown()[0], '      '));
    }
    await step('#2 ← heartbeat {"build":"b2"} while it is up', async () => {
        const before = shown()[0];
        s2.emit('heartbeat', { build: 'b2' }); await r.advance(0);
        return said() + (shown()[0] === before ? ', the same node' : ', a new node');
    });
    await step('click its Dismiss (×)', async () => { shown()[0].querySelector('[aria-label="Dismiss"]').click(); await r.advance(0); return said(); });
    await step('#2 ← heartbeat {"build":"b2"} after Dismiss', async () => { s2.emit('heartbeat', { build: 'b2' }); await r.advance(0); return said(); });
    await step('click its Refresh', async () => { shown()[0].querySelector('.notice-action').click(); });
    await step('a page module subscribes onSSE("order-update") after the stream is up', async () => { bus.onSSE('order-update', () => {}); });
    await step('#' + r.lastStream().n + ' error: what the page shows while it is down', async () => {
        const before = r.doc.querySelectorAll('*').length;
        r.lastStream().fail(0);
        await r.advance(10);
        return 'elements in the page before the drop ' + before + ', 10 ms into it ' + r.doc.querySelectorAll('*').length;
    });
    return out;
}

// ═══ htmx-sse.js ═════════════════════════════════════════════════════════════
//
// htmx itself is not loaded: htmx-sse.js calls htmx.defineExtension and then
// only the internal API it is handed at init, so a stub of that API drives
// the shipped extension. The stub's getTriggerSpecs reads only each trigger's
// name ("sse:order-update throttle:3s" -> "sse:order-update"); htmx's own
// throttle and from: modifiers are htmx's, not the extension's.

function htmxRealm() {
    const r = makeRealm({ surface: 'edge' });
    const exts = {};
    const htmx = {
        defineExtension(name, ext) { exts[name] = ext; },
        trigger(elt, name) { r.note('htmx.trigger(' + desc(elt, r) + ', ' + JSON.stringify(name) + ')'); },
    };
    r.ctx.htmx = htmx;
    r.runScript('shingo-edge/www/static/js/htmx-sse.js');
    const api = {
        getInternalData(elt) { return elt.__htmxInternal || (elt.__htmxInternal = {}); },
        getAttributeValue(elt, name) { return elt.getAttribute(name) !== null ? elt.getAttribute(name) : elt.getAttribute('data-' + name); },
        getClosestMatch(elt, pred) { for (let n = elt; n instanceof Element; n = n.parentNode) if (pred(n)) return n; return null; },
        bodyContains(elt) { return r.doc.body.contains(elt); },
        triggerEvent(elt, name) { r.note('event ' + name + ' on ' + desc(elt, r)); return true; },
        triggerErrorEvent(elt, name) { r.note('error event ' + name + ' on ' + desc(elt, r)); },
        getTriggerSpecs(elt) { return (elt.getAttribute('hx-trigger') || '').split(',').map((s) => ({ trigger: s.trim().split(/\s+/)[0] })); },
    };
    exts.sse.init(api);
    return { r, ext: exts.sse, htmx };
}

function templateSSE() {
    const dir = path.join(REPO, 'shingo-edge', 'www', 'templates');
    const rows = [];
    for (const f of fs.readdirSync(dir).sort()) {
        if (!f.endsWith('.html')) continue;
        const src = fs.readFileSync(path.join(dir, f), 'utf8');
        const re = /<[a-z]+\b[^>]*\bsse-connect="([^"]*)"[^>]*>/g;
        let mm;
        while ((mm = re.exec(src))) {
            const tag = mm[0];
            const id = (/\bid="([^"]*)"/.exec(tag) || [])[1] || '(no id)';
            const trig = (/\bhx-trigger="([^"]*)"/.exec(tag) || [])[1] || '';
            const sse = trig.split(',').map((s) => s.trim()).filter((s) => s.indexOf('sse:') === 0);
            rows.push(f + ' #' + id + ': sse-connect ' + JSON.stringify(mm[1]) + ', hx-ext ' + JSON.stringify((/\bhx-ext="([^"]*)"/.exec(tag) || [])[1] || '') + ', sse triggers: ' + sse.join(' | '));
        }
    }
    return rows;
}

async function htmxSSEPins() {
    const out = [
        '# htmx-sse — shingo-edge/www/static/js/htmx-sse.js, the htmx SSE extension (classic script)',
        '# Who connects through it (from the templates):',
    ];
    templateSSE().forEach((row) => out.push('#   ' + row));
    out.push('# One page, steps in order, with #changeover-content as changeover.html writes it.');
    out.push('');
    const { r, ext } = htmxRealm();
    const step = timeline(r, out);
    const elt = mk(r, 'div', {
        id: 'changeover-content', 'hx-ext': 'sse', 'sse-connect': '/events',
        'hx-get': '/changeover/partial?process=1',
        'hx-trigger': 'refreshChangeover from:body, sse:order-update, sse:order-failed, sse:changeover-update',
    });
    r.doc.body.appendChild(elt);
    await step('htmx processes the element (htmx:afterProcessNode)', async () => { ext.onEvent('htmx:afterProcessNode', { target: elt }); });
    out.push('    #1 listens for: ' + r.streams[0].types().join(', '));
    const s1 = r.streams[0];
    await step('#1 open', async () => s1.open());
    await step('#1 ← order-update {"id":1}', async () => s1.emit('order-update', { id: 1 }));
    await step('#1 ← connected {"build":"b1"}; ← connected {"build":"b2"}; ← heartbeat {"build":"b3"}', async () => {
        s1.emit('connected', { build: 'b1' }); s1.emit('connected', { build: 'b2' }); s1.emit('heartbeat', { build: 'b3' });
    });
    await step('#1 error, readyState CONNECTING (the browser retries by itself)', async () => { s1.fail(0); return waitForOpen(r, 10000); });
    out.push('');
    out.push('## a fatal error (readyState CLOSED) and the retry ladder');
    for (let i = 0; i < 10; i++) {
        const s = r.lastStream();
        await step('#' + s.n + ' error, readyState CLOSED', async () => { s.fail(2); return waitForOpen(r, 200000); });
    }
    out.push('    #' + r.lastStream().n + ' listens for: ' + r.lastStream().types().join(', '));
    {
        const s = r.lastStream();
        await step('#' + s.n + ' open, then error, readyState CLOSED', async () => { s.open(); s.fail(2); return waitForOpen(r, 200000); });
    }
    out.push('');
    out.push('## the element leaves the page');
    {
        const s = r.lastStream();
        await step('#' + s.n + ' open; the element is removed from the body; #' + s.n + ' ← order-update {"id":2}', async () => { s.open(); elt.remove(); s.emit('order-update', { id: 2 }); });
        r.doc.body.appendChild(elt);
        const q = htmxRealm();
        const qs = [];
        const qstep = timeline(q.r, qs);
        const e2 = mk(q.r, 'div', { id: 'orders-content', 'sse-connect': '/events', 'hx-trigger': 'sse:order-update throttle:3s' });
        q.r.doc.body.appendChild(e2);
        q.ext.onEvent('htmx:afterProcessNode', { target: e2 });
        await qstep('a second page: #orders-content connected; removed; its stream errors CLOSED', async () => { e2.remove(); q.r.streams[0].fail(2); return waitForOpen(q.r, 200000); });
        out.push(...qs);
        const t = htmxRealm();
        const ts = [];
        const tstep = timeline(t.r, ts);
        const e3 = mk(t.r, 'div', { id: 'orders-content', 'sse-connect': '/events', 'hx-trigger': 'sse:order-update' });
        t.r.doc.body.appendChild(e3);
        await tstep('a third page: #orders-content processed', async () => { t.ext.onEvent('htmx:afterProcessNode', { target: e3 }); });
        await tstep('then htmx:beforeCleanupElement on it (htmx swapping it out)', async () => { t.ext.onEvent('htmx:beforeCleanupElement', { target: e3 }); });
        out.push(...ts);
    }
    return out;
}

// ═══ htmx request errors on Edge admin (a census of the shipped tree) ════════

function walkFiles(dir, pred) {
    const out = [];
    for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
        const p = path.join(dir, e.name);
        if (e.isDirectory()) out.push(...walkFiles(p, pred));
        else if (pred(p)) out.push(p);
    }
    return out.sort();
}

// reachedShared is the shared/*.js modules the given files name by URL,
// followed through the shared modules they name, in file-name order.
function reachedShared(from) {
    const dir = path.join(REPO, 'shared');
    const all = fs.readdirSync(dir).filter((f) => f.endsWith('.js')).sort();
    const named = (src) => all.filter((f) => src.indexOf('/static/shared/' + f) >= 0);
    const reached = new Set();
    let next = [];
    from.forEach((f) => next.push(...named(fs.readFileSync(f, 'utf8'))));
    while (next.length) {
        const f = next.pop();
        if (reached.has(f)) continue;
        reached.add(f);
        next.push(...named(fs.readFileSync(path.join(dir, f), 'utf8')));
    }
    return all.filter((f) => reached.has(f)).map((f) => path.join(dir, f));
}

function htmxErrorPins() {
    const www = path.join(REPO, 'shingo-edge', 'www');
    const rel = (p) => path.relative(REPO, p).split(path.sep).join('/');
    // shared/*.js too: the Edge hub installs the htmx:afterSwap listeners its
    // shared modules define. Only the shared modules Edge admin reaches: a
    // file that an Edge script or template names (/static/shared/<file>), or
    // that such a shared module names in turn. A shared module no Edge page
    // loads (shared/http.js's htmx hook, before wave C installs it) handles
    // nothing on Edge admin, whatever its text says.
    const edge = walkFiles(path.join(www, 'static'), (p) => p.endsWith('.js') && !p.endsWith('.min.js'))
        .concat(walkFiles(path.join(www, 'templates'), (p) => p.endsWith('.html')));
    const files = edge.concat(reachedShared(edge));
    const listens = [];
    const ERR = ['htmx:responseError', 'htmx:sendError', 'htmx:sseError', 'htmx:timeout', 'htmx:swapError', 'htmx:targetError', 'htmx:onLoadError'];
    const errHits = {};
    ERR.forEach((e) => { errHits[e] = []; });
    let configWrites = [];
    let hxOn = [];
    for (const f of files) {
        const src = fs.readFileSync(f, 'utf8');
        const re = /addEventListener\(\s*['"](htmx:[\w-]+)['"]/g;
        let mm;
        while ((mm = re.exec(src))) listens.push(rel(f) + ': ' + mm[1]);
        ERR.forEach((e) => {
            if (src.indexOf("'" + e + "'") >= 0 || src.indexOf('"' + e + '"') >= 0 || src.indexOf('hx-on::' + e.slice(5).replace(/[A-Z]/g, (c) => '-' + c.toLowerCase())) >= 0) {
                if (!/htmx-sse\.js$/.test(f)) errHits[e].push(rel(f));
            }
        });
        if (/htmx\.config\.\w+\s*=/.test(src)) configWrites.push(rel(f));
        if (/\bhx-on[:-]/.test(src)) hxOn.push(rel(f));
    }
    const metaConfig = walkFiles(path.join(www, 'templates'), (p) => p.endsWith('.html'))
        .filter((f) => /<meta[^>]+name="htmx-config"/.test(fs.readFileSync(f, 'utf8'))).map(rel);
    const min = fs.readFileSync(path.join(www, 'static', 'js', 'htmx.min.js'), 'utf8');
    const version = (/version:"([^"]+)"/.exec(min) || [])[1] || '(not found)';
    const rh = (/responseHandling:(\[(?:\{[^}]*\},?)*\])/.exec(min) || [])[1] || '(not found)';
    // htmx's error-event path: triggerErrorEvent logs n.error through its
    // console.error wrapper. Pinned as the shipped text, so an htmx upgrade
    // that stops logging shows here.
    const logs = /if\(n\.error\)\{O\(/.test(min) && /function O\(e\)\{if\(console\.error\)\{console\.error\(e\)/.test(min);
    const out = [
        '# htmx request errors — Edge admin (shingo-edge/www/static and templates, tests and .min.js left out)',
        '',
        'htmx.min.js version: ' + version,
        'htmx default responseHandling (no page overrides it): ' + rh,
        'htmx logs an error event\'s error with console.error: ' + (logs ? 'yes' : 'not found in htmx.min.js'),
        'htmx.config written by: ' + (configWrites.length ? configWrites.join(', ') : 'nothing'),
        '<meta name="htmx-config"> in: ' + (metaConfig.length ? metaConfig.join(', ') : 'no template'),
        'hx-on attributes in: ' + (hxOn.length ? hxOn.join(', ') : 'nothing'),
        '',
        '## who listens for an htmx error event',
    ];
    ERR.forEach((e) => out.push(e + ': ' + (errHits[e].length ? errHits[e].join(', ') : 'nobody')));
    out.push('');
    out.push('## every htmx: listener on Edge admin');
    listens.forEach((l) => out.push(l));
    out.push('');
    out.push('## so, on a partial refresh (hx-get) that answers 4xx/5xx, or never answers');
    const nobody = ERR.slice(0, 2).every((e) => errHits[e].length === 0);
    out.push(nobody
        ? '4xx/5xx: htmx does not swap (responseHandling [45].. swap:false error:true) and fires htmx:responseError; nothing on the page listens, so the panel keeps its old content and the operator is shown nothing.'
        : 'a page listens for htmx:responseError or htmx:sendError: see above.');
    if (nobody) out.push('no answer: htmx fires htmx:sendError (and htmx:afterRequest); nothing on the page listens, so the same.');
    return out;
}

// ═══ hub census: which copy each surface hands a page ═══════════════════════

function hubCensus() {
    const names = ['escapeHtml', 'h', 'el', 'debounce', 'delegateActions', 'installBackdropClose', 'showModal', 'hideModal',
        'confirm', 'prompt', 'uiConfirm', 'uiConfirmWithInput', 'uiPrompt', 'toast', 'api', 'apiGet', 'apiPost', 'apiPut',
        'apiDelete', 'apiResult', 'createSSE', 'onSSE', 'showRefreshBanner'];
    const out = [
        '# hubs — for each helper name, what each surface\'s module exports under it:',
        '#   own     the module defines it (its own copy)',
        '#   shared  the module re-exports the one in shared/',
        '#   -       not exported',
        '# and, under shared, the shared/ file that defines it.',
        '',
        'name'.padEnd(22) + 'core-hub  edge-hub  shared',
    ];
    const added = (w, before) => Object.keys(w).filter((k) => before.indexOf(k) < 0).join(', ') || 'nothing';
    const core = makeRealm({ surface: 'core' });
    const coreWindowBefore = Object.keys(core.window);
    const cm = core.load('/static/app.js');
    core.load('/static/shared/utils.js');
    const coreAdded = added(core.window, coreWindowBefore);
    const edge = makeRealm({ surface: 'edge' });
    const edgeWindowBefore = Object.keys(edge.window);
    const em = edge.load('/static/js/shingoedge.js');
    edge.load('/static/shared/utils.js');
    const edgeAdded = added(edge.window, edgeWindowBefore);
    // A hub's export is "shared" when it is the very value the defining shared
    // module exports in the same realm (one module instance per URL).
    const home = sharedHome();
    const kind = (r, mod, n) => (!(n in mod) ? '-'
        : (home[n] && mod[n] === r.load('/static/shared/' + home[n])[n]) ? 'shared' : 'own');
    for (const n of names) {
        out.push(n.padEnd(22) + kind(core, cm, n).padEnd(10) + kind(edge, em, n).padEnd(10) + (home[n] || '-'));
    }
    out.push('');
    out.push('edge-hub window.ShingoEdge: {' + Object.keys(edge.window.ShingoEdge || {}).join(', ') + '}');
    out.push('edge-hub window.ShingoEdge entries that are the module\'s exports: '
        + Object.keys(edge.window.ShingoEdge || {}).filter((k) => edge.window.ShingoEdge[k] === em[k]).length + ' of ' + Object.keys(edge.window.ShingoEdge || {}).length);
    out.push('loading the hub (and the shared/ modules it imports) sets on window: core-hub ' + coreAdded
        + '; edge-hub ' + edgeAdded);
    return out;
}

// sharedHome maps each name a shared/*.js module exports to that file, read
// off the export lines. shared/helper_pins_test.go holds that no two files
// export one name, save a copy it lists: utils.js's showModal, hideModal,
// confirm, prompt and toast beside layer.js's and notice.js's, until wave C.
// The files are read in name order, so utils.js, the copy the hubs take
// today, is the one named here.
function sharedHome() {
    const out = {};
    fs.readdirSync(path.join(REPO, 'shared')).filter((f) => f.endsWith('.js')).sort().forEach((f) => {
        const src = fs.readFileSync(path.join(REPO, 'shared', f), 'utf8');
        for (const m of src.matchAll(/^export\s+(?:async\s+)?(?:function\s*\*?|const|let|var|class)\s*([\w$]+)/gm)) out[m[1]] = f;
    });
    return out;
}

// ═══ main ════════════════════════════════════════════════════════════════════

module.exports = { modalPins, dialogPins, toastPins, apiPins };

if (require.main === module) (async function main() {
    golden('hubs.txt', hubCensus());
    for (const s of SURFACES) {
        golden('modal.' + s.name + '.txt', modalPins(s));
        golden('dialog.' + s.name + '.txt', await dialogPins(s));
        golden('toast.' + s.name + '.txt', await toastPins(s));
        golden('api.' + s.name + '.txt', await apiPins(s));
    }
    golden('sse.edge-hub.txt', await createSSEPins('edge-hub'));
    golden('sse.shared-createSSE.txt', await createSSEPins('shared'));
    golden('sse.shared-bus.txt', await busPins());
    golden('sse.core-hub.txt', await coreHubBusPins());
    golden('sse.htmx-sse.txt', await htmxSSEPins());
    golden('htmx-errors.edge-admin.txt', htmxErrorPins());
    process.stdout.write(JSON.stringify(goldens));
})().catch((e) => {
    console.error('helper pins crashed:', e && e.stack ? e.stack : e);
    process.exit(1);
});
