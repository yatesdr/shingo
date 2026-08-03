// Unit tests for the loaders page's Material-flow gating and the loader-box
// flow line. Run under plain Node via the Go wrapper
// loaders_flow_gating_test.go. Exit 0 on pass, 1 on any assertion failure.
//
// The bug these exist to prevent: the whole Material-flow section was
// display:none for dedicated_positions, on the reasoning that a dedicated
// loader's spots are their own in/out. Only OUTBOUND was ever true of that.
// Inbound is where the Edge retrieves empties FROM (loaderEmptySource →
// tryCreateL1); blank, the threshold→empty-to-home chain silently no-ops at
// debug level. Springfield ran a dedicated loader with a blank inbound_source
// and the replenishment chain was mute with nothing on any screen to say so.
//
// The headline assertions are that (1) gating a dedicated loader never blanks
// a field value — the save path reads .value off these inputs unconditionally,
// so a gate that cleared them would silently drop config on the next save —
// and (2) a dedicated loader's inbound is visible on the box.
//
// THE GATE CHANGED SHAPE and these moved with it. The form used to render
// every field and grey out the ones that did not apply, with a paragraph
// beside each explaining why it was greyed out. It now renders only what
// applies: a field that does not apply is absent, and an absent field needs
// no paragraph. So the assertions are about VISIBILITY rather than disabled —
// with the value-preserving one intact, because it is the load-bearing one
// and hiding can drop config exactly as disabling could.
//
// Visibility is the `is-hidden` class, not element.style.display, and it is
// derived from a state object rather than toggled by the change handler —
// the form-state convention in docs/ui-style-guide.md. formShape() is a pure
// function of state, so the rules are asserted directly as well as through
// the DOM.
//
// One deliberate exception, asserted as such: ticking "fed by hand" CLEARS
// the source as well as hiding it. That one is the operator saying there is
// no source, so screen and saved state agreeing is the point.

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

// --- harness -------------------------------------------------------------
// Enough DOM for the modal's flow fields plus the module's top-level lookups.
function makeEl(id, tag) {
    const classes = new Set();
    return {
        id: id,
        tagName: (tag || 'input').toUpperCase(),
        value: '',
        checked: false,
        disabled: false,
        textContent: '',
        innerHTML: '',
        style: {},
        dataset: {},
        classList: {
            add(c) { classes.add(c); },
            remove(c) { classes.delete(c); },
            contains(c) { return classes.has(c); },
            toggle(c, on) { if (on) classes.add(c); else classes.delete(c); },
        },
        addEventListener() {},
    };
}

// shown() is the assertion the visibility checks are written against: a field
// is on screen when it does NOT carry the is-hidden class.
function shown(el) { return !el.classList.contains('is-hidden'); }

function load() {
    const ids = [
        'loader-layout', 'loader-inbound', 'loader-outbound', 'loader-buffer',
        'loader-flow-section', 'loader-flow-scope', 'loader-outbound-note',
        'loader-buffer-note-dedicated', 'loader-role', 'loader-replenishment',
        'loader-replenishment-hint', 'loader-name', 'loader-edit-id',
        'loader-modal', 'loader-result', 'loader-modal-title', 'loader-submit-btn',
        'loader-kind', 'loader-fed-by-hand', 'loader-supply-row', 'loader-staging-row',
        'loader-mix-row', 'loader-mix-editor', 'loader-mix-add-type',
        'loader-mix-add-want', 'loader-edit-id', 'loader-windows-row',
        'loader-windows-editor',
    ];
    const els = {};
    ids.forEach(function (id) { els[id] = makeEl(id); });

    const ctxObj = {
        console: console,
        // readyState is deliberately NOT 'complete': the module's tail would
        // call init() → refresh() → apiGet and hit the network.
        document: {
            readyState: 'loading',
            getElementById(id) { return els[id] || null; },
            querySelectorAll() { return []; },
            addEventListener() {},
        },
        window: { innerHeight: 900, scrollBy() {}, confirm() { return true; } },
        setInterval() { return 0; },
        clearInterval() {},
        setTimeout() { return 0; },
        // Injected in place of the stripped ES import.
        apiGet() { return Promise.resolve({}); },
        apiPost() { return Promise.resolve({}); },
        delegateActions() {},
        escapeHtml(s) { return String(s == null ? '' : s); },
        toast() {},
    };
    vm.createContext(ctxObj);
    const src = fs.readFileSync(path.join(__dirname, 'loaders.js'), 'utf8')
        .replace(/^import[^;]+;\s*/m, '');   // drop the ES import; deps injected above
    vm.runInContext(src, ctxObj);
    return { ctx: ctxObj, els: els };
}

// --- form gating ---------------------------------------------------------

