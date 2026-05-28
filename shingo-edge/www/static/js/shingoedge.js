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
//
// The IIFE wrap that used to populate `var ShingoEdge = {}` was
// dropped in SPRINT 4. Helpers are now top-level `export function` /
// `export const` declarations; the trailing `window.ShingoEdge = { … }`
// block exists only for the two remaining non-module consumers:
//   - traffic.html inline <script> reads ShingoEdge.api / ShingoEdge.toast
//   - operator-station/operator.js reads window.ShingoEdge.createSSE
// Migrate those to module imports and the window-bridge can go.

import {
    installBackdropClose,
    installHtmxTimestampConversion,
    installTableSort,
    convertTimestamps,
} from '/static/shared/utils.js';
installBackdropClose();
installHtmxTimestampConversion();
document.addEventListener('DOMContentLoaded', function() {
    convertTimestamps();
    installTableSort();
});

// --- HTML escaping ---
export function escapeHtml(text) {
    if (text === null || text === undefined || text === '') return '';
    var div = document.createElement('div');
    div.appendChild(document.createTextNode(text));
    return div.innerHTML;
}

// --- SSE Factory ---
export function createSSE(url, handlers) {
    var es = null;
    var reconnectDelay = 1000;
    // Build id seen on the first 'connected' event after page load.
    // If a later reconnect reports a different id the edge has been
    // restarted; force-reload so the tab picks up the new JS bundle
    // (cacheBust on the HTML only fires on fresh page loads, so a
    // long-lived operator-station tab would otherwise keep its
    // stale module graph indefinitely).
    var seenBuild = null;

    function connect() {
        es = new EventSource(url);

        es.addEventListener('connected', function(e) {
            reconnectDelay = 1000;
            var build = '';
            try { build = (JSON.parse(e.data) || {}).build || ''; } catch (_) {}
            if (!build) return;
            if (seenBuild === null) {
                seenBuild = build;
            } else if (seenBuild !== build) {
                location.reload();
            }
        });

        // Map camelCase handler names to kebab-case event types
        // e.g. onInventoryUpdate -> inventory-update
        for (var key in handlers) {
            if (key.indexOf('on') === 0 && typeof handlers[key] === 'function') {
                (function(handlerName, fn) {
                    var eventType = handlerName.substring(2)
                        .replace(/([A-Z])/g, '-$1')
                        .toLowerCase()
                        .substring(1); // remove leading dash
                    es.addEventListener(eventType, function(e) {
                        try {
                            var data = JSON.parse(e.data);
                            fn(data);
                        } catch (err) {
                            console.error('SSE parse error:', err);
                        }
                    });
                })(key, handlers[key]);
            }
        }

        es.onerror = function() {
            es.close();
            setTimeout(connect, reconnectDelay);
            reconnectDelay = Math.min(reconnectDelay * 2, 10000);
        };
    }

    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', connect);
    } else {
        connect();
    }

    // Close on navigation so the browser releases the HTTP/1.1
    // connection slot immediately; otherwise the next page can sit
    // waiting on the previous /events socket to time out.
    window.addEventListener('beforeunload', function() {
        if (es) es.close();
    });

    return { close: function() { if (es) es.close(); } };
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
    if (m) m.classList.add('active');
}

