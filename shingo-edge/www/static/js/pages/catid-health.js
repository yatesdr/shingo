// catid-health.js — the part-identity health strip on the Processes page.
//
// The Edge computes every item from state on each read (engine/catid_health.go),
// so the strip only renders what /api/catid-health (one line per process) and
// /api/processes/{id}/catid-health (its items) answer; the Processes page
// refetches on its 10 s refresh, which is what clears an item whose condition
// ended with no hint (a read recovering, a window expiring). One block per
// process with a red or amber item, toned by its worst one, each of those
// items with the verbs that resolve it: pin an unmapped CATID to a style (the
// suggestion, or a picked one), Charge or Write off a hold, count the slot the
// PLC counts on, start the changeover a wrong part offers (its action). An
// info item needs no one now (a second counter of the same stroke, a hold to
// clear, an incoming style with no CATIDs): the list leaves it out, and the
// process's Settings tab shows it with its verbs (notes). A classic script,
// like desktop-bodies.js, so its Node test loads it as CommonJS.
(function () {
    'use strict';

    function esc(s) {
        return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;')
            .replace(/>/g, '&gt;').replace(/"/g, '&quot;');
    }

    const TONE = { red: 'pd-ch-red', amber: 'pd-ch-amber', info: 'pd-ch-info' };

    // LISTED: the severities the Processes list shows.
    const LISTED = { red: true, amber: true };

    // listed(items): the items the list shows; notes(items): the rest, for the
    // process's own page.
    function listed(items) { return (items || []).filter(it => LISTED[it.severity]); }
    function notes(items) { return (items || []).filter(it => !LISTED[it.severity]); }

    // shows(row): whether the list shows a process's summary row: its worst
    // item is red or amber.
    function shows(row) { return !!(row && LISTED[row.worst]); }

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

    function itemHTML(pid, it) {
        const acts = actionsHTML(pid, it);
        return '<div class="pd-ch-item ' + (TONE[it.severity] || TONE.info) + '"><span>' + esc(it.headline) +
            (it.detail ? ' <span class="pd-dim">' + esc(it.detail) + '</span>' : '') + '</span>' +
            (acts ? '<span class="pd-ch-acts">' + acts + '</span>' : '') + '</div>';
    }

    // listHTML renders the summary rows whose worst item is red or amber, each
    // process with its red and amber items when itemsByProcess carries them
    // (else its worst headline). Nothing to show is no strip.
    function listHTML(rows, itemsByProcess) {
        const shown = Array.isArray(rows) ? rows.filter(shows) : [];
        if (shown.length === 0) return '';
        const items = itemsByProcess || {};
        return '<div class="pd-ch" role="list">' + shown.map(function (r) {
            const its = Array.isArray(items[r.process_id]) ? listed(items[r.process_id]) : null;
            const head = '<b class="pd-ch-name" data-open="' + esc(r.process_id) + '">' + esc(r.name) + '</b>';
            if (!its || its.length === 0) {
                const more = r.count > 1 ? ' <span class="pd-dim">and ' + (r.count - 1) + ' more</span>' : '';
                return '<div class="pd-ch-row ' + (TONE[r.worst] || TONE.info) + '" role="listitem">' + head + ' ' +
                    esc(r.headline) + more + '</div>';
            }
            return '<div class="pd-ch-row ' + (TONE[r.worst] || TONE.info) + '" role="listitem">' + head +
                its.map(it => itemHTML(r.process_id, it)).join('') + '</div>';
        }).join('') + '</div>';
    }

    const api = { listHTML, itemHTML, listed, notes, shows };
    if (typeof module !== 'undefined' && module.exports) module.exports = api;
    if (typeof window !== 'undefined') window.CatidHealth = api;
})();
