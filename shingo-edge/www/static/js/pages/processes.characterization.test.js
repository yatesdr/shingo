// Characterization tests for processes.js claim editor.
//
// Pins the current (role, swap_mode) → (visible fields, saveClaim payload)
// behavior. The rewrite must continue to satisfy these assertions; any
// silent change to which fields show/require/POST is caught here.
//
// Runs under plain Node (no jsdom, no npm install). Mocks just enough of
// document/window/ShingoEdge to load processes.js via vm.runInContext.
//
// Exit 0 = all cases passed. Exit 1 = at least one assertion failed; the
// failure prints a structured diff.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

// -----------------------------------------------------------------------
// DOM stub
// -----------------------------------------------------------------------

function makeElement(id, opts = {}) {
    const tagName = (opts.tag || 'div').toUpperCase();
    const el = {
        id,
        tagName,
        type: opts.type || '',
        value: opts.value !== undefined ? opts.value : '',
        defaultValue: opts.defaultValue || '',
        checked: !!opts.checked,
        textContent: '',
        innerHTML: '',
        disabled: false,
        selectedIndex: 0,
        style: { display: opts.display !== undefined ? opts.display : '', cssText: '' },
        dataset: Object.assign({}, opts.dataset || {}),
        classList: makeClassList(),
        options: opts.options || [],
        _children: [],
        _parent: null,
        _listeners: {},
    };
    el.getAttribute = (k) => el.dataset[k] || '';
    el.setAttribute = (k, v) => { el.dataset[k] = v; };
    el.removeAttribute = () => {};
    el.appendChild = (c) => { el._children.push(c); c._parent = el; };
    el.querySelector = (sel) => querySelectorImpl(el, sel, false);
    el.querySelectorAll = (sel) => querySelectorImpl(el, sel, true);
    el.closest = (sel) => closestImpl(el, sel);
    el.contains = () => true;
    el.addEventListener = (ev, fn) => {
        (el._listeners[ev] = el._listeners[ev] || []).push(fn);
    };
    el.remove = () => {};
    return el;
}

function makeClassList() {
    const set = new Set();
    return {
        add: (c) => set.add(c),
        remove: (c) => set.delete(c),
        contains: (c) => set.has(c),
        toggle: (c, on) => { if (on) set.add(c); else set.delete(c); },
        _has: (c) => set.has(c),
    };
}

function querySelectorImpl(root, sel, all) {
    // Implement enough for processes.js:
    //   '[data-action]', '.station-node-cb', '.station-node-cb:checked',
    //   '.allowed-payload-cb', '.allowed-payload-cb:checked', '.process-tab',
    //   '.process-tab-panel', 'input,select,textarea', '[name="..."]'
    const results = [];
    function visit(node) {
        if (!node || !node._children) return;
        node._children.forEach(c => {
            if (matchesSelector(c, sel)) results.push(c);
            visit(c);
        });
    }
    visit(root);
    return all ? results : (results[0] || null);
}

function matchesSelector(node, sel) {
    if (!node) return false;
    if (sel.startsWith('[data-action')) return !!node.dataset.action;
    if (sel === '.station-node-cb') return node.classList._has('station-node-cb');
    if (sel === '.station-node-cb:checked') return node.classList._has('station-node-cb') && node.checked;
    if (sel === '.allowed-payload-cb') return node.classList._has('allowed-payload-cb');
    if (sel === '.allowed-payload-cb:checked') return node.classList._has('allowed-payload-cb') && node.checked;
    if (sel === '.process-tab') return node.classList._has('process-tab');
    if (sel === '.process-tab-panel') return node.classList._has('process-tab-panel');
    if (sel === 'input, select, textarea' || sel === 'input,select,textarea') {
        return ['INPUT', 'SELECT', 'TEXTAREA'].includes(node.tagName);
    }
    if (sel.startsWith('[name="')) {
        const m = sel.match(/^\[name="(.+)"\]$/);
        return m && node.dataset.name === m[1];
    }
    return false;
}

