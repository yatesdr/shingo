// lend-count.js — the count's arithmetic and words, real, for a Core page test
// that builds its vm context by hand (imports dropped, names lent one by one):
// shared/count.js and shared/count-words.js. A page's numbers then come from
// the code the page runs, not a stub. The Edge floor's tests have the same
// lender (shingo-edge/www/testdata/static/operator-station/lend-count.js).
//
//   require('./lend-count.js')(ctx);   // ctx: a vm context, or an object to make one
//
// Not a test itself: it sits in testdata, outside the module's embed.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const SHARED = path.join(__dirname, '..', '..', '..', '..', '..', 'shared');

function script(src) {
    return src.replace(/^import\s[^;]*;[ \t]*$/gm, '')
        .replace(/^export\s+const\s+/gm, 'var ')
        .replace(/^export\s+/gm, '');
}

module.exports = function lendCount(ctx) {
    if (!vm.isContext(ctx)) vm.createContext(ctx);
    for (const f of ['count.js', 'count-words.js']) {
        vm.runInContext(script(fs.readFileSync(path.join(SHARED, f), 'utf8')), ctx, { filename: f });
    }
    return ctx;
};
