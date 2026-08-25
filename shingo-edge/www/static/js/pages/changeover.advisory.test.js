// changeover.advisory.test.js — does the unresolved-participants advisory
// actually RENDER?
//
// The question this answers is "what appears on the page", and only rendering
// it can answer that. Asserting that changeover.js contains the string
// 'changeover-advisory' would be a true answer to a narrower question: it would
// pass with the element hidden, the list empty, or the node names dropped.
//
// Loaded the same way processes.characterization.test.js loads its target: an
// ES module run through vm.runInContext with the import line stripped and the
// imported names supplied as bare globals.

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let failures = 0;
let passed = 0;

function fail(what, expected, actual) {
    failures++;
    console.log(`FAIL: ${what}\n  expected: ${expected}\n  actual:   ${actual}`);
}

function check(what, cond, expected, actual) {
    if (cond) { passed++; } else { fail(what, expected, actual); }
}

// A DOM stub with exactly what changeover.js touches at module scope plus the
// one element the advisory writes into.
function makeContext() {
    const advisory = { id: 'changeover-advisory', hidden: true, innerHTML: '' };
    const pageData = { dataset: { processId: '7' } };
    const document = {
        body: {},
        getElementById(id) {
            if (id === 'changeover-advisory') return advisory;
            if (id === 'page-data') return pageData;
            return null;
        },
    };
    const context = vm.createContext({
        document,
        window: {},
        api: { post: () => Promise.resolve({}), get: () => Promise.resolve({}) },
        escapeHtml: (s) => String(s)
            .replace(/&/g, '&amp;').replace(/</g, '&lt;')
            .replace(/>/g, '&gt;').replace(/"/g, '&quot;'),
        toast: () => {},
        confirm: () => Promise.resolve(true),
        navigateToProcess: () => {},
        delegateActions: () => {},
        htmx: { trigger: () => {} },
        console, parseInt, parseFloat, JSON, Math,
        Array, Object, String, Number, Boolean, Promise,
        setTimeout, clearTimeout, encodeURIComponent,
    });
    let src = fs.readFileSync(path.join(__dirname, 'changeover.js'), 'utf8');
    src = src.replace(/^import \{[^}]+\} from [^\n]+\n+/m, '');
    // Expose the function under test — module scope is not reachable from
    // outside a vm script otherwise.
    src += '\n;globalThis.__render = renderUnresolvedParticipants;';
    vm.runInContext(src, context);
    return { context, advisory };
}

// --- a real advisory shows, names every node, and is not hidden ------------
{
    const { context, advisory } = makeContext();
    context.__render(['PLN_002_C', 'PLN_002_D']);

    check('advisory is visible', advisory.hidden === false, false, advisory.hidden);
    check('advisory names the first node',
        advisory.innerHTML.includes('PLN_002_C'), 'contains PLN_002_C', advisory.innerHTML);
    check('advisory names the second node',
        advisory.innerHTML.includes('PLN_002_D'), 'contains PLN_002_D', advisory.innerHTML);
    check('advisory says the changeover is still running',
        /running regardless/i.test(advisory.innerHTML), 'says it is not blocking', advisory.innerHTML);
    check('advisory points at where to fix it',
        advisory.innerHTML.includes('/processes'), 'links to the process nodes page', advisory.innerHTML);
    // The list is indexed-over positions only — ones that own no changeover task.
    // Positions that DO own a task have their rows auto-created at start and are
    // driveable without configuration, so the advisory must not describe the
    // listed nodes as work the changeover is waiting on.
    check('advisory says these positions own no task',
        /owns? no changeover task/i.test(advisory.innerHTML),
        'explains the positions own no task', advisory.innerHTML);
    check('advisory pluralises noun AND verb for two',
        advisory.innerHTML.includes('2 participant nodes have'), '2 participant nodes have', advisory.innerHTML);
}

// --- one node reads as one node -------------------------------------------
{
    const { context, advisory } = makeContext();
    context.__render(['PLN_002_C']);
    check('advisory singularises noun AND verb for one',
        advisory.innerHTML.includes('1 participant node has'),
        '1 participant node has', advisory.innerHTML);
}

// --- nothing to say means nothing shown ------------------------------------
for (const [label, value] of [['undefined', undefined], ['null', null], ['empty array', []]]) {
    const { context, advisory } = makeContext();
    advisory.hidden = false;              // start visible so clearing is observable
    advisory.innerHTML = 'stale content';
    context.__render(value);
    check(`advisory hidden for ${label}`, advisory.hidden === true, true, advisory.hidden);
    check(`advisory cleared for ${label}`, advisory.innerHTML === '', "''", advisory.innerHTML);
}

// --- node names are escaped, not injected ----------------------------------
{
    const { context, advisory } = makeContext();
    context.__render(['<img src=x onerror=alert(1)>']);
    check('node names are HTML-escaped',
        !advisory.innerHTML.includes('<img'), 'no raw <img', advisory.innerHTML);
}

if (failures > 0) {
    console.log(`\nFAILED: ${failures} assertion(s); ${passed} passed`);
    process.exit(1);
}
console.log(`OK: ${passed} assertions passed`);
