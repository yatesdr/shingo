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

import { delegateActions, installBackdropClose, installTableSort, showRefreshBanner } from '/static/shared/utils.js';
installBackdropClose();
document.addEventListener('DOMContentLoaded', function() { installTableSort(); });
export { delegateActions };

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

// HTML escape (replaces per-page esc/escapeHtml)
export function escapeHtml(s) {
  if (s === null || s === undefined || s === '') return '';
  var d = document.createElement('div');
  d.appendChild(document.createTextNode(s));
  return d.innerHTML;
}

// Tagged-template HTML builder. Static parts pass through; interpolations
// are escaped, arrays joined. Returns a string suitable for innerHTML.
//
//   container.innerHTML = h`<div class="x">${name}</div>${rows.map(r => h`<p>${r}</p>`)}`;
//
// Nested h`` *outside* an array (e.g. `${cond ? h`...` : ''}`) returns a
// string that the outer h`` will re-escape — wrap with the __html opt-out:
// `${cond ? {__html:true, value: h`...`} : ''}`. Arrays of h`` results
// are joined unescaped and need no wrap.
export function h(strings) {
  var out = strings[0];
  for (var i = 1; i < arguments.length; i++) {
    var v = arguments[i];
    if (Array.isArray(v)) {
      out += v.join('');
    } else if (v === null || v === undefined || v === false) {
      // skip — lets `${cond && h`...`}` work
    } else if (typeof v === 'object' && v.__html === true) {
      out += v.value; // pre-built safe HTML opt-out
    } else {
      out += escapeHtml(String(v));
    }
    out += strings[i];
  }
  return out;
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

// Generic modal show/hide
export function showModal(id) {
  document.getElementById(id).classList.add('active');
}
export function hideModal(id) {
  document.getElementById(id).classList.remove('active');
}

// --- Dialog helpers (Promise-based, replace native alert/confirm/prompt) ---
//
// Mirrors the shingoedge.js / shared/utils.js shape so page scripts on
// either surface call the same names. Migration rule: every page that
// uses native confirm()/prompt()/alert() switches to these when the page
// JS is touched.

export function uiConfirm(message) {
  return new Promise(function(resolve) {
    var overlay = document.createElement('div');
    overlay.className = 'modal-overlay active confirm-overlay';
    var box = document.createElement('div');
    box.className = 'modal confirm-box';
    box.style.maxWidth = '420px';
    var p = document.createElement('p');
    p.textContent = message;
    p.style.margin = '0 0 1rem';
    box.appendChild(p);
    var row = document.createElement('div');
    row.style.cssText = 'display:flex;gap:0.5rem;justify-content:flex-end';
    var cancelBtn = document.createElement('button');
    cancelBtn.className = 'btn';
    cancelBtn.textContent = 'Cancel';
    cancelBtn.onclick = function() { overlay.remove(); resolve(false); };
    var okBtn = document.createElement('button');
    okBtn.className = 'btn btn-danger';
    okBtn.textContent = 'Confirm';
    okBtn.onclick = function() { overlay.remove(); resolve(true); };
    row.appendChild(cancelBtn); row.appendChild(okBtn);
    box.appendChild(row);
    overlay.appendChild(box);
    document.body.appendChild(overlay);
  });
}

export function uiPrompt(message, opts) {
  opts = opts || {};
  return new Promise(function(resolve) {
    var overlay = document.createElement('div');
    overlay.className = 'modal-overlay active confirm-overlay';
    var box = document.createElement('div');
    box.className = 'modal confirm-box';
    box.style.maxWidth = '420px';
    var p = document.createElement('p');
    p.textContent = message;
    p.style.margin = '0 0 0.75rem';
    box.appendChild(p);
    var input = document.createElement('input');
    input.className = 'form-input';
    input.type = opts.type === 'number' ? 'number' : 'text';
    if (opts.value !== undefined) input.value = String(opts.value);
    if (opts.min !== undefined) input.min = String(opts.min);
    if (opts.max !== undefined) input.max = String(opts.max);
    input.style.marginBottom = '1rem';
    box.appendChild(input);
    var done = function(v) { overlay.remove(); resolve(v); };
    var row = document.createElement('div');
    row.style.cssText = 'display:flex;gap:0.5rem;justify-content:flex-end';
    var cancelBtn = document.createElement('button');
    cancelBtn.className = 'btn';
    cancelBtn.textContent = 'Cancel';
    cancelBtn.onclick = function() { done(null); };
    var okBtn = document.createElement('button');
    okBtn.className = 'btn btn-primary';
    okBtn.textContent = 'OK';
    okBtn.onclick = function() { done(input.value); };
    input.addEventListener('keydown', function(e) {
      if (e.key === 'Enter') done(input.value);
      else if (e.key === 'Escape') done(null);
    });
    row.appendChild(cancelBtn); row.appendChild(okBtn);
    box.appendChild(row);
    overlay.appendChild(box);
    document.body.appendChild(overlay);
    setTimeout(function() { input.focus(); }, 0);
  });
}

export function toast(message, level, opts) {
  level = level || 'info';
  opts = opts || {};
  var container = document.getElementById('toast-container');
  if (!container) {
    container = document.createElement('div');
    container.id = 'toast-container';
    container.style.cssText = 'position:fixed;top:1rem;right:1rem;display:flex;flex-direction:column;gap:0.5rem;z-index:10000;pointer-events:none';
    document.body.appendChild(container);
  }
  var t = document.createElement('div');
  t.className = 'toast toast-' + level;
  t.textContent = message;
  var stripe = { success: 'var(--success)', error: 'var(--danger)', warning: 'var(--warning)', info: 'var(--info)' }[level] || 'var(--info)';
  t.style.cssText = 'padding:0.6rem 1rem;border-radius:var(--radius);background:var(--surface);color:var(--text);border:1px solid var(--border);border-left:4px solid ' + stripe + ';box-shadow:var(--shadow-md);pointer-events:auto;min-width:12rem;max-width:24rem;font-size:0.9rem';
  container.appendChild(t);
  if (!opts.sticky) {
    var dur = (level === 'error') ? 5000 : 3200;
    setTimeout(function() { t.remove(); }, dur);
  } else {
    t.style.cursor = 'pointer';
    t.title = 'Click to dismiss';
    t.addEventListener('click', function() { t.remove(); });
  }
  return t;
}

// Generic JSON request. Throws the server error string (or parsed object's
// `error` field) on non-2xx responses; returns parsed JSON on success.
export function api(method, url, body) {
  var opts = { method: method };
  if (body !== undefined && body !== null) {
    opts.headers = { 'Content-Type': 'application/json' };
    opts.body = JSON.stringify(body);
  }
  return fetch(url, opts).then(function(r) {
    if (!r.ok) return r.text().then(function(t) {
      try { throw JSON.parse(t); }
      catch(e) {
        if (typeof e === 'object' && e.error) throw e.error;
        throw t;
      }
    });
    return r.json();
  });
}
export function apiGet(url)        { return api('GET', url); }
export function apiPost(url, body) { return api('POST', url, body || {}); }
export function apiPut(url, body)  { return api('PUT',  url, body || {}); }
export function apiDelete(url)     { return api('DELETE', url); }

// --- Time formatting ---
// timeAgo:        relative ("3m ago"), '-' on falsy.
// formatTime:     local-time string. opts.precision === 'ms' returns
//                 HH:MM:SS.mmm for high-resolution log views.
// formatDuration: human-readable elapsed duration in ms.
export function timeAgo(ts) {
  if (!ts) return '-';
  var d = Date.now() - new Date(ts).getTime();
  if (d < 60000) return 'just now';
  if (d < 3600000) return Math.floor(d / 60000) + 'm ago';
  if (d < 86400000) return Math.floor(d / 3600000) + 'h ago';
  return Math.floor(d / 86400000) + 'd ago';
}

export function formatTime(ts, opts) {
  if (!ts || ts === '0001-01-01T00:00:00Z') return '-';
  var d = new Date(ts);
  if (isNaN(d.getTime())) return ts;
  if (opts && opts.precision === 'ms') {
    return d.toTimeString().slice(0, 8) + '.' + String(d.getMilliseconds()).padStart(3, '0');
  }
  return d.toLocaleString();
}

export function formatDuration(ms) {
  if (!ms || ms <= 0) return '-';
  if (ms < 1000) return ms + 'ms';
  var s = Math.floor(ms / 1000);
  if (s < 60) return s + 's';
  var m = Math.floor(s / 60);
  s = s % 60;
  if (m < 60) return m + 'm ' + s + 's';
  var h = Math.floor(m / 60);
  m = m % 60;
  return h + 'h ' + m + 'm';
}

// Convert UTC timestamps to browser local time
export function convertTimestamps() {
  document.querySelectorAll('time[data-utc]').forEach(function(el) {
    var d = new Date(el.getAttribute('data-utc'));
    if (!isNaN(d)) {
      el.textContent = d.toLocaleString();
    }
  });
}
document.addEventListener('DOMContentLoaded', convertTimestamps);

// SSE connection for live updates
(function() {
  let es;
  let reconnectDelay = 1000;
  // Build id seen on the first 'connected' event after page load.
  // If a later reconnect reports a different id the core has been
  // restarted; force-reload so the tab picks up the new bundle.
  let seenBuild = null;

  // checkBuild captures the first build id and shows a refresh banner
  // on any later mismatch. Shared by the 'connected' (once per
  // reconnect) and 'heartbeat' (every 30s on the existing connection)
  // handlers — the latter catches Core restarts that a reverse proxy
  // held the SSE socket open through, where onerror would otherwise
  // never fire.
  //
  // Pre-fix this called location.reload() automatically. The banner
  // lets the operator pick the moment so mid-action state isn't
  // nuked on every Core deploy.
  function checkBuild(e) {
    var build = '';
    try { build = (JSON.parse(e.data) || {}).build || ''; } catch (_) {}
    if (!build) return;
    if (seenBuild === null) {
      seenBuild = build;
    } else if (seenBuild !== build) {
      showRefreshBanner();
    }
  }

  function connect() {
    es = new EventSource('/events');

    es.addEventListener('connected', function(e) {
      reconnectDelay = 1000;
      checkBuild(e);
    });

    es.addEventListener('heartbeat', checkBuild);

    es.addEventListener('order-update', function(e) {
      // Page-specific handlers can override via window.onOrderUpdate
      if (typeof window.onOrderUpdate === 'function') window.onOrderUpdate(e);
    });

    es.addEventListener('inventory-update', function(e) {
      if (typeof window.onInventoryUpdate === 'function') window.onInventoryUpdate(e);
    });

    es.addEventListener('node-update', function(e) {
      if (typeof window.onNodeUpdate === 'function') window.onNodeUpdate(e);
    });

    es.addEventListener('bin-update', function(e) {
      if (typeof window.onBinUpdate === 'function') window.onBinUpdate(e);
    });

    es.addEventListener('mission-event', function(e) {
      if (typeof window.onMissionEvent === 'function') window.onMissionEvent(e);
    });

    es.addEventListener('system-status', function(e) {
      const data = JSON.parse(e.data);
      if (data.fleet !== undefined) {
        const el = document.getElementById('fleet-status');
        if (el) {
          el.className = 'health ' + (data.fleet === 'connected' ? 'health-ok' : 'health-fail');
        }
      }
      if (data.messaging !== undefined) {
        const el = document.getElementById('msg-status');
        if (el) {
          el.className = 'health ' + (data.messaging === 'connected' ? 'health-ok' : 'health-fail');
        }
      }
      if (data.redis !== undefined) {
        const el = document.getElementById('redis-status');
        if (el) {
          el.className = 'health ' + (data.redis === 'connected' ? 'health-ok' : 'health-fail');
        }
      }
    });

    es.addEventListener('robot-update', function(e) {
      // Page-specific handler installed by pages/robots.js as
      // window.onRobotUpdate. The grid rebuild lives there so it runs in the
      // scope where openRobotModal / filterRobots / currentRobotVehicle exist
      // (matching the onOrderUpdate / onNodeUpdate delegation pattern above).
      if (typeof window.onRobotUpdate === 'function') window.onRobotUpdate(e);
    });

    es.addEventListener('cms-transaction', function(e) {
      if (typeof window.cmsAppendRows === 'function') {
        var txns = JSON.parse(e.data);
        window.cmsAppendRows(txns);
      }
    });

    es.addEventListener('debug-log', function(e) {
      if (typeof window.debugAppendRow === 'function') {
        var entry = JSON.parse(e.data);
        window.debugAppendRow(entry);
      }
    });

    es.addEventListener('fire-alarm', function(e) {
      if (typeof window.onFireAlarmUpdate === 'function') {
        var data = JSON.parse(e.data);
        window.onFireAlarmUpdate(data);
      }
    });

    es.onerror = function() {
      es.close();
      setTimeout(connect, reconnectDelay);
      reconnectDelay = Math.min(reconnectDelay * 2, 10000);
    };
  }

  // Close SSE connection when navigating away so the browser
  // releases the HTTP/1.1 connection slot immediately.
  window.addEventListener('beforeunload', function() {
    if (es) es.close();
  });

  connect();
})();

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

// enterSubmits — used as data-action-keydown="enterSubmits:targetFn"
// on form-input elements that should submit on Enter (and ignore
// other keys). targetFn is the bare name of a window-resolved
// function; this helper calls it after preventDefault.
export function enterSubmits(targetFnName, el, evt) {
  if (!evt || evt.key !== 'Enter') return;
  evt.preventDefault();
  var fn = window[targetFnName];
  if (typeof fn === 'function') fn(el, evt);
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

// Register the shared submit-gate handler on document.body once, here in the
// always-loaded module, so every page's data-action-submit="confirmDeleteForm"
// delete form is actually guarded. delegateActions merges per (root, event), so
// page modules still add their own handlers on top. Previously each page had to
// remember to register confirmDeleteForm and most didn't, so destructive delete
// forms (payloads, bin-types) submitted with no confirmation at all.
delegateActions(document.body, { confirmDeleteForm }, { events: ['submit'] });

// All exports above are declared inline (`export function …`). The
// trailing `export { … }` block that used to live here was removed in
// SPRINT 4 — every helper is now a top-level named export.
