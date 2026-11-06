// station-card.js — a station counter's card owns its positions: one box per
// position of the process, each greyed with its refusal when the binding's
// rules refuse it (the dry run, GET /api/process-nodes/{id}/countable). The
// boxes start as the bindings stand, and when this is the process's only
// station counter its countable unbound positions start ticked. Nothing binds
// until the card's save, which sends one PUT …/counted-at per box that differs
// from its binding. A classic script, so its Node test loads it.
(function () {
    'use strict';

    // rows(counterID, counterCount, positions, verdicts): one row per position,
    // { id, name, at, checked, disabled, refusal }. at is the station counter
    // the position is counted at (0: the production counter); verdicts holds
    // each position's dry run, { countable, refusal } — a position with none
    // yet is greyed.
    function rows(counterID, counterCount, positions, verdicts) {
        const v = verdicts || {};
        return (positions || []).map(function (n) {
            const at = n.line_counter_id || 0;
            const verdict = v[n.id];
            const countable = !!(verdict && verdict.countable);
            return {
                id: n.id,
                name: n.name,
                at: at,
                checked: at === counterID || (countable && at === 0 && counterCount === 1),
                disabled: !countable,
                refusal: countable ? '' : ((verdict && verdict.refusal) || 'not checked yet'),
            };
        });
    }

    // changes(counterID, positions, verdicts, ticked): the bindings the save
    // sends, { id, line_counter_id }, for each countable position whose box
    // differs from its binding: ticked and not counted here binds here;
    // unticked and counted here goes back to the production counter. A box
    // left unticked at a position another counter counts leaves it there.
    function changes(counterID, positions, verdicts, ticked) {
        const v = verdicts || {};
        const t = ticked || {};
        const out = [];
        (positions || []).forEach(function (n) {
            if (!(v[n.id] && v[n.id].countable)) return;
            const here = (n.line_counter_id || 0) === counterID;
            if (t[n.id] && !here) out.push({ id: n.id, line_counter_id: counterID });
            else if (!t[n.id] && here) out.push({ id: n.id, line_counter_id: null });
        });
        return out;
    }

    // when(iso): a time as the card shows it, local, to the minute.
    function when(iso) {
        const d = new Date(iso);
        if (isNaN(d.getTime())) return String(iso || '');
        const p = n => String(n).padStart(2, '0');
        return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' + p(d.getHours()) + ':' + p(d.getMinutes());
    }

    // measures(c, fmt): what the card measures, each said as measured: the
    // clicks per part (the ratio), the parts in the line since the last
    // program change beside it (the lead: 0 ahead is a counter at the end of
    // the line, not at the load), and the deepest run-out. c is a station
    // counter as the list sends it (in_line, anchor_at, ratio,
    // ratio_intervals, deepest_runout); fmt formats a time (when).
    function measures(c, fmt) {
        const f = fmt || when;
        const ratio = c.ratio == null ? 'clicks per part: not measured yet'
            : '≈' + Number(c.ratio).toFixed(1) + ' clicks per part over ' + c.ratio_intervals + ' strokes';
        let lead;
        if (c.in_line == null) {
            lead = 'not anchored yet: the next program change sets it';
        } else if (c.in_line === 0) {
            lead = '0 ahead since the program change at ' + f(c.anchor_at) + ': counts like the production counter';
        } else {
            lead = '≈' + c.in_line + ' in the line since the program change at ' + f(c.anchor_at) + ' (measured)';
        }
        const deepest = 'deepest run-out seen: ' + (c.deepest_runout || 0);
        return { ratio: ratio, lead: lead, deepest: deepest };
    }

    // inLineSentence(c): the binding confirm's words on the parts in the line
    // at c.
    function inLineSentence(c) {
        if (c && c.in_line != null) {
            return '≈' + c.in_line + ' in the line; they net against this cart’s run-out under the borrowing rule.';
        }
        return 'The parts in the line (not anchored yet: the next program change measures them) net against ' +
            'this cart’s run-out under the borrowing rule.';
    }

    const api = { rows, changes, measures, inLineSentence };
    if (typeof module !== 'undefined' && module.exports) module.exports = api;
    if (typeof window !== 'undefined') window.StationCard = api;
})();
