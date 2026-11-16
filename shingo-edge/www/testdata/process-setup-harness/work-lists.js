// work-lists.js — the robot work lists on the Styles tab (round 4), pinned on
// the fixture's Style A: a position's row clicked, its robots' steps drawn
// under it from the preview's steady state (flow/preview?steps=1).
//
// The steps are the builders' (steady.json, written by engine's
// TestSteady_PageHarnessFixture); this pins what the page draws of them: the
// robots in the order the server lists the orders, each with its words, and
// each step's glyph, verb, node and the role the node is named in.
//
// One position per swap, and an Index Swap on each Next carrier, set as an
// engineer sets it: the row's Advanced, Next carrier, the answer.
'use strict';

let T;

// workText is the open work list as lines: its head, then per robot its name
// and words, then its steps.
function workText(page) {
    const td = page.byId('pd-workrow');
    if (!td) return ['  (no work list under the row)'];
    const h = String(td.innerHTML);
    const out = [];
    const head = /<h2>([\s\S]*?)<\/h2>(?:<small>([\s\S]*?)<\/small>)?/.exec(h);
    if (head) out.push('  ' + T.decode(head[1]) + (head[2] ? ' — ' + T.decode(head[2]) : ''));
    for (const m of h.matchAll(/<p class="(pd-dim|pd-wlerr)"( title="[^"]*")?>([\s\S]*?)<\/p>/g)) {
        out.push('  ' + (m[1] === 'pd-wlerr' ? '! ' : '') + T.text(m[3]) + (m[2] ? ' [hover: ' + T.decode(m[2].slice(8, -1)) + ']' : ''));
    }
    for (const r of h.split('<div class="pd-wlrobot').slice(1)) {
        const name = /<b>([^<]*)<\/b><small>([^<]*)<\/small>/.exec(r) || [];
        out.push('  ' + T.decode(name[1] || '?') + ' · ' + T.decode(name[2] || ''));
        for (const s of r.matchAll(/<span class="pd-wlg">([^<]*)<\/span><span class="pd-wlv">([^<]*)<\/span><span class="pd-wln">([\s\S]*?)<\/span><\/span>/g)) {
            const role = (/data-role="([^"]*)"/.exec(s[3]) || [])[1];
            out.push('    ' + T.decode(s[1]) + ' ' + T.decode(s[2]).padEnd(13) + ' ' + T.text(s[3]) + (role ? ' (' + role + ')' : ''));
        }
    }
    return out;
}

async function select(page, node) {
    page.click('root', { row: node });
    await page.settle();
}

// nextCarrier sets node's Next carrier to v in node's Advanced sheet (the
// row's own, round 5), then closes it with Done.
async function nextCarrier(page, node, v) {
    page.click('root', { act: 'adv-open', node: node });
    await page.settle();
    if (String(page.scrim.innerHTML).indexOf('data-act="set-seg" data-key="refill_by" data-node="' + node + '"') < 0) {
        throw new Error(node + "'s Advanced sheet offers no Next carrier");
    }
    page.click('scrim', { act: 'set-seg', key: 'refill_by', node: node, v: v }, { inside: ['.pd-modal'] });
    await page.settle();
    page.pd('closeSheet')();
    await page.settle();
}

async function run(t) {
    T = t;
    const L = ['# The Styles tab on Press A\'s Style A: a row clicked, its robot work lists drawn under it. The steps',
        '# are the builders\' (steady.json); each line is glyph, verb, node and the role it is named in.'];
    const page = await T.openPage({ hash: '#style=1' });
    await page.settle();
    const asked = page.calls.filter(c => /\/flow\/preview/.test(c.url)).map(c => c.url);
    L.push('', '## the Styles tab\'s previews ask for the steps: ' + [...new Set(asked)].join(' · '));
    for (const [node, what] of [['POS_A5', '1-Robot Swap'], ['POS_A3', '2-Robot Swap'], ['POS_A7', 'Sequential A/B'],
        ['POS_A1', 'Index Swap, Next carrier as stored (Robot 1)']]) {
        await select(page, node);
        L.push('', '## ' + node + ' (' + what + ')', ...workText(page));
    }
    for (const [v, robot] of [['index_robot', 'Robot 2'], ['own_robot', 'Robot 3']]) {
        await nextCarrier(page, 'POS_A1', v);
        L.push('', '## POS_A1, Next carrier ' + robot + " (its row's Advanced › Next carrier)", ...workText(page));
    }
    // A swap not complete sends no robot: POS_A3's destination cleared.
    await select(page, 'POS_A3');
    page.pd('apply')({ type: 'setDest', node: 'POS_A3', dest: '' });
    await page.settle();
    L.push('', '## POS_A3 with its Outbound destination cleared (the builder refuses it)', ...workText(page));
    // A back position's click opens its press's list, under the press's row.
    // (POS_A3 let go first: a click on the selected row lets go of it.)
    await select(page, 'POS_A3');
    await select(page, 'POS_B1');
    L.push('', '## POS_B1 clicked (POS_A1\'s back position)', ...workText(page));
    T.out['work-lists.txt'] = L.join('\n') + '\n';
}

module.exports = { run };
