// http.test.js — the spec of shared/http.js. Run by shared/components_test.go
// (TestSharedComponentSpecs). The answers are world.js's fetch stub; the two
// it cannot give (an answer that never comes, a body that stalls) are made
// here from the request's own signal.

'use strict';

const { test, run, page, eq, ok } = require('./spec.js');

const JSONCT = { 'content-type': 'application/json' };
// The Edge's lapsed session: adminMiddleware's 303 to /login, followed by
// fetch to the login page's 200.
const LOGIN_PAGE = {
    status: 200, body: '<!doctype html><html><body><form action="/login"></form></body></html>',
    headers: { 'content-type': 'text/html; charset=utf-8' }, redirected: true, url: 'http://surface.test/login',
};

function door(answer) {
    const p = page('http.js');
    if (answer) p.r.answer = typeof answer === 'function' ? answer : () => answer;
    return { r: p.r, H: p.mods[0], toasts: () => p.r.doc.querySelectorAll('.notice-toast') };
}

// hang makes fetch an answer that never comes: it rejects only when its
// signal aborts, as the browser's does. stall answers the headers and never
// the body.
function hang(r, stallBody) {
    r.ctx.fetch = (url, init) => {
        r.requests.push({ url: String(url), init });
        const abort = (reject) => {
            const fail = () => {
                const e = new r.TypeError('The user aborted a request.');
                e.name = 'AbortError';
                reject(e);
            };
            if (init.signal.aborted) fail(); else init.signal.addEventListener('abort', fail);
        };
        if (!stallBody) return new Promise((_, reject) => abort(reject));
        return Promise.resolve({ ok: true, status: 200, headers: { get: () => null }, text: () => new Promise((_, reject) => abort(reject)) });
    };
}

test('nothing happens at import: no listener, no timer, no element', async () => {
    const f = door();
    eq([f.r.doc.listenerTypes(), f.r.clock.timers.length, f.r.doc.body.children.length], [[], 0, 0]);
});

test('the request: JSON is asked for; a body is JSON with its type; a write given none sends {}, null sends none', async () => {
    const f = door({ status: 200, body: '{}', headers: JSONCT });
    await f.H.http.get('/a');
    await f.H.http.post('/b', { a: 1 });
    await f.H.http.post('/c');
    await f.H.send('POST', '/d', null);
    eq(f.r.requests.map((q) => q.init.method + ' ' + q.url + ' ' + JSON.stringify(q.init.headers) + ' ' + (q.init.body === undefined ? '-' : q.init.body)), [
        'GET /a {"Accept":"application/json"} -',
        'POST /b {"Accept":"application/json","Content-Type":"application/json"} {"a":1}',
        'POST /c {"Accept":"application/json","Content-Type":"application/json"} {}',
        'POST /d {"Accept":"application/json"} -',
    ]);
});

test('cannot happen: a 204 or an empty 2xx rejects (app.js:253, shingoedge.js:349)', async () => {
    for (const status of [204, 200, 201]) {
        const f = door({ status, body: '', headers: JSONCT });
        eq(await f.H.http.post('/x', { a: 1 }), { ok: true, status, body: null, error: '', fieldErrors: {} }, String(status));
        eq(f.toasts().length, 0);
    }
});

test('cannot happen: a 4xx whose body is JSON null throws a TypeError (app.js:249)', async () => {
    const f = door({ status: 400, body: 'null\n', headers: JSONCT });
    eq(await f.H.http.get('/x'), { ok: false, status: 400, body: null, error: 'Refused (400)', fieldErrors: {} });
});

test('a refusal reads as the server wrote it: Edge {"error"}, Core configRefusal with its fields, kanbanHeld, http.Error text', async () => {
    const cases = [
        [400, '{"error":"process not found"}\n', 'process not found', {}],
        [400, '{"error":"Port must be a number.","errors":{"port":"Port must be a number."},"ok":false}\n', 'Port must be a number.', { port: 'Port must be a number.' }],
        [409, '{"drain":[],"error":"position holds bins","needs_force":true}\n', 'position holds bins', {}],
        [400, 'invalid bin type\n', 'invalid bin type', {}],
        [422, '{"message":"no orders"}', 'no orders', {}],
    ];
    for (const [status, body, error, fieldErrors] of cases) {
        const f = door({ status, body, headers: JSONCT });
        const out = await f.H.http.post('/x', {});
        eq([out.ok, out.status, out.error, out.fieldErrors], [false, status, error, fieldErrors], body);
    }
});

