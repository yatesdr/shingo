// layer.js — the one overlay stack. Every sheet, modal, confirm, prompt and
// popover a page opens is an entry here, innermost last, and one set of rules
// answers for all of them (style guide §10, "The layer"):
//
//   - Escape closes the innermost entry, and only it.
//   - A backdrop click is Escape for the innermost entry, where the entry opts
//     in; a press that began inside the card never closes it.
//   - An entry with typed work asks before Escape or the backdrop discards it.
//   - The body's scroll lock is COUNTED: it lifts when the last locking entry
//     closes, back to whatever the page had set.
//   - Focus moves into an entry when it opens and back to its opener when it
//     closes.
//   - Fields go back to their defaults when an entry closes, unless it keeps
//     its state.
//   - A confirm or prompt always resolves, whatever closes it.
//
// WHY ONE STACK. The three copies of showModal (Core's app.js, Edge's
// shingoedge.js, utils.js) and the Processes page's sheet stack each kept
// their own idea of "the open one". Escape closed the last open overlay in
// document order rather than the last opened, a prompt's own Escape and the
// document's both fired, a confirm's overlay was stripped of .active by a
// handler that did not know it was a confirm and its promise never resolved,
// and a backdrop close on the Edge left the page scroll-locked. One list of
// what is open, in the order it opened, makes each of those a question with
// one answer. The P0 helper pins hold each old behaviour; this module's spec
// (shared/testdata/components/layer.test.js) names each as cannot-happen.
//
// NOTHING HAPPENS AT IMPORT. The document listeners (one keydown, one press,
// one click) go on with the first entry and stay: a page that never opens one
// pays nothing.

import { el } from './utils.js';

// The defaults per kind. A popover is a list or menu anchored to a control: it
// does not lock the page, has no typed work to lose, keeps its fields and
// closes on a press outside it. A dialog is one this module builds (confirm,
// prompt) and removes again.
const KINDS = {
    modal:   { reveal: 'active', lock: true,  ask: true,  keep: false, outside: false },
    sheet:   { reveal: 'active', lock: true,  ask: true,  keep: false, outside: false },
    dialog:  { reveal: 'remove', lock: true,  ask: false, keep: true,  outside: false },
    popover: { reveal: 'hidden', lock: false, ask: false, keep: true,  outside: true },
};

const stack = [];
let wired = false;
let pressedOn = null;
let locks = 0;
let overflowBefore = '';

// openLayer registers node as the innermost entry and shows it. opts:
//   kind          'modal' (default), 'sheet', 'dialog' or 'popover' (KINDS)
//   card          the element a press must start outside of to be a backdrop
//                 press (default: the node's first element child; a popover is
//                 its own card)
//   reveal        how it shows and hides: 'active' (the .active class),
//                 'hidden' (the hidden property), 'remove' (taken out of the
//                 document on close) or 'none' (the page shows and hides it)
//   backdrop      true: a click on the backdrop is Escape for this entry
//                 (ruling 2: opt-in per entry)
//   ask           ask before Escape or the backdrop discards typed work
//   preserveState keep the fields as they are when it closes
//   anchor        a popover's control: a press on it is not outside
//   focus         false, an element, or a selector for where focus goes in
//   opener        where focus returns (default: what had focus at open)
//   onClose(why)  after it closed: 'escape', 'backdrop', 'outside', 'discard',
//                 'cancel', 'ok', 'close' or 'parent'
//
// It returns a handle: { el, state, open, close(why), run(fn), status(text,
// bad), markClean() }. `state` is the entry's own: it lives while the entry is
// open and is dropped when it closes (ruling 5: a chain's progress is kept by
// the sheet that made it, and only while it is open).
//
// Opening a node that is already open returns its handle and changes nothing.
export function openLayer(node, opts) {
    if (!node) return null;
    const open = entryOf(node);
    if (open) return open.handle;
    const o = opts || {};
    const kind = KINDS[o.kind] ? o.kind : 'modal';
    const k = KINDS[kind];
    wire();
    const e = {
        el: node,
        kind,
        card: o.card || (kind === 'popover' ? node : (node.firstElementChild || node)),
        reveal: o.reveal || k.reveal,
        lock: o.lock !== undefined ? !!o.lock : k.lock,
        ask: o.ask !== undefined ? !!o.ask : k.ask,
        keep: o.preserveState !== undefined ? !!o.preserveState : k.keep,
        outside: o.outside !== undefined ? !!o.outside : k.outside,
        backdrop: !!o.backdrop,
        anchor: o.anchor || null,
        opener: o.opener || document.activeElement,
        onClose: typeof o.onClose === 'function' ? o.onClose : null,
        closed: false,
        running: false,
        asking: false,
        snapshot: null,
    };
    e.handle = {
        el: node,
        state: {},
        get open() { return !e.closed; },
        close: (why) => closeEntry(e, why || 'close'),
        run: (fn) => runEntry(e, fn),
        status: (text, bad) => setStatus(e, text, bad),
        markClean: () => { e.snapshot = snapshot(node); },
    };
    stack.push(e);
    if (e.reveal === 'active') node.classList.add('active');
    else if (e.reveal === 'hidden') node.hidden = false;
    if (e.lock) lock();
    e.snapshot = snapshot(node);
    if (o.focus !== false) {
        const f = focusTarget(node, o.focus);
        if (f) f.focus();
    }
    return e.handle;
}

