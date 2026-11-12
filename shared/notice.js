// notice.js — the one toast and the one banner (style guide §11, "The
// notice"). Both draw into one fixed region at the top of the page, made on
// first use: the banners across it, the toasts stacked below them on the
// right, so a toast never covers a banner's button.
//
// TOAST LIFETIMES are the ones the three copies agreed on (the P0 helper
// pins): an error 5 s, every other level 3.2 s, and {sticky:true} until it is
// clicked. {sticky:false} is the level's own lifetime. utils.js gave an error
// with {sticky:false} 3.2 s, because its test read "not explicitly false"
// where it meant "not sticky"; here sticky is the only switch.
//
// ONE OF EACH. A toast whose words and level are already up is not stacked a
// second time: the old one goes and the new one starts its lifetime again. A
// page whose background reads fail every ten seconds says so once, not once
// per read. A STICKY ONE STAYS: the same words while a sticky toast says them
// add nothing, so a page's own "Signed out" never replaces the door's, and its
// Sign-in link stays on screen.
//
// THE BANNER is a line across the top of the page, one per key, that stays
// until it is cleared or dismissed. banner() carries no words of its own.
//
// THE TWO BANNERS EVERY SURFACE SHOWS are worded here, once, for Core, Edge
// admin and the station (S5): "A new version is available" with Refresh
// (showNewBuildBanner, on the bus's build-changed; admin pages only, ruling
// 4: a kiosk reloads instead), and "Lost connection to <Core|this Edge> ·
// last answered <time>" (showLostConnectionBanner on the bus's disconnected,
// clearLostConnectionBanner on its connected). They sit under two keys, and
// both can show at once.
//
// NOTHING HAPPENS AT IMPORT: the region is made by the first toast or banner.

import { el } from '/static/shared/dom.js';
import { formatClock, formatTime, serverNow, simSpeed } from '/static/shared/clock.js';

const LEVELS = ['info', 'success', 'warning', 'error'];
const LIFETIME_MS = { error: 5000 };
const DEFAULT_LIFETIME_MS = 3200;

let region = null;
const banners = new Map();
const live = new WeakMap();   // toast element -> { text, level, timer }

function parts() {
    if (!region || !region.isConnected) {
        region = {
            root: el('div', { className: 'notice-root' }),
            banners: el('div', { className: 'notice-banners' }),
            stack: el('div', { className: 'notice-stack', role: 'status', 'aria-live': 'polite' }),
            get isConnected() { return this.root.isConnected; },
        };
        region.root.appendChild(region.banners);
        region.root.appendChild(region.stack);
        document.body.appendChild(region.root);
    }
    return region;
}

// words is the text a message shows: an Error's message, a refusal body's
// error, a string as itself, nothing for null.
function words(m) {
    if (m === null || m === undefined) return '';
    if (typeof m === 'object') {
        if (typeof m.message === 'string') return m.message;
        if (typeof m.error === 'string') return m.error;
    }
    return String(m);
}

// action is a toast's or banner's one control: {label, href} is a link (the
// sign-in link the HTTP door offers), {label, onClick} a button.
function action(a) {
    if (!a || !a.label) return null;
    if (a.href) return el('a', { className: 'notice-action', href: a.href }, a.label);
    return el('button', { className: 'notice-action', type: 'button', onclick: a.onClick || null }, a.label);
}

// toast shows one message and returns its element.
//   level  'info' (default), 'success', 'warning', 'error'
//   opts   sticky (stays until clicked), action ({label, href} or
//          {label, onClick})
export function toast(message, level, opts) {
    const o = opts || {};
    const lvl = LEVELS.indexOf(level) >= 0 ? level : 'info';
    const text = words(message);
    const { stack } = parts();
    for (const old of Array.prototype.slice.call(stack.children)) {
        const was = live.get(old);
        if (!was || was.text !== text || was.level !== lvl) continue;
        // A sticky one already up says it, and stays with its action (the
        // door's Sign-in link): the same words again add nothing.
        if (was.sticky) return old;
        drop(old);
    }
    const t = el('div', {
        className: 'notice-toast notice-toast--' + lvl + (o.sticky ? ' is-sticky' : ''),
        role: lvl === 'error' ? 'alert' : null,
        title: o.sticky ? 'Click to dismiss' : null,
    }, [el('span', { className: 'notice-text' }, text), action(o.action)]);
    const note = { text, level: lvl, timer: null, sticky: !!o.sticky };
    live.set(t, note);
    if (o.sticky) t.addEventListener('click', () => drop(t));
    else note.timer = setTimeout(() => drop(t), LIFETIME_MS[lvl] || DEFAULT_LIFETIME_MS);
    stack.appendChild(t);
    return t;
}

