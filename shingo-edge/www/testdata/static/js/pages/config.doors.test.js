// config.doors.test.js — what the Edge Configuration page (static/js/pages/
// config.js) says when its own buttons' requests are answered: a refusal, no
// answer, and a lapsed session. Run by TestConfigPageDoorsJS
// (config_page_doors_test.go). Exit 0 on pass, 1 on any failure.
//
// The page runs unmodified in the helper pins' realm (shared/testdata/
// helper-pins-harness/world.js): one tab with its own DOM, fake clock, fetch
// and module instances, the Edge hub, the layer and the notice included. The
// DOM is the elements of templates/config.html the page reads at load and the
// ones these buttons write to, with the template's ids.
//
// A LAPSED SESSION IS NOT A SAVE. The Edge's admin routes answer a caller
// with no session with a 303 to /login (router.go), fetch follows it, and the
// page gets the login page, 200 text/html. utils.js's apiResult read that as
// ok, so Back up now said "Backed up." while nothing was backed up. The HTTP
// door (shared/http.js) reads it as {ok:false, status:401}, puts "Signed out"
// with its Sign-in link on the notice and says so (shown: true); the page's
// own line then says only that it failed.

'use strict';

const path = require('path');

const REPO = path.join(__dirname, '..', '..', '..', '..', '..', '..');
const { makeRealm, Event, Element } = require(path.join(REPO, 'shared', 'testdata', 'helper-pins-harness', 'world.js'));

// The page writes its live words through lastElementChild and empties the
// backup list with replaceChildren, which the realm's DOM does not carry.
if (!('lastElementChild' in Element.prototype)) {
    Object.defineProperty(Element.prototype, 'lastElementChild', {
        configurable: true,
        get() { const c = this.children; return c[c.length - 1] || null; },
    });
}
if (!Element.prototype.replaceChildren) {
    Element.prototype.replaceChildren = function (...kids) {
        this.textContent = '';
        kids.forEach((k) => this.appendChild(k));
    };
}

let failed = 0;
let passed = 0;
function check(name, ok, detail) {
    if (ok) { passed++; console.log('ok  ' + name); }
    else { failed++; console.log('FAIL ' + name + (detail ? '\n     ' + detail : '')); }
}

const LOGIN = { status: 200, body: '<!DOCTYPE html><html><body><form action="/login"></form></body></html>',
    redirected: true, url: 'http://edge.test/login?next=%2Fconfig', headers: { 'content-type': 'text/html; charset=utf-8' } };
const REFUSED = { status: 502, body: '{"error":"storage unreachable"}\n', headers: { 'content-type': 'application/json' } };
const OK = { status: 200, body: '{"status":"ok"}\n', headers: { 'content-type': 'application/json' } };

function page(answer) {
    const r = makeRealm({ surface: 'edge' });
    const d = r.doc;
    const mk = (tag, attrs, kids) => {
        const e = d.createElement(tag);
        Object.keys(attrs || {}).forEach((k) => e.setAttribute(k, attrs[k]));
        (kids || []).forEach((k) => e.appendChild(typeof k === 'string' ? d.createTextNode(k) : k));
        return e;
    };
    const cnt = (id) => mk('span', { id, class: 'set-cnt' }, [mk('i'), mk('span')]);
    const root = mk('div', { id: 'config', 'data-station-id': 'EDGE-1',
        // Storage configured, so the backup buttons and list are on.
        'data-backup-status': '{"available":true,"configured":true}' }, [
        mk('div', { class: 'set-fld' }, [mk('input', { 'data-field': 'bucket', value: 'edge-backups' })]),
        mk('div', { class: 'set-fld' }, [mk('input', { 'data-field': 'endpoint', value: 'https://s3.test' })]),
        cnt('core-sync'), cnt('plc-live'), cnt('shift-live'), cnt('backup-live'),
        mk('span', { id: 'shift-add-hint' }), mk('button', { id: 'shift-add-btn', type: 'button' }),
        mk('small', { id: 'storage-sub' }), mk('button', { id: 'storage-toggle', type: 'button' }),
        mk('div', { class: 'set-fld' }, [mk('input', { 'data-field': 'secret_key', type: 'password', placeholder: 'saved' })]),
        mk('button', { id: 'secret-remove', type: 'button' }),
        mk('button', { type: 'button', 'data-action': 'testBackup' }, ['Test connection']),
        mk('button', { type: 'button', 'data-action': 'runBackup' }, ['Back up now']),
        mk('div', { id: 'backup-op', class: 'set-note' }),
        mk('button', { type: 'button', 'data-action': 'listBackups' }, ['Show backups']),
        mk('span', { id: 'backup-list-status' }), mk('div', { id: 'backup-list' }),
    ]);
    const pwModal = mk('div', { id: 'password-modal', class: 'modal-overlay' }, [mk('div', { class: 'modal' }, [
        mk('input', { type: 'password', 'data-pw': 'old' }), mk('input', { type: 'password', 'data-pw': 'new' }),
        mk('input', { type: 'password', 'data-pw': 'again' }), mk('div', { id: 'pw-refusal' }),
        mk('button', { type: 'button', 'data-action': 'changePassword' }, ['Change']),
    ])]);
    d.body.appendChild(root);
    d.body.appendChild(pwModal);
    const sent = [];
    r.answer = (url, init) => { sent.push((init && init.method || 'GET') + ' ' + url); return answer(url); };
    r.load('/static/js/pages/config.js');
    const click = (sel) => {
        const b = d.querySelector(sel);
        b.dispatchEvent(new Event('mousedown', { bubbles: true, cancelable: true }));
        b.dispatchEvent(new Event('click', { bubbles: true, cancelable: true }));
    };
    return {
        r, d, sent, click,
        text: (id) => d.getElementById(id).textContent,
        notices: () => d.querySelectorAll('.notice-toast').map((t) => t.textContent + (t.classList.contains('is-sticky') ? ' (sticky)' : '')),
    };
}

