// catid-health.js — the part-identity health strip on the Processes page.
//
// The Edge computes every item from state on each read (engine/catid_health.go),
// so the strip only renders what /api/catid-health answers and refetches when
// the 'catid-health' SSE hint names a change. One block per process with any
// item, toned by its worst one. A classic script, like desktop-bodies.js, so
// its Node test loads it as CommonJS.
(function () {
    'use strict';

    function esc(s) {
        return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;')
            .replace(/>/g, '&gt;').replace(/"/g, '&quot;');
    }

    const TONE = { red: 'pd-ch-red', amber: 'pd-ch-amber', info: 'pd-ch-info' };

    // listHTML renders /api/catid-health's rows: each process's name, its worst
    // item's headline and how many more it has. Nothing to show is no strip.
    function listHTML(rows) {
        if (!Array.isArray(rows) || rows.length === 0) return '';
        return '<div class="pd-ch" role="list">' + rows.map(function (r) {
            const more = r.count > 1 ? ' <span class="pd-dim">and ' + (r.count - 1) + ' more</span>' : '';
            return '<div class="pd-ch-row ' + (TONE[r.worst] || TONE.info) + '" role="listitem" data-open="' +
                esc(r.process_id) + '"><b>' + esc(r.name) + '</b> ' + esc(r.headline) + more + '</div>';
        }).join('') + '</div>';
    }

    // onHint calls fn on every 'catid-health' hint, over one stream.
    let stream = null;
    function onHint(fn) {
        if (stream || typeof EventSource === 'undefined') return;
        stream = new EventSource('/events?topics=catid-health');
        stream.addEventListener('catid-health', function () { fn(); });
    }

    const api = { listHTML, onHint };
    if (typeof module !== 'undefined' && module.exports) module.exports = api;
    if (typeof window !== 'undefined') window.CatidHealth = api;
})();
