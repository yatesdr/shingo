// bus.js — the live bus, shared by Core and Edge: onSSE subscribes a handler
// to an event type over ONE EventSource per tab; setSSEQuery, setSSEWatchdog
// and setSSEReloadOnBuild set it up; closeSSEBus tears it down.
//
// ES module, served at /static/shared/bus.js; a page imports each helper from
// here, so a page has one bus. Nothing runs at import: the stream opens on the
// first subscribe, and the close on tab unload is wired on the first connect.
// It imports nothing, and no UI: a build change is the synthetic
// 'build-changed' it dispatches, and the page that shows the new-build banner
// subscribes to it (Core's app.js), so a kiosk that reloads instead
// (setSSEReloadOnBuild) loads no banner code at all.

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
// NO SERVER TOPIC, NO STREAM. The bus's own frames (connected, disconnected,
// heartbeat, stream-gap, build-changed) are never topics: they come on a
// stream some section opened for a server type. A page that subscribes only
// those opens nothing, and a stream that has lost its last server topic is
// not reopened. A stream with no topics would be the unfiltered one (the
// Edge's parseTopics reads no topics as every type): a hub's build-changed
// subscription on a page that holds no stream used to open exactly that.
//
// On every (re)connect the bus dispatches a synthetic 'connected' event, so
// sections can re-fetch their snapshot and close the gap of events missed
// while disconnected (§13). A build-id mismatch dispatches the synthetic
// 'build-changed' with the frame that carried the new build, or reloads a
// kiosk. handler receives (parsedJSON, rawEvent).
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
const _busStreamTypes = { connected: true, disconnected: true, heartbeat: true, 'stream-gap': true, 'build-changed': true };

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
// the stream is treated as dead. Pick a few heartbeats' worth. 0 disarms, the
// timer already running on the live stream included: left running, it dropped
// the stream once more after the page had turned the watchdog off.
export function setSSEWatchdog(ms) {
    _busWatchdogMs = ms > 0 ? ms : 0;
    if (!_busWatchdogMs && _busWatchdog !== null) { clearTimeout(_busWatchdog); _busWatchdog = null; }
}

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
// 'build-changed' dispatch (admin pages show the refresh banner on it) to an
// immediate location.reload() (kiosk pages — no operator to dismiss; the
// right move is to adopt the new Core build). Applies to mismatches seen on both the connected and heartbeat
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
        _busDispatch('build-changed', e);
    }
}

// _busWireUnload closes the live stream when the tab goes. Wired on the first
// connect rather than at import, so importing the bus does nothing; until a
// connect there is no stream for it to close.
let _busUnloadWired = false;
function _busWireUnload() {
    if (_busUnloadWired || typeof window === 'undefined') return;
    _busUnloadWired = true;
    window.addEventListener('beforeunload', () => { if (_busES) _busES.close(); });
}

function _busConnect() {
    _busWireUnload();
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
    setTimeout(() => { if (!_busClosed && !_busES && _busDesiredTopics()) _busConnect(); }, delay);
}

function _busSync() {
    if (_busSyncScheduled) return;
    _busSyncScheduled = true;
    Promise.resolve().then(() => {
        _busSyncScheduled = false;
        if (_busClosed) return;
        if (!_busDesiredTopics()) return;             // no server topic yet: nothing to open
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
