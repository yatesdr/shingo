// layer.test.js — the spec of shared/layer.js. Run by shared/components_test.go
// (TestSharedComponentSpecs).

'use strict';

const { test, run, page, mk, key, press, watch, eq, ok } = require('./spec.js');

// A page with two markup modals (#m1 holds the fields the helper pins use) and
// the button that had focus before anything opened.
function modals(attrs) {
    const p = page('layer.js');
    const L = p.mods[0];
    const r = p.r;
    const opener = mk(r, 'button', { id: 'opener' }, ['Open']);
    const card = mk(r, 'div', { class: 'modal', id: 'm1-card' }, [
        mk(r, 'input', { id: 'm1-text', type: 'text', value: 'dflt' }),
        mk(r, 'input', { id: 'm1-check', type: 'checkbox', checked: '' }),
        mk(r, 'select', { id: 'm1-sel' }, [mk(r, 'option', { value: 'a' }, ['A']), mk(r, 'option', { value: 'b', selected: '' }, ['B']), mk(r, 'option', { value: 'c' }, ['C'])]),
        mk(r, 'textarea', { id: 'm1-area' }, ['area-dflt']),
        mk(r, 'input', { id: 'm1-hidden', type: 'hidden', value: 'keep' }),
    ]);
    const m1 = mk(r, 'div', Object.assign({ id: 'm1', class: 'modal-overlay' }, attrs || {}), [card]);
    const m2 = mk(r, 'div', { id: 'm2', class: 'modal-overlay' }, [mk(r, 'div', { class: 'modal' }, [mk(r, 'input', { id: 'm2-text' })])]);
    [opener, m1, m2].forEach((e) => r.doc.body.appendChild(e));
    opener.focus();
    const g = (id) => r.doc.getElementById(id);
    return { p, L, r, opener, m1, m2, card, g, open: (e) => e.classList.contains('active') };
}
function type(f) { f.g('m1-text').value = 'typed'; }
const question = (r) => r.doc.querySelectorAll('.layer-scrim');
const button = (scrim, words) => scrim.querySelectorAll('button').find((b) => b.textContent === words);

// ── import ──────────────────────────────────────────────────────────────────

test('nothing happens at import: no listener, no element, no lock', async () => {
    const p = page('layer.js');
    eq(p.doc.listenerTypes(), [], 'document listeners');
    eq(p.doc.body.children.length, 0, 'body children');
    eq(p.doc.body.style.overflow, '', 'overflow');
    eq(p.mods[0].layerDepth(), 0, 'depth');
});

test('the first entry wires one keydown, one press pair and one click, once', async () => {
    const f = modals();
    f.L.showModal('m1');
    f.L.showModal('m2');
    eq(f.r.doc.listenerTypes().sort(), ['click', 'keydown', 'mousedown', 'touchstart']);
});

// ── Escape ──────────────────────────────────────────────────────────────────

test('Escape closes the innermost entry first, one per press', async () => {
    const f = modals();
    f.L.showModal('m1');
    f.L.showModal('m2');
    key(f.r.doc.body, 'Escape');
    eq([f.open(f.m1), f.open(f.m2)], [true, false], 'after one Escape');
    key(f.r.doc.body, 'Escape');
    eq([f.open(f.m1), f.open(f.m2)], [false, false], 'after two');
});

test('cannot happen: Escape closes the last overlay in document order, not the last opened (shared/utils.js:641)', async () => {
    const f = modals();
    f.L.showModal('m2');
    f.L.showModal('m1');   // m1 is first in the document, opened last
    key(f.r.doc.body, 'Escape');
    eq([f.open(f.m1), f.open(f.m2)], [false, true]);
});

test('Escape inside a field of the entry closes it', async () => {
    const f = modals();
    f.L.showModal('m1');
    key(f.g('m1-text'), 'Escape');
    eq(f.open(f.m1), false);
});

