// station-card.js — the process page's Counting block, and the station
// counter's card in it.
//
// The block is two groups, each line labelled with where its fact comes from:
// the MES slots (each slot's CATID, count and rising or not as the slot watch
// read them: discovered; the slot the process counts on: confirmed), and the
// station counters' cards (the tag discovered until its first read, then
// confirmed; the positions counted there, confirmed by the card's save; the
// clicks per part and a stop: measured). The slots are their own read
// (GET /api/processes/{id}/counting-slots).
//
// A card owns its positions: one box per position of the process, each greyed
// with its refusal when the binding's rules refuse it (the dry run, GET
// /api/process-nodes/{id}/countable). The boxes start as the bindings stand,
// and when this is the process's only station counter its countable unbound
// positions start ticked. Nothing binds until the card's save, which sends one
// PUT …/counted-at per box that differs from its binding. A classic script, so
// its Node test loads it.
(function () {
    'use strict';

    // The applicability rule, said where a station counter is made and bound.
    const RULE = 'A station counter clicks where the part leaves the cart, once per assembly, under every ' +
        'style that uses the positions counted at it. It reads the cell’s own PLC; there is no ratio.';

    // The second tag, said where it is typed.
    const SECOND_TAG = 'A second tag is for a station with one fixture per hand, one hand running at a ' +
        'time: the counter counts the two tags’ clicks added together.';

    // tagsOf(c): a station counter's tags as the card says them, the second
    // after " + " (the counter counts their sum).
    function tagsOf(c) {
        return c.tag_name_2 ? c.tag_name + ' + ' + c.tag_name_2 : String(c.tag_name || '');
    }

    function esc(s) {
        return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;')
            .replace(/>/g, '&gt;').replace(/"/g, '&quot;');
    }

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

    // measures(c): what the card measures, each said as measured: the clicks
    // per part (the ratio) and the strokes since its last click (a stop past
    // the deepest run-out this boot has seen). c is a station counter as the
    // list sends it (ratio, ratio_intervals, strokes_since_click, stopped).
    function measures(c) {
        const ratio = c.ratio == null ? 'clicks per part: not measured yet'
            : '≈' + Number(c.ratio).toFixed(1) + ' clicks per part over ' + c.ratio_intervals + ' strokes';
        const n = c.strokes_since_click || 0;
        const stopped = c.stopped ? 'stopped: no click for ' + n + ' production strokes, more than the deepest run-out seen'
            : 'last click ' + n + ' production strokes ago';
        return { ratio: ratio, stopped: stopped };
    }

    // leaf(tag): a tag's member, after its struct.
    function leaf(tag) {
        const t = String(tag || '');
        return t.slice(t.lastIndexOf('.') + 1);
    }

    // line(src, text, extra): one line of the block, its label first (where
    // the fact comes from), then its words and any control.
    function line(src, text, extra) {
        return '<div class="pd-row" data-src="' + src + '"><span class="pd-tag">' + src + '</span> <span>' +
            esc(text) + '</span>' + (extra || '') + '</div>';
    }

    // hint(text): a line that states no fact, unlabelled.
    function hint(text) {
        return '<div class="pd-dim">' + esc(text) + '</div>';
    }

    // group(id, label, sub, inner): one of the block's three groups.
    function group(id, label, sub, inner) {
        return '<div class="pd-sfld" data-group="' + id + '"><label>' + esc(label) +
            (sub ? '<small>' + esc(sub) + '</small>' : '') + '</label><div class="v col">' + inner + '</div></div>';
    }

    // slotsGroup: the slot the process counts on (confirmed: a person's
    // save), then each slot of its MES struct as the watch last read it.
    function slotsGroup(p, slots, f) {
        if (!p.counter_tag_name) return group('slots', 'MES slots', '', hint('wire the production counter first'));
        const rows = slots.map(s => {
            if (!s.read) return line(s.label, leaf(s.tag) + ' · not read at the last pass');
            const moving = s.rising ? 'rising' : 'not rising' + (s.rose_at ? ', last rose ' + f(s.rose_at) : '');
            return line(s.label, leaf(s.tag) + ' · CATID ' + s.catid + (s.part ? '' : ', no part') + ' · count ' +
                s.count + ' · ' + moving + (s.counted ? ' · counted' : '') +
                (s.not_recorded_since ? ' · strokes since ' + f(s.not_recorded_since) + ' are not in the record' : ''));
        }).join('');
        return group('slots', 'MES slots', 'the counter’s struct, read once a minute',
            line('confirmed', 'counts on ' + leaf(p.counter_tag_name)) +
            (rows || hint('not a MES struct’s Prod_Counter_0N, or not read yet')));
    }

    // positionsHTML: a card's boxes, one per position (rows), a refused one
    // greyed with its sentence, one counted at another counter named so, and
    // the card's save.
    function positionsHTML(c, v) {
        const counters = v.counters || [];
        const other = id => {
            const o = counters.find(x => x.id === id);
            return o ? (o.label || o.tag_name) : 'station counter ' + id;
        };
        const boxes = rows(c.id, counters.length, v.positions || [], v.verdicts).map(r =>
            '<label class="pd-row' + (r.disabled ? ' pd-dim' : '') + '"' + (r.refusal ? ' title="' + esc(r.refusal) + '"' : '') + '>' +
            '<span><input type="checkbox" data-scpos="' + r.id + '" data-sc="' + c.id + '"' + (r.checked ? ' checked' : '') +
            (r.disabled ? ' disabled' : '') + '> ' + esc(r.name) + '</span>' +
            (r.refusal ? '<span class="pd-dim">' + esc(r.refusal) + '</span>'
                : r.at && r.at !== c.id ? '<span class="pd-dim">counted at ' + esc(other(r.at)) + '</span>' : '') +
            '</label>').join('');
        if (!boxes) return '';
        return boxes + '<button class="pd-btn" data-act="sc-positions" data-sc="' + c.id + '">Save positions</button>';
    }

    // cardHTML: a station counter's card: its verbs, its tag (discovered until
    // its first read, then confirmed), the positions counted there
    // (confirmed) with their boxes, and what it measures.
    function cardHTML(c, v) {
        const m = measures(c);
        const here = (v.positions || []).filter(n => n.line_counter_id === c.id).map(n => n.name);
        return '<div class="pd-row"><span>' + esc(c.label || 'station counter ' + c.id) + '</span>' +
            '<span><button class="pd-dimlink" data-act="sc-edit" data-sc="' + c.id + '">Edit</button>' +
            '<button class="pd-dimlink" data-act="sc-delete" data-sc="' + c.id + '">Delete</button></span></div>' +
            line(c.last_poll_at ? 'confirmed' : 'discovered',
                'tag ' + tagsOf(c) + (c.last_poll_at ? ': saved and read' : ': saved, not read yet')) +
            line('confirmed', here.length ? 'counts ' + here.join(', ') : 'counts no position yet: tick them and save') +
            positionsHTML(c, v) + [m.ratio, m.stopped].map(x => line('measured', x)).join('');
    }

    function stationsGroup(v) {
        const p = v.process || {};
        const wired = !!(p.counter_plc_name && p.counter_tag_name);
        return group('stations', 'Station counters', wired ? RULE : 'wire the production counter first; a station counter reads beside it',
            (v.counters || []).map(c => cardHTML(c, v)).join('') +
            (wired ? '<button class="pd-btn" data-act="sc-new">New station counter</button>' : ''));
    }

    // countingHTML(v): the block. v holds the process, slots (the MES slots'
    // read), counters (the station counters' list), positions and verdicts
    // (the cards' boxes), and fmt (a time's format, when by default).
    function countingHTML(v) {
        return '<div class="pd-sect"><h2>Counting</h2><span class="pd-dim">each line says where it comes from: ' +
            'discovered (read from the PLC), confirmed (saved by a person), or measured</span></div>' +
            slotsGroup(v.process || {}, v.slots || [], v.fmt || when) + stationsGroup(v);
    }

    const api = { RULE, SECOND_TAG, tagsOf, rows, changes, measures, countingHTML };
    if (typeof module !== 'undefined' && module.exports) module.exports = api;
    if (typeof window !== 'undefined') window.StationCard = api;
})();
