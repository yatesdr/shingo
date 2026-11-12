// nodes.escape-layer.test.js — Escape on a real Core page belongs to the one
// overlay stack (shared/layer.js). Run by TestNodesEscapeLayerJS
// (nodes_escape_layer_test.go). Exit 0 on pass, 1 on any failure.
//
// The Nodes page runs unmodified: nodes-supermarket.js (and what it imports:
// nodes-detail.js, nodes-kanban.js, app.js, the shared modules) loads in the
// helper pins' realm (shared/testdata/helper-pins-harness/world.js), one tab
// with its own DOM, events and module instances, the layer included. Its
// New node group modal is the template's markup, opened by its own button.
//
// What it holds, on that page:
//   - Escape closes the innermost entry first: a confirm opened over the modal
//     goes, and the modal stays, until the next Escape.
//   - Escape asks before it discards what was typed; Keep editing keeps it;
//     Discard closes and the field goes back to its markup.
// The page used to close its modals from its own document Escape handler,
// which ran before the layer's: one Escape shut the modal and the confirm
// over it, and typed work went without a question.

'use strict';

const path = require('path');

const REPO = path.join(__dirname, '..', '..', '..', '..', '..');
const { makeRealm, Event } = require(path.join(REPO, 'shared', 'testdata', 'helper-pins-harness', 'world.js'));

let failed = 0;
let passed = 0;
function check(name, ok, detail) {
    if (ok) { passed++; console.log('ok  ' + name); }
    else { failed++; console.log('FAIL ' + name + (detail ? '\n     ' + detail : '')); }
}

function page() {
    const r = makeRealm({ surface: 'core' });
    const d = r.doc;
    const mk = (tag, attrs, kids) => {
        const e = d.createElement(tag);
        Object.keys(attrs || {}).forEach((k) => e.setAttribute(k, attrs[k]));
        (kids || []).forEach((k) => e.appendChild(typeof k === 'string' ? d.createTextNode(k) : k));
        return e;
    };
    d.body.appendChild(mk('div', { id: 'page-data', 'data-authenticated': 'true', 'data-bin-types': '[]', 'data-edges': '[]' }));
    const opener = mk('button', { type: 'button', id: 'ngrp-open', 'data-action': 'openNgrpModal' }, ['+ Node group']);
    // As nodes.html draws it (#ngrp-modal, data-backdrop-close).
    const modal = mk('div', { id: 'ngrp-modal', class: 'modal-overlay', 'data-backdrop-close': '' }, [
        mk('div', { class: 'modal' }, [
            mk('input', { type: 'text', id: 'ngrp-name', class: 'form-input' }),
            mk('div', { id: 'ngrp-result' }),
            mk('button', { type: 'button', class: 'btn', 'data-action': 'closeNgrpModal' }, ['Cancel']),
        ]),
    ]);
    // The Add lane modal beside it, as nodes.html draws it (no backdrop close).
    const lane = mk('div', { id: 'lane-modal', class: 'modal-overlay' }, [
        mk('div', { class: 'modal' }, [
            mk('input', { type: 'text', id: 'lane-name', class: 'form-input' }),
            mk('div', { id: 'lane-name-error', class: 'form-error' }),
            mk('div', { id: 'lane-form-error', class: 'form-error' }),
        ]),
    ]);
    d.body.appendChild(opener);
    d.body.appendChild(modal);
    d.body.appendChild(lane);
    r.load('/static/pages/nodes-supermarket.js');
    const layer = r.load('/static/shared/layer.js');
    const ev = (type, extra) => new Event(type, Object.assign({ bubbles: true, cancelable: true }, extra || {}));
    return {
        r, d, layer, modal,
        open() { opener.focus(); opener.dispatchEvent(ev('mousedown')); opener.dispatchEvent(ev('click')); },
        escape() { (d.activeElement || d.body).dispatchEvent(ev('keydown', { key: 'Escape' })); },
        isOpen: () => modal.classList.contains('active'),
        name: () => d.getElementById('ngrp-name'),
        question: () => d.querySelectorAll('.layer-scrim').find((s) => /Discard the changes\?/.test(s.textContent)) || null,
        press(scrim, words) {
            const b = scrim.querySelectorAll('button').find((x) => x.textContent === words);
            b.dispatchEvent(ev('mousedown')); b.dispatchEvent(ev('click'));
        },
    };
}

function watch(p) {
    const w = { state: 'pending', value: undefined };
    p.then((v) => { w.state = 'resolved'; w.value = v; });
    return w;
}

(async () => {
    // Escape closes the innermost entry first.
    {
        const p = page();
        p.open();
        check('the modal opens from its own button', p.isOpen());
        const w = watch(p.layer.confirm('Delete GRP-1?'));
        await p.r.advance(0);
        p.escape();
        await p.r.advance(0);
        check('Escape over the modal: the confirm resolves false', w.state === 'resolved' && w.value === false, JSON.stringify(w));
        check('Escape over the modal: the modal stays open', p.isOpen());
        p.escape();
        await p.r.advance(0);
        check('the next Escape closes the modal', !p.isOpen());
    }

    // Escape asks before it discards typed work.
    {
        const p = page();
        p.open();
        p.name().value = 'GRP-9';
        p.escape();
        await p.r.advance(0);
        const q = p.question();
        check('typed, Escape: the modal stays open', p.isOpen());
        check('typed, Escape: "Discard the changes?" is asked', !!q);
        if (q) {
            p.press(q, 'Keep editing');
            await p.r.advance(0);
            check('Keep editing: the modal and what was typed stay', p.isOpen() && p.name().value === 'GRP-9', p.name().value);
            p.escape();
            await p.r.advance(0);
            const q2 = p.question();
            check('typed, Escape again: asked again', !!q2);
            if (q2) {
                p.press(q2, 'Discard');
                await p.r.advance(0);
                check('Discard: the modal closes', !p.isOpen());
                check('Discard: the field goes back to its markup', p.name().value === '', p.name().value);
            }
        }
    }

    // Untouched, Escape closes without a question.
    {
        const p = page();
        p.open();
        p.escape();
        await p.r.advance(0);
        check('untouched, Escape: closed, nothing asked', !p.isOpen() && !p.question());
    }

    console.log((failed ? 'FAILED: ' + failed + ' of ' : 'PASS: ') + (passed + failed) + ' Escape cases on the Nodes page');
    process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e && e.stack || e); process.exit(1); });
