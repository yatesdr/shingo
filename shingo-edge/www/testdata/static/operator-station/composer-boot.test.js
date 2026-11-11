// composer-boot.test.js — the station's deferred loads: the composer's assets
// (ensureComposer) and the composer model (ensureComposerModel), the one
// loader the flow panel and the composer both wait on.
//
// composer-boot.js runs here whole, in a vm with a fake document: every tag
// it appends is kept, and the test fires each one's load or error itself, so
// what was requested, in what order, and what a failed request leaves behind
// are all visible. Its `import()` of composer-render.js is the one line
// rewritten (vm has no module loader): it records the URL and does what the
// module's evaluation does, which is to set window.ComposerUI.
//
// Runs under plain Node (no npm). Exit 0 = pass. Run by composer_boot_test.go.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

// The page code under test. This file lives in www/testdata, outside the
// embedded static tree, so no binary carries or serves it
// (TestStaticTree_HoldsNoTestJS); the code it reads stays in www/static.
const SRC_DIR = path.join(__dirname, '..', '..', '..', 'static', 'operator-station');

let failures = 0;
let finished = false;
process.on('exit', function (code) {
    if (!finished && code === 0) { console.error('FAIL: the test did not reach its end'); process.exitCode = 1; }
});
function check(name, cond, detail) {
    if (cond) { console.log('  ok   ' + name); return; }
    failures++;
    console.log('  FAIL ' + name + (detail ? '\n       ' + detail : ''));
}

function flush() {
    let p = Promise.resolve();
    for (let i = 0; i < 6; i++) p = p.then(() => new Promise(r => setImmediate(r)));
    return p;
}

// load runs a fresh composer-boot.js: its own document, window and import.
function load(opts) {
    opts = opts || {};
    const head = { children: [], appendChild(c) { c.parentNode = head; head.children.push(c); return c; } };
    function tag(name) {
        const t = {
            tagName: name.toUpperCase(), dataset: {}, listeners: {}, parentNode: null, fired: '',
            addEventListener(ev, fn) { (t.listeners[ev] = t.listeners[ev] || []).push(fn); },
            remove() {
                if (!t.parentNode) return;
                const i = t.parentNode.children.indexOf(t);
                if (i >= 0) t.parentNode.children.splice(i, 1);
                t.parentNode = null;
            },
        };
        return t;
    }
    const document = {
        head: head,
        createElement: tag,
        // The two selectors composer-boot.js asks: a tag of one kind
        // carrying one data-composer-asset.
        querySelector(sel) {
            const m = /^(link|script)\[data-composer-asset="([^"]+)"\]$/.exec(sel);
            if (!m) throw new Error('composer-boot.js asked a selector this fake does not answer: ' + sel);
            return head.children.find(c => c.tagName === m[1].toUpperCase() && c.dataset.composerAsset === m[2]) || null;
        },
    };
    const window = { location: { hash: opts.hash || '' } };
    if (opts.model) window.ComposerModel = opts.model;
    const imports = [];
    const errors = [];
    const ctx = vm.createContext({
        console: { log: console.log, error: (...a) => errors.push(a.map(String).join(' ')) },
        Promise, Error, URL, document, window,
        __import(url) {
            imports.push(url);
            if (opts.importFails) return Promise.reject(new Error('import failed'));
            window.ComposerUI = { openPicker() {} };
            return Promise.resolve({});
        },
    });
    const raw = fs.readFileSync(path.join(SRC_DIR, 'composer-boot.js'), 'utf8');
    let src = raw.replace(/^export /mg, '');
    if (src === raw) throw new Error('composer-boot.js no longer starts its exports at line start; update load()');
    const imp = /\bimport\(/g;
    if ((src.match(imp) || []).length !== 1) throw new Error('composer-boot.js has not exactly one import(); update load()');
    src = src.replace(imp, '__import(').replace(/import\.meta\.url/g, '"http://edge.test/static/operator-station/composer-boot.js"');
    const names = ['ensureComposer', 'ensureComposerModel'].filter(n => new RegExp('function ' + n + '\\(').test(src));
    vm.runInContext(src + '\n__out = {' + names.map(n => n + ': ' + n).join(', ') + '};', ctx);
    const out = ctx.__out;
    // tags lists what was appended, by its asset URL, in order.
    out.tags = () => head.children.map(c => c.dataset.composerAsset);
    out.appended = [];
    const append = head.appendChild;
    head.appendChild = c => { out.appended.push(c.dataset.composerAsset); return append(c); };
    // fire answers the oldest tag for `asset` that has not been answered.
    out.fire = (asset, ev) => {
        const t = head.children.find(c => c.dataset.composerAsset === asset && !c.fired);
        if (!t) throw new Error('no unanswered tag for ' + asset + ' (tags: ' + out.tags().join(', ') + ')');
        t.fired = ev;
        if (ev === 'load' && asset === '/static/operator-station/composer-model.js') window.ComposerModel = opts.loads || { init() {} };
        (t.listeners[ev] || []).forEach(fn => fn({}));
    };
    out.window = window;
    out.imports = imports;
    out.errors = errors;
    return out;
}

const CSS = '/static/operator-station/composer.css';
const SPEC = '/static/operator-station/flowspec-data.js';
const MODEL = '/static/operator-station/composer-model.js';
const RENDER = '/static/operator-station/composer-render.js';

(async function main() {

// ── a request that failed is made again on the next tap ─────────────────────
//
// THE BUG (found on the way, fixed here). loadScript skipped a src whose tag
// was already in <head>, and a tag whose load failed stayed there. So the
// "tap again" every failed load asks for skipped the file it lost: a lost
// flowspec-data.js opened the composer without the flowspec on the second
// tap. A failed tag is taken out; the next tap appends a new one.
console.log('a failed request is made again');
{
    const b = load();
    let p = b.ensureComposer();
    await flush(); b.fire(CSS, 'load');
    await flush(); b.fire(SPEC, 'error');
    const first = await p;
    check('the first tap: a lost flowspec-data.js opens nothing', first === null, String(first));
    check('the failed tag is not left in <head>', b.tags().indexOf(SPEC) < 0, b.tags().join(', '));
    p = b.ensureComposer();
    await flush();
    const specTags = b.appended.filter(a => a === SPEC).length;
    check('the next tap appends flowspec-data.js again', specTags === 2, 'appended: ' + b.appended.join(', '));
    if (specTags === 2) {
        b.fire(SPEC, 'load');
        await flush();
        if (b.tags().indexOf(MODEL) >= 0) { b.fire(MODEL, 'load'); await flush(); }
        const second = await p;
        check('and opens the composer once it has it', !!second, String(second));
    }
}

console.log(failures ? '\n' + failures + ' failure(s)' : '\nall passed');
finished = true;
process.exitCode = failures ? 1 : 0;
})().catch(err => { console.error(err); finished = true; process.exitCode = 1; });
