// Unit tests for the node pop-up's load and Save (nodes-detail.js
// openNodeModal, loadNodeDetail, handleNodeSave). Run under plain Node via the
// Go wrapper nodes_detail_popup_save_wrapper_test.go. Exit 0 on pass, 1 on any
// assertion failure.
//
// The bug these exist to prevent: the pop-up opens on defaults and fills in the
// node's stored settings after. From 2026-10-08 the fill died on a
// ReferenceError, and Save wrote the defaults over a group's retrieve and store
// algorithms, its ASRS switch and resolve-around, unawaited, on every save. The
// rule now, as on the payloads page: the pop-up writes nothing until the detail
// for the node it shows has loaded, and then writes only what the person
// changed.

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
// A rejection the page leaves unhandled is an error it never showed anyone.
process.on('unhandledRejection', (e) => check('the page handles its own errors', false, String(e && e.message || e)));

// --- a fake DOM: just what the pop-up reads and writes ----------------------

// matches answers the few selector shapes the pop-up's queries use: .cls,
// [data-foo-bar], tag, tag.cls and tag[attr="v"].
function matches(e, sel) {
    let m;
    if ((m = /^\[data-([a-z-]+)\]$/.exec(sel))) {
        const key = m[1].replace(/-([a-z])/g, (_, c) => c.toUpperCase());
        return e.dataset[key] !== undefined;
    }
    if ((m = /^([a-z]*)\.([\w-]+)$/.exec(sel))) {
        return (!m[1] || e.tagName === m[1]) && e.classList.contains(m[2]);
    }
    if ((m = /^([a-z]+)\[(\w+)="([^"]*)"\]$/.exec(sel))) {
        return e.tagName === m[1] && String(e[m[2]]) === m[3];
    }
    return /^[a-z]+$/.test(sel) && e.tagName === sel;
}

function fakeEl(tag) {
    const classes = new Set();
    const e = {
        tagName: tag || 'div', type: '', name: '', value: '', checked: false, textContent: '',
        dataset: {}, style: {}, children: [], parent: null, submitted: 0,
        classList: {
            add(c) { classes.add(c); },
            remove(c) { classes.delete(c); },
            toggle(c, on) { if (on === undefined ? !classes.has(c) : on) classes.add(c); else classes.delete(c); },
            contains(c) { return classes.has(c); },
        },
        get className() { return Array.from(classes).join(' '); },
        set className(v) { classes.clear(); String(v).split(/\s+/).filter(Boolean).forEach((c) => classes.add(c)); },
        appendChild(c) { c.parent = e; e.children.push(c); return c; },
        remove() { if (e.parent) e.parent.children = e.parent.children.filter((c) => c !== e); },
        closest(sel) { for (let p = e; p; p = p.parent) if (matches(p, sel)) return p; return fakeEl(); },
        querySelector(sel) { return e.querySelectorAll(sel)[0] || null; },
        querySelectorAll(sel) {
            const out = [];
            (function walk(n) { n.children.forEach((c) => { if (matches(c, sel)) out.push(c); walk(c); }); })(e);
            return out;
        },
        addEventListener() {}, scrollIntoView() {}, focus() {},
        submit() { e.submitted++; },
    };
    let html = '';
    Object.defineProperty(e, 'innerHTML', {
        get() { return html; },
        set(v) {
            html = v;
            e.children = [];
            // The boxes a row is drawn with, by class, with the value each was drawn with.
            const re = /<(input|span|button)([^>]*)>/g;
            let m;
            while ((m = re.exec(v)) !== null) {
                const attr = (name) => { const a = new RegExp(' ' + name + '="([^"]*)"').exec(m[2]); return a ? a[1] : ''; };
                const c = fakeEl(m[1]);
                c.className = attr('class');
                c.value = attr('value');
                e.appendChild(c);
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
        querySelector() { return null; },
        querySelectorAll() { return []; },
    };
    doc.getElementById('page-data').dataset = { authenticated: 'true', binTypes: '[]', edges: '[]' };
    // As the template draws them: the group sections start hidden, the two
    // switches are checkboxes.
    ['ngrp-algorithms', 'nf-table-turn', 'lane-gate', 'nf-asrs-controls'].forEach((id) => doc.getElementById(id).classList.add('hide'));
    doc.getElementById('nf-asrs-enabled').type = 'checkbox';
    doc.getElementById('nf-resolve-around').type = 'checkbox';
    doc.getElementById('lane-gate').appendChild(doc.getElementById('lane-gate-list'));
    return doc;
}

// --- the network, controlled per test ---------------------------------------

function deferred() {
    let resolve, reject;
    const p = new Promise((res, rej) => { resolve = res; reject = rej; });
    return { p, resolve, reject };
}

// run evaluates one page module in its own context, its imports taken from
// lent and its exports left as the context's globals.
function run(name, globals, lent) {
    const file = path.join(__dirname, name);
    const src = fs.readFileSync(file, 'utf8')
        .replace(/^import \{([^}]*)\} from '([^']*)';$/mg, (_, names) => {
            names.split(',').map((n) => n.trim()).filter(Boolean).forEach((n) => {
                globals[n] = lent(n);
            });
            return '';
        })
        .replace(/^export /mg, '');
    const ctx = vm.createContext(globals);
    vm.runInContext(src, ctx, { filename: file });
    return ctx;
}

