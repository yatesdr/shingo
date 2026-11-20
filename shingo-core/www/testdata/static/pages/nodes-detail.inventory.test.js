// Unit test for the node modal's inventory cell on the Nodes page
// (nodes-detail.js invCount): a bin's count, and above its part's ratio of 1
// its whole UoP with its parts small beside it (owner, 2026-11-19). Run under
// plain Node via nodes_inventory_count_test.go. Exit 0 on pass, 1 on failure.
//
// invCount is lifted out of nodes-detail.js by name, with its ratios as the page
// reads them from #page-data, and count-words.js lent real (lend-count.js): the
// module's top level wires the whole page on load.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const SRC_DIR = path.join(__dirname, '..', '..', '..', 'static', 'pages');
const src = fs.readFileSync(path.join(SRC_DIR, 'nodes-detail.js'), 'utf8');

let failures = 0;
function check(name, got, want) {
    if (got === want) {
        console.log('  ok  ' + name);
    } else {
        failures++;
        console.log('  FAIL ' + name + ' — got ' + JSON.stringify(got) + ', want ' + JSON.stringify(want));
    }
}

function lift(name) {
    const at = src.search(new RegExp('^function\\s+' + name + '\\b', 'm'));
    if (at < 0) return null;
    for (let i = src.indexOf('{', at), depth = 0; i < src.length; i++) {
        if (src[i] === '{') depth++;
        else if (src[i] === '}' && --depth === 0) return src.slice(at, i + 1);
    }
    return null;
}

console.log("the node modal's inventory: a bin's UoP cell");
const fn = lift('invCount');
if (!fn) {
    failures++;
    console.log('  FAIL nodes-detail.js defines invCount');
} else {
    const ctx = {
        escapeHtml: (s) => String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;'),
        // The page's data-uop-ratios: only parts above 1.
        _uopRatios: { 'P-8': 8 },
    };
    vm.createContext(ctx);
    require('./lend-count.js')(ctx);
    vm.runInContext(fn, ctx);
    const cell = (b) => vm.runInContext('invCount(' + JSON.stringify(b) + ')', ctx);
    check('a bin at 8: its UoP, its parts beside it', cell({ payload_code: 'P-8', uop_remaining: 404 }),
        '50 <span class="text-muted">404 parts</span>');
    check('a bin at 1: the count, as it read', cell({ payload_code: 'P-1', uop_remaining: 404 }), '404');
    check('an empty bin at 1: 0, as it read', cell({ payload_code: '', uop_remaining: 0 }), '0');
    check('a bin with no count at 8: 0 UoP, 0 parts', cell({ payload_code: 'P-8' }),
        '0 <span class="text-muted">0 parts</span>');
}

console.log('node modal inventory: ' + (failures ? failures + ' failed' : 'all passed'));
process.exit(failures ? 1 : 0);
