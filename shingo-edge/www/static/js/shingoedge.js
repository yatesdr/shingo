// Shingo Edge — shop-floor materials management UI
//
// ES module. Loaded via <script type="module" src="…">. Per-page
// scripts in pages/*.js also load as modules so they execute AFTER
// this file and import the helpers they need as named bindings.
//
// Top-level side effects on Edge admin page load:
//   1. Install the data-backdrop-close listener (shared helper).
//   2. Install the htmx:afterSwap → convertTimestamps wiring so
//      <time data-utc=…> nodes that arrive via partial swap get
//      localized.
//   3. Run convertTimestamps once on DOMContentLoaded for the
//      initial full-page render.
//   4. Put every failed htmx request on the notice (shared/http.js's
//      hookHtmxErrors): a partial refresh or an hx-post action that is
//      refused, or never answered, used to keep the old content and say
//      nothing.
//
// The IIFE wrap that used to populate `var ShingoEdge = {}` was
// dropped in SPRINT 4. Helpers are now top-level `export function` /
// `export const` declarations; the trailing `window.ShingoEdge = { … }`
// block exists only for the one remaining non-module consumer:
//   - templates/diagnostics.html calls window.ShingoEdge.toast
// Migrate it to module imports and the window-bridge can go.

import { escapeHtml } from '/static/shared/dom.js';
import { confirm, prompt } from '/static/shared/layer.js';
import { banner, toast } from '/static/shared/notice.js';
import {
    installHtmxLiveDurations,
    installHtmxTimestampConversion,
    installLiveDurations,
    convertTimestamps,
} from '/static/shared/clock.js';
import { installTableSort } from '/static/shared/list.js';
import { installBackdropClose } from '/static/shared/utils.js';
import { hookHtmxErrors, http } from '/static/shared/http.js';
installBackdropClose();
// Once per page, on the body every htmx event bubbles to. A module runs after
// the document is parsed, so the body is there.
hookHtmxErrors(document.body);
installHtmxTimestampConversion();
// Live elapsed/countdown spans, re-armed after htmx swaps. The interval only
// runs while such nodes exist, so pages without them pay nothing.
installHtmxLiveDurations();
document.addEventListener('DOMContentLoaded', function() {
    convertTimestamps();
    installTableSort();
    installLiveDurations();
});

// --- HTML escaping ---
// The shared one: safe between tags and inside a quoted attribute
// (production.js builds data-part="…" with it). Edge's own DOM-based copy
// left quotes alone.
export { escapeHtml };

// --- the live bus on an Edge admin page ---
// A page that holds a stream subscribes on shared/bus.js's one bus, in its
// own module, beside its own topics. The bus opens its stream on the
// microtask after a burst of subscriptions; this module runs before the
// page's, so a subscription made here would open a stream before the page's
// topics were known (the page's would then reopen it), and open one on a
// page that holds none. So the two verbs below are called by the page.

// showNewBuildBanner is an Edge admin page's answer to the bus's
// build-changed (ruling 4: an admin page shows the banner and the person
// picks the moment; a kiosk reloads). The words and the Refresh are today's
// refresh banner's; S5 sets the final ones. A page subscribes it beside its
// topics: onSSE('build-changed', showNewBuildBanner).
export function showNewBuildBanner() {
    banner('build', {
        text: 'A new version is available.',
        action: { label: 'Refresh', onClick: function() { location.reload(); } },
    });
}

// refreshOnEvents fires an htmx partial's "sse:<type>" triggers from the
// page's one bus stream: each sse: name in the element's hx-trigger becomes a
// subscription, through the page's own onSSE, that calls htmx.trigger(the
// element, 'sse:<type>', data). The hx-trigger keeps its list, throttles and
// from: as the template writes them. htmx-sse.js did this from a second
// stream it opened on the element. The page passes onSSE so that a page with
// no stream does not load the bus for this module's sake. The element is
// looked up again on each frame (a page that dropped it does nothing). It
// returns how many types it subscribed.
export function refreshOnEvents(id, onSSE) {
    var el = document.getElementById(id);
    if (!el) return 0;
    var types = sseTriggerTypes(el);
    types.forEach(function(type) {
        onSSE(type, function(data) {
            var target = document.getElementById(id);
            if (target && window.htmx) window.htmx.trigger(target, 'sse:' + type, data);
        });
    });
    return types.length;
}

// sseTriggerTypes is the event types an element's hx-trigger names as
// "sse:<type>", in its order: "sse:order-update throttle:3s" is order-update.
function sseTriggerTypes(el) {
    return (el.getAttribute('hx-trigger') || '').split(',').map(function(spec) {
        return spec.trim().split(/\s+/)[0];
    }).filter(function(name) {
        return name.indexOf('sse:') === 0 && name.length > 4;
    }).map(function(name) {
        return name.slice(4);
    });
}

// --- Modal helpers ---
// Toggles .active on .modal-overlay (Core's CSS pattern). Matches the
// shared helper in /static/shared/utils.js so per-page scripts can
// call either form interchangeably during the ES module migration.
//
// hideModal default: clear inputs. Pass { preserveState: true } from
// wizard / long edit-flow callers that legitimately need preservation.
export function showModal(id) {
    var m = document.getElementById(id);
    if (m) {
        m.classList.add('active');
        document.body.style.overflow = 'hidden';
    }
}