(async () => {
    // Back up now.
    for (const [label, ans, want] of [
        ['a lapsed session', LOGIN, 'Backup failed.'],
        ['a refusal', REFUSED, 'Backup failed: storage unreachable'],
        ['no answer', null, 'Backup failed: No answer from the server'],
        ['a backup made', OK, 'Backed up.'],
    ]) {
        const p = page(() => (ans === null ? { reject: 'Failed to fetch' } : ans));
        p.click('[data-action="runBackup"]');
        await p.r.advance(0);
        check('Back up now, ' + label + ': ' + JSON.stringify(want), p.text('backup-op') === want, JSON.stringify(p.text('backup-op')));
        if (ans === LOGIN) {
            check('Back up now, a lapsed session: "Signed out" with Sign in on the notice, once',
                JSON.stringify(p.notices()) === JSON.stringify(['Signed outSign in (sticky)']), JSON.stringify(p.notices()));
        } else {
            check('Back up now, ' + label + ': nothing on the notice (the line says it)', p.notices().length === 0, JSON.stringify(p.notices()));
        }
    }

    // Test connection, Show backups, Change password: a lapsed session is not done.
    {
        const p = page(() => LOGIN);
        p.click('[data-action="testBackup"]');
        await p.r.advance(0);
        check('Test connection, a lapsed session: failed, not "The storage answered."', p.text('backup-op') === 'Storage test failed.', JSON.stringify(p.text('backup-op')));
    }
    {
        const p = page(() => LOGIN);
        p.click('[data-action="listBackups"]');
        await p.r.advance(0);
        check('Show backups, a lapsed session: not "none in storage"', p.text('backup-list-status') === 'Could not list backups.', JSON.stringify(p.text('backup-list-status')));
    }
    {
        const p = page(() => LOGIN);
        p.d.querySelector('[data-pw="old"]').value = 'a';
        p.d.querySelector('[data-pw="new"]').value = 'b';
        p.d.querySelector('[data-pw="again"]').value = 'b';
        p.click('[data-action="changePassword"]');
        await p.r.advance(0);
        check('Change password, a lapsed session: "Not changed."', p.text('pw-refusal') === 'Not changed.', JSON.stringify(p.text('pw-refusal')));
        check('Change password, a lapsed session: no "Password changed"', !p.notices().some((n) => /Password changed/.test(n)), JSON.stringify(p.notices()));
    }
    {
        const p = page(() => REFUSED);
        p.d.querySelector('[data-pw="old"]').value = 'a';
        p.d.querySelector('[data-pw="new"]').value = 'b';
        p.d.querySelector('[data-pw="again"]').value = 'b';
        p.click('[data-action="changePassword"]');
        await p.r.advance(0);
        check('Change password, a refusal: the server\'s sentence', p.text('pw-refusal') === 'Not changed: storage unreachable', JSON.stringify(p.text('pw-refusal')));
    }

    console.log((failed ? 'FAILED: ' + failed + ' of ' : 'PASS: ') + (passed + failed) + ' Edge config door cases');
    process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e && e.stack || e); process.exit(1); });
