// drills.layer.test.js — the two drill pop-ups (components/CellDrill.js,
// components/DrillModal.js) are entries of the one overlay stack
// (shared/layer.js). Run by TestDrillsOnTheLayerJS (drills_layer_test.go).
// Exit 0 on pass, 1 on any failure.
//
// Each drill runs unmodified in the helper pins' realm
// (shared/testdata/helper-pins-harness/world.js): one tab with its own DOM,
// fake clock, fetch and module instances, the layer included. A drill is
// opened from a focused button, as a tile or a KPI opens it.
//
// What it holds, per drill:
//   - the page behind does not scroll while it is open (the counted lock), and
//     stays locked while a confirm over it opens and closes;
//   - Escape over a confirm on top of it closes the confirm first, and the
//     drill on the next Escape;
//   - a press that began in the card and ended on the backdrop does not close
//     it; a click on the backdrop does;
//   - focus goes back to what opened it;
//   - its 60 s idle close closes its entry (nothing left locked or open), and
//     closing cancels its read in flight;
//   - it never asks before closing (it holds no typed work).
// The drills drew their own overlay with their own Escape and backdrop: one
// Escape closed the drill and the confirm over it, the page scrolled behind
// it, a drag out of the card closed it, and focus was lost.

'use strict';

const path = require('path');

const REPO = path.join(__dirname, '..', '..', '..', '..', '..');
const { makeRealm, Event, Element } = require(path.join(REPO, 'shared', 'testdata', 'helper-pins-harness', 'world.js'));

