// ask-pending.test.js — a desktop page's action that went to Core as an ask
// and is still out (the Edge answers 202 {pending, notice}) must say so, and
// never say it was done.
//
// The station's postAction (operator-util.js) has handled the 202 since the
// asks landed; the Production page's LOAD and REQUEST EMPTY and the
// containment page's hold clear posted through api.post, which resolves any
// 2xx, and showed "Bin loaded" / "Empty bin requested" / "Hold cleared" over
// an act Core had not answered (RULINGS D1).
//
// Each page function is lifted out of its file by name and run in a vm with
// stubbed DOM, toast and transport; postAct itself (ask-act.js) is loaded the
// same way and driven with a stubbed fetch.

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

// extractFn returns the source of the function named name in src: from its
// declaration to the brace that closes it.
function extractFn(src, name) {
    const re = new RegExp('(async\\s+)?function\\s+' + name + '\\s*\\(');
    const m = re.exec(src);
    if (!m) throw new Error('no function ' + name);
    let i = src.indexOf('{', m.index);
    let depth = 0;
    for (; i < src.length; i++) {
        if (src[i] === '{') depth++;
        else if (src[i] === '}') { depth--; if (depth === 0) return src.slice(m.index, i + 1); }
    }
    throw new Error('unclosed function ' + name);
}

const PENDING = { status: 'pending', pending: true, notice: 'sent — waiting for Core' };

// A transport whose every action is answered 202 pending: api.post resolves
// the body (handleResponse reads any 2xx as done), postAct returns it.
function pendingTransport(posted) {
    return {
        api: { post: async (url, body) => { posted.push(url); return PENDING; } },
        postAct: async (url, body) => { posted.push(url); return PENDING; },
    };
}

function pageContext(extra) {
    const toasts = [];
    const els = {
        'rb-node-id': { value: '42' },
        'rb-payload-code': { value: 'PART-A' },
        're-node-id': { value: '42' },
        're-payload': { value: 'PART-A' },
    };
    const qty = { value: '5', dataset: { part: 'P-1', desc: '' } };
    const document = {
        getElementById: (id) => els[id] || null,
        querySelectorAll: (sel) => (sel === '.rb-manifest-qty' ? [qty] : []),
        querySelector: () => null,
    };
    const ctx = vm.createContext(Object.assign({
        document,
        window: { location: { reload: () => toasts.push(['reload']) } },
        toast: (msg, type) => toasts.push([type, msg]),
        hideModal: () => {},
        closeLoadBinModal: () => {},
        confirmHolding: async () => true,
        applyHeldReload: () => {},
        parseInt, String, Number, isNaN, JSON, console, Promise,
    }, extra));
    return { ctx, toasts };
}

function runPage(file, fn, setup) {
    const src = fs.readFileSync(path.join(__dirname, file), 'utf8');
    const posted = [];
    const { ctx, toasts } = pageContext(pendingTransport(posted));
    vm.runInContext(extractFn(src, fn) + '\n;globalThis.__fn = ' + fn + ';', ctx);
    return { run: () => ctx.__fn.apply(setup || null, setup ? [setup] : []), toasts, posted };
}

async function pagesSayPending() {
    const cases = [
        ['production.js', 'submitLoadBin', null, 'Bin loaded'],
        ['production.js', 'submitRequestEmpty', null, 'Empty bin requested'],
        ['containment.js', 'unholdHeldBin', { dataset: { binId: '7' } }, 'Hold cleared'],
    ];
    for (const [file, fn, el, doneWords] of cases) {
        const p = runPage(file, fn, el);
        await p.run();
        const said = JSON.stringify(p.toasts);
        check(`${fn}: the act was sent`, p.posted.length === 1, '1 post', p.posted.length);
        check(`${fn}: a 202 pending answer shows no success toast`,
            !p.toasts.some(([type]) => type === 'success'), 'no success toast', said);
        check(`${fn}: a 202 pending answer never says "${doneWords}"`,
            !said.includes(doneWords), 'not ' + doneWords, said);
        check(`${fn}: a 202 pending answer shows the notice`,
            p.toasts.some(([type, msg]) => type === 'info' && msg === PENDING.notice), 'info: ' + PENDING.notice, said);
        check(`${fn}: a 202 pending answer does not reload the page over the notice`,
            !p.toasts.some(([kind]) => kind === 'reload'), 'no reload', said);
    }
}

// postAct: the 202 is held by URL for ASK_HOLD_MS, a 200 is the body, a refusal
// throws its sentence — and the hold is the station's.
async function postActHolds() {
    const src = fs.readFileSync(path.join(__dirname, '..', 'ask-act.js'), 'utf8');
    let now = 1000;
    const sent = [];
    let answer = { status: 202, body: PENDING };
    const ctx = vm.createContext({
        Date: { now: () => now },
        Map, JSON, String, Promise, console,
        fetch: async (url, opts) => {
            sent.push(url);
            const a = answer;
            const res = {
                ok: a.status >= 200 && a.status < 300,
                status: a.status,
                json: async () => a.body,
                text: async () => JSON.stringify(a.body),
                clone() { return res; },
            };
            return res;
        },
    });
    vm.runInContext(src.replace(/^export const /gm, 'var ').replace(/^export /gm, ''), ctx);

    const first = await ctx.postAct('/api/process-nodes/42/load-bin', {});
    check('postAct: a 202 pending answer is returned as pending', first && first.pending === true, 'pending', JSON.stringify(first));
    const again = await ctx.postAct('/api/process-nodes/42/load-bin', {});
    check('postAct: the same act inside the hold is not sent again', sent.length === 1, '1 send', sent.length);
    check('postAct: the held act answers pending with the notice', again && again.pending && again.notice === PENDING.notice,
        'pending + notice', JSON.stringify(again));
    answer = { status: 200, body: { ok: true } };
    const other = await ctx.postAct('/api/process-nodes/43/load-bin', {});
    check('postAct: another node is not held', sent.length === 2 && other && other.ok === true, '2 sends, ok', sent.length + ' ' + JSON.stringify(other));
    now += ctx.ASK_HOLD_MS + 1;
    await ctx.postAct('/api/process-nodes/42/load-bin', {});
    check('postAct: past the hold the act is sent again', sent.length === 3, '3 sends', sent.length);
    answer = { status: 409, body: { error: 'bin changed since you looked' } };
    let thrown = null;
    try { await ctx.postAct('/api/process-nodes/44/load-bin', {}); } catch (e) { thrown = e; }
    check('postAct: a refusal throws its sentence', thrown === 'bin changed since you looked', 'the sentence', String(thrown));

    // The hold is the station's: one number for one rule (operator-util.js).
    const util = fs.readFileSync(path.join(__dirname, '..', '..', 'operator-station', 'operator-util.js'), 'utf8');
    const m = /const ASK_HOLD_MS = (\d+);/.exec(util);
    check('postAct holds as long as the station does', m && Number(m[1]) === ctx.ASK_HOLD_MS,
        'station ' + (m && m[1]), String(ctx.ASK_HOLD_MS));
}

(async () => {
    await pagesSayPending();
    let postActLoaded = true;
    try { fs.accessSync(path.join(__dirname, '..', 'ask-act.js')); } catch (e) { postActLoaded = false; }
    if (postActLoaded) {
        await postActHolds();
    } else {
        failures++;
        console.log('FAIL: static/js/ask-act.js does not exist');
    }
    if (failures > 0) {
        console.log(`\nFAILED: ${failures} assertion(s); ${passed} passed`);
        process.exit(1);
    }
    console.log(`OK: ${passed} assertions passed`);
})();