test('Escape with nothing open is left to the page: not prevented', async () => {
    const f = modals();
    eq(key(f.r.doc.body, 'Escape').defaultPrevented, false, 'before any entry');
    f.L.showModal('m1');
    eq(key(f.r.doc.body, 'Escape').defaultPrevented, true, 'closing m1');
    eq(key(f.r.doc.body, 'Escape').defaultPrevented, false, 'after it closed');
    eq(f.L.layerDepth(), 0);
});

test('a widget that handles Escape itself (preventDefault) keeps it', async () => {
    const f = modals();
    f.L.showModal('m1');
    f.g('m1-text').addEventListener('keydown', (e) => e.preventDefault());
    key(f.g('m1-text'), 'Escape');
    eq(f.open(f.m1), true);
});

test('another key does nothing', async () => {
    const f = modals();
    f.L.showModal('m1');
    key(f.r.doc.body, 'Enter');
    eq(f.open(f.m1), true);
});

// ── scroll lock ─────────────────────────────────────────────────────────────

test('the scroll lock is counted and gives back what the page had', async () => {
    const f = modals();
    f.r.doc.body.style.overflow = 'auto';
    f.L.showModal('m1');
    f.L.showModal('m2');
    eq(f.r.doc.body.style.overflow, 'hidden', 'two open');
    f.L.hideModal('m1');
    eq(f.r.doc.body.style.overflow, 'hidden', 'one open');
    f.L.hideModal('m2');
    eq(f.r.doc.body.style.overflow, 'auto', 'none open');
});

test('showing one modal twice is one entry: one hide closes it and unlocks', async () => {
    const f = modals();
    f.L.showModal('m1');
    f.L.showModal('m1');
    eq(f.L.layerDepth(), 1, 'depth');
    f.L.hideModal('m1');
    eq([f.open(f.m1), f.r.doc.body.style.overflow], [false, '']);
});

test('cannot happen: a backdrop close leaves the body scroll-locked (shingoedge.js:129 + shared/utils.js:942)', async () => {
    const f = modals({ 'data-backdrop-close': '' });
    f.L.showModal('m1');
    press(f.m1);
    eq([f.open(f.m1), f.r.doc.body.style.overflow], [false, '']);
});

// ── backdrop (ruling 2) ─────────────────────────────────────────────────────

test('a backdrop click closes nothing unless the entry opts in', async () => {
    const f = modals();
    f.L.showModal('m1');
    press(f.m1);
    eq(f.open(f.m1), true);
});

test('data-backdrop-close and {closeOnBackdrop:true} each opt in, with no install call', async () => {
    const a = modals({ 'data-backdrop-close': '' });
    a.L.showModal('m1');
    press(a.m1);
    const b = modals();
    b.L.showModal('m1', { closeOnBackdrop: true });
    press(b.m1);
    eq([a.open(a.m1), b.open(b.m1)], [false, false]);
});

test('cannot happen: a backdrop close fires on a drag that began in the card (shared/utils.js:650)', async () => {
    const f = modals();
    f.L.showModal('m1', { closeOnBackdrop: true });
    press(f.card, f.m1);
    eq(f.open(f.m1), true);
});

test('a click inside the card is not a backdrop click', async () => {
    const f = modals({ 'data-backdrop-close': '' });
    f.L.showModal('m1');
    press(f.card);
    press(f.g('m1-text'));
    eq(f.open(f.m1), true);
});

test('a backdrop close tells the page (backdropclose) and onClose says why', async () => {
    const f = modals({ 'data-backdrop-close': '' });
    let events = 0;
    const why = [];
    f.m1.addEventListener('backdropclose', () => { events++; });
    f.L.showModal('m1', { onClose: (w) => why.push(w) });
    press(f.m1);
    eq([events, why], [1, ['backdrop']]);
});

test('a backdrop click is Escape: it asks before discarding typed work', async () => {
    const f = modals({ 'data-backdrop-close': '' });
    f.L.showModal('m1');
    type(f);
    press(f.m1);
    eq([f.open(f.m1), question(f.r).length], [true, 1], 'the question is up over the modal');
    button(question(f.r)[0], 'Discard').click();
    await f.r.advance(0);
    eq([f.open(f.m1), f.g('m1-text').value, question(f.r).length], [false, 'dflt', 0], 'Discard closed and cleared it');
});