function closestImpl(node, sel) {
    let cur = node;
    while (cur) {
        if (matchesSelector(cur, sel)) return cur;
        cur = cur._parent;
    }
    return null;
}

// -----------------------------------------------------------------------
// Build the form DOM that processes.html renders for the claim editor
// -----------------------------------------------------------------------

function buildDOM() {
    const elements = {};
    function add(id, opts) {
        elements[id] = makeElement(id, opts);
        return elements[id];
    }

    // Page bootstrap
    add('page-data', { dataset: { activeProcessId: '0' } });

    // Process modal — only used by other functions, present for completeness
    add('new-process-name', { tag: 'input' });
    add('new-process-description', { tag: 'textarea' });
    add('new-process-counter-tag', { tag: 'input' });
    add('new-process-counter-plc', { tag: 'select' });
    add('process-modal-title');
    add('process-name', { tag: 'input' });
    add('process-description', { tag: 'textarea' });
    add('process-production-state', { tag: 'select' });
    add('counter-plc', { tag: 'select' });
    add('counter-tag', { tag: 'input' });
    add('counter-enabled', { tag: 'input', type: 'checkbox' });
    add('auto-cutover-enabled', { tag: 'input', type: 'checkbox' });

    // Style modal
    add('style-id', { tag: 'input', type: 'hidden' });
    add('style-name', { tag: 'input' });
    add('style-description', { tag: 'textarea' });
    add('style-modal-title');

    // Claims tab
    add('claims-style-selector', { tag: 'select', value: '1' });
    add('claims-list');

    // Claim modal — these are what we exercise
    add('claim-modal-title');
    add('claims-edit-id', { tag: 'input', type: 'hidden', value: '' });
    const claimsAddNode = add('claims-add-node', { tag: 'select', value: 'N1' });
    // Stub options array used by saveClaim's NGRP branch
    claimsAddNode.options = [{ value: 'N1', dataset: { type: 'NODE' }, disabled: false, style: {} }];
    claimsAddNode.options.selectedIndex = 0;
    Object.defineProperty(claimsAddNode, 'selectedIndex', {
        get() { return 0; },
        set() {}
    });

    add('claims-add-role', { tag: 'select', value: 'consume' });
    add('claims-add-swap-group');
    add('claims-add-swap', { tag: 'select', value: 'single_robot' });
    add('claims-add-payload-group');
    add('claims-add-payload', { tag: 'select', value: 'PL1' });
    add('claims-add-allowed-group', { display: 'none' });
    add('claims-add-transitional-group', { display: 'none' });
    add('claims-add-transitional', { tag: 'input', type: 'checkbox' });
    add('claims-add-home-location-group', { display: 'none' });
    add('claims-add-home-location', { tag: 'input', type: 'checkbox' });
    add('claims-allowed-picker');
    add('claims-add-capacity', { tag: 'input', value: '10' });
    add('claims-add-reorder-group');
    add('claims-add-reorder', { tag: 'input', value: '2' });
    add('claims-add-lineside-group');
    add('claims-add-lineside-soft', { tag: 'input', value: '0' });
    add('claims-staging-fieldset');
    add('claims-add-inbound', { tag: 'select', value: 'IN1' });
    add('claims-add-outbound', { tag: 'select', value: 'OUT1' });
    add('claims-staging-warning', { display: 'none' });
    add('claims-source-fieldset');
    add('claims-inbound-source-group');
    add('claims-add-inbound-source', { tag: 'select', value: 'SRC1' });
    add('claims-outbound-destination-group');
    add('claims-add-outbound-destination', { tag: 'select', value: 'DST1' });
    add('claims-changeover-fieldset');
    add('claims-add-evacuate', { tag: 'input', type: 'checkbox' });
    add('claims-ab-fieldset', { display: 'none' });
    add('claims-ab-legend');
    add('claims-ab-help');
    add('claims-ab-label');
    const pairSel = add('claims-add-paired-node', { tag: 'select', value: '' });
    pairSel.options = [{ value: '', textContent: '-- None --' }];
    add('claims-add-second-paired-group', { display: 'none' });
    add('claims-add-second-paired-node', { tag: 'select', value: '' });
    add('claims-add-reuse-bins-row', { display: 'none' });
    add('claims-add-reuse-bins', { tag: 'input', type: 'checkbox' });
    add('claims-auto-request-fieldset', { display: 'none' });
    add('claims-auto-request-manual-swap', { display: 'none' });
    add('claims-add-auto-push-row', { display: 'none' });
    add('claims-add-auto-push', { tag: 'input', type: 'checkbox' });
    add('claims-auto-request-standard', { display: 'none' });
    add('claims-add-auto-request', { tag: 'select', value: '' });
    add('claims-add-auto-confirm', { tag: 'input', type: 'checkbox' });

    // Station modal
    add('station-id', { tag: 'input', type: 'hidden' });
    add('station-name', { tag: 'input' });
    add('station-note', { tag: 'textarea' });
    add('station-enabled', { tag: 'input', type: 'checkbox' });
    add('station-modal-title');

    return elements;
}

