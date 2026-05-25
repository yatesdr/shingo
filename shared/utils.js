// Shared JavaScript utilities for Shingo admin surfaces (Core + Edge).
//
// ES module, served at /static/shared/utils.js. Consumers import the
// helpers they need. The operator HMI already uses ES modules; Core and
// Edge admin migrate via the UI consistency refactor.
//
// Design intent:
//   - Same vocabulary across surfaces; no per-surface reimplementations.
//   - HTML construction: prefer h`` for new code, escapeHtml for the
//     legacy string-concat sites that haven't been migrated yet.
//   - DOM mutation: el(tag, props, children) wires listeners during
//     construction; no orphaned addEventListener calls.
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

// ─── String / DOM safety ────────────────────────────────────────────────

// Last-resort escape for legacy innerHTML concatenation. Prefer h``.
export function escapeHtml(s) {
    if (s === null || s === undefined || s === '') return '';
    const d = document.createElement('div');
    d.appendChild(document.createTextNode(s));
    return d.innerHTML;
}

// Tagged-template HTML builder. Static parts pass through; interpolations
// are escaped, arrays joined unescaped (for `${rows.map(r => h`...`)}`
// patterns), null/undefined/false dropped, and pre-built safe HTML can
// opt out via `${ { __html: true, value: precomputed } }`.
//
//   container.innerHTML = h`<div class="x">${name}</div>${rows.map(r => h`<p>${r}</p>`)}`;
export function h(strings, ...values) {
    let out = strings[0];
    for (let i = 0; i < values.length; i++) {
        const v = values[i];
        if (Array.isArray(v)) {
            out += v.join('');
        } else if (v === null || v === undefined || v === false) {
            // skip — lets `${cond && h`...`}` work
        } else if (typeof v === 'object' && v.__html === true) {
            out += v.value; // pre-built safe HTML opt-out
        } else {
            out += escapeHtml(String(v));
        }
        out += strings[i + 1];
    }
    return out;
}

// DOM element builder.
//   props: any HTML attribute name (lowercased), className, dataset
//          (object of data-* keys), style (object of CSS properties),
//          onclick / onchange / etc. (camelCased → addEventListener).
//   children: string, Node, array (recursively), or null/undefined/false (skipped).
//
// Single signature across all surfaces; operator-util.js's previous
// two-arg form (tag, children) is migrated to (tag, null, children) as
// part of the consistency refactor.
export function el(tag, props, children) {
    const node = document.createElement(tag);
    if (props) {
        for (const key in props) {
            if (!Object.prototype.hasOwnProperty.call(props, key)) continue;
            const val = props[key];
            if (val === null || val === undefined || val === false) continue;
            if (key === 'className') {
                node.className = val;
            } else if (key === 'dataset') {
                for (const dk in val) node.dataset[dk] = val[dk];
            } else if (key === 'style' && typeof val === 'object') {
                for (const sk in val) node.style[sk] = val[sk];
            } else if (key.length > 2 && key.indexOf('on') === 0 && typeof val === 'function') {
                node.addEventListener(key.substring(2).toLowerCase(), val);
            } else {
                node.setAttribute(key, val);
            }
        }
    }
    if (children !== undefined && children !== null) {
        appendChildren(node, children);
    }
    return node;
}

function appendChildren(node, children) {
    const list = Array.isArray(children) ? children : [children];
    for (let i = 0; i < list.length; i++) {
        const c = list[i];
        if (c === null || c === undefined || c === false) continue;
        if (Array.isArray(c)) { appendChildren(node, c); continue; }
        node.appendChild(c instanceof Node ? c : document.createTextNode(String(c)));
    }
}

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

// ─── Time formatting ─────────────────────────────────────────────────────

export function timeAgo(ts) {
    if (!ts) return '-';
    const d = Date.now() - new Date(ts).getTime();
    if (d < 60000) return 'just now';
    if (d < 3600000) return Math.floor(d / 60000) + 'm ago';
    if (d < 86400000) return Math.floor(d / 3600000) + 'h ago';
    return Math.floor(d / 86400000) + 'd ago';
}

export function formatTime(ts, opts) {
    if (!ts || ts === '0001-01-01T00:00:00Z') return '-';
    const d = new Date(ts);
    if (isNaN(d.getTime())) return ts;
    if (opts && opts.precision === 'ms') {
        return d.toTimeString().slice(0, 8) + '.' + String(d.getMilliseconds()).padStart(3, '0');
    }
    return d.toLocaleString();
}

export function formatDuration(ms) {
    if (!ms || ms <= 0) return '-';
    if (ms < 1000) return ms + 'ms';
    let s = Math.floor(ms / 1000);
    if (s < 60) return s + 's';
    let m = Math.floor(s / 60);
    s = s % 60;
    if (m < 60) return m + 'm ' + s + 's';
    const h = Math.floor(m / 60);
    m = m % 60;
    return h + 'h ' + m + 'm';
}