// closeLayer closes an entry, by its handle or its element, as a deliberate
// close does: no question asked. Anything opened from inside it closes first.
export function closeLayer(target, why) {
    const e = target && target.el ? entryOf(target.el) : entryOf(target);
    if (e) closeEntry(e, why || 'close');
}

// layerDepth is how many entries are open. A page's own Escape (clear a
// selection, say) acts only when it is 0: the layer's Escape is for the layers.
export function layerDepth() { return stack.length; }

// ─── the names the hubs export, behind the one stack ───────────────────────

// showModal opens a markup modal (.modal-overlay, shown by .active) as an
// entry. The backdrop closes it where the overlay carries data-backdrop-close
// or the caller passes closeOnBackdrop; no install call is needed for either.
// A missing id is no error, as utils.js has it.
export function showModal(id, opts) {
    const m = typeof id === 'string' ? document.getElementById(id) : id;
    if (!m) return null;
    const o = opts || {};
    return openLayer(m, {
        kind: 'modal',
        backdrop: !!o.closeOnBackdrop || m.hasAttribute('data-backdrop-close'),
        preserveState: o.preserveState,
        ask: o.ask,
        focus: o.focus,
        onClose: o.onClose,
    });
}

// hideModal is the deliberate close: it never asks. preserveState keeps the
// fields for this close; without it, what showModal was given stands. A modal
// shown some other way (its class set by hand) is still hidden and cleared,
// so every hide reads the same.
export function hideModal(id, opts) {
    const m = typeof id === 'string' ? document.getElementById(id) : id;
    if (!m) return;
    const said = opts && opts.preserveState !== undefined;
    const e = entryOf(m);
    if (e) { closeEntry(e, 'close', said ? !!opts.preserveState : undefined); return; }
    m.classList.remove('active');
    if (!(said && opts.preserveState)) resetFields(m);
}

// confirm resolves true on its OK and false on anything else that closes it:
// Cancel, Escape, the backdrop where opts.backdrop opts in, a page closing it.
// opts: ok, cancel (button words), danger (default true: the OK is
// .btn-danger, as both hubs' confirms are), backdrop.
export function confirm(message, opts) {
    return dialog(message, opts || {}, null, (yes) => yes);
}

// prompt resolves the typed text on OK or Enter, and null on anything else.
// opts: type ('number' or text), min, max, value, placeholder, ok, cancel.
export function prompt(message, opts) {
    const o = opts || {};
    const input = el('input', {
        className: 'layer-input',
        type: o.type === 'number' ? 'number' : 'text',
        value: o.value !== undefined && o.value !== null ? String(o.value) : '',
        placeholder: o.placeholder || null,
    });
    if (o.min !== undefined) input.min = String(o.min);
    if (o.max !== undefined) input.max = String(o.max);
    return dialog(message, Object.assign({ ok: 'OK', danger: false }, o), input,
        (yes) => (yes ? input.value : null));
}

