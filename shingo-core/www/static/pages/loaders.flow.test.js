// Unit tests for the Nodes page's stations: the create card, the box (the
// slots it asks for, what it says when one is empty, tap-to-assign) and the
// Settings behind its one link. Run under plain Node via the Go wrapper
// loaders_flow_gating_test.go. Exit 0 on pass, 1 on any assertion failure.
//
// The shape these pin: the create card asks three things and nothing else;
// every place a station uses is a slot on its box, labelled in plant words;
// an empty required slot is red and says what it needs, and the box header
// counts them; everything with a default sits in Settings, which saves one
// row at a time and writes every other stored field back unchanged.
//
// Two older guards carry over, re-expressed on the box:
//   - A station's source is always visible. Springfield ran a dedicated loader
//     with a blank inbound_source and the replenishment chain was mute with
//     nothing on any screen to say so. On the box that is a red "Needs a
//     place" under "Empties come from", unless the station is fed directly.
//   - The Edge SKIPS a loader it cannot project and says so only in journald.
//     The missing-member refusals are the red slots now; the malformed-member
//     ones stay a red tag in the header (configGapHtml).
//
// Visibility is derived from state by formShape (docs/ui-style-guide.md
// form-state convention), so the rules are asserted directly as well as
// through the DOM.

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
function makeEl(id, tag) {
    const classes = new Set();
    const attrs = {};
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
        setAttribute(k, v) { attrs[k] = String(v); },
        getAttribute(k) { return k in attrs ? attrs[k] : null; },
        addEventListener() {},
        focus() {},
    };
}

function shown(el) { return !el.classList.contains('is-hidden'); }

// load builds a fresh module instance. opts.auth renders the signed-in page.
// Every apiPost is recorded in order on h.posts, and resolves {} unless
// opts.post says otherwise.
function load(opts) {
    opts = opts || {};
    const ids = [
        'loader-modal', 'loader-name', 'loader-submit-btn',
        'loader-role-produce', 'loader-role-consume',
        'loader-stages-row', 'loader-stages-single', 'loader-stages-two',
        'loader-name-error', 'loader-role-error', 'loader-stages-error', 'loader-form-error',
        'loader-mix-add-type', 'loader-mix-add-want',
    ];
    const els = {};
    ids.forEach(function (id) { els[id] = makeEl(id); });
    if (opts.auth) {
        els['page-data'] = makeEl('page-data', 'div');
        els['page-data'].dataset.authenticated = 'true';
    }
    const listeners = [];
    const posts = [];
    const toasts = [];
    // The page builds markup with app.js's h``; this is the same helper, with
    // an escape that does what the browser's text-node escape does.
    const esc = function (v) { return String(v).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'); };
    const h = function (strings) {
        let out = strings[0];
        for (let i = 1; i < arguments.length; i++) {
            const v = arguments[i];
            if (Array.isArray(v)) out += v.join('');
            else if (v === null || v === undefined || v === false) { /* skipped */ }
            else if (typeof v === 'object' && v.__html === true) out += v.value;
            else out += esc(v);
            out += strings[i];
        }
        return out;
    };
    const ctxObj = {
        console: console,
        Promise: Promise,
        // readyState is deliberately NOT 'complete': the module's tail would
        // call init() → refresh() → apiGet and hit the network.
        document: {
            readyState: 'loading',
            getElementById(id) { return els[id] || null; },
            querySelectorAll() { return []; },
            querySelector() { return null; },
            addEventListener(type, fn, capture) { listeners.push({ type: type, fn: fn, capture: !!capture }); },
            createElement(tag) { return makeEl('', tag); },
        },
        window: { innerHeight: 900, scrollBy() {} },
        setInterval() { return 0; },
        clearInterval() {},
        setTimeout() { return 0; },
        // Injected in place of the stripped ES import.
        apiGet(url) { return Promise.resolve(url === '/api/loader/list' && opts.list ? { loaders: opts.list() } : {}); },
        apiPost(url, body) { posts.push({ url: url, body: body }); return Promise.resolve(opts.post ? opts.post(url, body) : {}); },
        delegateActions() {},
        h: h,
        toast(msg, level) { toasts.push({ msg: msg, level: level }); },
        uiConfirm() { return Promise.resolve(true); },
    };
    vm.createContext(ctxObj);
    const src = fs.readFileSync(path.join(__dirname, 'loaders.js'), 'utf8')
        .replace(/^import[^;]+;\s*/m, '');   // drop the ES import; deps injected above
    vm.runInContext(src, ctxObj);
    return {
        ctx: ctxObj, els: els, posts: posts, listeners: listeners, toasts: toasts,
        set(expr) { vm.runInContext(expr, ctxObj); },
        get(expr) { return vm.runInContext(expr, ctxObj); },
    };
}

function tick() { return new Promise(function (r) { setImmediate(r); }); }
async function settle() { for (let i = 0; i < 10; i++) await tick(); }

const nodesHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'templates', 'nodes.html'), 'utf8');
const binsHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'templates', 'bins.html'), 'utf8');
const smktSrc = fs.readFileSync(path.join(__dirname, 'nodes-supermarket.js'), 'utf8');

function loaderModalMarkup() {
    const a = nodesHtml.indexOf('<div id="loader-modal"');
    const b = nodesHtml.indexOf('<!-- Add Lane Modal -->');
    return nodesHtml.slice(a, b);
}

