// --- Shared utilities (Core admin) ---
//
// ES module. SPRINT 3 migrated every per-page script to explicit
// `import { … } from '/static/app.js'`; SPRINT 4 promoted the
// previously bare function declarations to inline `export function`
// declarations and dropped the trailing `export { … }` block. The
// canonical cross-surface helpers live in /static/shared/utils.js;
// this file holds Core-specific helpers + SSE bootstrap.
//
// HTML construction tools available across pages:
//   h`...`            — tagged-template literal. Interpolated values are
//                       HTML-escaped automatically; arrays are joined.
//                       Use for innerHTML output sites; the safety is
//                       structural, not discipline-based.
//   el(tag, props,    — DOM-node builder. Use when the result needs
//      children)        addEventListener wired during construction.
//   <template>+clone  — for static skeletons reused per render.
//   escapeHtml(s)     — last resort for legacy string concatenation.
//                       Prefer h`` for new code.

import { delegateActions, escapeHtml, h } from '/static/shared/dom.js';
import { formatTime, timeAgo } from '/static/shared/clock.js';
import { installTableSort } from '/static/shared/list.js';
import { onSSE } from '/static/shared/bus.js';
import { confirm as uiConfirm, confirmWithInput as uiConfirmWithInput, hideModal, prompt as uiPrompt, showModal } from '/static/shared/layer.js';
import { banner, toast } from '/static/shared/notice.js';
import { send } from '/static/shared/http.js';
document.addEventListener('DOMContentLoaded', function() { installTableSort(); });
// escapeHtml and h are the shared ones (one helper per job); pages keep
// importing them from here.
export { delegateActions, escapeHtml, h };

// Debounce: delays execution until `ms` milliseconds after the last call.
// Used to prevent SSE event bursts from saturating the browser main thread.
export function debounce(fn, ms) {
  var timer;
  return function() {
    var args = arguments;
    var self = this;
    clearTimeout(timer);
    timer = setTimeout(function() { fn.apply(self, args); }, ms);
  };
}

// Element builder. props: attributes (className, dataset, id, ...) and
// event listeners (onclick, onchange) by camelCase name. children:
// string, Node, or array of either.
export function el(tag, props, children) {
  var node = document.createElement(tag);
  if (props) {
    for (var key in props) {
      if (!Object.prototype.hasOwnProperty.call(props, key)) continue;
      var val = props[key];
      if (key === 'className') {
        node.className = val;
      } else if (key === 'dataset') {
        for (var dk in val) node.dataset[dk] = val[dk];
      } else if (key.indexOf('on') === 0 && typeof val === 'function') {
        node.addEventListener(key.substring(2).toLowerCase(), val);
      } else if (key === 'style' && typeof val === 'object') {
        for (var sk in val) node.style[sk] = val[sk];
      } else if (val !== null && val !== undefined && val !== false) {
        node.setAttribute(key, val);
      }
    }
  }
  if (children !== undefined && children !== null) {
    var list = Array.isArray(children) ? children : [children];
    for (var i = 0; i < list.length; i++) {
      var c = list[i];
      if (c === null || c === undefined || c === false) continue;
      node.appendChild(c instanceof Node ? c : document.createTextNode(String(c)));
    }
  }
  return node;
}

// The modals, confirm and prompt are the one overlay stack's (shared/layer.js,
// style guide §10): Escape closes the innermost, the body's scroll lock is
// counted, focus goes in and comes back, fields clear on hide, and a confirm
// always resolves. A modal that opts in with data-backdrop-close closes on its
// backdrop without an install call, so this file no longer calls
// installBackdropClose (it and the layer would both answer one click).
// Core's pages say uiConfirm / uiPrompt / uiConfirmWithInput.
export { showModal, hideModal, uiConfirm, uiPrompt, uiConfirmWithInput };

// toast is the one notice's (shared/notice.js, style guide §11): an error
// lives 5 s, the rest 3.2 s, {sticky:true} until clicked, and the same words
// at the same level are said once.
export { toast };

