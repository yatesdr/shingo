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
//   settings.js settingsPage, durationFromText, durationToText, collectList
//   bus.js      onSSE, setSSEQuery, setSSEWatchdog, setSSEReloadOnBuild, closeSSEBus
//
// Left here: HTTP, the modals, confirm / prompt / toast, the backdrop close,
// and createSSE with the refresh banner the bus shows too.
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
