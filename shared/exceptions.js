// exceptions.js — the exception list: one setting that is the same everywhere
// except where it differs, said as the rule plus the places that break it.
//
// WHY A SHARED PIECE. A setting kept per position (a light curtain, the counter
// a position's strokes count on) or per style (a position's next carrier) was
// drawn as one row per place, each saying the default: a 23-position press drew
// 23 rows of "light curtain off". This draws the rule once ("2 of 23
// positions", "all 23 at the production counter") and a row only where a place
// differs, each with its controls and a ×, then "+ Add". Three uses, by owner
// ruling (2026-11-14): a process's position settings, a style's "Set for this
// style", and a recipe group's own keys.
//
// IT DRAWS; THE PAGE ACTS. Markup only, by string concatenation like the
// pages that use it. A control in a row is the caller's markup; the × and the
// add carry the caller's data-act and data attributes, so the page's one
// delegated click handler acts on them. No state, no DOM, no imports beyond
// esc: it runs in a browser and in node's spec runner alike.
//
// NOTHING HIDES BEHIND A HOVER. The ×'s name is in its aria-label and its
// title, and the summary says what every place not listed has.
//
// ES module, served at /static/shared/exceptions.js. Style: components.css
// (.exc-*).

import { esc } from '/static/shared/esc.js';

// attrs is a data-* attribute list from { key: value }: data-<key>="<value>".
function attrs(map) {
    let out = '';
    for (const k of Object.keys(map || {})) {
        if (map[k] === undefined || map[k] === null) continue;
        out += ' data-' + k + '="' + esc(map[k]) + '"';
    }
    return out;
}

// exceptionList draws one setting:
//   id        the list's name on the page (data-exc), for a test or a scroll
//   label     the setting's name
//   sub       one muted line under the label, or '' (only where it changes
//             what a person would pick)
//   summary   the rule: what every place not listed has, or how many differ
//   rows      [{ name, cells: [html], remove: { act, ...data } | null }]: one
//             per place that differs. cells are the caller's controls,
//             already escaped; name is escaped here.
//   add       { label, act, ...data } | null: the "+ Add" after the rows
//   empty     the summary's own add, drawn beside it when there are no rows
//             (default: the add)
//
// The ×'s act and data are the row's remove; its aria-label names the place
// and the setting.
export function exceptionList(spec) {
    const s = spec || {};
    const rows = Array.isArray(s.rows) ? s.rows : [];
    const addBtn = (a) => {
        if (!a || !a.act) return '';
        const data = Object.assign({}, a);
        delete data.label;
        delete data.act;
        return '<button class="pd-btn quiet exc-add" data-act="' + esc(a.act) + '"' + attrs(data) + '>' +
            esc(a.label || '+ Add') + '</button>';
    };
    const row = (r) => {
        const cells = (r.cells || []).map((c) => '<td>' + c + '</td>').join('');
        let x = '';
        if (r.remove && r.remove.act) {
            const data = Object.assign({}, r.remove);
            delete data.act;
            const say = `Remove ${r.name || ''} from ${s.label || 'this setting'}`;
            x = '<td class="exc-x"><button type="button" data-act="' + esc(r.remove.act) + '"' + attrs(data) +
                ' aria-label="' + esc(say) + '" title="' + esc(say) + '">×</button></td>';
        }
        return '<tr><td class="exc-pos">' + esc(r.name || '') + '</td>' + cells + x + '</tr>';
    };
    const sum = '<div class="exc-sum"><span>' + esc(s.summary || '') + '</span>' +
        (rows.length ? '' : addBtn(s.empty === undefined ? s.add : s.empty)) + '</div>';
    return '<div class="exc"' + (s.id ? ' data-exc="' + esc(s.id) + '"' : '') + '>' +
        '<div class="exc-name">' + esc(s.label || '') + (s.sub ? '<small>' + esc(s.sub) + '</small>' : '') + '</div>' +
        '<div class="exc-body">' + sum +
        (rows.length ? '<table class="exc-rows"><tbody>' + rows.map(row).join('') + '</tbody></table>' + addBtn(s.add) : '') +
        '</div></div>';
}

// countOf is "n of total <noun>s", "all total <noun>s" or "no <noun>": the
// summary's usual words.
export function countOf(n, total, noun, plural) {
    const many = plural || noun + 's';
    if (!total) return 'no ' + many;
    if (n === total) return 'all ' + total + ' ' + (total === 1 ? noun : many);
    return n + ' of ' + total + ' ' + (total === 1 ? noun : many);
}
