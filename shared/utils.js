// Shared JavaScript utilities for Shingo admin surfaces (Core + Edge).
//
// ES module, served at /static/shared/utils.js. Consumers import the
// helpers they need. The operator HMI already uses ES modules; Core and
// Edge admin migrate via the UI consistency refactor.
//
// ONE JOB PER FILE. The helpers that have a job of their own live in a file
// beside this one, and a page imports each from that file's URL: nothing here
// re-exports them, so no page can reach one module by two URLs.
//   dom.js      escapeHtml, h``, el, delegateActions, switchTab, debounce
//   clock.js    serverNow, the plant-local formatters, formatDuration, live durations
//   list.js     installTableSort, reconcileList, createStore
//
// Design intent:
//   - Same vocabulary across surfaces; no per-surface reimplementations.
//   - HTTP: api.get/.post/.put/.del all throw the server error string
//     (or parsed { error } field) on non-2xx, returning parsed JSON on
//     success.
//   - Modals: .active class on .modal-overlay; closeOnBackdrop and
//     preserveState are opt-in. Default: button-only close + clear on
//     hide. Rationale and decision history live in
//     docs/ui-style-guide.md.
//
// Browser support requirement: Chromium 60+ / Firefox 60+ / Safari 11+
// (ES modules, async/await, classList, EventSource). Verify any plant
// kiosk older than that before shipping.

import { el, escapeHtml } from '/static/shared/dom.js';

// ─── HTTP ────────────────────────────────────────────────────────────────

// Internal: shared fetch + error parsing. Throws the server error string
// (or parsed { error } field) on non-2xx; returns parsed JSON on success.
async function request(method, url, body) {
    const opts = { method };
    if (body !== undefined && body !== null) {
        opts.headers = { 'Content-Type': 'application/json' };
        opts.body = JSON.stringify(body);
    }
    const r = await fetch(url, opts);
    if (!r.ok) {
        const text = await r.text();
        try {
            const obj = JSON.parse(text);
            if (obj && typeof obj === 'object' && obj.error) throw obj.error;
            throw text;
        } catch (e) {
            // JSON.parse failure: throw the raw text
            if (e instanceof SyntaxError) throw text;
            throw e;
        }
    }
    // Empty body (204, or empty 200) → return null
    const text = await r.text();
    return text ? JSON.parse(text) : null;
}

export const api = {
    get:  (url)       => request('GET',    url),
    post: (url, body) => request('POST',   url, body || {}),
    put:  (url, body) => request('PUT',    url, body || {}),
    del:  (url)       => request('DELETE', url),
};

// apiResult is api's non-throwing twin: it always resolves to
// { ok, status, body }, so a 400 carrying errors{field: msg} reaches the page
// whole. request() above throws only the error string and drops the rest of
// the body; it and its callers are unchanged.
//
//   ok     — the HTTP status was 2xx (a body's own ok: false is the caller's to read)
//   status — the HTTP status, or 0 when the request never got an answer
//   body   — the parsed JSON, or null for an empty body. A body that is not
//            JSON is { error: <text> } on a failure and the raw text on a 2xx.
export const apiResult = async function apiResult(method, url, body) {
    const opts = { method };
    if (body !== undefined && body !== null) {
        opts.headers = { 'Content-Type': 'application/json' };
        opts.body = JSON.stringify(body);
    }
    let r;
    try {
        r = await fetch(url, opts);
    } catch (e) {
        return { ok: false, status: 0, body: { error: 'No answer from the server: ' + (e && e.message ? e.message : String(e)) } };
    }
    let text = '';
    try { text = await r.text(); } catch (e) { text = ''; }
    let parsed = null;
    if (text) {
        try { parsed = JSON.parse(text); } catch (e) { parsed = r.ok ? text : { error: text }; }
    }
    return { ok: r.ok, status: r.status, body: parsed };
};

// ─── SSE factory ─────────────────────────────────────────────────────────

