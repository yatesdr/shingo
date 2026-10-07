// config-backups.test.js — E5 pinned through delegateActions: a click on a
// listed backup's Restore button posts exactly the listed key. Run under node
// by config_backups_js_test.go.
//
// Before U3 the button was built as data-action="stageRestore:"<key>")", so
// the handler got the key wrapped in quotes with a stray bracket and posted a
// key matching no backup. The key now rides in data-key.
//
// Loads the real shared/utils.js (el, delegateActions) and config-backups.js
// into one VM with a small DOM stub; nothing else.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let passed = 0;
let failed = 0;
function eq(got, want, label) {
    if (got === want) { passed++; return; }
    failed++;
    console.error('FAIL: ' + label + ': got ' + JSON.stringify(got) + ', want ' + JSON.stringify(want));
}

class Node {
    constructor(tag, text) {
        this.tagName = (tag || '#text').toUpperCase();
        this.children = [];
        this.parentNode = null;
        this.attrs = {};
        this.dataset = {};
        this.className = '';
        this._text = text === undefined ? '' : String(text);
        this._listeners = {};
    }
    get textContent() {
        if (this.tagName === '#TEXT') return this._text;
        return this.children.map((c) => c.textContent).join('');
    }
    setAttribute(k, v) { this.attrs[k] = String(v); }
    appendChild(c) { c.parentNode = this; this.children.push(c); return c; }
    contains(o) { let cur = o; while (cur) { if (cur === this) return true; cur = cur.parentNode; } return false; }
    addEventListener(ev, fn) { (this._listeners[ev] = this._listeners[ev] || []).push(fn); }
    closest(sel) {
        const m = /^\[data-([\w-]+)\]$/.exec(sel);
        if (!m) throw new Error('stub selector not supported: ' + sel);
        const key = m[1].replace(/-([a-z])/g, (_, c) => c.toUpperCase());
        let cur = this;
        while (cur) { if (cur.dataset && cur.dataset[key]) return cur; cur = cur.parentNode; }
        return null;
    }
    find(pred) {
        if (pred(this)) return this;
        for (const c of this.children) { const f = c.find(pred); if (f) return f; }
        return null;
    }
}

function load(file, exportsList) {
    const src = fs.readFileSync(file, 'utf8')
        .replace(/export\s+async\s+function\s+/g, 'async function ')
        .replace(/export\s+function\s+/g, 'function ')
        .replace(/export\s+const\s+/g, 'const ');
    return src + '\n' + exportsList.map((n) => 'this.' + n + ' = ' + n + ';').join(' ');
}

const ctx = vm.createContext({
    Node,
    document: {
        createElement: (t) => new Node(t),
        createTextNode: (t) => new Node('#text', t),
        getElementById: () => null,
        addEventListener: () => {},
        querySelectorAll: () => [],
        body: new Node('body'),
    },
    window: { addEventListener: () => {} },
    console, setTimeout, clearTimeout, Promise,
    fetch: () => Promise.reject(new Error('no fetch in this test')),
    EventSource: function () {},
    location: { reload: () => {} },
});
vm.runInContext(load(path.join(__dirname, '..', '..', '..', '..', '..', 'shared', 'utils.js'), ['el', 'delegateActions']), ctx);
vm.runInContext(load(path.join(__dirname, 'config-backups.js'), ['backupRow', 'makeStageRestore', 'backupWords', 'formatBytes', 'stationBody', 'stationErrors']), ctx);

const tick = () => new Promise((r) => setTimeout(r, 0));