// Fixtures. Plain synthetic names; no plant data.
function unloader(over) {
    return Object.assign({
        id: 20, name: 'UNL-A', role: 'consume', layout: 'shared_window', replenishment: 'operator',
        inbound_source: '', outbound_dest: '', funnel_windows: false, fed_directly: false,
    }, over || {});
}
function loader(over) {
    return Object.assign({
        id: 21, name: 'LDR-A', role: 'produce', layout: 'shared_window', replenishment: 'threshold',
        inbound_source: '', outbound_dest: '', funnel_windows: false, fed_directly: false,
    }, over || {});
}
function pair(over1, over2) {
    const one = { loader: unloader(Object.assign({ id: 7, name: 'PAIR-A · stage 1', second_stage_loader_id: 8 }, over1 || {})),
        homes: [], payloads: [] };
    const two = { loader: unloader(Object.assign({ id: 8, name: 'PAIR-A · stage 2' }, over2 || {})),
        homes: [], payloads: [] };
    return [one, two];
}
function labels(group) {
    return group.slots.filter(function (s) { return s.label; }).map(function (s) { return s.label; });
}

(async function main() {

// --- frame 1: the create card --------------------------------------------

console.log('create card — three questions and nothing else');

(function cardMarkup() {
    const m = loaderModalMarkup();
    check('card: titled "New station", opened from "+ Station"',
        m.indexOf('>New station<') >= 0 && nodesHtml.indexOf('>+ Station</button>') >= 0);
    check('card: no data-backdrop-close (a data-input modal closes by button or Esc)',
        m.indexOf('data-backdrop-close') < 0);
    check('card: modal-body and modal-footer', m.indexOf('class="modal-body"') >= 0 && m.indexOf('class="modal-footer"') >= 0);
    check('card: × and Cancel both close it',
        (m.match(/data-action="closeLoaderModal"/g) || []).length === 2);
    check('card: a .form-error slot under each question',
        ['loader-name', 'loader-role', 'loader-stages'].every(function (f) {
            return m.indexOf('class="form-error" id="' + f + '-error" data-error-for="' + f + '"') >= 0;
        }));
    ['Name', 'What does it do?', 'Fills bins', 'Empties bins', 'How does the full bin come off?',
        'One station', 'The operator removes the bin and adds a bin.',
        'Two stations', 'The first takes the full bin off the cart. The second puts an empty bin on.', 'Create',
    ].forEach(function (w) { check('card words: "' + w + '"', m.indexOf(w) >= 0); });
    check('card: gone — layout, supply, carrier, destinations, bare type, checkboxes',
        ['loader-kind', 'loader-replenishment', 'loader-carrier-type', 'loader-stage1-sends',
            'loader-stage2-sends', 'loader-inbound', 'loader-outbound', 'loader-bare-type',
            'type="checkbox"', 'Leaves carriers bare as'].every(function (x) { return m.indexOf(x) < 0; }));
    check('card: the node datalists are gone from the page',
        nodesHtml.indexOf('loader-nodes-dl') < 0 && nodesHtml.indexOf('<datalist') < 0);
})();

(function cardShape() {
    const h = load();
    const shape = h.ctx.formShape;
    const card = function (over) { return Object.assign(h.ctx.blankForm(), over || {}); };
    check('stages: not asked before a role is picked', shape(card()).stages === false);
    check('stages: asked of an unloader', shape(card({ role: 'consume' })).stages === true);
    check('stages: never asked of a loader', shape(card({ role: 'produce' })).stages === false);
    const s = shape(card({ role: 'consume' }));
    check('card: no Settings row appears on the create card',
        ['partials', 'autoPush', 'fedByHand', 'supply', 'changeover', 'dedicated', 'funnel', 'mix', 'windows', 'remove']
            .every(function (k) { return s[k] === false; }));
    check('a loader is never two stations, whatever was picked first',
        h.ctx.normalizeForm(card({ role: 'produce', stages: 'two' })).stages === '');
})();

(function cardValidation() {
    const h = load();
    const v = function (over) { return h.ctx.validateForm(Object.assign(h.ctx.blankForm(), over)); };
    const fields = function (r) { return r.errors.map(function (e) { return e.field; }).join(','); };
    check('validate: name, role required', fields(v({})) === 'loader-name,loader-role');
    check('validate: an unloader must say how the bin comes off',
        fields(v({ name: 'U', role: 'consume' })) === 'loader-stages');
    check('validate: a loader needs only name and role', v({ name: 'L', role: 'produce' }).ok);
    check('validate: an unloader with an answer is ok',
        v({ name: 'U', role: 'consume', stages: 'single' }).ok && v({ name: 'U', role: 'consume', stages: 'two' }).ok);
})();

(function cardRequests() {
    const h = load();
    const req = function (over) { return h.ctx.createRequest(Object.assign(h.ctx.blankForm(), over)); };
    const l = req({ name: 'L', role: 'produce' });
    check('create loader: /api/loader/create, role produce, no unloader switches',
        l.url === '/api/loader/create' && l.body.role === 'produce' && l.body.name === 'L' &&
        !('auto_push' in l.body) && !('accept_partials' in l.body));
    const u = req({ name: 'U', role: 'consume', stages: 'single' });
    check('create one-station unloader: auto pull defaults on',
        u.url === '/api/loader/create' && u.body.role === 'consume' && u.body.auto_push === true);
    const t = req({ name: 'P', role: 'consume', stages: 'two' });
    check('create two stations: the new body — name, accept_partials, auto_push; nothing else',
        t.url === '/api/loader/create-two-stage' &&
        JSON.stringify(Object.keys(t.body).sort()) === JSON.stringify(['accept_partials', 'auto_push', 'name']) &&
        t.body.auto_push === true && t.body.accept_partials === false);
})();

await (async function cardThroughTheDom() {
    const h = load();
    h.ctx.openLoaderModal();
    check('open: the card is active, nothing picked',
        h.els['loader-modal'].classList.contains('active') &&
        h.els['loader-role-produce'].getAttribute('aria-pressed') === 'false' &&
        !shown(h.els['loader-stages-row']));
    h.ctx.pickStationRole('consume');
    check('Empties bins: pressed, and the second question appears',
        h.els['loader-role-consume'].classList.contains('is-selected') &&
        h.els['loader-role-consume'].getAttribute('aria-pressed') === 'true' &&
        shown(h.els['loader-stages-row']));
    h.ctx.pickStationStages('two');
    check('Two stations: pressed', h.els['loader-stages-two'].classList.contains('is-selected'));
    h.ctx.pickStationRole('produce');
    check('back to Fills bins: the second question goes, and its answer with it',
        !shown(h.els['loader-stages-row']) && !h.els['loader-stages-two'].classList.contains('is-selected'));

    // Submitting a blank card shows each error under its own question.
    const h2 = load();
    h2.ctx.openLoaderModal();
    await h2.ctx.submitLoader();
    check('submit blank: errors land in their slots, nothing is posted',
        h2.els['loader-name-error'].textContent === 'Name is required' &&
        h2.els['loader-role-error'].textContent === 'Pick what it does' &&
        h2.els['loader-name'].classList.contains('form-input--error') && h2.posts.length === 0);

    // A good card posts once and closes.
    const h3 = load({ post: function () { return { id: 31 }; } });
    h3.ctx.openLoaderModal();
    h3.els['loader-name'].value = 'P';
    h3.ctx.pickStationRole('consume');
    h3.ctx.pickStationStages('two');
    await h3.ctx.submitLoader();
    check('submit two stations: one POST to create-two-stage, card closed',
        h3.posts.length === 1 && h3.posts[0].url === '/api/loader/create-two-stage' &&
        !h3.els['loader-modal'].classList.contains('active'));
    check('submit: the new station’s windows are armed for the next tap',
        h3.get('armed && armed.loaderID') === 31 && h3.get('armed && armed.slot') === 'windows');

    // A server refusal shows in the card's own error slot and keeps it open.
    const h4 = load({ post: function () { return { error: 'name taken' }; } });
    h4.ctx.openLoaderModal();
    h4.els['loader-name'].value = 'L';
    h4.ctx.pickStationRole('produce');
    await h4.ctx.submitLoader();
    check('submit refused: the reason in the card, card still open',
        h4.els['loader-form-error'].textContent === 'name taken' && h4.els['loader-modal'].classList.contains('active'));

    // Esc closes the card and clears it.
    const h5 = load();
    h5.ctx.openLoaderModal();
    h5.els['loader-name'].value = 'X';
    h5.ctx.pickStationRole('consume');
    const esc = h5.listeners.filter(function (l) { return l.type === 'keydown'; });
    esc.forEach(function (l) { l.fn({ key: 'Escape' }); });
    check('Esc: closes the card and clears what was in it',
        esc.length >= 1 && !h5.els['loader-modal'].classList.contains('active') &&
        h5.els['loader-name'].value === '' && !h5.els['loader-role-consume'].classList.contains('is-selected'));
})();

// --- frame 2: the box ----------------------------------------------------

console.log('the box — slots in plant words, red until filled');

(function singleStationSlots() {
    const h = load();
    const u = h.ctx.stationSlots({ loader: unloader(), homes: [], payloads: [] }, null);
    check('one-station unloader: Windows · Fulls come from · Empties go to · Parts it drains',
        JSON.stringify(labels(u[0])) === JSON.stringify(['Windows', 'Fulls come from', 'Empties go to', 'Parts it drains']));
    const l = h.ctx.stationSlots({ loader: loader(), homes: [], payloads: [] }, null);
    check('loader: Windows · Empties come from · Fulls go to · Parts it fills',
        JSON.stringify(labels(l[0])) === JSON.stringify(['Windows', 'Empties come from', 'Fulls go to', 'Parts it fills']));
    const d = h.ctx.stationSlots({ loader: loader({ layout: 'dedicated_positions' }), homes: [], payloads: [] }, null);
    check('one spot per part: no shared part set — each position picks its own',
        labels(d[0]).indexOf('Parts it fills') < 0);
})();

(function pairSlots() {
    const h = load();
    const p = pair();
    const g = h.ctx.stationSlots(p[0], p[1]);
    check('stage 1: Windows · Fulls come from · Parts it drains · Carts go on to (shown, not asked)',
        JSON.stringify(labels(g[0])) === JSON.stringify(['Windows', 'Fulls come from', 'Parts it drains', 'Carts go on to']) &&
        g[0].slots[3].readonly === true && g[0].slots[3].required === false);
    check('stage 2: Windows · Carts with an empty bin go to · the wait checkbox, unticked',
        JSON.stringify(labels(g[1])) === JSON.stringify(['Windows', 'Carts with an empty bin go to']) &&
        g[1].slots[2].key === 'waitCheck' && g[1].slots[2].checked === false);
    const pw = pair({ outbound_dest: 'WAIT-G' }, { inbound_source: 'WAIT-G' });
    const gw = h.ctx.stationSlots(pw[0], pw[1]);
    check('pull mode: the box is ticked and says where carts wait; stage 1 shows no second answer',
        gw[1].slots[2].checked === true && labels(gw[1]).indexOf('Carts wait at') === 2 &&
        gw[1].slots[3].value === 'WAIT-G' && labels(gw[0]).indexOf('Carts go on to') < 0);
    // Ticked before a group is named: the slot opens, red, and nothing is saved.
    h.set('waitOpen[7] = true');
    const go = h.ctx.stationSlots(p[0], p[1]);
    check('ticked, no group yet: "Carts wait at" appears empty and required',
        go[1].slots[3] && go[1].slots[3].label === 'Carts wait at' && go[1].slots[3].empty && go[1].slots[3].required);
    check('stage 2 is not asked for parts — it is fed carts, not fulls',
        labels(g[1]).indexOf('Parts it drains') < 0);
})();

(function needsAndHeader() {
    const h = load();
    const fresh = h.ctx.stationSlots({ loader: unloader(), homes: [], payloads: [] }, null);
    check('a new one-station unloader needs 4: windows, where fulls come from, where empties go, a part',
        h.ctx.countNeeds(fresh) === 4);
    check('header: "Not running yet — 4 slots need a place"',
        h.ctx.statusText(4) === 'Not running yet — 4 slots need a place');
    check('header: singular for one', h.ctx.statusText(1) === 'Not running yet — 1 slot needs a place');
    check('header: nothing when nothing is missing', h.ctx.statusText(0) === '');
    const done = h.ctx.stationSlots({ loader: unloader({ inbound_source: 'IN-G', outbound_dest: 'OUT-G' }),
        homes: [{ position_node_id: 1 }], payloads: [{ payload_code: 'P1' }] }, null);
    check('a complete station needs nothing', h.ctx.countNeeds(done) === 0);
    const fed = h.ctx.stationSlots({ loader: unloader({ fed_directly: true, outbound_dest: 'OUT-G' }),
        homes: [{ position_node_id: 1 }], payloads: [{ payload_code: 'P1' }] }, null);
    check('fed directly from process: the source is not asked for', h.ctx.countNeeds(fed) === 0 && fed[0].slots[1].fed === true);
    const blank = h.ctx.stationSlots({ loader: unloader({ fed_directly: undefined, outbound_dest: 'OUT-G' }),
        homes: [{ position_node_id: 1 }], payloads: [{ payload_code: 'P1' }] }, null);
    check('a blank source is NOT "fed directly": only the field says so',
        h.ctx.countNeeds(blank) === 1 && blank[0].slots[1].empty && blank[0].slots[1].required && !blank[0].slots[1].fed);
    const p = pair();
    check('a new pair needs 5: two sets of windows, fulls source, a part, where carts go',
        h.ctx.countNeeds(h.ctx.stationSlots(p[0], p[1])) === 5);
})();

(function boxHtml() {
    const h = load({ auth: true });
    const html = h.ctx.gridHtml([{ loader: unloader(), homes: [], payloads: [] }]);
    check('box: header counts what is missing', html.indexOf('Not running yet — 4 slots need a place') >= 0);
    check('box: an empty required slot is red and says what it needs',
        html.indexOf('is-needed') >= 0 && html.indexOf('Needs a place') >= 0 &&
        html.indexOf('Needs a window') >= 0 && html.indexOf('Needs a part') >= 0 &&
        html.indexOf('tap, then tap a group or node') >= 0);
    check('box: each slot is a tap target (armSlot)', (html.match(/data-action="armSlot"/g) || []).length === 3);
    check('box: one Settings link', (html.match(/data-action="toggleStationSettings"/g) || []).length === 1 &&
        html.indexOf('>Settings<') >= 0);
    check('box: says what the station is', html.indexOf('Empties bins · one station') >= 0);
    const filled = h.ctx.gridHtml([{ loader: unloader({ inbound_source: 'IN-G', outbound_dest: 'OUT-G' }),
        homes: [{ position_node_id: 1 }], payloads: [{ payload_code: 'P1' }] }]);
    check('box: a filled place names it and can be cleared',
        filled.indexOf('IN-G') >= 0 && filled.indexOf('data-action="clearSlot"') >= 0 && filled.indexOf('Not running yet') < 0);
    const ro = load().ctx.gridHtml([{ loader: unloader(), homes: [], payloads: [] }]);
    check('box, signed out: still says what is missing, offers nothing to tap',
        ro.indexOf('Needs a place') >= 0 && ro.indexOf('data-action=') < 0);
})();

(function pairRendersAsOne() {
    const h = load({ auth: true });
    const p = pair();
    p[0].loader.bare_bin_type_code = 'CART-BARE';
    const html = h.ctx.gridHtml(p);
    check('pair: one station under its own name',
        (html.match(/class="loader-station loader-pair"/g) || []).length === 1 && html.indexOf('>PAIR-A<') >= 0);
    check('pair: stage 2 drawn inside the pair, not again on its own',
        (html.match(/class="loader-station[ "]/g) || []).length === 1);
    check('pair: each stage says what it does',
        html.indexOf('Stage 1 · takes the full bin off') >= 0 && html.indexOf('Stage 2 · puts an empty bin on') >= 0);
    check('pair: "Empties bins · two stations"', html.indexOf('Empties bins · two stations') >= 0);
    check('pair: no bin-type code and no "bare" anywhere on the box',
        html.indexOf('-BARE') < 0 && html.toLowerCase().indexOf('bare') < 0);
    check('pair: the wait checkbox, in plant words',
        html.indexOf('Carts wait in a group between the stations') >= 0 &&
        html.indexOf('data-action-change="toggleWaitGroup" data-loader-id="7"') >= 0);
    check('pair: stage 2 naming no payload is not flagged — the Edge allows it of an unloader',
        html.indexOf('no payloads') < 0);
})();

(function armedRendering() {
    const h = load({ auth: true });
    h.set('armed = {loaderID: 20, slot: "windows"}');
    const html = h.ctx.gridHtml([{ loader: unloader(), homes: [], payloads: [] }]);
    check('armed: the slot is marked and its button says to tap a node',
        html.indexOf('loader-slot-windows is-needed is-armed') >= 0 && html.indexOf('+ add — tap a node below') >= 0);
    h.set('loaderData = [{loader: ' + JSON.stringify(unloader()) + ', homes: [], payloads: []}]');
    check('armed: the bar says what is being added and how',
        h.ctx.assignBarHtml().indexOf('Adding a window — tap a node. Greyed nodes can’t go here.') >= 0 &&
        h.ctx.assignBarHtml().indexOf('data-action-input="filterAssign"') >= 0 &&
        h.ctx.assignBarHtml().indexOf('Esc stops adding') >= 0);
    h.set('armed = null');
    check('disarmed: no bar', h.ctx.assignBarHtml() === '');
    const esc = h.listeners.filter(function (l) { return l.type === 'keydown'; });
    h.set('armed = {loaderID: 20, slot: "windows"}');
    esc.forEach(function (l) { l.fn({ key: 'Escape' }); });
    check('Esc disarms', h.get('armed') === null);
    check('tap interception runs in the capture phase, ahead of the tile’s own action',
        h.listeners.some(function (l) { return l.type === 'click' && l.capture; }));
})();

console.log('tap-to-assign — what the grid dims, and why');

(function assignReasons() {
    const h = load();
    const p = pair();
    const other = { loader: unloader({ id: 40, name: 'PRESS4-UNLOAD' }), homes: [{ position_node_id: 140 }], payloads: [] };
    p[1].homes = [{ position_node_id: 41 }];
    h.set('loaderData = ' + JSON.stringify([p[0], p[1], other]));
    const a2 = { loaderID: 8, slot: 'windows' };
    const ctx2 = h.ctx.armContext(a2);
    const r = function (t) { return h.ctx.assignReason(a2, t, ctx2); };
    check('stage 2, direct mode', ctx2.stage === 2 && ctx2.direct === true);
    check('window slot: a group is not a window, and says so',
        r({ id: 90, name: 'G', group: true, synthetic: true }).note === 'a group can’t be a window');
    check('window slot: another station’s window says whose',
        r({ id: 140, name: 'SMN_040' }).note === 'window of PRESS4-UNLOAD' && !r({ id: 140 }).ok);
    check('window slot: its own window is marked as already there',
        r({ id: 41, name: 'SMN_041' }).current === true && r({ id: 41 }).note === 'already Stage 2');
    check('window slot, stage 2 direct: a node in a group says which',
        r({ id: 38, name: 'SMN_038', parentName: 'AMR Supermarket' }).note === 'in AMR Supermarket');
    check('window slot, stage 2: a node with a bin on it', r({ id: 44, name: 'SMN_044', hasBin: true }).note === 'has a bin on it');
    check('window slot: a free node lights up', r({ id: 42, name: 'SMN_042' }).ok && r({ id: 42 }).note === 'tap to add');
    const a1 = { loaderID: 7, slot: 'windows' };
    check('window slot, stage 1: being in a group does not stop it',
        h.ctx.assignReason(a1, { id: 38, name: 'SMN_038', parentName: 'AMR Supermarket' }, h.ctx.armContext(a1)).ok);

    const ao = { loaderID: 8, slot: 'outbound' };
    const cto = h.ctx.armContext(ao);
    const ro = function (t) { return h.ctx.assignReason(ao, t, cto); };
    check('place slot: a group lights up', ro({ id: 90, name: 'EMPTIES', group: true, synthetic: true, typeCode: 'NGRP' }).ok);
    check('place slot: a plain node is a place too', ro({ id: 42, name: 'SMN_042' }).ok);
    check('place slot: a lane is not', ro({ id: 91, name: 'LANE-1', synthetic: true, typeCode: 'LANE' }).note === 'a lane');
    check('place slot: the station’s own window is not a place to send to',
        ro({ id: 41, name: 'SMN_041' }).note === 'window of PAIR-A');

    const pw = pair({ outbound_dest: 'WAIT-G' }, { inbound_source: 'WAIT-G' });
    h.set('loaderData = ' + JSON.stringify(pw));
    const aw = { loaderID: 8, slot: 'wait' };
    const ctw = h.ctx.armContext(aw);
    check('pull mode: not direct, and the current group is marked',
        ctw.direct === false && h.ctx.assignReason(aw, { id: 90, name: 'WAIT-G', group: true }, ctw).current === true);
})();

console.log('filling a slot — the API calls, in order');

(function updateBodies() {
    const h = load();
    const s1 = unloader({ id: 7, name: 'PAIR-A · stage 1', second_stage_loader_id: 8, outbound_dest: 'DERIVED', funnel_windows: true });
    const s2 = unloader({ id: 8, name: 'PAIR-A · stage 2', outbound_dest: 'EMPTIES' });
    const w = h.ctx.waitGroupUpdates(s1, s2, 'WAIT-G');
    check('wait group: stage 2 first (pull from it), then stage 1 (send to it)',
        w.length === 2 && w[0].id === 8 && w[0].inbound_source === 'WAIT-G' && w[1].id === 7 && w[1].outbound_dest === 'WAIT-G');
    check('wait group: nothing else moves', w[0].outbound_dest === 'EMPTIES' && w[1].funnel_windows === true &&
        w[1].name === 'PAIR-A · stage 1');
    const c = h.ctx.waitGroupUpdates(s1, s2, '');
    check('unticking: both sides cleared, stage 2 first, so Core derives stage 1 again',
        c[0].id === 8 && c[0].inbound_source === '' && c[1].id === 7 && c[1].outbound_dest === '');
    const i = h.ctx.placeUpdate(unloader({ fed_directly: true }), 'inbound', 'IN-G');
    check('naming a source answers "fed directly" too', i.inbound_source === 'IN-G' && i.fed_directly === false);
})();

await (async function assignThroughTheApi() {
    const h = load({ auth: true });
    const p = pair();
    h.set('loaderData = ' + JSON.stringify(p));
    h.set('armed = {loaderID: 8, slot: "wait"}');
    h.ctx.assignToArmed({ id: 90, name: 'WAIT-G', group: true });
    await settle();
    check('tap a group for "Carts wait at": two updates, stage 2 then stage 1, then disarmed',
        h.posts.length === 2 && h.posts[0].url === '/api/loader/update' && h.posts[0].body.id === 8 &&
        h.posts[0].body.inbound_source === 'WAIT-G' && h.posts[1].body.id === 7 &&
        h.posts[1].body.outbound_dest === 'WAIT-G' && h.get('armed') === null);

    const one = [{ loader: unloader(), homes: [{ position_node_id: 5, sort_order: 0 }], payloads: [] }];
    const h2 = load({ auth: true, list: function () { return one; } });
    h2.set('loaderData = ' + JSON.stringify(one));
    h2.set('armed = {loaderID: 20, slot: "windows"}');
    h2.ctx.assignToArmed({ id: 6, name: 'N6' });
    await settle();
    check('tap a node for Windows: set-home then reorder with it last — the drop’s own calls',
        h2.posts.length === 2 && h2.posts[0].url === '/api/loader/set-home' && h2.posts[0].body.position_node_id === 6 &&
        h2.posts[1].url === '/api/loader/reorder-homes' && JSON.stringify(h2.posts[1].body.ordered_ids) === '[5,6]');
    check('tap a node for Windows: stays armed for the next one', h2.get('armed && armed.slot') === 'windows');

    // Drag: a group header dropped on a place slot.
    const lone = [{ loader: unloader(), homes: [], payloads: [] }];
    const h3 = load({ auth: true, list: function () { return lone; } });
    h3.set('loaderData = ' + JSON.stringify(lone));
    h3.set('nodesByName = {"EMPTIES": 90}; nodeInfo = {90: {id: 90, name: "EMPTIES", synthetic: true, typeCode: "NGRP"}}');
    const slotEl = makeEl('', 'div');
    slotEl.dataset.loaderId = '20';
    slotEl.dataset.slot = 'outbound';
    const dt = function (m) {
        return { types: Object.keys(m), getData(k) { return m[k] || ''; }, dropEffect: '' };
    };
    const ev = function (m) { return { preventDefault() {}, stopPropagation() {}, dataTransfer: dt(m), target: slotEl }; };
    h3.ctx.onPlaceDrop.call(slotEl, ev({ 'application/x-node-group': 'EMPTIES' }));
    await settle();
    check('drag a group onto "Empties go to": one update naming it',
        h3.posts.length === 1 && h3.posts[0].body.outbound_dest === 'EMPTIES');
    const boxEl = makeEl('', 'div');
    boxEl.dataset.loaderId = '20';
    boxEl.querySelectorAll = function () { return []; };
    h3.ctx.onBoxDrop.call(boxEl, ev({ 'application/x-node-group': 'EMPTIES' }));
    await settle();
    check('drag a group onto a Windows box: refused with "a group can’t be a window", nothing posted',
        h3.posts.length === 1 && h3.toasts.some(function (t) { return t.msg === 'a group can’t be a window'; }));
    const srcEl = makeEl('', 'div');
    srcEl.dataset.loaderId = '20';
    srcEl.dataset.slot = 'inbound';
    h3.ctx.onPlaceDrop.call(srcEl, ev({ 'application/x-node-group': 'EMPTIES' }));
    await settle();
    check('drag a group onto "Fulls come from": one update setting inbound_source',
        h3.posts.length === 2 && h3.posts[1].url === '/api/loader/update' &&
        h3.posts[1].body.inbound_source === 'EMPTIES' && h3.posts[1].body.fed_directly === false);
})();

(function supermarketIgnoresGroupDrags() {
    const body = function (fn) {
        const i = smktSrc.indexOf('function ' + fn + '(');
        return smktSrc.slice(i, smktSrc.indexOf('\n}\n', i));
    };
    check('group headers are a drag source', smktSrc.indexOf("addEventListener('dragstart', onGroupDragStart)") >= 0);
    check('a group drag carries only application/x-node-group, never text/plain',
        body('onGroupDragStart').indexOf("'application/x-node-group'") >= 0 && body('onGroupDragStart').indexOf('text/plain') < 0);
    check('onDropGrid ignores a group drag', body('onDropGrid').indexOf('isGroupDrag(e)') >= 0);
    check('the lane slots ignore a group drag', body('onDrop').indexOf('isGroupDrag(e)') >= 0);
})();

// --- frame 3: Settings ----------------------------------------------------

console.log('settings — one link, saves as ticked, nothing else moves');

(function settingsShape() {
    const h = load();
    const shape = function (l) { return h.ctx.formShape(h.ctx.formStateFromLoader(l)); };
    const u = shape(unloader());
    check('unloader: partials, auto pull, fed directly', u.partials && u.autoPush && u.fedByHand);
    check('unloader: no supply question and no changeover directive', !u.supply && !u.changeover);
    const l = shape(loader());
    check('loader: supply and changeover directive; no unloader switches', l.supply && l.changeover && !l.partials && !l.autoPush);
    check('fill one window at a time: offered to a shared-window station',
        shape(unloader()).funnel === true && shape(loader()).funnel === true);
    check('fill one window at a time: NOT offered where every spot is its own',
        shape(loader({ layout: 'dedicated_positions' })).funnel === false);
    check('mix and window capability: not for one spot per part',
        shape(loader({ layout: 'dedicated_positions' })).mix === false && shape(loader({ layout: 'dedicated_positions' })).windows === false);
    check('a pair has no layout to pick', shape(unloader({ second_stage_loader_id: 8 })).dedicated === false);
})();

(function settingsRoundTrip() {
    const h = load();
    const stored = unloader({ funnel_windows: true, inbound_source: 'IN-G', outbound_dest: 'OUT-G',
        changeover_load_directive: false, replenishment: 'operator', name: 'PAIR-A · stage 1', second_stage_loader_id: 8 });
    const b = h.ctx.settingUpdate(stored, 'acceptPartials', true);
    check('funnel_windows round-trips unchanged when its row is not touched (true)', b.funnel_windows === true);
    check('funnel_windows round-trips unchanged when its row is not touched (false)',
        h.ctx.settingUpdate(unloader({ funnel_windows: false }), 'autoPush', true).funnel_windows === false);
    check('ticking a row changes that row only',
        b.accept_partials === true && b.inbound_source === 'IN-G' && b.outbound_dest === 'OUT-G' &&
        b.name === 'PAIR-A · stage 1' && b.layout === 'shared_window' && b.replenishment === 'operator');
    check('ticking "Fill one window at a time" writes it', h.ctx.settingUpdate(unloader(), 'funnel', true).funnel_windows === true);
    check('ticking "Fed directly" clears the source',
        h.ctx.settingUpdate(stored, 'fedByHand', true).inbound_source === '' &&
        h.ctx.settingUpdate(stored, 'fedByHand', true).fed_directly === true);
    check('"One spot per part" is the layout', h.ctx.settingUpdate(loader(), 'dedicated', true).layout === 'dedicated_positions');
    check('a loader never sends an unloader switch as true',
        h.ctx.settingUpdate(loader({ accept_partials: true, auto_push: true }), 'funnel', true).accept_partials === false);
    check('no bare type on the wire, ever', !('bare_bin_type_id' in b));
})();

await (async function settingsSaveThroughTheApi() {
    const h = load({ auth: true });
    h.set('loaderData = ' + JSON.stringify([{ loader: unloader({ funnel_windows: true }), homes: [], payloads: [] }]));
    const cb = makeEl('', 'input');
    cb.type = 'checkbox';
    cb.checked = true;
    cb.setAttribute('data-loader-id', '20');
    cb.setAttribute('data-field', 'acceptPartials');
    await h.ctx.saveStationSetting(cb);
    check('a ticked row saves at once: one update, funnel untouched',
        h.posts.length === 1 && h.posts[0].url === '/api/loader/update' &&
        h.posts[0].body.accept_partials === true && h.posts[0].body.funnel_windows === true);

    // A new station shows the red source slot; ticking "Fed directly from
    // process" sends fed_directly:true, and the box then says so.
    const fresh = unloader({ fed_directly: false });
    const before = h.ctx.gridHtml([{ loader: fresh, homes: [], payloads: [] }]);
    check('a new station: the source slot is red', /loader-slot-inbound is-needed/.test(before) &&
        before.indexOf('Fed directly from process') < 0);
    const h2 = load({ auth: true });
    h2.set('loaderData = ' + JSON.stringify([{ loader: fresh, homes: [], payloads: [] }]));
    const fed = makeEl('', 'input');
    fed.type = 'checkbox';
    fed.checked = true;
    fed.setAttribute('data-loader-id', '20');
    fed.setAttribute('data-field', 'fedByHand');
    await h2.ctx.saveStationSetting(fed);
    check('ticking "Fed directly from process" sends fed_directly:true and a blank source',
        h2.posts.length === 1 && h2.posts[0].body.fed_directly === true && h2.posts[0].body.inbound_source === '');
    const after = h2.ctx.gridHtml([{ loader: unloader({ fed_directly: true }), homes: [], payloads: [] }]);
    check('then the box shows "Fed directly from process", not red',
        after.indexOf('Fed directly from process') >= 0 && !/loader-slot-inbound is-needed/.test(after));
})();

(function settingsHtml() {
    const h = load({ auth: true });
    h.set("binTypeCatalog = [{id: 1, code: 'CART', bare: false}, {id: 2, code: 'CART-BARE', bare: true}]");
    const u = { loader: unloader(), homes: [{ position_node_id: 5 }], payloads: [], window_bin_types: {} };
    h.set('loaderData = ' + JSON.stringify([u]));
    const html = h.ctx.settingsHtml(u, null);
    ['Accept partly used bins, not only full ones', 'Pull the next full automatically when a window frees',
        'Fed directly from process', 'One spot per part (each window takes one part only)', 'Fill one window at a time',
        'Keep on hand', 'Delete station', 'Changes save as you tick them.',
    ].forEach(function (w) { check('settings words: "' + w + '"', html.indexOf(w) >= 0); });
    check('settings, unloader: no supply dropdown, no changeover directive',
        html.indexOf('Order empties') < 0 && html.indexOf('During a changeover') < 0);
    check('settings: gone — "Leaves carriers bare as", the layout dropdown',
        html.indexOf('Leaves carriers bare as') < 0 && html.indexOf('Multi window') < 0 && html.indexOf('Single window') < 0);
    check('settings: every row saves as ticked', (html.match(/data-action-change="saveStationSetting"/g) || []).length === 5);
    check('settings: the window-type picker never offers a bare type',
        html.indexOf('>CART<') >= 0 && html.indexOf('CART-BARE') < 0);
    check('binTypeOptions(set, false) leaves bare out', h.ctx.binTypeOptions([], false).indexOf('CART-BARE') < 0);
    const d = h.ctx.settingsHtml({ loader: loader({ layout: 'dedicated_positions' }), homes: [], payloads: [] }, null);
    check('settings, one spot per part: no "Fill one window at a time"', d.indexOf('Fill one window at a time') < 0);
    check('settings, loader: supply and the changeover directive',
        d.indexOf('Order empties') >= 0 && d.indexOf('During a changeover, tell this station which carrier to load') >= 0);
    const p = pair();
    const ph = h.ctx.settingsHtml(p[0], p[1]);
    check('settings, pair: stage 1 rows; no layout row; windows of both stages',
        ph.indexOf('>Stage 1<') >= 0 && ph.indexOf('One spot per part') < 0 && ph.indexOf('Accept partly used bins') >= 0);
})();

(function binsPageBareCheckboxGone() {
    check('bins page: the bare checkbox is gone from both forms',
        binsHtml.indexOf('name="bare"') < 0 && binsHtml.indexOf('bt-edit-bare') < 0);
})();

// --- config gap: the malformed-member refusals ---------------------------

console.log('config gap — the refusals the Edge makes that are not an empty slot');

(function configGap() {
    const h = load();
    const gap = function (l, homes, payloads) { return h.ctx.configGapHtml({ loader: l, homes: homes || [], payloads: payloads || [] }); };
    check('a blank payload code is a refusal', gap(unloader(), [{ position_node_id: 1 }], [{ payload_code: '' }]).indexOf('a blank payload') >= 0);
    check('a position with no node is a refusal', gap(unloader(), [{ position_node_id: 0 }], [{ payload_code: 'p' }]).indexOf('a position with no node') >= 0);
    check('a complete station is silent', gap(unloader(), [{ position_node_id: 1 }], [{ payload_code: 'p' }]) === '');
    check('dedicated, position with no payload: legal, silent',
        gap(loader({ layout: 'dedicated_positions' }), [{ position_node_id: 62, payload_code: '' }]) === '');
    const html = gap(unloader(), [{ position_node_id: 0 }], [{ payload_code: 'p' }]);
    check('config gap is not a link', html.indexOf('<a') < 0);
    // THE SPRINGFIELD CASE: windows dragged in, payload never set. Now the
    // part slot is red and the header counts it.
    const s = h.ctx.stationSlots({ loader: unloader({ inbound_source: 'IN-G', outbound_dest: 'OUT-G' }),
        homes: [{ position_node_id: 99 }, { position_node_id: 102 }], payloads: [] }, null);
    check('windows but no part: exactly the part slot is missing', h.ctx.countNeeds(s) === 1 &&
        s[0].slots[3].key === 'parts' && s[0].slots[3].empty);
    // The dedicated blank-inbound case: the source slot is red unless the
    // station is fed directly.
    const d = h.ctx.stationSlots({ loader: loader({ layout: 'dedicated_positions', outbound_dest: 'OUT-G' }),
        homes: [{ position_node_id: 62 }], payloads: [] }, null);
    check('dedicated, blank source: "Empties come from" is red', d[0].slots[1].required && d[0].slots[1].empty);
    // THE SPRINGFIELD DEDICATED LOADER: a spot per part, fed from the
    // supermarket, no "Fulls go to" — its fulls stay on its own spots, and it
    // runs. The box must not call it "Not running yet".
    const spr = { loader: loader({ layout: 'dedicated_positions', replenishment: 'threshold',
        inbound_source: 'AMR Supermarket', outbound_dest: '' }),
        homes: [{ position_node_id: 62 }], payloads: [{ payload_code: 'X', uop_threshold: 1 }] };
    const sd = h.ctx.stationSlots(spr, null);
    check('dedicated loader, blank "Fulls go to": needs nothing', h.ctx.countNeeds(sd) === 0,
        'needs=' + h.ctx.countNeeds(sd));
    check('dedicated loader, blank "Fulls go to": says its fulls stay on its spots',
        sd[0].slots[2].stays === true && !sd[0].slots[2].required);
    const sprHtml = h.ctx.gridHtml([spr]);
    check('dedicated loader: no "Not running yet" header, and the slot reads "Stays on its own spots"',
        sprHtml.indexOf('Not running yet') < 0 && sprHtml.indexOf('Stays on its own spots') >= 0);
    const head = h.ctx.stationsHeadHtml(pair().concat([spr]));
    check('Stations heading: the page section shape, a pair counted once',
        head.indexOf('class="section-head node-section-head"') >= 0 && />Stations <span class="node-section-count">2</.test(head),
        head);
    const su = h.ctx.stationSlots({ loader: unloader({ layout: 'dedicated_positions', inbound_source: 'IN-G' }),
        homes: [{ position_node_id: 62 }], payloads: [] }, null);
    check('a dedicated UNLOADER still needs "Empties go to"', su[0].slots[2].required && su[0].slots[2].empty);
    const both = h.ctx.gridHtml([{ loader: loader({ replenishment: 'threshold' }), homes: [{ position_node_id: 0 }],
        payloads: [{ payload_code: 'X', uop_threshold: 0 }] }]);
    const g = both.indexOf('loader-config-gap'), t = both.indexOf('loader-threshold-gap');
    check('both warnings render when both apply, config gap first', g >= 0 && t >= 0 && g < t, 'gap=' + g + ' thr=' + t);
})();

if (failures > 0) {
    console.log('\nFAILED: ' + failures + ' assertion(s)');
    process.exit(1);
}
console.log('\nPASS: stations — create card, box, tap-to-assign, settings, config gap');

})().catch(function (e) { console.log('FAIL (threw): ' + (e && e.stack || e)); process.exit(1); });
