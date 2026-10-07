// changeover.refresh.test.js — the Changeover page's live refresh.
//
// Two things, both about what the page does when #changeover-content is
// re-fetched (lane F and LC4):
//   - an action's burst of triggers (its own refreshChangeover plus the SSE
//     events it causes) costs ONE reload, and order traffic keeps 2 s spacing;
//   - a reload keeps the chosen target style and its open preview.
//
// Loaded like changeover.advisory.test.js: the module run through
// vm.runInContext with the import line stripped and its imports supplied as
// globals. Timers and the clock are fakes, so the windows are exact.

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let failures = 0;
let passed = 0;

function check(what, cond, expected, actual) {
    if (cond) { passed++; return; }
    failures++;
    console.log(`FAIL: ${what}\n  expected: ${expected}\n  actual:   ${actual}`);
}

function makeOption(value, disabled) { return { value, disabled: !!disabled }; }

function makeSelect(options) {
    return { options, value: '' };
}

function makeContext() {
    let now = 1000000;
    let timers = [];
    let nextID = 1;
    const listeners = {};
    const elements = {};
    const fakeSetTimeout = (fn, ms) => { const id = nextID++; timers.push({ id, at: now + ms, fn }); return id; };
    const fakeClearTimeout = (id) => { timers = timers.filter((t) => t.id !== id); };
    const advance = (ms) => {
        const until = now + ms;
        for (;;) {
            timers.sort((a, b) => a.at - b.at);
            const t = timers[0];
            if (!t || t.at > until) break;
            timers.shift();
            now = t.at;
            t.fn();
        }
        now = until;
    };
    const document = {
        body: {
            addEventListener(type, fn) { (listeners[type] = listeners[type] || []).push(fn); },
        },
        getElementById(id) {
            if (id === 'page-data') return { dataset: { processId: '7' } };
            return elements[id] || null;
        },
    };
    const context = vm.createContext({
        document,
        window: {},
        api: { post: () => Promise.resolve({}), get: () => Promise.resolve({}) },
        escapeHtml: (s) => String(s),
        toast: () => {},
        confirm: () => Promise.resolve(true),
        navigateToProcess: () => {},
        delegateActions: () => {},
        htmx: { trigger: () => {} },
        Date: { now: () => now },
        setTimeout: fakeSetTimeout,
        clearTimeout: fakeClearTimeout,
        console, parseInt, parseFloat, JSON, Math,
        Array, Object, String, Number, Boolean, Promise, encodeURIComponent,
    });
    let src = fs.readFileSync(path.join(__dirname, 'changeover.js'), 'utf8');
    src = src.replace(/^import \{[^}]+\} from [^\n]+\n+/m, '');
    vm.runInContext(src, context);

    const content = { id: 'changeover-content' };
    let requests = 0;
    // fire dispatches an htmx event to the body listeners, as htmx does by
    // bubbling it from #changeover-content.
    const fire = (type, detail) => {
        let prevented = false;
        const evt = { detail, preventDefault() { prevented = true; } };
        (listeners[type] || []).forEach((fn) => fn(evt));
        return prevented;
    };
    const trigger = (eventType) => fire('htmx:confirm', {
        elt: content,
        target: content,
        triggeringEvent: { type: eventType },
        issueRequest: () => { requests++; },
    });
    return {
        elements, advance, fire, trigger, content,
        requests: () => requests,
    };
}

// --- one action's burst is one reload ---------------------------------------
{
    const t = makeContext();
    const prevented = t.trigger('refreshChangeover');
    t.advance(40); t.trigger('sse:changeover-update');
    t.advance(40); t.trigger('sse:order-update');
    t.advance(40); t.trigger('sse:order-failed');
    check('htmx\'s own request is held back', prevented === true, true, prevented);
    check('nothing reloads inside the settle window', t.requests() === 0, 0, t.requests());
    t.advance(1000);
    check('a burst of four triggers is one reload', t.requests() === 1, 1, t.requests());
    t.advance(10000);
    check('and no trailing reload follows it', t.requests() === 1, 1, t.requests());
}