// The hub's door verbs keep the contract Core's pages were written to: they
// resolve the answer's body and reject on anything else. Behind them is the
// one HTTP door (shared/http.js) with its notice off, because each page says
// a refusal in its own words ('Save failed: ' + e). So a 204 or an empty 2xx
// resolves null, a 4xx whose body is null is still a refusal with a sentence,
// a lapsed session is "Signed out", and an answer that does not come within
// the door's deadline is a refusal too.
//
// THE REJECTION IS AN ERROR WHOSE STRING IS THE SENTENCE. Pages catch it the
// way they caught the string this file used to throw: 'Save failed: ' + e,
// e.message || e, toast(e). An Error prints as "Error: <sentence>", so this
// one prints the sentence alone. Its name is 'ApiError', so a page that shows
// the door's sentence and a fallback for its own failures tells them apart
// (where it used to ask whether the rejection was a string).
export function api(method, url, body) {
  return send(method, url, body, { notice: false }).then(function(res) {
    if (res.ok) return res.body;
    throw apiError(res.error);
  });
}
function apiError(sentence) {
  var err = new Error(sentence);
  err.name = 'ApiError';
  err.toString = function() { return this.message; };
  return err;
}
export function apiGet(url)        { return api('GET', url); }
export function apiPost(url, body) { return api('POST', url, body || {}); }
export function apiPut(url, body)  { return api('PUT',  url, body || {}); }
export function apiDelete(url)     { return api('DELETE', url); }

// --- Time formatting ---
// formatDuration lives in shared/utils.js only (the twin of protocol.FormatDuration).
// This file's own ladder printed "21m 3s" and "500ms" beside the shared "21m" and
// "0 s"; it is deleted, not re-exported, so a page imports the one ladder by name.
// formatTime moved to shared/utils.js (plant-timezone pinned). Pages import
// it from '/static/shared/utils.js'; app.js keeps no date formatter of its
// own — two copies of a clock is how the two-convention display bug happened.
//
// timeAgo went the same way, one step later and for the second half of the same
// rule: it differenced a SERVER stamp against the BROWSER's Date.now(), and its
// callers pass server stamps exclusively (last_counted_at, created_at,
// updated_at). Re-exported rather than re-implemented so every existing
// `import { timeAgo } from '/static/app.js'` keeps working and there is still
// only one of it.
export { timeAgo };

// timeAgoHTML is a relative stamp ("1h ago") that carries the absolute plant
// time in its title, so ten rows of "1h ago" can still be told apart. Returns
// HTML; an absent stamp renders the timeAgo dash with no title.
export function timeAgoHTML(ts) {
  if (!ts) return escapeHtml(timeAgo(ts));
  return '<span title="' + escapeHtml(formatTime(ts)) + '">' + escapeHtml(timeAgo(ts)) + '</span>';
}

// setText sets an element's text by id; an absent value renders the guide's
// no-data dash, never an empty cell or the word "null".
export function setText(id, v) {
  var e = document.getElementById(id);
  if (e) e.textContent = (v === null || v === undefined) ? '—' : v;
}

// Timestamp conversion: the formatTime/convertTimestamps pair that used to
// live here was a byte-identical twin of shared/utils.js's. Deleted — pages
// import from '/static/shared/utils.js', whose formatter is pinned to the
// plant timezone (window.PLANT_TZ, inlined by layout.html). The server
// already renders plant-local on first paint; there is no DOMContentLoaded
// rewrite to do.