export function hideModal(id, opts) {
    var modal = document.getElementById(id);
    if (!modal) return;
    modal.classList.remove('active');
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

// --- Confirm dialog ---
export function confirm(message) {
    return new Promise(function(resolve) {
        var overlay = document.createElement('div');
        overlay.className = 'confirm-overlay';
        var box = document.createElement('div');
        box.className = 'confirm-box';
        box.innerHTML = '<p>' + escapeHtml(message) + '</p>';
        var cancelBtn = document.createElement('button');
        cancelBtn.className = 'btn';
        cancelBtn.textContent = 'Cancel';
        var confirmBtn = document.createElement('button');
        confirmBtn.className = 'btn btn-danger';
        confirmBtn.textContent = 'Confirm';
        box.appendChild(cancelBtn);
        box.appendChild(confirmBtn);
        overlay.appendChild(box);
        document.body.appendChild(overlay);
        cancelBtn.onclick = function() { overlay.remove(); resolve(false); };
        confirmBtn.onclick = function() { overlay.remove(); resolve(true); };
    });
}

// --- Prompt dialog ---
// Promise-based, styled overlay. Replaces native window.prompt.
// opts.type ('text'|'number'), opts.value (default), opts.min, opts.max.
// Resolves to the entered string, or null on cancel/escape.
export function prompt(message, opts) {
    opts = opts || {};
    return new Promise(function(resolve) {
        var overlay = document.createElement('div');
        overlay.className = 'confirm-overlay';
        var box = document.createElement('div');
        box.className = 'confirm-box';
        box.innerHTML = '<p>' + escapeHtml(message) + '</p>';
        var input = document.createElement('input');
        input.className = 'form-input';
        input.type = opts.type === 'number' ? 'number' : 'text';
        if (opts.value !== undefined) input.value = String(opts.value);
        if (opts.min !== undefined) input.min = String(opts.min);
        if (opts.max !== undefined) input.max = String(opts.max);
        input.style.cssText = 'margin:0.5rem 0 1rem;display:block;width:100%';
        box.appendChild(input);
        var cancelBtn = document.createElement('button');
        cancelBtn.className = 'btn';
        cancelBtn.textContent = 'Cancel';
        var okBtn = document.createElement('button');
        okBtn.className = 'btn btn-primary';
        okBtn.textContent = 'OK';
        box.appendChild(cancelBtn);
        box.appendChild(okBtn);
        overlay.appendChild(box);
        document.body.appendChild(overlay);
        var done = function(v) { overlay.remove(); resolve(v); };
        cancelBtn.onclick = function() { done(null); };
        okBtn.onclick = function() { done(input.value); };
        input.addEventListener('keydown', function(e) {
            if (e.key === 'Enter') done(input.value);
            else if (e.key === 'Escape') done(null);
        });
        setTimeout(function() { input.focus(); }, 0);
    });
}

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

// --- API helpers ---
export const api = {
    get: function(url) {
        return fetch(url).then(handleResponse);
    },
    post: function(url, body) {
        return fetch(url, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(body)
        }).then(handleResponse);
    },
    put: function(url, body) {
        return fetch(url, {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(body)
        }).then(handleResponse);
    },
    del: function(url) {
        return fetch(url, { method: 'DELETE' }).then(handleResponse);
    }
};

function handleResponse(res) {
    if (!res.ok) {
        return res.text().then(function(text) {
            try {
                var obj = JSON.parse(text);
                throw obj.error || text;
            } catch (e) {
                if (typeof e === 'string') throw e;
                throw text;
            }
        });
    }
    return res.json();
}

// --- Toast notifications ---
export function toast(message, type) {
    type = type || 'info';
    var container = document.querySelector('.toast-container');
    if (!container) {
        container = document.createElement('div');
        container.className = 'toast-container';
        document.body.appendChild(container);
    }
    var t = document.createElement('div');
    t.className = 'toast toast-' + type;
    t.textContent = message;
    container.appendChild(t);
    setTimeout(function() {
        t.style.opacity = '0';
        setTimeout(function() { t.remove(); }, 300);
    }, 3000);
}

