// catid-health.js — the part-identity health strip on the Processes page.
//
// The Edge computes every item from state on each read (engine/catid_health.go),
// so the strip only renders what /api/catid-health (one line per process) and
// /api/processes/{id}/catid-health (its items) answer, and refetches when the
// 'catid-health' SSE hint names a change. One block per process with any item,
// toned by its worst one, each item with the verbs that resolve it: pin an
// unmapped CATID to a style (the suggestion, or a picked one), Charge or Write
// off a hold. A classic script, like desktop-bodies.js, so its Node test loads
// it as CommonJS.
(function () {
    'use strict';

    function esc(s) {
        return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;')
            .replace(/>/g, '&gt;').replace(/"/g, '&quot;');
    }

    const TONE = { red: 'pd-ch-red', amber: 'pd-ch-amber', info: 'pd-ch-info' };

    // actionsHTML: the verbs an item carries, as data-act buttons the page's
    // click handler runs.
    function actionsHTML(pid, it) {
        const d = it.data || {};
        if (it.kind === 'unmapped_catid') {
            let out = '';
            if (d.suggestion) {
                out += '<button class="pd-btn" data-act="ch-pin" data-pid="' + esc(pid) + '" data-style="' +
                    esc(d.suggestion.style_id) + '" data-name="' + esc(d.suggestion.style_name) + '" data-catid="' +
                    esc(d.catid) + '">Pin to ' +
                    esc(d.suggestion.style_name) + '</button>';
            }
            const opts = (d.styles || []).map(s => '<option value="' + esc(s.id) + '">' + esc(s.name) + '</option>').join('');
            if (opts) {
                out += '<select class="pd-inp" data-ch-pick="' + esc(d.catid) + '">' + opts + '</select>' +
                    '<button class="pd-btn" data-act="ch-pin-pick" data-pid="' + esc(pid) + '" data-catid="' +
                    esc(d.catid) + '">Pin</button>';
            }
            return out;
        }
        if (it.kind === 'held') {
            return '<button class="pd-btn" data-act="ch-charge" data-hold="' + esc(d.hold_id) + '">Charge</button>' +
                '<button class="pd-btn" data-act="ch-writeoff" data-hold="' + esc(d.hold_id) + '">Write off</button>';
        }
        return '';
    }

    function itemHTML(pid, it) {
        const acts = actionsHTML(pid, it);
        return '<div class="pd-ch-item ' + (TONE[it.severity] || TONE.info) + '"><span>' + esc(it.headline) +
            (it.detail ? ' <span class="pd-dim">' + esc(it.detail) + '</span>' : '') + '</span>' +
            (acts ? '<span class="pd-ch-acts">' + acts + '</span>' : '') + '</div>';
    }

    // listHTML renders the summary rows, each process with its items when
    // itemsByProcess carries them (else its worst headline). Nothing to show
    // is no strip.
    function listHTML(rows, itemsByProcess) {
        if (!Array.isArray(rows) || rows.length === 0) return '';
        const items = itemsByProcess || {};
        return '<div class="pd-ch" role="list">' + rows.map(function (r) {
            const its = items[r.process_id];
            const head = '<b class="pd-ch-name" data-open="' + esc(r.process_id) + '">' + esc(r.name) + '</b>';
            if (!Array.isArray(its) || its.length === 0) {
                const more = r.count > 1 ? ' <span class="pd-dim">and ' + (r.count - 1) + ' more</span>' : '';
                return '<div class="pd-ch-row ' + (TONE[r.worst] || TONE.info) + '" role="listitem">' + head + ' ' +
                    esc(r.headline) + more + '</div>';
            }
            return '<div class="pd-ch-row ' + (TONE[r.worst] || TONE.info) + '" role="listitem">' + head +
                its.map(it => itemHTML(r.process_id, it)).join('') + '</div>';
        }).join('') + '</div>';
    }

    // onHint calls fn on every 'catid-health' hint, over one stream.
    let stream = null;
    function onHint(fn) {
        if (stream || typeof EventSource === 'undefined') return;
        stream = new EventSource('/events?topics=catid-health');
        stream.addEventListener('catid-health', function () { fn(); });
    }

    const api = { listHTML, itemHTML, onHint };
    if (typeof module !== 'undefined' && module.exports) module.exports = api;
    if (typeof window !== 'undefined') window.CatidHealth = api;
})();
