// http.js — the one HTTP door (style guide §17, "The HTTP door"). It never
// throws: every call resolves to
//
//   { ok, status, body, error, fieldErrors }
//
//   ok           the answer was a 2xx, and not a lapsed session's login page
//   status       the HTTP status; 0 when nothing answered (no connection, a
//                timeout); 401 for a lapsed session however the server said it
//   body         the parsed JSON; null for an empty answer (a 204, an empty
//                2xx) or a failure that was not JSON; a 2xx that was not JSON
//                is its text
//   error        '' when ok, else one sentence: the server's own `error` (the
//                Edge's writeError, Core's jsonError, configRefusal,
//                kanbanHeld), else its `message`, else an http.Error text line,
//                else a sentence made from the status
//   fieldErrors  {field: message}: Core's configRefusal `errors`, or a
//                `field_errors` list of {field, message}; {} when none
//
// and, only when the caller's own signal aborted it, aborted: true; and, when
// the door put the failure on the notice itself, shown: true.
//
// A FAILURE IS SHOWN ONCE, HERE. The door puts the error on the notice, so a
// page never has a write that fails with nothing on screen. A caller that
// draws the error itself, beside the field it is about, passes
// {notice: false} for that call. A 401 is the exception: it always goes on
// the notice, which alone carries the Sign in link, and the answer says
// shown: true, so the caller draws nothing more for it.
//
// A LAPSED SESSION IS NOT A SUCCESS (ruling 3). The Edge's admin routes answer
// an expired session with a 303 to /login, fetch follows it, and the page used
// to read the login page's 200 as "saved". A redirect that lands on the login
// path, or HTML where JSON was asked for, is {ok:false, status:401, error:
// 'Signed out'}, and the notice offers the sign-in link back to this page.
//
// EVERY REQUEST HAS A DEADLINE: REQUEST_TIMEOUT_MS, the operator station's
// POST_TIMEOUT_MS (operator-util.js), set well above the slowest legitimate
// answer so it only ends one that is not coming. It is the request's own
// AbortController and its own timer, cleared when the answer is read: nothing
// outlives the request.

import { toast } from '/static/shared/notice.js';

export const REQUEST_TIMEOUT_MS = 30000;
const LOGIN_PATH = '/login';
const SIGNED_OUT = 'Signed out';
const NO_ANSWER = 'No answer from the server';

// send is the door. opts:
//   timeout  ms before the request is given up (default REQUEST_TIMEOUT_MS; 0
//            means none)
//   signal   the caller's AbortSignal (a preview replaced by a newer one); an
//            abort resolves {ok:false, status:0, aborted:true} and shows
//            nothing
//   notice   false: the caller shows the error itself
export async function send(method, url, body, opts) {
    const o = opts || {};
    const init = { method: method || 'GET', headers: { Accept: 'application/json' } };
    if (body !== undefined && body !== null) {
        init.headers['Content-Type'] = 'application/json';
        init.body = JSON.stringify(body);
    }
    const ms = o.timeout === undefined ? REQUEST_TIMEOUT_MS : Number(o.timeout) || 0;
    const ctrl = typeof AbortController === 'function' ? new AbortController() : null;
    let timedOut = false;
    const timer = ctrl && ms > 0 ? setTimeout(() => { timedOut = true; ctrl.abort(); }, ms) : null;
    const caller = o.signal || null;
    const pass = () => { if (ctrl) ctrl.abort(); };
    if (caller) {
        if (caller.aborted) pass(); else caller.addEventListener('abort', pass);
    }
    if (ctrl) init.signal = ctrl.signal;
    let out;
    try {
        const res = await fetch(url, init);
        // The body is read inside the deadline: an answer whose headers came
        // and whose body stalled is as much "no answer" as one that never came.
        const text = await res.text();
        out = readAnswer(res, text);
    } catch (_) {
        out = nothing(caller && caller.aborted && !timedOut ? 'Cancelled'
            : timedOut ? 'No answer in ' + Math.round(ms / 1000) + ' s' : NO_ANSWER);
        if (caller && caller.aborted && !timedOut) out.aborted = true;
    } finally {
        if (timer !== null) clearTimeout(timer);
        if (caller) caller.removeEventListener('abort', pass);
    }
    // A 401 goes on the notice whatever the caller asked: only the notice
    // carries the Sign in link (the wave C ruling), so a page that draws its
    // refusals in place draws nothing more for a shown answer.
    if (!out.ok && !out.aborted && (o.notice !== false || out.status === 401)) {
        say(out);
        out.shown = true;
    }
    return out;
}