// Wraps EventSource with exponential backoff and build-id auto-reload.
// The server emits a `connected` event carrying its per-process build id;
// when a reconnect reports a different id, the tab hard-reloads so it
// picks up the new static bundle.
//
//   const sse = createSSE('/events', {
//       'order-update': (data) => { ... },
//       'system-status': (data) => { ... },
//   });
//   sse.close(); // on page teardown
export function createSSE(url, handlers) {
    let es;
    let reconnectDelay = 1000;
    let seenBuild = null;
    let closed = false;

    // checkBuild captures the first build id seen and shows a refresh
    // banner if any later event reports a different one. Used by both
    // the 'connected' (fires once per reconnect) and 'heartbeat' (fires
    // every 30s on the existing connection) handlers — the latter is
    // what catches Edge restarts that a reverse proxy held the SSE
    // socket open through, where onerror would otherwise never fire.
    //
    // Pre-fix this called location.reload() automatically, which
    // nuked operator mid-action state every deploy. The banner lets
    // the operator pick the moment.
    function checkBuild(e) {
        let build = '';
        try { build = (JSON.parse(e.data) || {}).build || ''; } catch (_) {}
        if (!build) return;
        if (seenBuild === null) {
            seenBuild = build;
        } else if (seenBuild !== build) {
            showRefreshBanner();
        }
    }

    function connect() {
        es = new EventSource(url);

        es.addEventListener('connected', e => {
            reconnectDelay = 1000;
            checkBuild(e);
        });

        es.addEventListener('heartbeat', checkBuild);

        for (const eventType in handlers) {
            if (typeof handlers[eventType] !== 'function') continue;
            const fn = handlers[eventType];
            es.addEventListener(eventType, e => {
                let parsed = null;
                try { parsed = JSON.parse(e.data); } catch (err) {
                    console.error('SSE parse error for ' + eventType + ':', err);
                    return;
                }
                fn(parsed, e);
            });
        }

        es.onerror = () => {
            if (closed) return;
            es.close();
            setTimeout(connect, reconnectDelay);
            reconnectDelay = Math.min(reconnectDelay * 2, 10000);
        };
    }

    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', connect);
    } else {
        connect();
    }

    window.addEventListener('beforeunload', () => { if (es) es.close(); });

    return { close: () => { closed = true; if (es) es.close(); } };
}

// showRefreshBanner pins a non-modal banner at the top of the viewport
// telling the operator a new server build is available. Tap reloads;
// dismiss hides until the next mismatch. Idempotent — repeated calls
// while the banner is shown are no-ops.
//
// Defined as an internal helper used by createSSE's build-mismatch
// check. Exposed for the Core admin SSE bootstrap (app.js) which has
// its own checkBuild and needs the same affordance.
export function showRefreshBanner() {
    if (document.getElementById('shingo-refresh-banner')) return;
    const banner = document.createElement('div');
    banner.id = 'shingo-refresh-banner';
    banner.setAttribute('role', 'status');
    banner.style.cssText = 'position:fixed;top:0;left:0;right:0;z-index:var(--z-toast);'
        + 'background:#fff3cd;border-bottom:1px solid #ffe69c;color:#664d03;'
        + 'padding:0.6rem 1rem;display:flex;align-items:center;gap:0.75rem;'
        + 'justify-content:center;font-size:0.95rem;'
        + 'box-shadow:0 2px 6px rgba(0,0,0,0.08);';
    const msg = document.createElement('span');
    msg.textContent = 'A new version is available.';
    const reloadBtn = document.createElement('button');
    reloadBtn.textContent = 'Refresh';
    reloadBtn.style.cssText = 'padding:0.3rem 0.9rem;background:#0d6efd;color:#fff;'
        + 'border:none;border-radius:4px;cursor:pointer;font-weight:600;';
    reloadBtn.addEventListener('click', () => location.reload());
    const dismissBtn = document.createElement('button');
    dismissBtn.textContent = 'Dismiss';
    dismissBtn.style.cssText = 'padding:0.3rem 0.7rem;background:transparent;color:#664d03;'
        + 'border:1px solid #c9a85e;border-radius:4px;cursor:pointer;';
    dismissBtn.addEventListener('click', () => banner.remove());
    banner.appendChild(msg);
    banner.appendChild(reloadBtn);
    banner.appendChild(dismissBtn);
    document.body.appendChild(banner);
}

// ─── Modals ─────────────────────────────────────────────────────────────

// Open a modal. Adds .active to the overlay element.
//
// opts.closeOnBackdrop (default false): wire a one-shot backdrop click
// listener that hides the modal. Use for info/confirm modals.
//
// Escape-to-close is wired globally on first showModal call; it closes
// the topmost .active modal. No per-modal opt-out.
export function showModal(id, opts) {
    const m = document.getElementById(id);
    if (!m) return;
    m.classList.add('active');
    ensureEscClose();
    if (opts && opts.closeOnBackdrop) {
        attachBackdropClose(m);
    }
}

// Hide a modal. Removes .active.
//
// opts.preserveState (default false): keep current input values. Use
// for wizards and long edit-flows. Default behavior clears every
// input/select/textarea inside the modal so reopening starts fresh.
export function hideModal(id, opts) {
    const m = document.getElementById(id);
    if (!m) return;
    m.classList.remove('active');
    if (opts && opts.preserveState) return;
    m.querySelectorAll('input, select, textarea').forEach(inp => {
        if (inp.type === 'hidden') return;
        if (inp.type === 'checkbox' || inp.type === 'radio') { inp.checked = false; return; }
        if (inp.tagName === 'SELECT') { inp.selectedIndex = 0; return; }
        inp.value = inp.defaultValue || '';
    });
}

let _escWired = false;
function ensureEscClose() {
    if (_escWired) return;
    _escWired = true;
    document.addEventListener('keydown', e => {
        if (e.key !== 'Escape') return;
        // Close the last .active modal (topmost in stack)
        const open = document.querySelectorAll('.modal-overlay.active');
        if (open.length === 0) return;
        open[open.length - 1].classList.remove('active');
    });
}