(async () => {
    // Keys with the characters the old string-built action mangled: quotes,
    // a bracket, a colon (delegateActions splits verb:args on it), spaces.
    const keys = [
        'edge/stn-0001/2026-10-07T10:00:00Z.tar.gz',
        'edge/stn-0001/"quoted") odd:key.tar.gz',
        "edge/stn-0001/it's & <b>.tar.gz",
    ];
    for (const key of keys) {
        const posts = [];
        const said = [];
        const handler = ctx.makeStageRestore({
            stationID: 'stn-0001',
            prompt: async (_msg, opts) => opts.value, // OK on the pre-filled ID, as an operator would
            post: async (url, body) => { posts.push({ url, body }); return { ok: true, body: { status: 'ok' } }; },
            done: (msg, ok) => said.push({ msg, ok }),
        });
        const root = new Node('div');
        ctx.delegateActions(root, { stageRestore: handler });
        const row = ctx.backupRow({ key, size: 2048, created_at: '2026-10-07T10:00:00Z' }, ctx.el, () => 'Oct 7 10:00');
        root.appendChild(row);
        const btn = row.find((n) => n.dataset && n.dataset.action === 'stageRestore');
        eq(!!btn, true, 'restore button present for ' + key);
        eq(btn.dataset.key, key, 'data-key carries the key as listed');
        (root._listeners.click || []).forEach((fn) => fn({ target: btn }));
        await tick();
        await tick();
        eq(posts.length, 1, 'one POST for ' + key);
        eq(posts[0] && posts[0].url, '/api/backups/restore', 'restore door');
        eq(posts[0] && posts[0].body.key, key, 'POST body key equals the listed key');
        eq(said.length === 1 && said[0].ok, true, 'staged message');
    }

    // A wrong station ID typed: nothing is posted.
    {
        const posts = [];
        const handler = ctx.makeStageRestore({
            stationID: 'stn-0001',
            prompt: async () => 'stn-9999',
            post: async (url, body) => { posts.push(body); return { ok: true }; },
            done: () => {},
        });
        const root = new Node('div');
        ctx.delegateActions(root, { stageRestore: handler });
        const row = ctx.backupRow({ key: 'k1', size: 1 }, ctx.el, () => '');
        root.appendChild(row);
        const btn = row.find((n) => n.dataset && n.dataset.action === 'stageRestore');
        (root._listeners.click || []).forEach((fn) => fn({ target: btn }));
        await tick();
        await tick();
        eq(posts.length, 0, 'mismatched station ID posts nothing');
    }

    // A restore already staged shows no button.
    {
        const row = ctx.backupRow({ key: 'k2', restore_pending: true }, ctx.el, () => '');
        eq(row.find((n) => n.dataset && n.dataset.action === 'stageRestore'), null, 'pending row has no Restore');
    }

    // The Backups title's words.
    const clock = (ts) => ts.slice(11, 16);
    eq(ctx.backupWords({ available: true, configured: false, enabled: false }, clock).text,
        'off · no storage set up', 'words: no storage');
    eq(ctx.backupWords({ available: true, configured: true, enabled: false }, clock).text,
        'off · no backup yet', 'words: off, none yet');
    const auto = ctx.backupWords({ available: true, configured: true, enabled: true,
        last_success_at: '2026-10-07T14:05:00Z', next_scheduled_at: '2026-10-07T15:05:00Z' }, clock);
    eq(auto.text, 'automatic · last 14:05 · next 15:05', 'words: automatic');
    eq(auto.cls, 'ok', 'words: automatic is ok');
    eq(ctx.backupWords({ available: true, configured: true, enabled: true, stale: true }, clock).cls,
        'warn', 'words: stale warns');
    eq(ctx.backupWords({ available: false }, clock).text, 'unavailable', 'words: no service');
    eq(ctx.formatBytes(1536), '1.5 KB', 'formatBytes');

    // The Station body on a legacy-only Edge carries the legacy id unchanged,
    // so a timezone save is not refused for want of a UID (lead ruling on E1).
    {
        const d = { station_uid: '', timezone: 'America/Chicago', auto_confirm: true };
        eq(ctx.stationErrors(d, 'plant-a.line-1'), null, 'legacy-only: no client error');
        const b = ctx.stationBody(d, 'plant-a.line-1');
        eq(b.station_id, 'plant-a.line-1', 'legacy-only: body carries station_id unchanged');
        eq(b.station_uid, '', 'legacy-only: no UID invented');
        eq(b.timezone, 'America/Chicago', 'legacy-only: timezone sent');
        eq(Object.prototype.hasOwnProperty.call(ctx.stationBody({ station_uid: 'stn-1', timezone: '', auto_confirm: false }, ''), 'station_id'),
            false, 'UID Edge: no station_id in the body');
        eq(ctx.stationErrors({ station_uid: '' }, '') !== null, true, 'neither id: client error (E1)');
    }

    console.log(passed + ' passed, ' + failed + ' failed');
    process.exit(failed ? 1 : 0);
})();