console.log('formShape — the rules, as a pure function of state');

(function shapeRules() {
    const h = load();
    const shape = h.ctx.formShape;
    const st = function (over) {
        return Object.assign({
            id: 1, name: 'L', role: 'produce', kind: 'multi_window',
            replenishment: 'operator', fedByHand: false,
            inbound: '', outbound: '', buffer: '',
        }, over || {});
    };

    // THE STAGING GROUP BELONGS TO DEDICATED HOMES. It holds empties that
    // rotate into a SPOT when that spot runs low — position language. A window
    // loader is fed from its inbound source and sends to its outbound, and has
    // no third place; offering it one invited a plant to configure a source
    // that nothing on the window path reads.
    check('staging: offered to a dedicated loader',
        shape(st({ kind: 'dedicated' })).staging === true);
    check('staging: NOT offered to a multi-window loader',
        shape(st({ kind: 'multi_window' })).staging === false);
    check('staging: NOT offered to a single-window loader',
        shape(st({ kind: 'single_window' })).staging === false);
    check('staging: not offered when nothing pulls at all',
        shape(st({ kind: 'dedicated', fedByHand: true })).staging === false);

    // The mix and the per-window capability are properties of a window SET.
    check('mix: offered to a window loader', shape(st({})).mix === true);
    check('mix: NOT offered to a dedicated loader - a spot is already one part',
        shape(st({ kind: 'dedicated' })).mix === false);
    check('windows: per-window capability follows the same rule',
        shape(st({})).windows === true &&
        shape(st({ kind: 'dedicated' })).windows === false);
    // Both are edited against a SAVED loader, so an unsaved one shows neither
    // rather than showing a control that cannot be used yet.
    check('mix + windows: wait for a saved loader',
        shape(st({ id: 0 })).mix === false &&
        shape(st({ id: 0 })).windows === false);

    check('supply: asked of a loader, not of an unloader',
        shape(st({})).supply === true &&
        shape(st({ role: 'consume' })).supply === false);
    check('outbound: asked of a window loader, not of dedicated spots',
        shape(st({})).outbound === true &&
        shape(st({ kind: 'dedicated' })).outbound === false);
    check('inbound: not asked when fed by hand',
        shape(st({ fedByHand: true })).inbound === false);
})();

console.log('applyLoaderForm — the same rules, through the DOM');

(function dedicatedGating() {
    const h = load();
    h.els['loader-kind'].value = 'dedicated';
    h.els['loader-role'].value = 'produce';
    h.els['loader-inbound'].value = 'AMR Supermarket';
    h.els['loader-outbound'].value = 'LEGACY-OUT';
    h.els['loader-buffer'].value = 'LEGACY-BUF';

    h.ctx.applyLoaderForm();

    check('dedicated: inbound still asked for', shown(h.els['loader-inbound']));
    check('dedicated: outbound not asked for - the spots are their own outbound',
        !shown(h.els['loader-outbound']));
    check('dedicated: staging group offered - the buffer is a dedicated-home idea',
        shown(h.els['loader-staging-row']));
    check('dedicated: no carrier mix - a spot is already one part',
        !shown(h.els['loader-mix-row']));

    // THE LOAD-BEARING ONE. submitLoader writes all of these on every save, so
    // a gate that blanked a hidden field would drop config silently.
    check('dedicated: inbound value preserved',
        h.els['loader-inbound'].value === 'AMR Supermarket',
        'got ' + JSON.stringify(h.els['loader-inbound'].value));
    check('dedicated: hidden outbound value preserved',
        h.els['loader-outbound'].value === 'LEGACY-OUT',
        'got ' + JSON.stringify(h.els['loader-outbound'].value));
    check('dedicated: staging value preserved',
        h.els['loader-buffer'].value === 'LEGACY-BUF',
        'got ' + JSON.stringify(h.els['loader-buffer'].value));
})();

(function multiWindowGating() {
    const h = load();
    h.els['loader-kind'].value = 'multi_window';
    h.els['loader-role'].value = 'produce';
    h.els['loader-inbound'].value = 'EMPTY-BANK';
    h.els['loader-outbound'].value = 'FG-MARKET';
    h.els['loader-buffer'].value = 'LEGACY-BUF';

    h.ctx.applyLoaderForm();

    check('multi window: inbound asked for', shown(h.els['loader-inbound']));
    check('multi window: outbound asked for', shown(h.els['loader-outbound']));
    check('multi window: NO staging group - inbound and outbound are the whole flow',
        !shown(h.els['loader-staging-row']));
    check('multi window: supply asked for - a loader has modes',
        shown(h.els['loader-supply-row']));
    check('multi window: values preserved',
        h.els['loader-inbound'].value === 'EMPTY-BANK' &&
        h.els['loader-outbound'].value === 'FG-MARKET');
    // A window loader is not offered the staging group, and a plant that had
    // one set keeps it rather than having it dropped on the next save.
    check('multi window: hidden staging value preserved',
        h.els['loader-buffer'].value === 'LEGACY-BUF',
        'got ' + JSON.stringify(h.els['loader-buffer'].value));
})();

