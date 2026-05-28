// Unit tests for installTableSort in /static/shared/utils.js. Run via
// the Go test wrapper table_sort_test.go. Exit 0 on pass, 1 on any
// assertion failure.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

// Minimal DOM stub built around the operations installTableSort
// actually uses. Not jsdom — just enough to drive the helper.

let nodeCounter = 0;

function makeElem(tag) {
    const el = {
        __id: ++nodeCounter,
        tagName: (tag || 'div').toUpperCase(),
        children: [],
        _attrs: {},
        textContent: '',
        style: {},
        _listeners: {},
        parentNode: null,
    };
    el.appendChild = (c) => {
        // Mirror DOM semantics: moving a node detaches from its prior
        // parent.
        if (c.parentNode && c.parentNode !== el) {
            const idx = c.parentNode.children.indexOf(c);
            if (idx >= 0) c.parentNode.children.splice(idx, 1);
        } else if (c.parentNode === el) {
            const idx = el.children.indexOf(c);
            if (idx >= 0) el.children.splice(idx, 1);
        }
        el.children.push(c);
        c.parentNode = el;
        return c;
    };
    el.hasAttribute = (k) => Object.prototype.hasOwnProperty.call(el._attrs, k);
    el.getAttribute = (k) => (el._attrs[k] === undefined ? null : el._attrs[k]);
    el.setAttribute = (k, v) => { el._attrs[k] = String(v); };
    el.addEventListener = (ev, fn) => {
        (el._listeners[ev] = el._listeners[ev] || []).push(fn);
    };
    el.querySelector = (sel) => {
        if (sel === 'tbody') return findFirst(el, n => n.tagName === 'TBODY');
        if (sel === '.sort-indicator') {
            return findFirst(el, n => (n._attrs.class || '').includes('sort-indicator'));
        }
        throw new Error('unsupported querySelector: ' + sel);
    };
    el.querySelectorAll = (sel) => {
        if (sel === 'table[data-sortable]') {
            return findAll(el, n => n.tagName === 'TABLE' && n.hasAttribute('data-sortable'));
        }
        if (sel === 'thead th[data-sort]') {
            const thead = findFirst(el, n => n.tagName === 'THEAD');
            if (!thead) return [];
            return findAll(thead, n => n.tagName === 'TH' && n.hasAttribute('data-sort'));
        }
        throw new Error('unsupported querySelectorAll: ' + sel);
    };
    return el;
}

function findFirst(root, pred) {
    if (pred(root)) return root;
    for (const c of root.children) {
        const hit = findFirst(c, pred);
        if (hit) return hit;
    }
    return null;
}
function findAll(root, pred, out) {
    out = out || [];
    if (pred(root)) out.push(root);
    for (const c of root.children) findAll(c, pred, out);
    return out;
}

function fireClick(el) {
    const listeners = el._listeners.click || [];
    listeners.forEach(fn => fn({ target: el }));
}

// Build a sortable table with rows. Each row gets cells whose
// textContent is taken from cellValues[i][col]; if cellValues[i][col]
// is an object {text, sortValue}, the cell sets data-sort-value too.
function buildTable(cellValues, opts) {
    opts = opts || {};
    const table = makeElem('table');
    table.setAttribute('data-sortable', '');
    const thead = makeElem('thead');
    const headerRow = makeElem('tr');
    (opts.headers || cellValues[0].map((_, i) => 'Col' + i)).forEach(h => {
        const th = makeElem('th');
        th.setAttribute('data-sort', '');
        th.textContent = h;
        headerRow.appendChild(th);
    });
    thead.appendChild(headerRow);
    table.appendChild(thead);

    const tbody = makeElem('tbody');
    cellValues.forEach((row, ri) => {
        const tr = makeElem('tr');
        tr.__rowIdx = ri;
        row.forEach(v => {
            const td = makeElem('td');
            if (v && typeof v === 'object') {
                td.textContent = String(v.text);
                if (v.sortValue !== undefined) td.setAttribute('data-sort-value', v.sortValue);
            } else {
                td.textContent = String(v);
            }
            tr.appendChild(td);
        });
        tbody.appendChild(tr);
    });
    table.appendChild(tbody);
    return table;
}