// load runs nodes-detail.js and the nodes-kanban.js it imports in a fresh page,
// each in its own context over one document. gets maps a URL prefix to the
// answer for that GET (the rest never answer); posts are recorded and
// answered by post(url, body), which defaults to success.
function load(gets, post) {
    // A group's kanban section answers an empty kanban unless a test says otherwise.
    gets = Object.assign({
        [KANBAN]: () => Promise.resolve({}),
        '/api/nodes/process-options': () => Promise.resolve({ processes: [] }),
    }, gets);
    const posts = [];
    const toasts = [];
    const doc = newDocument();
    const answer = (url, body) => {
        posts.push({ url, body });
        return post ? post(url, body) : Promise.resolve({});
    };
    const app = {
        escapeHtml: (s) => String(s == null ? '' : s),
        el() { return fakeEl(); },
        api() { return new Promise(() => {}); },
        delegateActions() {},
        openFromQuery() {},
        uiConfirm() { return Promise.resolve(true); },
        toast(message, level) { toasts.push({ message, level }); },
        apiGet(url) {
            for (const prefix of Object.keys(gets)) {
                if (url.indexOf(prefix) === 0) return gets[prefix](url);
            }
            return new Promise(() => {});
        },
        apiPost: answer,
    };
    const globals = () => ({
        document: doc,
        console: { log() {}, warn() {}, error() {} },
        setTimeout() {}, clearTimeout() {},
        JSON, Promise, Error, Set, Map, Array, Object, Number, String, Math,
        parseInt, parseFloat, isFinite, encodeURIComponent,
        // The kanban's confirmable posts go through fetch, not apiPost.
        fetch(url, opts) {
            return answer(url, JSON.parse(opts.body)).then(
                (res) => ({ ok: true, text: () => Promise.resolve(JSON.stringify(res || {})) }));
        },
    });
    const fromApp = (n) => app[n];
    const kanban = run('nodes-kanban.js', globals(), fromApp);
    // An older nodes-detail.js may import a name nodes-kanban.js no longer has.
    const ctx = run('nodes-detail.js', globals(), (n) => app[n] ||
        (typeof kanban[n] === 'function' ? (...a) => kanban[n](...a) : () => Promise.resolve(true)));
    return { ctx, doc, posts, toasts, form: doc.getElementById('node-form') };
}

// props are the posts that write a node property.
function props(posts) {
    return posts.filter((p) => p.url === '/api/nodes/properties/set').map((p) => p.body);
}

// group is a group's tile, as the nodes page draws it.
function group(id, name) {
    return { dataset: { id: String(id), name: name, typeCode: 'NGRP', nodeTypeId: '3', synthetic: 'true', enabled: 'true', count: '0' } };
}

// detail is /api/nodes/detail's answer for a group with these properties.
function detail(id, props) {
    return {
        node: { id: id, node_type_code: 'NGRP', parent_id: null },
        bin_type_mode: 'all', station_mode: 'all',
        stations: [], effective_stations: [], bin_types: [], effective_bin_types: [],
        properties: Object.keys(props).map((k) => ({ key: k, value: props[k] })),
    };
}

// The settings a group has stored that are none of the defaults (FIFO, LKND,
// ASRS on, resolve-around off).
const STORED = { retrieve_algorithm: 'COST', store_algorithm: 'DPTH', asrs_enabled: 'off', resolve_around: 'on' };
const DETAIL = '/api/nodes/detail';
const LANE_GATE = '/api/nodes/lane-gate-points';
const KANBAN = '/api/nodes/kanban?id=';
const FAILED = "Could not load this node's saved settings. Nothing will be saved. Close and reopen.";

