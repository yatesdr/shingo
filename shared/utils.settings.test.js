// Unit tests for the settings-page helpers in /static/shared/utils.js:
// durationFromText / durationToText, collectList, apiResult and settingsPage.
// Run under plain Node via the Go wrapper settings_page_test.go. Exit 0 on
// pass, 1 on any assertion failure.
//
// Self-contained: a DOM stub with just what the helpers touch (classes,
// data-* attributes, children, insertBefore, events), and a fake send so a
// save is one recorded call rather than a network request.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let passed = 0;
let failed = 0;
function assert(cond, label) {
    if (cond) { passed++; }
    else { failed++; console.error('FAIL: ' + label); }
}
function eq(got, want, label) {
    assert(got === want, label + ': got ' + JSON.stringify(got) + ', want ' + JSON.stringify(want));
}

// ─── DOM stub ─────────────────────────────────────────────────────────────
class Node {
    constructor(tag, text) {
        this.tagName = (tag || '#text').toUpperCase();
        this.children = [];
        this.parentNode = null;
        this.attrs = {};
        this.dataset = {};
        this.classes = new Set();
        this.hidden = false;
        this.disabled = false;
        this.style = {};
        this._text = text === undefined ? '' : String(text);
        this._listeners = {};
        const self = this;
        this.classList = {
            add: (c) => self.classes.add(c),
            remove: (c) => self.classes.delete(c),
            contains: (c) => self.classes.has(c),
            toggle: (c, on) => {
                const want = on === undefined ? !self.classes.has(c) : !!on;
                if (want) self.classes.add(c); else self.classes.delete(c);
                return want;
            },
        };
    }
    set className(v) { this.classes = new Set(String(v).split(/\s+/).filter(Boolean)); }
    get className() { return Array.from(this.classes).join(' '); }
    get textContent() {
        if (this.tagName === '#TEXT') return this._text;
        return this.children.map((c) => c.textContent).join('');
    }
    set textContent(v) { this.children = []; this.appendChild(new Node('#text', v)); }
    setAttribute(k, v) { this.attrs[k] = String(v); }
    getAttribute(k) {
        if (k.indexOf('data-') === 0) {
            const key = k.slice(5).replace(/-([a-z])/g, (_, c) => c.toUpperCase());
            return Object.prototype.hasOwnProperty.call(this.dataset, key) ? this.dataset[key] : null;
        }
        return Object.prototype.hasOwnProperty.call(this.attrs, k) ? this.attrs[k] : null;
    }
    appendChild(c) {
        if (c.parentNode) c.parentNode.removeChild(c);
        c.parentNode = this; this.children.push(c); return c;
    }
    insertBefore(c, ref) {
        if (c.parentNode) c.parentNode.removeChild(c);
        c.parentNode = this;
        const i = ref ? this.children.indexOf(ref) : -1;
        if (i < 0) this.children.push(c); else this.children.splice(i, 0, c);
        return c;
    }
    removeChild(c) {
        const i = this.children.indexOf(c);
        if (i >= 0) this.children.splice(i, 1);
        c.parentNode = null;
        return c;
    }
    remove() { if (this.parentNode) this.parentNode.removeChild(this); }
    get nextSibling() {
        if (!this.parentNode) return null;
        const sibs = this.parentNode.children;
        return sibs[sibs.indexOf(this) + 1] || null;
    }
    addEventListener(ev, fn) { (this._listeners[ev] = this._listeners[ev] || []).push(fn); }
    dispatch(ev, target) {
        let cur = target || this;
        const e = { type: ev, target: target || this };
        while (cur) {
            (cur._listeners[ev] || []).forEach((fn) => fn(e));
            cur = cur.parentNode;
        }
    }
    matches(sel) {
        let m = /^\.([\w-]+)$/.exec(sel);
        if (m) return this.classes.has(m[1]);
        m = /^\[([\w-]+)(?:="((?:[^"\\]|\\.)*)")?\]$/.exec(sel);
        if (m) {
            const v = this.getAttribute(m[1]);
            if (m[2] === undefined) return v !== null;
            return v === m[2].replace(/\\(.)/g, '$1');
        }
        throw new Error('stub selector not supported: ' + sel);
    }
    querySelectorAll(sel) {
        const out = [];
        const walk = (n) => {
            for (const c of n.children) {
                if (c.tagName !== '#TEXT' && c.matches(sel)) out.push(c);
                walk(c);
            }
        };
        walk(this);
        return out;
    }
    querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
    closest(sel) {
        let cur = this;
        while (cur) { if (cur.tagName !== '#TEXT' && cur.matches(sel)) return cur; cur = cur.parentNode; }
        return null;
    }
}

