// Shingo Edge — the Edge admin hub: what every admin page does on load, and
// the Edge's own page verbs.
//
// ES module, loaded by header.html on every Edge admin page before the page's
// own module. A page imports each shared helper from the file that defines it
// (/static/shared/dom.js, layer.js, notice.js, bus.js, …); this module exports
// only what is the Edge's: api, refreshOnEvents, the process selector verbs,
// and the Processes page's modal pair until it moves.
//
// On load it:
//   1. localizes <time data-utc=…> after every htmx swap, and once on
//      DOMContentLoaded for the first render (clock.js);
//   2. arms the live elapsed/countdown spans, re-armed after swaps;
//   3. sorts the tables marked for it (list.js);
//   4. puts every failed htmx request on the notice (shared/http.js's
//      hookHtmxErrors): a partial refresh or an hx-post action that is
//      refused, or never answered, used to keep the old content and say
//      nothing;
//   5. shows the new-build banner on the bus's build-changed, and the
//      lost-connection banner from its disconnected to its next connected.
//      Those are the bus's own frames, which open no stream (shared/bus.js):
//      a page that holds a stream for its own topics shows the banners on
//      it, and a page that holds none still holds none.
//
// window.ShingoEdge carries toast for the one classic consumer,
// templates/diagnostics.html's inline script.

import {
    clearLostConnectionBanner, showLostConnectionBanner, showNewBuildBanner, toast,
} from '/static/shared/notice.js';
import {
    installHtmxLiveDurations,
    installHtmxTimestampConversion,
    installLiveDurations,
    convertTimestamps,
} from '/static/shared/clock.js';
import { installTableSort } from '/static/shared/list.js';
import { hookHtmxErrors, http } from '/static/shared/http.js';
import { onSSE, sseStatus } from '/static/shared/bus.js';
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
onSSE('build-changed', showNewBuildBanner);
onSSE('disconnected', function() { showLostConnectionBanner('edge', sseStatus().lastFrameAt); });
onSSE('connected', clearLostConnectionBanner);

// --- the live bus on an Edge admin page ---
// A page that holds a stream subscribes its server topics on shared/bus.js's
// one bus, in its own module. The bus opens its stream on the microtask after
// a burst of subscriptions; this module runs before the page's, so a server
// topic subscribed here would open the stream before the page's topics were
// known, and the page's would reopen it. The bus's own frames open nothing,
// so the banners above are subscribed here, once. Their words are
// notice.js's (the same on Core and the station): an admin page shows the
// new-build banner and the person picks the moment (ruling 4; a kiosk
// reloads).

// refreshOnEvents fires an htmx partial's "sse:<type>" triggers from the
// page's one bus stream: each sse: name in the element's hx-trigger becomes a
// subscription, through the page's own onSSE, that calls htmx.trigger(the
// element, 'sse:<type>', data). The hx-trigger keeps its list, throttles and
// from: as the template writes them. htmx-sse.js did this from a second
// stream it opened on the element. The page calls it, with its onSSE, beside
// its other subscriptions, so the stream opens once with all of the page's
// topics. The element is looked up again on each frame (a page that dropped
// it does nothing). It returns how many types it subscribed.
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

// --- window.ShingoEdge for the one classic consumer ---
// templates/diagnostics.html's inline script calls window.ShingoEdge.toast;
// it is the notice's toast. When that script moves into diagnostics.js,
// this line goes.
window.ShingoEdge = { toast: toast };