test('only the innermost entry answers a backdrop click', async () => {
    const f = modals({ 'data-backdrop-close': '' });
    f.L.showModal('m1');
    f.L.showModal('m2');
    press(f.m1);
    eq([f.open(f.m1), f.open(f.m2)], [true, true]);
});

// ── ask before discard ──────────────────────────────────────────────────────

test('Escape on an entry with nothing typed closes it without asking', async () => {
    const f = modals();
    f.L.showModal('m1');
    key(f.r.doc.body, 'Escape');
    eq([f.open(f.m1), question(f.r).length], [false, 0]);
});

test('Escape on typed work asks; Escape on the question is Keep editing and keeps every field', async () => {
    const f = modals();
    f.L.showModal('m1');
    type(f);
    f.g('m1-check').checked = false;
    key(f.g('m1-text'), 'Escape');
    eq(question(f.r).length, 1, 'asked');
    eq(question(f.r)[0].textContent.indexOf('Discard the changes?') >= 0, true, 'the words');
    key(f.r.doc.body, 'Escape');
    await f.r.advance(0);
    eq([f.open(f.m1), question(f.r).length, f.g('m1-text').value, f.g('m1-check').checked], [true, 0, 'typed', false]);
    eq(f.r.doc.activeElement.id, 'm1-text', 'focus back in the field');
});

test('Keep editing keeps the entry and its work; a second Escape asks again', async () => {
    const f = modals();
    f.L.showModal('m1');
    type(f);
    key(f.r.doc.body, 'Escape');
    button(question(f.r)[0], 'Keep editing').click();
    await f.r.advance(0);
    eq([f.open(f.m1), f.g('m1-text').value], [true, 'typed']);
    key(f.r.doc.body, 'Escape');
    eq(question(f.r).length, 1);
});

test('Escape while the question is up answers it; it never stacks a second question', async () => {
    const f = modals();
    f.L.showModal('m1');
    type(f);
    key(f.r.doc.body, 'Escape');
    f.L.showModal('m1');   // a stray re-show is no second entry
    key(f.r.doc.body, 'Escape');
    eq([question(f.r).length, f.open(f.m1)], [0, true], 'the question answered, the modal kept');
    key(f.r.doc.body, 'Escape');
    eq(question(f.r).length, 1, 'asked again, once');
});

test('a deliberate close (hideModal, closeLayer, a sheet\'s Cancel) never asks', async () => {
    const f = modals();
    f.L.showModal('m1');
    type(f);
    f.L.hideModal('m1');
    const s = f.L.openSheet({ title: 'T', body: mk(f.r, 'input', { 'data-f': 'x' }) });
    s.el.querySelector('input').value = 'typed';
    button(s.el, 'Cancel').click();
    eq([f.open(f.m1), s.open, question(f.r).length], [false, false, 0]);
});

test('a select, a checkbox and a textarea count as typed work', async () => {
    for (const edit of [(f) => { f.g('m1-sel').selectedIndex = 2; }, (f) => { f.g('m1-check').checked = false; }, (f) => { f.g('m1-area').value = 'x'; }]) {
        const f = modals();
        f.L.showModal('m1');
        edit(f);
        key(f.r.doc.body, 'Escape');
        eq(question(f.r).length, 1);
    }
});

test('a field drawn after the entry opened is read against its markup (data-was or value)', async () => {
    const f = modals();
    f.L.showModal('m1');
    const late = mk(f.r, 'input', { 'data-was': '5', value: '5' });
    f.card.appendChild(late);
    key(f.r.doc.body, 'Escape');
    eq(f.open(f.m1), false, 'unchanged: closes');
    f.L.showModal('m1');
    const late2 = mk(f.r, 'input', { value: '5' });
    f.card.appendChild(late2);
    late2.value = '6';
    key(f.r.doc.body, 'Escape');
    eq(question(f.r).length, 1, 'changed: asks');
});