function mk(tag, cls, data, children) {
    const n = new Node(tag);
    if (cls) n.className = cls;
    Object.assign(n.dataset, data || {});
    (children || []).forEach((c) => n.appendChild(c));
    return n;
}

function loadUtils(fetchImpl) {
    const src = fs.readFileSync(path.join(__dirname, 'utils.js'), 'utf8');
    const transformed = src
        .replace(/export\s+function\s+/g, 'function ')
        .replace(/export\s+const\s+/g, 'const ');
    const body = mk('body');
    const toasts = [];
    const ctx = vm.createContext({
        Node,
        document: {
            body,
            createElement: (t) => new Node(t),
            createTextNode: (t) => new Node('#text', t),
            getElementById: () => null,
            addEventListener: () => {},
            querySelectorAll: () => [],
        },
        window: { addEventListener: () => {} },
        console, setTimeout, clearTimeout, Promise,
        fetch: fetchImpl || (() => Promise.reject(new Error('no fetch in this test'))),
        EventSource: function () {},
        location: { reload: () => {} },
    });
    vm.runInContext(transformed +
        '; this.durationFromText = durationFromText; this.durationToText = durationToText;' +
        ' this.collectList = collectList; this.settingsPage = settingsPage; this.apiResult = apiResult;' +
        ' this.toast = toast;', ctx);
    ctx.__toasts = toasts;
    return ctx;
}

const tick = () => new Promise((r) => setTimeout(r, 0));

