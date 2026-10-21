// Unit tests for the payloads page's edit save (payloads.js submitPLEdit). Run
// under plain Node via the Go wrapper payloads_edit_save_wrapper_test.go. Exit
// 0 on pass, 1 on any assertion failure.
//
// The bug these exist to prevent: the edit form loads a payload's carrier
// types and manifest after it opens, and a save sent before those loads
// finished, or after one failed, posted [] for both, which the server took as
// "replace both with nothing". The page sends a list only once it has loaded
// it; a list it never loaded is left out, and the server leaves the stored one
// standing.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let failures = 0;
function check(name, cond, detail) {
    if (cond) {
        console.log('  ok  ' + name);
    } else {
        failures++;
        console.log('  FAIL ' + name + (detail ? ' — ' + detail : ''));
    }
}
function same(name, got, want) {
    const g = JSON.stringify(got), w = JSON.stringify(want);
    check(name, g === w, 'got ' + g + ', want ' + w);
}

// --- a fake DOM: just what the edit modal reads and writes ------------------

function fakeEl(tag) {
    const e = {
        tagName: tag || 'div', value: '', checked: false, readOnly: false, textContent: '', className: '',
        dataset: {}, style: {}, children: [], listeners: {},
        classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
        addEventListener(type, fn) { (this.listeners[type] = this.listeners[type] || []).push(fn); },
        appendChild(c) { this.children.push(c); return c; },
        querySelector(sel) { return this.querySelectorAll(sel)[0] || null; },
        querySelectorAll(sel) {
            if (sel === 'input[type=checkbox]') return this.children.filter((c) => c.type === 'checkbox');
            if (sel.charAt(0) === '.') return this.children.filter((c) => c.className.split(' ').indexOf(sel.slice(1)) >= 0);
            return [];
        },
    };
    let html = '';
    Object.defineProperty(e, 'innerHTML', {
        get() { return html; },
        set(v) {
            html = v;
            e.children = [];
            // A manifest row's boxes, by class, with the value each was drawn with.
            const re = /<(input|span|button)([^>]*)>/g;
            let m;
            while ((m = re.exec(v)) !== null) {
                const attr = (name) => { const a = new RegExp(name + '="([^"]*)"').exec(m[2]); return a ? a[1] : ''; };
                const c = fakeEl(m[1]);
                c.className = attr('class');
                c.value = attr('value');
                e.children.push(c);
            }
        },
    });
    return e;
}

function newDocument() {
    const byId = {};
    const doc = {
        body: fakeEl('body'),
        getElementById(id) { return (byId[id] = byId[id] || Object.assign(fakeEl(), { id })); },
        createElement(tag) { return fakeEl(tag); },
        addEventListener() {},
        querySelector(sel) { return this.querySelectorAll(sel)[0] || null; },
        querySelectorAll(sel) {
            const m = /^#(\S+) (.*)$/.exec(sel);
            if (!m) return [];
            const host = this.getElementById(m[1]);
            const rest = m[2];
            if (rest === '.plpart-row') return host.querySelectorAll('.plpart-row');
            if (rest === '.plpart-row .mr-part') {
                return host.querySelectorAll('.plpart-row').map((r) => r.querySelector('.mr-part')).filter(Boolean);
            }
            if (rest === 'input[type=checkbox]:checked') return host.querySelectorAll('input[type=checkbox]').filter((c) => c.checked);
            return [];
        },
    };
    return doc;
}

// --- fetch, controlled per test ---------------------------------------------

// A deferred answer for a GET the test settles by hand.
function deferred() {
    let resolve, reject;
    const p = new Promise((res, rej) => { resolve = res; reject = rej; });
    return { p, resolve, reject };
}
function okJSON(data) { return { ok: true, status: 200, json: () => Promise.resolve(data), text: () => Promise.resolve(JSON.stringify(data)) }; }

function load(gets) {
    let src = fs.readFileSync(path.join(__dirname, 'payloads.js'), 'utf8');
    src = src.replace(/^import .*$/m, '');
    const posts = [];
    const doc = newDocument();
    // The edit form's carrier checkboxes, as the template draws them.
    const host = doc.getElementById('ple-bin-types');
    for (const id of [1, 2, 3]) {
        const cb = fakeEl('input');
        cb.type = 'checkbox';
        cb.value = String(id);
        cb.dataset = { code: 'TYPE-' + id };
        host.children.push(cb);
    }
    const ctx = {
        document: doc,
        console: { log() {}, warn() {}, error() {} },
        location: { href: '' },
        window: { location: { reload() {} } },
        setTimeout() {},
        toast() {},
        api() {},
        el() { return fakeEl(); },
        escapeHtml: (s) => String(s == null ? '' : s),
        hideModal() {},
        showModal() {},
        removeParentElement() {},
        delegateActions() {},
        encodeURIComponent,
        JSON, Promise, Error, parseInt, isNaN,
        fetch(url, opts) {
            if (opts && opts.method === 'POST') {
                posts.push({ url, body: JSON.parse(opts.body) });
                return new Promise(() => {});
            }
            for (const prefix of Object.keys(gets)) {
                if (url.indexOf(prefix) === 0) return gets[prefix]();
            }
            return new Promise(() => {}); // robot groups, sequences, part lookups: never answer
        },
    };
    vm.createContext(ctx);
    vm.runInContext(src, ctx);
    return { ctx, posts, doc };
}