test('a data-scratch field (a search box) is never work; a data-dirty element always is', async () => {
    const f = modals();
    const q = mk(f.r, 'input', { 'data-scratch': '' });
    f.card.appendChild(q);
    f.L.showModal('m1');
    q.value = 'PLN';
    key(f.r.doc.body, 'Escape');
    eq(f.open(f.m1), false, 'scratch typing: closes');
    f.L.showModal('m1');
    f.card.appendChild(mk(f.r, 'div', { 'data-dirty': '' }));
    key(f.r.doc.body, 'Escape');
    eq(question(f.r).length, 1, 'dirty: asks');
});

test('markClean takes what the page filled in after opening as the starting point', async () => {
    const f = modals();
    const h = f.L.showModal('m1');
    f.g('m1-text').value = 'loaded from the server';
    h.markClean();
    key(f.r.doc.body, 'Escape');
    eq(f.open(f.m1), false);
});

// ── fields on close ─────────────────────────────────────────────────────────

test('fields go back to their markup on hide; a hidden input is left alone', async () => {
    const f = modals();
    f.L.showModal('m1');
    type(f);
    f.g('m1-check').checked = false;
    f.g('m1-sel').selectedIndex = 2;
    f.g('m1-area').value = 'x';
    f.g('m1-hidden').value = 'id-7';
    f.L.hideModal('m1');
    eq([f.g('m1-text').value, f.g('m1-check').checked, f.g('m1-sel').selectedIndex, f.g('m1-area').value, f.g('m1-hidden').value],
        ['dflt', true, 1, 'area-dflt', 'id-7']);
});

test('preserveState keeps the fields: on hideModal, or for every close when given to showModal', async () => {
    const f = modals();
    f.L.showModal('m1');
    type(f);
    f.L.hideModal('m1', { preserveState: true });
    eq(f.g('m1-text').value, 'typed', 'hideModal {preserveState}');
    const g = modals({ 'data-backdrop-close': '' });
    g.L.showModal('m1', { preserveState: true, ask: false });
    type(g);
    press(g.m1);
    eq([g.open(g.m1), g.g('m1-text').value], [false, 'typed'], 'showModal {preserveState}, backdrop');
});

test('Discard clears what was typed', async () => {
    const f = modals();
    f.L.showModal('m1');
    type(f);
    key(f.r.doc.body, 'Escape');
    button(question(f.r)[0], 'Discard').click();
    await f.r.advance(0);
    eq([f.open(f.m1), f.g('m1-text').value], [false, 'dflt']);
});

test('a missing id is no error, on show or hide', async () => {
    const f = modals();
    eq([f.L.showModal('nope'), f.L.hideModal('nope')], [null, undefined]);
});

// ── focus ───────────────────────────────────────────────────────────────────

test('cannot happen: no surface moves focus in or returns it (P0: shingoedge.js, app.js, utils.js, processes-desktop.js openSheet / closeSheet)', async () => {
    const f = modals();
    f.L.showModal('m1');
    eq(f.r.doc.activeElement.id, 'm1-text', 'in, on open');
    f.L.hideModal('m1');
    eq(f.r.doc.activeElement.id, 'opener', 'back, on close');
    const s = f.L.openSheet({ title: 'T', body: mk(f.r, 'input', { 'data-f': 'x' }) });
    eq(f.r.doc.activeElement === s.el.querySelector('input'), true, 'into a sheet, its first field');
    key(f.r.doc.body, 'Escape');
    eq(f.r.doc.activeElement.id, 'opener', 'back from a sheet');
});