// -----------------------------------------------------------------------
// Test harness: load processes.js in a sandboxed VM context
// -----------------------------------------------------------------------

function createContext(elements, apiRecorder) {
    function walkAll() {
        // Flat list of all live elements plus their descendants.
        const out = [];
        const seen = new Set();
        function visit(node) {
            if (!node || seen.has(node)) return;
            seen.add(node);
            out.push(node);
            (node._children || []).forEach(visit);
        }
        Object.values(elements).forEach(visit);
        return out;
    }
    const document = {
        getElementById: (id) => elements[id] || null,
        querySelectorAll: (sel) => walkAll().filter(e => matchesSelector(e, sel)),
        querySelector: (sel) => walkAll().find(e => matchesSelector(e, sel)) || null,
        addEventListener: () => {},
        body: { addEventListener: () => {} },
        readyState: 'complete',
        createElement: () => makeElement('synth'),
    };

    const ShingoEdge = {
        showModal: (id) => { if (elements[id]) elements[id].style.display = ''; },
        hideModal: (id) => { if (elements[id]) elements[id].style.display = 'none'; },
        toast: () => {},
        confirm: () => Promise.resolve(true),
        escapeHtml: (s) => String(s == null ? '' : s),
        api: {
            get: () => Promise.resolve([]),
            post: (url, body) => { apiRecorder.push({ method: 'POST', url, body }); return Promise.resolve({ id: 1 }); },
            put: () => Promise.resolve({}),
            del: () => Promise.resolve({}),
        },
        tagSelect: () => {},
        h: (s) => s,
        el: () => makeElement('synth'),
    };

    return vm.createContext({
        document,
        window: { claimedByStation: {} },
        ShingoEdge,
        // ES-module imports get stripped before vm.runInContext; the
        // stripped bindings (api, escapeHtml, showModal, hideModal,
        // toast, confirm, prompt, tagSelect, delegateActions) need to
        // resolve as bare identifiers in the loaded source.
        api:             ShingoEdge.api,
        escapeHtml:      ShingoEdge.escapeHtml,
        showModal:       ShingoEdge.showModal,
        hideModal:       ShingoEdge.hideModal,
        toast:           ShingoEdge.toast,
        confirm:         ShingoEdge.confirm,
        prompt:          () => Promise.resolve(null),
        createSSE:       () => ({ close: () => {} }),
        tagSelect:       ShingoEdge.tagSelect,
        populateForm:    () => {},
        getFormData:     () => ({}),
        delegateActions: () => {},
        console,
        parseInt,
        parseFloat,
        Math,
        JSON,
        Set,
        Array,
        Object,
        String,
        Number,
        Boolean,
        Promise,
        setTimeout,
        clearTimeout,
        encodeURIComponent,
        htmx: { trigger: () => {} },
        location: { reload: () => {}, href: '' },
    });
}