// http is the door by verb. A write given no body sends {}, as Core's apiPost
// and utils.js's api.post do, for a handler that decodes one; null sends none
// (send's own rule), as the Processes page's postJSON did.
export const http = {
    get: (url, opts) => send('GET', url, undefined, opts),
    post: (url, body, opts) => send('POST', url, body === undefined ? {} : body, opts),
    put: (url, body, opts) => send('PUT', url, body === undefined ? {} : body, opts),
    patch: (url, body, opts) => send('PATCH', url, body === undefined ? {} : body, opts),
    del: (url, opts) => send('DELETE', url, undefined, opts),
};

// hookHtmxErrors puts htmx's failed requests on the same notice the door
// uses: a 4xx/5xx partial (htmx:responseError), no answer (htmx:sendError) and
// htmx's own timeout (htmx:timeout). It listens on root (default
// document.body), the events bubble to it, and returns the function that
// takes the listeners off. Nothing calls it yet: wave C installs it on the
// Edge admin pages that load htmx.
export function hookHtmxErrors(root) {
    const target = root || document.body;
    const onResponse = (ev) => {
        const x = ev && ev.detail && ev.detail.xhr;
        if (!x) return;
        const ct = typeof x.getResponseHeader === 'function' ? x.getResponseHeader('content-type') : '';
        say(readAnswer({
            status: x.status,
            ok: x.status >= 200 && x.status < 300,
            redirected: false,
            url: x.responseURL || '',
            headers: { get: (k) => (String(k).toLowerCase() === 'content-type' ? ct : null) },
        }, x.responseText || ''));
    };
    const onNoAnswer = () => say(nothing(NO_ANSWER));
    const on = [['htmx:responseError', onResponse], ['htmx:sendError', onNoAnswer], ['htmx:timeout', onNoAnswer]];
    for (const [type, fn] of on) target.addEventListener(type, fn);
    return () => { for (const [type, fn] of on) target.removeEventListener(type, fn); };
}

// ─── reading an answer ──────────────────────────────────────────────────────

function nothing(error) {
    return { ok: false, status: 0, body: null, error, fieldErrors: {} };
}

function header(res, name) {
    const v = res.headers && typeof res.headers.get === 'function' ? res.headers.get(name) : null;
    return v ? String(v) : '';
}

function isLoginURL(url) {
    const path = String(url || '').replace(/^[a-z][a-z0-9+.-]*:\/\/[^/]*/i, '');
    return path === LOGIN_PATH || path.indexOf(LOGIN_PATH + '?') === 0 || path.indexOf(LOGIN_PATH + '#') === 0;
}

function readAnswer(res, text) {
    const status = res.status;
    const html = /text\/html/i.test(header(res, 'content-type')) || /^\s*<(?:!doctype|html)/i.test(text || '');
    if (status === 401 || (res.redirected && isLoginURL(res.url)) || (res.ok && html)) {
        return { ok: false, status: 401, body: null, error: SIGNED_OUT, fieldErrors: {} };
    }
    let body = null;
    let json = false;
    if (text) {
        try { body = JSON.parse(text); json = true; } catch (_) { body = null; }
    }
    if (res.ok) return { ok: true, status, body: json ? body : (text || null), error: '', fieldErrors: {} };
    return {
        ok: false,
        status,
        body: json ? body : null,
        error: sentence(status, json ? body : null, json || html ? '' : text),
        fieldErrors: fieldErrorsOf(json ? body : null),
    };
}

function sentence(status, body, text) {
    const said = (v) => (typeof v === 'string' && v.trim() ? v.trim() : '');
    let s = '';
    if (body && typeof body === 'object') s = said(body.error) || said(body.message);
    else s = said(body);
    if (!s) s = said(text);
    if (s.length > 200) s = s.slice(0, 200) + '…';
    if (s) return s;
    return status >= 500 ? 'Server error (' + status + ')' : 'Refused (' + status + ')';
}

function fieldErrorsOf(body) {
    const out = {};
    if (!body || typeof body !== 'object') return out;
    if (body.errors && typeof body.errors === 'object' && !Array.isArray(body.errors)) {
        for (const k of Object.keys(body.errors)) {
            if (typeof body.errors[k] === 'string') out[k] = body.errors[k];
        }
    }
    if (Array.isArray(body.field_errors)) {
        for (const f of body.field_errors) {
            if (f && f.field && f.severity !== 'warning') out[f.field] = String(f.message || '');
        }
    }
    return out;
}

function say(out) {
    if (out.status === 401) {
        const here = typeof location !== 'undefined' ? location.pathname + (location.search || '') : '/';
        toast(SIGNED_OUT, 'error', { sticky: true, action: { label: 'Sign in', href: LOGIN_PATH + '?next=' + encodeURIComponent(here) } });
        return;
    }
    toast(out.error, 'error');
}