let passed = 0;
let failed = 0;
function assert(cond, label) {
    if (cond) { passed++; }
    else { failed++; console.error('FAIL: ' + label); }
}

function loadUtils() {
    const src = fs.readFileSync(path.join(__dirname, 'utils.js'), 'utf8');
    const transformed = src
        .replace(/export\s+function\s+/g, 'function ')
        .replace(/export\s+const\s+/g, 'const ');
    const root = makeElem('html');
    const ctx = vm.createContext({
        document: {
            createElement: (tag) => makeElem(tag),
            createTextNode: (text) => {
                const n = makeElem('#text');
                n.textContent = text;
                return n;
            },
            querySelectorAll: (sel) => root.querySelectorAll(sel),
            addEventListener: () => {},
            readyState: 'complete',
        },
        window: { addEventListener: () => {} },
        console,
        setTimeout, clearTimeout, fetch: () => {},
        EventSource: function () {},
        location: { reload: () => {} },
        Intl,
        Array,
        Object,
    });
    vm.runInContext(transformed + '; this.installTableSort = installTableSort;', ctx);
    return { ctx, root };
}

function rowOrder(table) {
    const tbody = table.querySelector('tbody');
    return tbody.children
        .filter(c => c.tagName === 'TR')
        .map(c => c.__rowIdx);
}

// Test 1: click toggles asc → desc. First click on shuffled input
// sorts ascending; second click reverses.
{
    const { ctx, root } = loadUtils();
    const table = buildTable([
        ['Charlie'],
        ['Alpha'],
        ['Bravo'],
    ]);
    root.appendChild(table);
    ctx.installTableSort();
    const th = table.children.find(c => c.tagName === 'THEAD').children[0].children[0];
    fireClick(th); // asc
    assert(JSON.stringify(rowOrder(table)) === JSON.stringify([1, 2, 0]),
        'first click sorts ascending — alpha/bravo/charlie');
    fireClick(th); // desc
    assert(JSON.stringify(rowOrder(table)) === JSON.stringify([0, 2, 1]),
        'second click reverses to descending — charlie/bravo/alpha');
}

// Test 2: numeric sort honors data-sort-value override.
{
    const { ctx, root } = loadUtils();
    const table = buildTable([
        [{ text: 'two hundred', sortValue: '200' }],
        [{ text: 'ten', sortValue: '10' }],
        [{ text: 'one hundred', sortValue: '100' }],
    ]);
    root.appendChild(table);
    ctx.installTableSort();
    const th = table.children.find(c => c.tagName === 'THEAD').children[0].children[0];
    fireClick(th);
    assert(JSON.stringify(rowOrder(table)) === JSON.stringify([1, 2, 0]),
        'numeric sort via data-sort-value: 10 < 100 < 200');
}

// Test 3: rows with data-no-sort stay at the end.
{
    const { ctx, root } = loadUtils();
    const table = buildTable([
        ['Charlie'],
        ['Alpha'],
        ['Bravo'],
    ]);
    // Mark the second row as no-sort.
    table.children.find(c => c.tagName === 'TBODY').children[1].setAttribute('data-no-sort', '');
    root.appendChild(table);
    ctx.installTableSort();
    const th = table.children.find(c => c.tagName === 'THEAD').children[0].children[0];
    fireClick(th);
    const order = rowOrder(table);
    assert(order[order.length - 1] === 1,
        'data-no-sort row remains last after sort (got ' + JSON.stringify(order) + ')');
}

console.log('installTableSort: ' + passed + ' passed, ' + failed + ' failed');
process.exit(failed === 0 ? 0 : 1);