function loadProcessesJS(context) {
    let src = fs.readFileSync(
        path.join(__dirname, 'processes.js'),
        'utf8'
    );
    // processes.js is an ES module that imports from shingoedge.js.
    // The test harness loads it via vm.runInContext (classic-script
    // semantics), so strip the leading import line. The ShingoEdge
    // stub built in createContext is exposed on the global as both
    // window.ShingoEdge AND as bare identifiers so the stripped
    // imports' referents are still available.
    src = src.replace(/^import \{[^}]+\} from [^\n]+\n+/m, '');
    vm.runInContext(src, context);
}

// -----------------------------------------------------------------------
// Expected visibility matrix per (role, swap_mode)
// -----------------------------------------------------------------------
//
// "show" means the element's style.display is NOT 'none' after the
// (toggleClaimsAddPayload + validateClaimStaging) pair runs for the
// given role/swap.

// "changeover" role removed during UI consistency refactor.
// Changeovers are now driven by swap_mode + evacuate_on_changeover,
// not a separate claim role.
const ROLES = ['consume', 'produce'];
const SWAPS = ['simple', 'sequential', 'single_robot', 'two_robot', 'two_robot_press_index', 'manual_swap'];

function expectedVisibility(role, swap) {
    const isManual = swap === 'manual_swap';
    const isPressIndex = swap === 'two_robot_press_index';
    const usesStaging = swap === 'single_robot' || swap === 'two_robot';
    const showPair = !isManual;

    return {
        'claims-add-payload-group': !isManual,
        'claims-add-allowed-group': isManual,
        'claims-add-transitional-group': isManual && role === 'produce',
        'claims-add-home-location-group': isManual,
        'claims-add-reorder-group': !isManual,
        'claims-add-lineside-group': role === 'consume' && !isManual,
        'claims-staging-fieldset': !isManual && usesStaging,
        'claims-add-swap-group': true,
        'claims-source-fieldset': true,
        'claims-inbound-source-group': true,
        'claims-outbound-destination-group': true,
        'claims-changeover-fieldset': !isManual,
        'claims-ab-fieldset': showPair,
        'claims-add-second-paired-group': showPair && isPressIndex,
        'claims-add-reuse-bins-row': showPair && isPressIndex,
        'claims-auto-request-fieldset': isManual,
        'claims-auto-request-manual-swap': isManual,
        'claims-auto-request-standard': !isManual,
        'claims-add-auto-push-row': isManual && role === 'consume',
    };
}

// -----------------------------------------------------------------------
// Run characterization
// -----------------------------------------------------------------------

function setRoleAndSwap(elements, role, swap) {
    elements['claims-add-role'].value = role;
    elements['claims-add-swap'].value = swap;
}

function isVisible(el) {
    return el.style.display !== 'none';
}

let failures = 0;
let passed = 0;

function reportFailure(label, expected, actual) {
    failures++;
    console.error(`FAIL: ${label}`);
    console.error(`  expected: ${JSON.stringify(expected)}`);
    console.error(`  actual:   ${JSON.stringify(actual)}`);
}

function runVisibilityCases() {
    for (const role of ROLES) {
        for (const swap of SWAPS) {
            const elements = buildDOM();
            const apiRecorder = [];
            const ctx = createContext(elements, apiRecorder);
            loadProcessesJS(ctx);

            setRoleAndSwap(elements, role, swap);
            // toggleClaimsAddPayload mutates DOM visibility; then validateClaimStaging refines.
            ctx.toggleClaimsAddPayload();
            ctx.validateClaimStaging();

            const expected = expectedVisibility(role, swap);
            const actual = {};
            for (const id of Object.keys(expected)) {
                actual[id] = isVisible(elements[id]);
            }
            for (const id of Object.keys(expected)) {
                if (expected[id] !== actual[id]) {
                    reportFailure(
                        `visibility[role=${role}, swap=${swap}]: ${id} should be ${expected[id] ? 'visible' : 'hidden'}, was ${actual[id] ? 'visible' : 'hidden'}`,
                        expected[id],
                        actual[id]
                    );
                } else {
                    passed++;
                }
            }
        }
    }
}