// confirmWithInput is a confirm with an optional free-text field (Core's
// uiConfirmWithInput): {ok, value}, ok false on anything but its OK.
export function confirmWithInput(message, opts) {
    const o = opts || {};
    const input = el('input', { className: 'layer-input', type: 'text', placeholder: o.placeholder || null });
    return dialog(message, o, input, (yes) => ({ ok: yes, value: input.value }));
}

// openSheet is the Processes page's sheet shell, moved: a card with a title,
// a body, a status line and Cancel and OK. OK runs opts.run through the
// entry's run (one run per press, the OK disabled while it runs); Cancel
// closes without asking, because it is a deliberate click. opts: title, sub,
// body (a Node, or markup the page built and escaped), ok, cancel, danger,
// run(state, handle), backdrop, ask, focus, onClose, cardClass (the card's
// class instead of .layer-card .layer-sheet, for a page that keeps its own).
export function openSheet(opts) {
    const o = opts || {};
    const body = el('div', { className: 'layer-body' });
    if (typeof o.body === 'string') body.innerHTML = o.body;
    else if (o.body) body.appendChild(o.body);
    const status = el('span', { className: 'layer-status', 'data-layer-status': '' });
    const cancel = el('button', { className: 'btn', type: 'button' }, o.cancel || 'Cancel');
    const ok = el('button', { className: 'btn ' + (o.danger ? 'btn-danger' : 'btn-primary'), type: 'button', 'data-layer-ok': '' }, o.ok || 'OK');
    const card = el('div', {
        className: o.cardClass || 'layer-card layer-sheet',
        role: 'dialog', 'aria-modal': 'true', 'aria-label': o.title || null,
    }, [
        el('div', { className: 'layer-head' }, [el('h2', null, o.title || ''), o.sub ? el('p', null, o.sub) : null]),
        body,
        el('div', { className: 'layer-foot' }, [status, cancel, ok]),
    ]);
    const scrim = el('div', { className: 'layer-scrim' }, card);
    document.body.appendChild(scrim);
    const h = openLayer(scrim, {
        kind: 'sheet', card, reveal: 'remove', preserveState: true,
        backdrop: !!o.backdrop, ask: o.ask, focus: o.focus, onClose: o.onClose,
    });
    ok.addEventListener('click', () => { if (o.run) h.run(o.run); });
    cancel.addEventListener('click', () => h.close('cancel'));
    return h;
}

// ─── the stack ──────────────────────────────────────────────────────────────

function entryOf(node) {
    for (const e of stack) if (e.el === node) return e;
    return null;
}

function wire() {
    if (wired) return;
    wired = true;
    document.addEventListener('keydown', onKey);
    // The press is read in the capture phase, before anything under it can
    // stop it: a backdrop click counts only when the press began on the
    // backdrop too (a text selection dragged out of a field ends in a click on
    // the overlay), and a popover closes on a press outside it, which is
    // before the click that opened it could reach it.
    document.addEventListener('mousedown', onPress, true);
    document.addEventListener('touchstart', onPress, true);
    document.addEventListener('click', onClick);
}

function onKey(ev) {
    if (ev.key !== 'Escape' || ev.defaultPrevented || ev.isComposing) return;
    const top = stack[stack.length - 1];
    if (!top) return;
    ev.preventDefault();
    dismiss(top, 'escape');
}

function onPress(ev) {
    pressedOn = ev.target;
    // Every popover the press lands outside of goes, innermost first, and
    // stops at the first entry it does not close.
    for (let top = stack[stack.length - 1]; top && top.kind === 'popover' && top.outside; top = stack[stack.length - 1]) {
        if (top.el.contains(ev.target) || (top.anchor && top.anchor.contains(ev.target))) return;
        closeEntry(top, 'outside');
    }
}

function onClick(ev) {
    const down = pressedOn;
    pressedOn = null;
    const top = stack[stack.length - 1];
    if (!top || !top.backdrop) return;
    if (ev.target !== top.el || down !== top.el) return;
    dismiss(top, 'backdrop');
}