function attachBackdropClose(modal) {
    if (modal.dataset.backdropWired === '1') return;
    modal.dataset.backdropWired = '1';
    modal.addEventListener('click', e => {
        // Click directly on the overlay (not inner .modal content) → close
        if (e.target === modal) modal.classList.remove('active');
    });
}

// ─── Confirm / Prompt / Toast ───────────────────────────────────────────

// Promise-based async confirm. Replaces native window.confirm.
//
//   if (!await confirm('Delete this?')) return;
export function confirm(message) {
    return new Promise(resolve => {
        const overlay = el('div', { className: 'confirm-overlay modal-overlay active' });
        const box = el('div', { className: 'confirm-box modal' });
        box.innerHTML = '<p>' + escapeHtml(message) + '</p>';
        const cancelBtn = el('button', {
            className: 'btn',
            onclick: () => { overlay.remove(); resolve(false); },
        }, 'Cancel');
        const okBtn = el('button', {
            className: 'btn btn-danger',
            onclick: () => { overlay.remove(); resolve(true); },
        }, 'Confirm');
        box.appendChild(cancelBtn);
        box.appendChild(okBtn);
        overlay.appendChild(box);
        document.body.appendChild(overlay);
    });
}

// Promise-based async prompt. Replaces native window.prompt.
// opts.type — 'text' | 'number' (default 'text')
// opts.min, opts.max — bounds for number inputs
// opts.value — default input value
// Resolves to the entered string, or null on cancel.
export function prompt(message, opts) {
    opts = opts || {};
    return new Promise(resolve => {
        const overlay = el('div', { className: 'confirm-overlay modal-overlay active' });
        const box = el('div', { className: 'confirm-box modal' });
        box.innerHTML = '<p>' + escapeHtml(message) + '</p>';
        const input = el('input', {
            className: 'form-input',
            type: opts.type === 'number' ? 'number' : 'text',
            value: opts.value !== undefined ? String(opts.value) : '',
        });
        if (opts.min !== undefined) input.min = String(opts.min);
        if (opts.max !== undefined) input.max = String(opts.max);
        const close = (result) => { overlay.remove(); resolve(result); };
        const cancelBtn = el('button', { className: 'btn', onclick: () => close(null) }, 'Cancel');
        const okBtn = el('button', { className: 'btn btn-primary', onclick: () => close(input.value) }, 'OK');
        input.addEventListener('keydown', e => {
            if (e.key === 'Enter') close(input.value);
            else if (e.key === 'Escape') close(null);
        });
        box.appendChild(input);
        box.appendChild(cancelBtn);
        box.appendChild(okBtn);
        overlay.appendChild(box);
        document.body.appendChild(overlay);
        setTimeout(() => input.focus(), 0);
    });
}

// Auto-dismissing toast notification.
//   toast('Saved', 'success');
//   toast('Network error', 'error', { sticky: true });
//
// Levels: 'success', 'error', 'warning', 'info'.
export function toast(message, level, opts) {
    level = level || 'info';
    opts = opts || {};
    let container = document.getElementById('toast-container');
    if (!container) {
        container = el('div', {
            id: 'toast-container',
            style: {
                position: 'fixed', top: '1rem', right: '1rem',
                display: 'flex', flexDirection: 'column', gap: '0.5rem',
                zIndex: '10000', pointerEvents: 'none',
            },
        });
        document.body.appendChild(container);
    }
    const t = el('div', {
        className: 'toast toast-' + level,
        style: {
            padding: '0.6rem 1rem', borderRadius: 'var(--radius)',
            background: 'var(--surface)', color: 'var(--text)',
            border: '1px solid var(--border)', boxShadow: 'var(--shadow-md)',
            pointerEvents: 'auto', minWidth: '12rem', maxWidth: '24rem',
            fontSize: '0.9rem',
        },
    }, message);
    // Color stripe via border-left
    t.style.borderLeftWidth = '4px';
    t.style.borderLeftColor = {
        success: 'var(--success)',
        error:   'var(--danger)',
        warning: 'var(--warning)',
        info:    'var(--info)',
    }[level] || 'var(--info)';
    container.appendChild(t);
    if (!opts.sticky) {
        const duration = (level === 'error' && opts.sticky !== false) ? 5000 : 3200;
        setTimeout(() => t.remove(), duration);
    } else {
        t.style.cursor = 'pointer';
        t.title = 'Click to dismiss';
        t.addEventListener('click', () => t.remove());
    }
    return t;
}

