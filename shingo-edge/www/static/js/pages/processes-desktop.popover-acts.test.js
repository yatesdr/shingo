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
const assert = require('assert');

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
        // Appended children survive an innerHTML write. The page re-writes
        // its markup by assignment (drawFlows); a real element drops the
        // children then parses the string — this stub keeps them, which is
        // the one behaviour the bar-goto pin needs (a button appended AFTER
        // a draw must still be in the tree for its click).
        scrollIntoView(opts) { el.lastScroll = opts; },
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

function loadPage(loadOpts) {
    loadOpts = loadOpts || {};
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
    // A pin that reads the scrim's inputs wires them before boot, since a
    // deep-linked sheet is drawn (and its listeners bound) during it.
    if (loadOpts.onScrim) loadOpts.onScrim(scrim);
    const main = makeElement('pd-main');
    root.appendChild(main);

    const pageData = makeElement('page-data');
    pageData.dataset = {
        processes: JSON.stringify(loadOpts.processes || [{ id: 1, name: 'Press 1', group_id: 0, active_style_id: 1, flow_composer_enabled: loadOpts.gateOff ? false : true }]),
        processGroups: '[]',
        styles: JSON.stringify(loadOpts.styles || [{ id: 1, process_id: 1, name: 'Style A' }]),
        stations: JSON.stringify(loadOpts.stations || []),
        stationNodes: JSON.stringify(loadOpts.stationNodes || {}),
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
    Object.assign(byId, loadOpts.extraIds || {});

    const doc = {
        readyState: 'complete',
        body: { classList: classList(), dataset: {} },
        listeners: {},
        getElementById: id => byId[id] || null,
        addEventListener(type, fn) { (doc.listeners[type] = doc.listeners[type] || []).push(fn); },
        querySelector: () => null,
        querySelectorAll: sel => (loadOpts.docQSA ? loadOpts.docQSA(sel) : []),
    };

    const win = {
        innerWidth: 1280,
        innerHeight: 800,
        location: { search: loadOpts.search || '?process=1', hash: loadOpts.hash || '' },
        ComposerModel,
        addEventListener: () => {},
        // THE ADDRESS BAR IS UNDER TEST in the navigation pin: boot records
        // every replaceState the page mints, and the pins read the list back.
        history: { replaceState: (st, title, url) => { win.replaceStates.push(String(url)); } },
        replaceStates: [],
    };
    // The generated flowspec block, the one the page loads beside it, for the
    // pins that read which fields a mode requires and what each is called.
    if (loadOpts.flowspec) {
        const fsSrc = fs.readFileSync(
            path.join(__dirname, '..', '..', 'operator-station', 'flowspec-data.js'), 'utf8');
        vm.runInThisContext('(function(window){' + fsSrc + '\n})', { filename: 'flowspec-data.js' })(win);
    }

    const COMPOSER = (loadOpts.composer) || {
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
    // A deep link that opens the advanced sheet (#adv=) really does open one:
    // shingoedge.js's .active toggle, minus the DOM it walks. Pins that never
    // deep-link a sheet keep the thrower and fail loudly if a sheet opens.
    const activeModal = id => {
        const m = doc.getElementById(id);
        if (m) m.classList.add('active');
    };
    const sheetOpener = loadOpts.allowSheet ? activeModal : noopModal;
    // A pin that opens a sheet may also see it close: the .active toggle's
    // other half, so "the sheet closed" is a class the pin can read.
    const sheetCloser = loadOpts.allowSheet ? (id => {
        const m = doc.getElementById(id);
        if (m) m.classList.remove('active');
    }) : noopModal;
    global.window = win;
    global.document = doc;
    // The sheets' writes build their bodies through the real body module.
    win.DesktopBodies = require(path.join(__dirname, 'desktop-bodies.js'));
    Object.assign(global, { esc, showModal: sheetOpener, hideModal: sheetCloser });
    Object.assign(win, { esc, showModal: sheetOpener, hideModal: sheetCloser });
    global.fetch = (url, init) => {
        if (loadOpts.fetch) {
            const r = loadOpts.fetch(url, init);
            if (r) return r;
        }
        if (/\/api\/processes\/1\/composer$/.test(url)) {
            return Promise.resolve({ ok: true, json: async () => COMPOSER });
        }
        return Promise.resolve({ ok: false, json: async () => ({}) });
    };
    // schedulePreview must not fire: this pin is about the click, and a timer
    // landing mid-test would redraw the tab under it.
    global.setTimeout = () => 0;
    global.clearTimeout = () => {};
    // A pin that drives the preview keeps the timers instead: the page's own
    // schedulePreview hands runPreview here, and the pin runs it when it wants
    // the request on the wire.
    if (loadOpts.timers) global.setTimeout = fn => loadOpts.timers.push(fn);
    global.requestAnimationFrame = () => {};
    // The rail's re-read runs on an interval; a pin that drives it collects
    // the callback and runs it when it wants the read on the wire.
    global.setInterval = fn => { if (loadOpts.intervals) loadOpts.intervals.push(fn); return 0; };
    global.CSS = { escape: s => String(s).replace(/[^a-zA-Z0-9_-]/g, c => '\\' + c) };
    // The picture and the map are stubbed to fail loudly if they ever run: the
    // pin's path draws the bar but never the picture (no #pd-svg exists).
    global.renderFlowPicture = () => { throw new Error('drawPicture must not run in this pin'); };
    // The row words are the table's (front/back under a position), so they
    // are read whenever the table draws; a stub cell has no rows to name.
    global.pictureRows = () => ({});
    global.sentencesFromModel = () => { throw new Error('sentencesFromModel must not run in this pin'); };
    global.relationPlacement = () => { throw new Error('relationPlacement must not run in this pin'); };
    global.moduleOf = () => { throw new Error('moduleOf must not run in this pin'); };
    for (const fn of ['makeProjector', 'rotate90For', 'dist2', 'cubicLength', 'cubicPathD', 'laneKey']) {
        global[fn] = () => { throw new Error(fn + ' must not run in this pin'); };
    }
    win.pictureCalls = [];
    win.pictureViews = [];
    if (loadOpts.realPicture) {
        // The picture pins draw for real: operator-flow.js's own renderer,
        // row words, sentences and module membership, so what the page sizes
        // its frame from is the height the picture itself reported.
        const flow = loadFlow();
        global.renderFlowPicture = (view, opts) => {
            const markup = flow.renderFlowPicture(view, opts);
            win.pictureCalls.push(opts);
            win.pictureViews.push(view);
            return markup;
        };
        global.pictureRows = flow.pictureRows;
        global.sentencesFromModel = flow.sentencesFromModel;
        global.relationPlacement = flow.relationPlacement;
        global.moduleOf = flow.moduleOf;
    }
    global.showModal = sheetOpener;
    global.hideModal = sheetCloser;

    let src = fs.readFileSync(path.join(__dirname, 'processes-desktop.js'), 'utf8');
    src = src.replace(/^import[\s\S]*?from\s+'[^']+';\s*$/gm, '');
    src = src.replace(/^export\s*\{[^}]*\};?\s*$/m, '');
    // Each load gets its own function scope: the page declares top-level
    // consts (M, B, S, POPOVER_ACTS…) and this pin loads it twice.
    vm.runInThisContext('(function(){\n' + src + '\n})();',
        { filename: 'processes-desktop.js' });

    return { root, pop, bar, main, doc, win, scrim };
}

// operator-flow.js in its own context, the way operator-flow.test.js loads
// it: scene-geom's exports stripped to declarations, the flow module's imports
// stripped and its station helpers stubbed, the model beside it on window so
// sentencesFromModel reads the same api the page does.
function loadFlow() {
    const dir = path.join(__dirname, '..', '..', 'operator-station');
    const ctx = {
        console, Math, Set, Number, isFinite, JSON, Object, Array,
        document: { getElementById() { return null; } },
        window: { location: { hash: '' } },
        el() { return {}; },
        esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;'); },
        getView() { return null; },
    };
    vm.createContext(ctx);
    vm.runInContext(fs.readFileSync(path.join(dir, 'composer-model.js'), 'utf8'), ctx);
    const geom = fs.readFileSync(
        path.join(__dirname, '..', '..', '..', '..', '..', 'shared', 'scene-geom.js'), 'utf8');
    vm.runInContext(geom.replace(/^export /mg, ''), ctx);
    const raw = fs.readFileSync(path.join(dir, 'operator-flow.js'), 'utf8');
    const src = raw.replace(/^import[^;]+;\s*/mg, '').replace(/^export /mg, '');
    vm.runInContext(src + '\n__out = { renderFlowPicture, pictureRows, sentencesFromModel, relationPlacement, ' +
        'moduleOf: typeof moduleOf === "function" ? moduleOf : undefined };', ctx);
    return ctx.__out;
}

// A press tall enough to wrap at a 600 px frame: four single-robot positions
// and a press index with its on-deck partner — five modules, two to a row.
const TALL_COMPOSER = {
    styles: [{
        id: 1, name: 'Style A',
        claims: ['PLN_01', 'PLN_02', 'PLN_03', 'PLN_04'].map(n => ({
            core_node_name: n, swap_mode: 'single_robot', payload_code: 'PART-A',
            inbound_source: 'SMN_010', outbound_destination: 'DN_001',
        })).concat([{
            core_node_name: 'PLN_05', swap_mode: 'two_robot_press_index', payload_code: 'PART-A',
            paired_core_node: 'PLN_06', inbound_source: 'SMN_010', outbound_destination: 'DN_001',
        }]),
        parts: ['PART-A'],
        advanced: {},
    }],
    routing: [
        { core_node_name: 'SMN_010', role: 'source', label: 'SMN_010', enabled: true, sequence: 1 },
        { core_node_name: 'DN_001', role: 'destination', label: 'DN_001', enabled: true, sequence: 2 },
    ],
    presets: [],
    palette: ['PART-A'],
    cell: {
        positions: ['PLN_01', 'PLN_02', 'PLN_03', 'PLN_04', 'PLN_05', 'PLN_06'].map((n, i) => ({
            core_node_name: n, kind: n === 'PLN_06' ? 'back' : 'front', sequence: i + 1,
        })),
        groups: {},
    },
};

// The frame and the svg in it, as drawFlows writes them: .pd-pic laid out
// `width` px wide, #pd-svg inside it with its attributes recorded.
function pictureFrame(width) {
    const pic = makeElement('');
    pic.classList.add('pd-pic');
    pic.clientWidth = width;
    const svg = makeElement('pd-svg');
    svg.attrs = {};
    svg.setAttribute = (k, v) => { svg.attrs[k] = String(v); };
    svg.getAttribute = k => (k in svg.attrs ? svg.attrs[k] : null);
    pic.appendChild(svg);
    return { pic, svg };
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
loadPage = function (...args) {
    const ctx = realLoad(...args);
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

    // ── the navigation pin: one replaceState per navigation ─────────────────
    {
        const { main, doc, win } = loadPage();
        await drain();
        await drain();
        win.replaceStates.length = 0;   // boot's own write is not the pin
        // A click on a style row lands on that flow and the URL says which.
        const styleRow = makeElement('');
        styleRow.dataset.style = '1';
        main.appendChild(styleRow);
        bubble(styleRow, 'click', doc);
        if (win.replaceStates.length !== 1 || win.replaceStates[0] !== '?process=1#style=1') {
            throw new Error('a style click must mint exactly one "?process=1#style=1", got ' +
                JSON.stringify(win.replaceStates));
        }
        // A tab click restates the tab.
        const tabBtn = makeElement('');
        tabBtn.dataset.tab = 'screens';
        main.appendChild(tabBtn);
        bubble(tabBtn, 'click', doc);
        if (win.replaceStates.length !== 2 || win.replaceStates[1] !== '?process=1#tab=screens') {
            throw new Error('a tab click must mint exactly one "?process=1#tab=screens", got ' +
                JSON.stringify(win.replaceStates));
        }
        // Returning to the list drops the state back to the bare path.
        const listBtn = makeElement('');
        listBtn.dataset.act = 'list';
        main.appendChild(listBtn);
        bubble(listBtn, 'click', doc);
        if (win.replaceStates.length !== 3 || win.replaceStates[2] !== '/processes') {
            throw new Error('returning to the list must mint exactly one bare "/processes", got ' +
                JSON.stringify(win.replaceStates));
        }
        checks++;
        console.log('ok: one replaceState per navigation, in the page\'s own vocabulary');
    }

    // ── the one-shot pin: boot-only hash options are consumed, not re-minted ──
    {
        const { win } = loadPage({ hash: '#style=1;adv=PLN_01;part=PLN_01', allowSheet: true });
        await drain();
        await drain();
        // openProcess's own syncHash is boot's one navigation write; the
        // one-shots (;adv=, ;part=) must be gone from it.
        if (win.replaceStates.length !== 1) {
            throw new Error('boot must mint exactly one replaceState, got ' +
                JSON.stringify(win.replaceStates));
        }
        if (win.replaceStates[0] !== '?process=1#style=1') {
            throw new Error('boot must restate only the durable state, got ' + win.replaceStates[0]);
        }
        checks++;
        console.log('ok: boot-only deep links (;adv=, ;part=) are consumed, never re-minted');
    }

    // ── the Escape rule: popover first, sheet asks on typed work ────────────
    {
        const { pop, main, doc, scrim } = loadPage();
        await drain();
        await drain();
        // The popover pins load the page with showModal throwing (the scrim
        // must never open under them). This pin needs the REAL shape: toggle
        // .active and clear on hide — shingoedge.js's, minus the DOM it walks.
        const realShow = id => {
            const m = doc.getElementById(id);
            if (m) m.classList.add('active');
        };
        const realHide = (id, opts) => {
            const m = doc.getElementById(id);
            if (!m) return;
            m.classList.remove('active');
            if (opts && opts.preserveState) return;
        };
        global.showModal = realShow;
        global.hideModal = realHide;
        Object.assign(global.window, { showModal: realShow, hideModal: realHide });
        const escapeKey = () => {
            for (const fn of (doc.listeners.keydown || [])) fn({ key: 'Escape' });
        };
        // POPOVER FIRST. With a popover open above no sheet at all, Escape
        // closes the popover — and must not need a sheet to exist to do it.
        const btn = addUsePresetButton(main);
        bubble(btn, 'click', doc);
        if (pop.hidden !== false) throw new Error('setup: the popover should be open here');
        escapeKey();
        if (pop.hidden !== true) {
            throw new Error('Escape did not close the open popover; the popover arm is not first');
        }
        checks++;
        console.log('ok: Escape closes the open popover first');

        // THE SHEET ASKS. The Add-group sheet has two plain text fields and
        // needs no fetch to open. Type into one, Escape: the question comes up
        // INSTEAD of the close, and Keep editing puts the held sheet back —
        // markup and typed value with it.
        const addBtn = makeElement('');
        addBtn.dataset.act = 'add-group';
        addBtn.closest = function (sel) { return sel === '[data-act]' ? this : null; };
        main.appendChild(addBtn);
        bubble(addBtn, 'click', doc);
        await drain();
        await drain();
        if (scrim.innerHTML.indexOf('data-f="name"') < 0) {
            throw new Error('setup: the Add-group sheet should be open here; scrim: ' + scrim.innerHTML.slice(0, 200));
        }
        // Type: the input carries the markup's value attribute ("") and the
        // typed property — attribute-vs-property IS the edit.
        // The fake input records setAttribute the way a real one does, so the
        // page's write-back into the markup is observable in the snapshot.
        const nameInput = {
            type: 'text', value: 'Presses west',
            getAttribute(n) { return n === 'data-f' ? 'name' : ''; },
            hasAttribute() { return false; },
            checked: false,
        };
        const realQSA = scrim.querySelectorAll;
        scrim.querySelectorAll = sel => (sel === '[data-f]' ? [nameInput] : (realQSA ? realQSA.call(scrim, sel) : []));
        escapeKey();
        if (scrim.innerHTML.indexOf('Discard the changes?') < 0) {
            throw new Error('Escape on a sheet with typed work closed or discarded it instead of asking; ' +
                'scrim: ' + scrim.innerHTML.slice(0, 200));
        }
        if (scrim.innerHTML.indexOf('sheet-cancel') < 0) {
            throw new Error('the ask sheet carries no Keep editing door');
        }
        // Keep editing restores the held sheet. The ask sheet's buttons live
        // in the scrim, whose own click listener (onAdvClick) answers them —
        // so the click bubbles through the scrim, not #pd-main. The typed
        // value rides the hold as a property and is re-applied: read it back
        // the way the page itself would, through the sheet's own accessor
        // shape (the restored markup's input, found by data-f).
        const keep = makeElement('');
        keep.dataset.act = 'sheet-cancel';
        keep.closest = function (sel) { return sel === '[data-act]' ? this : null; };
        scrim.appendChild(keep);
        // The restored markup's input: a real DOM re-derives it by parsing;
        // the stub re-types the one field the sheet had. `value` is a setter
        // recording what the page writes, which is the assertion.
        const restored = {
            type: 'text', _v: '', dataset: { f: 'name' },
            getAttribute: () => 'name',
            set value(v) { this._v = v; },
            get value() { return this._v; },
        };
        scrim.querySelectorAll = sel => (sel === '[data-f]' ? [restored] : []);
        bubble(keep, 'click', doc);
        await drain();
        await drain();
        if (scrim.innerHTML.indexOf('data-f="name"') < 0) {
            throw new Error('Keep editing did not put the held sheet back; scrim: ' +
                scrim.innerHTML.slice(0, 200));
        }
        if (restored._v !== 'Presses west') {
            throw new Error('Keep editing did not put the typed value back; got "' + restored._v + '"');
        }
        checks++;
        console.log('ok: Escape over a typed-in sheet asks, and Keep editing restores it');
    }

    // ── the bar's problem line is the control ────────────────────────────────
    {
        // A claim whose choreography is unset puts the style's own position
        // under a swap_mode finding: the bar reads BLOCKED and its first
        // finding names PLN_01 — a node the engineer can go to.
        const { root, main, doc } = loadPage({
            composer: {
                styles: [{
                    id: 1, name: 'Style A',
                    claims: [{
                        core_node_name: 'PLN_01', swap_mode: null, payload_code: 'PART-A',
                        inbound_source: 'SMN_010', outbound_destination: 'DN_001',
                    }],
                    parts: ['PART-A'],
                    advanced: {},
                }],
                routing: [
                    { core_node_name: 'SMN_010', role: 'source', label: 'SMN_010', enabled: true, sequence: 1 },
                    { core_node_name: 'DN_001', role: 'destination', label: 'DN_001', enabled: true, sequence: 2 },
                ],
                presets: [],
                palette: ['PART-A'],
                cell: { positions: [{ core_node_name: 'PLN_01', kind: 'front', sequence: 1 }], groups: {} },
            },
        });
        await drain();
        await drain();
        await drain();
        await drain();
        const bar = doc.getElementById('pd-bar');
        if (!/>\d+ to fix(?: · showing the first)?</.test(bar.innerHTML)) {
            throw new Error('setup: the bar should read blocked here; bar: ' + bar.innerHTML.slice(0, 200));
        }
        if (!/data-act="bar-goto" data-node="PLN_01"/.test(bar.innerHTML)) {
            throw new Error('the blocked bar does not offer the go-to door on the line that names the position; ' +
                'bar: ' + bar.innerHTML.slice(0, 300));
        }
        // A real click on it: the page selects the position (the same state a
        // card click sets) and scrolls the table row into view. The fake root
        // cannot search a tree, so it is taught this one row — the click
        // itself still walks the page's own closest()/data-act dispatch.
        const line = makeElement('');
        line.dataset.act = 'bar-goto';
        line.dataset.node = 'PLN_01';
        const row = makeElement('');
        row.dataset.row = 'PLN_01';
        row.parentNode = main;
        main.appendChild(row);
        main.appendChild(line);
        const rootQuery = root.querySelector;
        root.querySelector = sel => (sel === '[data-row="PLN_01"]' ? row : (rootQuery ? rootQuery.call(root, sel) : null));
        bubble(line, 'click', doc);
        if (row.lastScroll === undefined) {
            throw new Error('clicking the problem line did not scroll the finding\'s row into view');
        }
        if (row.lastScroll.block !== 'nearest') {
            throw new Error('the row was scrolled with ' + JSON.stringify(row.lastScroll) + ', not nearest');
        }
        checks++;
        console.log('ok: the bar\'s problem line takes you to the finding\'s position');
    }

    // ── a finding with no position is not a control ──────────────────────────
    {
        // The unplaced-part finding has no node: PLN_01 is placed and set up,
        // but the style carries a second part with nowhere to go.
        const { main, doc } = loadPage({
            composer: {
                styles: [{
                    id: 1, name: 'Style A',
                    claims: [{
                        core_node_name: 'PLN_01', swap_mode: 'single_robot', payload_code: 'PART-A',
                        inbound_source: 'SMN_010', outbound_destination: 'DN_001',
                    }],
                    parts: ['PART-A', 'PART-B'],
                    advanced: {},
                }],
                routing: [
                    { core_node_name: 'SMN_010', role: 'source', label: 'SMN_010', enabled: true, sequence: 1 },
                    { core_node_name: 'DN_001', role: 'destination', label: 'DN_001', enabled: true, sequence: 2 },
                ],
                presets: [],
                palette: ['PART-A', 'PART-B'],
                cell: { positions: [{ core_node_name: 'PLN_01', kind: 'front', sequence: 1 }], groups: {} },
            },
        });
        await drain();
        await drain();
        await drain();
        await drain();
        const bar = doc.getElementById('pd-bar');
        if (!/>\d+ to fix(?: · showing the first)?</.test(bar.innerHTML)) {
            throw new Error('setup: the unplaced part should block the bar; bar: ' + bar.innerHTML.slice(0, 200));
        }
        if (/data-act="bar-goto"/.test(bar.innerHTML)) {
            throw new Error('the bar offers a go-to door with no position behind the finding');
        }
        checks++;
        console.log('ok: a finding with no position is not clickable');
    }

    // ── an unplaced part carries its own door off the flow ───────────────────
    {
        // The bar's fix chip was the desktop's only way to take a part that
        // sits on no position out of the style's part set; the part picker's
        // take-off needs a position. The door is on the part itself now, as on
        // the station's strip: the same removePart, an edit to the draft, no
        // request.
        const sent = [];
        const { main, doc } = loadPage({
            composer: {
                styles: [{
                    id: 1, name: 'Style A',
                    claims: [{
                        core_node_name: 'PLN_01', swap_mode: 'single_robot', payload_code: 'PART-A',
                        inbound_source: 'SMN_010', outbound_destination: 'DN_001',
                    }],
                    parts: ['PART-A', 'PART-B'],
                    advanced: {},
                }],
                routing: [
                    { core_node_name: 'SMN_010', role: 'source', label: 'SMN_010', enabled: true, sequence: 1 },
                    { core_node_name: 'DN_001', role: 'destination', label: 'DN_001', enabled: true, sequence: 2 },
                ],
                presets: [],
                palette: ['PART-A', 'PART-B'],
                cell: { positions: [{ core_node_name: 'PLN_01', kind: 'front', sequence: 1 }], groups: {} },
            },
            fetch: url => {
                if (!/\/api\/processes\/1\/composer$/.test(url)) sent.push(url);
                return null;
            },
        });
        await drain();
        await drain();
        await drain();
        await drain();
        const bar = doc.getElementById('pd-bar');
        if (!/data-act="rmpart" data-part="PART-B"[^>]*>Take PART-B off this flow</.test(bar.innerHTML)) {
            throw new Error('the unplaced part has no door off the flow; bar: ' + bar.innerHTML.slice(0, 400));
        }
        if (/data-part="PART-A"/.test(bar.innerHTML)) {
            throw new Error('a placed part was offered a door off the flow; its way out is its position');
        }
        const before = sent.length;
        const door = makeElement('');
        door.dataset.act = 'rmpart';
        door.dataset.part = 'PART-B';
        door.parentNode = main;
        main.appendChild(door);
        bubble(door, 'click', doc);
        if (/data-act="rmpart"/.test(bar.innerHTML)) {
            throw new Error('the part is still offered after its door was taken; bar: ' + bar.innerHTML.slice(0, 400));
        }
        if (/to fix/.test(bar.innerHTML) || /PART-B/.test(bar.innerHTML)) {
            throw new Error('the unplaced-part finding outlived the part; bar: ' + bar.innerHTML.slice(0, 400));
        }
        if (sent.length !== before) {
            throw new Error('taking a part off the draft sent a request: ' + sent.slice(before).join(', '));
        }
        checks++;
        console.log('ok: an unplaced part is taken off the flow from its own door, and only it');
    }

    // ── the screen sheet's picker names a position another process holds ────
    {
        // One position belongs to one process; the save refuses a second. The
        // picker says so before the engineer tries: a name that is a live
        // position of ANOTHER process reads "on <that process>", from one
        // read of /api/process-nodes at sheet open. A sibling screen's claim
        // on this process still reads "claimed by <screen>", and wins when a
        // name is both. This process's own rows get no "on".
        const opts = makeElement('');
        const input = makeElement('');
        const box = makeElement('npk-positions');
        box.dataset.npkbox = 'positions';
        box.querySelector = sel => (sel === '[data-npkq]' ? input
            : sel === '[data-npkpart="opts"]' ? opts : null);
        let sheetOpen = false;
        const fetched = [];
        const ok = body => Promise.resolve({ ok: true, json: async () => body });
        const { main, doc } = loadPage({
            allowSheet: true,
            stations: [
                { id: 10, process_id: 1, name: 'Screen A' },
                { id: 11, process_id: 1, name: 'Screen B' },
            ],
            stationNodes: { '11': ['PLN_02'] },
            extraIds: { 'npk-positions': box },
            docQSA: sel => (sel === '.pd-npk' && sheetOpen ? [box] : []),
            fetch: url => {
                fetched.push(url);
                if (url === '/api/operator-stations/10/claimed-nodes') return ok(['PLN_01']);
                if (url === '/api/core-nodes') {
                    return ok(['PLN_01', 'PLN_02', 'PLN_03', 'PLN_04', 'PLN_05']
                        .map(name => ({ name, node_type: 'PLN' })));
                }
                if (url === '/api/process-nodes') {
                    return ok([
                        { process_id: 1, core_node_name: 'PLN_01', process_name: 'Press 1' },
                        { process_id: 1, core_node_name: 'PLN_04', process_name: 'Press 1' },
                        { process_id: 2, core_node_name: 'PLN_02', process_name: 'Press 2' },
                        { process_id: 2, core_node_name: 'PLN_03', process_name: 'Press 2' },
                    ]);
                }
                return null;
            },
        });
        await drain();
        await drain();
        fetched.length = 0;   // boot's reads are not the pin
        sheetOpen = true;
        const edit = makeElement('');
        edit.dataset.act = 'screen-edit';
        edit.dataset.station = '10';
        edit.closest = function (sel) { return sel === '[data-act]' ? this : null; };
        main.appendChild(edit);
        bubble(edit, 'click', doc);
        for (let i = 0; i < 6; i++) await drain();
        const pn = fetched.filter(u => u === '/api/process-nodes').length;
        if (pn !== 1) {
            throw new Error('opening the screen sheet must read /api/process-nodes exactly once, got ' +
                pn + ' in ' + JSON.stringify(fetched));
        }
        const others = fetched.filter(u => u !== '/api/process-nodes').sort();
        const want = ['/api/core-nodes', '/api/operator-stations/10/claimed-nodes'];
        if (JSON.stringify(others) !== JSON.stringify(want)) {
            throw new Error('the sheet\'s other reads changed: ' + JSON.stringify(others));
        }
        for (const fn of (input.listeners.focus || [])) fn({ type: 'focus' });
        const row = name => {
            const m = opts.innerHTML.match(new RegExp('data-npkname="' + name + '">' + name +
                '(?:<small>([^<]*)</small>)?</button>'));
            if (!m) throw new Error('no picker row for ' + name + ': ' + opts.innerHTML.slice(0, 400));
            return m[1] || '';
        };
        const got = {};
        for (const n of ['PLN_01', 'PLN_02', 'PLN_03', 'PLN_04', 'PLN_05']) got[n] = row(n);
        const expect = { PLN_01: '', PLN_02: 'claimed by Screen B', PLN_03: 'on Press 2', PLN_04: '', PLN_05: '' };
        if (JSON.stringify(got) !== JSON.stringify(expect)) {
            throw new Error('picker notes wrong: got ' + JSON.stringify(got) + ', want ' + JSON.stringify(expect));
        }
        checks++;
        console.log('ok: the screen picker marks another process\'s position "on <process>" from one read');
    }

    // ── the screen sheet says which removed position it had to keep ─────────
    //
    // Saving a screen's positions is a set-to, and a removed position with an
    // active order cannot be retired: the server disables it, keeps it on the
    // screen, and names it in `kept`. A sheet that closed on that reply would
    // leave the engineer believing the position was gone.
    async function saveScreenWithReply(reply) {
        const opts = makeElement('');
        const input = makeElement('');
        const box = makeElement('npk-positions');
        box.dataset.npkbox = 'positions';
        box.querySelector = sel => (sel === '[data-npkq]' ? input
            : sel === '[data-npkpart="opts"]' ? opts : null);
        const status = makeElement('');
        let sheetOpen = false;
        const puts = [];
        const ok = body => Promise.resolve({ ok: true, json: async () => body });
        const { main, doc, scrim } = loadPage({
            allowSheet: true,
            stations: [{ id: 10, process_id: 1, name: 'Screen A', enabled: true }],
            stationNodes: { '10': ['PLN_01', 'PLN_02'] },
            extraIds: { 'npk-positions': box },
            docQSA: sel => (sel === '.pd-npk' && sheetOpen ? [box] : []),
            fetch: (url, init) => {
                const method = (init && init.method) || 'GET';
                // Nothing names PLN_02, so the removal goes through on the first Save.
                if (url === '/api/operator-stations/10/claimed-nodes/impact') return ok({ warning: '' });
                if (url === '/api/operator-stations/10/claimed-nodes') {
                    if (method === 'PUT') {
                        puts.push(JSON.parse(init.body));
                        return ok(reply);
                    }
                    return ok(['PLN_01', 'PLN_02']);
                }
                if (url === '/api/operator-stations/10' && method === 'PUT') return ok({ status: 'ok' });
                if (url === '/api/operator-stations') {
                    return ok([{ id: 10, process_id: 1, name: 'Screen A', enabled: true }]);
                }
                if (url === '/api/core-nodes') {
                    return ok(['PLN_01', 'PLN_02'].map(name => ({ name, node_type: 'PLN' })));
                }
                if (url === '/api/process-nodes') return ok([]);
                return null;
            },
        });
        scrim.querySelector = sel => (sel === '.mf .st' ? status : null);
        await drain();
        await drain();
        sheetOpen = true;
        const edit = makeElement('');
        edit.dataset.act = 'screen-edit';
        edit.dataset.station = '10';
        main.appendChild(edit);
        bubble(edit, 'click', doc);
        for (let i = 0; i < 6; i++) await drain();
        if (!scrim.classList.contains('active')) throw new Error('setup: the screen sheet did not open');
        // The engineer takes PLN_02 off the screen with its chip's ×.
        const drop = makeElement('');
        drop.dataset.npk = 'drop';
        drop.dataset.npkkey = 'positions';
        drop.dataset.npkname = 'PLN_02';
        scrim.appendChild(drop);
        bubble(drop, 'click', doc);
        const save = makeElement('');
        save.dataset.act = 'sheet-ok';
        scrim.appendChild(save);
        bubble(save, 'click', doc);
        for (let i = 0; i < 10; i++) await drain();
        if (puts.length !== 1 || JSON.stringify(puts[0]) !== JSON.stringify({ nodes: ['PLN_01'] })) {
            throw new Error('setup: the positions PUT was not sent as expected: ' + JSON.stringify(puts));
        }
        return { open: scrim.classList.contains('active'), status };
    }

    {
        const { open, status } = await saveScreenWithReply({ status: 'ok', kept: ['PLN_02'] });
        if (!open) throw new Error('the sheet closed although the server kept PLN_02 on the screen');
        const want = 'Saved. PLN_02 stays on this screen until its active order finishes.';
        if (status.textContent !== want) {
            throw new Error('status line: got ' + JSON.stringify(status.textContent) + ', want ' + JSON.stringify(want));
        }
        if (status.className !== 'st') {
            throw new Error('a kept position is not a refusal; status class was ' + JSON.stringify(status.className));
        }
        checks++;
        console.log('ok: a kept position keeps the screen sheet open and is named on its status line');
    }

    {
        const { open, status } = await saveScreenWithReply({ status: 'ok', kept: ['PLN_02', 'PLN_05'] });
        const want = 'Saved. PLN_02, PLN_05 stay on this screen until their active orders finish.';
        if (!open || status.textContent !== want) {
            throw new Error('two kept positions: open=' + open + ', status ' + JSON.stringify(status.textContent) +
                ', want ' + JSON.stringify(want));
        }
        checks++;
        console.log('ok: several kept positions are named together in one sentence');
    }

    {
        const { open } = await saveScreenWithReply({ status: 'ok', kept: [] });
        if (open) throw new Error('the sheet stayed open although the server kept nothing');
        checks++;
        console.log('ok: a save that kept nothing closes the screen sheet');
    }

    {
        const { open } = await saveScreenWithReply({ status: 'ok' });
        if (open) throw new Error('the sheet stayed open on a reply without kept');
        checks++;
        console.log('ok: a reply without kept closes the screen sheet as before');
    }

    // ── taking a position off a screen names what it orphans first ──────────
    //
    // Before anything is written, a save that takes positions off the screen
    // asks the server what still names them (POST …/claimed-nodes/impact). A
    // warning is shown and the save waits for a second Save; it is a warning,
    // not a refusal. Nothing to warn about and the save goes straight through.
    async function removeFromScreen(preview, saves) {
        const opts = makeElement('');
        const input = makeElement('');
        const box = makeElement('npk-positions');
        box.dataset.npkbox = 'positions';
        box.querySelector = sel => (sel === '[data-npkq]' ? input
            : sel === '[data-npkpart="opts"]' ? opts : null);
        const status = makeElement('');
        let sheetOpen = false;
        const puts = [];
        let rowPuts = 0;
        const previews = [];
        const ok = body => Promise.resolve({ ok: true, json: async () => body });
        const { main, doc, scrim } = loadPage({
            allowSheet: true,
            stations: [{ id: 10, process_id: 1, name: 'Screen A', enabled: true }],
            stationNodes: { '10': ['PLN_01', 'PLN_02'] },
            extraIds: { 'npk-positions': box },
            docQSA: sel => (sel === '.pd-npk' && sheetOpen ? [box] : []),
            fetch: (url, init) => {
                const method = (init && init.method) || 'GET';
                if (url === '/api/operator-stations/10/claimed-nodes/impact' && method === 'POST') {
                    previews.push(JSON.parse(init.body));
                    return ok(preview);
                }
                if (url === '/api/operator-stations/10/claimed-nodes') {
                    if (method === 'PUT') {
                        puts.push(JSON.parse(init.body));
                        return ok({ status: 'ok', kept: [] });
                    }
                    return ok(['PLN_01', 'PLN_02']);
                }
                if (url === '/api/operator-stations/10' && method === 'PUT') {
                    rowPuts++;
                    return ok({ status: 'ok' });
                }
                if (url === '/api/operator-stations') {
                    return ok([{ id: 10, process_id: 1, name: 'Screen A', enabled: true }]);
                }
                if (url === '/api/core-nodes') {
                    return ok(['PLN_01', 'PLN_02'].map(name => ({ name, node_type: 'PLN' })));
                }
                if (url === '/api/process-nodes') return ok([]);
                return null;
            },
        });
        scrim.querySelector = sel => (sel === '.mf .st' ? status : null);
        await drain();
        await drain();
        sheetOpen = true;
        const edit = makeElement('');
        edit.dataset.act = 'screen-edit';
        edit.dataset.station = '10';
        main.appendChild(edit);
        bubble(edit, 'click', doc);
        for (let i = 0; i < 6; i++) await drain();
        const drop = makeElement('');
        drop.dataset.npk = 'drop';
        drop.dataset.npkkey = 'positions';
        drop.dataset.npkname = 'PLN_02';
        scrim.appendChild(drop);
        bubble(drop, 'click', doc);
        const after = [];
        for (let s = 0; s < saves; s++) {
            const save = makeElement('');
            save.dataset.act = 'sheet-ok';
            scrim.appendChild(save);
            bubble(save, 'click', doc);
            for (let i = 0; i < 10; i++) await drain();
            after.push({ puts: puts.length, rowPuts, status: status.textContent, cls: status.className });
        }
        return { after, previews, open: scrim.classList.contains('active') };
    }

    {
        const warning = 'Removing PLN_02 leaves recipe "Two up" and style "Style A" (the active style) naming ' +
            'a position this process no longer has. Save again to remove it anyway.';
        const { after, previews } = await removeFromScreen({ warning }, 2);
        if (previews.length < 1 || JSON.stringify(previews[0]) !== JSON.stringify({ nodes: ['PLN_01'] })) {
            throw new Error('a save taking PLN_02 off the screen did not ask what names it first: ' + JSON.stringify(previews));
        }
        if (after[0].puts !== 0 || after[0].rowPuts !== 0 || after[0].status !== warning) {
            throw new Error('the first Save wrote before warning, or did not show the warning: ' + JSON.stringify(after[0]));
        }
        if (after[0].cls !== 'st') {
            throw new Error('the warning is not a refusal; status class was ' + JSON.stringify(after[0].cls));
        }
        if (after[1].puts !== 1 || after[1].rowPuts !== 1) {
            throw new Error('a second Save after the warning did not take the position off: ' + JSON.stringify(after[1]));
        }
        checks++;
        console.log('ok: taking off a position a recipe names shows the recipe by name, and a second Save confirms it');
    }

    {
        const { after, open } = await removeFromScreen({ warning: '' }, 1);
        if (after[0].puts !== 1 || after[0].status || open) {
            throw new Error('a position nothing names was not taken off on the first Save, or warned: ' +
                JSON.stringify(after[0]) + ' open=' + open);
        }
        checks++;
        console.log('ok: taking off a position nothing names goes through without a warning');
    }

    // ── selection: a card finds its row, a second click or Escape lets go ───
    //
    // The desktop picture never edits. A card click selects the position and
    // scrolls its table row into view, through the same path as the bar's
    // problem line; clicking the selected card again, or Escape with nothing
    // else open, clears the selection. A real DOM drops and re-parses the
    // tree on every drawFlows; the stub keeps appended elements, so the pin
    // reads the selection back from the markup drawFlows wrote.
    function selectionRig() {
        // The headings' sticky strip, 43 px tall — the height it renders at
        // 1280 with its two-line headings — and the bar, 57. The main column
        // is the scroller both of them sit over.
        const head = makeElement('pd-poshead');
        head.offsetHeight = 43;
        const ctx = loadPage({ extraIds: { 'pd-poshead': head } });
        ctx.bar.offsetHeight = 57;
        const { root, main } = ctx;
        ctx.box = main;
        const svg = makeElement('pd-svg');
        main.appendChild(svg);
        const card = makeElement('');
        card.dataset.pos = 'PLN_01';
        svg.appendChild(card);
        const row = makeElement('');
        row.dataset.row = 'PLN_01';
        main.appendChild(row);
        const rootQuery = root.querySelector;
        root.querySelector = sel => (sel === '[data-row="PLN_01"]' ? row
            : sel === '.pd-main' ? main
                : (rootQuery ? rootQuery.call(root, sel) : null));
        const selected = () => /class="selrow" data-row="PLN_01"/.test(root.innerHTML);
        const escapeKey = () => {
            for (const fn of (ctx.doc.listeners.keydown || [])) fn({ key: 'Escape' });
        };
        return Object.assign(ctx, { card, row, selected, escapeKey });
    }

    {
        const { card, row, box, doc, selected } = selectionRig();
        await drain();
        await drain();
        if (selected()) throw new Error('setup: nothing should be selected at load');
        bubble(card, 'click', doc);
        if (!selected()) throw new Error('a card click did not select its position\'s row');
        if (row.lastScroll === undefined || row.lastScroll.block !== 'nearest') {
            throw new Error('a card click did not scroll its row into view (nearest); got ' +
                JSON.stringify(row.lastScroll));
        }
        // The headings stick to the top of the main column and the bar to its
        // foot, so a row brought into view at either edge of the column would
        // land under one of them: the column reserves both heights before the
        // row is scrolled to.
        if (box.style.scrollPaddingTop !== '43px' || box.style.scrollPaddingBottom !== '57px') {
            throw new Error('a card click scrolled its row without reserving the 43 px sticky header ' +
                'and the 57 px bar on the main column; scroll-padding was ' +
                JSON.stringify([box.style.scrollPaddingTop, box.style.scrollPaddingBottom]));
        }
        bubble(card, 'click', doc);
        if (selected()) throw new Error('a second click on the selected card did not deselect it');
        checks++;
        console.log('ok: a card click selects and scrolls its row into view below the header; a second click lets go');
    }

    {
        const { row, doc, selected } = selectionRig();
        await drain();
        await drain();
        bubble(row, 'click', doc);
        if (!selected()) throw new Error('a row click did not select the row');
        bubble(row, 'click', doc);
        if (selected()) throw new Error('a second click on the selected row did not deselect it');
        checks++;
        console.log('ok: a second click on the selected row lets go');
    }

    {
        const { card, main, pop, doc, selected, escapeKey } = selectionRig();
        await drain();
        await drain();
        bubble(card, 'click', doc);
        if (!selected()) throw new Error('setup: the card click should have selected');
        // The popover is innermost: Escape closes it and keeps the selection.
        const btn = addUsePresetButton(main);
        bubble(btn, 'click', doc);
        if (pop.hidden !== false) throw new Error('setup: the popover should be open here');
        escapeKey();
        if (pop.hidden !== true) throw new Error('Escape did not close the popover first');
        if (!selected()) throw new Error('the Escape that closed the popover also dropped the selection');
        escapeKey();
        if (selected()) throw new Error('Escape with nothing else open did not deselect');
        checks++;
        console.log('ok: Escape closes the popover first, then lets go of the selection');
    }

    // ── the frame takes the picture's own height ────────────────────────────
    //
    // The picture lays out at the frame's width and reports how tall that
    // made it. The svg is that tall and the viewBox says the same, so the
    // drawing is 1:1 — never a fixed band the picture is squeezed into.
    {
        const { pic, svg } = pictureFrame(600);
        const { root, win } = loadPage({ composer: TALL_COMPOSER, extraIds: { 'pd-svg': svg }, realPicture: true });
        await drain();
        await drain();
        const call = win.pictureCalls[win.pictureCalls.length - 1];
        if (!call) throw new Error('setup: the page never drew the picture');
        const h = call.height;
        if (!(h > 430)) throw new Error('setup: the fixture should wrap past a 430 px band; the picture reported ' + h);
        if (!call.frame || call.frame.w !== pic.clientWidth) {
            throw new Error('the picture was laid out at ' + JSON.stringify(call.frame) +
                ', not the frame\'s ' + pic.clientWidth + ' px width');
        }
        if (svg.attrs.viewBox !== '0 0 600 ' + h) {
            throw new Error('the viewBox is "' + svg.attrs.viewBox + '" — want "0 0 600 ' + h +
                '", the height the picture reported');
        }
        if (svg.attrs.height !== String(h) || svg.attrs.width !== '600') {
            throw new Error('the svg is ' + svg.attrs.width + 'x' + svg.attrs.height +
                ' — want 600x' + h + ', the picture\'s own size');
        }
        if (/pd-legend/.test(root.innerHTML)) {
            throw new Error('the page still draws a fixed two-robot legend beside the picture\'s own');
        }
        if (!/class="lg"/.test(svg.innerHTML)) throw new Error('the picture drew no legend of its own');
        checks++;
        console.log('ok: the svg is the size the picture reported, and the picture carries the only legend');
    }

    // ── a selection is the module's, and so is the second click ─────────────
    //
    // Selecting the press outlines its whole module, the on-deck card
    // included, so a click on that on-deck card is a click on the selected
    // thing: it lets go rather than moving the selection inside the module.
    {
        const { pic, svg } = pictureFrame(600);
        const { doc, main, win } = loadPage({ composer: TALL_COMPOSER, extraIds: { 'pd-svg': svg }, realPicture: true });
        main.appendChild(pic);   // the click bubbles svg → frame → column → #pd-root
        await drain();
        await drain();
        const card = name => {
            const c = makeElement('');
            c.dataset.pos = name;
            svg.appendChild(c);
            return c;
        };
        const press = card('PLN_05'), deck = card('PLN_06'), other = card('PLN_01');
        // What the picture was last asked to outline. The on-deck position
        // runs no claim and so has no table row; the picture is where its
        // selection shows.
        const selected = () => win.pictureCalls[win.pictureCalls.length - 1].selected || '';
        bubble(press, 'click', doc);
        if (selected() !== 'PLN_05') throw new Error('setup: a click on the press card should select it, got "' + selected() + '"');
        bubble(deck, 'click', doc);
        if (selected()) {
            throw new Error('a click on the on-deck card of the selected module moved the selection to "' +
                selected() + '" instead of letting go');
        }
        bubble(deck, 'click', doc);
        if (selected() !== 'PLN_06') throw new Error('with nothing selected, the on-deck card should select itself');
        bubble(other, 'click', doc);
        if (selected() !== 'PLN_01') throw new Error('a click on another module\'s card should move the selection to it');
        checks++;
        console.log('ok: a second click anywhere in the selected module lets go; another module takes the selection');
    }

    // ── a redraw keeps where the engineer was scrolled ───────────────────────
    {
        const { root, main, doc } = loadPage();
        await drain();
        await drain();
        // A real innerHTML write replaces the column, the table's box and its
        // headings strip with fresh elements scrolled to the origin. The stub
        // mints a fresh set per write. The column scrolls down; the table's
        // box scrolls only sideways, and its headings follow it.
        let gen = 0;
        const fresh = {};
        const scrolled = ['.pd-main', '#pd-postbl', '#pd-poshead'];
        const at = sel => {
            const k = gen + sel;
            if (!fresh[k]) fresh[k] = Object.assign(makeElement(''), { scrollTop: 0, scrollLeft: 0 });
            return fresh[k];
        };
        let html = root.innerHTML;
        Object.defineProperty(root, 'innerHTML', {
            get: () => html,
            set: v => { html = v; gen++; },
        });
        const rootQuery = root.querySelector;
        root.querySelector = sel => (scrolled.indexOf(sel) >= 0 ? at(sel)
            : (rootQuery ? rootQuery.call(root, sel) : null));
        at('.pd-main').scrollTop = 140;
        at('#pd-postbl').scrollLeft = 60;
        at('#pd-poshead').scrollLeft = 60;
        const row = makeElement('');
        row.dataset.row = 'PLN_01';
        main.appendChild(row);
        const before = gen;
        bubble(row, 'click', doc);
        if (gen === before) throw new Error('setup: the row click did not redraw');
        const got = [at('.pd-main').scrollTop, at('#pd-postbl').scrollLeft, at('#pd-poshead').scrollLeft];
        if (got.join() !== '140,60,60') {
            throw new Error('drawFlows lost the scroll: column top, table left, headings left = ' +
                got.join(', ') + ' (want 140, 60, 60)');
        }
        checks++;
        console.log('ok: a redraw of the flow keeps the column and the table where they were scrolled');
    }

    // ── the headings scroll sideways with the rows ───────────────────────────
    //
    // The headings are their own strip so they can stick to the main column,
    // which leaves the table's box scrolling sideways under them alone. A
    // scroll of the box carries the strip with it, or the headings sit over
    // the wrong columns.
    {
        const head = makeElement('pd-poshead');
        head.scrollLeft = 0;
        const { doc } = loadPage({ extraIds: { 'pd-poshead': head } });
        await drain();
        await drain();
        const box = Object.assign(makeElement('pd-postbl'), { scrollLeft: 120 });
        const other = Object.assign(makeElement('pd-rail-body'), { scrollLeft: 30 });
        for (const fn of (doc.listeners.scroll || [])) fn({ target: other });
        if (head.scrollLeft !== 0) throw new Error('a scroll elsewhere moved the headings to ' + head.scrollLeft);
        for (const fn of (doc.listeners.scroll || [])) fn({ target: box });
        if (head.scrollLeft !== 120) {
            throw new Error('the table scrolled sideways to 120 and its headings stayed at ' + head.scrollLeft);
        }
        checks++;
        console.log('ok: the headings strip follows the table\'s sideways scroll');
    }

    // ── the headings are a strip outside the table's sideways box ────────────
    {
        const { root } = loadPage();
        await drain();
        await drain();
        const html = root.innerHTML;
        const strip = html.indexOf('id="pd-poshead"');
        const box = html.indexOf('id="pd-postbl"');
        if (strip < 0 || box < 0 || strip > box) {
            throw new Error('the positions box has no headings strip ahead of its table box');
        }
        const stripHTML = html.slice(strip, box);
        const boxHTML = html.slice(box, html.indexOf('</table>', box));
        if (stripHTML.indexOf('<thead>') < 0 || stripHTML.indexOf('<colgroup>') < 0) {
            throw new Error('the headings strip carries no thead and colgroup of its own');
        }
        if (boxHTML.indexOf('<thead>') >= 0) {
            throw new Error('the table box still draws its own thead; the headings would show twice');
        }
        checks++;
        console.log('ok: the headings are a strip of their own, with the same columns, ahead of the rows');
    }

    // ── one phrase for adding: "Add a position" ──────────────────────────────
    {
        const { root, pop, main, doc } = loadPage({
            composer: {
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
                presets: [],
                palette: ['PART-A'],
                cell: {
                    positions: [
                        { core_node_name: 'PLN_01', kind: 'front', sequence: 1 },
                        { core_node_name: 'PLN_02', kind: 'front', sequence: 2 },
                        { core_node_name: 'PLN_03', kind: 'front', sequence: 3 },
                    ],
                    groups: {},
                },
            },
        });
        await drain();
        await drain();
        const hint = '<span class="hint">click a card to find its row · click a cell to change it</span>';
        if (root.innerHTML.indexOf(hint) < 0) {
            throw new Error('the table hint does not read "click a card to find its row · click a cell to change it"');
        }
        const add = makeElement('');
        add.classList.add('pd-btn');
        add.dataset.act = 'add-position';
        main.appendChild(add);
        bubble(add, 'click', doc);
        if (pop.hidden !== false) throw new Error('setup: the free-position menu should be open');
        if (pop.innerHTML.indexOf('<div class="pd-lbl">Add a position</div>') !== 0) {
            throw new Error('the free-position menu is not headed "Add a position": ' + pop.innerHTML.slice(0, 120));
        }
        checks++;
        console.log('ok: the hint says what a card click does, and the add menu says "Add a position"');
    }

    {
        const { doc } = loadPage({
            composer: {
                styles: [{ id: 1, name: 'Style A', claims: [], parts: [], advanced: {} }],
                routing: [],
                presets: [],
                palette: [],
                cell: { positions: [{ core_node_name: 'PLN_01', kind: 'front', sequence: 1 }], groups: {} },
            },
        });
        await drain();
        await drain();
        const bar = doc.getElementById('pd-bar');
        if (!/Nothing in the flow yet/.test(bar.innerHTML)) {
            throw new Error('setup: the bar should read empty here; bar: ' + bar.innerHTML.slice(0, 200));
        }
        if (bar.innerHTML.indexOf('<div class="d">Add a position below, or use a preset</div>') < 0) {
            throw new Error('the desktop\'s empty bar does not read "Add a position below, or use a preset"; bar: ' +
                bar.innerHTML.slice(0, 300));
        }
        checks++;
        console.log('ok: the desktop\'s empty bar names its own way to add a position');
    }

    // ── the editor's dead ends: a first row, a recipe menu with no shapes ───
    //
    // wireButtons stubs the pop's querySelectorAll for any [data-x] list the
    // page binds, handing back one element per data-x="v" the page wrote, so a
    // click on one is a real click on what the page drew.
    function wireButtons(pop) {
        const made = {};
        pop.querySelectorAll = sel => {
            const m = /^\[data-([a-z-]+)\]$/.exec(sel);
            if (!m) return [];
            const re = new RegExp('data-' + m[1] + '="([^"]*)"', 'g');
            const out = [];
            let x;
            while ((x = re.exec(pop.innerHTML))) {
                const b = makeElement('');
                b.dataset[attrName('data-' + m[1])] = x[1];
                pop.appendChild(b);
                out.push(b);
                made[m[1] + '=' + x[1]] = b;
            }
            return out;
        };
        return made;
    }
    // The owner's press on 2026-10-26: no recipes, a style with no flow, and a
    // routing set with no source and no destination, so the generator offers
    // nothing either.
    const firstRowComposer = () => ({
        styles: [{ id: 1, name: 'Style A', claims: [], parts: [], advanced: {} }],
        routing: [],
        presets: [],
        palette: [],
        cell: {
            positions: [
                { core_node_name: 'PLN_01', kind: 'front', sequence: 1 },
                { core_node_name: 'PLN_02', kind: 'front', sequence: 2 },
            ],
            groups: {},
        },
    });

    // A style with no rows has no choreography to borrow, so "+ Add a
    // position" asks for one, then for the position: the first row comes from
    // the desktop, not from a notice sending the engineer elsewhere. Nothing
    // here reads the routing set or the generator; the missing source and
    // destination are the row's to fill.
    {
        const { root, pop, main, doc } = loadPage({ composer: firstRowComposer() });
        await drain();
        await drain();
        if (root.innerHTML.indexOf('<button class="pd-btn" data-act="add-position">+ Add a position</button>') < 0) {
            throw new Error('setup: a style with no rows does not draw "+ Add a position"');
        }
        const made = wireButtons(pop);
        const add = makeElement('');
        add.classList.add('pd-btn');
        add.dataset.act = 'add-position';
        main.appendChild(add);
        bubble(add, 'click', doc);
        if (pop.hidden !== false || pop.innerHTML.indexOf('data-freemode="single_robot"') < 0) {
            throw new Error('a style with no rows: "+ Add a position" offered no choreography to start from; pop ' +
                JSON.stringify(pop.innerHTML.slice(0, 160)));
        }
        bubble(made['freemode=single_robot'], 'click', doc);
        if (pop.hidden !== false || !made['freepos=PLN_02']) {
            throw new Error('after the choreography, the free positions were not offered: ' + pop.innerHTML.slice(0, 200));
        }
        bubble(made['freepos=PLN_02'], 'click', doc);
        await drain();
        if (root.innerHTML.indexOf('data-row="PLN_02"') < 0) {
            throw new Error('a style with no rows did not gain PLN_02 as its first row');
        }
        if (root.innerHTML.indexOf('This style has no spots yet') >= 0) {
            throw new Error('the dead-end notice is still drawn');
        }
        // The source and destination the routing set could not fill are the
        // row's own pickers, and the bar flags the row: work to finish, not a
        // dead end.
        for (const kind of ['source', 'dest']) {
            if (root.innerHTML.indexOf('data-node="PLN_02" data-kind="' + kind + '"') < 0) {
                throw new Error('the first row has no ' + kind + ' picker to fill');
            }
        }
        const bar = doc.getElementById('pd-bar').innerHTML;
        if (bar.indexOf('data-node="PLN_02"') < 0 || bar.indexOf('class="h bad"') < 0) {
            throw new Error('the bar does not flag the new row as work to finish: ' + bar.slice(0, 200));
        }
        checks++;
        console.log('ok: a style with no rows gains its first row from "+ Add a position": a choreography, then a position');
    }

    // ── + New recipe: an empty draft at once, the mode picked at its first row ─
    //
    // Owner, 2026-10-26: start with a fresh recipe and let them build from
    // there. No popover and no generated shapes — an empty draft, on a press
    // whose routing set has no source and no destination. The mode is picked
    // where an empty style picks it, at the first "+ Add a position"; after
    // that the draft's rows have one mode and the button offers only it. The
    // row takes the routing set's defaults as any added row does, and where
    // the set has none the server's sentence (the composer's routing_gaps)
    // stands beside the row's empty destination.
    const gapWhy = "Press 1's routing set names no outbound destination — add one under Routing set";
    const recipeComposer = () => {
        const c = firstRowComposer();
        c.cell.positions.push({ core_node_name: 'PLN_03', kind: 'front', sequence: 3 });
        c.routing_gaps = { outbound_destination: gapWhy };
        return c;
    };
    function newRecipeRig(server) {
        const urls = [];
        const ctx = loadPage({
            flowspec: true,
            composer: recipeComposer(),
            fetch: url => {
                urls.push(url);
                return /\/api\/processes\/1\/composer$/.test(url)
                    ? Promise.resolve({ ok: true, json: async () => JSON.parse(JSON.stringify(server())) })
                    : null;
            },
        });
        ctx.urls = urls;
        ctx.made = wireButtons(ctx.pop);
        ctx.act = (name, data) => {
            const b = makeElement('');
            if (name === 'add-position' || name === 'new-recipe') b.classList.add('pd-btn');
            b.dataset.act = name;
            Object.assign(b.dataset, data || {});
            ctx.main.appendChild(b);
            bubble(b, 'click', ctx.doc);
        };
        return ctx;
    }

    {
        const ctx = newRecipeRig(recipeComposer);
        const { root, pop, made, act } = ctx;
        await drain();
        await drain();
        ctx.urls.length = 0;
        act('new-recipe');
        for (let i = 0; i < 4; i++) await drain();
        if (pop.hidden === false) {
            throw new Error('+ New recipe opened a popover: ' + JSON.stringify(pop.innerHTML.slice(0, 200)));
        }
        // NOTHING PREFILLS: no rows, no mode, no notes, and no read of anything
        // that could propose one — the composer alone, for the routing set.
        if (root.innerHTML.indexOf('<h1>New recipe</h1>') < 0 || root.innerHTML.indexOf('data-row=') >= 0 ||
            root.innerHTML.indexOf('class="pd-notice"') >= 0) {
            throw new Error('+ New recipe did not open an empty draft titled "New recipe"');
        }
        if (ctx.urls.some(u => !/\/api\/processes\/1\/composer$/.test(u))) {
            throw new Error('+ New recipe read more than the composer: ' + JSON.stringify(ctx.urls));
        }
        act('add-position');
        if (pop.hidden !== false || !made['freemode=two_robot']) {
            throw new Error('the empty draft\'s first "+ Add a position" did not offer the modes: ' + pop.innerHTML.slice(0, 200));
        }
        bubble(made['freemode=two_robot'], 'click', ctx.doc);
        bubble(made['freepos=PLN_01'], 'click', ctx.doc);
        await drain();
        if (root.innerHTML.indexOf('data-row="PLN_01"') < 0 ||
            root.innerHTML.indexOf('<h1>New recipe · 2‑Robot Swap</h1>') < 0 ||
            root.innerHTML.split('data-row=').length !== 2) {
            throw new Error('the first row did not land, alone, in 2-Robot Swap');
        }
        for (const k of Object.keys(made)) delete made[k];
        act('add-position');
        if (pop.hidden !== false || Object.keys(made).some(k => k.indexOf('freemode=') === 0) ||
            !Object.keys(made).some(k => k.indexOf('freepos=') === 0)) {
            throw new Error('the next "+ Add a position" did not go straight to the positions: ' + pop.innerHTML.slice(0, 200));
        }
        checks++;
        console.log('ok: + New recipe opens an empty draft; its first row picks the mode, the next takes it');

        const row = root.innerHTML.slice(root.innerHTML.indexOf('data-row="PLN_01"'));
        const tr = row.slice(0, row.indexOf('</tr>'));
        const at = tr.indexOf('data-kind="dest"');
        const dest = at < 0 ? '' : tr.slice(at, tr.indexOf('</td>', at));
        if (dest.indexOf('<small class="pd-gap">' + gapWhy + '</small>') < 0) {
            throw new Error('the row\'s empty destination does not carry the routing set\'s sentence: ' + tr.slice(0, 1600));
        }
        if (tr.split('pd-gap').length !== 2) {
            throw new Error('the sentence is drawn on a field other than the destination');
        }
        // The save of this row is the server's to refuse
        // (TestPresets_ARecipeRowItsModeCannotRunIsRefusedAtSave): a draft is
        // never previewed, so the bar raises no finding on it.
        checks++;
        console.log('ok: the draft\'s empty destination carries the routing set\'s sentence');
    }

    // No reload: a destination added to the routing set after the page loaded
    // reaches the next draft's first row, and the sentence is gone. The page
    // holds the composer it opened with; the server's now has DN_001.
    {
        let server = recipeComposer();
        const ctx = newRecipeRig(() => server);
        const { root, made, act } = ctx;
        await drain();
        await drain();
        server = recipeComposer();
        delete server.routing_gaps;
        server.routing = [{ core_node_name: 'DN_001', role: 'destination', label: 'DN_001', enabled: true, sequence: 1 }];
        act('new-recipe');
        for (let i = 0; i < 4; i++) await drain();
        act('add-position');
        if (!made['freemode=two_robot']) throw new Error('setup: the empty draft offered no modes');
        bubble(made['freemode=two_robot'], 'click', ctx.doc);
        bubble(made['freepos=PLN_01'], 'click', ctx.doc);
        await drain();
        const row = root.innerHTML.slice(root.innerHTML.indexOf('data-row="PLN_01"'));
        const tr = row.slice(0, row.indexOf('</tr>'));
        if (tr.indexOf('<span class="t">DN_001</span>') < 0 || tr.indexOf('pd-gap') >= 0) {
            throw new Error('a destination added without a reload did not reach the new row: ' + tr.slice(0, 1600));
        }
        checks++;
        console.log('ok: a destination added without a reload reaches the next draft\'s row, and the sentence is gone');
    }

    // A flow whose rows were all taken off is empty again, and is asked again:
    // a recipe draft whose only row went is not bound to the mode it had.
    {
        const ctx = newRecipeRig(recipeComposer);
        const { pop, made, act } = ctx;
        await drain();
        await drain();
        act('new-recipe');
        for (let i = 0; i < 4; i++) await drain();
        act('add-position');
        bubble(made['freemode=single_robot'], 'click', ctx.doc);
        bubble(made['freepos=PLN_01'], 'click', ctx.doc);
        await drain();
        act('rmpos', { node: 'PLN_01' });
        await drain();
        for (const k of Object.keys(made)) delete made[k];
        act('add-position');
        if (pop.hidden !== false || !made['freemode=two_robot']) {
            throw new Error('a draft emptied by its rows\' × did not offer the modes again: ' +
                JSON.stringify(pop.innerHTML.slice(0, 200)));
        }
        checks++;
        console.log('ok: a recipe draft emptied by its rows\' × offers the modes again');
    }

    // The copy path and the generator are gone (owner, 2026-10-26): a recipe
    // starts empty here and is built by hand, so the Recipes tab neither copies
    // one from another cell nor suggests one — not even from a read that still
    // carried generated shapes — and the page names neither path.
    {
        const shape = [{ core_node_name: 'PLN_01', role: 'consume', swap_mode: 'single_robot' }];
        const { root, pop, main, doc } = loadPage({
            composer: firstRowComposer(),
            fetch: url => (/\/api\/processes\/1\/presets$/.test(url)
                ? Promise.resolve({ ok: true, json: async () => ({ presets: [], candidates: [], styles_with_flow: 0,
                    generated: [{ mode: 'single_robot', shape, where: 'PLN_01' }] }) })
                : null),
        });
        await drain();
        await drain();
        const tab = makeElement('');
        tab.dataset.tab = 'presets';
        main.appendChild(tab);
        bubble(tab, 'click', doc);
        for (let i = 0; i < 4; i++) await drain();
        const page = root.innerHTML + pop.innerHTML;
        if (!/<h2>Recipes<\/h2>/.test(page)) throw new Error('setup: the Recipes tab did not draw');
        for (const gone of ['import-from', 'Import from', 'From another cell', 'Suggested', 'shape-draft',
            'start from a suggestion']) {
            if (page.indexOf(gone) >= 0) throw new Error('the Recipes tab still offers a recipe it did not build: ' + gone);
        }
        const src = fs.readFileSync(path.join(__dirname, 'processes-desktop.js'), 'utf8');
        for (const id of ['openImportFrom', 'openImportProcess', 'importPreset', 'openShapeDraft', "'shape-draft'",
            'h.import', 'h.draft', '.generated']) {
            if (src.indexOf(id) >= 0) throw new Error('the page still names a retired path: ' + id);
        }
        checks++;
        console.log('ok: no door copies or suggests a recipe, and the page names neither path');
    }


    // ── the positions table: role is a column, a value reads first, staging
    //    says what it needs and who else is parked there ─────────────────────
    //
    // The pop's option buttons are bound by querySelectorAll('[data-opt]').
    // The stub hands back one element per data-opt the page actually wrote,
    // so an option click is a real click on what the page drew.
    function wireOptions(pop) {
        const opts = [];
        pop.querySelectorAll = sel => {
            if (sel !== '[data-opt]') return [];
            opts.length = 0;
            const re = /data-opt="(\d+)"/g;
            let m;
            while ((m = re.exec(pop.innerHTML))) {
                const b = makeElement('');
                b.dataset.opt = m[1];
                pop.appendChild(b);
                opts.push(b);
            }
            return opts.slice();
        };
        return opts;
    }
    function chip(main, node, kind) {
        const b = makeElement('');
        b.classList.add('pd-sel');
        b.dataset.act = 'pick';
        b.dataset.node = node;
        b.dataset.kind = kind;
        main.appendChild(b);
        return b;
    }
    const tableComposer = (claims, staging) => ({
        styles: [{ id: 1, name: 'Style A', claims, parts: ['PART-A'], advanced: {} }],
        routing: [
            { core_node_name: 'SMN_010', role: 'source', label: 'SMN_010', enabled: true, sequence: 1 },
            { core_node_name: 'DN_001', role: 'destination', label: 'DN_001', enabled: true, sequence: 2 },
        ].concat(staging.map((n, i) => ({ core_node_name: n, role: 'staging', label: n, enabled: true, sequence: 3 + i }))),
        presets: [],
        palette: ['PART-A'],
        cell: {
            positions: [
                { core_node_name: 'PLN_01', kind: 'front', sequence: 1 },
                { core_node_name: 'PLN_02', kind: 'front', sequence: 2 },
            ],
            groups: {},
        },
    });
    // A field's word as the page draws it: read from the flowspec table the
    // page loaded, so a relabelled table does not break a pin about layout.
    const label = (win, field) => {
        const w = win.FLOWSPEC && win.FLOWSPEC.labels && win.FLOWSPEC.labels[field];
        if (!w) throw new Error('setup: the flowspec table has no label for ' + field);
        return w;
    };
    // The staging cells as drawn, for a failure message.
    const staging = root => (root.innerHTML.match(/data-kind="col:staging[^>]*>.*?<\/button>/g) || []).join('\n      ');
    const claim = (node, extra) => Object.assign({
        core_node_name: node, swap_mode: 'single_robot', payload_code: 'PART-A',
        inbound_source: 'SMN_010', outbound_destination: 'DN_001',
    }, extra || {});

    {
        const { root, pop, main, doc } = loadPage({ flowspec: true, composer: tableComposer([claim('PLN_01')], []) });
        await drain();
        await drain();
        const opts = wireOptions(pop);
        if (root.innerHTML.indexOf('<th>Position</th><th>Role</th>') < 0) {
            throw new Error('the positions table has no Role heading after Position');
        }
        if (root.innerHTML.indexOf('data-act="set-role"') >= 0) {
            throw new Error('the role is still the pill under the position name');
        }
        const roleChip = '<button class="pd-sel" data-act="pick" data-node="PLN_01" data-kind="role" ' +
            'title="Role consume"><span class="t">consume</span><i class="car"></i></button>';
        if (root.innerHTML.indexOf(roleChip) < 0) {
            throw new Error('the Role cell is not a "consume" chip with a caret');
        }
        bubble(chip(main, 'PLN_01', 'role'), 'click', doc);
        if (pop.hidden !== false) throw new Error('a click on the role chip did not open its menu');
        if (pop.innerHTML !== '<button data-opt="0" class="on">consume</button><button data-opt="1" class="">produce</button>') {
            throw new Error('the role menu is not consume (on) / produce: ' + pop.innerHTML);
        }
        bubble(opts[1], 'click', doc);
        if (root.innerHTML.indexOf('title="Role produce"><span class="t">produce</span>') < 0) {
            throw new Error('picking produce did not set the role');
        }
        checks++;
        console.log('ok: the role is a chip in its own Role column, and its menu sets it');
    }

    {
        const { root, win } = loadPage({
            flowspec: true,
            composer: tableComposer([claim('PLN_01', { inbound_staging: 'SLN_010' }), claim('PLN_02')], ['SLN_010']),
        });
        await drain();
        await drain();
        const word = label(win, 'inbound_staging');
        const set = 'data-node="PLN_01" data-kind="col:staging:staging" title="' + word + ' SLN_010">' +
            '<span class="t">SLN_010</span>';
        if (root.innerHTML.indexOf(set) < 0) {
            throw new Error('a set staging chip does not read its value alone ("SLN_010"): ' + staging(root));
        }
        const empty = 'data-node="PLN_02" data-kind="col:staging:staging" title="' + word + ' not set">' +
            '<span class="t"><span class="k">' + word + '</span> —</span>';
        if (root.innerHTML.indexOf(empty) < 0) {
            throw new Error('an empty required staging chip does not read "' + word + ' —": ' + staging(root));
        }
        checks++;
        console.log('ok: a set chip reads its value; an empty one reads its label and a dash');
    }

    {
        const { root, pop, main, doc, win } = loadPage({
            flowspec: true,
            composer: tableComposer([claim('PLN_01'), claim('PLN_02', { inbound_staging: 'SLN_010' })],
                ['SLN_010', 'SLN_011']),
        });
        await drain();
        await drain();
        const opts = wireOptions(pop);
        bubble(chip(main, 'PLN_01', 'col:staging:staging'), 'click', doc);
        if (pop.hidden !== false) throw new Error('a click on the staging chip did not open its menu');
        const want = '<div class="none">required for 1‑Robot Swap</div>' +
            '<button data-opt="1" class="">SLN_010<span class="use"> · used by PLN_02</span></button>' +
            '<button data-opt="2" class="">SLN_011</button>';
        if (pop.innerHTML !== want) {
            throw new Error('the staging menu does not say "required" and mark the shared node:\n      got  ' +
                pop.innerHTML + '\n      want ' + want);
        }
        // Shared staging is a warning, never a refusal: the marked node is
        // still a choice, and choosing it sets it.
        bubble(opts[0], 'click', doc);
        if (root.innerHTML.indexOf('data-node="PLN_01" data-kind="col:staging:staging" title="' + label(win, 'inbound_staging') + ' SLN_010">') < 0) {
            throw new Error('picking the shared staging node did not set it');
        }
        checks++;
        console.log('ok: the staging menu says "required for 1‑Robot Swap" and marks a node another position uses');
    }

    // A STAGING FIELD THE MODE FORBIDS DOES NOT USE THE NODE. A two-robot swap
    // can still carry an outbound staging value, but flowspec forbids that
    // field for the mode: Robot 2 takes the old bin to the dock. The picture
    // lists such a node as not used by this flow, so the menu must not call it
    // "used by" that position. The inbound staging the same claim requires
    // still counts.
    {
        const { pop, main, doc } = loadPage({
            flowspec: true,
            composer: tableComposer([claim('PLN_01'), claim('PLN_02', {
                swap_mode: 'two_robot', inbound_staging: 'SLN_010', outbound_staging: 'SLN_011',
            })], ['SLN_010', 'SLN_011']),
        });
        await drain();
        await drain();
        bubble(chip(main, 'PLN_01', 'col:staging:staging'), 'click', doc);
        if (pop.hidden !== false) throw new Error('a click on the staging chip did not open its menu');
        const want = '<div class="none">required for 1‑Robot Swap</div>' +
            '<button data-opt="1" class="">SLN_010<span class="use"> · used by PLN_02</span></button>' +
            '<button data-opt="2" class="">SLN_011</button>';
        if (pop.innerHTML !== want) {
            throw new Error('the staging menu counts a field the mode forbids as a use:\n      got  ' +
                pop.innerHTML + '\n      want ' + want);
        }
        checks++;
        console.log('ok: a two-robot claim\'s forbidden outbound staging does not mark its node "used by"');
    }

    // THE FOOT SAYS WHAT EACH ROWLESS POSITION IS FOR. A position another row
    // draws has no row of its own, and the foot named them all "paired above"
    // — but a position set as another module's inbound staging is not paired
    // with anything. Each is named by its role and the row that sets it.
    {
        const c = tableComposer([
            claim('PLN_01', { swap_mode: 'two_robot_press_index', paired_core_node: 'PLN_02' }),
            claim('PLN_03', { swap_mode: 'two_robot', inbound_staging: 'PLN_04' }),
        ], []);
        c.cell.positions = [
            { core_node_name: 'PLN_01', kind: 'front', sequence: 1 },
            { core_node_name: 'PLN_02', kind: 'back', sequence: 2 },
            { core_node_name: 'PLN_03', kind: 'front', sequence: 3 },
            { core_node_name: 'PLN_04', kind: 'back', sequence: 4 },
        ];
        const { root } = loadPage({ flowspec: true, composer: c });
        await drain();
        await drain();
        const want = '<tr class="paired"><td colspan="10">PLN_02 is on deck for PLN_01 · ' +
            'PLN_04 is inbound staging for PLN_03 — no row of their own</td></tr>';
        if (root.innerHTML.indexOf(want) < 0) {
            throw new Error('the foot does not name each rowless position by its role:\n      got  ' +
                ((root.innerHTML.match(/<tr class="paired">.*?<\/tr>/) || ['(no foot)'])[0]) + '\n      want ' + want);
        }
        checks++;
        console.log('ok: the foot says which position is on deck and which is staging, for which row');
    }

    // ── a save says it saved, and the subtitle reads the draft ──────────────
    //
    // Each of the five writes ends in a sentence that starts "Saved", in the
    // place the engineer is looking: the flow's own bar, the settings notice,
    // or a notice on the tab the write was made from. The flow's subtitle is
    // the draft's, so a mode changed in the table is the mode the head names.
    async function pin(name, fn) {
        try { await fn(); checks++; console.log('ok: ' + name); } catch (e) {
            failures++;
            console.error('FAIL  ' + name + '\n      ' + (e && e.message));
        }
    }
    const okJSON = body => Promise.resolve({ ok: true, status: 200, json: async () => body });
    const savedComposer = () => ({
        styles: [{
            id: 1, name: 'Style A',
            claims: [{
                core_node_name: 'PLN_01', swap_mode: 'single_robot', payload_code: 'PART-A',
                inbound_source: 'SMN_010', outbound_destination: 'DN_001',
            }],
            claim_modes: ['single_robot'], claim_count: 1, last_run: '09-12',
            parts: ['PART-A'], advanced: {},
        }],
        routing: [
            { core_node_name: 'SMN_010', role: 'source', label: 'SMN_010', enabled: true, sequence: 1 },
            { core_node_name: 'DN_001', role: 'destination', label: 'DN_001', enabled: true, sequence: 2 },
        ],
        presets: [],
        palette: ['PART-A'],
        cell: { positions: [{ core_node_name: 'PLN_01', kind: 'front', sequence: 1 }], groups: {} },
    });
    // A sheet's text field and status line, as the page reads them off the scrim.
    function wireSheet(scrim, value) {
        const input = makeElement('');
        input.value = value;
        const status = makeElement('');
        scrim.querySelector = sel => (sel === '[data-f="name"]' ? input
            : sel === '.mf .st' ? status : null);
        return status;
    }
    function click(parent, doc, data) {
        const b = makeElement('');
        Object.assign(b.dataset, data);
        parent.appendChild(b);
        bubble(b, 'click', doc);
        return b;
    }
    const settle = async () => { for (let i = 0; i < 12; i++) await drain(); };
    // Picks "2‑Robot Swap" off the mode chip's own menu, by real clicks.
    function pickTwoRobot(main, pop, doc) {
        const opts = wireOptions(pop);
        bubble(chip(main, 'PLN_01', 'mode'), 'click', doc);
        const two = pop.innerHTML.match(/data-opt="(\d+)"(?:(?!<\/button>).)*2‑Robot Swap<\/button>/);
        if (!two) throw new Error('setup: the mode menu has no 2‑Robot Swap: ' + pop.innerHTML);
        bubble(opts.find(b => b.dataset.opt === two[1]), 'click', doc);
    }

    await pin('the subtitle names the draft\'s mode, not the saved one', async () => {
        const { root, pop, main, doc } = loadPage({ flowspec: true, composer: savedComposer() });
        await settle();
        const before = '<div class="sub">1‑Robot Swap · 1 position · last run 09-12';
        if (root.innerHTML.indexOf(before) < 0) throw new Error('setup: the subtitle is not the saved flow\'s');
        pickTwoRobot(main, pop, doc);
        const want = '<div class="sub">2‑Robot Swap · 1 position · last run 09-12';
        if (root.innerHTML.indexOf(want) < 0) {
            const got = (root.innerHTML.match(/<div class="sub">[^<]*/) || [''])[0];
            throw new Error('the subtitle does not follow the draft: got ' + JSON.stringify(got) +
                ', want ' + JSON.stringify(want));
        }
    });

    await pin('Save flow leaves "Saved 14:05" on the bar', async () => {
        const RealDate = Date;
        global.Date = class extends RealDate {
            constructor(...a) { if (a.length) super(...a); else super(2026, 9, 6, 14, 5, 30); }
        };
        try {
            const bar = makeElement('pd-bar');
            const saves = [];
            const { pop, main, doc } = loadPage({
                flowspec: true, composer: savedComposer(), extraIds: { 'pd-bar': bar },
                fetch: (url, init) => {
                    if (/\/flow\/save$/.test(url)) { saves.push(init.body); return okJSON({ status: 'ok' }); }
                    return null;
                },
            });
            await settle();
            pickTwoRobot(main, pop, doc);
            if (bar.innerHTML.indexOf('>Unsaved changes<') < 0) throw new Error('setup: the edit did not dirty the bar: ' + bar.innerHTML);
            click(main, doc, { act: 'save' });
            await settle();
            if (saves.length !== 1) throw new Error('setup: Save flow did not post once: ' + saves.length);
            const want = '<div class="prov">Saved 14:05</div>';
            if (bar.innerHTML.indexOf(want) < 0) {
                const got = (bar.innerHTML.match(/<div class="prov[^"]*">[^<]*<\/div>/) || [''])[0];
                throw new Error('after a save the bar reads ' + JSON.stringify(got) + ', want ' + JSON.stringify(want));
            }
        } finally {
            global.Date = RealDate;
        }
    });

    await pin('Save settings says "Saved."', async () => {
        const puts = [];
        const { root, doc } = loadPage({
            hash: '#tab=settings',
            fetch: (url, init) => {
                if (url === '/api/processes/1' && init && init.method === 'PUT') {
                    puts.push(init.body);
                    return okJSON({ status: 'ok' });
                }
                return null;
            },
        });
        await settle();
        click(root, doc, { act: 'st-save' });
        await settle();
        if (puts.length !== 1) throw new Error('setup: Save settings did not PUT once: ' + puts.length);
        const want = '<div class="pd-notice">Saved.</div>';
        if (root.innerHTML.indexOf(want) < 0) throw new Error('Save settings drew no ' + want);
    });

    await pin('Save as recipe names the recipe it saved and where it is', async () => {
        const posts = [];
        const { root, main, doc, scrim } = loadPage({
            allowSheet: true, composer: savedComposer(),
            fetch: (url, init) => {
                if (url === '/api/processes/1/presets' && init && init.method === 'POST') {
                    posts.push(init.body);
                    return okJSON({ id: 9 });
                }
                return null;
            },
        });
        await settle();
        click(main, doc, { act: 'save-preset' });
        if (!scrim.classList.contains('active')) throw new Error('setup: the naming sheet did not open');
        wireSheet(scrim, 'Single up');
        click(scrim, doc, { act: 'sheet-ok' });
        await settle();
        if (posts.length !== 1) throw new Error('setup: the preset was not posted once: ' + posts.length);
        if (scrim.classList.contains('active')) throw new Error('the naming sheet is still open after the save');
        const want = '<div class="pd-notice">Saved recipe Single up. It is in the rail under Recipes.</div>';
        if (root.innerHTML.indexOf(want) < 0) throw new Error('the Flows tab drew no ' + want);
    });

    await pin('Clone names the style it saved', async () => {
        const posts = [];
        const { root, main, doc, scrim } = loadPage({
            allowSheet: true, composer: savedComposer(),
            fetch: (url, init) => {
                if (url === '/api/styles/1/clone' && init && init.method === 'POST') {
                    posts.push(init.body);
                    return okJSON({ id: 2 });
                }
                return null;
            },
        });
        await settle();
        click(main, doc, { act: 'style-clone', style: '1' });
        if (!scrim.classList.contains('active')) throw new Error('setup: the clone sheet did not open');
        wireSheet(scrim, 'Style B');
        click(scrim, doc, { act: 'sheet-ok' });
        await settle();
        if (posts.length !== 1) throw new Error('setup: the clone was not posted once: ' + posts.length);
        const want = '<div class="pd-notice">Saved Style B, a copy of Style A.</div>';
        if (root.innerHTML.indexOf(want) < 0) throw new Error('the Flows tab drew no ' + want);
    });

    async function applyPreset(saveStatus) {
        const saves = [];
        const shape = [{
            core_node_name: 'PLN_01', swap_mode: 'single_robot',
            inbound_source: 'SMN_010', outbound_destination: 'DN_001',
        }];
        const view = { presets: [{ id: 7, name: 'Single up', version: 1, shape, members: [] }], candidates: [] };
        const { root, doc, scrim } = loadPage({
            allowSheet: true, composer: savedComposer(), hash: '#tab=presets;apply=7;tick=1',
            fetch: (url, init) => {
                if (url === '/api/processes/1/presets') return okJSON(view);
                if (/\/flow\/preview$/.test(url)) return okJSON({ fingerprint: 'fp-1', findings: [], order_count: 0 });
                if (/\/flow\/save$/.test(url)) {
                    saves.push(init.body);
                    return Promise.resolve({
                        ok: saveStatus < 300, status: saveStatus,
                        json: async () => (saveStatus < 300 ? { status: 'ok' } : { error: 'refused here' }),
                    });
                }
                return null;
            },
        });
        await settle();
        if (!scrim.classList.contains('active')) throw new Error('setup: the apply sheet did not open');
        if (!/Save to 1 part</.test(scrim.innerHTML)) throw new Error('setup: the row is not ready: ' + scrim.innerHTML);
        click(scrim, doc, { act: 'sheet-ok' });
        await settle();
        if (saves.length !== 1) throw new Error('setup: the apply did not save once: ' + saves.length);
        return { root, scrim };
    }

    await pin('a clean apply closes its dialog and says "Saved to 1 part."', async () => {
        const { root, scrim } = await applyPreset(200);
        if (scrim.classList.contains('active')) throw new Error('the apply dialog is still open after every part saved');
        const want = '<div class="pd-notice">Saved to 1 part.</div>';
        if (root.innerHTML.indexOf(want) < 0) throw new Error('the Presets tab drew no ' + want);
    });

    await pin('an apply with a refusal keeps its dialog open', async () => {
        const { root, scrim } = await applyPreset(400);
        if (!scrim.classList.contains('active')) throw new Error('the apply dialog closed over a refused part');
        if (root.innerHTML.indexOf('pd-notice') >= 0) throw new Error('a refused apply drew a success notice');
    });

    // ── a part taken off a saved flow is named to the preview and the save ──
    //
    // The saved flow runs PART-A on PLN_01. The engineer puts PART-B there from
    // the position's part picker, which moves PART-A off it: PART-A is on no
    // position, the bar says so and offers "Take PART-A off this flow". The
    // server compares the draft with the STORED flow, so until the request says
    // the part was taken off on purpose it reads the part as lost and the bar
    // stays shut. The stubbed server here answers exactly that rule.
    async function takeOffSavedPart() {
        const timers = [];
        const previews = [];
        const saves = [];
        const lost = {
            core_node_name: '', side: 'to', field: 'payload_code', severity: 'error',
            message: '1 part needs a position: PART-A',
        };
        const composer = savedComposer();
        composer.palette = ['PART-A', 'PART-B'];
        const page = loadPage({
            composer, timers,
            fetch: (url, init) => {
                if (/\/flow\/preview$/.test(url)) {
                    const body = JSON.parse(init.body);
                    previews.push(body);
                    const off = body.taken_off || [];
                    return okJSON({
                        fingerprint: 'fp-1', order_count: 1, actions: [], unresolved: [],
                        preflight: { state: 'unchecked', missing: [] },
                        findings: off.indexOf('PART-A') >= 0 ? [] : [lost],
                    });
                }
                if (/\/flow\/save$/.test(url)) {
                    saves.push(JSON.parse(init.body));
                    return okJSON({ fingerprint: 'fp-2', written: 1, deleted: 0 });
                }
                if (url === '/api/payload-catalog' || /\/payloads$/.test(url)) return okJSON([]);
                return null;
            },
        });
        const { root, main, pop, bar, doc } = page;
        const saveBtn = makeElement('');
        saveBtn.disabled = true;
        root.querySelector = sel => (sel === '[data-act="save"]' ? saveBtn : null);
        // The part picker's search box, and the popover's onclick as the
        // listener a browser makes of it.
        const q = makeElement('');
        q.focus = () => {};
        pop.querySelector = sel => (sel === '.pd-partq' ? q : null);
        pop.addEventListener('click', ev => pop.onclick && pop.onclick(ev));
        // runPreview, as the page's own schedulePreview queued it.
        const preview = async () => {
            const run = timers.filter(fn => fn.name === 'runPreview').pop();
            timers.length = 0;
            if (!run) throw new Error('setup: no preview was scheduled');
            await run();
            await settle();
        };
        await settle();
        click(main, doc, { act: 'pick', node: 'PLN_01', kind: 'part' });
        await settle();
        click(pop, doc, { pp: 'place', code: 'PART-B' });
        await preview();
        if (!/data-act="rmpart" data-part="PART-A"[^>]*>Take PART-A off this flow</.test(bar.innerHTML)) {
            throw new Error('setup: PART-A is not offered a door off the flow; bar: ' + bar.innerHTML.slice(0, 400));
        }
        if (!saveBtn.disabled) throw new Error('setup: Save is open over a part with no position');
        click(main, doc, { act: 'rmpart', part: 'PART-A' });
        await preview();
        return Object.assign(page, { previews, saves, saveBtn, preview });
    }

    await pin('a part taken off a saved flow: the preview names it, Save opens, the save names it', async () => {
        const { main, doc, bar, previews, saves, saveBtn } = await takeOffSavedPart();
        const body = previews[previews.length - 1];
        assert.deepStrictEqual(Object.keys(body), ['to_style_id', 'cells', 'taken_off'],
            'the preview body gains taken_off and nothing else');
        assert.deepStrictEqual(body.taken_off, ['PART-A'], 'the preview does not name the part taken off');
        if (/to fix|needs a position|PART-A/.test(bar.innerHTML)) {
            throw new Error('the finding outlived the take-off; bar: ' + bar.innerHTML.slice(0, 400));
        }
        if (saveBtn.disabled) throw new Error('Save is still shut after the part was taken off');
        click(main, doc, { act: 'save' });
        await settle();
        if (saves.length !== 1) throw new Error('the save was not posted once: ' + saves.length);
        assert.deepStrictEqual(Object.keys(saves[0]), ['to_style_id', 'cells', 'fingerprint', 'station_id'],
            'the save body carries no taken_off: the preview read it, the save never did');
    });

    await pin('discarding the draft forgets the part taken off', async () => {
        const { main, doc, previews, preview } = await takeOffSavedPart();
        click(main, doc, { act: 'discard' });
        await preview();
        assert.deepStrictEqual(previews[previews.length - 1].taken_off, [],
            'the discarded draft\'s take-off rode into the next preview');
    });

    // ── the Advanced sheet and Presets say what they do ─────────────────────
    //
    // The field's name is the flowspec table's; the help under it is this
    // page's and says part. The reorder point's source pill is drawn only for
    // a source that tells the engineer something: a number nobody stamped is
    // just a number. Presets count the parts a preset was applied to, and say
    // so; a shape whose positions run different modes has a mark of its own.

    const advComposer = src => {
        const c = savedComposer();
        c.styles[0].advanced = { PLN_01: { reorder_point: 5, reorder_point_source: src } };
        return c;
    };

    await pin('Advanced names allowed parts in the help, from the table in the label', async () => {
        const { win, scrim } = loadPage({
            hash: '#style=1;adv=PLN_01', allowSheet: true, flowspec: true, composer: advComposer('legacy'),
        });
        await settle();
        if (!scrim.classList.contains('active')) throw new Error('setup: the advanced sheet did not open');
        const want = '<label>' + label(win, 'allowed_payload_codes') +
            '<small>which parts a robot may bring to this position</small></label>';
        if (scrim.innerHTML.indexOf(want) < 0) {
            const got = (scrim.innerHTML.match(/<label>[^<]*<small>which[^<]*<\/small><\/label>/) || [''])[0];
            throw new Error('the allowed-parts row reads ' + JSON.stringify(got) + ', want ' + JSON.stringify(want));
        }
    });

    await pin('a reorder point with no stamped source draws no "legacy" tag', async () => {
        const { scrim } = loadPage({
            hash: '#style=1;adv=PLN_01', allowSheet: true, flowspec: true, composer: advComposer('legacy'),
        });
        await settle();
        if (scrim.innerHTML.indexOf('data-adv="reorder_point"') < 0) throw new Error('setup: no reorder point row: ' + scrim.innerHTML);
        if (scrim.innerHTML.indexOf('pd-src') >= 0) {
            throw new Error('an unstamped reorder point still draws a source tag: ' +
                (scrim.innerHTML.match(/<span class="pd-src[^>]*>[^<]*<\/span>/) || [''])[0]);
        }
    });

    await pin('a typed reorder point says it was typed', async () => {
        const { scrim } = loadPage({
            hash: '#style=1;adv=PLN_01', allowSheet: true, flowspec: true, composer: advComposer('manual'),
        });
        await settle();
        const want = '<span class="pd-src" title="typed in by an engineer, not calculated">typed</span>';
        if (scrim.innerHTML.indexOf(want) < 0) {
            throw new Error('the reorder point source reads ' +
                JSON.stringify((scrim.innerHTML.match(/<span class="pd-src[^>]*>[^<]*<\/span>/) || [''])[0]) +
                ', want ' + JSON.stringify(want));
        }
    });

    const presetsView = () => ({
        presets: [
            { id: 7, name: 'Single up', version: 1, mode: 'single_robot', where: 'PLN_01', members: [], drifted: [] },
            { id: 8, name: 'Mixed pair', version: 1, mode: '', where: 'PLN_01 / PLN_02', members: [], drifted: [] },
        ],
        candidates: [{ shape_key: 'k1', mode: '', where: 'PLN_01 / PLN_02', members: [1, 2], member_names: ['Style A', 'Style B'] }],
        styles_with_flow: 2,
    });
    const presetsPage = hash => loadPage({
        composer: savedComposer(), hash: hash,
        fetch: url => (url === '/api/processes/1/presets' ? okJSON(presetsView()) : null),
    });

    await pin('Presets say what the count counts', async () => {
        const { root } = presetsPage('#tab=presets');
        await settle();
        const h = root.innerHTML;
        if (h.indexOf('<th>Name</th><th>Shape</th><th>Applied to</th>') < 0) {
            throw new Error('the presets table heads its count ' + JSON.stringify((h.match(/<th>Shape<\/th><th>[^<]*<\/th>/) || [''])[0]));
        }
        if (h.indexOf('<th>Shape</th><th>Run by</th><th></th>') < 0) {
            throw new Error('the found-shapes table heads its count ' + JSON.stringify((h.match(/<th>Shape<\/th><th>[^<]*<\/th><th><\/th>/) || [''])[0]));
        }
        if (h.indexOf('Used by') >= 0) throw new Error('"Used by" is still on the Presets tab');
    });

    await pin('a preset applied to nothing says how a part gets on it', async () => {
        const { root } = presetsPage('#tab=presets;preset=7');
        await settle();
        const want = '<p class="pd-dim">Not applied to any part yet. Saving a preset from a flow does not ' +
            'apply it to that flow; Apply to parts… does.</p>';
        if (root.innerHTML.indexOf(want) < 0) {
            throw new Error('the empty member list reads ' +
                JSON.stringify((root.innerHTML.match(/<tr class="pd-memrow"><td colspan="6">(.*?)<\/td>/) || ['', ''])[1]));
        }
    });

    await pin('a mixed shape draws its own mark', async () => {
        const { root } = presetsPage('#tab=presets');
        await settle();
        const want = '<span class="pd-shape"><span class="pd-mixed" aria-hidden="true"><i></i><i></i></span>' +
            '<span class="w" title="its positions run different modes">mixed</span>';
        const n = root.innerHTML.split(want).length - 1;
        if (n !== 2) {
            throw new Error('want the mixed mark on the mixed preset and the mixed found shape (2), got ' + n + ': ' +
                JSON.stringify(root.innerHTML.match(/<span class="pd-shape">.*?<\/span><\/span>/g)));
        }
    });

    // ── Escape on Advanced asks before it discards typing ───────────────────
    //
    // The sheet's question, on the dialog that draws its own markup into
    // the scrim. The scrim's inputs are parsed out of the markup the page
    // wrote, the way a browser would: one element per <input>, attributes as
    // written, `value` a property typing moves, and a fresh set on every
    // innerHTML write — so a value the page puts back is one it really wrote.
    function liveInputs(scrim) {
        let html = scrim.innerHTML, els = null;
        Object.defineProperty(scrim, 'innerHTML', {
            configurable: true,
            get: () => html,
            set: v => { html = String(v); els = null; },
        });
        const parse = () => {
            if (els) return els;
            els = [];
            for (const tag of html.match(/<input\b[^>]*>/g) || []) {
                const attrs = {};
                for (const a of tag.matchAll(/([\w-]+)(?:="([^"]*)")?/g)) {
                    if (a[1] !== 'input' && !(a[1] in attrs)) attrs[a[1]] = a[2] === undefined ? '' : a[2];
                }
                const dataset = {};
                for (const k of Object.keys(attrs)) if (k.startsWith('data-')) dataset[attrName(k)] = attrs[k];
                els.push({
                    type: attrs.type || 'text', value: attrs.value || '', checked: 'checked' in attrs,
                    attrs, dataset, listeners: {},
                    getAttribute: n => (n in attrs ? attrs[n] : null),
                    hasAttribute: n => n in attrs,
                    addEventListener(t, fn) { (this.listeners[t] = this.listeners[t] || []).push(fn); },
                });
            }
            return els;
        };
        scrim.querySelectorAll = sel => {
            const m = /^\[([\w-]+)(?:="([^"]*)")?\]$/.exec(sel);
            if (!m) return [];
            return parse().filter(el => m[1] in el.attrs && (m[2] === undefined || el.attrs[m[1]] === m[2]));
        };
        return {
            find: (attr, v) => parse().find(el => el.attrs[attr] === v),
            type(el, v) { el.value = v; for (const fn of (el.listeners.input || [])) fn({ target: el }); },
        };
    }
    const escapeOn = doc => { for (const fn of (doc.listeners.keydown || [])) fn({ key: 'Escape' }); };
    const asks = scrim => scrim.innerHTML.indexOf('<h2>Discard the changes?</h2>') >= 0 &&
        scrim.innerHTML.indexOf('<button class="pd-btn" data-act="sheet-cancel">Keep editing</button>') >= 0;
    // The dialogs' own popover. The page draws it hidden inside the dialog's
    // markup; the stub is the element $('pd-advpop') finds.
    const advpopStub = () => Object.assign(makeElement('pd-advpop'), { hidden: true });
    const openAdv = async () => {
        let live = null;
        const page = loadPage({
            hash: '#style=1;adv=PLN_01', allowSheet: true, flowspec: true, composer: advComposer('manual'),
            onScrim: sc => { live = liveInputs(sc); }, extraIds: { 'pd-advpop': advpopStub() },
        });
        await settle();
        if (!page.scrim.classList.contains('active')) throw new Error('setup: the advanced sheet did not open');
        // #pd-pop is drawn hidden; the stub element starts shown, and Escape
        // would close it first.
        page.pop.hidden = true;
        return Object.assign(page, { live });
    };
    // The dialogs' pickers, opened by a real click: Advanced's carry-over
    // picker (Generate variants went in pick-a-part U5).
    const openAdvPick = (scrim, doc) => click(scrim, doc, { advkind: 'pick', adv: 'changeover_carryover_disposition' });
    const dialogs = [
        ['Advanced', openAdv, 'PLN_01 · Advanced', l => l.find('data-adv', 'reorder_point'), '40',
            (scrim, doc) => click(scrim, doc, { advkind: 'toggle', adv: 'evacuate_on_changeover' })],
    ];
    for (const [name, open, heading, field, typed, redraw] of dialogs) {
        await pin(name + ': Escape over typed input asks, and Keep editing keeps the value', async () => {
            const { scrim, doc, live } = await open();
            const el = field(live);
            if (!el) throw new Error('setup: no typed field in ' + scrim.innerHTML.slice(0, 300));
            live.type(el, typed);
            escapeOn(doc);
            if (!asks(scrim)) throw new Error('Escape did not ask; scrim: ' + scrim.innerHTML.slice(0, 300));
            click(scrim, doc, { act: 'sheet-cancel' });
            if (!scrim.classList.contains('active') || scrim.innerHTML.indexOf(heading) < 0) {
                throw new Error('Keep editing did not put the dialog back; scrim: ' + scrim.innerHTML.slice(0, 300));
            }
            const back = field(live);
            if (!back || back.value !== typed) throw new Error('Keep editing lost the typed value: ' + (back && back.value));
            escapeOn(doc);
            if (!asks(scrim)) throw new Error('a second Escape after Keep editing did not ask again');
            click(scrim, doc, { act: 'escape-discard' });
            if (scrim.classList.contains('active')) throw new Error('Discard did not close the dialog');
            escapeOn(doc);
        });
        await pin(name + ': typing survives a redraw as typed input', async () => {
            const { scrim, doc, live } = await open();
            live.type(field(live), typed);
            redraw(scrim, doc);
            if (scrim.innerHTML.indexOf(heading) < 0) throw new Error('setup: the redraw closed the dialog');
            escapeOn(doc);
            if (!asks(scrim)) throw new Error('Escape after a redraw did not ask; scrim: ' + scrim.innerHTML.slice(0, 300));
        });
        await pin(name + ': Escape with nothing typed closes it', async () => {
            const { scrim, doc } = await open();
            escapeOn(doc);
            if (scrim.classList.contains('active') || asks(scrim)) throw new Error('a clean dialog did not close on Escape');
        });
    }

    // ── Escape closes the dialog's own popover first ────────────────────────
    //
    // The innermost popover, then the sheet: with a picker open over Advanced
    // the first Escape closes only the picker — the dialog and
    // anything typed into it stay — and the second follows the dialog's rule.
    for (const [name, open, pick, field, typed] of [
        ['Advanced', openAdv, openAdvPick, l => l.find('data-adv', 'reorder_point'), '40'],
    ]) {
        for (const withTyping of [true, false]) {
            await pin(name + ': Escape closes its open picker first, then ' +
                (withTyping ? 'asks over typed input' : 'closes a clean dialog'), async () => {
                const { scrim, doc, live } = await open();
                if (withTyping) live.type(field(live), typed);
                const html = scrim.innerHTML;
                pick(scrim, doc);
                await settle();
                const advpop = doc.getElementById('pd-advpop');
                if (advpop.hidden !== false) throw new Error('setup: the picker did not open');
                escapeOn(doc);
                if (advpop.hidden !== true) throw new Error('the first Escape left the picker open');
                if (!scrim.classList.contains('active') || scrim.innerHTML !== html || asks(scrim)) {
                    throw new Error('the first Escape did more than close the picker; scrim: ' + scrim.innerHTML.slice(0, 200));
                }
                if (withTyping && field(live).value !== typed) throw new Error('the typed value did not stay');
                escapeOn(doc);
                if (withTyping) {
                    if (!asks(scrim)) throw new Error('the second Escape did not ask over typed input');
                } else if (scrim.classList.contains('active')) {
                    throw new Error('the second Escape did not close the clean dialog');
                }
            });
        }
    }

    // ── when this Edge has no plant map ──────────────────────────────────────
    //
    // The key route is walked over the plant map, so with no map its picker
    // has nothing to offer for a reason the engineer cannot fix from here, and
    // it says that reason. The Settings routing panel and the Add-process
    // sheet draw no map frame around a map that is not there. The routing
    // panel's heading line is the page's own description of the set: the
    // server's summary sentence counts backfill rows only.
    const plantMap = () => ({
        revision: 4,
        points: {
            SMN_010_a: { x: 0, y: 0 }, LM1: { x: 5, y: 0 }, PLN_01: { x: 10, y: 0 }, DN_001: { x: 10, y: 5 },
        },
        edges: [{ from: 'SMN_010_a', to: 'LM1' }, { from: 'LM1', to: 'PLN_01' }],
    });
    const mapComposer = map => {
        const c = savedComposer();
        c.cell.groups = { SMN_010: ['SMN_010_a'] };
        if (map) c.map = map;
        return c;
    };
    // The real projection, for the pins that draw a real map.
    function realGeometry() {
        const src = fs.readFileSync(
            path.join(__dirname, '..', '..', '..', '..', '..', 'shared', 'scene-geom.js'), 'utf8');
        const geom = vm.runInThisContext('(function(){' + src.replace(/^export\s+/gm, '') +
            '\nreturn { makeProjector, rotate90For, dist2, cubicLength, cubicPathD, laneKey };})',
            { filename: 'scene-geom.js' })();
        Object.assign(global, geom);
    }
    const viaPop = async map => {
        const ctx = loadPage({ flowspec: true, composer: mapComposer(map) });
        await settle();
        const b = makeElement('');
        b.classList.add('pd-sel');
        Object.assign(b.dataset, { act: 'pick', node: 'PLN_01', kind: 'via:0' });
        ctx.main.appendChild(b);
        bubble(b, 'click', ctx.doc);
        if (ctx.pop.hidden !== false) throw new Error('a click on the key-route chip did not open its menu');
        return ctx.pop.innerHTML;
    };

    await pin('the key-route picker on an Edge with no plant map says so', async () => {
        const got = await viaPop(null);
        const want = '<button data-opt="0" class="on">shortest way</button>' +
            '<div class="none">No plant map on this Edge yet</div>';
        if (got !== want) throw new Error('the key-route menu reads\n      got  ' + got + '\n      want ' + want);
    });

    await pin('the key-route picker on an Edge with a plant map offers its waypoints', async () => {
        const got = await viaPop(plantMap());
        const want = '<button data-opt="0" class="on">shortest way</button>' +
            '<button data-opt="1" class="">LM1</button>';
        if (got !== want) throw new Error('the key-route menu reads\n      got  ' + got + '\n      want ' + want);
    });

    const routingView = {
        rows: [
            { id: 1, core_node_name: 'SMN_010', role: 'source', label: 'SMN_010', enabled: true, sequence: 1 },
            { id: 2, core_node_name: 'DN_001', role: 'destination', label: 'DN_001', enabled: true, sequence: 2 },
        ],
        summary: 'Derived from 1 backfill row.',
    };
    const settingsPage = map => loadPage({
        allowSheet: true, composer: mapComposer(map), hash: '#tab=settings',
        fetch: url => (url === '/api/processes/1/routing-nodes' ? okJSON(routingView) : null),
    });
    const routingHead = '<div class="pd-sect"><h2>Routing set</h2><span class="pd-dim">' +
        'where this process may draw bins from, stage them, and send them</span></div>';
    const noMapNote = '<p class="pd-note">No plant map on this Edge yet. Core sends it with the node ' +
        'list; until then the lists above are the whole routing set.</p>';

    await pin('the routing panel with no plant map draws one line and no map frame', async () => {
        const { root } = settingsPage(null);
        await settle();
        const h = root.innerHTML;
        if (h.indexOf('<h2>Routing set</h2>') < 0) throw new Error('setup: the routing panel is not drawn');
        if (h.indexOf(routingHead) < 0) {
            throw new Error('the routing heading reads ' + JSON.stringify((h.match(/<h2>Routing set<\/h2>.*?<\/div>/) || [''])[0]));
        }
        if (h.indexOf('backfill') >= 0) throw new Error('the server’s backfill sentence is still drawn');
        if (h.indexOf(noMapNote) < 0) throw new Error('the no-map line does not read ' + JSON.stringify(noMapNote));
        for (const frame of ['pd-rsmap', 'rs-zoom', 'pd-nomap', 'no plant map cached']) {
            if (h.indexOf(frame) >= 0) throw new Error('the no-map routing panel still draws ' + frame);
        }
    });

    await pin('the routing panel with a plant map draws the map and its caption', async () => {
        const { root } = settingsPage(plantMap());
        realGeometry();
        await settle();
        const h = root.innerHTML;
        if (h.indexOf(routingHead) < 0) {
            throw new Error('the routing heading reads ' + JSON.stringify((h.match(/<h2>Routing set<\/h2>.*?<\/div>/) || [''])[0]));
        }
        if (h.indexOf('backfill') >= 0) throw new Error('the server’s backfill sentence is still drawn');
        if (h.indexOf('<div class="pd-map pd-rsmap">') < 0) throw new Error('the routing map frame is not drawn');
        if (h.indexOf('plant map from Core · revision 4') < 0) throw new Error('the map caption does not name the revision');
        if (h.indexOf('No plant map on this Edge yet') >= 0) throw new Error('a page with a map says it has none');
    });

    const addProcessSheet = async map => {
        const { root, doc, scrim } = loadPage({ allowSheet: true, composer: mapComposer(map) });
        if (map) realGeometry();
        await settle();
        click(root, doc, { act: 'add-process' });
        await settle();
        if (!scrim.classList.contains('active')) throw new Error('setup: the Add process sheet did not open');
        return scrim.innerHTML;
    };

    await pin('the Add process sheet with no plant map has no "Where that is" section', async () => {
        const h = await addProcessSheet(null);
        for (const frame of ['Where that is', 'pd-sheetmap', 'pd-nomap']) {
            if (h.indexOf(frame) >= 0) throw new Error('the no-map Add process sheet still draws ' + frame);
        }
    });

    await pin('the Add process sheet with a plant map draws "Where that is"', async () => {
        const h = await addProcessSheet(plantMap());
        if (h.indexOf('<div class="pd-lbl">Where that is</div>') < 0) throw new Error('the map section is missing');
        if (h.indexOf('id="pd-sheetmap"') < 0) throw new Error('the sheet map frame is missing');
    });

    // ── the list, Add process and the rail say what they mean ────────────────
    //
    // The list's search box finds a process by any of its styles, not only the
    // one running. A picker's search field names what it searches. The Group
    // field says what a group does. The Add-process routing note is one
    // sentence. A refused name stops being reported once the engineer types.
    // The rail names a position the way the rest of the page does.
    const listPage = () => {
        const q = makeElement('pd-q');
        const ctx = loadPage({
            search: '?list',
            styles: [
                { id: 1, process_id: 1, name: 'Style A' },
                { id: 2, process_id: 1, name: 'Style B' },
            ],
            extraIds: { 'pd-q': q },
        });
        return Object.assign(ctx, { q });
    };

    await pin('a list row carries every style name of its process as search text', async () => {
        const { root } = listPage();
        await settle();
        if (root.innerHTML.indexOf('<tr data-open="1" data-hay="Style A · Style B">') < 0) {
            throw new Error('the row reads ' + JSON.stringify((root.innerHTML.match(/<tr data-open[^>]*>/) || [''])[0]));
        }
    });

    await pin('the list search finds a process by a style that is not running', async () => {
        const { root, doc, q } = listPage();
        await settle();
        const hay = (root.innerHTML.match(/<tr data-open="1"(?: data-hay="([^"]*)")?>/) || [])[1];
        const tr = makeElement('');
        if (hay !== undefined) tr.dataset.hay = hay;
        for (const text of ['Press 1', '—', 'Style A', '2', 'on', 'not wired', 'off', '0', 'Running Style A']) {
            const td = makeElement('');
            td.textContent = text;
            tr.children.push(td);
        }
        const acts = makeElement('');
        acts.classList.add('pd-acts');
        acts.textContent = 'FlowsSettings';
        tr.children.push(acts);
        root.querySelectorAll = sel => (sel === '.pd-tbl tbody tr' ? [tr] : []);
        q.value = 'style b';
        bubble(q, 'input', doc);
        if (tr.hidden) throw new Error('searching "style b" hid the process that has Style B');
        q.value = 'style c';
        bubble(q, 'input', doc);
        if (!tr.hidden) throw new Error('searching "style c" kept a process with no such style');
    });

    await pin('a picker searching parts says part, a picker searching nodes says node', async () => {
        const h = await addProcessSheet(null);
        const parts = h.split('placeholder="find a part by name"').length - 1;
        const nodes = h.split('placeholder="find a node by name"').length - 1;
        // The positions field is the checklist, whose field filters a list
        // that is already open rather than finding into a closed one.
        const check = h.split('placeholder="filter by name, or pick a prefix below"').length - 1;
        if (parts !== 1 || nodes !== 3 || check !== 1) {
            throw new Error('want 1 part, 3 node and 1 checklist placeholder, got ' + parts + ', ' + nodes + ' and ' + check);
        }
    });

    await pin('the Group field says what a group does, on both sheets', async () => {
        const add = await addProcessSheet(null);
        if (add.indexOf('<label>Group<small>groups processes on the list</small></label>') < 0) {
            throw new Error('the Add process Group help reads ' + JSON.stringify((add.match(/<label>Group.*?<\/label>/) || [''])[0]));
        }
        const { root } = loadPage({ allowSheet: true, composer: savedComposer(), hash: '#tab=settings' });
        await settle();
        if (root.innerHTML.indexOf('groups processes on the list') < 0) {
            throw new Error('the Settings Group help does not say "groups processes on the list"');
        }
        if (/pure taxonomy/.test(add + root.innerHTML)) throw new Error('"pure taxonomy" is still drawn');
    });

    await pin('the Add process routing note is one sentence', async () => {
        const h = await addProcessSheet(null);
        const want = '<p class="pd-note">Operators are offered only these places, never the whole plant.</p>';
        if (h.indexOf(want) < 0) {
            throw new Error('the routing note reads ' + JSON.stringify((h.match(/<p class="pd-note">A name.*?<\/p>/) || [''])[0]));
        }
    });

    await pin('a refused name stops being reported once the engineer types', async () => {
        const { root, doc, scrim } = loadPage({ allowSheet: true, composer: mapComposer(null) });
        await settle();
        const status = wireSheet(scrim, '');
        const name = scrim.querySelector('[data-f="name"]');
        click(root, doc, { act: 'add-process' });
        await settle();
        click(scrim, doc, { act: 'sheet-ok' });
        await settle();
        if (status.textContent !== 'A process needs a name.') {
            throw new Error('setup: the empty name was not refused, the line reads ' + JSON.stringify(status.textContent));
        }
        name.value = 'Press 9';
        name.parentNode = scrim;
        bubble(name, 'input', doc);
        if (status.textContent !== '') throw new Error('the refusal still reads ' + JSON.stringify(status.textContent));
        if (status.className !== 'st') throw new Error('the line is still marked bad: ' + status.className);
    });

    // THE STATE CELL IS DATA, TONED BY WHAT IS TRUE. The running style's name
    // is typed by an engineer, so it is escaped like every other cell. The ok
    // tone marks a process making a part; a changeover is not that. The name
    // sits in its own no-wrap box so a hyphenated name breaks only after
    // "Running", and ellipsises with the full name in its title.
    await pin('the list state cell escapes the style name and tones only a running part', async () => {
        const q = makeElement('pd-q');
        const { root } = loadPage({
            search: '?list',
            processes: [
                { id: 1, name: 'Press 1', group_id: 0, active_style_id: 1 },
                { id: 2, name: 'Press 2', group_id: 0, active_style_id: 2, target_style_id: 3 },
            ],
            styles: [
                { id: 1, process_id: 1, name: 'LOADER-<b>&-RUN' },
                { id: 2, process_id: 2, name: 'Style B' },
                { id: 3, process_id: 2, name: 'Style C' },
            ],
            extraIds: { 'pd-q': q },
        });
        await settle();
        const cell = id => {
            const tr = (root.innerHTML.match(new RegExp('<tr data-open="' + id + '".*?</tr>')) || [''])[0];
            const m = tr.match(/<span class="pd-state([^"]*)">(.*?)<\/span><\/td>/);
            if (!m) throw new Error('setup: no state cell in row ' + id + ': ' + JSON.stringify(tr));
            return { cls: m[1].trim().split(/\s+/), html: m[2] };
        };
        const run = cell(1);
        if (/<b>/.test(run.html)) throw new Error('the style name is not escaped: ' + JSON.stringify(run.html));
        if (run.cls.indexOf('running') < 0) throw new Error('a running part is not toned running: ' + JSON.stringify(run.cls));
        const want = 'Running <span class="pd-statename" title="LOADER-&lt;b&gt;&amp;-RUN">LOADER-&lt;b&gt;&amp;-RUN</span>';
        if (run.html !== want) throw new Error('the running cell reads ' + JSON.stringify(run.html));
        const co = cell(2);
        if (co.cls.indexOf('running') >= 0) throw new Error('a changeover is toned running');
        if (co.html !== 'Changing over') throw new Error('the changeover cell reads ' + JSON.stringify(co.html));
        const css = fs.readFileSync(path.join(__dirname, '..', '..', 'css', 'processes-desktop.css'), 'utf8');
        const rule = (css.match(/\.pd-statename\s*\{([^}]*)\}/) || [])[1] || '';
        // ON ONE LINE AND WHOLE: the cell says what is running, so the name
        // must neither break at its hyphens nor be cut to an ellipsis; the
        // list's columns size to it instead.
        if (!/white-space:\s*nowrap/.test(rule)) {
            throw new Error('.pd-statename does not hold the name on one line: ' + JSON.stringify(rule));
        }
        if (/text-overflow|overflow:\s*hidden|max-width/.test(rule)) {
            throw new Error('.pd-statename cuts the name it should show whole: ' + JSON.stringify(rule));
        }
    });

    await pin('the rail names each position in full', async () => {
        const c = savedComposer();
        c.styles[0].claim_nodes = ['PLN_01', 'PLN_02'];
        const { root } = loadPage({ composer: c });
        await settle();
        const sub = (root.innerHTML.match(/<div class="pd-row[^"]*" data-style="1">.*?<div class="s">(.*?)<\/div><\/div>/) || [])[1];
        if (sub === undefined) throw new Error('setup: the rail row for Style A is not drawn');
        if (sub.indexOf('PLN_01/PLN_02') < 0 || /\bP0[12]\b/.test(sub)) {
            throw new Error('the rail sub-line reads ' + JSON.stringify(sub));
        }
    });

    // THE RAIL IS RECIPES, THEN STYLES, each under its own label, and a
    // recipe row opens that recipe as a draft to change and save again.
    await pin('the rail lists the recipes, then the styles', async () => {
        const { root } = loadPage({});
        await settle();
        const h = root.innerHTML;
        const r = h.indexOf('Recipes · 1'), st = h.indexOf('Styles · 1');
        if (r < 0 || st < 0 || r > st) throw new Error('rail order: ' + h.slice(h.indexOf('pd-rail'), h.indexOf('pd-rail') + 600));
        if (h.indexOf('data-act="recipe-open" data-preset="7"') < 0) throw new Error('no row for recipe 7');
        if (h.indexOf('data-act="new-recipe"') < 0) throw new Error('no + New recipe');
    });

    // A PROCESS WITH A RECIPE IS DRAWN BY RELATION, AND A RECIPE IS ONE
    // STATION'S (SYNTH round 2 rows 17 and 23). The composer read's cell is
    // process-wide: here a press station's spots, the stationless on-deck slot
    // it shares, and an unloader station's window across the plant. The style picture
    // draws all of them, by relation, because the process has a recipe; the
    // recipe draft draws the recipe's own station and the stationless slot,
    // and never the unloader's window, which would stand two far columns off.
    await pin('the recipe draft draws its own station, by relation', async () => {
        const io = { inbound_source: 'SMN_010', outbound_destination: 'DN_001' };
        const head = Object.assign({ core_node_name: 'PLN_02', role: 'produce', swap_mode: 'two_robot_press_index',
            paired_core_node: 'PLN_01', payload_code: 'PART-A' }, io);
        const composer = {
            styles: [{ id: 1, name: 'Style A', claims: [head], parts: ['PART-A'], advanced: {} }],
            routing: [
                { core_node_name: 'SMN_010', role: 'source', label: 'SMN_010', enabled: true, sequence: 1 },
                { core_node_name: 'DN_001', role: 'destination', label: 'DN_001', enabled: true, sequence: 2 },
            ],
            presets: [{ id: 7, name: 'Two up', mode: 'two_robot_press_index', where: 'PLN_02',
                cells: { PLN_02: Object.assign({}, head, { payload_code: '' }) } }],
            palette: ['PART-A'],
            cell: {
                geometry: true,
                positions: [
                    { core_node_name: 'PLN_01', kind: 'back', sequence: 1, x: 10, y: 20.5 },
                    { core_node_name: 'PLN_02', kind: 'front', sequence: 2, x: 11, y: 20, station_id: 18 },
                    { core_node_name: 'PLN_03', kind: 'front', sequence: 3, x: 12, y: 20, station_id: 18 },
                    { core_node_name: 'ULN_02', kind: 'front', sequence: 4, x: 100, y: 20, station_id: 25 },
                ],
                groups: {},
            },
        };
        const { pic, svg } = pictureFrame(1084);
        const { doc, main, win } = loadPage({ composer, extraIds: { 'pd-svg': svg }, realPicture: true });
        main.appendChild(pic);
        await settle();
        const names = () => (win.pictureViews[win.pictureViews.length - 1].cell.positions || []).map(p => p.core_node_name).join(' ');
        const last = () => win.pictureCalls[win.pictureCalls.length - 1] || {};
        if (!win.pictureCalls.length) throw new Error('setup: the page never drew the picture');
        if (last().relation !== true) throw new Error('the style picture of a process with a recipe asked for relation: ' + last().relation);
        if (names() !== 'PLN_01 PLN_02 PLN_03 ULN_02') throw new Error('the style picture draws the whole process cell; it drew ' + names());
        const row = makeElement('');
        row.dataset.act = 'recipe-open';
        row.dataset.preset = '7';
        main.appendChild(row);
        bubble(row, 'click', doc);
        await settle();
        if (names() !== 'PLN_01 PLN_02 PLN_03') throw new Error('the recipe draft drew ' + names() + ' — want its station and the stationless slot');
        if (last().relation !== true) throw new Error('the recipe draft asked for relation: ' + last().relation);
        if (/data-pos="ULN_02"/.test(svg.innerHTML) || !/data-pos="PLN_01"/.test(svg.innerHTML)) {
            throw new Error('the recipe draft markup: ' + (svg.innerHTML.match(/data-pos="[^"]+"/g) || []).join(' '));
        }
    });

    // A STYLE BUILT ON THE HMI REACHES THE RAIL WITHOUT A RELOAD: the rail's
    // own re-read redraws its lists and nothing else.
    await pin('a style built on a station appears in the rail on its next read', async () => {
        const intervals = [];
        const body = makeElement('pd-rail-body');
        let reads = 0;
        loadPage({
            intervals, extraIds: { 'pd-rail-body': body },
            fetch: url => {
                if (!/\/api\/processes\/1\/composer$/.test(url)) return null;
                reads++;
                const styles = [{ id: 1, name: 'Style A', claims: [], parts: [], advanced: {} }];
                if (reads > 1) styles.push({ id: 2, name: 'Style B', claim_count: 2, claims: [], parts: [], advanced: {} });
                return Promise.resolve({ ok: true, json: async () => ({ styles: styles, routing: [], presets: [],
                    palette: [], cell: { positions: [], groups: {} } }) });
            },
        });
        await settle();
        if (!intervals.length) throw new Error('the page sets no re-read interval');
        intervals[0]();
        await settle();
        if (body.innerHTML.indexOf('Style B') < 0) throw new Error('the rail did not pick up Style B: ' + body.innerHTML.slice(0, 300));
    });

    // THE GATE IS A SENTENCE IN THE HEADER'S OWN TEXT STYLE. It is decided in
    // Settings and only read here, so it is drawn as words — no pill border,
    // no chip, no second door to the Settings tab.
    await pin('the app bar states the gate as plain text', async () => {
        for (const [off, words] of [[false, 'HMI setup on: operators set up styles'], [true, 'HMI setup off: operators pick from the styles']]) {
            const { root } = loadPage({ gateOff: off });
            await settle();
            const want = '<div class="pd-spacer"></div><div class="pd-gate">' + words + '</div></div>';
            if (root.innerHTML.indexOf(want) < 0) {
                throw new Error('the app bar ends ' + JSON.stringify((root.innerHTML.match(/<div class="pd-spacer"><\/div>.*?<\/div><\/div>/) || [''])[0]));
            }
        }
        const css = fs.readFileSync(path.join(__dirname, '..', '..', 'css', 'processes-desktop.css'), 'utf8');
        if (/\.pd-pill/.test(css)) throw new Error('processes-desktop.css still styles .pd-pill');
    });

    await recipeGroupPins({ pin, okJSON, click, wireSheet, wireOptions, chip, settle, liveInputs });
    if (!nodeProcess.argv[2]) await settingsPositionsPins({ pin, okJSON, click, wireSheet, settle });

    if (failures > 0) {
        console.error('FAILED: ' + failures + ' of ' + (failures + checks));
        nodeProcess.exit(1);
    }
    console.log('OK: ' + checks + ' checks passed');
    nodeProcess.exit(0);
}

// ── recipe groups: the engineer's editor in the recipe draft ─────────────────
//
// SYNTH-round2 rows 9, 10, 18 and 24 (recipe groups, 2026-10-24). Under the
// positions table, Set up groups lays out one card per part, pre-filled from
// the cells; In from / Out to are the ★ cell's own fields; a group's Advanced
// is the existing sheet in group scope and rides the save as
// groups[i].advanced; the per-row gear is gone from a recipe draft, whose save
// would drop it; the picture fades and tags by focus with its layout held by
// the groups' pairs; "On the floor" is the SAVED card's; the New style sheet
// has a Spots step whose mint carries the moves as the station's bytes.
//
// A SYNTHETIC PRESS-6 SHAPE: press spots in three columns (0.6, 0.95, 3.5) and
// three rows (0, 0.4, 0.8 north), an on-deck spot at each end between the
// middle and north rows, an unloader station far off. Node names only.
//
// THE DEPLOY NOTE RUNS ON THE SERVER'S OWN READS too:
// processes_desktop_groups_test.go hands this script the Press 6 fixture's
// desktop composer read (`--walk <read> <out>`), and the saved version's
// (`--floor <read> <out>`), and posts what the page posted to the real router.
const P6XY = {
    PLN_005: [0, 0.55], PLN_006: [0.6, 0], PLN_007: [3.5, 0], PLN_008: [4.5, 0.6], PLN_009: [0.95, 0],
    PLN_010: [0.6, 0.4], PLN_011: [0.95, 0.4], PLN_012: [3.5, 0.4], PLN_013: [0.6, 0.8], PLN_014: [0.95, 0.8],
    PLN_015: [3.5, 0.8],
};
const p6cell = (head, deck, dest) => ({ core_node_name: head, role: 'produce', swap_mode: 'two_robot_press_index',
    payload_code: '', paired_core_node: deck, inbound_source: 'SMN_BUF', outbound_destination: dest });
const p6claim = (head, deck, part, dest) => Object.assign(p6cell(head, deck, dest), { payload_code: part });
// SYNTH-round2 §2's groups, as the server stores and the desktop reads them.
const P6_GROUPS = () => [
    { of: 'PLN_006', name: 'Left Hand', spots: [
        { core_node_name: 'PLN_006', paired_core_node: ['PLN_005'] },
        { core_node_name: 'PLN_010', paired_core_node: ['PLN_005'] }] },
    { of: 'PLN_007', name: 'Right Hand', spots: [
        { core_node_name: 'PLN_007', paired_core_node: ['PLN_008'] },
        { core_node_name: 'PLN_012', paired_core_node: ['PLN_008'] }],
    advanced: { reorder_point: 1, auto_reorder: true } },
];
// The card's places for that recipe, in the shape domain.RecipeSpots ships.
const P6_GROUPED_SPOTS = () => [
    { node: 'PLN_006', job: 'spot', targets: ['PLN_006', 'PLN_010'], group: 'Left Hand' },
    { node: 'PLN_005', job: 'on_deck', of: 'PLN_006', targets: ['PLN_005'], group: 'Left Hand',
        at: { PLN_006: ['PLN_005'], PLN_010: ['PLN_005'] } },
    { node: 'PLN_007', job: 'spot', targets: ['PLN_007', 'PLN_012'], group: 'Right Hand' },
    { node: 'PLN_008', job: 'on_deck', of: 'PLN_007', targets: ['PLN_008'], group: 'Right Hand',
        at: { PLN_007: ['PLN_008'], PLN_012: ['PLN_008'] } },
];
// v0's card for the ungrouped recipe: a spot moves to any free position the
// recipes pair with its deck.
const P6_PLAIN_SPOTS = () => [
    { node: 'PLN_006', job: 'spot', targets: ['PLN_006', 'PLN_009', 'PLN_010'] },
    { node: 'PLN_005', job: 'on_deck', of: 'PLN_006', targets: ['PLN_005'] },
    { node: 'PLN_007', job: 'spot', targets: ['PLN_007', 'PLN_012'] },
    { node: 'PLN_008', job: 'on_deck', of: 'PLN_007', targets: ['PLN_008'] },
];
function press6Composer(o) {
    o = o || {};
    const card = {
        id: '7', name: 'Two spot press index', mode: 'two_robot_press_index', where: 'PLN_006 / PLN_007',
        cells: { PLN_006: p6cell('PLN_006', 'PLN_005', 'ULN_002'), PLN_007: p6cell('PLN_007', 'PLN_008', o.dest7 || 'ULN_002') },
        slots: [{ nodes: ['PLN_006'], role: 'produce' }, { nodes: ['PLN_007'], role: 'produce' }],
        last_moves: { PLN_006: 'PLN_010', PLN_007: 'PLN_012' },
        spots: o.grouped ? P6_GROUPED_SPOTS() : P6_PLAIN_SPOTS(),
    };
    if (o.grouped) card.groups = P6_GROUPS();
    const one = {
        id: '8', name: 'One spot press index', mode: 'two_robot_press_index', where: 'PLN_009',
        cells: { PLN_009: p6cell('PLN_009', 'PLN_005', 'ULN_002') }, slots: [{ nodes: ['PLN_009'], role: 'produce' }],
        spots: [{ node: 'PLN_009', job: 'spot', targets: ['PLN_009'] }, { node: 'PLN_005', job: 'on_deck', of: 'PLN_009', targets: ['PLN_005'] }],
    };
    const running = o.running || [p6claim('PLN_010', 'PLN_005', 'SYN-P6-A', 'ULN_002'), p6claim('PLN_012', 'PLN_008', 'SYN-P6-B', 'ULN_005')];
    return {
        styles: [{ id: 1, name: 'SYN-RUN', claims: running, parts: running.map(c => c.payload_code), advanced: {} }],
        routing: [
            { core_node_name: 'SMN_BUF', role: 'source', label: 'SMN_BUF', enabled: true, sequence: 1 },
            { core_node_name: 'ULN_002', role: 'destination', label: 'ULN_002', enabled: true, sequence: 1 },
            { core_node_name: 'ULN_005', role: 'destination', label: 'ULN_005', enabled: true, sequence: 2 },
            { core_node_name: 'STG_1', role: 'staging', label: 'STG_1', enabled: true, sequence: 1 },
        ],
        presets: [card, one],
        palette: ['SYN-P6-A', 'SYN-P6-B'],
        cell: {
            geometry: true,
            positions: Object.keys(P6XY).sort().map((n, i) => ({
                core_node_name: n, kind: n === 'PLN_005' || n === 'PLN_008' ? 'back' : 'front', sequence: i + 1,
                x: P6XY[n][0], y: P6XY[n][1], station_id: 18,
            })).concat([
                { core_node_name: 'ULN_002', kind: 'front', sequence: 12, x: 100, y: 0, station_id: 25 },
                { core_node_name: 'ULN_005', kind: 'front', sequence: 13, x: 101, y: 0, station_id: 25 },
            ]),
            groups: {},
        },
    };
}

// A card's own markup in the picture: from its data-pos to the next card's.
function cardMarkup(svg, n) {
    const i = svg.indexOf('data-pos="' + n + '"');
    if (i < 0) return '';
    const j = svg.indexOf('data-pos="', i + 10);
    return svg.slice(i, j < 0 ? undefined : j);
}
const cvsIn = (svg, n) => (cardMarkup(svg, n).match(/data-robot="\d" data-dir="\w"/g) || [])
    .map(s => s.replace(/data-robot="(\d)" data-dir="(\w)"/, '$1$2')).join(' ');
const faintIn = (svg, n) => new RegExp('<g class="node[^"]*\\bfaint\\b[^"]*" data-pos="' + n + '"').test(svg);
const tagIn = (svg, n) => {
    const m = cardMarkup(svg, n).match(/<g class="gtag (g\d)">.*?<text[^>]*>([^<]*)<\/text>/);
    return m ? m[2] + ' ' + m[1] : '';
};
const topIn = (svg, n) => {
    const m = new RegExp('data-pos="' + n + '"[^>]*?transform="translate\\((-?[\\d.]+),(-?[\\d.]+)\\)"').exec(svg);
    return m ? Number(m[2]) : NaN;
};
const plain = html => String(html).replace(/<\/?(div|p|br|tr|td|th|li|h\d)\b[^>]*>/g, ' ').replace(/<[^>]*>/g, '')
    .replace(/&amp;/g, '&').replace(/\s+/g, ' ').trim();
const between = (h, a, b) => { const i = h.indexOf(a); if (i < 0) return ''; const j = h.indexOf(b, i + a.length); return h.slice(i, j < 0 ? undefined : j); };

async function recipeGroupPins(h) {
    const { pin, okJSON, click, wireSheet, wireOptions, chip, settle, liveInputs } = h;
    const PRESS6_PAGE = {
        processes: [{ id: 1, name: 'Press 6', group_id: 0, active_style_id: 1, flow_composer_enabled: true }],
        styles: [{ id: 1, process_id: 1, name: 'SYN-RUN' }],
        stations: [{ id: 18, process_id: 1, name: 'Press 6 Operator Screen' }],
    };
    // One Press 6 page: the composer read, every POST recorded and answered
    // by `answer` (200 by default), and the Recipes tab's read.
    const p6Page = (composer, extra, answer) => {
        const posts = [];
        let live = null;
        const ctx = loadPage(Object.assign({}, PRESS6_PAGE, {
            flowspec: true, allowSheet: true, composer: composer,
            onScrim: sc => { live = liveInputs(sc); },
            fetch: (url, init) => {
                if (/\/api\/processes\/\d+\/composer$/.test(url)) return okJSON(composer);
                if (/\/presets$/.test(url) && !(init && init.method)) return okJSON({ presets: [], candidates: [] });
                if (init && init.method === 'POST') {
                    posts.push({ url: url, body: init.body });
                    return answer ? answer(url, init) : okJSON({ id: 99, style_id: 5, style_name: 'SYN-P6-A/B', minted: true });
                }
                return null;
            },
        }, extra || {}));
        return Object.assign(ctx, { posts: posts, live: () => live });
    };
    const openRecipe = async (ctx, id) => {
        click(ctx.main, ctx.doc, { act: 'recipe-open', preset: id || '7' });
        await settle();
    };
    // Picks the option labelled `label` off the popover a click opened.
    const pickOption = (ctx, open, label) => {
        const opts = wireOptions(ctx.pop);
        open();
        const re = new RegExp('data-opt="(\\d+)"[^>]*>(?:<[^>]*>)*' + label.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '(?:<|$)');
        const m = ctx.pop.innerHTML.match(re);
        if (!m) throw new Error('setup: no option ' + label + ' in ' + ctx.pop.innerHTML);
        bubble(opts.find(b => b.dataset.opt === m[1]), 'click', ctx.doc);
    };
    const addSpot = (ctx, g, node) => pickOption(ctx, () => click(ctx.main, ctx.doc, { act: 'gspot-add', g: String(g) }), node);
    const setDest = (ctx, head, node) => pickOption(ctx, () => bubble(chip(ctx.main, head, 'dest'), 'click', ctx.doc), node);
    const groupsHTML = root => between(root.innerHTML, '<div class="pd-groups"', '<div class="pd-bar"');

    // The deploy note's clicks, on whatever composer read the page is given.
    async function deployNote(ctx) {
        await openRecipe(ctx, ((ctx.composer.presets || []).find(p => p.name === 'Two spot press index') || {}).id);
        if (ctx.root.innerHTML.indexOf('Recipe · Two spot press index') < 0) throw new Error('the recipe draft did not open');
        click(ctx.main, ctx.doc, { act: 'groups-setup' });
        await settle();
        addSpot(ctx, 0, 'PLN_010');
        await settle();
        addSpot(ctx, 1, 'PLN_012');
        await settle();
        setDest(ctx, 'PLN_007', 'ULN_005');
        await settle();
        click(ctx.main, ctx.doc, { act: 'shape-save' });
        if (!ctx.scrim.classList.contains('active')) throw new Error('Save recipe… opened no naming sheet');
        const named = /data-f="name" value="([^"]*)"/.exec(ctx.scrim.innerHTML);
        if (!named || named[1] !== 'Two spot press index') throw new Error('the naming sheet opens on ' + (named && named[1]));
        wireSheet(ctx.scrim, named[1]);
        click(ctx.scrim, ctx.doc, { act: 'sheet-ok' });
        await settle();
        return ctx.posts.find(p => /\/presets$/.test(p.url));
    }

    const ARGV = nodeProcess.argv.slice(2);
    if (ARGV[0] === '--walk' || ARGV[0] === '--floor') {
        const composer = JSON.parse(fs.readFileSync(ARGV[1], 'utf8'));
        const ctx = Object.assign(p6Page(composer), { composer: composer });
        await settle();
        const out = {};
        if (ARGV[0] === '--walk') {
            const posted = await deployNote(ctx);
            out.body = posted ? JSON.parse(posted.body) : null;
            out.groups = plain(groupsHTML(ctx.root));
        } else {
            await openRecipe(ctx, ((composer.presets || []).find(p => p.name === 'Two spot press index') || {}).id);
            out.floor = plain(between(ctx.root.innerHTML, '<div class="pd-floor"', '</div></div>'));
            out.cards = (ctx.root.innerHTML.match(/data-gcard="\d+"/g) || []).length;
        }
        fs.writeFileSync(ARGV[2], JSON.stringify(out));
        return;
    }

    // THE PER-ROW GEAR IS GONE FROM A RECIPE DRAFT. A recipe's save strips
    // every cell's Advanced (domain.StripFlowPayload), so the gear opened a
    // sheet whose every field was dropped without a word. A group's
    // Advanced is the one a recipe keeps.
    await pin('a recipe draft draws no per-row Advanced gear', async () => {
        const ctx = p6Page(press6Composer());
        await settle();
        if (ctx.root.innerHTML.indexOf('data-act="advanced"') < 0) throw new Error('setup: the style has no gear to lose');
        await openRecipe(ctx);
        const h = ctx.root.innerHTML;
        if (h.indexOf('Recipe · Two spot press index') < 0) throw new Error('setup: the draft did not open');
        if (h.indexOf('data-act="advanced"') >= 0 || h.indexOf('<th>Advanced</th>') >= 0) {
            throw new Error('the recipe draft still draws the per-row gear: ' +
                (h.match(/<button class="pd-adv[^>]*>/) || [''])[0]);
        }
    });

    await pin('Set up groups lays out a card per part, pre-filled from the cells', async () => {
        const ctx = p6Page(press6Composer());
        await settle();
        await openRecipe(ctx);
        if (ctx.root.innerHTML.indexOf('data-act="groups-setup"') < 0) throw new Error('the draft has no Set up groups');
        if (ctx.root.innerHTML.indexOf('data-gcard') >= 0) throw new Error('cards are drawn before Set up groups');
        click(ctx.main, ctx.doc, { act: 'groups-setup' });
        await settle();
        const g = groupsHTML(ctx.root);
        const cards = g.match(/data-gcard="\d+"/g) || [];
        if (cards.length !== 2) throw new Error('want 2 cards, got ' + cards.length + ': ' + plain(g).slice(0, 300));
        if (!/data-gname="0" value="Left Hand"/.test(g) || !/data-gname="1" value="Right Hand"/.test(g)) {
            throw new Error('the two produce slots are not named Left Hand / Right Hand: ' + (g.match(/data-gname="\d" value="[^"]*"/g) || []).join(' '));
        }
        const left = between(g, 'data-gcard="0"', 'data-gcard="1"');
        if (!/data-grow="PLN_006"/.test(left) || plain(left).indexOf('PLN_006 ★') < 0) throw new Error('Left Hand\'s ★ row is not PLN_006: ' + plain(left));
        if (/data-act="glist-rm"[^>]*data-v="PLN_005"/.test(left)) throw new Error('the ★ cell\'s own on-deck spot can be taken out of its row');
        if (plain(left).indexOf('PLN_005') < 0) throw new Error('PLN_005 is not on Left Hand\'s ★ row');
        if (/data-act="gspot-rm"[^>]*data-node="PLN_006"/.test(left)) throw new Error('the ★ row can be removed');
        if (!/data-act="glist-add"[^>]*>\+ on deck</.test(left)) throw new Error('no + on deck on the ★ row');
        if (/>\+ third<|>\+ lane</.test(left)) throw new Error('a list the ★ cell leaves blank is offered: ' + plain(left));
        if (!/data-act="gspot-add"[^>]*>\+ Add a press spot</.test(left)) throw new Error('no + Add a press spot');
        if (!/data-act="gadv" data-g="0"[^>]*>Edit…</.test(left)) throw new Error('no Advanced · Edit… on Left Hand');
    });

    await pin('+ Add a press spot offers the positions no group holds, and pre-fills the ★ row\'s lists', async () => {
        const ctx = p6Page(press6Composer());
        await settle();
        await openRecipe(ctx);
        click(ctx.main, ctx.doc, { act: 'groups-setup' });
        await settle();
        wireOptions(ctx.pop);
        click(ctx.main, ctx.doc, { act: 'gspot-add', g: '0' });
        const offered = (ctx.pop.innerHTML.match(/data-opt="\d+"[^>]*>[^<]+/g) || []).map(s => s.replace(/.*>/, '')).join(' ');
        if (offered !== 'PLN_009 PLN_010 PLN_011 PLN_012 PLN_013 PLN_014 PLN_015') {
            throw new Error('Left Hand offers ' + JSON.stringify(offered) + ' — want this station\'s positions no group holds');
        }
        addSpot(ctx, 0, 'PLN_010');
        await settle();
        const left = between(groupsHTML(ctx.root), 'data-gcard="0"', 'data-gcard="1"');
        const row = between(left, 'data-grow="PLN_010"', '</tr>');
        if (!row) throw new Error('no PLN_010 row in Left Hand: ' + plain(left));
        if (!/data-act="glist-rm"[^>]*data-v="PLN_005"/.test(row)) throw new Error('PLN_010\'s on-deck did not pre-fill PLN_005: ' + plain(row));
        if (!/data-act="gspot-rm"[^>]*data-node="PLN_010"/.test(row)) throw new Error('the added row cannot be taken out');
        wireOptions(ctx.pop);
        click(ctx.main, ctx.doc, { act: 'gspot-add', g: '1' });
        if (/>PLN_010</.test(ctx.pop.innerHTML) || />PLN_005</.test(ctx.pop.innerHTML)) {
            throw new Error('Right Hand is offered a node Left Hand holds: ' + ctx.pop.innerHTML);
        }
    });

    await pin('In from / Out to are the ★ cell\'s, read and written there', async () => {
        const ctx = p6Page(press6Composer());
        await settle();
        await openRecipe(ctx);
        click(ctx.main, ctx.doc, { act: 'groups-setup' });
        await settle();
        const right = () => between(groupsHTML(ctx.root), 'data-gcard="1"', '<div class="pd-floor"');
        if (!/data-node="PLN_007" data-kind="dest"[^>]*><span class="t">ULN_002</.test(right())) {
            throw new Error('Right Hand\'s Out to does not read PLN_007\'s destination: ' + plain(right()));
        }
        setDest(ctx, 'PLN_007', 'ULN_005');
        await settle();
        if (!/data-node="PLN_007" data-kind="dest"[^>]*><span class="t">ULN_005</.test(right())) throw new Error('Out to did not take ULN_005');
        const table = between(ctx.root.innerHTML, 'id="pd-postbl"', 'class="pd-groups"');
        if (!/data-row="PLN_007".*?data-kind="dest"[^>]*><span class="t">ULN_005</.test(table)) {
            throw new Error('the positions table\'s PLN_007 row does not show the card\'s ULN_005');
        }
    });

    // A PRODUCE POSITION'S FINISHED-GOODS ROWS: one section, in the order an
    // engineer decides them — strict pack, its tolerance (drawn only while
    // strict pack is on), where partials go — and the partial home's help says
    // what each kind of home does. In group scope the same section, less the
    // cell's partial home, and the tolerance rides the recipe's save. Pinned
    // before the tolerance existed, when the sheet drew the partial home under
    // Unfinished carriers and strict pack under Counting.
    await pin('a produce position\'s sheet draws its finished-goods rows', async () => {
        const fgOf = scrim => scrim.innerHTML.split('<div class="pd-sec">').slice(1).map(sec => {
            const title = (sec.match(/<div class="pd-lbl">([^<]*)/) || [])[1];
            const keys = [...new Set((sec.match(/data-adv="[^"]*"/g) || []).map(s => s.slice(10, -1)))];
            return title + ': ' + keys.join(',');
        }).filter(s => /strict_pack|pack_tolerance_pct|partial_dest/.test(s)).join(' | ');
        const extra = () => ({ extraIds: { 'pd-advpop': Object.assign(makeElement('pd-advpop'), { hidden: true }) } });
        const ctx = p6Page(press6Composer(), extra());
        await settle();
        click(ctx.main, ctx.doc, { act: 'advanced', node: 'PLN_010' });
        const fg = () => fgOf(ctx.scrim);
        if (ctx.scrim.innerHTML.indexOf('<h2>PLN_010 · Advanced</h2>') < 0) throw new Error('setup: the sheet is not PLN_010\'s');
        const off = 'Finished goods: strict_pack,partial_dest';
        if (fg() !== off) throw new Error('strict pack off: ' + fg() + '\n      want ' + off);
        const home = plain(ctx.scrim.innerHTML.split('<div class="pd-fld">').find(f => f.indexOf('data-adv="partial_dest"') >= 0) || '');
        if (home.indexOf('Partials go to') !== 0) throw new Error('the partial home is not named Partials go to: ' + home);
        for (const says of ['where full bins go, at its real count', 'a node group holds it there to be finished later', 'the unloader unloads it there']) {
            if (home.indexOf(says) < 0) throw new Error('the partial home\'s help does not say "' + says + '": ' + home);
        }
        if (/called back/.test(home)) throw new Error('the partial home still says a bin waits to be called back: ' + home);
        click(ctx.scrim, ctx.doc, { advkind: 'toggle', adv: 'strict_pack' });
        const on = 'Finished goods: strict_pack,pack_tolerance_pct,partial_dest';
        if (fg() !== on) throw new Error('strict pack on: ' + fg() + '\n      want ' + on);

        // Group scope: Right Hand's Advanced, strict pack on, a tolerance typed.
        const g = p6Page(press6Composer(), extra());
        await settle();
        await openRecipe(g);
        click(g.main, g.doc, { act: 'groups-setup' });
        await settle();
        click(g.main, g.doc, { act: 'gadv', g: '1' });
        if (fgOf(g.scrim) !== 'Finished goods: strict_pack') throw new Error('group scope, strict pack off: ' + fgOf(g.scrim));
        click(g.scrim, g.doc, { advkind: 'toggle', adv: 'strict_pack' });
        if (fgOf(g.scrim) !== 'Finished goods: strict_pack,pack_tolerance_pct') throw new Error('group scope, strict pack on: ' + fgOf(g.scrim));
        const live = g.live();
        live.type(live.find('data-adv', 'pack_tolerance_pct'), '5');
        click(g.scrim, g.doc, { act: 'adv-apply' });
        await settle();
        click(g.main, g.doc, { act: 'shape-save' });
        wireSheet(g.scrim, 'Two spot press index');
        click(g.scrim, g.doc, { act: 'sheet-ok' });
        await settle();
        const posted = g.posts.find(p => /\/presets$/.test(p.url));
        if (!posted) throw new Error('the save posted nothing');
        const adv = JSON.parse(posted.body).groups[1].advanced;
        const want = '{"strict_pack":true,"pack_tolerance_pct":5}';
        if (JSON.stringify(adv) !== want) throw new Error('Right Hand\'s Advanced sent ' + JSON.stringify(adv) + '\n      want ' + want);
    });

    await pin('Advanced · Edit… is the sheet in group scope, and Apply rides the save as groups[i].advanced', async () => {
        const ctx = p6Page(press6Composer(), { extraIds: { 'pd-advpop': Object.assign(makeElement('pd-advpop'), { hidden: true }) } });
        await settle();
        await openRecipe(ctx);
        click(ctx.main, ctx.doc, { act: 'groups-setup' });
        await settle();
        click(ctx.main, ctx.doc, { act: 'gadv', g: '1' });
        const sheet = ctx.scrim.innerHTML;
        if (sheet.indexOf('<h2>Right Hand · Advanced</h2>') < 0) throw new Error('the sheet is not Right Hand\'s: ' + plain(sheet).slice(0, 200));
        for (const key of ['allowed_payload_codes', 'auto_request_payload', 'index_robot_supplies', 'sequence', 'evac_nodes', 'evac_dest', 'partial_dest']) {
            if (sheet.indexOf('data-adv="' + key + '"') >= 0) throw new Error('group scope draws ' + key + ', which a recipe may not say');
        }
        for (const key of ['reorder_point', 'auto_reorder', 'keep_staged_node', 'strict_pack', 'curtain_required']) {
            if (sheet.indexOf('data-adv="' + key + '"') < 0) throw new Error('group scope hides ' + key);
        }
        const live = ctx.live();
        live.type(live.find('data-adv', 'reorder_point'), '1');
        click(ctx.scrim, ctx.doc, { advkind: 'toggle', adv: 'auto_reorder' });
        click(ctx.scrim, ctx.doc, { act: 'adv-apply' });
        await settle();
        const right = between(groupsHTML(ctx.root), 'data-gcard="1"', '<div class="pd-floor"');
        if (plain(right).indexOf('Reorder point 1 · Auto reorder on') < 0) throw new Error('Right Hand\'s card says ' + plain(right));
        if (/data-act="advanced"/.test(ctx.root.innerHTML)) throw new Error('setup: a per-row gear is back');
        click(ctx.main, ctx.doc, { act: 'shape-save' });
        wireSheet(ctx.scrim, 'Two spot press index');
        click(ctx.scrim, ctx.doc, { act: 'sheet-ok' });
        await settle();
        const posted = ctx.posts.find(p => /\/presets$/.test(p.url));
        if (!posted) throw new Error('the save posted nothing');
        const body = JSON.parse(posted.body);
        const adv = body.groups && body.groups[1] && body.groups[1].advanced;
        if (JSON.stringify(adv) !== '{"reorder_point":1,"auto_reorder":true}') throw new Error('Right Hand\'s Advanced sent: ' + JSON.stringify(adv));
        if (body.groups[0].advanced) throw new Error('Left Hand sent an Advanced nobody set: ' + JSON.stringify(body.groups[0].advanced));
        if ((body.cells || []).some(c => c.advanced)) throw new Error('a cell carries an Advanced the save would drop');
    });

    // THE DEPLOY NOTE, CLICK BY CLICK: the body is SYNTH-round2 §2's recipe,
    // through the cells form, and the page stays on the saved recipe.
    await pin('the deploy note\'s clicks save SYNTH §2\'s recipe through the cells form', async () => {
        const ctx = Object.assign(p6Page(press6Composer()), { composer: press6Composer() });
        await settle();
        const posted = await deployNote(ctx);
        if (!posted) throw new Error('the save posted nothing');
        const groups = P6_GROUPS();
        delete groups[1].advanced;
        const want = ctx.win.DesktopBodies.flowPresetCreate('Two spot press index', {
            cells: [p6cell('PLN_006', 'PLN_005', 'ULN_002'), p6cell('PLN_007', 'PLN_008', 'ULN_005')], groups: groups,
        });
        if (posted.body !== JSON.stringify(want)) throw new Error('posted ' + posted.body + '\n      want   ' + JSON.stringify(want));
        if (ctx.scrim.classList.contains('active')) throw new Error('the naming sheet is still open after the save');
    });

    await pin('a refused save says the server\'s words beside the cards', async () => {
        const says = 'PLN_005 is in both Left Hand and Right Hand — a node serves one group only';
        const ctx = Object.assign(p6Page(press6Composer(), null, () => Promise.resolve({
            ok: false, status: 400, json: async () => ({ error: says }),
        })), { composer: press6Composer() });
        await settle();
        await deployNote(ctx);
        const g = groupsHTML(ctx.root);
        if (g.indexOf('<div class="pd-refusal" id="pd-grefusal">' + says + '</div>') < 0) {
            throw new Error('the cards do not carry the refusal: ' + plain(g).slice(0, 300));
        }
        if (!ctx.scrim.classList.contains('active')) throw new Error('the naming sheet closed over a refusal');
    });

    // THE PICTURE BY FOCUS (SYNTH row 18), its grid held by the groups' pairs
    // (the lead's D11): PLN_005 feeds PLN_006 and PLN_010, which stand in two
    // rows, so it takes the middle row in every state.
    await pin('the draft picture fades, tags and draws chevrons by focus', async () => {
        const { pic, svg } = pictureFrame(1084);
        const ctx = p6Page(press6Composer(), { extraIds: { 'pd-svg': svg }, realPicture: true });
        ctx.main.appendChild(pic);
        await settle();
        await openRecipe(ctx);
        click(ctx.main, ctx.doc, { act: 'groups-setup' });
        await settle();
        addSpot(ctx, 0, 'PLN_010');
        await settle();
        addSpot(ctx, 1, 'PLN_012');
        await settle();
        const last = () => ctx.win.pictureCalls[ctx.win.pictureCalls.length - 1];
        const pairs = '{"PLN_005":["PLN_006","PLN_010"],"PLN_008":["PLN_007","PLN_012"]}';
        const state = (what, want) => {
            const o = last(), s = svg.innerHTML;
            if (o.relation !== true) throw new Error(what + ': not by relation');
            if (JSON.stringify(o.pairs) !== pairs) throw new Error(what + ': pairs ' + JSON.stringify(o.pairs) + ', want ' + pairs);
            if (topIn(s, 'PLN_005') - 18 !== topIn(s, 'PLN_010')) throw new Error(what + ': PLN_005 is not in PLN_010\'s row');
            for (const n of Object.keys(P6XY)) {
                const full = want.full.indexOf(n) >= 0;
                if (faintIn(s, n) === full) throw new Error(what + ': ' + n + (full ? ' is faint' : ' is full'));
            }
            for (const n of Object.keys(want.cvs)) {
                if (cvsIn(s, n) !== want.cvs[n]) throw new Error(what + ': ' + n + ' chevrons ' + JSON.stringify(cvsIn(s, n)) + ', want ' + JSON.stringify(want.cvs[n]));
            }
            for (const [n, t] of [['PLN_006', 'L g1'], ['PLN_010', 'L g1'], ['PLN_007', 'R g2'], ['PLN_012', 'R g2'], ['PLN_005', ''], ['PLN_009', '']]) {
                if (tagIn(s, n) !== t) throw new Error(what + ': ' + n + ' tag ' + JSON.stringify(tagIn(s, n)) + ', want ' + JSON.stringify(t));
            }
        };
        state('no focus', {
            full: ['PLN_005', 'PLN_006', 'PLN_007', 'PLN_008', 'PLN_010', 'PLN_012'],
            cvs: { PLN_005: '', PLN_006: '', PLN_010: '' },
        });
        if (cardMarkup(svg.innerHTML, 'PLN_006').indexOf('Left Hand · ★ default') < 0) throw new Error('PLN_006\'s card does not say it is Left Hand\'s ★ spot');
        if (cardMarkup(svg.innerHTML, 'PLN_005').indexOf('feeds PLN_006 · PLN_010') < 0) throw new Error('PLN_005\'s card does not say what it feeds');
        const card = makeElement('');
        card.dataset.gcard = '0';
        ctx.main.appendChild(card);
        bubble(card, 'mouseover', ctx.doc);
        state('Left Hand hovered', {
            full: ['PLN_005', 'PLN_006', 'PLN_010'],
            cvs: { PLN_005: '1u 2r', PLN_006: '2r', PLN_010: '2r', PLN_007: '', PLN_008: '' },
        });
        if (!/<div class="pd-grp focus" data-gcard="0"/.test(ctx.root.innerHTML) && !card.classList.contains('focus')) {
            throw new Error('the hovered card is not marked');
        }
        bubble(card, 'mouseout', ctx.doc);
        state('Left Hand left', { full: ['PLN_005', 'PLN_006', 'PLN_007', 'PLN_008', 'PLN_010', 'PLN_012'], cvs: { PLN_005: '' } });
        click(ctx.main, ctx.doc, { grow: 'PLN_010', g: '0' });
        await settle();
        state('the PLN_010 row', { full: ['PLN_005', 'PLN_010'], cvs: { PLN_005: '1u 2r', PLN_010: '2r', PLN_006: '' } });
        bubble(card, 'mouseover', ctx.doc);
        state('a pinned row outlives a hover', { full: ['PLN_005', 'PLN_010'], cvs: { PLN_010: '2r' } });
        click(ctx.main, ctx.doc, { grow: 'PLN_010', g: '0' });
        await settle();
        bubble(card, 'mouseout', ctx.doc);
        state('the row let go', { full: ['PLN_005', 'PLN_006', 'PLN_007', 'PLN_008', 'PLN_010', 'PLN_012'], cvs: { PLN_010: '' } });
    });

    // "ON THE FLOOR" IS THE SAVED CARD'S (SYNTH row 10): generated from the
    // spots, at and carry Go computed, never from the draft.
    await pin('On the floor reads the saved card, and says when it is not saved yet', async () => {
        const saved = p6Page(press6Composer({ grouped: true, dest7: 'ULN_005' }));
        await settle();
        await openRecipe(saved);
        const g = groupsHTML(saved.root);
        if ((g.match(/data-gcard="\d+"/g) || []).length !== 2) throw new Error('a grouped recipe does not open with its cards: ' + plain(g).slice(0, 200));
        const floor = plain(between(saved.root.innerHTML, '<div class="pd-floor"', '</div></div>'));
        const want = 'On the floor Left Hand runs on PLN_006 or PLN_010, on deck PLN_005 · Right Hand on PLN_007 or PLN_012, ' +
            'on deck PLN_008 · nothing else is offered.';
        if (floor !== want) throw new Error('the block reads ' + JSON.stringify(floor) + '\n      want ' + JSON.stringify(want));
        addSpot(saved, 0, 'PLN_013');
        await settle();
        const dirty = plain(between(saved.root.innerHTML, '<div class="pd-floor"', '</div></div>'));
        if (dirty.indexOf(want.slice('On the floor '.length)) < 0 || dirty.indexOf('Your changes reach the floor when you save.') < 0) {
            throw new Error('an edited draft\'s block reads ' + JSON.stringify(dirty));
        }
        const fresh = p6Page(press6Composer());
        await settle();
        await openRecipe(fresh);
        click(fresh.main, fresh.doc, { act: 'groups-setup' });
        await settle();
        const unsaved = plain(between(fresh.root.innerHTML, '<div class="pd-floor"', '</div></div>'));
        const words = 'On the floor Not saved yet: what the floor can pick is said here once these groups are saved. ' +
            'Until then the floor runs this recipe without groups.';
        if (unsaved !== words) throw new Error('an unsaved block reads ' + JSON.stringify(unsaved));
    });

    await pin('On the floor says when the running style stands outside the groups', async () => {
        const ctx = p6Page(press6Composer({ grouped: true, dest7: 'ULN_005',
            running: [p6claim('PLN_013', 'PLN_005', 'SYN-P6-A', 'ULN_002'), p6claim('PLN_012', 'PLN_008', 'SYN-P6-B', 'ULN_005')] }));
        await settle();
        await openRecipe(ctx);
        const floor = plain(between(ctx.root.innerHTML, '<div class="pd-floor"', '</div></div>'));
        if (floor.indexOf('The running style SYN-RUN stands outside these groups: PLN_013 is in none of them.') < 0) {
            throw new Error('the block reads ' + JSON.stringify(floor));
        }
        const inside = p6Page(press6Composer({ grouped: true, dest7: 'ULN_005' }));
        await settle();
        await openRecipe(inside);
        if (/running style/.test(inside.root.innerHTML)) throw new Error('a running style inside its groups is reported');
    });

    // THE SHOTS HARNESS'S ONE-SHOTS: every state photographed is a URL.
    await pin('#tab=presets;recipe= opens the draft, and ;groups= ;focus= ;row= ;gadv= reach each state', async () => {
        const a = p6Page(press6Composer(), { hash: '#tab=presets;recipe=7;groups=1;focus=Right Hand' });
        await settle();
        if (a.root.innerHTML.indexOf('Recipe · Two spot press index') < 0) throw new Error(';recipe= opened no draft');
        if (!/<div class="pd-grp focus" data-gcard="1"/.test(a.root.innerHTML)) throw new Error(';groups=1;focus=Right Hand did not focus Right Hand');
        if (a.doc.body.dataset.pdRecipe !== 'draft;groups=2;focus=Right Hand') throw new Error('marker ' + JSON.stringify(a.doc.body.dataset.pdRecipe));
        const b = p6Page(press6Composer({ grouped: true, dest7: 'ULN_005' }), {
            hash: '#tab=presets;recipe=7;row=PLN_010;gadv=Left Hand',
            extraIds: { 'pd-advpop': Object.assign(makeElement('pd-advpop'), { hidden: true }) },
        });
        await settle();
        if (!/<tr class="focus" data-grow="PLN_010"/.test(b.root.innerHTML)) throw new Error(';row=PLN_010 did not click the row');
        if (b.scrim.innerHTML.indexOf('<h2>Left Hand · Advanced</h2>') < 0) throw new Error(';gadv=Left Hand opened no sheet');
        if (b.doc.body.dataset.pdRecipe !== 'draft;groups=2;floor=saved;row=PLN_010;gadv=Left Hand') {
            throw new Error('marker ' + JSON.stringify(b.doc.body.dataset.pdRecipe));
        }
        if (b.win.replaceStates.some(u => /recipe=|groups=|focus=|row=|gadv=/.test(u))) {
            throw new Error('a one-shot was minted back into the address: ' + b.win.replaceStates.join(' | '));
        }
    });

    // ── New style's Spots step (F6) ──
    //
    // After a part per slot: the same relation picture and the same
    // spotsTap / spotsLit the station runs, over the recipe's card, opened on
    // its last_moves. The mint sends the moves as overrides, and its body is
    // the station's mintBody bytes with the desktop's node per part.
    await pin('flowMint is the station\'s mintBody bytes for the same picks and moves', async () => {
        const B = require(path.join(__dirname, 'desktop-bodies.js'));
        const modelSrc = fs.readFileSync(path.join(__dirname, '..', '..', 'operator-station', 'composer-model.js'), 'utf8');
        const M = vm.runInThisContext('(function(module){' + modelSrc + '\nreturn module.exports;})', { filename: 'composer-model.js' })({ exports: {} });
        const moves = { PLN_006: 'PLN_010', PLN_007: 'PLN_012' };
        for (const mv of [moves, {}]) {
            const a = JSON.stringify(B.flowMint(7, [{ code: 'SYN-P6-A' }, { code: 'SYN-P6-B' }], 18, mv));
            const b = JSON.stringify(M.mintBody(7, ['SYN-P6-A', 'SYN-P6-B'], 18, mv));
            if (a !== b) throw new Error('moves ' + JSON.stringify(mv) + ': flowMint ' + a + '\n      mintBody ' + b);
        }
    });

    const newStylePage = (composer, hash) => {
        const ids = {};
        for (const id of ['pd-newrecipe', 'pd-newslots', 'pd-newname', 'pd-newspots']) ids[id] = makeElement(id);
        const ctx = p6Page(composer, { hash: hash, realPicture: true, extraIds: ids });
        const slot = (i, code) => Object.assign(makeElement(''), { value: code, type: 'select' });
        const fields = { '[data-f="slot0"]': slot(0, 'SYN-P6-A'), '[data-f="slot1"]': slot(1, 'SYN-P6-B'),
            '[data-f="recipe"]': Object.assign(makeElement(''), { value: '7' }), '[data-f="catid"]': Object.assign(makeElement(''), { value: '' }),
            '.mf .st': makeElement('') };
        ctx.scrim.querySelector = sel => fields[sel] || null;
        const tap = node => {
            const box = makeElement('pd-nsvg');
            ctx.scrim.appendChild(box);
            const c = makeElement('');
            c.dataset.pos = node;
            box.appendChild(c);
            bubble(c, 'click', ctx.doc);
        };
        return Object.assign(ctx, { ids: ids, tap: tap, status: fields['.mf .st'] });
    };
    await pin('New style asks where the spots go, opened where the cell last ran the recipe', async () => {
        const ctx = newStylePage(press6Composer(), '#newstyle=7');
        await settle();
        if (!ctx.scrim.classList.contains('active')) throw new Error(';newstyle=7 opened no sheet');
        const spots = ctx.ids['pd-newspots'].innerHTML;
        if (spots.indexOf('id="pd-nsvg"') < 0) throw new Error('the sheet draws no Spots picture: ' + plain(spots).slice(0, 200));
        if (plain(spots).indexOf('PLN_006 → PLN_010') < 0 || plain(spots).indexOf('PLN_007 → PLN_012') < 0) {
            throw new Error('the Spots step is not pre-filled from last_moves: ' + plain(spots));
        }
        const o = ctx.win.pictureCalls[ctx.win.pictureCalls.length - 1] || {};
        if (o.relation !== true || o.editable !== true) throw new Error('the Spots picture is not the relation picture, tappable');
        ctx.tap('PLN_010');
        const lit = ctx.win.pictureCalls[ctx.win.pictureCalls.length - 1] || {};
        if (JSON.stringify(lit.targets) !== '["PLN_006","PLN_009"]' || lit.selected !== 'PLN_010' || !lit.selectCard) {
            throw new Error('a tap on PLN_010 lit ' + JSON.stringify(lit.targets) + ' selected ' + lit.selected);
        }
        ctx.tap('PLN_009');
        if (plain(ctx.ids['pd-newspots'].innerHTML).indexOf('PLN_006 → PLN_009') < 0) throw new Error('the move did not land: ' + plain(ctx.ids['pd-newspots'].innerHTML));
        if (ctx.doc.body.dataset.pdNewStyle !== 'spots;moves=2') throw new Error('marker ' + JSON.stringify(ctx.doc.body.dataset.pdNewStyle));
    });

    await pin('the New style mint sends the Spots step\'s moves as overrides, the station\'s bytes', async () => {
        const ctx = newStylePage(press6Composer(), '#newstyle=7');
        await settle();
        click(ctx.scrim, ctx.doc, { act: 'sheet-ok' });
        await settle();
        const posted = ctx.posts.find(p => /\/flow\/mint$/.test(p.url));
        if (!posted) throw new Error('Create minted nothing: ' + ctx.status.textContent);
        const want = ctx.win.ComposerModel.mintBody(7, ['SYN-P6-A', 'SYN-P6-B'], 18, { PLN_006: 'PLN_010', PLN_007: 'PLN_012' });
        want.parts[0].node = 'PLN_010';
        want.parts[1].node = 'PLN_012';
        if (posted.body !== JSON.stringify(want)) throw new Error('posted ' + posted.body + '\n      want   ' + JSON.stringify(want));
    });

    // USE A RECIPE draws the relation picture of what it would lay on.
    await pin('Use a recipe shows the shape on the relation picture', async () => {
        const ctx = p6Page(press6Composer(), { realPicture: true });
        await settle();
        click(ctx.main, ctx.doc, { act: 'use-preset-pick', preset: '7' });
        if (!ctx.scrim.classList.contains('active')) throw new Error('Use a recipe opened no sheet');
        if (ctx.scrim.innerHTML.indexOf('id="pd-usvg"') < 0) throw new Error('the Use sheet draws no picture');
        const o = ctx.win.pictureCalls[ctx.win.pictureCalls.length - 1] || {};
        if (o.relation !== true) throw new Error('the Use sheet\'s picture is not by relation');
        if (JSON.stringify((o.active || []).slice().sort()) !== '["PLN_005","PLN_008","PLN_010","PLN_012"]') {
            throw new Error('the Use sheet draws full ' + JSON.stringify(o.active));
        }
    });
}

// ── Settings · Positions and the Add positions checklist ─────────────────────
//
// Press 6 with its two screens: the press's own (18) and the Unloader (25).
// Press 4 holds PLN_001; the Bin Loader holds SMN_001, a Core loader's window.
// The screen's list the SERVER answers carries PLN_009BCD, which the page was
// not loaded with: an add that sent the page's own list would drop it.
async function settingsPositionsPins(h) {
    const { pin, okJSON, click, wireSheet, settle } = h;
    const live = () => [
        { id: 87, process_id: 1, process_name: 'Press 6', operator_station_id: 18, station_name: 'Press 6 Operator Screen',
            core_node_name: 'PLN_005', name: 'PLN_005', sequence: 1, enabled: true },
        { id: 127, process_id: 1, process_name: 'Press 6', operator_station_id: 18, station_name: 'Press 6 Operator Screen',
            core_node_name: 'PLN_006BCD', name: 'PLN_006BCD', sequence: 2, enabled: true },
        { id: 144, process_id: 1, process_name: 'Press 6', operator_station_id: 25, station_name: 'Unloader',
            core_node_name: 'ULN_002', name: 'ULN_002', sequence: 1, enabled: true },
        { id: 89, process_id: 1, process_name: 'Press 6', operator_station_id: null, station_name: '',
            core_node_name: 'PLN_009', name: 'PLN_009', sequence: 3, enabled: true },
        { id: 50, process_id: 2, process_name: 'Press 4', operator_station_id: 30, station_name: 'P400',
            core_node_name: 'PLN_001', name: 'PLN_001', sequence: 1, enabled: true },
        { id: 60, process_id: 3, process_name: 'Bin Loader', operator_station_id: 8, station_name: 'Bin Loader',
            core_node_name: 'SMN_001', name: 'SMN_001', sequence: 1, enabled: true },
    ];
    const coreNodes = [
        { name: 'PLN_001', node_type: 'storage' }, { name: 'PLN_005', node_type: 'storage' },
        { name: 'PLN_005PAL', node_type: 'storage' }, { name: 'PLN_006BCD', node_type: 'storage' },
        { name: 'PLN_006PAL', node_type: 'storage' }, { name: 'PLN_009', node_type: 'storage' },
        { name: 'SMN_001', node_type: 'storage', shared_loader: true },
        { name: 'SMN_002', node_type: 'storage' }, { name: 'ULN_002', node_type: 'storage' },
        { name: 'Press Unloader', node_type: 'NGRP', members: ['ULN_002', 'ULN_005'] },
    ];
    const claimed = { 18: ['PLN_005', 'PLN_006BCD', 'PLN_009BCD'], 25: ['ULN_002'] };
    // One Press 6 page on Settings: every write recorded, the claimed-nodes
    // read answered from `claimed` unless `readFails`, the impact from `warning`.
    const page = (hash, o) => {
        o = o || {};
        const writes = [];
        const ctx = loadPage({
            allowSheet: true, hash: hash,
            processes: [{ id: 1, name: 'Press 6', group_id: 0, active_style_id: 1, flow_composer_enabled: true }],
            stations: [{ id: 18, process_id: 1, name: 'Press 6 Operator Screen' }, { id: 25, process_id: 1, name: 'Unloader' }],
            stationNodes: { 18: ['PLN_005', 'PLN_006BCD'], 25: ['ULN_002'] },
            fetch: (url, init) => {
                const method = (init && init.method) || 'GET';
                if (url === '/api/process-nodes') return okJSON(live());
                if (url === '/api/core-nodes') return okJSON(coreNodes);
                const m = /^\/api\/operator-stations\/(\d+)\/claimed-nodes(\/impact)?$/.exec(url);
                if (!m) return null;
                if (method === 'GET') {
                    return o.readFails ? Promise.resolve({ ok: false, status: 500, json: async () => ({}) })
                        : okJSON(claimed[m[1]] || []);
                }
                writes.push({ method: method, url: url, body: JSON.parse(init.body) });
                if (m[2]) return okJSON({ warning: o.warning || '' });
                return okJSON({ status: 'ok', kept: [] });
            },
        });
        return Object.assign(ctx, { writes: writes });
    };
    const rowOf = (html, name) => (html.match(new RegExp('<button[^>]*>(?:<i class="box"></i>)?' + name + '(?:<small>[^<]*</small>)?</button>')) || [''])[0];

    await pin('Settings lists each position under its screen, with its curtain switch and a Remove', async () => {
        const ctx = page('#tab=settings');
        await settle();
        const html = ctx.root.innerHTML;
        if (html.indexOf('<h2>Positions</h2>') < 0 || html.indexOf('data-act="pos-add"') < 0) {
            throw new Error('Settings draws no Positions section with Add positions');
        }
        if (html.indexOf('<h2>Light curtain interlock</h2>') >= 0) throw new Error('the curtain section is still its own heading');
        const press = html.indexOf('<div class="pd-lbl pd-posscreen">Press 6 Operator Screen</div>');
        const unloader = html.indexOf('<div class="pd-lbl pd-posscreen">Unloader</div>');
        if (press < 0 || unloader < 0) throw new Error('the rows are not grouped under their two screens');
        const at = n => html.indexOf('data-act="pos-remove" data-node="' + n + '"');
        if (!(press < at('PLN_005') && at('PLN_005') < unloader && unloader < at('ULN_002'))) {
            throw new Error('PLN_005 is not under the press\'s screen, or ULN_002 not under the Unloader');
        }
        if (html.indexOf('data-act="pos-remove" data-node="PLN_005" data-station="18"') < 0) throw new Error('PLN_005\'s Remove does not name screen 18');
        if (html.indexOf('data-act="cu-toggle" data-node="87"') < 0) throw new Error('PLN_005 has no curtain switch');
        const loose = html.indexOf('<div class="pd-lbl pd-posscreen">On no screen</div>');
        if (loose < unloader || html.indexOf('data-act="cu-toggle" data-node="89"') < loose) throw new Error('PLN_009 is not under On no screen');
        if (html.indexOf('data-act="pos-remove" data-node="PLN_009"') >= 0) throw new Error('a position on no screen offers a Remove it cannot do');
    });

    await pin('Add positions greys what is a position already, names whose, and offers a loader window', async () => {
        const ctx = page('#tab=settings;addpos=');
        await settle();
        const s = ctx.scrim.innerHTML;
        if (!ctx.scrim.classList.contains('active') || s.indexOf('<h2>Add positions</h2>') < 0) throw new Error(';addpos= opened no sheet');
        const want = {
            PLN_001: '<button class="off" disabled><i class="box"></i>PLN_001<small>on Press 4</small></button>',
            PLN_005: '<button class="off" disabled><i class="box"></i>PLN_005<small>already on Press 6 Operator Screen</small></button>',
            ULN_002: '<button class="off" disabled><i class="box"></i>ULN_002<small>already on Unloader</small></button>',
        };
        for (const n of Object.keys(want)) {
            if (rowOf(s, n) !== want[n]) throw new Error(n + ' draws ' + JSON.stringify(rowOf(s, n)) + '\n      want ' + want[n]);
        }
        if (!/data-npkname="SMN_001"><i class="box"><\/i>SMN_001<small>loader window · also on Bin Loader<\/small>/.test(s)) {
            throw new Error('the loader window is not offered with its note: ' + JSON.stringify(rowOf(s, 'SMN_001')));
        }
        if (!/data-npk="add"[^>]*data-npkname="PLN_005PAL"/.test(s)) throw new Error('PLN_005PAL is not tickable');
        if (!/data-npkname="PLN_009"><i class="box"><\/i>PLN_009<small>on no screen yet<\/small>/.test(s)) {
            throw new Error('a position on no screen is not offered to put on one: ' + JSON.stringify(rowOf(s, 'PLN_009')));
        }
        if (s.indexOf('Press Unloader') >= 0) throw new Error('a group is offered as a box');
        const pres = (s.match(/data-npk="prefix" data-npkkey="addpos" data-npkname="(\w+)"/g) || []).map(x => x.replace(/.*"(\w+)"$/, '$1'));
        if (pres.join(' ') !== 'PLN SMN ULN') throw new Error('prefixes ' + JSON.stringify(pres));
        if (/data-npk="(all|none)"/.test(s)) throw new Error('the checklist offers a tick-all');
        if (s.indexOf('data-act="pos-screen" data-station="25"') < 0) throw new Error('two screens, and no chooser');
    });

    await pin('a prefix filters the checklist to its family and only filters', async () => {
        const ctx = page('#tab=settings;addpos=PLN_');
        await settle();
        const s = ctx.scrim.innerHTML;
        if (s.indexOf('PLN_005PAL') < 0 || s.indexOf('>SMN_002<') >= 0) throw new Error('PLN_ did not filter to PLN: ' + s.length);
        if (!/<button class="on" data-npk="prefix" data-npkkey="addpos" data-npkname="PLN">/.test(s)) throw new Error('PLN is not marked as the filter');
        click(ctx.scrim, ctx.doc, { npk: 'prefix', npkkey: 'addpos', npkname: 'PLN' });
        click(ctx.scrim, ctx.doc, { npk: 'prefix', npkkey: 'addpos', npkname: 'SMN' });
        wireSheet(ctx.scrim, '');
        click(ctx.scrim, ctx.doc, { act: 'sheet-ok' });
        await settle();
        if (ctx.writes.length) throw new Error('pressing prefixes ticked something: ' + JSON.stringify(ctx.writes));
    });

    await pin('Add sends the screen\'s list as the server answers it, plus the ticked nodes', async () => {
        const ctx = page('#tab=settings;addpos=');
        await settle();
        click(ctx.scrim, ctx.doc, { npk: 'add', npkkey: 'addpos', npkname: 'PLN_005PAL' });
        click(ctx.scrim, ctx.doc, { npk: 'add', npkkey: 'addpos', npkname: 'PLN_006PAL' });
        click(ctx.scrim, ctx.doc, { act: 'sheet-ok' });
        await settle();
        const put = ctx.writes.find(w => w.method === 'PUT');
        if (!put || put.url !== '/api/operator-stations/18/claimed-nodes') throw new Error('Add wrote ' + JSON.stringify(ctx.writes));
        const want = '["PLN_005","PLN_006BCD","PLN_009BCD","PLN_005PAL","PLN_006PAL"]';
        if (JSON.stringify(put.body.nodes) !== want) throw new Error('Add sent ' + JSON.stringify(put.body.nodes) + '\n      want ' + want);
        if (ctx.scrim.classList.contains('active')) throw new Error('the sheet is still open');
        const says = '<div class="pd-notice">Added PLN_005PAL, PLN_006PAL to Press 6 Operator Screen.</div>';
        if (ctx.root.innerHTML.indexOf(says) < 0) throw new Error('Settings does not say ' + says);
    });

    await pin('the screen chooser sends the ticks to the screen picked', async () => {
        const ctx = page('#tab=settings;addpos=');
        await settle();
        click(ctx.scrim, ctx.doc, { act: 'pos-screen', station: '25' });
        click(ctx.scrim, ctx.doc, { npk: 'add', npkkey: 'addpos', npkname: 'SMN_002' });
        click(ctx.scrim, ctx.doc, { act: 'sheet-ok' });
        await settle();
        const put = ctx.writes.find(w => w.method === 'PUT');
        if (!put || put.url !== '/api/operator-stations/25/claimed-nodes' || JSON.stringify(put.body.nodes) !== '["ULN_002","SMN_002"]') {
            throw new Error('the Unloader add wrote ' + JSON.stringify(ctx.writes));
        }
    });

    await pin('a screen list that cannot be read adds nothing', async () => {
        const ctx = page('#tab=settings;addpos=', { readFails: true });
        await settle();
        const status = wireSheet(ctx.scrim, '');
        click(ctx.scrim, ctx.doc, { npk: 'add', npkkey: 'addpos', npkname: 'PLN_005PAL' });
        click(ctx.scrim, ctx.doc, { act: 'sheet-ok' });
        await settle();
        if (ctx.writes.length) throw new Error('an add over an unread list was sent: ' + JSON.stringify(ctx.writes));
        if (status.textContent.indexOf('could not be read, so nothing was added') < 0) throw new Error('the sheet says ' + JSON.stringify(status.textContent));
    });

    await pin('Remove asks with what still names the position, then sends the list without it', async () => {
        const says = 'PLN_005 is named by recipe Two up.';
        const ctx = page('#tab=settings', { warning: says });
        await settle();
        click(ctx.root, ctx.doc, { act: 'pos-remove', node: 'PLN_005', station: '18' });
        await settle();
        const ask = ctx.writes.find(w => /\/impact$/.test(w.url));
        if (!ask || JSON.stringify(ask.body.nodes) !== '["PLN_006BCD","PLN_009BCD"]') throw new Error('the impact ask was ' + JSON.stringify(ask));
        if (ctx.scrim.innerHTML.indexOf('<h2>Remove PLN_005</h2>') < 0 || ctx.scrim.innerHTML.indexOf(says) < 0) {
            throw new Error('the question does not carry the warning: ' + ctx.scrim.innerHTML.slice(0, 300));
        }
        if (ctx.writes.some(w => w.method === 'PUT')) throw new Error('Remove wrote before it was confirmed');
        click(ctx.scrim, ctx.doc, { act: 'sheet-ok' });
        await settle();
        const put = ctx.writes.find(w => w.method === 'PUT');
        if (!put || JSON.stringify(put.body.nodes) !== '["PLN_006BCD","PLN_009BCD"]') throw new Error('Remove sent ' + JSON.stringify(put));
        if (ctx.root.innerHTML.indexOf('<div class="pd-notice">Removed PLN_005 from Press 6 Operator Screen.</div>') < 0) {
            throw new Error('Settings does not say what was removed');
        }
    });
}

mainAsync().catch(e => {
    console.error('FAIL (harness): ' + (e && e.message));
    nodeProcess.exit(1);
});
