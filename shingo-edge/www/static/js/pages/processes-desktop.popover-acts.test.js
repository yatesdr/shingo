// processes-desktop.popover-acts.test.js — a popover act's click survives paint.
//
// onClick is bound to #pd-root and boot() puts a listener on the DOCUMENT that
// closes #pd-pop on any click outside it. An act that opens the popover must
// therefore stop the event on its way through #pd-root, or the menu it just
// opened is hidden again one listener later — the "clickable, nothing happens"
// report. The page keeps the set of acts that stop in POPOVER_ACTS; this pin
// walks a REAL bubbling click through the page's own listeners and asserts the
// menu is still open after the bubble ends.
//
// WHAT IS REAL: the page module, booted for real (page-data, the composer read
// over a stubbed fetch, selectStyle, drawFlows), its own onClick on #pd-root,
// and its own document-level closer. The click is dispatched by walking the
// ancestor chain the way a browser bubble does, honouring stopPropagation.
//
// WHAT IS STUBBED: the HTML parser. The page builds its markup by string
// concatenation, and standing up a parser to re-derive one <button> the page
// already spells would be a second renderer. The element under test carries the
// exact attributes drawFlows writes for it and sits at the same depth in the
// tree, which is everything the dispatch path can see.
//
// Run by www/processes_desktop_popover_acts_test.go, like apply-loop.test.js.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

// The page under test declares `function process()` at module scope (its row
// lookup). Stripping the imports makes that a global, and node's own `process`
// object goes with it — so the real one is captured before any load.
const nodeProcess = process;

let failures = 0;
let checks = 0;
function test(name, fn) {
    try {
        fn();
        checks++;
    } catch (e) {
        failures++;
        console.error('FAIL  ' + name + '\n      ' + (e && e.message));
    }
}

// ── a small DOM: elements, an ancestor walk, matches, bubbling ───────────────

function classList() {
    const set = new Set();
    return {
        add: (...cs) => cs.forEach(c => set.add(c)),
        remove: (...cs) => cs.forEach(c => set.delete(c)),
        contains: c => set.has(c),
    };
}

// matches for the selector shapes this page's closest() calls use: '.class',
// '#id', '[data-x]' / '[data-x="v"]', and space-separated ancestors
// ('#pd-svg [data-pos]') — the last part on the node itself, the rest on
// strict ancestors in order.
function attrName(attr) {
    return attr.replace(/^data-/, '').replace(/-([a-z])/g, (_, c) => c.toUpperCase());
}
function matchesOne(n, part) {
    if (part.startsWith('.')) return n.classList.contains(part.slice(1));
    if (part.startsWith('#')) return n.id === part.slice(1);
    const m = part.match(/^\[([^\]=]+)(?:="([^"]*)")?\]$/);
    if (m) {
        const v = n.dataset[attrName(m[1])];
        return m[2] === undefined ? v !== undefined : v === m[2];
    }
    return false;
}
function matches(n, sel) {
    const parts = sel.trim().split(/\s+/);
    if (!matchesOne(n, parts[parts.length - 1])) return false;
    let anc = n.parentNode;
    for (let i = parts.length - 2; i >= 0; i--) {
        while (anc && !matchesOne(anc, parts[i])) anc = anc.parentNode;
        if (!anc) return false;
        anc = anc.parentNode;
    }
    return true;
}

function makeElement(id) {
    const el = {
        id: id || '',
        hidden: false,
        innerHTML: '',
        style: {},
        dataset: {},
        classList: classList(),
        children: [],
        parentNode: null,
        offsetParent: null,
        offsetWidth: 240,
        offsetHeight: 40,
        clientWidth: 0,
        clientHeight: 0,
        listeners: {},
        getAttribute: () => null,
        setAttribute: () => {},
        addEventListener(type, fn) {
            (this.listeners[type] = this.listeners[type] || []).push(fn);
        },
        appendChild(c) { c.parentNode = this; this.children.push(c); return c; },
        querySelector: () => null,
        querySelectorAll: () => [],
        getBoundingClientRect: () => ({ left: 0, top: 0, right: 200, bottom: 24, width: 200, height: 24 }),
        closest(sel) {
            for (let n = this; n; n = n.parentNode) if (matches(n, sel)) return n;
            return null;
        },
    };
    return el;
}