// installBackdropClose registers a single document-level click
// listener that closes any .modal-overlay element marked
// `data-backdrop-close` when the click target IS the overlay (not
// an inner .modal child). Modals that want this behavior set the
// attribute in their markup; everything else stays opaque to the
// listener.
//
// Idempotent: subsequent calls do nothing. Page scripts call this
// once (typically at the top of their module) before any per-page
// delegateActions registrations.
let _backdropInstalled = false;
export function installBackdropClose() {
    if (_backdropInstalled) return;
    _backdropInstalled = true;
    // Track where the pointer was pressed. A click whose target is the overlay
    // can be the tail of a text-selection drag that STARTED inside the modal
    // (e.g. selecting text in an input) and released on the backdrop — the
    // browser retargets that click to the common ancestor (the overlay), which
    // would otherwise dismiss the modal mid-selection. Only treat it as a
    // backdrop dismiss when the press ALSO began on the overlay itself.
    let downTarget = null;
    const recordDown = (evt) => { downTarget = evt.target; };
    document.addEventListener('mousedown', recordDown, true);
    document.addEventListener('touchstart', recordDown, true);
    document.addEventListener('click', (evt) => {
        const t = evt.target;
        if (t && t instanceof Element && t.hasAttribute('data-backdrop-close') && downTarget === t) {
            t.classList.remove('active');
            // AND TELL THE PAGE, because hiding is not closing. Stripping the
            // class is the whole close for a modal that carries no state; a
            // modal that ALSO owns state — an id, a URL parameter, a
            // subscription — was left believing it was still open. The order
            // modal was: the overlay went away, _orderModalID stayed set and
            // ?open=N stayed on the URL, so a refresh reopened what the
            // operator had just dismissed, and an SSE update could re-show it
            // without one.
            //
            // An event rather than a callback registry: every existing user
            // keeps working untouched, and a page with state listens for it.
            t.dispatchEvent(new CustomEvent('backdropclose', { bubbles: true }));
        }
    });
}

// onSSE — single-connection, multi-subscriber SSE event bus.
//
//   const off = onSSE('robot-update', data => { ... });   // returns unsubscribe
//   off();
//
// Today's app.js dispatches each SSE type to a single window.onXxx global,
// so two sections that both need 'order-update' silently clobber each other.
// This bus keeps a Map<type, Set<handler>> and fans out to every subscriber,
// over ONE EventSource per tab regardless of how many sections subscribe.
//
// The connection is topic-filtered: it requests /events?topics=<subscribed
// types> so the server (Core's AddClientFiltered; the Edge's HandleSSE
// parseTopics) never streams event types nobody
// on this tab asked for — keeping a /missions tab off the per-pulse
// cell-heartbeat firehose and a kiosk off admin-only events. Subscribing a
// new type after connect transparently reopens with the widened topic set
// (coalesced onto a microtask so a burst of page-load subscribes is one
// reopen).
//
// On every (re)connect the bus dispatches a synthetic 'connected' event, so
// sections can re-fetch their snapshot and close the gap of events missed
// while disconnected (§13). Build-id mismatch shows the shared refresh
// banner. handler receives (parsedJSON, rawEvent).
//
// A LOST FRAME IS NOT SILENT on a server that numbers its frames (the Edge's
// /events: each frame's SSE id is the hub's sequence number, and the connected
// frame and every heartbeat carry `seq`, the number of the last frame queued
// for this stream). The bus keeps the highest id it has seen; a heartbeat
// whose seq is ahead of it means a frame never arrived, and the bus dispatches
// the synthetic 'stream-gap' so a section refetches. A server that sends no
// seq (Core) is never checked.
//
// A WATCHDOG, when a page asks for one (setSSEWatchdog): no frame at all for
// that long and the stream is treated as dead — closed, 'disconnected'
// dispatched, reopened — rather than shown live because no error fired.
const _busHandlers = new Map(); // type -> Set<fn>
let _busES = null;
let _busTopics = null;          // topics the live ES was opened with
let _busSeenBuild = null;
let _busReconnectDelay = 1000;
let _busClosed = false;
let _busSyncScheduled = false;
let _busQuery = '';             // extra query parameters (setSSEQuery)
let _busLastSeq = 0;            // highest frame id seen on the live stream
let _busWatchdogMs = 0;         // 0: no watchdog (setSSEWatchdog)
let _busWatchdog = null;

// Frames the stream itself makes, or the bus makes locally: never requested
// as a topic, never listened to as a server type.
const _busStreamTypes = { connected: true, disconnected: true, heartbeat: true, 'stream-gap': true };

function _busDesiredTopics() {
    const out = [];
    _busHandlers.forEach((_set, type) => {
        if (!_busStreamTypes[type]) out.push(type);
    });
    return out.sort().join(',');
}

// setSSEQuery adds parameters to the stream's URL (the station board names its
// station, so the Edge marks its screen online on connect and heartbeat). Call
// before the first subscribe.
export function setSSEQuery(params) {
    _busQuery = Object.keys(params || {}).sort().map(k => encodeURIComponent(k) + '=' + encodeURIComponent(params[k])).join('&');
}

// setSSEWatchdog arms the no-frame watchdog: ms with no frame of any kind and
// the stream is treated as dead. Pick a few heartbeats' worth. 0 disarms.
export function setSSEWatchdog(ms) { _busWatchdogMs = ms > 0 ? ms : 0; }

