// Pure helpers — no DOM mutation outside showToast/postAction's container lookups.

export const stationID = parseInt(document.body.dataset.stationId, 10);

// el(tag, props, children) — DOM builder aligned with the shared
// signature in /static/shared/utils.js. Two-arg callers
// (`el('div', { className, textContent })`) keep working because
// Object.assign covers className/textContent/title/type/value directly;
// className, dataset, style, and on* handler keys get the shared-form
// special handling so future call sites can use the richer prop set
// (`el('button', { className: 'x', onclick: fn }, ['text'])`) without
// a second signature reshuffle.
export function el(tag, props, children) {
    const e = document.createElement(tag);
    if (props) {
        for (const key in props) {
            if (!Object.prototype.hasOwnProperty.call(props, key)) continue;
            const val = props[key];
            if (val === null || val === undefined || val === false) continue;
            if (key === 'dataset' && typeof val === 'object') {
                for (const dk in val) e.dataset[dk] = val[dk];
            } else if (key === 'style' && typeof val === 'object') {
                for (const sk in val) e.style[sk] = val[sk];
            } else if (key.length > 2 && key.indexOf('on') === 0 && typeof val === 'function') {
                e.addEventListener(key.substring(2).toLowerCase(), val);
            } else {
                // className, textContent, type, value, title, etc. land here.
                e[key] = val;
            }
        }
    }
    if (children !== undefined && children !== null) {
        const list = Array.isArray(children) ? children : [children];
        for (let i = 0; i < list.length; i++) {
            const c = list[i];
            if (c === null || c === undefined || c === false) continue;
            e.appendChild(c instanceof Node ? c : document.createTextNode(String(c)));
        }
    }
    return e;
}

export function esc(s) {
    if (!s) return '';
    const d = document.createElement('div');
    d.textContent = s;
    return d.innerHTML;
}

export function fillColor(pct, remaining) {
    if (remaining <= 0) return 'var(--os-red)';
    if (pct < 0.33) return 'var(--os-red)';
    if (pct < 0.66) return 'var(--os-amber)';
    return 'var(--os-green-bright)';
}

// Extracts the operator-facing message from a raw error detail string.
//   1. "rds HTTP NNN: {json}"  → returns json.msg
//   2. "{json}"                → returns json.msg if present
//   3. anything else           → returns the raw string
export function friendlyOrderError(detail) {
    if (!detail) return 'Order failed';
    let s = String(detail);
    const jsonStart = s.indexOf(': {');
    if (jsonStart !== -1 && s.slice(0, jsonStart).indexOf('HTTP') !== -1) {
        s = s.slice(jsonStart + 2);
    }
    const trimmed = s.trim();
    if (trimmed.startsWith('{')) {
        try {
            const parsed = JSON.parse(trimmed);
            if (parsed && typeof parsed.msg === 'string' && parsed.msg.length > 0) {
                return parsed.msg;
            }
        } catch (err) {
            console.error('friendlyOrderError JSON parse', err);
        }
    }
    return s;
}

const toastContainer = document.getElementById('os-toast');

export function showToast(msg, type, opts) {
    opts = opts || {};
    const classes = ['os-toast-msg'];
    if (type) classes.push(type);
    if (opts.sticky) classes.push('sticky');

    const toast = el('div', { className: classes.join(' ') });
    while (toastContainer.children.length >= 3) {
        toastContainer.firstChild.remove();
    }

    if (opts.sticky) {
        const text = el('span', { textContent: msg });
        const close = el('button', {
            className: 'os-toast-close',
            textContent: '\u00D7',
            type: 'button',
        });
        close.addEventListener('click', (e) => {
            e.stopPropagation();
            toast.remove();
        });
        toast.appendChild(text);
        toast.appendChild(close);
    } else {
        toast.textContent = msg;
        setTimeout(() => toast.remove(), 3200);
    }
    toastContainer.appendChild(toast);
    return toast;
}