(async () => {
    const u = loadUtils();

    // ─── durations, text → Go ─────────────────────────────────────────────
    for (const [text, want] of [
        ['45 min', '45m0s'], ['45min', '45m0s'], ['10 s', '10s'], ['1 h', '1h0m0s'],
        ['1 h 30 min', '1h30m0s'], ['90 s', '1m30s'], ['500 ms', '500ms'], ['1.5 h', '1h30m0s'],
        ['2 minutes', '2m0s'], ['3 hours', '3h0m0s'], ['0', '0s'], ['  10 S ', '10s'],
        // A Go string typed in is accepted, and comes back as Go writes it.
        ['45m0s', '45m0s'], ['1h30m', '1h30m0s'], ['10s', '10s'], ['1m30.5s', '1m30.5s'], ['250ms', '250ms'],
        ['1h0m0s', '1h0m0s'],
    ]) {
        eq(u.durationFromText(text), want, 'durationFromText(' + JSON.stringify(text) + ')');
    }
    for (const bad of ['abc', '45', '10 parsecs', '-5 min', '5 min x', 'min']) {
        eq(u.durationFromText(bad), null, 'durationFromText(' + JSON.stringify(bad) + ') is not a duration');
    }
    eq(u.durationFromText(''), '', 'blank stays blank');
    eq(u.durationFromText('   '), '', 'whitespace is blank');

    // ─── durations, Go → text ─────────────────────────────────────────────
    for (const [goStr, want] of [
        ['45m0s', '45 min'], ['10s', '10 s'], ['1h0m0s', '1 h'], ['1h30m0s', '1 h 30 min'],
        ['1m30s', '1 min 30 s'], ['500ms', '500 ms'], ['0s', '0 s'], ['2h', '2 h'], ['24h0m0s', '24 h'],
    ]) {
        eq(u.durationToText(goStr), want, 'durationToText(' + JSON.stringify(goStr) + ')');
    }
    eq(u.durationToText('garbage'), 'garbage', 'an unparseable stored value is shown as stored');
    eq(u.durationToText(''), '', 'blank shows blank');
    // Round trip: what the box shows parses back to what was stored.
    for (const goStr of ['45m0s', '10s', '1h0m0s', '1h30m0s', '1m30s', '500ms', '0s']) {
        eq(u.durationFromText(u.durationToText(goStr)), goStr, 'round trip ' + goStr);
    }

    // ─── list collect after remove-first ──────────────────────────────────
    {
        const list = mk('div', null, null, [
            mk('div', 'row', { v: 'a:9093' }), mk('div', 'row', { v: 'b:9093' }), mk('div', 'row', { v: 'c:9093' }),
        ]);
        const read = (r) => r.dataset.v;
        eq(JSON.stringify(u.collectList(list, '.row', read)), '["a:9093","b:9093","c:9093"]', 'collect all rows');
        list.children[0].remove();
        eq(JSON.stringify(u.collectList(list, '.row', read)), '["b:9093","c:9093"]', 'remove-first keeps the rest');
        list.children[1].remove();
        eq(JSON.stringify(u.collectList(list, '.row', read)), '["b:9093"]', 'remove-last keeps the first');
        list.appendChild(mk('div', 'row', { v: '' }));
        eq(JSON.stringify(u.collectList(list, '.row', read)), '["b:9093"]', 'a blank new row is left out');
        eq(JSON.stringify(u.collectList(null, '.row', read)), '[]', 'no container, no rows');
    }

    // ─── apiResult never throws ───────────────────────────────────────────
    {
        const answer = (status, text) => () => Promise.resolve({
            ok: status >= 200 && status < 300, status, text: () => Promise.resolve(text),
        });
        let r = await loadUtils(answer(400, '{"ok":false,"error":"bad","errors":{"port":"not a number"}}')).apiResult('PUT', '/x', {});
        assert(r.ok === false && r.status === 400 && r.body.errors.port === 'not a number', 'a 400 arrives with its errors: ' + JSON.stringify(r));
        r = await loadUtils(answer(200, '{"ok":true,"restart":[]}')).apiResult('PUT', '/x', {});
        assert(r.ok === true && r.status === 200 && r.body.ok === true, 'a 200 arrives parsed: ' + JSON.stringify(r));
        r = await loadUtils(answer(500, 'boom')).apiResult('GET', '/x');
        assert(r.ok === false && r.body.error === 'boom', 'a non-JSON failure is {error: text}: ' + JSON.stringify(r));
        r = await loadUtils(answer(204, '')).apiResult('DELETE', '/x');
        assert(r.ok === true && r.body === null, 'an empty body is null: ' + JSON.stringify(r));
        r = await loadUtils(() => Promise.reject(new Error('offline'))).apiResult('GET', '/x');
        assert(r.ok === false && r.status === 0 && /offline/.test(r.body.error), 'no answer is status 0: ' + JSON.stringify(r));
    }

    // ─── settingsPage: dirty tracking, one request, errors, discard ───────
    {
        // The page's draft lives in two plain objects standing in for its
        // inputs; read/write go through them as a page's would go through the DOM.
        const fleet = { host: 'rds', port: '8088' };
        const plant = { tz: 'UTC' };
        const root = mk('div', 'set-page set-ui');
        const hostInput = mk('input', 'set-inp', { field: 'host' });
        const portInput = mk('input', 'set-inp', { field: 'port' });
        const hostRow = mk('div', 'set-fld', null, [mk('label'), mk('div', 'v', null, [hostInput])]);
        const portRow = mk('div', 'set-fld', null, [mk('label'), mk('div', 'v', null, [portInput])]);
        root.appendChild(hostRow);
        root.appendChild(portRow);

        const calls = [];
        let answer = { ok: true, status: 200, body: { ok: true, status: 'ok', applied: ['fleet'], restart: [], failed: [] } };
        const page = u.settingsPage(root, {
            url: '/api/config',
            send: (method, url, body) => { calls.push({ method, url, body: JSON.parse(JSON.stringify(body)) }); return Promise.resolve(answer); },
            sections: {
                fleet: {
                    read: () => ({ host: fleet.host, port: fleet.port }),
                    write: (v) => { fleet.host = v.host; fleet.port = v.port; },
                    validate: (d) => (/^\d+$/.test(d.port) ? null : { port: 'Port must be a number.' }),
                    body: (d) => ({ host: d.host, port: Number(d.port) }),
                },
                plant: {
                    read: () => ({ tz: plant.tz }),
                    write: (v) => { plant.tz = v.tz; },
                },
            },
        });
        const bar = root.querySelector('.set-savebar');
        const prov = bar.querySelector('.prov');
        const saveBtn = bar.querySelector('[data-set="save"]');
        const discardBtn = bar.querySelector('[data-set="discard"]');
        assert(!!bar && !!saveBtn && !!discardBtn, 'the save bar is built when the page drew none');
        eq(prov.textContent, 'No unsaved changes', 'clean page');
        eq(saveBtn.disabled, true, 'Save is disabled until dirty');
        eq(discardBtn.disabled, true, 'Discard is disabled until dirty');
        eq(page.isDirty(), false, 'not dirty at load');

        fleet.host = 'rds2';
        root.dispatch('input', hostInput);
        await tick();
        eq(prov.textContent, 'Unsaved changes', 'an input makes the page dirty');
        assert(prov.classList.contains('dirty'), 'the dirty words take the dirty class');
        eq(saveBtn.disabled, false, 'Save is enabled when dirty');
        eq(JSON.stringify(page.dirtySections()), '["fleet"]', 'only the touched section is dirty');

        fleet.host = 'rds';
        page.update();
        eq(page.isDirty(), false, 'typing the old value back is clean again');

        // A client-side refusal: nothing is sent, the error sits under its row.
        fleet.port = 'abc';
        page.update();
        await page.save();
        eq(calls.length, 0, 'a client validation failure sends nothing');
        let err = root.querySelector('[data-err-for="port"]');
        assert(!!err && err.textContent === 'Port must be a number.', 'the field error is drawn');
        assert(err && err.parentNode === root && portRow.nextSibling === err, 'the error line sits right under its row');

        // A server refusal: one request, the server's errors under the fields.
        fleet.port = '9090';
        plant.tz = 'America/Chicago';
        page.update();
        answer = { ok: false, status: 400, body: { ok: false, error: 'Unknown time zone', errors: { tz: 'Unknown time zone', host: 'Host is unreachable' } } };
        await page.save();
        eq(calls.length, 1, 'one save is one request');
        eq(calls[0].method, 'PUT', 'the save is a PUT');
        eq(calls[0].url, '/api/config', 'to the URL the page passed');
        eq(JSON.stringify(calls[0].body), '{"fleet":{"host":"rds","port":9090},"plant":{"tz":"America/Chicago"}}',
            'the body is {section: wire} for the dirty sections, through body()');
        eq(root.querySelector('[data-err-for="port"]'), null, 'the old client error is cleared');
        err = root.querySelector('[data-err-for="host"]');
        assert(!!err && hostRow.nextSibling === err, 'a server field error is drawn under its row');
        const refusal = root.querySelector('.set-refusal');
        assert(refusal && !refusal.hidden && /Unknown time zone/.test(refusal.textContent),
            'an error with no field on the page goes in the refusal box: ' + (refusal && refusal.textContent));
        eq(page.isDirty(), true, 'a refused save stays dirty');

        // Only one section dirty: only it is sent.
        plant.tz = 'UTC';
        page.update();
        answer = { ok: true, status: 200, body: { ok: true, status: 'ok', applied: ['fleet'], restart: ['Fleet address'], failed: [] } };
        await page.save();
        eq(calls.length, 2, 'second save, second request');
        eq(JSON.stringify(Object.keys(calls[1].body)), '["fleet"]', 'a clean section is not sent');
        eq(page.isDirty(), false, 'a saved page is clean');
        eq(prov.textContent, 'No unsaved changes', 'and says so');
        eq(saveBtn.disabled, true, 'Save is disabled again');
        eq(root.querySelector('.set-err'), null, 'a good save clears the field errors');
        const notice = root.querySelector('.set-notice');
        assert(notice && !notice.hidden && /Fleet address/.test(notice.textContent), 'the restart notice names the field');
        assert(notice.nextSibling === refusal && refusal.nextSibling === bar, 'notice and refusal sit directly above the bar');
        eq(refusal.hidden, true, 'no refusal after a clean save');

        // failed: saved, but a reconfigure did not take.
        fleet.host = 'rds3';
        page.update();
        answer = { ok: true, status: 200, body: { ok: true, applied: [], restart: [], failed: ['fleet'] } };
        await page.save();
        assert(!refusal.hidden && /not applied: fleet/.test(refusal.textContent), 'failed is shown: ' + refusal.textContent);
        eq(page.isDirty(), false, 'a save with a failed apply is still saved');
        eq(notice.hidden, true, 'an empty restart list clears the notice');

        // Discard writes the last saved values back.
        fleet.host = 'typo';
        plant.tz = 'Mars/Base';
        page.update();
        eq(JSON.stringify(page.dirtySections()), '["fleet","plant"]', 'two sections dirty');
        discardBtn.dispatch('click');
        eq(fleet.host, 'rds3', 'discard restores the saved host');
        eq(plant.tz, 'UTC', 'discard restores the saved zone');
        eq(page.isDirty(), false, 'discarded page is clean');
    }

    // ─── a section with its own save (Edge Shifts: PUT /api/shifts) ───────
    {
        const cfg = { tz: 'UTC' };
        const shifts = { rows: ['06:00-14:00'] };
        const root = mk('div', 'set-page set-ui');
        const tzRow = mk('div', 'set-fld', null, [mk('input', 'set-inp', { field: 'tz' })]);
        const shRow = mk('div', 'set-fld', null, [mk('input', 'set-inp', { field: 'shift_1' })]);
        root.appendChild(tzRow);
        root.appendChild(shRow);

        const order = [];
        let sharedAnswer = { ok: true, status: 200, body: { ok: true, applied: ['station'], restart: [], failed: [] } };
        let ownAnswer = { ok: true };
        const page = u.settingsPage(root, {
            url: '/api/config',
            send: (method, url, body) => { order.push({ door: url, body: JSON.parse(JSON.stringify(body)) }); return Promise.resolve(sharedAnswer); },
            sections: {
                station: { read: () => ({ tz: cfg.tz }), write: (v) => { cfg.tz = v.tz; } },
                shifts: {
                    label: 'shifts',
                    read: () => ({ rows: shifts.rows.slice() }),
                    write: (v) => { shifts.rows = v.rows.slice(); },
                    save: (d) => { order.push({ door: 'shifts', body: d }); return Promise.resolve(ownAnswer); },
                },
            },
        });
        const refusal = root.querySelector('.set-refusal');

        // Only the own-save section dirty: no shared request at all.
        shifts.rows = ['06:00-14:00', '14:00-22:00'];
        page.update();
        let out = await page.save();
        eq(order.length, 1, 'own-save only: one call');
        eq(order[0].door, 'shifts', 'and it is the section\x27s own save, not the shared door');
        eq(out.ok, true, 'own-save only: ok');
        eq(page.isDirty(), false, 'a saved own-save section is clean');

        // Both dirty: the shared request first, without the own-save section, then the own save.
        order.length = 0;
        cfg.tz = 'America/Chicago';
        shifts.rows = ['07:00-15:00'];
        page.update();
        out = await page.save();
        eq(order.map((o) => o.door).join(','), '/api/config,shifts', 'shared request first, then the own save');
        eq(JSON.stringify(Object.keys(order[0].body)), '["station"]', 'an own-save section is left out of the shared request');
        eq(JSON.stringify(order[1].body), '{"rows":["07:00-15:00"]}', 'the own save gets its draft');
        eq(page.isDirty(), false, 'both saved, page clean');
        eq(refusal.hidden, true, 'no refusal when both saved');

        // Partial: shared saves, own save fails with a field error and a general one.
        order.length = 0;
        cfg.tz = 'UTC';
        shifts.rows = ['25:00-26:00'];
        page.update();
        ownAnswer = { ok: false, error: 'shift hours are out of range', errors: { shift_1: 'Start must be 00:00-23:59.' } };
        out = await page.save();
        eq(out.ok, false, 'a partial outcome is not ok');
        eq(order.length, 2, 'both parts were sent');
        eq(JSON.stringify(page.dirtySections()), '["shifts"]', 'only the section that saved is clean');
        const err = root.querySelector('[data-err-for="shift_1"]');
        assert(!!err && shRow.nextSibling === err, 'the own save\x27s field error is drawn under its row');
        assert(!refusal.hidden && refusal.textContent === 'Settings saved; shifts failed: see the marked fields.',
            'the partial outcome is said plainly: ' + JSON.stringify(refusal.textContent));

        // The other way round, with an error that has no field on the page.
        order.length = 0;
        cfg.tz = 'Mars/Base';
        page.update();
        sharedAnswer = { ok: false, status: 400, body: { ok: false, error: 'Unknown time zone', errors: { timezone: 'Unknown time zone' } } };
        ownAnswer = { ok: true };
        out = await page.save();
        eq(JSON.stringify(page.dirtySections()), '["station"]', 'the refused shared section stays dirty, the saved own section is clean');
        assert(refusal.textContent === 'Settings failed: Unknown time zone; shifts saved.',
            'shared failure, own success: ' + JSON.stringify(refusal.textContent));
        eq(root.querySelector('[data-err-for="shift_1"]'), null, 'the previous field errors are cleared');

        // An own save that throws is a failure, not an exception out of save().
        order.length = 0;
        sharedAnswer = { ok: true, status: 200, body: { ok: true, restart: [], failed: [] } };
        cfg.tz = 'UTC';
        shifts.rows = ['08:00-16:00'];
        page.update();
        const throwing = u.settingsPage(mk('div', 'set-ui'), {
            url: '/x', send: () => Promise.resolve(sharedAnswer),
            sections: { s: { label: 'shifts', read: () => ({ v: shifts.rows[0] }), save: () => Promise.reject(new Error('offline')) } },
        });
        shifts.rows = ['09:00-17:00'];
        throwing.update();
        out = await throwing.save();
        eq(out.ok, false, 'a thrown own save is a failure');
        eq(throwing.isDirty(), true, 'and stays dirty');
        eq(throwing.state.refusal, 'Not saved. Offline', 'one part only: the plain refusal');
    }

    // ─── a page that drew its own bar keeps it ────────────────────────────
    {
        const own = mk('div', 'set-savebar', null, [
            mk('span', 'prov'), mk('button', 'set-btn quiet', { set: 'discard' }), mk('button', 'set-btn primary', { set: 'save' }),
        ]);
        const root = mk('div', 'set-page set-ui', null, [own]);
        u.settingsPage(root, { url: '/x', restart: ['Station UID'], sections: { a: { read: () => ({}) } } });
        eq(root.querySelectorAll('.set-savebar').length, 1, 'no second bar');
        const notice = root.querySelector('.set-notice');
        assert(notice && !notice.hidden && /Station UID/.test(notice.textContent), 'the notice at render shows the server list');
    }

    if (failed > 0) {
        console.error('FAILED: ' + failed + ' of ' + (passed + failed));
        process.exit(1);
    }
    console.log('PASS: ' + passed + ' assertions for settings helpers in shared/utils.js');
})().catch((e) => { console.error(e && e.stack || e); process.exit(1); });
