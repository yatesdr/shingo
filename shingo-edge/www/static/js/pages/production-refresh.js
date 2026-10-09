// production-refresh.js — which /events types refresh #production-content,
// fed from production.js's one createSSE('/events') stream.
//
// The content's hx-trigger listens for "sse:<type>" DOM events. htmx's sse
// extension used to fire exactly those (htmx.trigger(elt, 'sse:<type>')) from
// a second EventSource it opened on the element, so the page held two streams
// to /events. Firing the same names from the page's own stream keeps the
// trigger list, its throttles and the refreshes as they were, on one
// connection.
//
// There is no order-completed here: nothing on the Edge sends one.
// EventOrderCompleted goes out as order-update (www/sse.go), which this list
// forwards, so a completion refreshes the page through that.

// PRODUCTION_REFRESH_EVENTS are the /events types the hx-trigger names, each
// as "sse:<type>". production_refresh_test.go holds the two lists equal.
export const PRODUCTION_REFRESH_EVENTS = ['order-update', 'order-failed', 'counter-update'];

// handlerName is createSSE's handler key for an event type:
// order-update -> onOrderUpdate.
export function handlerName(type) {
    return 'on' + type.split('-').map(function(w) { return w.charAt(0).toUpperCase() + w.slice(1); }).join('');
}

// productionStreamHandlers returns createSSE handlers for the page's stream:
// each refresh event calls refresh('sse:<type>', data) and then the page's own
// handler for that type, if any; the page's other handlers pass through.
export function productionStreamHandlers(refresh, own) {
    var out = Object.assign({}, own);
    PRODUCTION_REFRESH_EVENTS.forEach(function(type) {
        var name = handlerName(type);
        var pageFn = own && own[name];
        out[name] = function(data) {
            refresh('sse:' + type, data);
            if (typeof pageFn === 'function') pageFn(data);
        };
    });
    return out;
}