function _busFeedWatchdog(es) {
    if (!_busWatchdogMs) return;
    if (_busWatchdog !== null) clearTimeout(_busWatchdog);
    _busWatchdog = setTimeout(() => {
        _busWatchdog = null;
        if (_busES === es) _busDrop(es);
    }, _busWatchdogMs);
}

// _busNoteSeq keeps the highest frame id seen.
function _busNoteSeq(ev) {
    const id = ev && ev.lastEventId ? Number(ev.lastEventId) : NaN;
    if (isFinite(id) && id > _busLastSeq) _busLastSeq = id;
}

function _busFrameSeq(ev) {
    try {
        const seq = (JSON.parse(ev.data) || {}).seq;
        return typeof seq === 'number' && isFinite(seq) ? seq : null;
    } catch (_) { return null; }
}

function _busDispatch(type, ev) {
    const set = _busHandlers.get(type);
    if (!set || set.size === 0) return;
    let parsed = null;
    if (ev && typeof ev.data === 'string' && ev.data.length) {
        try { parsed = JSON.parse(ev.data); }
        catch (err) { console.error('onSSE parse ' + type, err); return; }
    }
    Array.prototype.forEach.call(Array.from(set), (fn) => {
        try { fn(parsed, ev); } catch (err) { console.error('onSSE handler ' + type, err); }
    });
}

let _busReloadOnBuild = false;

// setSSEReloadOnBuild switches the bus's build-id-mismatch behavior from the
// dismissible refresh banner (admin pages) to an immediate location.reload()
// (kiosk pages — no operator to dismiss; the right move is to adopt the new
// Core build). Applies to mismatches seen on both the connected and heartbeat
// frames, so a kiosk reloads even when a proxy holds the SSE socket open
// through a Core restart. Idempotent.
export function setSSEReloadOnBuild(on) { _busReloadOnBuild = !!on; }

function _busCheckBuild(e) {
    let build = '';
    try { build = (JSON.parse(e.data) || {}).build || ''; } catch (_) {}
    if (!build) return;
    if (_busSeenBuild === null) { _busSeenBuild = build; return; }
    if (_busSeenBuild === build) return;
    if (_busReloadOnBuild) {
        if (typeof location !== 'undefined') location.reload();
    } else {
        showRefreshBanner();
    }
}

function _busConnect() {
    const topics = _busDesiredTopics();
    _busTopics = topics;
    const params = [];
    if (topics) params.push('topics=' + encodeURIComponent(topics));
    if (_busQuery) params.push(_busQuery);
    const url = params.length ? ('/events?' + params.join('&')) : '/events';
    const es = new EventSource(url);
    _busES = es;
    _busFeedWatchdog(es);
    es.addEventListener('connected', (e) => {
        _busReconnectDelay = 1000;
        _busFeedWatchdog(es);
        const seq = _busFrameSeq(e);
        _busLastSeq = seq === null ? 0 : seq;
        _busCheckBuild(e);
        _busDispatch('connected', e);
    });
    es.addEventListener('heartbeat', (e) => {
        _busFeedWatchdog(es);
        _busCheckBuild(e);
        const seq = _busFrameSeq(e);
        if (seq !== null && seq > _busLastSeq) {
            _busLastSeq = seq;
            _busDispatch('stream-gap', null);
        }
        _busDispatch('heartbeat', e);
    });
    _busHandlers.forEach((_set, type) => {
        if (_busStreamTypes[type]) return;
        es.addEventListener(type, (ev) => {
            _busFeedWatchdog(es);
            _busNoteSeq(ev);
            _busDispatch(type, ev);
        });
    });
    es.onerror = () => { if (!_busClosed) _busDrop(es); };
}

// _busDrop closes a dead stream, says so, and reopens after the backoff.
function _busDrop(es) {
    es.close();
    if (_busES === es) _busES = null;
    if (_busWatchdog !== null) { clearTimeout(_busWatchdog); _busWatchdog = null; }
    // Synthetic 'disconnected' so subscribers can show an offline state
    // (the bus auto-reconnects below; a later 'connected' re-fires).
    _busDispatch('disconnected', null);
    const delay = _busReconnectDelay;
    _busReconnectDelay = Math.min(_busReconnectDelay * 2, 10000);
    setTimeout(() => { if (!_busClosed && !_busES) _busConnect(); }, delay);
}

function _busSync() {
    if (_busSyncScheduled) return;
    _busSyncScheduled = true;
    Promise.resolve().then(() => {
        _busSyncScheduled = false;
        if (_busClosed) return;
        if (_busHandlers.size === 0) return;          // nothing to listen for yet
        if (_busES && _busDesiredTopics() === _busTopics) return; // already correct
        if (_busES) { _busES.close(); _busES = null; }
        _busConnect();
    });
}

export function onSSE(type, handler) {
    if (typeof handler !== 'function') return () => {};
    let set = _busHandlers.get(type);
    if (!set) { set = new Set(); _busHandlers.set(type, set); }
    set.add(handler);
    _busSync();
    return function off() {
        const s = _busHandlers.get(type);
        if (!s) return;
        s.delete(handler);
        if (s.size === 0) _busHandlers.delete(type);
        // Intentionally does not narrow topics / reconnect on unsubscribe —
        // a slightly-wider topic set is harmless and avoids teardown churn.
    };
}