// dismiss is Escape's close: an entry with typed work asks first. The
// question is a confirm on the stack, so Escape on it is Keep editing. Its
// answer acts as it is given, not a microtask later: the next key may already
// be on its way.
function dismiss(e, why) {
    if (e.asking) return;
    if (e.ask && typed(e)) {
        e.asking = true;
        dialog('Discard the changes?', { ok: 'Discard', cancel: 'Keep editing' }, null, (yes) => {
            e.asking = false;
            if (yes) closeEntry(e, 'discard');
            return yes;
        });
        return;
    }
    closeEntry(e, why);
}

function closeEntry(e, why, keepOverride) {
    if (e.closed) return;
    for (let i = stack.length - 1; i >= 0; i--) {
        const x = stack[i];
        if (x !== e && e.el.contains(x.el)) closeEntry(x, 'parent');
    }
    e.closed = true;
    stack.splice(stack.indexOf(e), 1);
    // An entry opened from a field inside this one would hand focus back to a
    // field that is no longer on screen: it inherits this one's opener.
    for (const x of stack) if (x.opener && e.el.contains(x.opener)) x.opener = e.opener;
    const active = document.activeElement;
    const hadFocus = !active || active === document.body || e.el.contains(active);
    if (e.reveal === 'active') e.el.classList.remove('active');
    else if (e.reveal === 'hidden') e.el.hidden = true;
    else if (e.reveal === 'remove') e.el.remove();
    if (!(keepOverride !== undefined ? keepOverride : e.keep)) resetFields(e.el);
    if (e.lock) unlock();
    // A page that keeps state beside a modal listens for this (orders.js,
    // bins.js): hiding is not closing.
    if (why === 'backdrop') e.el.dispatchEvent(new CustomEvent('backdropclose', { bubbles: true }));
    e.handle.state = null;
    if (hadFocus) refocus(e);
    if (e.onClose) e.onClose(why);
}

// refocus puts focus back where it came from: the opener if it is still in
// the page, else into what is now the innermost entry.
function refocus(e) {
    if (e.opener && e.opener.isConnected && e.opener !== document.body && typeof e.opener.focus === 'function') {
        e.opener.focus();
        return;
    }
    const top = stack[stack.length - 1];
    const f = top ? focusTarget(top.el) : null;
    if (f) f.focus();
}

// ONE RUN PER PRESS (the Processes page's runSheet): a second press while a
// run is out does nothing, the entry's OK buttons ([data-layer-ok]) are
// disabled for the run, and the finally gives them back whatever the run did,
// so a write that never answers cannot leave the sheet saying "Saving…" with
// nothing left to press.
async function runEntry(e, fn) {
    if (e.closed || e.running || typeof fn !== 'function') return undefined;
    e.running = true;
    okButtons(e, true);
    try {
        return await fn(e.handle.state, e.handle);
    } finally {
        e.running = false;
        if (!e.closed) okButtons(e, false);
    }
}

function okButtons(e, off) {
    for (const b of e.el.querySelectorAll('[data-layer-ok]')) b.disabled = off;
}

// setStatus writes the entry's one line of feedback: the element marked
// data-layer-status inside it, .is-bad for a refusal.
function setStatus(e, text, bad) {
    const s = e.el.querySelector('[data-layer-status]');
    if (!s) return;
    s.textContent = text || '';
    if (bad) s.classList.add('is-bad'); else s.classList.remove('is-bad');
}

function lock() {
    if (locks++ === 0) {
        overflowBefore = document.body.style.overflow;
        document.body.style.overflow = 'hidden';
    }
}

function unlock() {
    if (locks > 0 && --locks === 0) document.body.style.overflow = overflowBefore;
}

// ─── fields ─────────────────────────────────────────────────────────────────
//
// TYPED IS WHAT CHANGED SINCE IT OPENED. The value of every field is noted
// when the entry opens; a field whose value differs now is typed work. A field
// drawn after the entry opened (a sheet that redraws part of itself) is read
// the Processes page's way instead: against the value its markup was drawn
// with (data-was, or the value / checked attribute). A field marked
// data-scratch (a search box) is never work; an element marked data-dirty
// (a picker whose selection moved) always is. A page that fills its fields
// after opening calls the handle's markClean().