test('focus goes where asked: an element, a selector, [autofocus], else the first field, else the first button', async () => {
    const f = modals();
    f.L.showModal('m1', { focus: '#m1-area' });
    eq(f.r.doc.activeElement.id, 'm1-area', 'selector');
    f.L.hideModal('m1');
    f.g('m1-sel').setAttribute('autofocus', '');
    f.L.showModal('m1');
    eq(f.r.doc.activeElement.id, 'm1-sel', '[autofocus]');
    f.L.hideModal('m1');
    f.L.showModal('m1', { focus: false });
    eq(f.r.doc.activeElement.id, 'opener', 'focus: false');
});

test('closing an entry under the focused one leaves focus where it is', async () => {
    const f = modals();
    f.L.showModal('m1');
    f.L.showModal('m2');
    f.L.hideModal('m1');
    eq(f.r.doc.activeElement.id, 'm2-text');
});

test('an entry opened from a field of one that closed first returns focus to that one\'s opener', async () => {
    const f = modals();
    f.L.showModal('m1');
    f.L.showModal('m2');   // opened from #m1-text
    f.L.hideModal('m1');
    f.L.hideModal('m2');
    eq(f.r.doc.activeElement.id, 'opener');
});

test('an opener that left the page hands focus to the innermost entry left', async () => {
    const f = modals();
    f.L.showModal('m1');
    const b = mk(f.r, 'button', { id: 'gone' }, ['x']);
    f.card.appendChild(b);
    b.focus();
    const btn = mk(f.r, 'button', {}, ['ok']);
    const pop = mk(f.r, 'div', { id: 'pop' }, [btn]);
    f.r.doc.body.appendChild(pop);
    const h = f.L.openLayer(pop, { kind: 'popover' });
    b.remove();
    h.close();
    eq(f.r.doc.activeElement.id, 'm1-text');
});

// ── confirm / prompt ────────────────────────────────────────────────────────

test('a confirm resolves once, whatever closes it: OK, Cancel, Escape, closeLayer', async () => {
    const out = [];
    for (const how of ['OK', 'Cancel', 'Escape', 'closeLayer']) {
        const f = modals();
        const w = watch(f.L.confirm('Delete?'));
        const s = question(f.r)[0];
        if (how === 'OK') { button(s, 'Confirm').click(); button(s, 'Confirm').click(); }
        if (how === 'Cancel') button(s, 'Cancel').click();
        if (how === 'Escape') key(f.r.doc.body, 'Escape');
        if (how === 'closeLayer') f.L.closeLayer(s);
        await f.r.advance(0);
        out.push(how + ' ' + w.state + ' ' + w.value + ' ' + question(f.r).length);
    }
    eq(out, ['OK resolved true 0', 'Cancel resolved false 0', 'Escape resolved false 0', 'closeLayer resolved false 0']);
});

test('cannot happen: Escape on a confirm or prompt after the page used showModal hides it and the promise never resolves (shared/utils.js:641)', async () => {
    const f = modals();
    f.L.showModal('m1');
    f.L.hideModal('m1');
    const c = watch(f.L.confirm('Delete?'));
    key(f.r.doc.body, 'Escape');
    const pr = watch(f.L.prompt('Name?'));
    key(f.r.doc.body, 'Escape');
    await f.r.advance(0);
    eq([c.state, c.value, pr.state, pr.value, question(f.r).length], ['resolved', false, 'resolved', null, 0]);
});

test('cannot happen: Escape in a prompt over a modal also closes the modal (shared/utils.js:702 then :641)', async () => {
    const f = modals();
    f.L.showModal('m1');
    const w = watch(f.L.prompt('Name?'));
    key(question(f.r)[0].querySelector('input'), 'Escape');
    await f.r.advance(0);
    eq([w.value, f.open(f.m1), f.L.layerDepth()], [null, true, 1]);
});

test('a confirm\'s backdrop does nothing unless opted in; opted in, it answers false', async () => {
    const f = modals();
    const a = watch(f.L.confirm('A?'));
    press(question(f.r)[0]);
    await f.r.advance(0);
    eq(a.state, 'pending', 'not opted in');
    button(question(f.r)[0], 'Cancel').click();
    const b = watch(f.L.confirm('B?', { backdrop: true }));
    press(question(f.r)[0]);
    await f.r.advance(0);
    eq([b.state, b.value], ['resolved', false], 'opted in');
});

