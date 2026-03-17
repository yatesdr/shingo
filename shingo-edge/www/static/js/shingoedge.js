// Convert UTC timestamps to browser local time
function convertTimestamps() {
    document.querySelectorAll('time[data-utc]').forEach(function(el) {
        var d = new Date(el.getAttribute('data-utc'));
        if (!isNaN(d)) {
            el.textContent = d.toLocaleString();
        }
    });
}
document.addEventListener('DOMContentLoaded', function() {
    convertTimestamps();
    // Re-convert after htmx swaps inject new content
    document.body.addEventListener('htmx:afterSwap', convertTimestamps);
});

// Shingo Edge — shop-floor materials management UI
(function() {
    'use strict';

    var ShingoEdge = {};

    // --- HTML escaping ---
    ShingoEdge.escapeHtml = function(text) {
        var div = document.createElement('div');
        div.appendChild(document.createTextNode(text));
        return div.innerHTML;
    };

    // --- SSE Factory ---
    ShingoEdge.createSSE = function(url, handlers) {
        var es = null;
        var reconnectDelay = 1000;

        function connect() {
            es = new EventSource(url);

            es.addEventListener('connected', function() {
                reconnectDelay = 1000;
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

        return { close: function() { if (es) es.close(); } };
    };

    // --- Modal helpers ---
    ShingoEdge.showModal = function(id) {
        document.getElementById(id).style.display = '';
    };

    ShingoEdge.hideModal = function(id) {
        var modal = document.getElementById(id);
        modal.style.display = 'none';
        // Clear form inputs on close
        var inputs = modal.querySelectorAll('input, select, textarea');
        for (var i = 0; i < inputs.length; i++) {
            var el = inputs[i];
            if (el.type === 'hidden') continue;
            if (el.type === 'checkbox') { el.checked = false; continue; }
            if (el.tagName === 'SELECT') { el.selectedIndex = 0; continue; }
            el.value = el.defaultValue || '';
        }
    };

    // --- Confirm dialog ---
    ShingoEdge.confirm = function(message) {
        return new Promise(function(resolve) {
            var overlay = document.createElement('div');
            overlay.className = 'confirm-overlay';
            var box = document.createElement('div');
            box.className = 'confirm-box';
            box.innerHTML = '<p>' + ShingoEdge.escapeHtml(message) + '</p>';
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
    };

    // --- Form helpers ---
    ShingoEdge.populateForm = function(formId, data) {
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
    };

    ShingoEdge.getFormData = function(formId) {
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
    };

    // --- DOM row helpers ---
    ShingoEdge.removeRow = function(rowId) {
        var row = document.getElementById(rowId);
        if (row) {
            row.style.opacity = '0';
            row.style.transition = 'opacity 0.3s';
            setTimeout(function() { row.remove(); }, 300);
        }
    };

    ShingoEdge.replaceRowCells = function(rowId, cellData) {
        var row = document.getElementById(rowId);
        if (!row) return;
        for (var key in cellData) {
            var cell = row.querySelector('[data-col="' + key + '"]');
            if (cell) cell.innerHTML = cellData[key];
        }
    };

    // --- API helpers ---
    ShingoEdge.api = {
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
    ShingoEdge.toast = function(message, type) {
        type = type || 'info';
        var container = document.querySelector('.toast-container');
        if (!container) {
            container = document.createElement('div');
            container.className = 'toast-container';
            document.body.appendChild(container);
        }
        var toast = document.createElement('div');
        toast.className = 'toast toast-' + type;
        toast.textContent = message;
        container.appendChild(toast);
        setTimeout(function() {
            toast.style.opacity = '0';
            setTimeout(function() { toast.remove(); }, 300);
        }, 3000);
    };

    // --- Theme management ---
    // Stored: 'light', 'dark', or null (system)
    // Cycle: light -> dark -> system -> light
    function getStoredTheme() {
        return localStorage.getItem('theme');
    }

    function getEffectiveTheme() {
        var stored = getStoredTheme();
        if (stored === 'light' || stored === 'dark') return stored;
        return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
    }

    function applyTheme() {
        var effective = getEffectiveTheme();
        document.documentElement.dataset.theme = effective;
        var btn = document.querySelector('.theme-toggle');
        if (!btn) return;
        var stored = getStoredTheme();
        if (stored === 'dark') {
            btn.textContent = '\u263D';
            btn.title = 'Theme: dark (click for system)';
        } else if (!stored) {
            btn.textContent = '\u25D0';
            btn.title = 'Theme: system (click for light)';
        } else {
            btn.textContent = '\u2600';
            btn.title = 'Theme: light (click for dark)';
        }
    }

    ShingoEdge.toggleTheme = function() {
        var stored = getStoredTheme();
        if (stored === 'light') {
            localStorage.setItem('theme', 'dark');
        } else if (stored === 'dark') {
            localStorage.removeItem('theme');
        } else {
            localStorage.setItem('theme', 'light');
        }
        applyTheme();
    };

    // Auto-init theme
    document.addEventListener('DOMContentLoaded', function() {
        applyTheme();
        window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', function() {
            if (!getStoredTheme()) applyTheme();
        });
    });

    // Export
    window.ShingoEdge = ShingoEdge;
})();