const settle = async () => { for (let i = 0; i < 6; i++) await new Promise((r) => setImmediate(r)); };

function shown(doc) {
    return {
        retrieve: doc.getElementById('nf-retrieve-algo').value,
        store: doc.getElementById('nf-store-algo').value,
        asrs: doc.getElementById('nf-asrs-enabled').checked,
        resolveAround: doc.getElementById('nf-resolve-around').checked,
    };
}

// opened is a page with group 12 open and its detail answered.
async function opened(props, post) {
    const page = load({
        [DETAIL]: () => Promise.resolve(detail(12, props)),
        [LANE_GATE]: () => Promise.resolve({ group_wait_points: 'AISLE-A-WAIT', lanes: [] }),
    }, post);
    page.ctx.openNodeModal(group(12, 'SMN-A'));
    await settle();
    return page;
}

async function main() {
    console.log("opening a group shows its stored settings");
    {
        const { doc } = await opened(STORED);
        same('stored algorithms, ASRS switch and resolve-around in the form', shown(doc),
            { retrieve: 'COST', store: 'DPTH', asrs: false, resolveAround: true });
        check('ASRS off hides the algorithm controls', doc.getElementById('nf-asrs-controls').classList.contains('hide'));
    }

    console.log("opening a group draws its waiting spots");
    {
        const { doc } = await opened(STORED);
        const box = doc.getElementById('lane-gate');
        check('the section is shown', !box.classList.contains('hide'));
        const row = box.querySelector('[data-group-id]');
        check('the group row is drawn against the group', row && String(row.dataset.groupId) === '12');
        same('with its stored spots', row && row.querySelector('.group-wait-input').value, 'AISLE-A-WAIT');
    }

    console.log("Save with nothing changed writes none of the four settings");
    {
        const { ctx, posts, form } = await opened(STORED);
        await ctx.handleNodeSave(null, null);
        same('no property posts', props(posts), []);
        check('the form is submitted', form.submitted === 1, 'submitted ' + form.submitted);
    }

    console.log("a group with nothing stored shows the defaults and writes nothing");
    {
        const { ctx, doc, posts, form } = await opened({});
        same('the defaults in the form', shown(doc), { retrieve: 'FIFO', store: 'LKND', asrs: true, resolveAround: false });
        await ctx.handleNodeSave(null, null);
        same('no property posts', props(posts), []);
        check('the form is submitted', form.submitted === 1, 'submitted ' + form.submitted);
    }

    console.log("one changed setting writes that key alone, and the form waits for it");
    {
        const write = deferred();
        const { ctx, doc, posts, form } = await opened(STORED, () => write.p);
        doc.getElementById('nf-retrieve-algo').value = 'FAVL';
        const saving = ctx.handleNodeSave(null, null);
        await settle();
        same('exactly the changed key', props(posts), [{ node_id: 12, key: 'retrieve_algorithm', value: 'FAVL' }]);
        check('not submitted while the write is out', form.submitted === 0, 'submitted ' + form.submitted);
        write.resolve({});
        await saving;
        check('submitted once it answers', form.submitted === 1, 'submitted ' + form.submitted);
    }

    console.log("a write that fails stops the save");
    {
        const { ctx, doc, posts, toasts, form } = await opened(STORED, () => Promise.reject('fleet is offline'));
        doc.getElementById('nf-resolve-around').checked = false;
        await ctx.handleNodeSave(null, null);
        same('the one write was tried', posts.map((p) => p.body), [{ node_id: 12, key: 'resolve_around', value: 'off' }]);
        check('the form is not submitted', form.submitted === 0, 'submitted ' + form.submitted);
        check('the reason is shown', toasts.some((t) => t.level === 'error' && t.message.indexOf('fleet is offline') >= 0), JSON.stringify(toasts));
    }

    console.log("a detail that fails to load: Save writes nothing and says why");
    {
        const { ctx, doc, posts, toasts, form } = load({
            [DETAIL]: () => Promise.reject('502 Bad Gateway'),
        });
        ctx.openNodeModal(group(12, 'SMN-A'));
        await settle();
        same('the pop-up says so', doc.getElementById('nf-load-error').textContent, FAILED);
        await ctx.handleNodeSave(null, null);
        same('nothing posted', posts, []);
        check('the form is not submitted', form.submitted === 0, 'submitted ' + form.submitted);
        check('Save says why', toasts.some((t) => t.message === FAILED), JSON.stringify(toasts));
    }

    console.log("a fill that throws part-way counts as failed, not loaded (the 2026-10-08 shape)");
    {
        const broken = detail(12, STORED);
        broken.bin_types = {}; // not a list: the fill throws before the properties
        const { ctx, doc, posts, form } = load({ [DETAIL]: () => Promise.resolve(broken) });
        ctx.openNodeModal(group(12, 'SMN-A'));
        await settle();
        same('the pop-up says so', doc.getElementById('nf-load-error').textContent, FAILED);
        await ctx.handleNodeSave(null, null);
        check('Save writes nothing', form.submitted === 0 && posts.length === 0, 'submitted ' + form.submitted + ', posts ' + posts.length);
    }

    console.log("a Save before the detail answers writes nothing and says so");
    {
        const { ctx, posts, toasts, form } = load({ [DETAIL]: () => new Promise(() => {}) });
        ctx.openNodeModal(group(12, 'SMN-A'));
        await ctx.handleNodeSave(null, null);
        same('nothing posted', posts, []);
        check('the form is not submitted', form.submitted === 0, 'submitted ' + form.submitted);
        check('Save says why', toasts.length === 1, JSON.stringify(toasts));
    }

    console.log("a Save while the group's kanban section is still loading writes nothing");
    {
        const kanban = deferred();
        const { ctx, posts, toasts, form } = load({
            [DETAIL]: () => Promise.resolve(detail(12, STORED)),
            [KANBAN]: () => kanban.p,
        });
        ctx.openNodeModal(group(12, 'SMN-A'));
        await settle();
        await ctx.handleNodeSave(null, null);
        same('nothing posted', posts, []);
        check('the form is not submitted', form.submitted === 0, 'submitted ' + form.submitted);
        check('Save says why', toasts.length === 1, JSON.stringify(toasts));
        kanban.resolve({});
        await settle();
        await ctx.handleNodeSave(null, null);
        check('once it answers, Save goes through', form.submitted === 1, 'submitted ' + form.submitted);
        check('and saves the kanban it loaded', posts.some((p) => p.url === '/api/nodes/kanban/settings'), JSON.stringify(posts.map((p) => p.url)));
    }

    console.log("a late answer for the last node opened changes nothing in the next");
    {
        const answers = { 12: deferred(), 13: deferred() };
        const { ctx, doc, posts, form } = load({
            [DETAIL]: (url) => answers[/id=(\d+)/.exec(url)[1]].p,
        });
        ctx.openNodeModal(group(12, 'SMN-A'));
        ctx.openNodeModal(group(13, 'SMN-B'));
        answers[12].resolve(detail(12, STORED));
        await settle();
        same("12's settings are not in 13's form", shown(doc), { retrieve: 'FIFO', store: 'LKND', asrs: true, resolveAround: false });
        await ctx.handleNodeSave(null, null);
        check("12's answer does not count as 13's load", form.submitted === 0 && posts.length === 0, 'submitted ' + form.submitted + ', posts ' + posts.length);

    }

    console.log("a late failure for the last node opened does not block the next");
    {
        const answers = { 12: deferred(), 13: deferred() };
        const { ctx, doc, posts, form } = load({
            [DETAIL]: (url) => answers[/id=(\d+)/.exec(url)[1]].p,
        });
        ctx.openNodeModal(group(12, 'SMN-A'));
        ctx.openNodeModal(group(13, 'SMN-B'));
        answers[13].resolve(detail(13, {}));
        await settle();
        answers[12].reject('timeout');
        await settle();
        same("12's failure is not shown in 13's pop-up", doc.getElementById('nf-load-error').textContent, '');
        await ctx.handleNodeSave(null, null);
        check('13 saves, and writes none of its settings', form.submitted === 1 && props(posts).length === 0,
            'submitted ' + form.submitted + ', ' + JSON.stringify(props(posts)));
    }

    if (failures) {
        console.log(failures + ' failure(s)');
        process.exit(1);
    }
    console.log('all passed');
}

main().catch((e) => { console.log('FAIL harness: ' + (e && e.stack || e)); process.exit(1); });