// A bubbling dispatch: target first, then each ancestor, then the document —
// stopPropagation ends the walk after the current node finishes its listeners,
// which is the DOM's own rule and the exact rule POPOVER_ACTS leans on.
function bubble(target, type, doc) {
    const ev = {
        type,
        target,
        bubbles: true,
        propagationStopped: false,
        stopPropagation() { this.propagationStopped = true; },
        stopImmediatePropagation() { this.propagationStopped = true; },
    };
    const chain = [];
    for (let n = target; n; n = n.parentNode) chain.push(n);
    chain.push(doc);
    for (const node of chain) {
        if (ev.propagationStopped) break;
        for (const fn of (node.listeners[type] || []).slice()) fn(ev);
    }
    return ev;
}

// ── the page, booted for real ────────────────────────────────────────────────

function loadPage() {
    // The model is the real one; openUsePreset only draws, but selectStyle and
    // drawBar run the model for real on the way to the click.
    const modelSrc = fs.readFileSync(
        path.join(__dirname, '..', '..', 'operator-station', 'composer-model.js'), 'utf8');
    const modelFactory = vm.runInThisContext(
        '(function(module, exports, console){' + modelSrc + '\nreturn module.exports;})',
        { filename: 'composer-model.js' });
    const modelMod = { exports: {} };
    const ComposerModel = modelFactory(modelMod, modelMod.exports, console);

    const escSrc = fs.readFileSync(
        path.join(__dirname, '..', '..', '..', '..', '..', 'shared', 'esc.js'), 'utf8');
    const escFactory = vm.runInThisContext(
        '(function(module, exports, console){' +
        escSrc.replace(/^import .*$/gm, '').replace(/export\s+function\s+esc\(/, 'function esc(') +
        '\nreturn { esc };})',
        { filename: 'esc.js' });
    const esc = escFactory({}, {}, console).esc;

    const root = makeElement('pd-root');
    const pop = makeElement('pd-pop');
    const bar = makeElement('pd-bar');
    const scrim = makeElement('pd-scrim');
    const main = makeElement('pd-main');
    root.appendChild(main);

    const pageData = makeElement('page-data');
    pageData.dataset = {
        processes: JSON.stringify([{ id: 1, name: 'Press 1', group_id: 0, active_style_id: 1, flow_composer_enabled: true }]),
        processGroups: '[]',
        styles: JSON.stringify([{ id: 1, process_id: 1, name: 'Style A' }]),
        stations: '[]',
        stationNodes: '{}',
        loaderGaps: '[]',
        activeProcessId: '1',
    };

    const byId = {
        'page-data': pageData,
        'pd-root': root,
        'pd-pop': pop,
        'pd-bar': bar,
        'pd-scrim': scrim,
    };

    const doc = {
        readyState: 'complete',
        body: { classList: classList(), dataset: {} },
        listeners: {},
        getElementById: id => byId[id] || null,
        addEventListener(type, fn) { (doc.listeners[type] = doc.listeners[type] || []).push(fn); },
        querySelector: () => null,
        querySelectorAll: () => [],
    };

    const win = {
        innerWidth: 1280,
        innerHeight: 800,
        location: { search: '?process=1', hash: '' },
        ComposerModel,
        addEventListener: () => {},
    };

    const COMPOSER = {
        styles: [{
            id: 1, name: 'Style A',
            claims: [{
                core_node_name: 'PLN_01', swap_mode: 'single_robot', payload_code: 'PART-A',
                inbound_source: 'SMN_010', outbound_destination: 'DN_001',
            }],
            parts: ['PART-A'],
            advanced: {},
        }],
        routing: [
            { core_node_name: 'SMN_010', role: 'source', label: 'SMN_010', enabled: true, sequence: 1 },
            { core_node_name: 'DN_001', role: 'destination', label: 'DN_001', enabled: true, sequence: 2 },
        ],
        presets: [{ id: 7, name: 'Two up', where: 'from Style A' }],
        palette: ['PART-A'],
        cell: { positions: [{ core_node_name: 'PLN_01', kind: 'front', sequence: 1 }], groups: {} },
    };

    const noopModal = () => { throw new Error('the scrim must not open in this pin'); };
    global.window = win;
    global.document = doc;
    Object.assign(global, { esc, showModal: noopModal, hideModal: noopModal });
    Object.assign(win, { esc, showModal: noopModal, hideModal: noopModal });
    global.fetch = url => {
        if (/\/api\/processes\/1\/composer$/.test(url)) {
            return Promise.resolve({ ok: true, json: async () => COMPOSER });
        }
        return Promise.resolve({ ok: false, json: async () => ({}) });
    };
    // schedulePreview must not fire: this pin is about the click, and a timer
    // landing mid-test would redraw the tab under it.
    global.setTimeout = () => 0;
    global.clearTimeout = () => {};
    global.requestAnimationFrame = () => {};
    global.CSS = { escape: s => String(s).replace(/[^a-zA-Z0-9_-]/g, c => '\\' + c) };
    // The picture and the map are stubbed to fail loudly if they ever run: the
    // pin's path draws the bar but never the picture (no #pd-svg exists).
    global.renderFlowPicture = () => { throw new Error('drawPicture must not run in this pin'); };
    global.pictureRows = () => { throw new Error('pictureRows must not run in this pin'); };
    global.sentencesFromModel = () => { throw new Error('sentencesFromModel must not run in this pin'); };
    for (const fn of ['makeProjector', 'rotate90For', 'dist2', 'cubicLength', 'cubicPathD', 'laneKey']) {
        global[fn] = () => { throw new Error(fn + ' must not run in this pin'); };
    }
    global.showModal = () => { throw new Error('showModal must not run in this pin'); };
    global.hideModal = () => { throw new Error('hideModal must not run in this pin'); };

    let src = fs.readFileSync(path.join(__dirname, 'processes-desktop.js'), 'utf8');
    src = src.replace(/^import[\s\S]*?from\s+'[^']+';\s*$/gm, '');
    src = src.replace(/^export\s*\{[^}]*\};?\s*$/m, '');
    // Each load gets its own function scope: the page declares top-level
    // consts (M, B, S, POPOVER_ACTS…) and this pin loads it twice.
    vm.runInThisContext('(function(){\n' + src + '\n})();',
        { filename: 'processes-desktop.js' });

    return { root, pop, main, doc };
}

