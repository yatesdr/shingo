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
// so a disable that cleared them would silently drop config on the next save —
// and (2) a dedicated loader's inbound is visible on the box.

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
    return {
        id: id,
        tagName: (tag || 'input').toUpperCase(),
        value: '',
        disabled: false,
        textContent: '',
        style: {},
        dataset: {},
        addEventListener() {},
    };
}

function load() {
    const ids = [
        'loader-layout', 'loader-inbound', 'loader-outbound', 'loader-buffer',
        'loader-flow-section', 'loader-flow-scope', 'loader-outbound-note',
        'loader-buffer-note-dedicated', 'loader-role', 'loader-replenishment',
        'loader-replenishment-hint', 'loader-name', 'loader-edit-id',
        'loader-modal', 'loader-result', 'loader-modal-title', 'loader-submit-btn',
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

// --- flow-field gating ---------------------------------------------------

console.log('setLayoutFlowVisibility');

(function dedicatedGating() {
    const h = load();
    h.els['loader-layout'].value = 'dedicated_positions';
    h.els['loader-inbound'].value = 'AMR Supermarket';
    h.els['loader-outbound'].value = 'LEGACY-OUT';
    h.els['loader-buffer'].value = 'LEGACY-BUF';

    h.ctx.setLayoutFlowVisibility();

    check('dedicated: section stays visible',
        h.els['loader-flow-section'].style.display === '',
        'display=' + JSON.stringify(h.els['loader-flow-section'].style.display));
    check('dedicated: inbound stays editable', h.els['loader-inbound'].disabled === false);
    check('dedicated: outbound disabled', h.els['loader-outbound'].disabled === true);
    check('dedicated: buffer disabled', h.els['loader-buffer'].disabled === true);

    // The load-bearing one: disabling must never blank a value, because
    // submitLoader reads .value off all three unconditionally on every save.
    check('dedicated: inbound value preserved',
        h.els['loader-inbound'].value === 'AMR Supermarket',
        'got ' + JSON.stringify(h.els['loader-inbound'].value));
    check('dedicated: disabled outbound value preserved',
        h.els['loader-outbound'].value === 'LEGACY-OUT',
        'got ' + JSON.stringify(h.els['loader-outbound'].value));
    check('dedicated: disabled buffer value preserved',
        h.els['loader-buffer'].value === 'LEGACY-BUF',
        'got ' + JSON.stringify(h.els['loader-buffer'].value));

    check('dedicated: scope label names the restriction',
        /inbound only/i.test(h.els['loader-flow-scope'].textContent),
        'got ' + JSON.stringify(h.els['loader-flow-scope'].textContent));
    check('dedicated: outbound note shown', h.els['loader-outbound-note'].style.display === '');
    check('dedicated: buffer note shown', h.els['loader-buffer-note-dedicated'].style.display === '');
})();

(function sharedWindowGating() {
    const h = load();
    h.els['loader-layout'].value = 'shared_window';
    h.els['loader-inbound'].value = 'EMPTY-BANK';
    h.els['loader-outbound'].value = 'FG-MARKET';

    h.ctx.setLayoutFlowVisibility();

    check('shared: section visible', h.els['loader-flow-section'].style.display === '');
    check('shared: inbound editable', h.els['loader-inbound'].disabled === false);
    check('shared: outbound editable', h.els['loader-outbound'].disabled === false);
    check('shared: buffer editable', h.els['loader-buffer'].disabled === false);
    check('shared: dedicated-only notes hidden',
        h.els['loader-outbound-note'].style.display === 'none' &&
        h.els['loader-buffer-note-dedicated'].style.display === 'none');
    check('shared: values preserved',
        h.els['loader-inbound'].value === 'EMPTY-BANK' &&
        h.els['loader-outbound'].value === 'FG-MARKET');
})();

(function switchingLayoutRegates() {
    // The gate is wired to the select's change event, so switching layout with
    // the modal already open must re-gate rather than leave the prior state.
    const h = load();
    h.els['loader-layout'].value = 'shared_window';
    h.ctx.setLayoutFlowVisibility();
    check('switch: outbound editable before', h.els['loader-outbound'].disabled === false);

    h.els['loader-layout'].value = 'dedicated_positions';
    h.els['loader-outbound'].value = 'STILL-HERE';
    h.ctx.setLayoutFlowVisibility();
    check('switch: outbound disabled after', h.els['loader-outbound'].disabled === true);
    check('switch: value survived the re-gate', h.els['loader-outbound'].value === 'STILL-HERE');
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
    check('shared: buffer still annotated', html.indexOf('buf BUF-1') >= 0);
})();

if (failures > 0) {
    console.log('\nFAILED: ' + failures + ' assertion(s)');
    process.exit(1);
}
console.log('\nPASS: loaders flow gating + box flow line');
