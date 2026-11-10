// station-card.js — the Production counter section's live words and its two
// list rows on the process's Settings tab: the MES slots and the station
// counters (docs/ui-style-guide/33-settings-pages.md).
//
// The section title says what the process counts on, in words: the slot, its
// CATID and the style that expects it, and the last stroke the slot watch saw
// (titleHTML). The MES slots row shows the counted slot, each slot that is in
// use on its own line, and folds the unused ones into one line (slotsField);
// the slots are their own read (GET /api/processes/{id}/counting-slots). The
// station counters row is a list: one row per counter with its tags, the
// positions it counts, a one-line status (amber only when it is stopped while
// the line produces, the list's stopped) and its verbs; then + Add.
//
// A counter's positions are a checklist opened by Change positions…: one box
// per position of the process, each greyed with its refusal when the
// binding's rules refuse it (the dry run, GET
// /api/process-nodes/{id}/countable). The boxes are the bindings, except that
// the process's only station counter, while it counts no position yet, starts
// with its countable unbound positions ticked. Nothing binds until the
// checklist's save, which sends one PUT …/counted-at per box that differs from
// its binding. A classic script, so its Node test loads it.
(function () {
    'use strict';

    // The applicability rule, said where a station counter is made and bound.
    const RULE = 'A station counter clicks where the part leaves the cart, once per assembly, under every ' +
        'style that uses the positions counted at it. It reads the cell’s own PLC; there is no ratio.';

    // The second tag, said where it is typed.
    const SECOND_TAG = 'A second tag is for a station with one fixture per hand, one hand running at a ' +
        'time: the counter counts the two tags’ clicks added together.';

    function esc(s) {
        return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;')
            .replace(/>/g, '&gt;').replace(/"/g, '&quot;');
    }

    // tagsOf(c): a station counter's tags as the row says them, the second
    // after " + " (the counter counts their sum).
    function tagsOf(c) {
        return c.tag_name_2 ? c.tag_name + ' + ' + c.tag_name_2 : String(c.tag_name || '');
    }

    // nameOf(c): a station counter as another row names it.
    function nameOf(c) {
        return c.label || tagsOf(c) || 'station counter ' + c.id;
    }

    // count(n, one, many): "1 stroke", "2 strokes".
    function count(n, one, many) {
        return n + ' ' + (n === 1 ? one : many);
    }

    // rows(counterID, counterCount, positions, verdicts): one row per position,
    // { id, name, at, checked, disabled, refusal }. at is the station counter
    // the position is counted at (0: the production counter); verdicts holds
    // each position's dry run, { countable, refusal } — a position with none
    // yet is greyed. A box is ticked where the position is counted here; the
    // process's only station counter, while it counts no position, also ticks
    // its countable unbound positions (the first save's likely answer). Once
    // it counts one, the boxes are the bindings: a position left unbound stays
    // unticked.
    function rows(counterID, counterCount, positions, verdicts) {
        const v = verdicts || {};
        const list = positions || [];
        const countsNone = !list.some(n => (n.line_counter_id || 0) === counterID);
        return list.map(function (n) {
            const at = n.line_counter_id || 0;
            const verdict = v[n.id];
            const countable = !!(verdict && verdict.countable);
            return {
                id: n.id,
                name: n.name,
                at: at,
                checked: at === counterID || (countable && at === 0 && counterCount === 1 && countsNone),
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

    // when(iso): a time to the minute, the default when the page passes no
    // formatter (the page passes the plant-local one).
    function when(iso) {
        const d = new Date(iso);
        if (isNaN(d.getTime())) return String(iso || '');
        const p = n => String(n).padStart(2, '0');
        return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' + p(d.getHours()) + ':' + p(d.getMinutes());
    }

    // leaf(tag): a tag's member, after its struct.
    function leaf(tag) {
        const t = String(tag || '');
        return t.slice(t.lastIndexOf('.') + 1);
    }

    // status(c): a station counter's one line, { text, warn }: a tag not read
    // yet, the clicks per part, and the strokes since its last click. warn
    // (amber) only when the list says it stopped: no click for more strokes
    // than the deepest run-out seen (the strip's measure, stationStopped).
    function status(c) {
        const parts = [];
        if (!c.last_poll_at) parts.push('not read yet');
        else if (c.tag_name_2 && !c.last_poll_at_2) parts.push('second tag not read yet');
        parts.push(c.ratio == null ? 'clicks per part not measured yet' : '≈' + Number(c.ratio).toFixed(1) + ' per part');
        const n = c.strokes_since_click || 0;
        if (c.stopped) parts.push('no click for ' + count(n, 'stroke', 'strokes') + ', more than the deepest run-out seen');
        else if (n === 0) parts.push('clicked since the last stroke');
        else parts.push('last click ' + count(n, 'stroke', 'strokes') + ' ago');
        return { text: parts.join(' · '), warn: !!c.stopped };
    }

    // catidStyle(catid, styles, activeID): the style that expects a CATID (its
    // expected_catid list holds it), the active style first; '' for none.
    function catidStyle(catid, styles, activeID) {
        const holds = s => String(s.expected_catid || '').split(',').some(x => x.trim() === catid);
        const list = (styles || []).filter(holds);
        const hit = list.find(s => s.id === activeID) || list[0];
        return hit ? hit.name : '';
    }

    // titleWords(v): the section title's live words, from the process as
    // saved and the slots' read: not wired; off; or what it counts on, the
    // counted slot's CATID and the style that expects it, and its last stroke
    // (clock, the slot rising; fmt, a stroke longer ago).
    function titleWords(v) {
        const p = v.process || {};
        if (!p.counter_plc_name || !p.counter_tag_name) return 'not wired';
        if (!p.counter_enabled) return 'off · this process isn’t counted';
        const s = (v.slots || []).find(x => x.counted);
        const out = ['counting on ' + leaf(s ? s.tag : p.counter_tag_name)];
        if (s && s.read && s.part) {
            const style = catidStyle(s.catid, v.styles, p.active_style_id);
            out.push('CATID ' + s.catid + (style ? ' (' + style + ')' : ''));
        }
        if (s && s.rose_at) out.push('last stroke ' + (s.rising ? (v.clock || when) : (v.fmt || when))(s.rose_at));
        return out.join(' · ');
    }

    // titleHTML(v): the live words as the section title carries them, dot and
    // muted text; the dot is green only while it counts.
    function titleHTML(v) {
        const words = titleWords(v);
        const on = words.indexOf('counting on') === 0;
        return '<span class="pd-cnt' + (on ? ' ok' : '') + '" data-live><i></i>' + esc(words) + '</span>';
    }

    // field(id, label, sub, inner): a settings row whose value is a column.
    function field(id, label, sub, inner) {
        return '<div class="pd-sfld pd-scl-fld" data-group="' + id + '"><label>' + esc(label) +
            (sub ? '<small>' + esc(sub) + '</small>' : '') + '</label><div class="v col">' + inner + '</div></div>';
    }

    // line(text, extra): one line of a row's value column.
    function line(text, extra, cls) {
        return '<div class="pd-scl-line' + (cls ? ' ' + cls : '') + '"><span>' + esc(text) + '</span>' + (extra || '') + '</div>';
    }

    // fold(slots): the slots' names as one phrase: Prod_Counter_02–05 for a
    // run of consecutive slots, else the names listed.
    function fold(slots) {
        const names = slots.map(s => leaf(s.tag));
        const nums = slots.map(s => s.slot);
        const m = /^(.*?)(\d+)$/.exec(names[0] || '');
        const run = nums.every((n, i) => i === 0 || n === nums[i - 1] + 1);
        if (names.length > 1 && m && run) {
            const last = /(\d+)$/.exec(names[names.length - 1]);
            if (last && names.every(n => n.indexOf(m[1]) === 0)) return names[0] + '–' + last[1];
        }
        return names.join(', ');
    }

    // slotsField(v): the MES slots row. The counted slot first; each slot in
    // use (a part, a count or rising) on its own line; the unused ones (no
    // part, count 0, not rising) and the unread ones folded into a line each.
    // A slot rising beside the counted one that the strip calls a second
    // counter of the same stroke says so (v.items, the process's health
    // items); any other rising slot naming a part carries the re-point the
    // strip offers (ch-repoint). Hidden until the counter is wired.
    function slotsField(v) {
        const p = v.process || {};
        if (!p.counter_plc_name || !p.counter_tag_name) return '';
        const f = v.fmt || when;
        const slots = v.slots || [];
        if (!slots.length) {
            return field('slots', 'MES slots', 'the counter’s struct, read once a minute',
                line('not a MES struct’s Prod_Counter_0N, or not read yet', '', 'pd-dim'));
        }
        const counted = slots.find(s => s.counted);
        const lockstep = s => (v.items || []).some(it => it.kind === 'counter_slot_lockstep' && it.data && it.data.tag === s.tag);
        const reading = s => (s.part ? 'CATID ' + s.catid : 'no part') + ' · count ' + s.count + ' · ' +
            (s.rising ? 'rising' : 'not rising' + (s.rose_at ? ', last rose ' + f(s.rose_at) : ''));
        const used = [], unused = [], unread = [];
        let first = '';
        slots.forEach(s => {
            if (s.counted) {
                first = line(leaf(s.tag) + ' · counted · ' + (s.read ? reading(s) : 'not read at the last pass'));
            } else if (!s.read) {
                unread.push(s);
            } else if (!s.part && !s.count && !s.rising) {
                unused.push(s);
            } else {
                const twin = lockstep(s);
                const words = leaf(s.tag) + ' · ' + reading(s) +
                    (twin ? ' · rises with ' + leaf(counted ? counted.tag : p.counter_tag_name) +
                        ': a second counter of the same stroke, counted once' : '') +
                    (s.not_recorded_since ? ' · strokes since ' + f(s.not_recorded_since) + ' are not in the record' : '');
                const repoint = s.rising && s.part && !twin
                    ? '<button class="pd-btn" data-act="ch-repoint" data-pid="' + esc(p.id) + '" data-tag="' + esc(s.tag) +
                        '">Count ' + esc(leaf(s.tag)) + '</button>'
                    : '';
                used.push(line(words, repoint));
            }
        });
        const folded = (list, word) => list.length ? line(fold(list) + ' ' + word, '', 'pd-dim') : '';
        return field('slots', 'MES slots', 'the counter’s struct, read once a minute',
            first + used.join('') + folded(unused, 'unused') + folded(unread, 'not read at the last pass'));
    }

    // checklistHTML(c, v): a counter's positions as boxes (rows), a refused
    // one greyed with its sentence, one counted at another counter named so;
    // then the checklist's save and its cancel.
    function checklistHTML(c, v) {
        const counters = v.counters || [];
        const other = id => {
            const o = counters.find(x => x.id === id);
            return o ? nameOf(o) : 'station counter ' + id;
        };
        const boxes = rows(c.id, counters.length, v.positions || [], v.verdicts).map(r =>
            '<label class="pd-scl-box' + (r.disabled ? ' pd-dim' : '') + '"' + (r.refusal ? ' title="' + esc(r.refusal) + '"' : '') + '>' +
            '<input type="checkbox" data-scpos="' + r.id + '" data-sc="' + c.id + '"' + (r.checked ? ' checked' : '') +
            (r.disabled ? ' disabled' : '') + '> <span>' + esc(r.name) + '</span>' +
            (r.refusal ? '<span class="pd-dim">' + esc(r.refusal) + '</span>'
                : r.at && r.at !== c.id ? '<span class="pd-dim">counted at ' + esc(other(r.at)) + '</span>' : '') +
            '</label>').join('');
        return '<div class="pd-scl-pos" data-checklist="' + c.id + '">' +
            (boxes || '<span class="pd-dim">this process has no positions yet</span>') +
            '<div class="pd-scl-acts">' +
            (boxes ? '<button class="pd-btn" data-act="sc-positions" data-sc="' + c.id + '">Save positions</button>' : '') +
            '<button class="pd-btn" data-act="sc-checklist-close" data-sc="' + c.id + '">Cancel</button></div></div>';
    }

    // counterRow(c, v): one station counter: its label and tags, its verbs,
    // the positions it counts, its status, and its checklist when open
    // (v.open, the counter whose checklist is open).
    function counterRow(c, v) {
        const here = (v.positions || []).filter(n => n.line_counter_id === c.id).map(n => n.name);
        const st = status(c);
        return '<div class="pd-scl-row" data-sc-row="' + c.id + '">' +
            '<div class="pd-scl-head"><b>' + esc(c.label || 'station counter ' + c.id) + '</b>' +
            '<span class="pd-dim">' + esc(tagsOf(c)) + '</span><span class="pd-spacer"></span>' +
            '<button class="pd-dimlink" data-act="sc-checklist" data-sc="' + c.id + '">Change positions…</button>' +
            '<button class="pd-dimlink" data-act="sc-edit" data-sc="' + c.id + '">Edit</button>' +
            '<button class="pd-dimlink" data-act="sc-delete" data-sc="' + c.id + '">Remove</button></div>' +
            line(here.length ? 'counts ' + here.join(', ') : 'counts no position yet', '', 'pd-dim') +
            line(st.text, '', st.warn ? 'pd-warn' : 'pd-dim') +
            (v.open === c.id ? checklistHTML(c, v) : '') + '</div>';
    }

    // stationsField(v): the station counters row: one row per counter, then
    // + Add after the last. Hidden while the counter is not wired and none
    // exists.
    function stationsField(v) {
        const p = v.process || {};
        const wired = !!(p.counter_plc_name && p.counter_tag_name);
        const list = v.counters || [];
        if (!wired && !list.length) return '';
        return field('stations', 'Station counters',
            'count positions where the part leaves the cart, in place of the production counter',
            list.map(c => counterRow(c, v)).join('') +
            (wired ? '<div><button class="pd-btn" data-act="sc-new">+ Add station counter</button></div>' : ''));
    }

    // countingHTML(v): the Production counter section's two list rows. v holds
    // the process (as saved), slots (the MES slots' read), counters (the
    // station counters' list), positions and verdicts (the checklists' boxes),
    // open (the counter whose checklist is open), items (the process's health
    // items), styles (the process's styles), and fmt and clock (a time's
    // formats; when by default).
    function countingHTML(v) {
        return slotsField(v) + stationsField(v);
    }

    const api = { RULE, SECOND_TAG, tagsOf, nameOf, rows, changes, status, titleWords, titleHTML, countingHTML };
    if (typeof module !== 'undefined' && module.exports) module.exports = api;
    if (typeof window !== 'undefined') window.StationCard = api;
})();