export function hideModal(id, opts) {
    var modal = document.getElementById(id);
    if (!modal) return;
    modal.classList.remove('active');
    // Restore body scroll only if no other modal is still open.
    if (!document.querySelector('.modal-overlay.active')) {
        document.body.style.overflow = '';
    }
    if (opts && opts.preserveState) return;
    var inputs = modal.querySelectorAll('input, select, textarea');
    for (var i = 0; i < inputs.length; i++) {
        var el = inputs[i];
        if (el.type === 'hidden') continue;
        if (el.type === 'checkbox' || el.type === 'radio') { el.checked = false; continue; }
        if (el.tagName === 'SELECT') { el.selectedIndex = 0; continue; }
        el.value = el.defaultValue || '';
    }
}

// --- confirm, prompt, toast: the layer's and the notice's ---
// The one confirm and prompt (shared/layer.js: Escape, the backdrop and
// every other close resolve them, focus goes in and comes back) and the one
// toast (shared/notice.js). Re-exported here until the pages import them
// from those files.
export { confirm, prompt, toast };

// --- Form helpers ---
export function populateForm(formId, data) {
    var form = document.getElementById(formId);
    if (!form) return;
    for (var key in data) {
        var el = form.querySelector('[name="' + key + '"]');
        if (!el) continue;
        if (el.type === 'checkbox') {
            el.checked = !!data[key];
        } else {
            el.value = data[key];
        }
    }
}

export function getFormData(formId) {
    var form = document.getElementById(formId);
    if (!form) return {};
    var data = {};
    var inputs = form.querySelectorAll('input, select, textarea');
    for (var i = 0; i < inputs.length; i++) {
        var el = inputs[i];
        if (!el.name) continue;
        if (el.type === 'checkbox') {
            data[el.name] = el.checked;
        } else if (el.type === 'number') {
            data[el.name] = parseFloat(el.value) || 0;
        } else {
            data[el.name] = el.value;
        }
    }
    return data;
}

// --- DOM row helpers ---
export function removeRow(rowId) {
    var row = document.getElementById(rowId);
    if (row) {
        row.style.opacity = '0';
        row.style.transition = 'opacity 0.3s';
        setTimeout(function() { row.remove(); }, 300);
    }
}

export function replaceRowCells(rowId, cellData) {
    var row = document.getElementById(rowId);
    if (!row) return;
    for (var key in cellData) {
        var cell = row.querySelector('[data-col="' + key + '"]');
        if (cell) cell.innerHTML = cellData[key];
    }
}

// --- api: the HTTP door, for a page that catches a refusal ---
// The pages call api.get / post / put / patch / del and catch what it
// rejects. Behind the names is shared/http.js's door with its notice off:
// every caller shows the refusal itself, and the door showing it too would
// say it twice. It resolves the answer's body (null for a 204 or an empty
// 2xx, which used to reject with a SyntaxError) and rejects an Error whose
// message is the door's one sentence: the server's own error, else "No
// answer from the server", "Signed out", "Server error (500)" and the rest
// http.js lists. A write given no body sends {}, the door's rule.
const QUIET = { notice: false };
function answered(p) {
    return p.then(function(res) {
        if (res.ok) return res.body;
        throw new Error(res.error);
    });
}
export const api = {
    get: function(url) { return answered(http.get(url, QUIET)); },
    post: function(url, body) { return answered(http.post(url, body, QUIET)); },
    put: function(url, body) { return answered(http.put(url, body, QUIET)); },
    patch: function(url, body) { return answered(http.patch(url, body, QUIET)); },
    del: function(url) { return answered(http.del(url, QUIET)); },
};

// --- Process selector navigation handlers ---
// Templates that emit `<select data-action-change="navigateToProcess">`
// (changeover.html, production.html) or `="navigateToProcessOrOrders"`
// (orders.html) pull these in via their per-page handler map. The
// pre-SPRINT-3 auto-dispatcher resolved them through `window[verb]`;
// after the dispatcher went away these need to be in the page's
// delegateActions map. SPRINT 4 promotes them to module exports.

export function navigateToProcess(el) {
    if (el && el.value) {
        window.location = '?process=' + el.value;
    }
}

export function navigateToProcessOrOrders(el) {
    var params = new URLSearchParams(window.location.search);
    var status = params.get('status');
    var qs = el && el.value ? 'process=' + el.value : '';
    if (status) qs = (qs ? qs + '&' : '') + 'status=' + status;
    window.location = '/orders' + (qs ? '?' + qs : '');
}

// --- delegateActions: re-exported from shared ---
// Per-page scripts that import `delegateActions` from this module
// pick up the canonical shared implementation. Keeping the re-export
// here means existing `import { delegateActions } from
// '/static/js/shingoedge.js'` sites don't need to change paths.
export { delegateActions } from '/static/shared/dom.js';

// --- window.ShingoEdge for non-module consumers ---
// One remaining non-module consumer still reaches for these as bare
// globals on the window:
//   - templates/diagnostics.html: uses window.ShingoEdge.toast
// When it is migrated to module imports, this block (and the
// IIFE-era comment block at the top) can be deleted outright.
window.ShingoEdge = {
    escapeHtml: escapeHtml,
    showModal: showModal,
    hideModal: hideModal,
    confirm: confirm,
    prompt: prompt,
    populateForm: populateForm,
    getFormData: getFormData,
    removeRow: removeRow,
    replaceRowCells: replaceRowCells,
    api: api,
    toast: toast,
};
