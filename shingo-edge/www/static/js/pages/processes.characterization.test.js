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
    // A SELECT's option list is what the picker code produces, so the stub has
    // to parse it — otherwise a test can only assert that innerHTML was
    // assigned, which is true no matter what is in it.
    if (tagName === 'SELECT') {
        let _html = '';
        Object.defineProperty(el, 'innerHTML', {
            get() { return _html; },
            set(v) {
                _html = String(v);
                const opts = [];
                let group = '';
                const re = /<optgroup label="([^"]*)">|<\/optgroup>|<option value="([^"]*)"([^>]*)>([^<]*)<\/option>/g;
                let m;
                while ((m = re.exec(_html)) !== null) {
                    if (m[1] !== undefined) { group = m[1]; continue; }
                    if (m[0] === '</optgroup>') { group = ''; continue; }
                    opts.push({
                        value: m[2],
                        textContent: m[4],
                        selected: /\bselected\b/.test(m[3] || ''),
                        _group: group,
                        dataset: {},
                        disabled: false,
                        style: {},
                    });
                }
                el.options = opts;
            },
        });
    }
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
    add('claims-allowed-picker');
    add('claims-add-capacity', { tag: 'input', value: '10' });
    add('claims-add-reorder-group');
    add('claims-add-reorder', { tag: 'input', value: '2' });
    add('claims-add-sequence-group');
    add('claims-add-sequence', { tag: 'input', value: '0' });
    add('claims-err-sequence', { display: 'none' });
    add('claims-add-auto-reorder-row');
    add('claims-add-auto-reorder', { tag: 'input', type: 'checkbox' });
    add('claims-add-keep-staged-row');
    add('claims-add-keep-staged', { tag: 'input', type: 'checkbox' });
    add('claims-add-lineside-group');
    add('claims-lineside-help');
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
    add('claims-third-position-help', { display: 'none' });
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

    // Claim-modal validation slots (round 2 unit 1). The modal element itself
    // is present because ensureClaimErrorDelegation marks it.
    add('claim-modal');
    [
        'claims-err-form',
        'claims-err-core-node-name',
        'claims-notice-core-node-name',
        'claims-err-swap-mode',
        'claims-err-payload-code',
        'claims-err-inbound-staging',
        'claims-err-outbound-staging',
        'claims-err-inbound-source',
        'claims-err-outbound-destination',
        'claims-err-paired-core-node',
        'claims-err-second-paired-core-node',
        'claims-mode-drop-note',
    ].forEach(function(id) { add(id, { display: 'none' }); });
    add('claims-show-all-nodes', { tag: 'input', type: 'checkbox' });
    // Collapse-card hints (density pass).
    add('claims-changeover-hint');
    add('claims-auto-request-hint');

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
            // postDetailed never throws; it reports. Default is a clean save
            // with no findings — a case that wants a server refusal overrides
            // it on its own context.
            postDetailed: (url, body) => {
                apiRecorder.push({ method: 'POST', url, body });
                return Promise.resolve({ ok: true, status: 200, data: { id: 1 }, error: '', fieldErrors: [], warnings: [] });
            },
            put: () => Promise.resolve({}),
            del: () => Promise.resolve({}),
        },
        tagSelect: () => {},
        h: (s) => s,
        el: () => makeElement('synth'),
    };

    return vm.createContext({
        document,
        window: {
            claimedByStation: {},
            // A small plant: two press positions on this process, a
            // supermarket GROUP, one of its lanes, a storage node, a staging
            // spot, and a dedicated loader home that is ALSO a line position
            // (the source_finder tier-2 case the filter must not hide).
            coreNodeCatalog: {
                PRESS_A:  { name: 'PRESS_A',  node_type: '' },
                PRESS_B:  { name: 'PRESS_B',  node_type: '' },
                PRESS_C:  { name: 'PRESS_C',  node_type: '' },
                SMG_01:   { name: 'SMG_01',   node_type: 'NGRP' },
                SMG_01_L1:{ name: 'SMG_01_L1',node_type: 'LANE' },
                STOR_01:  { name: 'STOR_01',  node_type: 'STOR' },
                STAGE_01: { name: 'STAGE_01', node_type: '' },
                SMN_014:  { name: 'SMN_014',  node_type: '' },
            },
            processNodeNames: ['PRESS_A', 'PRESS_B', 'PRESS_C', 'SMN_014'],
        },
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
    // A/B fieldset shows only for the paired-node modes (sequential + press
    // index); single_robot / two_robot use staging, not paired nodes.
    const showPair = swap === 'sequential' || isPressIndex;

    return {
        'claims-add-payload-group': !isManual,
        'claims-add-allowed-group': false,
        'claims-add-reorder-group': !isManual,
        'claims-add-sequence-group': true,
        'claims-add-auto-reorder-row': !isManual,
        'claims-add-lineside-group': role === 'consume' && !isManual,
        'claims-lineside-help': role === 'consume' && !isManual,
        'claims-staging-fieldset': !isManual && usesStaging,
        'claims-add-keep-staged-row': !isManual && usesStaging,
        'claims-add-swap-group': true,
        'claims-source-fieldset': !isManual,
        'claims-inbound-source-group': !isManual,
        'claims-outbound-destination-group': !isManual,
        'claims-changeover-fieldset': !isManual,
        'claims-ab-fieldset': showPair,
        'claims-add-second-paired-group': showPair && isPressIndex,
        'claims-third-position-help': showPair && isPressIndex,
        'claims-add-reuse-bins-row': showPair && isPressIndex,
        // Was pinned `false` unconditionally, matching a bug rather than an
        // intent: 2635ad10 set out to hide this for manual_swap only and said
        // so in its message, but wrote the parent without the condition. The
        // consequence was that auto_confirm had no reachable control at all.
        'claims-auto-request-fieldset': !isManual,
        'claims-auto-request-manual-swap': false,
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
        inbound_staging: 'STAGE_IN_1',
        outbound_staging: 'STAGE_OUT_1',
        inbound_source: 'SRC_A',
        outbound_destination: 'DEST_A',
        auto_request_payload: '',
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
    // ABSENT MEANS "THIS FORM HAS NO OPINION", and which fields that covers
    // moved in round 2 unit 4.
    //
    // reorder_point_source: nothing in this editor sets provenance, so it stays
    // absent and updateClaim leaves the stored value alone. A literal here is
    // the round-1 bug coming back — every save reset provenance to "legacy".
    //
    // sequence: absent only because THIS fixture leaves the input at 0, which
    // means "no opinion" and lets the store assign the next free board slot. A
    // non-zero value IS sent — see runSurfacedFieldsSaveCase.
    for (const k of ['sequence', 'reorder_point_source']) {
        if (k in rec.body) {
            reportFailure(`saveClaim body[${k}] must be ABSENT here`, undefined, rec.body[k]);
        } else {
            passed++;
        }
    }
    // auto_reorder and keep_staged now have controls, so the form owns them and
    // sends them. They were absent in round 1 because there was nothing to
    // send; sending them is not a regression of that fix, it is the reason the
    // fix used pointers instead of preservation hacks.
    for (const k of ['auto_reorder', 'keep_staged']) {
        if (!(k in rec.body)) {
            reportFailure(`saveClaim body[${k}] must be PRESENT (the form owns a control for it)`, 'present', 'absent');
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

// Run saveClaim for a (produce, manual_swap) loader claim with NO allowed
// payloads selected — the real post-cutover flow, where the per-style picker is
// hidden because Core owns the loader's payload set (the loader board). Pins the
// fix for the "Select at least one allowed payload" dead end: validation must NOT
// block, and the claim saves with a blank payload_code + empty allowed list.
async function runSaveClaimManualSwapNoPickerCase() {
    const elements = buildDOM();
    const apiRecorder = [];
    const ctx = createContext(elements, apiRecorder);
    loadProcessesJS(ctx);

    elements['claims-style-selector'].value = '7';
    ctx.onClaimsStyleChanged();

    elements['claims-add-node'].value = 'NODE_L';
    elements['claims-add-role'].value = 'produce';
    elements['claims-add-swap'].value = 'manual_swap';
    elements['claims-add-capacity'].value = '0';
    elements['claims-add-reorder'].value = '0';
    elements['claims-add-lineside-soft'].value = '0';

    // No checkbox injected: the picker is retired, so getSelectedAllowedPayloads()
    // returns []. Pre-fix this tripped "Select at least one allowed payload" and
    // saveClaim returned without POSTing.
    ctx.toggleClaimsAddPayload();
    ctx.validateClaimStaging();

    await ctx.saveClaim();

    if (apiRecorder.length !== 1) {
        reportFailure(`saveClaim/manual_swap-no-picker: expected 1 POST (validation must not block), got ${apiRecorder.length}`, 1, apiRecorder.length);
        return;
    }
    const body = apiRecorder[0].body;
    const checks = {
        role: 'produce',
        swap_mode: 'manual_swap',
        payload_code: '',
        allowed_payload_codes: [],
    };
    for (const k of Object.keys(checks)) {
        if (JSON.stringify(body[k]) !== JSON.stringify(checks[k])) {
            reportFailure(`saveClaim/manual_swap-no-picker body[${k}]`, checks[k], body[k]);
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

// -----------------------------------------------------------------------
// Round 2 unit 1 — validation errors render ON the field
// -----------------------------------------------------------------------
//
// saveClaim used to surface validation.errors[0] as a toast and discard the
// rest: an operator with three problems heard about one of them, and the
// message never said which input it meant. These assert the DOM after a
// refused save, because "does the operator see where" is a question about what
// is on the page.

function shown(el) {
    return !!el && el.hidden === false && el.style.display !== 'none';
}

async function runFieldErrorRenderCase() {
    const elements = buildDOM();
    const apiRecorder = [];
    const ctx = createContext(elements, apiRecorder);
    loadProcessesJS(ctx);

    elements['claims-style-selector'].value = '5';
    ctx.onClaimsStyleChanged();

    // press-index with NO back press node and NO outbound destination: two
    // errors at once, which is the case the single toast could not report.
    elements['claims-add-node'].value = 'NODE_A';
    elements['claims-add-role'].value = 'produce';
    elements['claims-add-swap'].value = 'two_robot_press_index';
    elements['claims-add-payload'].value = 'PL1';
    elements['claims-add-paired-node'].value = '';
    elements['claims-add-outbound-destination'].value = '';

    await ctx.saveClaim();

    if (apiRecorder.length !== 0) {
        reportFailure('fieldErrors: a refused save must not POST', 0, apiRecorder.length);
    } else { passed++; }

    const pairedSlot = elements['claims-err-paired-core-node'];
    const destSlot = elements['claims-err-outbound-destination'];

    if (!shown(pairedSlot)) {
        reportFailure('fieldErrors: paired_core_node slot visible', 'visible',
            JSON.stringify({ hidden: pairedSlot.hidden, display: pairedSlot.style.display }));
    } else { passed++; }
    if (!/Back Press Node/.test(pairedSlot.textContent)) {
        reportFailure('fieldErrors: paired slot carries its message', 'mentions Back Press Node', pairedSlot.textContent);
    } else { passed++; }

    // The SECOND error is the one the old toast threw away.
    if (!shown(destSlot)) {
        reportFailure('fieldErrors: outbound_destination slot visible (the error the toast discarded)',
            'visible', JSON.stringify({ hidden: destSlot.hidden, display: destSlot.style.display }));
    } else { passed++; }
    if (!/Outbound Destination/.test(destSlot.textContent)) {
        reportFailure('fieldErrors: destination slot carries its message', 'mentions Outbound Destination', destSlot.textContent);
    } else { passed++; }

    // The inputs are marked, so the eye lands on them.
    if (!elements['claims-add-paired-node'].classList._has('form-input--error')) {
        reportFailure('fieldErrors: paired input marked', 'form-input--error', 'absent');
    } else { passed++; }

    // Untouched fields stay clean.
    if (shown(elements['claims-err-payload-code'])) {
        reportFailure('fieldErrors: a valid field shows nothing', 'hidden', 'visible');
    } else { passed++; }

    // Editing the field clears its message — a red that outlives its cause
    // teaches the operator to ignore red.
    ctx.clearClaimFieldError('paired_core_node');
    if (shown(pairedSlot)) {
        reportFailure('fieldErrors: editing clears the field message', 'hidden', 'visible');
    } else { passed++; }
    if (elements['claims-add-paired-node'].classList._has('form-input--error')) {
        reportFailure('fieldErrors: editing clears the input border', 'unmarked', 'still marked');
    } else { passed++; }
    // ...and only that field's.
    if (!shown(destSlot)) {
        reportFailure('fieldErrors: clearing one field leaves the others', 'still visible', 'cleared');
    } else { passed++; }
}

// Server findings render through the SAME path, so the operator sees one thing
// whether JS or Go caught it — including the wire (snake_case) field names,
// which is what domain.ValidateNodeClaim actually emits.
function runServerFieldErrorCase() {
    const elements = buildDOM();
    const ctx = createContext(elements, []);
    loadProcessesJS(ctx);

    ctx.renderClaimFieldErrors([
        { field: 'inbound_staging', message: 'Single-robot swap requires inbound staging', severity: 'error' },
        { field: 'core_node_name', message: 'not a node on this style process', severity: 'warning' },
    ]);

    if (!shown(elements['claims-err-inbound-staging'])) {
        reportFailure('serverErrors: wire-named field lands on its slot', 'visible', 'hidden');
    } else { passed++; }

    // A WARNING is not a refusal: separate slot, and the input is NOT marked.
    // Rendering advice in the refusal colour is how a refusal colour stops
    // meaning anything.
    if (!shown(elements['claims-notice-core-node-name'])) {
        reportFailure('serverErrors: warning renders in the notice slot', 'visible', 'hidden');
    } else { passed++; }
    if (shown(elements['claims-err-core-node-name'])) {
        reportFailure('serverErrors: warning must NOT use the error slot', 'hidden', 'visible');
    } else { passed++; }
    if (elements['claims-add-node'].classList._has('form-input--error')) {
        reportFailure('serverErrors: warning must not mark the input as an error', 'unmarked', 'marked');
    } else { passed++; }
}

// A finding for a field with no slot must still reach the operator. Dropping
// it silently is the toast problem again: a refusal with no stated reason.
function runOrphanFieldErrorCase() {
    const elements = buildDOM();
    const ctx = createContext(elements, []);
    loadProcessesJS(ctx);

    ctx.renderClaimFieldErrors([
        { field: 'a_field_the_ui_has_never_heard_of', message: 'something is wrong', severity: 'error' },
    ]);
    const form = elements['claims-err-form'];
    if (!shown(form) || !/something is wrong/.test(form.textContent)) {
        reportFailure('orphanErrors: unmapped finding falls back to the form slot', 'visible with text',
            JSON.stringify({ hidden: form.hidden, text: form.textContent }));
    } else { passed++; }
}

// -----------------------------------------------------------------------
// Round 2 unit 2 — hiding a field must not eat its value
// -----------------------------------------------------------------------
//
// renderClaimForm used to blank paired_core_node, second_paired_core_node and
// reuse_compatible_bins whenever the A/B fieldset went out of view. Toggling
// the swap mode to look at another mode's fields and toggling back silently
// destroyed a press-index pairing the operator never touched.
//
// Rendering is not editing. The state is the model; visibility is a view.

async function runHiddenFieldSurvivalCase() {
    const elements = buildDOM();
    const apiRecorder = [];
    const ctx = createContext(elements, apiRecorder);
    loadProcessesJS(ctx);

    elements['claims-style-selector'].value = '9';
    ctx.onClaimsStyleChanged();

    // An existing 3-position press-index claim, fully configured.
    ctx.editClaim({
        id: 42,
        core_node_name: 'PRESS_A',
        role: 'produce',
        swap_mode: 'two_robot_press_index',
        payload_code: 'PL1',
        uop_capacity: 720,
        reorder_point: 10,
        inbound_source: 'SRC1',
        outbound_destination: 'DST1',
        paired_core_node: 'PRESS_B',
        second_paired_core_node: 'PRESS_C',
        reuse_compatible_bins: true,
    });

    // The operator looks at what 2-Robot Swap offers, then goes back.
    elements['claims-add-swap'].value = 'two_robot';
    ctx.renderClaimForm();
    elements['claims-add-swap'].value = 'two_robot_press_index';
    ctx.renderClaimForm();

    await ctx.saveClaim();

    if (apiRecorder.length !== 1) {
        // 0 POSTs means the save was REFUSED: the round trip ate
        // paired_core_node, and press-index validation then rejected the
        // claim the operator never edited. That is the bug, not a test setup
        // problem — stop renderClaimForm blanking hidden fields.
        reportFailure('hiddenFields: the round trip must not eat values (0 POSTs = save refused because a value was destroyed)', 1, apiRecorder.length);
        return;
    }
    const body = apiRecorder[0].body;
    const survives = {
        paired_core_node: 'PRESS_B',
        second_paired_core_node: 'PRESS_C',
        reuse_compatible_bins: true,
        outbound_destination: 'DST1',
        inbound_source: 'SRC1',
    };
    for (const k of Object.keys(survives)) {
        if (JSON.stringify(body[k]) !== JSON.stringify(survives[k])) {
            reportFailure(`hiddenFields: ${k} survives a swap-mode round trip`, survives[k], body[k]);
        } else { passed++; }
    }
}

// The other half: a mode that genuinely cannot use a value still drops it —
// this is not "never clear anything", it is "clear once, at save, for the mode
// the operator actually chose".
async function runForbiddenFieldDropCase() {
    const elements = buildDOM();
    const apiRecorder = [];
    const ctx = createContext(elements, apiRecorder);
    loadProcessesJS(ctx);

    elements['claims-style-selector'].value = '9';
    ctx.onClaimsStyleChanged();

    ctx.editClaim({
        id: 43,
        core_node_name: 'PRESS_A',
        role: 'produce',
        swap_mode: 'two_robot_press_index',
        payload_code: 'PL1',
        inbound_staging: 'IN1',
        paired_core_node: 'PRESS_B',
        second_paired_core_node: 'PRESS_C',
        reuse_compatible_bins: true,
        outbound_destination: 'DST1',
    });

    // Actually switch to two_robot and save there.
    elements['claims-add-swap'].value = 'two_robot';
    ctx.renderClaimForm();

    // The operator is told BEFORE saving what the mode will discard.
    const note = elements['claims-mode-drop-note'];
    if (!(note.hidden === false && note.style.display !== 'none')) {
        reportFailure('forbiddenFields: drop note is shown before saving', 'visible', 'hidden');
    } else { passed++; }
    for (const want of ['Paired Node', 'Third Press Position', 'Reuse compatible bins']) {
        if (note.textContent.indexOf(want) < 0) {
            reportFailure(`forbiddenFields: note names "${want}"`, want, note.textContent);
        } else { passed++; }
    }

    await ctx.saveClaim();
    if (apiRecorder.length !== 1) {
        reportFailure('forbiddenFields: expected 1 POST', 1, apiRecorder.length);
        return;
    }
    const body = apiRecorder[0].body;
    const cleared = {
        paired_core_node: '',
        second_paired_core_node: '',
        reuse_compatible_bins: false,
    };
    for (const k of Object.keys(cleared)) {
        if (JSON.stringify(body[k]) !== JSON.stringify(cleared[k])) {
            reportFailure(`forbiddenFields: ${k} dropped for two_robot`, cleared[k], body[k]);
        } else { passed++; }
    }
    // ...and what the mode DOES use is untouched.
    if (body.inbound_staging !== 'IN1') {
        reportFailure('forbiddenFields: two_robot keeps inbound staging', 'IN1', body.inbound_staging);
    } else { passed++; }
}

// A mode with nothing to discard says nothing. A note that is always on is a
// note nobody reads.
function runNoDropNoteWhenNothingToDropCase() {
    const elements = buildDOM();
    const ctx = createContext(elements, []);
    loadProcessesJS(ctx);

    ctx.editClaim({
        id: 44,
        core_node_name: 'N1',
        role: 'consume',
        swap_mode: 'two_robot',
        payload_code: 'PL1',
        inbound_staging: 'IN1',
    });
    ctx.renderClaimForm();
    const note = elements['claims-mode-drop-note'];
    if (note.hidden === false && note.style.display !== 'none') {
        reportFailure('dropNote: silent when the mode discards nothing', 'hidden', note.textContent);
    } else { passed++; }
}

// -----------------------------------------------------------------------
// Round 2 unit 3 — the node pickers are filtered and ranked
// -----------------------------------------------------------------------
//
// All six geometry pickers were dumps of every core node. These assert the
// options the code actually produces, so a filter that silently stops
// filtering, or one that starts hiding a legitimate choice, both show up.

function optionValues(sel) {
    return (sel.options || []).map((o) => o.value).filter((v) => v !== '');
}
function optionGroup(sel, value) {
    const o = (sel.options || []).find((x) => x.value === value);
    return o ? o._group : '(absent)';
}

function runNodePickerFilterCase() {
    const elements = buildDOM();
    const ctx = createContext(elements, []);
    loadProcessesJS(ctx);

    // A style must be selected: editClaim and openClaimModal both
    // early-return without one, and every assertion below would then be
    // about a form that was never populated.
    elements['claims-style-selector'].value = '9';
    ctx.onClaimsStyleChanged();

    ctx.editClaim({
        id: 70,
        core_node_name: 'PRESS_A',
        role: 'produce',
        swap_mode: 'two_robot_press_index',
        payload_code: 'PL1',
        paired_core_node: 'PRESS_B',
        outbound_destination: 'SMG_01',
    });
    ctx.renderClaimForm();

    // A paired POSITION cannot be a group: the press-index builder emits
    // pickup/dropoff at concrete coordinates.
    const paired = elements['claims-add-paired-node'];
    if (optionValues(paired).indexOf('SMG_01') >= 0) {
        reportFailure('pickers: a group is not offered as a press position', 'SMG_01 absent', optionValues(paired).join(','));
    } else { passed++; }

    // ...and not the claim's own node, nor the other paired position.
    if (optionValues(paired).indexOf('PRESS_A') >= 0) {
        reportFailure('pickers: a position cannot be paired with itself', 'PRESS_A absent', optionValues(paired).join(','));
    } else { passed++; }
    const second = elements['claims-add-second-paired-node'];
    if (optionValues(second).indexOf('PRESS_B') >= 0) {
        reportFailure('pickers: the third position excludes the back position', 'PRESS_B absent', optionValues(second).join(','));
    } else { passed++; }

    // Line positions on this process rank first.
    if (optionGroup(paired, 'PRESS_C') !== 'This process') {
        reportFailure('pickers: this process ranks first for positions', 'This process', optionGroup(paired, 'PRESS_C'));
    } else { passed++; }
    if (optionGroup(paired, 'STOR_01') !== 'Other nodes') {
        reportFailure('pickers: off-process nodes rank second', 'Other nodes', optionGroup(paired, 'STOR_01'));
    } else { passed++; }

    // STAGING excludes groups but keeps concrete nodes.
    const inbound = elements['claims-add-inbound'];
    if (optionValues(inbound).indexOf('SMG_01') >= 0) {
        reportFailure('pickers: staging excludes groups', 'SMG_01 absent', optionValues(inbound).join(','));
    } else { passed++; }
    if (optionValues(inbound).indexOf('STAGE_01') < 0) {
        reportFailure('pickers: staging keeps concrete nodes', 'STAGE_01 present', optionValues(inbound).join(','));
    } else { passed++; }

    // ENDPOINTS take a group OR a node, groups first — and hide NOTHING but
    // self. SMN_014 is a dedicated loader home: a line position AND a valid
    // InboundSource (source_finder tier 2). A filter keyed on "not a line
    // position" would hide a supported configuration, so it must be offered.
    const src = elements['claims-add-inbound-source'];
    if (optionValues(src).indexOf('SMN_014') < 0) {
        reportFailure('pickers: a dedicated loader home is still a valid source', 'SMN_014 present', optionValues(src).join(','));
    } else { passed++; }
    if (optionGroup(src, 'SMG_01') !== 'Groups') {
        reportFailure('pickers: endpoints rank groups first', 'Groups', optionGroup(src, 'SMG_01'));
    } else { passed++; }
    if (optionValues(src).indexOf('PRESS_A') >= 0) {
        reportFailure('pickers: a cell cannot source from itself', 'PRESS_A absent', optionValues(src).join(','));
    } else { passed++; }
    // A press other than this one is unusual, not impossible — still offered.
    if (optionValues(src).indexOf('PRESS_C') < 0) {
        reportFailure('pickers: endpoints exclude only self', 'PRESS_C present', optionValues(src).join(','));
    } else { passed++; }
}

// The value a claim already holds is ALWAYS in its picker, even when the
// filter would exclude it. A picker that drops the value it is displaying
// makes the next save write a blank — unit 2's bug in a different hat.
function runNodePickerKeepsOutOfFilterValueCase() {
    const elements = buildDOM();
    const ctx = createContext(elements, []);
    loadProcessesJS(ctx);

    // A style must be selected: editClaim and openClaimModal both
    // early-return without one, and every assertion below would then be
    // about a form that was never populated.
    elements['claims-style-selector'].value = '9';
    ctx.onClaimsStyleChanged();

    // A press-index claim whose back position was configured as a GROUP —
    // wrong, and exactly what an operator inherits from an older config.
    ctx.editClaim({
        id: 71,
        core_node_name: 'PRESS_A',
        role: 'produce',
        swap_mode: 'two_robot_press_index',
        payload_code: 'PL1',
        paired_core_node: 'SMG_01',
        outbound_destination: 'SMG_01',
    });
    ctx.renderClaimForm();

    const paired = elements['claims-add-paired-node'];
    if (paired.value !== 'SMG_01') {
        reportFailure('pickers: an out-of-filter current value survives', 'SMG_01', paired.value);
    } else { passed++; }
    const kept = (paired.options || []).find((o) => o.value === 'SMG_01');
    if (!kept) {
        reportFailure('pickers: the out-of-filter value is still an option', 'present', 'dropped');
    } else if (!/not offered/.test(kept.textContent)) {
        reportFailure('pickers: the kept value says why it looks odd', 'labelled', kept.textContent);
    } else { passed++; }
}

// The escape hatch: a plant's naming can defeat any heuristic.
function runShowAllNodesCase() {
    const elements = buildDOM();
    const ctx = createContext(elements, []);
    loadProcessesJS(ctx);

    // A style must be selected: editClaim and openClaimModal both
    // early-return without one, and every assertion below would then be
    // about a form that was never populated.
    elements['claims-style-selector'].value = '9';
    ctx.onClaimsStyleChanged();

    ctx.editClaim({
        id: 72,
        core_node_name: 'PRESS_A',
        role: 'produce',
        swap_mode: 'two_robot_press_index',
        payload_code: 'PL1',
        paired_core_node: 'PRESS_B',
        outbound_destination: 'SMG_01',
    });
    ctx.renderClaimForm();
    if (optionValues(elements['claims-add-paired-node']).indexOf('SMG_01') >= 0) {
        reportFailure('showAll: filtered by default', 'SMG_01 absent', 'present');
    } else { passed++; }

    elements['claims-show-all-nodes'].checked = true;
    ctx.toggleShowAllNodes();
    if (optionValues(elements['claims-add-paired-node']).indexOf('SMG_01') < 0) {
        reportFailure('showAll: reveals every node', 'SMG_01 present', 'absent');
    } else { passed++; }

    // Not persisted — re-opening the modal starts filtered again.
    ctx.openClaimModal();
    if (elements['claims-show-all-nodes'].checked) {
        reportFailure('showAll: not persisted across a modal open', 'unchecked', 'checked');
    } else { passed++; }
}

// -----------------------------------------------------------------------
// Round 2 unit 4 — the invisible fields get controls
// -----------------------------------------------------------------------
//
// auto_reorder, keep_staged and sequence are live persisted columns. Round 1
// stopped the editor CORRUPTING them; they were still unreachable. A form that
// owns a field sends it, so these now appear in the POST body — except
// sequence 0, which means "no opinion" and stays absent so the store can
// assign the next free board slot.

async function runSurfacedFieldsSaveCase() {
    const elements = buildDOM();
    const apiRecorder = [];
    const ctx = createContext(elements, apiRecorder);
    loadProcessesJS(ctx);
    elements['claims-style-selector'].value = '11';
    ctx.onClaimsStyleChanged();

    ctx.editClaim({
        id: 80,
        core_node_name: 'N1',
        role: 'consume',
        swap_mode: 'single_robot',
        payload_code: 'PL1',
        inbound_staging: 'IN1',
        outbound_staging: 'OUT1',
        sequence: 4,
        auto_reorder: true,
        keep_staged: true,
    });

    // The stored values reach the controls.
    if (elements['claims-add-sequence'].value !== '4') {
        reportFailure('surfaced: sequence loads into its input', '4', elements['claims-add-sequence'].value);
    } else { passed++; }
    if (!elements['claims-add-auto-reorder'].checked) {
        reportFailure('surfaced: auto_reorder loads into its checkbox', true, false);
    } else { passed++; }
    if (!elements['claims-add-keep-staged'].checked) {
        reportFailure('surfaced: keep_staged loads into its checkbox', true, false);
    } else { passed++; }

    // The operator changes them.
    elements['claims-add-sequence'].value = '9';
    elements['claims-add-auto-reorder'].checked = false;
    elements['claims-add-keep-staged'].checked = false;

    await ctx.saveClaim();
    if (apiRecorder.length !== 1) {
        reportFailure('surfaced: expected 1 POST', 1, apiRecorder.length);
        return;
    }
    const body = apiRecorder[0].body;
    const want = { sequence: 9, auto_reorder: false, keep_staged: false };
    for (const k of Object.keys(want)) {
        if (JSON.stringify(body[k]) !== JSON.stringify(want[k])) {
            reportFailure(`surfaced: body[${k}] — a form that owns a field sends it`, want[k], body[k]);
        } else { passed++; }
    }
}

// sequence 0 is "no opinion", not "position zero". It must stay ABSENT so the
// store's next-free-slot default still fires; sending a literal 0 would claim
// the top of the board for every new claim.
async function runSequenceZeroStaysAbsentCase() {
    const elements = buildDOM();
    const apiRecorder = [];
    const ctx = createContext(elements, apiRecorder);
    loadProcessesJS(ctx);
    elements['claims-style-selector'].value = '11';
    ctx.onClaimsStyleChanged();

    ctx.openClaimModal();
    elements['claims-add-node'].value = 'PRESS_A';
    elements['claims-add-role'].value = 'consume';
    elements['claims-add-swap'].value = 'sequential';
    elements['claims-add-payload'].value = 'PL1';
    elements['claims-add-sequence'].value = '0';

    await ctx.saveClaim();
    if (apiRecorder.length !== 1) {
        reportFailure('sequenceZero: expected 1 POST', 1, apiRecorder.length);
        return;
    }
    if ('sequence' in apiRecorder[0].body) {
        reportFailure('sequenceZero: 0 means no opinion and must not be sent',
            'absent', apiRecorder[0].body.sequence);
    } else { passed++; }
    // ...but the two flags are always sent, because the form owns them.
    for (const k of ['auto_reorder', 'keep_staged']) {
        if (!(k in apiRecorder[0].body)) {
            reportFailure(`sequenceZero: ${k} is owned by this form and must be sent`, 'present', 'absent');
        } else { passed++; }
    }
}

// The compare grid is a DIFFERENT surface with a different answer: it edits one
// field per cell and owns none of these, so it must keep omitting all four.
// That is the whole point of absent-means-untouched, and it is easy to break by
// copying the modal's body.
function runCompareGridStillOmitsCase() {
    const elements = buildDOM();
    const ctx = createContext(elements, []);
    loadProcessesJS(ctx);
    const body = ctx.claimToBody({
        style_id: 1, core_node_name: 'N1', role: 'consume', swap_mode: 'sequential',
        payload_code: 'PL1', sequence: 5, auto_reorder: true, keep_staged: true,
        reorder_point_source: 'calculated',
    });
    for (const k of ['sequence', 'auto_reorder', 'keep_staged', 'reorder_point_source']) {
        if (k in body) {
            reportFailure(`compareGrid: ${k} must stay absent (the grid owns no control for it)`, 'absent', body[k]);
        } else { passed++; }
    }
}

// -----------------------------------------------------------------------
// Round 2 unit 6 — the table knows about rounds 3 and 4
// -----------------------------------------------------------------------
//
// The point of these is PRESENCE. A field that is not registered in
// claimFieldVisibility is unenterable no matter how much backend supports it —
// that is the round-1 trap that would have shipped IndexRobotSupplies with no
// way to set it. Rounds 3 and 4 should add a control and a value, not go
// hunting for every place a field must be declared.
//
// Asserted against the table directly, not against DOM visibility: with no
// element present isVisible() returns false, so a DOM assertion would pass
// identically whether or not the key had ever been added. That is the vacuous
// test this file exists to avoid.

const ROUND_3_4_SLOTS = [
    'claims-add-tooling-relevance-row',
    'claims-add-evac-destination-group',
    'claims-add-index-robot-supplies-row',
    'claims-routing-fieldset',
    'claims-add-key-routes-group',
];

function runRound34SlotCases() {
    const elements = buildDOM();
    const ctx = createContext(elements, []);
    loadProcessesJS(ctx);

    for (const role of ROLES) {
        for (const swap of SWAPS) {
            const vis = ctx.claimFieldVisibility(role, swap);
            for (const key of ROUND_3_4_SLOTS) {
                if (!(key in vis)) {
                    reportFailure(
                        `round3/4 slots: ${key} missing from claimFieldVisibility(${role}, ${swap}) — ` +
                        `an unregistered field is unenterable whatever the backend does`,
                        'present', 'absent');
                } else if (vis[key] !== false) {
                    reportFailure(
                        `round3/4 slots: ${key} must be inert until its round ships`,
                        false, vis[key]);
                } else { passed++; }
            }
        }
    }

    // Today's staging rule is UNCHANGED: plain press-index neither shows nor
    // requires staging. Round 3's evac mode is what grants it, and that is
    // behind a constant — so this must stay false now, and it is the assertion
    // that catches someone "preparing" round 3 by turning it on early.
    const pressIndex = ctx.claimFieldVisibility('produce', 'two_robot_press_index');
    if (pressIndex['claims-staging-fieldset'] !== false) {
        reportFailure('round3/4 slots: press-index staging stays hidden until round 3',
            false, pressIndex['claims-staging-fieldset']);
    } else { passed++; }
    // ...while the modes that use staging today are untouched by the new term.
    for (const swap of ['single_robot', 'two_robot']) {
        const v = ctx.claimFieldVisibility('consume', swap);
        if (v['claims-staging-fieldset'] !== true) {
            reportFailure(`round3/4 slots: ${swap} still shows staging`, true, v['claims-staging-fieldset']);
        } else { passed++; }
    }
}

(async () => {
    runVisibilityCases();
    await runFieldErrorRenderCase();
    await runHiddenFieldSurvivalCase();
    await runForbiddenFieldDropCase();
    runNoDropNoteWhenNothingToDropCase();
    runNodePickerFilterCase();
    runNodePickerKeepsOutOfFilterValueCase();
    runShowAllNodesCase();
    await runSurfacedFieldsSaveCase();
    await runSequenceZeroStaysAbsentCase();
    runCompareGridStillOmitsCase();
    runRound34SlotCases();
    runServerFieldErrorCase();
    runOrphanFieldErrorCase();
    await runSaveClaimSchemaCase();
    await runSaveClaimManualSwapCase();
    await runSaveClaimManualSwapNoPickerCase();
    await runSaveClaimEvacuateOnChangeoverCase();

    if (failures > 0) {
        console.error(`\nFAILED: ${failures} assertion(s); ${passed} passed`);
        process.exit(1);
    } else {
        console.log(`PASS: ${passed} assertions across ${ROLES.length * SWAPS.length} (role,swap) cells + saveClaim schema cases`);
        process.exit(0);
    }
})();