function fields(root) {
    return Array.prototype.filter.call(root.querySelectorAll('input, select, textarea'), (f) =>
        ['hidden', 'button', 'submit', 'reset', 'image'].indexOf(f.type) < 0 && !f.hasAttribute('data-scratch'));
}

function valueOf(f) {
    return f.type === 'checkbox' || f.type === 'radio' ? String(f.checked) : String(f.value);
}

function drawnValue(f) {
    if (f.type === 'checkbox' || f.type === 'radio') return String(f.hasAttribute('checked'));
    if (f.tagName === 'SELECT') { const o = f.options[defaultIndex(f)]; return o ? String(o.value) : ''; }
    if (f.hasAttribute('data-was')) return f.getAttribute('data-was');
    return String(f.defaultValue || '');
}

function defaultIndex(sel) {
    const opts = sel.options;
    for (let i = 0; i < opts.length; i++) if (opts[i].hasAttribute('selected')) return i;
    return 0;
}

function snapshot(root) {
    const m = new Map();
    for (const f of fields(root)) m.set(f, valueOf(f));
    return m;
}

function typed(e) {
    if (e.el.querySelector('[data-dirty]')) return true;
    for (const f of fields(e.el)) {
        const was = e.snapshot.has(f) ? e.snapshot.get(f) : drawnValue(f);
        if (valueOf(f) !== was) return true;
    }
    return false;
}

// resetFields puts every field back to what its markup says, as a form reset
// does. A hidden input is left alone: it carries what the page put there (an
// id), not what was typed.
function resetFields(root) {
    for (const f of root.querySelectorAll('input, select, textarea')) {
        if (f.type === 'hidden') continue;
        if (f.type === 'checkbox' || f.type === 'radio') { f.checked = f.hasAttribute('checked'); continue; }
        if (f.tagName === 'SELECT') { f.selectedIndex = defaultIndex(f); continue; }
        f.value = f.defaultValue || '';
    }
}

// focusTarget is where focus goes in: the element or selector given, else
// [autofocus], else the first field, else the first button or link.
function focusTarget(root, want) {
    if (want && typeof want === 'object' && typeof want.focus === 'function') return want;
    if (typeof want === 'string') return root.querySelector(want);
    const usable = (f) => !f.disabled && f.type !== 'hidden' && f.getAttribute('tabindex') !== '-1';
    const auto = root.querySelector('[autofocus]');
    if (auto && usable(auto)) return auto;
    const field = fields(root).find(usable);
    if (field) return field;
    return Array.prototype.find.call(root.querySelectorAll('button, [href], [tabindex]'), usable) || null;
}

// ─── confirm / prompt ───────────────────────────────────────────────────────

// dialog builds one confirm or prompt as a 'dialog' entry. Every way it can
// close resolves it, once: OK as answer(true), everything else (the entry's
// onClose) as answer(false).
function dialog(message, o, input, answer) {
    return new Promise((resolve) => {
        let done = false;
        const finish = (yes) => { if (!done) { done = true; resolve(answer(yes)); } };
        const cancel = el('button', { className: 'btn', type: 'button' }, o.cancel || 'Cancel');
        const ok = el('button', {
            className: 'btn ' + (o.danger === false ? 'btn-primary' : 'btn-danger'), type: 'button', 'data-layer-ok': '',
        }, o.ok || 'Confirm');
        const card = el('div', { className: 'layer-card', role: 'alertdialog', 'aria-modal': 'true' }, [
            el('p', { className: 'layer-msg' }, message === null || message === undefined ? '' : String(message)),
            input,
            el('div', { className: 'layer-foot' }, [cancel, ok]),
        ]);
        const scrim = el('div', { className: 'layer-scrim' }, card);
        document.body.appendChild(scrim);
        const h = openLayer(scrim, {
            kind: 'dialog', card, backdrop: !!o.backdrop,
            onClose: () => finish(false),
        });
        ok.addEventListener('click', () => { finish(true); h.close('ok'); });
        cancel.addEventListener('click', () => h.close('cancel'));
        if (input) {
            input.addEventListener('keydown', (ev) => {
                if (ev.key !== 'Enter') return;
                ev.preventDefault();
                finish(true);
                h.close('ok');
            });
        }
    });
}