function editButton(id) {
    return { dataset: { id: String(id), code: 'PART-A', uop: '40', notes: '', toteLayers: '', uopPerTote: '' } };
}

const settle = () => new Promise((r) => setImmediate(r));
const MANIFEST = '/api/payloads/templates/manifest';
const BIN_TYPES = '/api/payloads/templates/bin-types';

async function main() {
    console.log('a save before the lists load sends neither list');
    {
        const m = deferred(), b = deferred();
        const { ctx, posts } = load({ [MANIFEST]: () => m.p, [BIN_TYPES]: () => b.p });
        ctx.openEditPayloadModal(editButton(7));
        ctx.submitPLEdit(null, null);
        check('one save posted', posts.length === 1, 'posts ' + posts.length);
        const body = (posts[0] || {}).body || {};
        check('no manifest key', !('manifest' in body), JSON.stringify(body.manifest));
        check('no bin_type_ids key', !('bin_type_ids' in body), JSON.stringify(body.bin_type_ids));
        same('the row still rides', [body.id, body.code], [7, 'PART-A']);
    }

    console.log('a save after the lists failed to load sends neither list');
    {
        const { ctx, posts } = load({
            [MANIFEST]: () => Promise.resolve({ ok: false, status: 500, json: () => Promise.resolve({}) }),
            [BIN_TYPES]: () => Promise.reject(new Error('offline')),
        });
        ctx.openEditPayloadModal(editButton(7));
        await settle(); await settle();
        ctx.submitPLEdit(null, null);
        const body = (posts[0] || {}).body || {};
        check('no manifest key', !('manifest' in body), JSON.stringify(body.manifest));
        check('no bin_type_ids key', !('bin_type_ids' in body), JSON.stringify(body.bin_type_ids));
    }

    console.log('a save after the lists loaded sends both, as loaded');
    {
        const { ctx, posts } = load({
            [MANIFEST]: () => Promise.resolve(okJSON([{ part_number: 'PART-A', catid: '70000001', parts_per_cycle: 2 }])),
            // The answer is the carrier types themselves, not their ids.
            [BIN_TYPES]: () => Promise.resolve(okJSON([{ id: 2, code: 'TYPE-2' }])),
        });
        ctx.openEditPayloadModal(editButton(7));
        await settle(); await settle();
        ctx.submitPLEdit(null, null);
        const body = (posts[0] || {}).body || {};
        same('manifest', body.manifest, [{ part_number: 'PART-A', catid: '70000001', parts_per_cycle: 2, description: '' }]);
        same('bin_type_ids: the stored type is checked', body.bin_type_ids, [2]);
    }

    console.log('a payload with no lines and no types loaded sends both, empty');
    {
        const { ctx, posts } = load({
            [MANIFEST]: () => Promise.resolve(okJSON([])),
            [BIN_TYPES]: () => Promise.resolve(okJSON([])),
        });
        ctx.openEditPayloadModal(editButton(7));
        await settle(); await settle();
        ctx.submitPLEdit(null, null);
        const body = (posts[0] || {}).body || {};
        same('manifest', body.manifest, []);
        same('bin_type_ids', body.bin_type_ids, []);
    }

    console.log("a late answer for the last payload opened does not fill the next one's form");
    {
        const first = { m: deferred(), b: deferred() };
        let opened = 0;
        const { ctx, posts } = load({
            [MANIFEST]: () => (opened === 1 ? first.m.p : new Promise(() => {})),
            [BIN_TYPES]: () => (opened === 1 ? first.b.p : new Promise(() => {})),
        });
        opened = 1;
        ctx.openEditPayloadModal(editButton(7));
        opened = 2;
        ctx.openEditPayloadModal(editButton(8));
        first.m.resolve(okJSON([{ part_number: 'PART-A', catid: '', parts_per_cycle: 1 }]));
        first.b.resolve(okJSON([{ id: 1, code: 'TYPE-1' }]));
        await settle(); await settle();
        ctx.submitPLEdit(null, null);
        const body = (posts[0] || {}).body || {};
        check('payload 8 sends no manifest', !('manifest' in body), JSON.stringify(body.manifest));
        check('payload 8 sends no bin_type_ids', !('bin_type_ids' in body), JSON.stringify(body.bin_type_ids));
    }

    if (failures) {
        console.log(failures + ' failure(s)');
        process.exit(1);
    }
    console.log('all passed');
}

main().catch((e) => { console.log('FAIL harness: ' + (e && e.stack || e)); process.exit(1); });