// showExitToast renders a sticky refusal with an inline recovery button. The
// button runs onAction — which typically takes the exit (e.g. abandon the
// changeover) and retries the original request — then dismisses the toast. The
// × lets the operator ignore the exit and leave the refusal on screen.
export function showExitToast(msg, actionLabel, onAction) {
    const toast = el('div', { className: 'os-toast-msg error sticky' });
    while (toastContainer.children.length >= 3) {
        toastContainer.firstChild.remove();
    }
    const text = el('span', { textContent: msg });
    const action = el('button', {
        className: 'os-toast-action',
        textContent: actionLabel,
        type: 'button',
    });
    action.addEventListener('click', async (e) => {
        e.stopPropagation();
        action.disabled = true;
        try { await onAction(); } finally { toast.remove(); }
    });
    const close = el('button', {
        className: 'os-toast-close',
        textContent: '×',
        type: 'button',
    });
    close.addEventListener('click', (e) => {
        e.stopPropagation();
        toast.remove();
    });
    toast.appendChild(text);
    toast.appendChild(action);
    toast.appendChild(close);
    toastContainer.appendChild(toast);
    return toast;
}

// fetchWithTimeout wraps fetch with an AbortController deadline so a silently
// severed connection (edge restart mid-request, a Wi-Fi blip, a half-open TCP,
// Core unreachable during a reboot) can't leave the promise pending forever.
// That is the class of bug behind the loader "Loading manifest…" / "Loading…"
// hangs AND the stalled live-refresh loop (scheduleRefresh awaits loadView, so a
// hung view fetch freezes every board update until a hard refresh). On timeout it
// rejects with an AbortError; callers handle it like any fetch failure. Default 8s.
export async function fetchWithTimeout(url, opts, ms) {
    const ctrl = new AbortController();
    const timer = setTimeout(function() { ctrl.abort(); }, ms || 8000);
    try {
        return await fetch(url, Object.assign({}, opts, { signal: ctrl.signal }));
    } finally {
        clearTimeout(timer);
    }
}

// postAction is the single POST→refresh path. Returns true on 2xx.
// Caller passes its own loadView callback so this module stays free of
// state/view dependencies.
// postAction posts an operator action and refreshes the view.
//
// opts is ADDITIVE and every existing 3-argument caller is unchanged:
//   opts.onResult(parsedBody)  — called with the decoded success body.
//
// WHY THE RESPONSE AND NOT AN EVENT. The station is event-driven: an action
// returns, the client re-renders from the orders list, and the structured
// response body has gone unread for so long that reaching for it needs a
// reason. This is the reason — the primes-only outcome is the OUTCOME OF THIS
// CLICK, not a fact about the node. Broadcast as an SSE event it would reach
// every station watching that cell and tell operators who pressed nothing to
// "press again when it lands". A per-click answer belongs on the reply to that
// click.
//
// It is also symmetric with what this function already does: it parses the
// body on failure, for `error` and for the inline `exit` action. It only ever
// threw the body away on success.
export async function postAction(url, body, loadView, opts) {
    try {
        const res = await fetch(url, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(body || {})
        });
        if (!res.ok) {
            const text = await res.text();
            let parsed = null;
            try { parsed = JSON.parse(text); } catch { parsed = null; }
            let msg = (parsed && parsed.error) || text;
            // chi's default unmatched-route response is a bare "404 page
            // not found". That happens when the URL was built with a
            // missing/zero param (e.g. confirm-delivery/0 from a half-built
            // complex order). Map it to an actionable message instead.
            if (res.status === 404) {
                msg = 'Order not found — refresh and try again';
            }
            // A refusal may carry an inline exit (e.g. an armed changeover
            // blocking a material request). Offer it as a button: taking it
            // runs the exit action and then re-issues THIS same request, so the
            // operator recovers without a page refresh or restart.
            const exit = parsed && parsed.exit;
            if (exit && exit.url && exit.label) {
                showExitToast(msg, exit.label, async () => {
                    const took = await postAction(exit.url, {}, null);
                    if (took) await postAction(url, body, loadView);
                });
                return false;
            }
            // An ADVISORY refusal is the system working: the request was
            // declined because what it asked for is already under way. Red
            // says "something is broken and you must act", and the only
            // correct action here is to wait — so a notice, and the view
            // still refreshes because the state it reports did change.
            if (parsed && parsed.notice) {
                showToast(msg, 'info');
                if (loadView) await loadView();
                return false;
            }
            showToast(msg, 'error');
            return false;
        }
        if (opts && typeof opts.onResult === 'function') {
            // Read before the refresh so a caller can compare against what it
            // asked for; failures here must not swallow the refresh.
            try {
                opts.onResult(await res.clone().json());
            } catch (e) {
                console.error('postAction onResult', url, e);
            }
        }
        if (loadView) await loadView();
        return true;
    } catch (err) {
        console.error('postAction', url, err);
        showToast('Network error', 'error');
        return false;
    }
}