(function unloaderHasNoSupplyQuestion() {
    // An unloader drains when the operator clears a window. There is exactly
    // one mode, so asking which one is a question with one answer.
    const h = load();
    h.els['loader-kind'].value = 'multi_window';
    h.els['loader-role'].value = 'consume';

    h.ctx.applyLoaderForm();

    check('unloader: supply question not asked', !shown(h.els['loader-supply-row']));
})();

(function fedByHandClearsTheSource() {
    // The one deliberate blank. Ticking it IS the operator saying there is no
    // source, so hiding the field without clearing it would save a source the
    // screen says does not exist.
    const h = load();
    h.els['loader-kind'].value = 'multi_window';
    h.els['loader-role'].value = 'produce';
    h.els['loader-inbound'].value = 'AMR Supermarket';
    h.els['loader-fed-by-hand'].checked = true;

    h.ctx.applyLoaderForm();

    check('fed by hand: source not asked for', !shown(h.els['loader-inbound']));
    check('fed by hand: source CLEARED, deliberately',
        h.els['loader-inbound'].value === '',
        'got ' + JSON.stringify(h.els['loader-inbound'].value));
})();

(function switchingKindRegates() {
    // The gate is wired to each control's change event, so switching with the
    // modal already open must re-decide rather than leave the prior state.
    const h = load();
    h.els['loader-kind'].value = 'multi_window';
    h.ctx.applyLoaderForm();
    check('switch: outbound asked before', shown(h.els['loader-outbound']));

    h.els['loader-kind'].value = 'dedicated';
    h.els['loader-outbound'].value = 'STILL-HERE';
    h.ctx.applyLoaderForm();
    check('switch: outbound dropped after', !shown(h.els['loader-outbound']));
    check('switch: value survived the re-gate', h.els['loader-outbound'].value === 'STILL-HERE');
})();

(function unsavedLoaderIsOfferedNeitherSection() {
    // A create has no id yet, so the two sections that edit a saved loader are
    // absent rather than present-but-inert. They used to render a "save the
    // loader first" placeholder, which is a control apologising for itself.
    const h = load();
    h.els['loader-kind'].value = 'multi_window';
    h.els['loader-edit-id'].value = '';

    h.ctx.applyLoaderForm();

    check('create: carrier mix absent until saved', !shown(h.els['loader-mix-row']));
    check('create: per-window capability absent until saved', !shown(h.els['loader-windows-row']));
})();

// --- box flow line -------------------------------------------------------

console.log('boxHtml flow line');

function box(loader) {
    const h = load();
    return h.ctx.boxHtml({ loader: loader, homes: [], payloads: [] });
}

(function dedicatedShowsInbound() {
    const html = box({
        id: 7, name: 'SMN Loader', role: 'produce',
        layout: 'dedicated_positions', replenishment: 'threshold',
        inbound_source: 'AMR Supermarket', outbound_dest: '', buffer_dest: '',
    });
    check('dedicated: renders inbound → (spots)',
        html.indexOf('AMR Supermarket → (spots)') >= 0,
        'html did not contain the flow line');
})();

(function dedicatedBlankInboundIsConspicuous() {
    const html = box({
        id: 8, name: 'Blank Loader', role: 'produce',
        layout: 'dedicated_positions', replenishment: 'threshold',
        inbound_source: '', outbound_dest: '', buffer_dest: '',
    });
    // A blank inbound on a dedicated loader is the Springfield silent-failure
    // config. It must render, and it must look wrong.
    check('dedicated: blank inbound renders as — → (spots)',
        html.indexOf('— → (spots)') >= 0,
        'blank inbound was not surfaced');
})();

(function sharedWindowUnchanged() {
    const html = box({
        id: 9, name: 'Shared Loader', role: 'produce',
        layout: 'shared_window', replenishment: 'operator',
        inbound_source: 'EMPTY-BANK', outbound_dest: 'FG-MARKET', buffer_dest: 'BUF-1',
    });
    check('shared: inbound → outbound unchanged',
        html.indexOf('EMPTY-BANK → FG-MARKET') >= 0);
    check('shared: staging group still annotated', html.indexOf('staging BUF-1') >= 0);
})();

if (failures > 0) {
    console.log('\nFAILED: ' + failures + ' assertion(s)');
    process.exit(1);
}
console.log('\nPASS: loaders flow gating + box flow line');
