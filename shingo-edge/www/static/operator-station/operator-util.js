// Pure helpers — no DOM mutation outside showToast/postAction's container lookups.

export const stationID = parseInt(document.body.dataset.stationId, 10);

export function el(tag, props) {
    const e = document.createElement(tag);
    if (props) Object.assign(e, props);
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

// postAction is the single POST→refresh path. Returns true on 2xx.
// Caller passes its own loadView callback so this module stays free of
// state/view dependencies.
export async function postAction(url, body, loadView) {
    try {
        const res = await fetch(url, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(body || {})
        });
        if (!res.ok) {
            const text = await res.text();
            let msg;
            try { msg = JSON.parse(text).error || text; } catch { msg = text; }
            // chi's default unmatched-route response is a bare "404 page
            // not found". That happens when the URL was built with a
            // missing/zero param (e.g. confirm-delivery/0 from a half-built
            // complex order). Map it to an actionable message instead.
            if (res.status === 404) {
                msg = 'Order not found — refresh and try again';
            }
            showToast(msg, 'error');
            return false;
        }
        if (loadView) await loadView();
        return true;
    } catch (err) {
        console.error('postAction', url, err);
        showToast('Network error', 'error');
        return false;
    }
}