// Rewrite <time data-utc="..."> elements to the browser's local-time string.
// Idempotent; safe to re-run after htmx swaps insert new <time> nodes.
export function convertTimestamps(root) {
    const scope = root || document;
    scope.querySelectorAll('time[data-utc]').forEach(elem => {
        const d = new Date(elem.getAttribute('data-utc'));
        if (!isNaN(d.getTime())) elem.textContent = d.toLocaleString();
    });
}

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

    function connect() {
        es = new EventSource(url);

        es.addEventListener('connected', e => {
            reconnectDelay = 1000;
            let build = '';
            try { build = (JSON.parse(e.data) || {}).build || ''; } catch (_) {}
            if (!build) return;
            if (seenBuild === null) {
                seenBuild = build;
            } else if (seenBuild !== build) {
                location.reload();
            }
        });

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

// ─── Misc ───────────────────────────────────────────────────────────────

// ─── Event delegation ───────────────────────────────────────────────────

// delegateActions wires event listeners on `root` that dispatch to
// entries in `handlerMap` based on the event target's nearest
// `[data-action]` (or `[data-action-<event>]`) ancestor.
//
// Default event is 'click'. Pass opts.events as an array to listen
// for several event types at once with the same handler map. Each
// event type uses its own attribute pair:
//
//   click     → data-action
//   change    → data-action-change
//   input     → data-action-input
//   blur      → data-action-blur     (registered as 'focusout' so it
//                                     bubbles)
//   keydown   → data-action-keydown
//   submit    → data-action-submit
//
// Usage:
//   delegateActions(document.body, {
//       openCreateProcessModal: (el, evt) => { … },
//       deleteClaim: (el, evt) => removeClaim(parseInt(el.dataset.id, 10)),
//   });
//
//   // multi-event registration — same map, several listeners
//   delegateActions(document.body, {
//       updatePreview: () => buildAndShow(),
//   }, { events: ['change', 'input'] });
//
// The handler receives (…args from verb:arg encoding, matchedElement, event).
// `this` is the matched element so handler bodies that used
// `onclick="foo(this)"` semantics keep working without a signature
// change.
//
// Idempotent per (root, event) pair: a dataset sentinel keyed on the
// event prevents double-binding when called multiple times for the
// same root + event.
//
// HTMX-swap pattern: page scripts that mount on document.body
// continue working when HTMX swaps a subtree — the listener is on
// the outer root that survives the swap. For partial swaps that need
// post-swap setup, call delegateActions(newRoot) on the swapped-in
// container; the dataset sentinel prevents double-firing.
export function delegateActions(root, handlerMap, opts) {
    opts = opts || {};
    if (Array.isArray(opts.events)) {
        opts.events.forEach((ev) => {
            delegateActions(root, handlerMap, { event: ev, sentinel: opts.sentinel });
        });
        return;
    }
    const event = opts.event || 'click';
    const attrName = event === 'click'
        ? 'data-action'
        : 'data-action-' + event;
    const datasetKey = event === 'click'
        ? 'action'
        : 'action' + event.charAt(0).toUpperCase() + event.slice(1);
    const sentinel = opts.sentinel
        || 'delegated' + (event === 'click' ? '' : '_' + event);
    if (root.dataset && root.dataset[sentinel] === '1') return;
    if (root.dataset) root.dataset[sentinel] = '1';

    // blur doesn't bubble; focusout does and reaches the same elements.
    const domEvent = event === 'blur' ? 'focusout' : event;

    root.addEventListener(domEvent, (evt) => {
        const target = evt.target;
        if (!target || !target.closest) return;
        const el = target.closest('[' + attrName + ']');
        if (!el || !root.contains(el)) return;
        const action = el.dataset[datasetKey];
        if (!action) return;

        // Click-only conventions:
        //   data-skip-on-checkbox="1" — row-level click that should
        //     ignore clicks originating inside a checkbox cell so the
        //     checkbox's own change handler can run cleanly.
        if (event === 'click' && el.dataset.skipOnCheckbox === '1' &&
            target.closest('input[type=checkbox]')) {
            return;
        }

        // Allow verb:arg1:arg2 encoding to match the prior auto-
        // dispatcher convention. Pure-verb actions still work.
        const parts = String(action).split(':');
        const verb = parts[0];

        // Built-in stopPropagation verb — exists so a child cell with
        // its own action doesn't trigger a parent row handler.
        if (verb === 'stopPropagation') {
            evt.stopPropagation();
            return;
        }

        // data-prevent-default="1" — flips preventDefault before
        // dispatch. Used for <a href="#"> click handlers that
        // shouldn't navigate, and form submits handled by a page
        // script via fetch().
        if (el.dataset.preventDefault === '1') {
            evt.preventDefault();
        }

        const fn = handlerMap[verb];
        if (typeof fn !== 'function') return;
        const args = parts.slice(1);
        args.push(el, evt);
        try {
            fn.apply(el, args);
        } catch (err) {
            console.error('action handler', event, verb, err);
        }
    });
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
    document.addEventListener('click', (evt) => {
        const t = evt.target;
        if (t && t instanceof Element && t.hasAttribute('data-backdrop-close')) {
            t.classList.remove('active');
        }
    });
}

// installHtmxTimestampConversion wires a document.body listener for
// `htmx:afterSwap` that re-runs convertTimestamps() against the
// swapped-in subtree (event.detail.target). Without this, <time
// data-utc=…> nodes that arrive via partial swap stay as raw UTC
// strings until the next full page load.
//
// Surfaces that use HTMX (Edge admin) call this once at module
// load. The initial DOMContentLoaded conversion is still wired
// per-surface — this only handles the swap case. Idempotent.
let _htmxTsInstalled = false;
export function installHtmxTimestampConversion() {
    if (_htmxTsInstalled) return;
    _htmxTsInstalled = true;
    const attach = () => {
        document.body.addEventListener('htmx:afterSwap', (evt) => {
            const target = (evt && evt.detail && evt.detail.target) || document;
            convertTimestamps(target);
        });
    };
    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', attach);
    } else {
        attach();
    }
}

// Debounce — delays calling fn until ms after the last invocation.
export function debounce(fn, ms) {
    let timer;
    return function(...args) {
        clearTimeout(timer);
        timer = setTimeout(() => fn.apply(this, args), ms);
    };
}
