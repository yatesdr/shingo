// composer-strip-unplaced-chip.test.js — the strip's unplaced chip is a door.
//
// An unplaced part used to render as a span that said `· unplaced` and offered
// nothing: the only way to take the part back off the style was to re-pick it
// from a position. The chip is a button now, carrying the same removePart verb
// the part popover's "take off this position" uses — and because a chip in the
// strip sits on NO position, the tap just leaves the set.
//
// WHAT IS REAL: composer-render.js booted through its own hash entry
// (openFromHash), the real ComposerModel underneath it, the real drawStrip →
// drawComposer → drawStrip cycle, and the real onClick handler pulled off the
// root the way boot binds it. The tap goes through the page's own switch on
// data-act.
//
// WHAT IS STUBBED: the HTML parser, exactly as
// processes-desktop.popover-acts.test.js argues it — the page builds its
// markup by string concatenation, so the chip under test is asserted against
// the string drawStrip wrote and then re-typed as an element carrying those
// same attributes for the click. The three operator-flow.js draw functions are
// recorders: the picture-redraw count is the pin's proof that a strip tap took
// the ONE DRAW PER TAP path (drawPicture, no panel) and not the panel path.
//
// Run by www/composer_strip_unplaced_chip_test.go.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

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

// ── the model, for real ──────────────────────────────────────────────────────

const modelSrc = fs.readFileSync(
    path.join(__dirname, 'composer-model.js'), 'utf8');
const modelFactory = vm.runInThisContext(
    '(function(module, exports, console){' + modelSrc + '\nreturn module.exports;})',
    { filename: 'composer-model.js' });
const modelMod = { exports: {} };
const ComposerModel = modelFactory(modelMod, modelMod.exports, console);

// esc, for real — it is one function and the strip's markup is asserted as text.
const escSrc = fs.readFileSync(
    path.join(__dirname, '..', '..', '..', '..', 'shared', 'esc.js'), 'utf8');
const escFactory = vm.runInThisContext(
    '(function(module, exports, console){' +
    escSrc.replace(/^import .*$/gm, '').replace(/export\s+function\s+esc\(/, 'function esc(') +
    '\nreturn { esc };})',
    { filename: 'esc.js' });
const esc = escFactory({}, {}, console).esc;

// ── a root, an element map, and picture-draw recording ───────────────────────

function makeEl(id) {
    return {
        id: id || '',
        hidden: true,
        innerHTML: '',
        dataset: {},
        style: {},
        scrollHeight: 0, scrollTop: 0, clientHeight: 0, clientWidth: 0,
        classList: {
            add() {}, remove() {}, contains: () => false,
            toggle() {},
        },
        children: [],
        parentNode: null,
        listeners: {},
        addEventListener(type, fn) { (this.listeners[type] = this.listeners[type] || []).push(fn); },
        appendChild(c) { c.parentNode = this; this.children.push(c); return c; },
        remove() { this.removed = true; },
        querySelector: () => null,
        querySelectorAll: () => [],
        getBoundingClientRect: () => ({ left: 0, top: 0, width: 0, height: 0 }),
    };
}

const els = {};
for (const id of ['os-comp-strip', 'os-comp-svg', 'os-comp-bar', 'os-comp-pop', 'os-comp-stage']) {
    els[id] = makeEl(id);
}
let composerRoot = null;
const doc = {
    readyState: 'complete',
    listeners: {},
    body: {
        dataset: {},
        // root() creates and BINDS #os-composer itself (the template does not
        // carry it), so the harness catches it here rather than pre-making it —
        // a pre-made root would come back from root() with no click listener
        // on it, and the tap would go nowhere. It is ALSO registered by id:
        // every later root() call goes through getElementById, and a second
        // element here would make close() hide a different node than the one
        // the draws filled.
        appendChild(n) { composerRoot = n; if (n.id) els[n.id] = n; return n; },
    },
    getElementById: id => els[id] || null,
    createElement: tag => makeEl(''),
    // The Escape pins drive the keydown listener the module registers here at
    // load, so it is recorded and not swallowed.
    addEventListener(type, fn) { (this.listeners[type] = this.listeners[type] || []).push(fn); },
    querySelector: () => null,
    querySelectorAll: () => [],
};

let pictureDraws = 0;
const win = {
    location: { search: '', hash: '', pathname: '/operator/station/5' },
    addEventListener() {},
};
Object.assign(global, {
    window: win,
    document: doc,
    esc,
    // The picture is operator-flow.js's, which this pin does not load: the
    // strip tap's proof is the COUNT, not the SVG.
    renderFlowPicture: () => { pictureDraws++; return ''; },
    pictureRows: () => ({}),
    sentencesFromModel: () => [],
    setTimeout: () => 0,
    clearTimeout: () => {},
    // placePop's settle-then-place; synchronous here is fine — the stub has
    // no layout to wait for.
    requestAnimationFrame: fn => fn(),
});

// ── the render module, booted through its own hash entry ─────────────────────

const VIEW = {
    process: { id: 1, active_style_id: 7, flow_composer_enabled: true },
    station: { id: 5 },
    composer: {
        styles: [{
            id: 7, name: 'Press A',
            claims: [{ core_node_name: 'PLN_01', payload_code: 'PART-A' }],
            parts: ['PART-A', 'PART-B'],
        }],
        palette: ['PART-A', 'PART-B'],
    },
    cell: { positions: [{ core_node_name: 'PLN_01', kind: 'front', sequence: 1 }] },
};

// The module reads the model off window at call time. The hash is set BEFORE
// the load, so the module's own bootFromHash opens the composer exactly as a
// shot does: fetch the view, fetch the cell, fetch the composer read, then
// openFromHash. No tap, no manual setView — the real path, end to end.
win.ComposerModel = ComposerModel;
win.location.hash = '#compose=7';
global.fetch = url => {
    if (/\/api\/operator-stations\/5\/view$/.test(url)) {
        return Promise.resolve({ ok: true, json: async () => VIEW });
    }
    if (/\/api\/operator-stations\/5\/composer$/.test(url)) {
        return Promise.resolve({ ok: true, json: async () => VIEW.composer });
    }
    // No cell picture on this read: the schematic caption says there is
    // nothing to draw, and the picture is a recorder here anyway.
    return Promise.resolve({ ok: false, json: async () => null });
};

const src = fs.readFileSync(path.join(__dirname, 'composer-render.js'), 'utf8')
    .replace(/^import[\s\S]*?from\s+'[^']+';\s*$/gm, '')
    .replace(/^export\s*\{[^}]*\};?\s*$/gm, '')
    .replace(/^export\s+function\s+/gm, 'function ');