// Test that saveClaim() POSTs the expected JSON body for a typical
// (consume, single_robot) claim. Captures the structural contract that
// the rewrite must keep.
async function runSaveClaimSchemaCase() {
    const elements = buildDOM();
    const apiRecorder = [];
    const ctx = createContext(elements, apiRecorder);
    loadProcessesJS(ctx);

    // Configure a consume/single_robot claim with deterministic field values.
    elements['claims-style-selector'].value = '42';
    // Trigger style selector handler to set internal _claimsStyleID
    ctx.onClaimsStyleChanged();

    elements['claims-add-node'].value = 'NODE_A';
    elements['claims-add-role'].value = 'consume';
    elements['claims-add-swap'].value = 'single_robot';
    elements['claims-add-payload'].value = 'WIDGET_X';
    elements['claims-add-capacity'].value = '30';
    elements['claims-add-reorder'].value = '5';
    elements['claims-add-lineside-soft'].value = '2';
    elements['claims-add-inbound'].value = 'STAGE_IN_1';
    elements['claims-add-outbound'].value = 'STAGE_OUT_1';
    elements['claims-add-inbound-source'].value = 'SRC_A';
    elements['claims-add-outbound-destination'].value = 'DEST_A';
    elements['claims-add-evacuate'].checked = true;
    elements['claims-add-reuse-bins'].checked = false;
    elements['claims-add-auto-push'].checked = false;
    elements['claims-add-paired-node'].value = '';
    elements['claims-add-second-paired-node'].value = '';
    elements['claims-add-auto-confirm'].checked = false;

    ctx.toggleClaimsAddPayload();
    ctx.validateClaimStaging();

    await ctx.saveClaim();

    if (apiRecorder.length !== 1) {
        reportFailure(`saveClaim/consume-single_robot: expected 1 POST, got ${apiRecorder.length}`,
            1, apiRecorder.length);
        return;
    }
    const rec = apiRecorder[0];
    if (rec.url !== '/api/style-node-claims') {
        reportFailure('saveClaim url', '/api/style-node-claims', rec.url);
        return;
    }
    const expected = {
        style_id: 42,
        core_node_name: 'NODE_A',
        role: 'consume',
        swap_mode: 'single_robot',
        payload_code: 'WIDGET_X',
        allowed_payload_codes: [],
        uop_capacity: 30,
        reorder_point: 5,
        lineside_soft_threshold: 2,
        auto_reorder: true,
        inbound_staging: 'STAGE_IN_1',
        outbound_staging: 'STAGE_OUT_1',
        inbound_source: 'SRC_A',
        outbound_destination: 'DEST_A',
        auto_request_payload: '',
        keep_staged: false,
        evacuate_on_changeover: true,
        reuse_compatible_bins: false,
        auto_push: false,
        paired_core_node: '',
        second_paired_core_node: '',
        auto_confirm: false,
    };
    for (const k of Object.keys(expected)) {
        if (JSON.stringify(rec.body[k]) !== JSON.stringify(expected[k])) {
            reportFailure(`saveClaim body[${k}]`, expected[k], rec.body[k]);
        } else {
            passed++;
        }
    }
}

