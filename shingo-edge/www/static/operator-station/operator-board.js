// operator-board.js — draws the loader and unloader board: a station whose
// every tile is a loader's (isBoard). What it draws is boardModel's and how
// big is fitBoard's (operator-board-model.js); this file is the DOM.
//
// The board is drawn in bands: the top line, the board bar, the status row and
// one section per zone. A band is rebuilt only when what it draws changed (its
// model output and its share of the fit); every other band keeps its nodes
// and listeners. Pinned by operator-loader-station.test.js ("A band is
// replaced only when what it draws changed").
//
// Every tap goes through the acts its caller hands in (operator-render.js),
// the same panels the old boards opened, so a tap posts what it posted.

import { el } from './operator-util.js';
import { boardModel, fitBoard, statusRowLayout } from './operator-board-model.js';

// What the person at the screen chose; this screen only, nothing is stored.
let showAll = false;
let partialsOpen = false;

// drawn is the board on the glass: one {key, sig, el} a band, in order. sig is
// the band's inputs as JSON; a band whose sig is unchanged is kept.
let drawn = [];

// resetBoard forgets the drawn board, when the grid is drawn as something
// else and its children are gone.
export function resetBoard() { drawn = []; }

// mark is the shape that goes with a colour: red a solid disc, amber an open
// triangle, so the two read apart without the hue. currentColor draws it.
function mark(tone) {
    const span = el('span', { className: 'os-pc-mark' });
    span.innerHTML = tone === 'red'
        ? '<svg width="18" height="18" viewBox="0 0 18 18" aria-hidden="true"><circle cx="9" cy="9" r="8" fill="currentColor"/>' +
          '<path class="os-pc-mark-ink" d="M9 4.6V10" stroke-width="2.2" stroke-linecap="round"/>' +
          '<circle class="os-pc-mark-dot" cx="9" cy="13.1" r="1.25"/></svg>'
        : '<svg width="18" height="18" viewBox="0 0 18 18" aria-hidden="true"><path d="M9 2.5L16.5 15.5H1.5Z" fill="none" ' +
          'stroke="currentColor" stroke-width="1.8" stroke-linejoin="round"/><path d="M9 7.5V11" stroke="currentColor" ' +
          'stroke-width="1.8" stroke-linecap="round"/><circle cx="9" cy="13.2" r="1" fill="currentColor"/></svg>';
    return span;
}

function chevron() {
    const span = el('span', { className: 'os-pc-chevron' });
    span.innerHTML = '<svg width="14" height="22" viewBox="0 0 14 22" aria-hidden="true"><path d="M3 3L11 11L3 19" fill="none" ' +
        'stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"/></svg>';
    return span;
}

// wordLine is a tone's word and its reason: "NOW · CELL-1 out". Plain carries no
// word, only the reason.
function wordLine(cls, tone, word, reason) {
    const line = el('span', { className: cls });
    if (word) {
        line.appendChild(mark(tone));
        line.appendChild(el('span', { className: 'os-pc-word', textContent: word }));
    }
    if (reason) line.appendChild(el('span', { className: 'os-pc-why', textContent: (word ? '· ' : '') + reason }));
    return line;
}

// buildCard draws one card. The spot's name and any code on the spot line
// never shrink; the words between them do.
function buildCard(card, acts) {
    const btn = el('button', { type: 'button', className: 'os-pc-card os-pc-card--' + card.tone });
    if (card.number) {
        btn.classList.add('os-pc-card--numbered');
        btn.appendChild(el('span', { className: 'os-pc-num', textContent: String(card.number) }));
    }
    const body = el('span', { className: 'os-pc-card-body' });
    const spot = el('span', { className: 'os-pc-spot' });
    spot.appendChild(el('span', { className: 'os-pc-spot-name', textContent: card.spot }));
    if (card.spotWords) spot.appendChild(el('span', { className: 'os-pc-spot-words', textContent: ' · ' + card.spotWords }));
    [card.spotCode, card.spotPart, card.spotCount, card.spotHeight].forEach(function (code, i) {
        if (!code) return;
        spot.appendChild(el('span', { className: 'os-pc-spot-code', textContent: (i === 0 ? ' ' : ' · ') + code }));
    });
    body.appendChild(spot);
    body.appendChild(el('span', { className: 'os-pc-headline', textContent: card.headline }));
    if (card.also && card.also.length) {
        body.appendChild(el('span', { className: 'os-pc-also', textContent: 'Also ' + card.also.join(', ') }));
    }
    if (card.word || card.reason) body.appendChild(wordLine('os-pc-reason', card.tone, card.word, card.reason));
    if (card.note) body.appendChild(el('span', { className: 'os-pc-note', textContent: card.note }));
    btn.appendChild(body);
    btn.appendChild(chevron());
    btn.addEventListener('click', function () { acts.act(card.act); });
    return btn;
}