test('a field_errors list is read as fields too; a warning in it is not an error', async () => {
    const f = door({ status: 400, body: JSON.stringify({ error: 'bad claim', field_errors: [{ field: 'node', message: 'unknown node', severity: 'error' }, { field: 'role', message: 'unusual', severity: 'warning' }] }), headers: JSONCT });
    eq((await f.H.http.post('/x', {})).fieldErrors, { node: 'unknown node' });
});

test('a failure with nothing to read still has a sentence: empty, null, an empty error, a proxy\'s HTML', async () => {
    const out = [];
    for (const [status, body] of [[400, ''], [500, ''], [400, '{"error":""}'], [400, '{"error":{"code":7}}'], [502, '<html><body>Bad Gateway</body></html>']]) {
        const f = door({ status, body, headers: JSONCT });
        out.push((await f.H.http.get('/x')).error);
    }
    eq(out, ['Refused (400)', 'Server error (500)', 'Refused (400)', 'Refused (400)', 'Server error (502)']);
});

test('the door\'s half of "Preview OK · null orders" (processes-desktop.js:2400): a bodiless 500 is not ok', async () => {
    const f = door({ status: 500, body: '' });
    const out = await f.H.http.post('/api/flows/preview', {});
    eq([out.ok, out.body, out.error], [false, null, 'Server error (500)']);
});

test('cannot happen: a dead Edge leaves a write as an unhandled rejection with nothing on screen (processes-desktop.js:2415, :6049; F22)', async () => {
    const f = door({ reject: 'Failed to fetch' });
    const out = await f.H.http.post('/api/processes', { name: 'P' });
    eq([out.ok, out.status, out.body, out.error], [false, 0, null, 'No answer from the server']);
    eq(f.toasts().map((t) => t.textContent), ['No answer from the server']);
});

test('a failure is shown once, by the door; {notice:false} leaves it to the caller', async () => {
    const f = door({ status: 409, body: '{"error":"lane held"}', headers: JSONCT });
    await f.H.http.post('/x', {});
    await f.H.http.post('/x', {});
    const quiet = await f.H.http.post('/x', {}, { notice: false });
    eq([f.toasts().length, quiet.error], [1, 'lane held']);
    const g = door({ status: 200, body: '{}', headers: JSONCT });
    await g.H.http.get('/x');
    eq(g.toasts().length, 0, 'a success says nothing');
});

// ── a lapsed session (ruling 3) ─────────────────────────────────────────────

test('cannot happen: a lapsed session\'s login page reads as done (processes-desktop.js:6056)', async () => {
    const f = door(LOGIN_PAGE);
    f.r.window.location.pathname = '/processes';
    f.r.window.location.search = '?process=3';
    const out = await f.H.http.post('/api/processes', { name: 'P' });
    eq(out, { ok: false, status: 401, body: null, error: 'Signed out', fieldErrors: {} });
    const t = f.toasts();
    eq([t.length, t[0].querySelector('.notice-text').textContent, t[0].querySelector('a').getAttribute('href'), t[0].className],
        [1, 'Signed out', '/login?next=%2Fprocesses%3Fprocess%3D3', 'notice-toast notice-toast--error is-sticky']);
});

test('cannot happen: after a lapsed session the next step of a chain writes to /api/processes/NaN/… (processes-desktop.js:6785)', async () => {
    const f = door(LOGIN_PAGE);
    // The Add-process chain as a page writes it on the door: each step needs
    // the one before to have landed.
    const made = await f.H.http.post('/api/processes', { name: 'P' });
    if (made.ok) await f.H.http.post('/api/processes/' + Number(made.body && made.body.id) + '/settings', {});
    eq(f.r.requests.map((q) => q.url), ['/api/processes']);
});

test('HTML where JSON was asked is a lapsed session, redirect or not; so is Core\'s own 401', async () => {
    const a = door({ status: 200, body: '<!DOCTYPE html><html></html>', headers: { 'content-type': 'text/html' } });
    const b = door({ status: 401, body: '{"error":"unauthorized"}\n', headers: JSONCT });
    eq([(await a.H.http.get('/x')).error, (await b.H.http.get('/x')).status, (await b.H.http.get('/x')).error], ['Signed out', 401, 'Signed out']);
});

test('the door\'s half of the presets read (processes-desktop.js:7138): a login page is "Signed out", not "could not reach"', async () => {
    const f = door(LOGIN_PAGE);
    eq((await f.H.http.get('/api/processes/3/presets')).error, 'Signed out');
});

test('a redirect somewhere other than the login path is followed as the answer it landed on', async () => {
    const f = door({ status: 200, body: '{"id":4}', headers: JSONCT, redirected: true, url: 'http://surface.test/api/processes/4' });
    eq(await f.H.http.get('/api/processes/latest'), { ok: true, status: 200, body: { id: 4 }, error: '', fieldErrors: {} });
});