// drawFlows writes the button as: <button class="pd-btn" data-act="use-preset">
function addUsePresetButton(main) {
    const btn = makeElement('');
    btn.classList.add('pd-btn');
    btn.dataset.act = 'use-preset';
    main.appendChild(btn);
    return btn;
}

const drain = () => new Promise(r => setImmediate(r));

let doc0 = null;
const realLoad = loadPage;
loadPage = function () {
    const ctx = realLoad();
    doc0 = global.document;
    return ctx;
};

async function mainAsync() {
    // ── the pin: a real click on Use a preset leaves the menu open ──────────
    {
        const { root, pop, main } = loadPage();
        await drain();
        await drain();
        if (!root.listeners.click || !root.listeners.click.length) {
            throw new Error('boot installed no click listener on #pd-root');
        }
        if (!doc0.listeners.click || !doc0.listeners.click.length) {
            throw new Error('boot installed no document-level closer');
        }
        const btn = addUsePresetButton(main);
        bubble(btn, 'click', doc0);
        if (pop.hidden !== false) {
            throw new Error('the popover is hidden after the click — the document-level closer ' +
                'hid it before paint, because the act does not stop the event');
        }
        if (!/use-preset-pick/.test(pop.innerHTML) || !/Two up/.test(pop.innerHTML)) {
            throw new Error('the popover does not name the cell\'s presets: ' + pop.innerHTML);
        }
        checks++;
        console.log('ok: a click on Use a preset opens the preset list and it survives the bubble');
    }

    // ── the closer still closes: a click genuinely outside the popover ──────
    {
        const { root, pop, main, doc } = loadPage();
        await drain();
        await drain();
        const btn = addUsePresetButton(main);
        bubble(btn, 'click', doc);
        if (pop.hidden !== false) throw new Error('setup: the popover should be open here');
        bubble(main, 'click', doc);   // a click with no popover act behind it
        if (pop.hidden !== true) {
            throw new Error('the document-level closer no longer closes a click outside the popover');
        }
        checks++;
        console.log('ok: a click outside the popover still closes it');
    }

    if (failures > 0) {
        console.error('FAILED: ' + failures + ' of ' + (failures + checks));
        nodeProcess.exit(1);
    }
    console.log('OK: ' + checks + ' checks passed');
    nodeProcess.exit(0);
}

mainAsync().catch(e => {
    console.error('FAIL (harness): ' + (e && e.message));
    nodeProcess.exit(1);
});
