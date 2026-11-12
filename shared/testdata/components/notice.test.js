// notice.test.js — the spec of shared/notice.js. Run by
// shared/components_test.go (TestSharedComponentSpecs).

'use strict';

const { test, run, page, eq } = require('./spec.js');

function notice() {
    const p = page('notice.js');
    return { r: p.r, N: p.mods[0], stack: () => p.r.doc.querySelector('.notice-stack'), banners: () => p.r.doc.querySelector('.notice-banners') };
}

// gone says when (ms after it was shown) t left the page, stepping 100 ms.
async function gone(r, t, maxMs) {
    for (let ms = 100; ms <= maxMs; ms += 100) {
        await r.advance(100);
        if (!t.isConnected) return ms;
    }
    return null;
}

test('nothing happens at import: no element, no listener, no timer', async () => {
    const f = notice();
    eq([f.r.doc.body.children.length, f.r.doc.listenerTypes(), f.r.clock.timers.length], [0, [], 0]);
});

test('lifetimes are the goldens\': an error 5 s, every other level 3.2 s', async () => {
    const out = {};
    for (const lvl of ['info', 'success', 'warning', 'error', undefined, 'bogus']) {
        const f = notice();
        out[String(lvl)] = await gone(f.r, f.N.toast('Msg', lvl), 8000);
    }
    eq(out, { info: 3200, success: 3200, warning: 3200, error: 5000, undefined: 3200, bogus: 3200 });
});

test('cannot happen: an error toast with {sticky:false} lives 3.2 s, not 5 s (shared/utils.js:753)', async () => {
    const f = notice();
    eq(await gone(f.r, f.N.toast('Msg', 'error', { sticky: false }), 8000), 5000);
});

test('{sticky:true} stays until it is clicked, and arms no timer', async () => {
    const f = notice();
    const t = f.N.toast('Msg', 'error', { sticky: true });
    eq(f.r.clock.timers.length, 0, 'timers');
    await f.r.advance(60000);
    eq(t.isConnected, true, 'after 60 s');
    t.click();
    eq(t.isConnected, false, 'after a click');
});

test('a toast that goes takes its timer with it', async () => {
    const f = notice();
    f.N.toast('a');
    await f.r.advance(3200);
    eq(f.r.clock.timers.length, 0);
});

test('the same words at the same level are one toast, its lifetime restarted', async () => {
    const f = notice();
    f.N.toast('Could not reach this edge', 'error');
    await f.r.advance(4000);
    const t = f.N.toast('Could not reach this edge', 'error');
    f.N.toast('Could not reach this edge', 'warning');
    eq([f.stack().children.length, f.r.clock.timers.length], [2, 2], 'one per level');
    eq(await gone(f.r, t, 8000), 5000, 'the refreshed one lives its full 5 s');
});

// A lapsed session on a Core page: the door puts its sticky "Signed out" with
// the Sign-in link on the notice, and the page then toasts the same words for
// the hub api's rejection. The plain toast used to replace the sticky one, and
// the link went with it (lead ruling: the sticky one stays).
test('a toast with the words and level of a sticky one already up is not added; the sticky one stays, link and all', async () => {
    const f = notice();
    const s = f.N.toast('Signed out', 'error', { sticky: true, action: { label: 'Sign in', href: '/login?next=%2Fnodes' } });
    const t = f.N.toast('Signed out', 'error');
    const again = f.N.toast('Signed out', 'error', { sticky: true, action: { label: 'Sign in', href: '/login?next=%2Fnodes' } });
    eq([f.stack().children.length, s.isConnected, t === s, again === s, f.r.clock.timers.length], [1, true, true, true, 0]);
    await f.r.advance(60000);
    eq([s.isConnected, !!s.querySelector('a.notice-action')], [true, true], 'after 60 s');
});

test('different words beside a sticky one are their own toast', async () => {
    const f = notice();
    f.N.toast('Signed out', 'error', { sticky: true });
    f.N.toast('Save failed: Signed out', 'error');
    f.N.toast('Signed out', 'warning');
    eq(f.stack().children.map((t) => t.textContent), ['Signed out', 'Save failed: Signed out', 'Signed out']);
});

