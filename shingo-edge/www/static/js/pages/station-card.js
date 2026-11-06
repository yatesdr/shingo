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

    const api = { rows, changes };
    if (typeof module !== 'undefined' && module.exports) module.exports = api;
    if (typeof window !== 'undefined') window.StationCard = api;
})();