vm.runInThisContext('(function(){\n' + src + '\n})();',
    { filename: 'composer-render.js' });

const drain = () => new Promise(r => setImmediate(r));

// The click path: onClick reads e.target.closest and nothing else about the
// event. The chip is retyped from the exact markup drawStrip wrote.
function closestFor(chip) {
    return sel => {
        if (sel === '[data-act]') return chip.dataset.act !== undefined ? chip : null;
        return null;
    };
}
function tapChip(chip) {
    composerRoot.listeners.click[0]({
        target: chip, stopPropagation() {}, propagationStopped: false,
    });
}

async function mainAsync() {
    for (let i = 0; i < 5; i++) await drain();
    const strip = els['os-comp-strip'].innerHTML;
    if (strip.indexOf('PART-A') < 0 || strip.indexOf('PART-B') < 0) {
        console.error('FAIL (harness): the strip does not carry both parts: ' + strip);
        nodeProcess.exit(1);
    }


    // ── the unplaced chip is a button carrying removePart ───────────────────
    test('the unplaced chip is a button carrying the removePart verb', () => {
        const want = '<button class="os-chip part free" data-act="rmpart" data-part="PART-B">' +
            esc('PART-B') + ' · unplaced</button>';
        if (strip.indexOf(want) < 0) {
            throw new Error('drawStrip did not write the unplaced chip as a removePart button\n' +
                '      wanted: ' + want + '\n      strip:  ' + strip);
        }
    });

    // ── the placed chip stays a label ────────────────────────────────────────
    test('the placed part stays a plain chip with no act', () => {
        const m = /<span class="os-chip part">[^<]*<\/span>/.exec(strip);
        if (!m || m[0].indexOf('PART-A') < 0) {
            throw new Error('the placed part is not a plain span chip; strip: ' + strip);
        }
        // Its panel is the way out; the chip itself carries no door.
        if (m[0].indexOf('data-act') >= 0) {
            throw new Error('the placed chip grew an act; its panel is the door out');
        }
    });

    // ── the tap takes the part off and redraws the picture once ──────────────
    test('a tap on the chip removes the part and redraws the picture once', () => {
        const chip = {
            dataset: { act: 'rmpart', part: 'PART-B' },
            closest: closestFor({ dataset: { act: 'rmpart', part: 'PART-B' } }),
        };
        const before = pictureDraws;
        tapChip(chip);
        const after = els['os-comp-strip'].innerHTML;
        if (after.indexOf('PART-B') >= 0) {
            throw new Error('PART-B is still on the strip after the tap: ' + after);
        }
        if (after.indexOf('PART-A') < 0) {
            throw new Error('the tap took the placed part with it: ' + after);
        }
        if (pictureDraws !== before + 1) {
            throw new Error('a strip tap must redraw the picture exactly once, drew ' +
                (pictureDraws - before));
        }
    });

    // ── the Escape rule, innermost first ─────────────────────────────────────
    // The module registered its keydown listener on the document stub at load;
    // driving it is one call on that listener list.
    function escapeKey() {
        for (const fn of (doc.listeners.keydown || [])) fn({ key: 'Escape' });
    }
    // The hash entry takes ;node=<position> and opens that panel — the one
    // open path that needs no DOM driving.
    function openPositionPanelViaHash(node) {
        win.location.hash = '#compose=7;node=' + node;
        win.ComposerUI.openFromHash();
        win.location.hash = '#compose=7';
    }

    test('Escape closes the open panel popover and leaves the composer up', () => {
        // The composer is on S4 from the boot; open a panel popover through the
        // module's own UI door, as a tap on the picture would.
        win.ComposerUI.setView(VIEW);   // re-prime in case an earlier pin closed it
        win.ComposerUI.openFromHash();
        openPositionPanelViaHash('PLN_01');
        const pop = els['os-comp-pop'];
        if (pop.hidden !== false) throw new Error('setup: the panel popover should be open here');
        escapeKey();
        if (pop.hidden !== true) {
            throw new Error('Escape did not close the open panel popover');
        }
        if (composerRoot.hidden === true) {
            throw new Error('Escape on S4 closed the whole composer; only the panel should go');
        }
    });

    test('Escape on the S2 sheet closes back to the board', () => {
        win.ComposerUI.openPicker(VIEW);
        if (composerRoot.hidden !== false) {
            throw new Error('setup: S2 should be drawn and visible here');
        }
        escapeKey();
        if (composerRoot.hidden !== true) {
            throw new Error('Escape on the S2 sheet did not close the composer');
        }
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