test('toasts stack in the order shown, under one region that is one child of body', async () => {
    const f = notice();
    f.N.toast('one');
    f.N.toast('two', 'error');
    f.N.toast('three', 'success');
    eq([f.stack().children.map((t) => t.textContent), f.r.doc.body.children.length], [['one', 'two', 'three'], 1]);
});

test('a message is text, never markup; an Error is its message; a refusal body is its error', async () => {
    const f = notice();
    const t = f.N.toast('<img src=x onerror=alert(1)>', 'error');
    eq([t.querySelector('.notice-text').childNodes.length, t.textContent], [1, '<img src=x onerror=alert(1)>']);
    eq([f.N.toast(new f.r.TypeError('boom')).textContent, f.N.toast({ error: 'held' }).textContent, f.N.toast(null).textContent], ['boom', 'held', '']);
});

test('an error toast is an alert; the others are read through the polite region', async () => {
    const f = notice();
    const e = f.N.toast('x', 'error');
    const i = f.N.toast('y');
    eq([e.getAttribute('role'), i.getAttribute('role'), f.stack().getAttribute('aria-live')], ['alert', null, 'polite']);
});

test('a toast carries one action: a link or a button', async () => {
    const f = notice();
    const a = f.N.toast('Signed out', 'error', { sticky: true, action: { label: 'Sign in', href: '/login?next=%2Fprocesses' } });
    let pressed = 0;
    const b = f.N.toast('Held', 'warning', { action: { label: 'Release', onClick: () => { pressed++; } } });
    b.querySelector('.notice-action').click();
    eq([a.querySelector('a').getAttribute('href'), a.querySelector('a').textContent, pressed], ['/login?next=%2Fprocesses', 'Sign in', 1]);
});

// ── the banner: what S5 puts on it ──────────────────────────────────────────

test('the banner shows both of S5\'s lines at once, each under its own key', async () => {
    const f = notice();
    let reloads = 0;
    f.N.banner('build', { text: 'A new version is available', action: { label: 'Refresh', onClick: () => { reloads++; } } });
    f.N.banner('connection', { text: 'Lost connection to Core · last answered 14:02', level: 'warning', dismiss: false });
    const rows = f.banners().children;
    eq(rows.map((b) => b.getAttribute('data-banner') + ': ' + b.querySelector('.notice-text').textContent),
        ['build: A new version is available', 'connection: Lost connection to Core · last answered 14:02']);
    rows[0].querySelector('.notice-action').click();
    eq([reloads, !!rows[1].querySelector('.notice-dismiss'), rows[1].className], [1, false, 'notice-banner notice-banner--warning']);
});

test('a banner under the same key redraws in place; clearBanner takes one down and leaves the other', async () => {
    const f = notice();
    const a = f.N.banner('connection', { text: 'Lost connection to this Edge · last answered 14:02', dismiss: false });
    const b = f.N.banner('connection', { text: 'Lost connection to this Edge · last answered 14:05', dismiss: false });
    f.N.banner('build', { text: 'A new version is available' });
    eq([a === b, f.banners().children.length, b.textContent], [true, 2, 'Lost connection to this Edge · last answered 14:05']);
    f.N.clearBanner('connection');
    f.N.clearBanner('nothing');
    eq(f.banners().children.map((x) => x.getAttribute('data-banner')), ['build']);
});

test('a banner\'s × takes it down and tells the page; the next banner call puts it back', async () => {
    const f = notice();
    let dismissed = 0;
    f.N.banner('build', { text: 'A new version is available', onDismiss: () => { dismissed++; } });
    f.banners().querySelector('.notice-dismiss').click();
    eq([f.banners().children.length, dismissed], [0, 1]);
    f.N.banner('build', { text: 'A new version is available' });
    eq(f.banners().children.length, 1);
});

test('banners sit above the toasts in one region: a toast never covers a banner', async () => {
    const f = notice();
    f.N.toast('Saved', 'success');
    f.N.banner('build', { text: 'A new version is available' });
    const root = f.r.doc.querySelector('.notice-root');
    eq(root.children.map((c) => c.className), ['notice-banners', 'notice-stack']);
});

run();