// formatETA renders an order's ETA as an operator-facing phrase.
//
// Bucket boundaries match the user-approved display rules:
//   < 45s   → "Arriving"
//   45–90s  → "ETA: ~1 min"
//   ≥ 90s   → "ETA: ~N min" rounded to nearest whole minute
//   overdue by > 60s → "Running late" + amber pill
// No sub-minute precision past the first bucket — fake precision was the
// thing Uber's UX research dropped. If the order has no ETA yet (Core
// hasn't stamped one, e.g. mid-transition or backfill pending) we return
// empty:true and the caller shows nothing rather than a placeholder.
//
// Lives here rather than in operator-render.js because both the tile pill
// and the modal's waiting label need it, and a pure formatter is exactly
// what this module is for — the alternative was the modal importing the
// tile renderer for one function.
// primeNoticeText turns a primes-only NodeOrderResult into the sentence the
// operator needs, or '' when the result is an ordinary swap.
//
// A primes-only round is the press-index partial-empty fix doing its job: the
// cell had a bare index position, so the swap that would have wedged was not
// minted and an empty was sent to fill the position instead. Without this the
// operator presses REQUEST SWAP, no swap appears, and nothing says why.
//
// Keyed on "primes and no swap legs", not on cycle_mode: a consume downgrade
// emits primes ALONGSIDE its delivery, and that round did do the thing the
// operator asked for.
export function primeNoticeText(result) {
    if (!result) return '';
    var primes = result.prime_orders || [];
    if (primes.length === 0) return '';
    if (result.order_a || result.order_b) return '';
    var where = primes.map(function(p) {
        var dest = p.delivery_node || 'the index position';
        return p.source_node ? (dest + ' from ' + p.source_node) : dest;
    }).join(' and ');
    return 'Priming ' + where + ' — press again when it lands.';
}

// withQueueCause appends Core's typed queue-cause sentence to a status label.
//
// THE STATUS WORD STAYS. `queued` and `sourcing` are distinct lifecycles and
// are not merged, renamed or collapsed anywhere — the cause is rendered BESIDE
// whatever the surface already said, never instead of it.
//
// The sentence is built once, at set-time, by Core's queue-reason formatter
// from the structured cause (queue_code + queue_cause carry the analytic
// signal; this carries the human one) and pushed to the Edge order row via
// OrderUpdate. Nothing here rebuilds or interprets it — this only puts what is
// already on the row in front of the operator.
//
// It is preferred over the status word wherever both exist because it is a
// whole sentence ("Waiting for material: 74577-6SA0A.06") and because it
// survives the status-write path independently: SetOrderQueueReason bypasses
// the transition validator, so the reason lands even in the window where the
// status push itself was refused.
//
// No cause returns the label untouched — an order parked with nothing said
// about it must not grow a dangling dash.
export function withQueueCause(label, order) {
    const cause = order && order.queue_reason;
    return cause ? label + ' — ' + cause : label;
}

// distinctQueueCauses returns the cause sentences across a set of parked
// orders, in first-seen order, with duplicates dropped. A swap pair parked for
// the same reason has one reason, not two identical lines.
//
// Orders with no cause contribute nothing rather than a blank entry: the count
// line already says how many are parked, and an empty line would read as a
// cause nobody can name.
export function distinctQueueCauses(orders) {
    const out = [];
    (orders || []).forEach(function(o) {
        const cause = o && o.queue_reason;
        if (cause && out.indexOf(cause) === -1) out.push(cause);
    });
    return out;
}

export function formatETA(etaStr) {
    if (!etaStr) return { text: '', overdue: false, empty: true };
    const etaMs = Date.parse(etaStr);
    if (isNaN(etaMs)) return { text: '', overdue: false, empty: true };
    const remainingSec = (etaMs - Date.now()) / 1000;
    const graceSec = 60;
    if (remainingSec < -graceSec) {
        return { text: 'Running late', overdue: true };
    }
    if (remainingSec < 45) {
        return { text: 'Arriving', overdue: false };
    }
    if (remainingSec < 90) {
        return { text: 'ETA: ~1 min', overdue: false };
    }
    const mins = Math.round(remainingSec / 60);
    return { text: 'ETA: ~' + mins + ' min', overdue: false };
}