// --- order traffic keeps its 2 s spacing ------------------------------------
{
    const t = makeContext();
    t.trigger('sse:order-update');
    t.advance(300);
    check('a lone order-update reloads after the settle window', t.requests() === 1, 1, t.requests());
    t.advance(200); t.trigger('sse:order-update');
    t.advance(1000);
    check('a second order-update inside 2 s waits', t.requests() === 1, 1, t.requests());
    t.advance(700);                           // 2.2 s: 1.9 s after the last reload
    check('not before 2 s have passed', t.requests() === 1, 1, t.requests());
    t.advance(200);
    check('it reloads 2 s after the last reload', t.requests() === 2, 2, t.requests());
}

// --- an action is not held back by order spacing ----------------------------
{
    const t = makeContext();
    t.trigger('sse:order-update');
    t.advance(300);                           // reload 1
    t.advance(100); t.trigger('sse:order-update'); // due 2 s after reload 1
    t.advance(100); t.trigger('refreshChangeover'); // an action: settle only
    t.advance(300);
    check('an action\'s reload comes after the settle window, not the order gap',
        t.requests() === 2, 2, t.requests());
    t.advance(5000);
    check('the order-update joined it (no extra reload)', t.requests() === 2, 2, t.requests());
}

// --- other htmx requests on the page are left alone -------------------------
{
    const t = makeContext();
    const prevented = t.fire('htmx:confirm', {
        elt: { id: 'some-node-button' }, triggeringEvent: { type: 'click' }, issueRequest: () => {},
    });
    check('a node action button is not held back', prevented === false, false, prevented);
}