test('a confirm writes its message as text, keeps its words and focuses Cancel', async () => {
    const f = modals();
    f.L.confirm('Delete <b>BIN-1</b>?\n\nSave anyway?', { ok: 'Delete', cancel: 'Keep' });
    const s = question(f.r)[0];
    eq(s.querySelector('.layer-msg').textContent, 'Delete <b>BIN-1</b>?\n\nSave anyway?');
    eq(s.querySelectorAll('button').map((b) => b.textContent), ['Keep', 'Delete']);
    eq(f.r.doc.activeElement.textContent, 'Keep');
});

test('a prompt resolves the text on OK or Enter, null otherwise, and focuses its input at once', async () => {
    const f = modals();
    const a = watch(f.L.prompt('Count?', { type: 'number', min: 0, max: 9, value: 0 }));
    const i = question(f.r)[0].querySelector('input');
    eq([f.r.doc.activeElement === i, i.type, i.min, i.max, i.value], [true, 'number', '0', '9', '0']);
    i.value = '7';
    key(i, 'Enter');
    await f.r.advance(0);
    eq([a.value, question(f.r).length, f.r.doc.activeElement.id], ['7', 0, 'opener']);
});

test('confirmWithInput resolves {ok, value}', async () => {
    const f = modals();
    const a = watch(f.L.confirmWithInput('Hold?', { placeholder: 'Reason' }));
    const i = question(f.r)[0].querySelector('input');
    i.value = 'dent';
    button(question(f.r)[0], 'Confirm').click();
    const b = watch(f.L.confirmWithInput('Hold?'));
    key(f.r.doc.body, 'Escape');
    await f.r.advance(0);
    eq([a.value, b.value, i.placeholder], [{ ok: true, value: 'dent' }, { ok: false, value: '' }, 'Reason']);
});

test('two confirms stack: Escape answers the second, the first stays', async () => {
    const f = modals();
    const a = watch(f.L.confirm('first'));
    const b = watch(f.L.confirm('second'));
    key(f.r.doc.body, 'Escape');
    await f.r.advance(0);
    eq([a.state, b.state, b.value, question(f.r).length], ['pending', 'resolved', false, 1]);
});

// ── popovers ────────────────────────────────────────────────────────────────

function popover(f, opts) {
    const anchor = mk(f.r, 'button', { id: 'anchor' }, ['v']);
    const item = mk(f.r, 'button', { id: 'item' }, ['Pick']);
    const pop = mk(f.r, 'div', { id: 'pop' }, [item]);
    pop.hidden = true;
    f.card.appendChild(anchor);
    f.card.appendChild(pop);
    return { anchor, item, pop, h: f.L.openLayer(pop, Object.assign({ kind: 'popover', anchor }, opts || {})) };
}

test('a popover shows by its hidden property, locks nothing and takes focus in', async () => {
    const f = modals();
    const q = popover(f);
    eq([q.pop.hidden, f.r.doc.body.style.overflow, f.r.doc.activeElement.id], [false, '', 'item']);
});

test('Escape closes a popover before the sheet under it', async () => {
    const f = modals();
    f.L.showModal('m1');
    const q = popover(f);
    key(q.item, 'Escape');
    eq([q.pop.hidden, f.open(f.m1)], [true, true], 'first Escape');
    key(f.r.doc.body, 'Escape');
    eq(f.open(f.m1), false, 'second Escape');
});

test('a press outside a popover closes it; on its anchor or inside it does not', async () => {
    const f = modals();
    f.L.showModal('m1');
    const q = popover(f);
    press(q.anchor);
    press(q.item);
    eq(q.pop.hidden, false, 'anchor, inside');
    press(f.g('m1-text'));
    eq([q.pop.hidden, f.open(f.m1)], [true, true], 'outside: the popover only');
});