// ─── SSE bus: the subscription every page has ───────────────────────────
//
// The shared onSSE bus (shared/bus.js) maintains ONE EventSource per tab,
// handles reconnect/backoff, and dispatches 'build-changed' on a Core
// redeploy, which the subscription below turns into the refresh banner — so
// this file no longer opens its own EventSource. app.js loads on
// every page, and because the bus opens its connection on the first
// subscription, subscribing here keeps build-id detection running on every
// page. Page-specific events
// (order-update, bin-update, robot-update, mission-event, cms-transaction,
// debug-log, fire-alarm) are subscribed via onSSE in their own page modules.
// This replaces the legacy auto-connecting SSE IIFE (Q-002) so each tab holds
// a single EventSource. node-update is subscribed by the pages that want it,
// not here. inventory-update no longer exists at all — its only producer was
// the correction path, which was removed; the two pages that subscribed to it
// stay live on their bin-update subscriptions.
//
// The handler is empty on purpose (RM4): the nav pills it used to paint
// (#fleet-status, #msg-status, #redis-status) are in no template. The
// SUBSCRIPTION is what matters: it is the one every layout page has, so
// without it _busSync (shared/bus.js) never connects on a page with no
// handler of its own and the new-build banner stops there. Do not swap it for
// 'connected': that topic is excluded from the filter list, so the page would
// become an unfiltered client and receive every event.
onSSE('system-status', function() {});

// The bus only says the build changed; a Core admin page shows the notice's
// banner (ruling 4), in the words and with the Refresh the old refresh banner
// had (S5 sets the final wording). A kiosk (the dashboards) sets
// setSSEReloadOnBuild and never dispatches this. 'build-changed' is a stream
// type, so it adds no topic and opens nothing.
//
// The bus dispatches it on every frame that carries the new build (each
// heartbeat), so a banner that is up is left as it is: redrawing it would
// take the focus off its Refresh. Dismissed, it comes back on the next frame,
// as the old one did.
var buildBanner = null;
onSSE('build-changed', function() {
  if (buildBanner && buildBanner.isConnected) return;
  buildBanner = banner('build', {
    text: 'A new version is available.',
    action: { label: 'Refresh', onClick: function() { location.reload(); } },
  });
});

// ─── Auto-dispatching delegated click handler ─────────────────────────
//
// Same convention as shingo-edge/www/static/js/shingoedge.js — page
// templates emit `data-action="verb"` / `data-action="verb:arg"` /
// `data-action="verb:a:b"`; this listener resolves the closest
// [data-action] ancestor, splits on `:`, looks up `window[verb]`, and
// calls it with `(args..., el, evt)`.
//
// data-backdrop-close on a .modal-overlay closes the modal when the
// overlay itself (not an inner .modal) is clicked.
//
// data-skip-on-checkbox="1" on a row tells the dispatcher to ignore
// clicks originating inside a checkbox cell (lets row-click and per-
// row checkbox actions coexist cleanly).

// ─── Generic action handlers ───────────────────────────────────────────
// Per-page scripts register these via delegateActions when they
// emit elements with data-action="removeParentElement" /
// "removeClosestRow" / "toggleVisibility". Stay small and
// DOM-mechanical — domain logic belongs in page scripts.

export function removeParentElement() {
  // `this` is the clicked element; e.g. a "×" button inside a row.
  if (this && this.parentElement) this.parentElement.remove();
}

export function removeClosestRow() {
  if (this && this.closest) {
    var row = this.closest('tr');
    if (row) row.remove();
  }
}

export function toggleVisibility(id) {
  var el = document.getElementById(id);
  if (!el) return;
  el.style.display = (el.style.display === 'none' || !el.style.display) ? '' : 'none';
}

// openFromQuery is the ?open=<value> deep link: a page reached as
// /orders?open=42, /robots?open=AMR-07, /nodes?open=ALN_003 or /bins?open=17
// opens that thing's pop-up on load. The order pop-up links every robot, node
// and bin it names this way; one reader, so the parameter means the same on
// every page.
export function openFromQuery(open) {
  var v = new URLSearchParams(location.search).get('open');
  if (v) open(v);
}

