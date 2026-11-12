// app.js — Core admin's own page verbs, loaded by layout.html on every admin
// page. The kiosk displays (dashboard display, map, node report, heartbeat)
// have their own templates and do not load it.
//
// ES module. What every Core page shares with the Edge lives in shared/, and a
// page imports each helper from the file that defines it (style guide §17):
// escapeHtml, h``, el, delegateActions, debounce from dom.js; the clock from
// clock.js; the modals, confirm and prompt from layer.js (Core's pages call
// confirm and prompt uiConfirm and uiPrompt); toast from notice.js; the live
// bus from bus.js. This file re-exports none of them.
//
// What is left here is Core's own: the door verbs Core's pages were written
// to (apiGet / apiPost / apiPut / apiDelete), a few page verbs (timeAgoHTML,
// setText, openFromQuery, enterSubmits, the submit gates), and what every
// admin page does at load: table sort, the bus subscription and its
// new-build banner, the submit gate on document.body.

import { delegateActions, escapeHtml } from '/static/shared/dom.js';
import { formatTime, timeAgo } from '/static/shared/clock.js';
import { installTableSort } from '/static/shared/list.js';
import { onSSE } from '/static/shared/bus.js';
import { confirm as uiConfirm, confirmWithInput as uiConfirmWithInput } from '/static/shared/layer.js';
import { banner, toast } from '/static/shared/notice.js';
import { send } from '/static/shared/http.js';
document.addEventListener('DOMContentLoaded', function() { installTableSort(); });

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
function apiRequest(method, url, body) {
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
export function apiGet(url)        { return apiRequest('GET', url); }
export function apiPost(url, body) { return apiRequest('POST', url, body || {}); }
export function apiPut(url, body)  { return apiRequest('PUT',  url, body || {}); }
export function apiDelete(url)     { return apiRequest('DELETE', url); }

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

// ─── SSE bus: the subscription every page has ───────────────────────────
//
// The shared onSSE bus (shared/bus.js) maintains ONE EventSource per tab,
// handles reconnect/backoff, and dispatches 'build-changed' on a Core
// redeploy, which the subscription below turns into the new-build banner — so
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
// root (shared/dom.js, '__delegateActionsMap_' + sentinel; the click
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
