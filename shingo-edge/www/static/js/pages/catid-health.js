// catid-health.js — a process's part-identity health items, on the Processes
// page.
//
// The Edge computes every item from state on each read (engine/catid_health.go),
// so this only renders what the routes answer. Two places, one renderer each:
//   - the Processes list: one count per process row (countHTML), from the
//     summary alone (/api/catid-health carries each process's red and amber
//     counts), so the list's 10 s refresh is one request. An info item needs
//     no one now and adds nothing to the count;
//   - the process's Settings tab: every item, worst first, with the verbs
//     that resolve it (itemHTML): pin an unmapped CATID to a style (the
//     suggestion, or a picked one), Charge or Write off a hold, count the slot
//     the PLC counts on, start the changeover a wrong part offers (its action).
// An item's detail (a PLC's read error among them) is its help, behind the
// headline, never the row's words. A classic script, like desktop-bodies.js,
// so its Node test loads it as CommonJS.
(function () {
    'use strict';

    function esc(s) {
        return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;')
            .replace(/>/g, '&gt;').replace(/"/g, '&quot;');
    }

    const TONE = { red: 'pd-ch-red', amber: 'pd-ch-amber', info: 'pd-ch-info' };

    // The list's count takes the shared chips' hues: red is an error, amber a
    // caution (05-status-indicators.md, the chip floors).
    const COUNT_TONE = { red: 'chip-err', amber: 'chip-warn' };

    // needing(row): how many of a summary row's items need someone (red and
    // amber); an info item counts in row.count only.
    function needing(row) { return row ? (Number(row.red) || 0) + (Number(row.amber) || 0) : 0; }

    // countHTML is a list row's one count: the process's red and amber items,
    // toned by its worst one, or '' when it has none. It opens the process's
    // Settings, where the items and their verbs are.
    function countHTML(row) {
        const n = needing(row);
        if (!n) return '';
        const tone = Number(row.red) > 0 ? 'red' : 'amber';
        return '<span class="chip ' + COUNT_TONE[tone] + ' pd-hcount" data-act="open-settings" data-process="' +
            esc(row.process_id) + '" title="' + n + ' to look at, on Settings">' + n + '</span>';
    }

    // actionsHTML: the verbs an item carries, as data-act buttons the page's
    // click handler runs.
    function actionsHTML(pid, it) {
        const d = it.data || {};
        const a = it.action;
        if (a && a.kind === 'start_changeover') {
            return '<button class="pd-btn primary" data-act="ch-start" data-pid="' + esc(pid) + '" data-style="' +
                esc(a.to_style_id) + '" data-name="' + esc(a.to_style_name) + '">Start changeover to ' +
                esc(a.to_style_name) + '</button>';
        }
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
        if (it.kind === 'counter_slot_idle') {
            const slot = String(d.tag || '').split('.').pop();
            return '<button class="pd-btn" data-act="ch-repoint" data-pid="' + esc(pid) + '" data-tag="' +
                esc(d.tag) + '">Count ' + esc(slot) + '</button>';
        }
        if (it.kind === 'held') {
            return '<button class="pd-btn" data-act="ch-charge" data-hold="' + esc(d.hold_id) + '">Charge</button>' +
                '<button class="pd-btn" data-act="ch-writeoff" data-hold="' + esc(d.hold_id) + '">Write off</button>';
        }
        return '';
    }

    // itemHTML is one item: its headline, toned by its severity, the detail
    // behind it as its help, and its verbs.
    function itemHTML(pid, it) {
        const acts = actionsHTML(pid, it);
        return '<div class="pd-ch-item ' + (TONE[it.severity] || TONE.info) + '"><span' +
            (it.detail ? ' title="' + esc(it.detail) + '"' : '') + '>' + esc(it.headline) + '</span>' +
            (acts ? '<span class="pd-ch-acts">' + acts + '</span>' : '') + '</div>';
    }

    const api = { countHTML, itemHTML, needing };
    if (typeof module !== 'undefined' && module.exports) module.exports = api;
    if (typeof window !== 'undefined') window.CatidHealth = api;
})();