test('closing an entry closes what was opened inside it first', async () => {
    const f = modals();
    f.L.showModal('m1');
    const why = [];
    const q = popover(f, { onClose: (w) => why.push(w) });
    f.L.hideModal('m1');
    eq([q.pop.hidden, why, f.L.layerDepth()], [true, ['parent'], 0]);
});

test('sheets, confirms, prompts and popovers all register in the one stack', async () => {
    const f = modals();
    f.L.showModal('m1');
    f.L.openSheet({ title: 'S' });
    popover(f);
    f.L.confirm('c');
    f.L.prompt('p');
    eq(f.L.layerDepth(), 5);
});

// ── the sheet shell: one run per press, its state, its status ──────────────

test('a sheet runs once per press: a second press while it runs does nothing, OK is off for the run', async () => {
    const f = modals();
    let runs = 0;
    let release;
    const s = f.L.openSheet({ title: 'Add', run: () => { runs++; return new Promise((res) => { release = res; }); } });
    const okBtn = s.el.querySelector('[data-layer-ok]');
    okBtn.click();
    okBtn.click();
    await f.r.advance(0);
    eq([runs, okBtn.disabled], [1, true], 'while it runs');
    release();
    await f.r.advance(0);
    eq([runs, okBtn.disabled], [1, false], 'after');
});

test('the door\'s half and the layer\'s half of the apply wedge (processes-desktop.js:7895 + :5917): a run whose write gets no answer gives OK back', async () => {
    const p = page('layer.js', 'http.js');
    const [L, H] = p.mods;
    p.r.answer = () => ({ reject: 'Failed to fetch' });
    const s = L.openSheet({
        title: 'Apply',
        run: async (state, h) => {
            h.status('Saving…');
            const out = await H.http.post('/api/x', { a: 1 }, { notice: false });
            if (!out.ok) h.status(out.error, true);
        },
    });
    const okBtn = s.el.querySelector('[data-layer-ok]');
    okBtn.click();
    await p.r.advance(0);
    const st = s.el.querySelector('[data-layer-status]');
    eq([okBtn.disabled, st.textContent, st.className, s.open], [false, 'No answer from the server', 'layer-status is-bad', true]);
});

test('ruling 5: the sheet keeps what a chain made across presses and drops it when it closes', async () => {
    const f = modals();
    const made = [];
    let refuse = true;
    const s = f.L.openSheet({
        title: 'Add process',
        run: async (state) => {
            if (!state.process) { state.process = 41; made.push('process'); }
            if (refuse) { refuse = false; return; }   // the second step refused
            if (!state.counter) { state.counter = 7; made.push('counter'); }
        },
    });
    const okBtn = s.el.querySelector('[data-layer-ok]');
    okBtn.click();
    await f.r.advance(0);
    okBtn.click();
    await f.r.advance(0);
    eq(made, ['process', 'counter'], 'the second press resumed at the failed step');
    eq(s.state, { process: 41, counter: 7 }, 'state while open');
    s.close();
    eq([s.state, s.open], [null, false], 'dropped on close');
});

test('a sheet opts into the backdrop as the ruling says Processes sheets do; by default it does not', async () => {
    const f = modals();
    const a = f.L.openSheet({ title: 'A' });
    press(a.el);
    const b = f.L.openSheet({ title: 'B', backdrop: true });
    press(b.el);
    eq([a.open, b.open], [true, false]);
});

test('a sheet\'s body may be markup the page built or a node; its title labels the card', async () => {
    const f = modals();
    const s = f.L.openSheet({ title: 'Rename', sub: 'style', body: '<p>x</p>' });
    const n = mk(f.r, 'p', {}, ['y']);
    const t = f.L.openSheet({ title: 'Other', body: n, cardClass: 'pd-modal' });
    eq([s.el.querySelector('.layer-body').innerHTML, s.el.querySelector('[role="dialog"]').getAttribute('aria-label'),
        t.el.contains(n), !!t.el.querySelector('.pd-modal')], ['<p>x</p>', 'Rename', true, true]);
});

run();