function moreCard(n, acts) {
    const btn = el('button', { type: 'button', className: 'os-pc-card os-pc-more', textContent: '+' + n + ' more' });
    btn.addEventListener('click', function () { showAll = true; acts.rerender(); });
    return btn;
}

// ── the bands ────────────────────────────────────────────────────────────────

function buildTop(m, acts) {
    const top = el('div', { className: 'os-pc-top' });
    const left = el('div', { className: 'os-pc-top-left' });
    left.appendChild(el('span', { className: 'os-pc-title', textContent: m.title }));
    left.appendChild(el('span', { className: 'os-pc-words', textContent: m.words.join(' · ') }));
    top.appendChild(left);
    // The stale sign: what the screen knows, in words (staleSign).
    if (m.sign) {
        top.appendChild(el('span', { className: 'os-pc-sign os-pc-sign--' + m.sign.dot, textContent: m.sign.words.join(' · ') }));
    }
    const btn = el('button', { type: 'button', className: 'os-pc-btn os-pc-showall',
        textContent: showAll ? 'Show work' : 'Show all ' + m.tileCount });
    btn.addEventListener('click', function () { showAll = !showAll; acts.rerender(); });
    top.appendChild(btn);
    return top;
}

function buildBar(bar, acts) {
    const wrap = el('div', { className: 'os-pc-bar' });
    bar.buttons.forEach(function (b) {
        const btn = el('button', { type: 'button', className: 'os-pc-btn' + (b.key === 'request' ? ' os-pc-btn--primary' : ''),
            textContent: b.label });
        if (!b.enabled || acts.busy(b.act)) btn.disabled = true;
        else btn.addEventListener('click', function () { acts.act(b.act); });
        wrap.appendChild(btn);
    });
    return wrap;
}

function bar(fraction, tick, cls) {
    const track = el('span', { className: 'os-pc-track' + (cls ? ' ' + cls : '') });
    const fill = el('span', { className: 'os-pc-fill' });
    fill.style.width = Math.round(Math.max(0, Math.min(1, fraction)) * 100) + '%';
    track.appendChild(fill);
    if (tick > 0) {
        const t = el('span', { className: 'os-pc-tick' });
        t.style.left = Math.round(Math.min(1, tick) * 100) + '%';
        track.appendChild(t);
    }
    return track;
}

function buildCells(cells, layout, rowsPerTile) {
    const wrap = el('div', { className: 'os-pc-status' });
    wrap.appendChild(el('div', { className: 'os-pc-status-label', textContent: 'Cells, closest to running out first' }));
    const row = el('div', { className: 'os-pc-tiles' + (layout.countUnder ? ' os-pc-tiles--under' : '') });
    row.style.setProperty('--os-pc-tile-cols', String(layout.columns || 1));
    if (cells.failed) {
        row.appendChild(el('div', { className: 'os-pc-tile os-pc-tile--quiet', textContent: 'Cells: could not look' }));
    }
    cells.tiles.forEach(function (t) {
        const tile = el('div', { className: 'os-pc-tile' + (t.red ? ' os-pc-tile--red' : '') + (t.covered ? ' os-pc-tile--quiet' : '') });
        tile.appendChild(el('div', { className: 'os-pc-tile-name', textContent: t.cell }));
        const hidden = t.hidden + Math.max(0, t.rows.length - rowsPerTile);
        t.rows.slice(0, rowsPerTile).forEach(function (r) {
            const line = el('div', { className: 'os-pc-row os-pc-row--' + r.state + (r.red ? ' os-pc-row--red' : '') });
            if (r.words) {
                const words = el('div', { className: 'os-pc-row-words' });
                words.appendChild(el('span', { className: 'os-pc-row-part', textContent: r.part }));
                words.appendChild(el('span', { className: 'os-pc-row-count', textContent: r.words }));
                line.appendChild(words);
            }
            const cap = r.capacity > 0 ? r.capacity : 0;
            line.appendChild(bar(cap && r.left != null ? r.left / cap : 0, cap ? r.tick / cap : 0,
                r.left == null ? 'os-pc-track--unknown' : ''));
            tile.appendChild(line);
        });
        if (hidden > 0) tile.appendChild(el('div', { className: 'os-pc-tile-more', textContent: '+' + hidden }));
        row.appendChild(tile);
    });
    if (cells.more > 0) row.appendChild(el('div', { className: 'os-pc-tile os-pc-tile--quiet os-pc-tile--more', textContent: '+' + cells.more + ' more cells' }));
    wrap.appendChild(row);
    return wrap;
}

