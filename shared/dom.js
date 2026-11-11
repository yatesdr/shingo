// dom.js — the HTML escape and the DOM helpers Core and Edge share: escapeHtml
// and the h`` template, the el() builder, delegated actions, the one tab
// handler, and debounce (a timing helper with no better home).
//
// ES module, served at /static/shared/dom.js; a page imports each helper from
// here. No imports, and nothing runs at import: loading it only defines.
//
// Design intent:
//   - HTML construction: prefer h`` for new code, escapeHtml for the
//     legacy string-concat sites that haven't been migrated yet.
//   - DOM mutation: el(tag, props, children) wires listeners during
//     construction; no orphaned addEventListener calls.
//
// escapeHtml IS NOT esc.js's esc, and they stay in two files. They differ by
// one character (escapeHtml escapes `'` as &#39;, esc leaves it), each is
// pinned as it is, and merging them would change one. esc.js is the station
// renderers' file with no imports; escapeHtml sits here beside h, which runs
// every interpolation through it, so a Core page (which never loads esc.js)
// gets both from one request.

// ─── String / DOM safety ────────────────────────────────────────────────

// Last-resort escape for legacy innerHTML concatenation. Prefer h``.
//
// Safe for text between tags AND inside a quoted attribute, either quote.
// It used to be DOM-based (text node in, innerHTML out), which escapes `&`,
// `<` and `>` and leaves `"` and `'` alone; h`` runs every interpolation
// through this, so `title="${note}"` could be broken out of by a quote in the
// note. Pin: utils.escape.test.js. Core imports this one; its own copy and
// the page-local ones are gone, and www/clock_globals_drift_test.go's
// TestEscapeHtmlDefinedOnce keeps it that way. Edge's shingoedge.js re-exports
// this one; operator-util.js carries its own text-only copy (another stream's
// pages).
const HTML_ESCAPES = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };
export function escapeHtml(s) {
    if (s === null || s === undefined || s === '') return '';
    return String(s).replace(/[&<>"']/g, (c) => HTML_ESCAPES[c]);
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

    // Stash a per-(root, sentinel) handler map on the root so multiple
    // delegateActions calls compose instead of clobbering. The first
    // call binds the listener and creates the map; subsequent calls
    // (often from sibling page-script modules that all attach to
    // document.body) merge their handlers into the same map. Without
    // this, the sentinel below dropped every map after the first on
    // the floor, which left half the buttons on pages like Nodes
    // (where three modules each register their own handlers) unwired.
    const mapKey = '__delegateActionsMap_' + sentinel;
    let mergedMap = root[mapKey];
    if (!mergedMap) {
        mergedMap = {};
        root[mapKey] = mergedMap;
    }
    Object.assign(mergedMap, handlerMap);

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

        const fn = mergedMap[verb];
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

// switchTab is the one tab handler (style guide §22). Bound as a delegated
// action, data-action="switchTab:<key>" on a .tab inside a .tabs bar: it marks
// the bar's tab whose data-tab is key active, and shows the .tab-panel sibling
// of the bar whose id is "tab-<key>", hiding the others. A page that loads a
// panel on first view wraps it rather than copying it:
//     switchTab: (key, el) => { switchTab(key, el); if (key === 'fleet') loadFleet(); }
// Returns key.
export function switchTab(key, el) {
    const bar = el && el.closest ? el.closest('.tabs') : null;
    if (!bar) return key;
    bar.querySelectorAll('.tab').forEach((t) => t.classList.toggle('active', t.dataset.tab === key));
    const kids = bar.parentNode ? bar.parentNode.children : [];
    for (let i = 0; i < kids.length; i++) {
        if (kids[i].classList.contains('tab-panel')) kids[i].classList.toggle('active', kids[i].id === 'tab-' + key);
    }
    return key;
}

// Debounce — delays calling fn until ms after the last invocation.
export function debounce(fn, ms) {
    let timer;
    return function(...args) {
        clearTimeout(timer);
        timer = setTimeout(() => fn.apply(this, args), ms);
    };
}