// The realm keeps innerHTML as one raw node; the drills build their card with
// innerHTML and then look inside it (the ✕, the body, the range buttons), so
// this test reads that markup into elements: tags, quoted attributes, text,
// and the entities the drills write. Enough for their own templates, no more.
const ENTITIES = { '&times;': '×', '&amp;': '&', '&lt;': '<', '&gt;': '>', '&quot;': '"', '&#39;': "'", '&hellip;': '…' };
const decode = (s) => s.replace(/&[#\w]+;/g, (e) => ENTITIES[e] || e);
const VOID = new Set(['input', 'br', 'img', 'hr']);
Object.defineProperty(Element.prototype, 'innerHTML', {
    configurable: true,
    get() { return ''; },
    set(v) {
        this.textContent = '';
        const doc = this.ownerDocument;
        const stack = [this];
        const re = /<\/([a-z0-9-]+)\s*>|<([a-z0-9-]+)((?:\s+[a-z0-9-]+(?:="[^"]*")?)*)\s*\/?>|([^<]+)/gi;
        let m;
        while ((m = re.exec(String(v))) !== null) {
            const top = stack[stack.length - 1];
            if (m[1]) { if (stack.length > 1) stack.pop(); continue; }
            if (m[4] !== undefined) { if (m[4].trim()) top.appendChild(doc.createTextNode(decode(m[4]))); continue; }
            const e = doc.createElement(m[2].toLowerCase());
            for (const a of m[3].matchAll(/([a-z0-9-]+)(?:="([^"]*)")?/gi)) e.setAttribute(a[1], decode(a[2] || ''));
            top.appendChild(e);
            if (!VOID.has(m[2].toLowerCase())) stack.push(e);
        }
    },
});

let failed = 0;
let passed = 0;
function check(name, ok, detail) {
    if (ok) { passed++; console.log('ok  ' + name); }
    else { failed++; console.log('FAIL ' + name + (detail ? '\n     ' + detail : '')); }
}

const DRILLS = [
    { name: 'CellDrill', module: '/static/components/CellDrill.js', open: (m) => m.openCellDrill('CELL-1'), close: (m) => m.closeCellDrill() },
    { name: 'DrillModal', module: '/static/components/DrillModal.js', open: (m) => m.openDrillModal('success_rate', {}), close: (m) => m.closeDrillModal() },
];

function page(drill) {
    const r = makeRealm({ surface: 'core' });
    const d = r.doc;
    const signals = [];
    // Every read the drill makes is held open (never answered), so a close
    // can be seen to cancel it.
    r.answer = (url, init) => { signals.push(init && init.signal); return { hang: true }; };
    const realFetch = r.ctx.fetch;
    r.ctx.fetch = (url, init) => { signals.push(init && init.signal); return new Promise(() => {}); };
    void realFetch;
    const opener = d.createElement('button');
    opener.setAttribute('id', 'tile');
    d.body.appendChild(opener);
    const m = r.load(drill.module);
    const layer = r.load('/static/shared/layer.js');
    const ev = (type, extra) => new Event(type, Object.assign({ bubbles: true, cancelable: true }, extra || {}));
    return {
        r, d, m, layer, signals, opener,
        open() { opener.focus(); drill.open(m); },
        overlay: () => d.querySelectorAll('.drill-modal')[0] || null,
        isOpen() { return !!this.overlay(); },
        lock: () => d.body.style.overflow,
        escape() { (d.activeElement || d.body).dispatchEvent(ev('keydown', { key: 'Escape' })); },
        press(down, up) { down.dispatchEvent(ev('mousedown')); (up || down).dispatchEvent(ev('click')); },
        asked: () => d.querySelectorAll('.layer-scrim').some((s) => /Discard/.test(s.textContent)),
    };
}

function watch(p) {
    const w = { state: 'pending', value: undefined };
    p.then((v) => { w.state = 'resolved'; w.value = v; });
    return w;
}

(async () => {
    for (const drill of DRILLS) {
        const N = drill.name + ': ';
        {
            const p = page(drill);
            p.open();
            check(N + 'opens', p.isOpen());
            check(N + 'the page behind does not scroll while it is open', p.lock() === 'hidden', JSON.stringify(p.lock()));
            check(N + 'focus moves into it', p.overlay().contains(p.d.activeElement),
                p.d.activeElement && (p.d.activeElement.id || p.d.activeElement.tagName));
            const w = watch(p.layer.confirm('Sure?'));
            await p.r.advance(0);
            p.escape();
            await p.r.advance(0);
            check(N + 'Escape over a confirm on it: the confirm resolves false', w.state === 'resolved' && w.value === false, JSON.stringify(w));
            check(N + 'Escape over a confirm on it: the drill stays open', p.isOpen());
            check(N + 'and the page behind is still locked', p.lock() === 'hidden', JSON.stringify(p.lock()));
            p.escape();
            await p.r.advance(0);
            check(N + 'the next Escape closes it, nothing asked', !p.isOpen() && !p.asked());
            check(N + 'closed: the lock is back to the page\'s own', p.lock() === '', JSON.stringify(p.lock()));
            check(N + 'closed: focus is back on what opened it', p.d.activeElement === p.opener,
                p.d.activeElement && (p.d.activeElement.id || p.d.activeElement.tagName));
            check(N + 'closed: its read in flight is cancelled', p.signals.length > 0 && p.signals.every((s) => s && s.aborted),
                p.signals.map((s) => s && s.aborted).join(','));
        }
        {
            const p = page(drill);
            p.open();
            const card = p.overlay().querySelector('.modal');
            p.press(card, p.overlay());
            await p.r.advance(0);
            check(N + 'a press in the card released on the backdrop does not close it', p.isOpen());
            if (!p.isOpen()) p.open();
            p.press(p.overlay());
            await p.r.advance(0);
            check(N + 'a click on the backdrop closes it', !p.isOpen() && p.layer.layerDepth() === 0);
        }
        {
            const p = page(drill);
            p.open();
            await p.r.advance(59000);
            check(N + 'still open at 59 s', p.isOpen());
            await p.r.advance(2000);
            check(N + 'its 60 s idle close closes its entry', !p.isOpen() && p.layer.layerDepth() === 0 && p.lock() === '',
                'open ' + p.isOpen() + ', depth ' + p.layer.layerDepth() + ', lock ' + JSON.stringify(p.lock()));
        }
        {
            const p = page(drill);
            p.open();
            drill.close(p.m);
            await p.r.advance(0);
            check(N + 'its own close (✕) closes its entry', !p.isOpen() && p.layer.layerDepth() === 0 && p.lock() === '');
            p.open();
            p.open();
            await p.r.advance(0);
            check(N + 'opened twice: one drill, one entry', p.d.querySelectorAll('.drill-modal').length === 1 && p.layer.layerDepth() === 1,
                p.d.querySelectorAll('.drill-modal').length + ' drills, depth ' + p.layer.layerDepth());
        }
    }
    console.log((failed ? 'FAILED: ' + failed + ' of ' : 'PASS: ') + (passed + failed) + ' drill cases on the layer');
    process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e && e.stack || e); process.exit(1); });