// Run saveClaim for a (consume, manual_swap) claim with allowed-payload picker
async function runSaveClaimManualSwapCase() {
    const elements = buildDOM();
    const apiRecorder = [];
    const ctx = createContext(elements, apiRecorder);
    loadProcessesJS(ctx);

    elements['claims-style-selector'].value = '7';
    ctx.onClaimsStyleChanged();

    elements['claims-add-node'].value = 'NODE_M';
    elements['claims-add-role'].value = 'consume';
    elements['claims-add-swap'].value = 'manual_swap';
    elements['claims-add-capacity'].value = '15';
    elements['claims-add-reorder'].value = '0';
    elements['claims-add-lineside-soft'].value = '0';
    elements['claims-add-evacuate'].checked = false;
    elements['claims-add-auto-push'].checked = true;
    elements['claims-add-auto-request'].value = 'WIDGET_Y';

    // Mock the allowed-payload picker — getSelectedAllowedPayloads queries
    // .allowed-payload-cb:checked. Inject one checkbox into the picker.
    const picker = elements['claims-allowed-picker'];
    const cb = makeElement('synth-cb', { tag: 'input', type: 'checkbox', value: 'WIDGET_Y' });
    cb.classList.add('allowed-payload-cb');
    cb.checked = true;
    picker.appendChild(cb);

    ctx.toggleClaimsAddPayload();
    ctx.validateClaimStaging();

    await ctx.saveClaim();

    if (apiRecorder.length !== 1) {
        reportFailure(`saveClaim/manual_swap: expected 1 POST, got ${apiRecorder.length}`, 1, apiRecorder.length);
        return;
    }
    const body = apiRecorder[0].body;
    const checks = {
        role: 'consume',
        swap_mode: 'manual_swap',
        // Per saveClaim's manual_swap branch, payload_code is forced to '' when swap is manual_swap.
        payload_code: '',
        allowed_payload_codes: ['WIDGET_Y'],
        auto_request_payload: 'WIDGET_Y',
        auto_push: true,
    };
    for (const k of Object.keys(checks)) {
        if (JSON.stringify(body[k]) !== JSON.stringify(checks[k])) {
            reportFailure(`saveClaim/manual_swap body[${k}]`, checks[k], body[k]);
        } else {
            passed++;
        }
    }
}

// Run saveClaim for a (produce, sequential) claim with evacuate_on_changeover
// — verifies the changeover mechanic is now driven by swap mode + the
// evacuate flag on the active claim, not by a special "changeover" role.
async function runSaveClaimEvacuateOnChangeoverCase() {
    const elements = buildDOM();
    const apiRecorder = [];
    const ctx = createContext(elements, apiRecorder);
    loadProcessesJS(ctx);

    elements['claims-style-selector'].value = '3';
    ctx.onClaimsStyleChanged();

    elements['claims-add-node'].value = 'NODE_X';
    elements['claims-add-role'].value = 'produce';
    elements['claims-add-swap'].value = 'sequential';
    elements['claims-add-payload'].value = 'PAYLOAD_Q';
    elements['claims-add-evacuate'].checked = true;

    ctx.toggleClaimsAddPayload();
    ctx.validateClaimStaging();

    await ctx.saveClaim();

    if (apiRecorder.length !== 1) {
        reportFailure(`saveClaim/evacuate-on-changeover: expected 1 POST, got ${apiRecorder.length}`, 1, apiRecorder.length);
        return;
    }
    const body = apiRecorder[0].body;
    const checks = {
        role: 'produce',
        swap_mode: 'sequential',
        payload_code: 'PAYLOAD_Q',
        evacuate_on_changeover: true,
    };
    for (const k of Object.keys(checks)) {
        if (JSON.stringify(body[k]) !== JSON.stringify(checks[k])) {
            reportFailure(`saveClaim/evacuate body[${k}]`, checks[k], body[k]);
        } else {
            passed++;
        }
    }
}

(async () => {
    runVisibilityCases();
    await runSaveClaimSchemaCase();
    await runSaveClaimManualSwapCase();
    await runSaveClaimEvacuateOnChangeoverCase();

    if (failures > 0) {
        console.error(`\nFAILED: ${failures} assertion(s); ${passed} passed`);
        process.exit(1);
    } else {
        console.log(`PASS: ${passed} assertions across ${ROLES.length * SWAPS.length} (role,swap) cells + saveClaim schema cases`);
        process.exit(0);
    }
})();