// closeSSEBus — tear down the shared EventSource (page unload / tests).
export function closeSSEBus() {
    _busClosed = true;
    if (_busWatchdog !== null) { clearTimeout(_busWatchdog); _busWatchdog = null; }
    if (_busES) { _busES.close(); _busES = null; }
}
if (typeof window !== 'undefined') {
    window.addEventListener('beforeunload', () => { if (_busES) _busES.close(); });
}

// ─── Settings pages ──────────────────────────────────────────────────────
//
// The house settings shape (docs/ui-style-guide/33-settings-pages.md): one
// page, one Save in the foot bar, a draft that is never applied half-typed.
// The CSS is shared/components.css's .set- rules; this is the behaviour.

const _DURATION_UNITS = {
    ns: 1e-6, us: 1e-3, 'µs': 1e-3,
    ms: 1, msec: 1, msecs: 1, millisecond: 1, milliseconds: 1,
    s: 1000, sec: 1000, secs: 1000, second: 1000, seconds: 1000,
    m: 60000, min: 60000, mins: 60000, minute: 60000, minutes: 60000,
    h: 3600000, hr: 3600000, hrs: 3600000, hour: 3600000, hours: 3600000,
};

// durationMs reads a duration as typed or as Go wrote it ("45 min", "10 s",
// "1 h 30 min", "45m0s", "1h30m", "1.5h", "500ms") into whole milliseconds.
// A bare 0 is zero. Anything else, including a negative, is null.
function durationMs(text) {
    const s = String(text === null || text === undefined ? '' : text).trim().toLowerCase();
    if (s === '') return null;
    if (/^0+(\.0+)?$/.test(s)) return 0;
    const re = /(\d+(?:\.\d+)?|\.\d+)\s*([a-zµ]+)\s*/gy;
    let total = 0;
    let at = 0;
    let m;
    while ((m = re.exec(s)) !== null) {
        const unit = _DURATION_UNITS[m[2]];
        if (unit === undefined) return null;
        total += parseFloat(m[1]) * unit;
        at = re.lastIndex;
    }
    if (at !== s.length) return null;
    return Math.round(total);
}

function _trimNum(n) {
    return String(Math.round(n * 1000) / 1000);
}

// goDuration writes milliseconds the way Go's time.Duration.String does
// ("45m0s", "1h0m0s", "10s", "1m30.5s", "500ms"), so the server's
// time.ParseDuration reads it and a value saved and read back is unchanged.
function goDuration(ms) {
    if (ms === 0) return '0s';
    if (ms < 1000) return _trimNum(ms) + 'ms';
    const h = Math.floor(ms / 3600000);
    const m = Math.floor((ms % 3600000) / 60000);
    const sec = (ms % 60000) / 1000;
    let out = '';
    if (h) out += h + 'h';
    if (h || m) out += m + 'm';
    return out + _trimNum(sec) + 's';
}

// durationFromText is what the page sends for a duration box: the typed text
// ("45 min", or a Go string typed in) as a Go duration string. Blank is ''
// (the page decides what blank means); text that is not a duration is null,
// which the page shows as a field error.
export function durationFromText(text) {
    if (String(text === null || text === undefined ? '' : text).trim() === '') return '';
    const ms = durationMs(text);
    return ms === null ? null : goDuration(ms);
}

// durationToText is what the box shows for a stored Go duration: one box
// with its unit in it ("45 min", "10 s", "1 h", "1 h 30 min"), never "45m0s".
// A value that does not parse is shown as stored, so nothing is hidden.
export function durationToText(goStr) {
    const ms = durationMs(goStr);
    if (ms === null) return goStr === null || goStr === undefined ? '' : String(goStr);
    if (ms === 0) return '0 s';
    const parts = [];
    const h = Math.floor(ms / 3600000);
    const m = Math.floor((ms % 3600000) / 60000);
    const s = Math.floor((ms % 60000) / 1000);
    const rest = ms % 1000;
    if (h) parts.push(h + ' h');
    if (m) parts.push(m + ' min');
    if (s) parts.push(s + ' s');
    if (rest) parts.push(_trimNum(rest) + ' ms');
    return parts.join(' ');
}

// collectList reads a list section from the rows that are on the page, in
// order, NOT by index: removing row 0 must never lose the rows after it (an
// index walk stops at the first missing index). read(row) returns the
// row's value; null, undefined and '' are left out (a blank new row).
export function collectList(container, rowSelector, read) {
    if (!container) return [];
    const rows = container.querySelectorAll(rowSelector);
    const out = [];
    for (let i = 0; i < rows.length; i++) {
        const v = read(rows[i]);
        if (v === null || v === undefined || v === '') continue;
        out.push(v);
    }
    return out;
}