// --- a reload keeps the chosen style and its preview ------------------------
// swapWith models one htmx 2 swap of the partial as the live check saw it:
//   - the partial is replaced: fresh elements, nothing chosen, preview empty;
//   - an element whose id was on the old page first carries the OLD element's
//     attributes (htmx's settle transition), so at afterSwap the panel's
//     display is the old one;
//   - about 20 ms later the settle step puts the server's attributes back
//     (style="display:none" on the preview panel), then afterSettle fires.
function swapWith(t, newSelect) {
    t.fire('htmx:beforeSwap', { target: t.content });
    const old = t.elements['changeover-preview'];
    const oldDisplay = old ? old.style.display : 'none';
    t.elements['co-to-style'] = newSelect;
    t.elements['changeover-preview'] = { style: { display: oldDisplay } };
    t.elements['changeover-preview-body'] = { innerHTML: '' };
    t.fire('htmx:afterSwap', { target: t.content });
    t.advance(20);
    if (t.elements['changeover-preview']) t.elements['changeover-preview'].style.display = 'none';
    t.fire('htmx:afterSettle', { target: t.content });
}
{
    const t = makeContext();
    t.elements['co-to-style'] = Object.assign(makeSelect([makeOption(''), makeOption('3'), makeOption('5')]), { value: '5' });
    t.elements['changeover-preview'] = { style: { display: '' } };
    t.elements['changeover-preview-body'] = { innerHTML: '<table>plan for 5</table>' };
    swapWith(t, makeSelect([makeOption(''), makeOption('3'), makeOption('5')]));
    check('the chosen style survives a reload', t.elements['co-to-style'].value === '5', '5', t.elements['co-to-style'].value);
    check('the preview stays open', t.elements['changeover-preview'].style.display === '', "''",
        t.elements['changeover-preview'].style.display);
    check('the preview keeps its plan', t.elements['changeover-preview-body'].innerHTML === '<table>plan for 5</table>',
        'plan for 5', t.elements['changeover-preview-body'].innerHTML);

    // The live check's failure: the second refresh found the panel hidden by
    // the first one's settle, saved nothing, and emptied the preview.
    swapWith(t, makeSelect([makeOption(''), makeOption('3'), makeOption('5')]));
    check('a second reload keeps the style', t.elements['co-to-style'].value === '5', '5', t.elements['co-to-style'].value);
    check('a second reload keeps the preview open', t.elements['changeover-preview'].style.display === '', "''",
        t.elements['changeover-preview'].style.display);
    check('a second reload keeps the plan', t.elements['changeover-preview-body'].innerHTML === '<table>plan for 5</table>',
        'plan for 5', t.elements['changeover-preview-body'].innerHTML);
}
{
    // A failed reload (htmx: shouldSwap false, no afterSwap/afterSettle) arms
    // nothing: the operator's next choice is the one carried, not a stale one.
    const t = makeContext();
    t.elements['co-to-style'] = Object.assign(makeSelect([makeOption(''), makeOption('3'), makeOption('5')]), { value: '5' });
    t.elements['changeover-preview'] = { style: { display: 'none' } };
    t.elements['changeover-preview-body'] = { innerHTML: '' };
    t.fire('htmx:beforeSwap', { target: t.content, shouldSwap: false });
    t.elements['co-to-style'].value = '3';
    swapWith(t, makeSelect([makeOption(''), makeOption('3'), makeOption('5')]));
    check('after a failed reload the current choice is carried', t.elements['co-to-style'].value === '3', '3',
        t.elements['co-to-style'].value);
}
{
    // A refresh that starts before the last one has settled (its restore not
    // yet applied) still carries the choice.
    const t = makeContext();
    t.elements['co-to-style'] = Object.assign(makeSelect([makeOption(''), makeOption('5')]), { value: '5' });
    t.elements['changeover-preview'] = { style: { display: '' } };
    t.elements['changeover-preview-body'] = { innerHTML: 'plan' };
    t.fire('htmx:beforeSwap', { target: t.content });
    t.elements['co-to-style'] = makeSelect([makeOption(''), makeOption('5')]);
    t.elements['changeover-preview'] = { style: { display: 'none' } };
    t.elements['changeover-preview-body'] = { innerHTML: '' };
    // second swap begins before the first one's afterSwap/afterSettle
    t.fire('htmx:beforeSwap', { target: t.content });
    t.elements['co-to-style'] = makeSelect([makeOption(''), makeOption('5')]);
    t.elements['changeover-preview'] = { style: { display: 'none' } };
    t.elements['changeover-preview-body'] = { innerHTML: '' };
    t.fire('htmx:afterSwap', { target: t.content });
    t.fire('htmx:afterSettle', { target: t.content });
    check('an overlapping refresh keeps the style', t.elements['co-to-style'].value === '5', '5', t.elements['co-to-style'].value);
    check('an overlapping refresh keeps the preview', t.elements['changeover-preview'].style.display === '' &&
        t.elements['changeover-preview-body'].innerHTML === 'plan', 'open with plan',
        JSON.stringify([t.elements['changeover-preview'].style.display, t.elements['changeover-preview-body'].innerHTML]));
}
{
    const t = makeContext();
    t.elements['co-to-style'] = Object.assign(makeSelect([makeOption(''), makeOption('5')]), { value: '5' });
    t.elements['changeover-preview'] = { style: { display: 'none' } };
    t.elements['changeover-preview-body'] = { innerHTML: '' };
    swapWith(t, makeSelect([makeOption(''), makeOption('5')]));
    check('a closed preview stays closed', t.elements['changeover-preview'].style.display === 'none', 'none',
        t.elements['changeover-preview'].style.display);
    check('the style is still kept without a preview', t.elements['co-to-style'].value === '5', '5',
        t.elements['co-to-style'].value);
}
{
    const t = makeContext();
    t.elements['co-to-style'] = Object.assign(makeSelect([makeOption(''), makeOption('5')]), { value: '5' });
    t.elements['changeover-preview'] = { style: { display: '' } };
    t.elements['changeover-preview-body'] = { innerHTML: 'plan' };
    swapWith(t, makeSelect([makeOption(''), makeOption('5', true)]));
    check('a style no longer offered is not re-chosen', t.elements['co-to-style'].value === '', "''",
        t.elements['co-to-style'].value);
    check('nor is its preview reopened', t.elements['changeover-preview'].style.display === 'none', 'none',
        t.elements['changeover-preview'].style.display);
}
{
    const t = makeContext();
    t.elements['co-to-style'] = Object.assign(makeSelect([makeOption(''), makeOption('5')]), { value: '5' });
    t.fire('htmx:beforeSwap', { target: t.content });
    delete t.elements['co-to-style']; // a changeover started: the partial has no picker
    let threw = null;
    try { t.fire('htmx:afterSwap', { target: t.content }); } catch (e) { threw = e; }
    check('a partial with no picker is left alone', threw === null, 'no error', threw);
}

if (failures > 0) {
    console.log(`\nFAILED: ${failures} assertion(s); ${passed} passed`);
    process.exit(1);
}
console.log(`OK: ${passed} assertions passed`);