// --- TagSelect: type-to-filter PLC tag picker ---
// Usage: tagSelect(inputId, plcSelectId)
// When the PLC <select> changes, fetches tags from /api/plcs/all-tags/{plc}
// and shows a filterable dropdown below the input.
export function tagSelect(inputId, plcSelectId) {
    var input = document.getElementById(inputId);
    var plcSelect = document.getElementById(plcSelectId);
    if (!input || !plcSelect) return;

    var wrapper = document.createElement('div');
    wrapper.className = 'tag-select';
    input.parentNode.insertBefore(wrapper, input);
    wrapper.appendChild(input);

    var dropdown = document.createElement('div');
    dropdown.className = 'tag-select-dropdown';
    wrapper.appendChild(dropdown);

    var allTags = [];
    var highlighted = -1;

    function render(filter) {
        var lc = (filter || '').toLowerCase();
        var matches = allTags.filter(function(t) {
            return t.name.toLowerCase().indexOf(lc) >= 0;
        });
        dropdown.innerHTML = '';
        if (matches.length === 0) {
            var empty = document.createElement('div');
            empty.className = 'tag-select-empty';
            empty.textContent = allTags.length === 0 ? 'No tags available from PLC' : 'No matching tags';
            dropdown.appendChild(empty);
        } else {
            matches.forEach(function(tag, idx) {
                var opt = document.createElement('div');
                opt.className = 'tag-select-option';
                opt.innerHTML = escapeHtml(tag.name) +
                    (tag.type ? '<span class="tag-type">' + escapeHtml(tag.type) + '</span>' : '');
                opt.dataset.idx = idx;
                opt.addEventListener('mousedown', function(e) {
                    e.preventDefault();
                    input.value = tag.name;
                    close();
                });
                dropdown.appendChild(opt);
            });
        }
        highlighted = -1;
    }

    function open() {
        render(input.value);
        dropdown.classList.add('open');
    }

    function close() {
        dropdown.classList.remove('open');
        highlighted = -1;
    }

    function fetchTags() {
        var plc = plcSelect.value;
        allTags = [];
        if (!plc) { close(); return; }
        api.get('/api/plcs/all-tags/' + encodeURIComponent(plc))
            .then(function(tags) {
                allTags = (tags || []).sort(function(a, b) {
                    return a.name.localeCompare(b.name);
                });
                if (document.activeElement === input) open();
            })
            .catch(function() {
                allTags = [];
            });
    }

    plcSelect.addEventListener('change', fetchTags);
    input.addEventListener('focus', function() {
        if (allTags.length > 0 || plcSelect.value) open();
    });
    input.addEventListener('input', function() { render(input.value); });
    input.addEventListener('blur', function() {
        setTimeout(close, 150);
    });
    input.addEventListener('keydown', function(e) {
        var items = dropdown.querySelectorAll('.tag-select-option');
        if (e.key === 'ArrowDown') {
            e.preventDefault();
            highlighted = Math.min(highlighted + 1, items.length - 1);
            items.forEach(function(el, i) { el.classList.toggle('highlighted', i === highlighted); });
            if (items[highlighted]) items[highlighted].scrollIntoView({ block: 'nearest' });
        } else if (e.key === 'ArrowUp') {
            e.preventDefault();
            highlighted = Math.max(highlighted - 1, 0);
            items.forEach(function(el, i) { el.classList.toggle('highlighted', i === highlighted); });
            if (items[highlighted]) items[highlighted].scrollIntoView({ block: 'nearest' });
        } else if (e.key === 'Enter' && highlighted >= 0 && items[highlighted]) {
            e.preventDefault();
            input.value = allTags.filter(function(t) {
                return t.name.toLowerCase().indexOf(input.value.toLowerCase()) >= 0;
            })[highlighted].name;
            close();
        } else if (e.key === 'Escape') {
            close();
        }
    });

    // Fetch tags if PLC is already selected on page load
    if (plcSelect.value) fetchTags();
}

// --- Process selector navigation handlers ---
// Templates that emit `<select data-action-change="navigateToProcess">`
// (changeover.html, material.html) or `="navigateToProcessOrOrders"`
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
    window.location = (el && el.value) ? '?process=' + el.value : '/orders';
}

// --- delegateActions: re-exported from shared ---
// Per-page scripts that import `delegateActions` from this module
// pick up the canonical shared implementation. Keeping the re-export
// here means existing `import { delegateActions } from
// '/static/js/shingoedge.js'` sites don't need to change paths.
export { delegateActions } from '/static/shared/utils.js';

// --- window.ShingoEdge for non-module consumers ---
// Two remaining non-module consumers still reach for these as bare
// globals on the window:
//   - traffic.html: inline <script> uses ShingoEdge.api / .toast
//   - operator-station/operator.js: uses window.ShingoEdge.createSSE
// When those two are migrated to module imports, this block (and the
// IIFE-era comment block at the top) can be deleted outright.
window.ShingoEdge = {
    escapeHtml: escapeHtml,
    createSSE: createSSE,
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
    tagSelect: tagSelect,
};