// settingsPage binds a settings page's draft, save bar and answers.
//
//   const page = settingsPage(root, {
//       url: '/api/config',            // the shared save door (PUT unless opts.method)
//       restart: ['Station UID'],      // the server's notice at render, if any
//       sections: {
//           fleet: {
//               read()        -> the section's draft as an object (from the page),
//               write(values) -> put values back on the page (Discard),
//               validate(d)   -> {field: msg} or null, before anything is sent,
//               body(d)       -> the section's wire body in the shared request
//                                (default: d itself),
//           },
//           shifts: {
//               label: 'shifts',  // how the outcome line names it (default: the key)
//               read, write, validate as above,
//               save(d)       -> Promise<{ok, errors?, error?}>: a section with
//                                its own door; it is left out of the shared request
//           },
//       },
//       onSaved(body, sections) -> after the shared request saved, e.g. to redraw live words
//   });
//
// Dirty is decided per section by comparing read() with what the page last
// loaded or saved, after every input, change and click inside root, and on
// page.update() (call it after changing the draft from code).
//
// A save validates every dirty section first; any error and nothing is sent.
// Then it sends ONE shared request, {section: body(d)} for the dirty sections
// without their own save, through apiResult, and reads the answer:
// {ok, errors{field: msg}, error} on a refusal, {ok, applied, restart, failed}
// on a save. Then each dirty section with its own save(d) is saved, in order.
// Each part stands alone: a section is clean again only if its own part saved,
// and a partial outcome is said plainly ("Settings saved; shifts failed: …").
//
// A field error is drawn under the row holding [data-field="<field>"]; one with
// no such field goes in the refusal box above the bar. `restart` lists the
// fields waiting on a restart, shown in the notice above the bar; `failed`
// names what was saved but did not apply.
//
// The bar is root's .set-savebar if the page drew one (with a .prov and two
// buttons [data-set="discard"] / [data-set="save"]); otherwise it is built at
// the end of root. The notice and refusal boxes go directly above it.
export function settingsPage(root, opts) {
    opts = opts || {};
    const sections = opts.sections || {};
    const names = Object.keys(sections);
    const send = opts.send || apiResult;
    const method = opts.method || 'PUT';
    const state = {
        baseline: {},
        saving: false,
        restart: Array.isArray(opts.restart) ? opts.restart.slice() : [],
        failed: [],
        refusal: '',
        errLines: [],
    };
    const snap = (n) => JSON.stringify(sections[n].read());
    names.forEach((n) => { state.baseline[n] = snap(n); });

    let bar = root.querySelector('.set-savebar');
    if (!bar) {
        bar = el('div', { className: 'set-savebar' }, [
            el('span', { className: 'prov' }, 'No unsaved changes'),
            el('button', { type: 'button', className: 'set-btn quiet', dataset: { set: 'discard' } }, 'Discard'),
            el('button', { type: 'button', className: 'set-btn primary', dataset: { set: 'save' } }, 'Save'),
        ]);
        root.appendChild(bar);
    }
    const prov = bar.querySelector('.prov');
    const discardBtn = bar.querySelector('[data-set="discard"]');
    const saveBtn = bar.querySelector('[data-set="save"]');
    const notice = el('div', { className: 'set-notice', role: 'status' });
    const refusal = el('div', { className: 'set-refusal', role: 'alert' });
    notice.hidden = true;
    refusal.hidden = true;
    bar.parentNode.insertBefore(notice, bar);
    bar.parentNode.insertBefore(refusal, bar);

    function dirtySections() {
        return names.filter((n) => snap(n) !== state.baseline[n]);
    }

    function render() {
        const dirty = dirtySections().length > 0;
        if (prov) {
            prov.textContent = state.saving ? 'Saving…' : (dirty ? 'Unsaved changes' : 'No unsaved changes');
            prov.classList.toggle('dirty', dirty);
        }
        if (saveBtn) saveBtn.disabled = !dirty || state.saving;
        if (discardBtn) discardBtn.disabled = !dirty || state.saving;
        notice.hidden = state.restart.length === 0;
        notice.textContent = state.restart.length
            ? 'Applies after a restart: ' + state.restart.join(', ') + '.'
            : '';
        let ref = state.refusal;
        if (state.failed.length) {
            ref = (ref ? ref + ' ' : '') + 'Saved, but not applied: ' + state.failed.join(', ') + '.';
        }
        refusal.hidden = !ref;
        refusal.textContent = ref;
    }

    function clearErrors() {
        state.errLines.forEach((line) => { if (line.parentNode) line.parentNode.removeChild(line); });
        state.errLines = [];
        state.refusal = '';
    }

    function fieldFor(key) {
        const q = String(key).replace(/["\\]/g, '\\$&');
        return root.querySelector('[data-field="' + q + '"]');
    }

    // showErrors draws each field error under its row and returns the
    // messages that had no field to sit under.
    function showErrors(errors) {
        const loose = [];
        Object.keys(errors || {}).forEach((key) => {
            const msg = String(errors[key]);
            const field = fieldFor(key);
            if (!field) { loose.push(msg); return; }
            const row = (field.closest && field.closest('.set-fld')) || field;
            const line = el('div', { className: 'set-err', role: 'alert', dataset: { errFor: key } }, msg);
            row.parentNode.insertBefore(line, row.nextSibling);
            state.errLines.push(line);
        });
        return loose;
    }

    // failureText says why one part did not save: its messages with no field
    // to sit under, else its top-level error, else that the marked fields are why.
    function failureText(answer, loose, placed, status) {
        if (loose.length) return loose.join(' ');
        if (!placed) return (answer && answer.error) || ('the server answered ' + status + '.');
        return 'see the marked fields.';
    }

    async function save() {
        const dirty = dirtySections();
        if (!dirty.length || state.saving) return null;
        clearErrors();
        state.failed = [];
        const errors = {};
        const drafts = {};
        dirty.forEach((n) => {
            const s = sections[n];
            drafts[n] = s.read();
            const e = s.validate ? s.validate(drafts[n]) : null;
            if (e) Object.assign(errors, e);
        });
        if (Object.keys(errors).length) {
            const loose = showErrors(errors);
            state.refusal = 'Not saved.' + (loose.length ? ' ' + loose.join(' ') : ' Fix the marked fields.');
            render();
            return { ok: false, shared: null, own: {}, errors };
        }

        const shared = dirty.filter((n) => typeof sections[n].save !== 'function');
        const own = dirty.filter((n) => typeof sections[n].save === 'function');
        const parts = [];   // {label, ok, why}
        const out = { ok: true, shared: null, own: {} };
        state.saving = true;
        render();
        try {
            if (shared.length) {
                const body = {};
                shared.forEach((n) => {
                    const s = sections[n];
                    body[n] = s.body ? s.body(drafts[n]) : drafts[n];
                });
                let res;
                try {
                    res = await send(method, opts.url, body);
                } catch (e) {
                    res = { ok: false, status: 0, body: { error: String(e && e.message ? e.message : e) } };
                }
                out.shared = res;
                const b = (res && res.body && typeof res.body === 'object') ? res.body : {};
                if (!res || !res.ok || b.ok === false) {
                    const loose = showErrors(b.errors);
                    const placed = Object.keys(b.errors || {}).length - loose.length;
                    parts.push({ label: 'Settings', ok: false, why: failureText(b, loose, placed, res ? res.status : 0) });
                } else {
                    shared.forEach((n) => { state.baseline[n] = JSON.stringify(drafts[n]); });
                    if (Array.isArray(b.restart)) state.restart = b.restart.slice();
                    state.failed = Array.isArray(b.failed) ? b.failed.slice() : [];
                    parts.push({ label: 'Settings', ok: true });
                    if (opts.onSaved) opts.onSaved(b, shared);
                }
            }
            for (const n of own) {
                const s = sections[n];
                const label = s.label || n;
                let r;
                try {
                    r = await s.save(drafts[n]);
                } catch (e) {
                    r = { ok: false, error: String(e && e.message ? e.message : e) };
                }
                r = r || { ok: false, error: 'no answer.' };
                out.own[n] = r;
                if (r.ok) {
                    state.baseline[n] = JSON.stringify(drafts[n]);
                    parts.push({ label, ok: true });
                } else {
                    const loose = showErrors(r.errors);
                    const placed = Object.keys(r.errors || {}).length - loose.length;
                    parts.push({ label, ok: false, why: failureText(r, loose, placed, 0) });
                }
            }
        } finally {
            state.saving = false;
        }

        out.ok = parts.every((p) => p.ok);
        if (out.ok) {
            if (!state.failed.length) toast('Saved', 'success');
        } else if (parts.length === 1) {
            const why = parts[0].why;
            state.refusal = 'Not saved. ' + why.charAt(0).toUpperCase() + why.slice(1);
        } else {
            // A partial outcome, said plainly: "Settings saved; shifts failed: …".
            const line = parts.map((p) => p.label + (p.ok ? ' saved' : ' failed: ' + p.why.replace(/\.$/, ''))).join('; ');
            state.refusal = line.charAt(0).toUpperCase() + line.slice(1) + '.';
        }
        render();
        return out;
    }

    function discard() {
        dirtySections().forEach((n) => {
            if (sections[n].write) sections[n].write(JSON.parse(state.baseline[n]));
        });
        clearErrors();
        render();
    }

    let pending = false;
    function later() {
        if (pending) return;
        pending = true;
        Promise.resolve().then(() => { pending = false; render(); });
    }
    root.addEventListener('input', later);
    root.addEventListener('change', later);
    root.addEventListener('click', later);
    if (saveBtn) saveBtn.addEventListener('click', () => { save(); });
    if (discardBtn) discardBtn.addEventListener('click', discard);
    render();

    return {
        save,
        discard,
        update: render,
        isDirty: () => dirtySections().length > 0,
        dirtySections,
        state,
    };
}