// enterSubmits — used as data-action-keydown="enterSubmits:targetFn"
// on form-input elements that should submit on Enter (and ignore
// other keys). The page maps enterSubmits in its delegateActions map,
// and targetFn is a verb in the page's click map (the one its Create
// button names): a page module's functions are never on window, so the
// target is looked up in the handler map delegateActions keeps on the
// root (shared/utils.js, '__delegateActionsMap_' + sentinel; the click
// sentinel is 'delegated'). This helper calls it after preventDefault.
export function enterSubmits(targetFnName, el, evt) {
  if (!evt || evt.key !== 'Enter') return;
  evt.preventDefault();
  var root = evt.currentTarget || document.body;
  var clickMap = root && root.__delegateActionsMap_delegated;
  var fn = clickMap && clickMap[targetFnName];
  if (typeof fn === 'function') fn.call(el, el, evt);
}

// confirmDeleteForm — for `data-action-submit="confirmDeleteForm"
// data-confirm-msg="..."` on a plain HTML <form>. Stops the
// synchronous browser submit, awaits the styled uiConfirm, then
// programmatically resubmits the form when the user confirms.
//
// The form needs a sentinel data attribute we set after confirm so
// the second submit doesn't loop into another uiConfirm.
export async function confirmDeleteForm(el, evt) {
  if (!el || !evt) return;
  if (el.dataset.confirmed === '1') return;       // resubmit path
  evt.preventDefault();
  var msg = el.dataset.confirmMsg || 'Are you sure?';
  if (!await uiConfirm(msg)) return;
  el.dataset.confirmed = '1';
  el.submit();                                    // bypasses the listener
}

// containmentForm — for `data-action-submit="containmentForm"` on the
// payloads screen's quality-containment toggles. Unlike the delete forms'
// native resubmit, this one POSTs via fetch so a REFUSAL — activating
// containment for a payload whose claims declare no containment route —
// surfaces as a red toast on the screen the operator is looking at, instead
// of navigating the browser to a raw JSON error body. Success reloads the
// page, which re-renders the payload's new containment state.
export async function containmentForm(el, evt) {
  if (!el || !evt) return;
  evt.preventDefault();
  var data = new FormData(el);
  var msg = el.dataset.confirmMsg || 'Are you sure?';
  // The partial-containment warning, if the row carries one: producers of
  // this part whose claims route no containment keep shipping to FG. The
  // floor needs that sentence BEFORE they believe the part is held, not
  // after a bin escapes.
  if (el.dataset.unrouted) {
    msg += ' WARNING: ' + el.dataset.unrouted +
      ' also produce this part with no containment route — their FG deliveries keep flowing until their holds are enabled.';
  }
  // A form declaring data-confirm-input gets the confirm dialog WITH a text
  // field inside it, and the typed value rides the POST as `reason` — set
  // unconditionally: the form itself no longer carries a reason input (it
  // lives in the dialog now), so a has() guard would never fire and the
  // typed value would be silently dropped.
  if (el.dataset.confirmInput !== undefined) {
    var answer = await uiConfirmWithInput(msg, { placeholder: el.dataset.confirmInput || 'Reason (optional)' });
    if (!answer.ok) return;
    data.set('reason', answer.value);
  } else {
    if (!await uiConfirm(msg)) return;
  }
  try {
    var res = await fetch(el.action, { method: 'POST', body: new URLSearchParams(data) });
    if (!res.ok) {
      var text = await res.text();
      var parsed = null;
      try { parsed = JSON.parse(text); } catch (e) { parsed = null; }
      toast((parsed && parsed.error) || text || ('HTTP ' + res.status), 'error');
      return;
    }
    window.location.reload();
  } catch (e) {
    toast('Request failed: ' + e, 'error');
  }
}

// Register the shared submit-gate handler on document.body once, here in the
// always-loaded module, so every page's data-action-submit="confirmDeleteForm"
// delete form is actually guarded. delegateActions merges per (root, event), so
// page modules still add their own handlers on top. Previously each page had to
// remember to register confirmDeleteForm and most didn't, so destructive delete
// forms (payloads, bin-types) submitted with no confirmation at all.
delegateActions(document.body, { confirmDeleteForm, containmentForm }, { events: ['submit'] });

// All exports above are declared inline (`export function …`). The
// trailing `export { … }` block that used to live here was removed in
// SPRINT 4 — every helper is now a top-level named export.