// ── the deadline ────────────────────────────────────────────────────────────

test('the deadline is the station\'s 30 s, per request: no answer by then is status 0 and the notice', async () => {
    const f = door();
    hang(f.r);
    eq(f.H.REQUEST_TIMEOUT_MS, 30000);
    let out = null;
    f.H.http.post('/x', {}).then((o) => { out = o; });
    await f.r.advance(29900);
    eq(out, null, 'at 29.9 s');
    await f.r.advance(100);
    eq([out.ok, out.status, out.error, f.toasts().length], [false, 0, 'No answer in 30 s', 1], 'at 30 s');
    ok(f.r.requests[0].init.signal.aborted, 'the request itself was aborted');
});

test('a body that stalls after its headers is no answer too', async () => {
    const f = door();
    hang(f.r, true);
    let out = null;
    f.H.http.get('/x').then((o) => { out = o; });
    await f.r.advance(30000);
    eq([out.status, out.error], [0, 'No answer in 30 s']);
});

test('the deadline dies with its request: nothing is left on the clock after an answer or a failure', async () => {
    // {notice:false}: a failure's toast has a lifetime of its own.
    const a = door({ status: 200, body: '{}', headers: JSONCT });
    await a.H.http.get('/x');
    const b = door({ reject: 'Failed to fetch' });
    await b.H.http.get('/x', { notice: false });
    const c = door();
    hang(c.r);
    const p = c.H.http.get('/x', { notice: false });
    eq(c.r.clock.timers.length, 1, 'one timer while it is out');
    await c.r.advance(30000);
    await p;
    eq([a.r.clock.timers.length, b.r.clock.timers.length, c.r.clock.timers.length], [0, 0, 0]);
});

test('a call may set its own deadline, or none', async () => {
    const f = door();
    hang(f.r);
    let out = null;
    f.H.http.get('/x', { timeout: 5000 }).then((o) => { out = o; });
    await f.r.advance(5000);
    eq(out && out.error, 'No answer in 5 s');
    const g = door();
    hang(g.r);
    g.H.http.get('/x', { timeout: 0 });
    eq(g.r.clock.timers.length, 0, 'timeout 0 arms nothing');
});

test('a caller that aborts its own request gets {aborted:true}, status 0 and no notice', async () => {
    const f = door();
    hang(f.r);
    const ctrl = new AbortController();
    const p = f.H.http.post('/api/flows/preview', {}, { signal: ctrl.signal });
    ctrl.abort();
    const out = await p;
    eq([out.ok, out.status, out.aborted, out.error, f.toasts().length, f.r.clock.timers.length], [false, 0, true, 'Cancelled', 0, 0]);
    const g = door();
    hang(g.r);
    const done = new AbortController();
    done.abort();
    eq((await g.H.http.get('/x', { signal: done.signal })).aborted, true, 'a signal aborted before the call');
});

// ── htmx ────────────────────────────────────────────────────────────────────

function htmxEvent(type, xhr) {
    const { Event } = require('../helper-pins-harness/world.js');
    return new Event(type, { bubbles: true, detail: xhr ? { xhr } : {} });
}
function xhr(status, text, ct) {
    return { status, responseText: text, responseURL: '', getResponseHeader: (k) => (k === 'content-type' ? ct || 'application/json' : null) };
}

test('hookHtmxErrors is not installed by anything: importing the door adds no htmx listener', async () => {
    const f = door();
    eq(f.r.doc.body.listenerTypes(), []);
});

test('hookHtmxErrors puts a failed partial, no answer and htmx\'s timeout on the same notice, and comes off again', async () => {
    const f = door();
    const off = f.H.hookHtmxErrors();
    eq(f.r.doc.body.listenerTypes(), ['htmx:responseError', 'htmx:sendError', 'htmx:timeout']);
    const el = f.r.doc.createElement('div');
    f.r.doc.body.appendChild(el);
    el.dispatchEvent(htmxEvent('htmx:responseError', xhr(500, '{"error":"database not answering"}')));
    el.dispatchEvent(htmxEvent('htmx:responseError', xhr(404, '404 page not found\n', 'text/plain')));
    el.dispatchEvent(htmxEvent('htmx:sendError'));
    el.dispatchEvent(htmxEvent('htmx:timeout'));
    el.dispatchEvent(htmxEvent('htmx:responseError', xhr(401, '')));
    eq(f.toasts().map((t) => t.querySelector('.notice-text').textContent), ['database not answering', '404 page not found', 'No answer from the server', 'Signed out']);
    off();
    eq(f.r.doc.body.listenerTypes(), []);
});

run();
