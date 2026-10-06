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
    global.requestAnimationFrame = () => {};
    global.CSS = { escape: s => String(s).replace(/[^a-zA-Z0-9_-]/g, c => '\\' + c) };
    // The picture and the map are stubbed to fail loudly if they ever run: the
    // pin's path draws the bar but never the picture (no #pd-svg exists).
    global.renderFlowPicture = () => { throw new Error('drawPicture must not run in this pin'); };
    // The row words are the table's (front/back under a position), so they
    // are read whenever the table draws; a stub cell has no rows to name.
    global.pictureRows = () => ({});
    global.sentencesFromModel = () => { throw new Error('sentencesFromModel must not run in this pin'); };
    global.moduleOf = () => { throw new Error('moduleOf must not run in this pin'); };
    for (const fn of ['makeProjector', 'rotate90For', 'dist2', 'cubicLength', 'cubicPathD', 'laneKey']) {
        global[fn] = () => { throw new Error(fn + ' must not run in this pin'); };
    }
    win.pictureCalls = [];
    if (loadOpts.realPicture) {
        // The picture pins draw for real: operator-flow.js's own renderer,
        // row words, sentences and module membership, so what the page sizes
        // its frame from is the height the picture itself reported.
        const flow = loadFlow();
        global.renderFlowPicture = (view, opts) => {
            const markup = flow.renderFlowPicture(view, opts);
            win.pictureCalls.push(opts);
            return markup;
        };
        global.pictureRows = flow.pictureRows;
        global.sentencesFromModel = flow.sentencesFromModel;
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

    return { root, pop, main, doc, win, scrim };
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
    vm.runInContext(src + '\n__out = { renderFlowPicture, pictureRows, sentencesFromModel, ' +
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

    // ── selection: a card finds its row, a second click or Escape lets go ───
    //
    // The desktop picture never edits. A card click selects the position and
    // scrolls its table row into view, through the same path as the bar's
    // problem line; clicking the selected card again, or Escape with nothing
    // else open, clears the selection. A real DOM drops and re-parses the
    // tree on every drawFlows; the stub keeps appended elements, so the pin
    // reads the selection back from the markup drawFlows wrote.
    function selectionRig() {
        // The table's scroller and its header row: a header 43 px tall, the
        // height it renders at 1280 with its two-line headings.
        const box = makeElement('pd-postbl');
        const thead = makeElement('');
        thead.offsetHeight = 43;
        box.querySelector = sel => (sel === 'thead' ? thead : null);
        const ctx = loadPage({ extraIds: { 'pd-postbl': box } });
        ctx.box = box;
        const { root, main } = ctx;
        const svg = makeElement('pd-svg');
        main.appendChild(svg);
        const card = makeElement('');
        card.dataset.pos = 'PLN_01';
        svg.appendChild(card);
        const row = makeElement('');
        row.dataset.row = 'PLN_01';
        main.appendChild(row);
        const rootQuery = root.querySelector;
        root.querySelector = sel => (sel === '[data-row="PLN_01"]' ? row : (rootQuery ? rootQuery.call(root, sel) : null));
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
        // The header row is sticky, so a row brought into view at the top of
        // the scroller would land under it: the scroller reserves the header's
        // own height before the row is scrolled to.
        if (box.style.scrollPaddingTop !== '43px') {
            throw new Error('a card click scrolled its row without reserving the 43 px sticky header; ' +
                'scroll-padding-top was ' + JSON.stringify(box.style.scrollPaddingTop));
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
        // A real innerHTML write replaces .pd-main and .pd-postbl with fresh
        // elements scrolled to the top. The stub mints a fresh pair per write.
        let gen = 0;
        const fresh = {};
        const at = sel => {
            const k = gen + sel;
            if (!fresh[k]) fresh[k] = Object.assign(makeElement(''), { scrollTop: 0 });
            return fresh[k];
        };
        let html = root.innerHTML;
        Object.defineProperty(root, 'innerHTML', {
            get: () => html,
            set: v => { html = v; gen++; },
        });
        const rootQuery = root.querySelector;
        root.querySelector = sel => (sel === '.pd-main' || sel === '.pd-postbl' ? at(sel)
            : (rootQuery ? rootQuery.call(root, sel) : null));
        at('.pd-main').scrollTop = 140;
        at('.pd-postbl').scrollTop = 60;
        const row = makeElement('');
        row.dataset.row = 'PLN_01';
        main.appendChild(row);
        const before = gen;
        bubble(row, 'click', doc);
        if (gen === before) throw new Error('setup: the row click did not redraw');
        if (at('.pd-main').scrollTop !== 140 || at('.pd-postbl').scrollTop !== 60) {
            throw new Error('drawFlows lost the scroll: .pd-main ' + at('.pd-main').scrollTop +
                ', .pd-postbl ' + at('.pd-postbl').scrollTop + ' (want 140, 60)');
        }
        checks++;
        console.log('ok: a redraw of the flow keeps the page and the table where they were scrolled');
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
        const want = '<div class="none">required for 1‑robot swap</div>' +
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
        console.log('ok: the staging menu says "required for 1‑robot swap" and marks a node another position uses');
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
    // Picks "2‑robot swap" off the mode chip's own menu, by real clicks.
    function pickTwoRobot(main, pop, doc) {
        const opts = wireOptions(pop);
        bubble(chip(main, 'PLN_01', 'mode'), 'click', doc);
        const two = pop.innerHTML.match(/data-opt="(\d+)"(?:(?!<\/button>).)*2‑robot swap<\/button>/);
        if (!two) throw new Error('setup: the mode menu has no 2‑robot swap: ' + pop.innerHTML);
        bubble(opts.find(b => b.dataset.opt === two[1]), 'click', doc);
    }

    await pin('the subtitle names the draft\'s mode, not the saved one', async () => {
        const { root, pop, main, doc } = loadPage({ flowspec: true, composer: savedComposer() });
        await settle();
        const before = '<div class="sub">1‑robot swap · 1 position · last run 09-12';
        if (root.innerHTML.indexOf(before) < 0) throw new Error('setup: the subtitle is not the saved flow\'s');
        pickTwoRobot(main, pop, doc);
        const want = '<div class="sub">2‑robot swap · 1 position · last run 09-12';
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

    await pin('Save as preset names the preset it saved and where it is', async () => {
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
        const want = '<div class="pd-notice">Saved preset Single up. It is on the Presets tab.</div>';
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

    // ── the Advanced sheet, Presets and Generate say what they do ───────────
    //
    // The field's name is the flowspec table's; the help under it is this
    // page's and says part. The reorder point's source pill is drawn only for
    // a source that tells the engineer something: a number nobody stamped is
    // just a number. Presets count the parts a preset was applied to, and say
    // so; a shape whose positions run different modes has a mark of its own.
    // Generate's help says what the cells open on.
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

    const genPage = (claims, extraIds) => loadPage({
        allowSheet: true, composer: savedComposer(), hash: '#tab=settings', extraIds,
        fetch: url => {
            if (url === '/api/payload-catalog') return okJSON([{ code: 'PART-B' }]);
            if (url === '/api/styles/1/node-claims') return okJSON(claims);
            return null;
        },
    });

    await pin('Generate says every cell starts on the base part', async () => {
        const { root, doc, scrim } = genPage([
            { core_node_name: 'PLN_01', swap_mode: 'single_robot', payload_code: 'PART-A' },
            { core_node_name: 'PLN_02', swap_mode: 'single_robot', payload_code: 'PART-A' },
        ]);
        await settle();
        click(root, doc, { act: 'st-generate' });
        await settle();
        if (!scrim.classList.contains('active')) throw new Error('setup: the generate dialog did not open');
        const want = '<p>One new style per row, stamped out of a base that already runs. Every cell ' +
            'starts on the base style’s part; change the ones that differ.</p>';
        if (scrim.innerHTML.indexOf(want) < 0) {
            throw new Error('the generate help reads ' + JSON.stringify((scrim.innerHTML.match(/<div class="mh">.*?<\/div>/) || [''])[0]));
        }
        if (/inherit/.test(scrim.innerHTML)) throw new Error('the generate dialog still says "inherit"');
    });

    await pin('Generate over a base with no flow says there is no part to set', async () => {
        const { root, doc, scrim } = genPage([]);
        await settle();
        click(root, doc, { act: 'st-generate' });
        await settle();
        const want = 'Style A has no flow yet, so there is nothing to set a part on.';
        if (scrim.innerHTML.indexOf(want) < 0) throw new Error('the empty-base note does not say ' + JSON.stringify(want));
    });

    // ── Escape on Advanced and Generate asks before it discards typing ──────
    //
    // The sheet's question, on the two dialogs that draw their own markup into
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
    const openGen = async () => {
        const page = genPage([{ core_node_name: 'PLN_01', swap_mode: 'single_robot', payload_code: 'PART-A' }],
            { 'pd-advpop': advpopStub() });
        await settle();
        const live = liveInputs(page.scrim);
        click(page.root, page.doc, { act: 'st-generate' });
        await settle();
        if (!page.scrim.classList.contains('active')) throw new Error('setup: the generate dialog did not open');
        return Object.assign(page, { live });
    };
    // The dialogs' pickers, opened by a real click: Advanced's carry-over
    // picker, and Generate's base-style picker inside its .pd-modal.
    const openAdvPick = (scrim, doc) => click(scrim, doc, { advkind: 'pick', adv: 'changeover_carryover_disposition' });
    const openGenPick = (scrim, doc) => {
        const modal = makeElement('');
        modal.classList.add('pd-modal');
        scrim.appendChild(modal);
        click(modal, doc, { act: 'pick', kind: 'gen-base' });
    };
    const dialogs = [
        ['Advanced', openAdv, 'PLN_01 · Advanced', l => l.find('data-adv', 'reorder_point'), '40',
            (scrim, doc) => click(scrim, doc, { advkind: 'toggle', adv: 'evacuate_on_changeover' })],
        ['Generate', openGen, '<h2>Generate variants</h2>', l => l.find('data-genrow', '0'), 'SYN-PART-09',
            (scrim, doc) => click(scrim, doc, { act: 'gen-add' })],
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
    // or Generate, the first Escape closes only the picker — the dialog and
    // anything typed into it stay — and the second follows the dialog's rule.
    for (const [name, open, pick, field, typed] of [
        ['Advanced', openAdv, openAdvPick, l => l.find('data-adv', 'reorder_point'), '40'],
        ['Generate', openGen, openGenPick, l => l.find('data-genrow', '0'), 'SYN-PART-09'],
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
        if (parts !== 1 || nodes !== 4) {
            throw new Error('want 1 part placeholder and 4 node placeholders, got ' + parts + ' and ' + nodes);
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

    // THE GATE IS A SENTENCE IN THE HEADER'S OWN TEXT STYLE. It is decided in
    // Settings and only read here, so it is drawn as words — no pill border,
    // no chip, no second door to the Settings tab.
    await pin('the app bar states the gate as plain text', async () => {
        for (const [off, words] of [[false, 'Operators may change flows'], [true, 'Operators run flows as set up']]) {
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