function buildPresses(p, layout) {
    const wrap = el('div', { className: 'os-pc-status' });
    wrap.appendChild(el('div', { className: 'os-pc-status-label', textContent: 'Bins filling at the presses, closest to full first' }));
    const row = el('div', { className: 'os-pc-tiles' });
    row.style.setProperty('--os-pc-tile-cols', String(layout.columns || 1));
    p.tiles.forEach(function (t) {
        const tile = el('div', { className: 'os-pc-tile os-pc-press' + (t.red ? ' os-pc-tile--red' : '') });
        const top = el('div', { className: 'os-pc-press-top' });
        top.appendChild(el('span', { className: 'os-pc-tile-name', textContent: t.cell + ' · ' + t.node }));
        top.appendChild(el('span', { className: 'os-pc-row-count', textContent: t.words }));
        tile.appendChild(top);
        tile.appendChild(el('div', { className: 'os-pc-press-bin', textContent: t.bin }));
        tile.appendChild(bar(t.capacity > 0 && t.have != null ? t.have / t.capacity : 0, 0, ''));
        tile.appendChild(el('div', { className: 'os-pc-press-part', textContent: t.part + (t.ready ? '' : ' · no empty ready') }));
        row.appendChild(tile);
    });
    if (p.more > 0) row.appendChild(el('div', { className: 'os-pc-tile os-pc-tile--quiet os-pc-tile--more', textContent: '+' + p.more + ' more' }));
    if (p.empties) {
        const tile = el('div', { className: 'os-pc-tile os-pc-empties' });
        tile.appendChild(el('div', { className: 'os-pc-tile-name', textContent: 'Empties ready' }));
        p.empties.lines.forEach(function (l) {
            const line = el('div', { className: 'os-pc-empties-line' + (l.words === 'none' ? ' os-pc-empties-line--none' : '') +
                (l.small ? ' os-pc-empties-line--height' : '') });
            line.appendChild(el('span', { className: 'os-pc-row-part', textContent: l.bin }));
            line.appendChild(el('span', { className: 'os-pc-row-count', textContent: l.words }));
            tile.appendChild(line);
        });
        tile.appendChild(el('div', { className: 'os-pc-empties-open', textContent: p.empties.open + ' open' }));
        if (p.empties.group) tile.appendChild(el('div', { className: 'os-pc-press-part', textContent: p.empties.group }));
        row.appendChild(tile);
    }
    wrap.appendChild(row);
    return wrap;
}

// buildStrip draws the buffer's spots in the plant's order: a numbered outline
// a partial, a filled box an empty bin, a filled box with an outline one
// coming (a clear is moving an empty there), a dashed box open, "?" a spot
// the board cannot place.
function buildStrip(z) {
    const strip = el('span', { className: 'os-pc-strip' });
    z.strip.forEach(function (s) {
        const box = el('span', { className: 'os-pc-spot-box os-pc-spot-box--' + s.state });
        if (s.state === 'partial') box.textContent = String(s.number);
        else if (s.state === 'unknown') box.textContent = '?';
        strip.appendChild(box);
    });
    return strip;
}