function drop(t) {
    const note = live.get(t);
    if (note && note.timer !== null) clearTimeout(note.timer);
    live.delete(t);
    t.remove();
}

// banner shows, or redraws in place, the banner under key, and returns its
// element. opts:
//   text       what it says
//   level      'info' (default), 'warning', 'error'
//   action     {label, onClick} or {label, href}: Refresh, say
//   dismiss    false: no × (a banner that clears itself, like a lost
//              connection's on reconnect); default true
//   onDismiss  after its × removed it
export function banner(key, opts) {
    const o = opts || {};
    let b = banners.get(key);
    if (!b || !b.isConnected) {
        b = el('div', { className: 'notice-banner', role: 'status', 'data-banner': String(key) });
        parts().banners.appendChild(b);
        banners.set(key, b);
    }
    b.className = 'notice-banner notice-banner--' + (LEVELS.indexOf(o.level) >= 0 ? o.level : 'info');
    b.textContent = '';
    b.appendChild(el('span', { className: 'notice-text' }, words(o.text)));
    const act = action(o.action);
    if (act) b.appendChild(act);
    if (o.dismiss !== false) {
        b.appendChild(el('button', {
            className: 'notice-dismiss', type: 'button', 'aria-label': 'Dismiss',
            onclick: () => { clearBanner(key); if (o.onDismiss) o.onDismiss(); },
        }, '×'));
    }
    return b;
}

// clearBanner takes the banner under key down; no banner is no error.
export function clearBanner(key) {
    const b = banners.get(key);
    if (!b) return;
    banners.delete(key);
    b.remove();
}

// ─── the build and the link ─────────────────────────────────────────────────

// showNewBuildBanner says the server runs a new build, with Refresh. The bus
// dispatches build-changed on every frame that carries the new build, so a
// banner that is up is left as it is: redrawing it would take the focus off
// its Refresh. Dismissed, it comes back on the next such frame.
export function showNewBuildBanner() {
    const up = banners.get('build');
    if (up && up.isConnected) return up;
    return banner('build', {
        text: 'A new version is available',
        action: { label: 'Refresh', onClick: () => location.reload() },
    });
}

const SERVER_NAMES = { core: 'Core', edge: 'this Edge' };

// showLostConnectionBanner says the page's stream to server ('core' or
// 'edge') dropped, and when it last answered: lastFrameAt is the browser time
// of the stream's last frame (bus.js sseStatus), null when none came. The bus
// says disconnected again on every reconnect attempt that fails, and each
// redraws the words, so the banner needs no timer. It has no ×: the next
// connect takes it down.
export function showLostConnectionBanner(server, lastFrameAt) {
    let text = 'Lost connection to ' + (SERVER_NAMES[server] || 'the server');
    if (typeof lastFrameAt === 'number' && isFinite(lastFrameAt)) text += ' · last answered ' + answeredAt(lastFrameAt);
    return banner('connection', { text, level: 'warning', dismiss: false });
}

// clearLostConnectionBanner takes it down: the stream is back.
export function clearLostConnectionBanner() { clearBanner('connection'); }

// answeredAt is the plant-local time of a browser instant, read on the
// server's clock as the page's other times are (clock.js serverNow): the time
// of day, or the date and time once it is 12 hours or more ago.
function answeredAt(wallMs) {
    const ago = Math.max(0, Date.now() - wallMs);
    const at = serverNow() - ago * simSpeed();
    return ago >= 12 * 3600 * 1000 ? formatTime(at) : formatClock(at);
}
