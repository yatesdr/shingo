// lend-count.js — the count's arithmetic and words, real, for a floor test that
// builds its vm context by hand (imports dropped, names lent one by one):
// shared/count.js, shared/count-words.js and the board model's ratioFor. A
// screen's numbers then come from the code the station runs, not a stub.
//
//   require('./lend-count.js')(ctx);   // ctx: a vm context, or an object to make one
//
// Not a test itself: it sits in testdata, outside the module's embed.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const SHARED = path.join(__dirname, '..', '..', '..', '..', '..', 'shared');
const SRC_DIR = path.join(__dirname, '..', '..', '..', 'static', 'operator-station');

function script(src) {
    return src.replace(/^import\s[^;]*;[ \t]*$/gm, '')
        .replace(/^export\s+const\s+/gm, 'var ')
        .replace(/^export\s+/gm, '');
}

// fnSource is the source of one top-level function of src, by name.
function fnSource(src, name) {
    const m = new RegExp('^(?:export\\s+)?function\\s+' + name + '\\s*\\(', 'm').exec(src);
    if (!m) throw new Error('lend-count.js: no function ' + name);
    let depth = 0;
    for (let i = src.indexOf('{', m.index); i < src.length; i++) {
        if (src[i] === '{') depth++;
        else if (src[i] === '}' && --depth === 0) return src.slice(m.index, i + 1).replace(/^export\s+/, '');
    }
    throw new Error('lend-count.js: unclosed function ' + name);
}

module.exports = function lendCount(ctx) {
    if (!vm.isContext(ctx)) vm.createContext(ctx);
    vm.runInContext(script(fs.readFileSync(path.join(SHARED, 'count.js'), 'utf8')), ctx, { filename: 'count.js' });
    vm.runInContext(script(fs.readFileSync(path.join(SHARED, 'count-words.js'), 'utf8')), ctx, { filename: 'count-words.js' });
    vm.runInContext(fnSource(fs.readFileSync(path.join(SRC_DIR, 'operator-board-model.js'), 'utf8'), 'ratioFor'), ctx,
        { filename: 'operator-board-model.js#ratioFor' });
    return ctx;
};