function buildZone(z, shown, cols, acts) {
    const sec = el('section', { className: 'os-pc-zone os-pc-zone--' + z.tone + (z.folded ? ' os-pc-zone--folded' : '') });
    const head = el('div', { className: 'os-pc-zone-head' });
    head.appendChild(el('span', { className: 'os-pc-rail' }));
    head.appendChild(el('span', { className: 'os-pc-zone-title', textContent: z.title }));
    head.appendChild(el('span', { className: 'os-pc-zone-count', textContent: String(z.count) }));
    if (z.word) head.appendChild(wordLine('os-pc-zone-reason', z.tone, z.word, z.reason));
    // A zone with no card says so beside its title; the partials section,
    // whose header carries its strip, says it on one line under the header.
    const emptyLine = z.empty && z.cards.length === 0 ? z.empty : '';
    if (emptyLine && !z.strip) head.appendChild(el('span', { className: 'os-pc-zone-empty', textContent: emptyLine }));
    if (z.strip) {
        if (z.folded) head.appendChild(el('span', { className: 'os-pc-strip-words', textContent: z.stripWords }));
        head.appendChild(buildStrip(z));
        if (!z.openedByBoard) {
            const t = el('button', { type: 'button', className: 'os-pc-btn os-pc-toggle', textContent: z.folded ? 'Show' : 'Hide' });
            t.addEventListener('click', function () { partialsOpen = !partialsOpen; acts.rerender(); });
            head.appendChild(t);
        }
    }
    sec.appendChild(head);
    if (emptyLine && z.strip) sec.appendChild(el('div', { className: 'os-pc-zone-empty os-pc-zone-line', textContent: emptyLine }));
    // A zone cut to no card still draws its "+N more" (fitBoard counts it).
    if (z.cards.length > 0) {
        const grid = el('div', { className: 'os-pc-cards' });
        grid.style.setProperty('--os-pc-cols', String(cols));
        z.cards.slice(0, shown).forEach(function (c) { grid.appendChild(buildCard(c, acts)); });
        if (shown < z.cards.length) grid.appendChild(moreCard(z.cards.length - shown, acts));
        sec.appendChild(grid);
    }
    return sec;
}

// ── the board ────────────────────────────────────────────────────────────────

// renderBoard draws the board into grid. viewport is {width, height}; acts is
// {act(act), busy(act), rerender()}, the caller's door to each panel and
// whether a bar request is in flight; when(iso) the clock a refusal's time is
// printed in; sign the station's stale sign (staleSign), drawn in the top line.
export function renderBoard(grid, view, viewport, acts, when, sign) {
    const m = boardModel(view, { partialsOpen: partialsOpen, when: when, sign: sign });
    const status = m.cells || m.presses;
    const statusAt = function (rows) { return statusRowLayout(status, viewport.width, rows).height; };
    let zones = m.zones;
    let fit;
    if (showAll) {
        zones = [{ key: 'all', title: 'All spots', count: m.all.length, tone: 'plain', cards: m.all }];
        fit = fitBoard({ width: viewport.width, height: Infinity }, statusAt, zones, !!m.bar);
    } else {
        fit = fitBoard(viewport, statusAt, zones, !!m.bar);
    }
    const layout = statusRowLayout(status, viewport.width, fit.rowsPerTile);
    grid.classList.remove('os-pc-step-0', 'os-pc-step-1', 'os-pc-step-2');
    grid.classList.add('os-pc-step-' + fit.step);
    if (showAll) grid.classList.add('os-pc-showall'); else grid.classList.remove('os-pc-showall');

    const bands = [{ key: 'top', inputs: [m.title, m.words, m.tileCount, showAll, m.sign], build: function () { return buildTop(m, acts); } }];
    if (m.bar) {
        bands.push({ key: 'bar', inputs: [m.bar, m.bar.buttons.map(function (b) { return acts.busy(b.act); })],
            build: function () { return buildBar(m.bar, acts); } });
    }
    if (m.cells) {
        bands.push({ key: 'cells', inputs: [m.cells, layout, fit.rowsPerTile],
            build: function () { return buildCells(m.cells, layout, fit.rowsPerTile); } });
    }
    if (m.presses) bands.push({ key: 'presses', inputs: [m.presses, layout], build: function () { return buildPresses(m.presses, layout); } });
    zones.forEach(function (z, i) {
        bands.push({ key: 'zone:' + z.key, inputs: [z, fit.shown[i], fit.columns],
            build: function () { return buildZone(z, fit.shown[i], fit.columns, acts); } });
    });
    if (drawn.length === 0) grid.innerHTML = '';
    drawn = bands.map(function (b, i) {
        const sig = JSON.stringify(b.inputs);
        const kept = drawn.find(function (d) { return d.key === b.key && d.sig === sig; });
        const node = kept ? kept.el : b.build();
        if (grid.children[i] !== node) grid.insertBefore(node, grid.children[i] || null);
        return { key: b.key, sig: sig, el: node };
    });
    while (grid.children.length > drawn.length) grid.children[drawn.length].remove();
}
