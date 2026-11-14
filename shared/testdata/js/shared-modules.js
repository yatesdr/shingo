// shared-modules.js — shared/'s ES modules in a node vm realm, loaded the way a
// browser's module map loads them: each file once, in its own scope, its
// imports from the other files bound to their exports.
//
// Not a test itself: it sits in testdata, outside the module's embed, so no
// binary carries or serves it.
//
//   const { realm } = require('<repo>/shared/testdata/js/shared-modules.js');
//   const load = realm();                       // a fresh realm, or realm(ctx)
//   const { sceneRoute } = load('scene-net.js');
//   const { pathWorld } = load('scene-path.js');
//
// realm(ctx, real) loads the files named in `real` (default: the scene modules
// below) for real. An import of any other shared file is dropped, and its names
// are whatever the realm's globals hold: a page harness stubs dom.js, clock.js
// and bus.js that way. bindImports(src, load) does the same to a page's own
// source, so the page and the shared modules hold one instance of each module,
// as they do in a browser.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const SHARED = path.join(__dirname, '..', '..');
const SCENE = ['scene-geom.js', 'scene-net.js', 'scene-path.js', 'scene-view.js', 'comet-layer.js'];

const IMPORT_RE = /^import\s*\{([^}]*)\}\s*from\s*'([^']+)';[ \t]*$/gm;
const ANY_IMPORT_RE = /^import[\s\S]*?from\s*'[^']+';[ \t]*$/gm;
const EXPORT_RE = /^export\s+(async\s+function|function|const|let|var|class)\s+([A-Za-z_$][\w$]*)/gm;

// An import list as a destructuring pattern: `a, b as c` is `a, b: c`.
function destructure(list) {
    return '{' + list.replace(/\s+/g, ' ').replace(/\b([\w$]+)\s+as\s+([\w$]+)/g, '$1: $2') + '}';
}
// An import line becomes as many empty lines as it had, so a stack trace's
// line numbers are the file's own.
function blankLines(m) { return m.replace(/[^\n]/g, ''); }

function sharedFile(spec, real) {
    const m = /^\/static\/shared\/([\w.-]+\.js)$/.exec(spec);
    return m && real.indexOf(m[1]) >= 0 ? m[1] : null;
}

// bindImports rewrites a source's import lines: one from a file `load` runs
// for real becomes a binding to that file's exports, and any other is blanked.
function bindImports(src, real, name) {
    src = src.replace(IMPORT_RE, function (m, list, from) {
        return (sharedFile(from, real) ? 'const ' + destructure(list) + ' = ' + name + '(' + JSON.stringify(from) + ');' : '') +
            blankLines(m);
    });
    return src.replace(ANY_IMPORT_RE, blankLines);
}

// realm returns load(file or '/static/shared/<file>'), the exports of that
// shared file, run once in `ctx` (a fresh vm context when none is given).
// reach names, per file, functions of its own that a test reads as if they
// were exported ({'scene-view.js': ['drawAisles']}); a name the file does not
// declare fails the load.
function realm(ctx, real, reach) {
    real = real || SCENE;
    reach = reach || {};
    if (!ctx) ctx = {};
    if (!vm.isContext(ctx)) vm.createContext(ctx);
    const cache = {};
    function load(spec) {
        const file = spec.indexOf('/') >= 0 ? sharedFile(spec, real) : (real.indexOf(spec) >= 0 ? spec : null);
        if (!file) throw new Error('shared-modules.js loads no ' + spec + ' (only ' + real.join(', ') + ')');
        if (cache[file]) return cache[file];
        const names = [];
        let src = bindImports(fs.readFileSync(path.join(SHARED, file), 'utf8'), real, '__shared');
        src = src.replace(EXPORT_RE, function (m, kw, n) { names.push(n); return kw + ' ' + n; });
        if (!names.length) throw new Error('shared/' + file + ' declares no "export function/const/let/var/class" this loader can read');
        (reach[file] || []).forEach(function (n) { names.push(n); });
        const fn = vm.runInContext('(function (__shared) {\'use strict\';' + src + '\nreturn { ' + names.join(', ') + ' };\n})', ctx,
            { filename: file });
        cache[file] = fn(load);
        return cache[file];
    }
    return load;
}

// runPage runs a page module's source in `ctx` as a script, its imports from
// the shared files `load` runs bound to them (ctx.__shared is set to `load`).
function runPage(ctx, src, load, real, filename) {
    ctx.__shared = load;
    vm.runInContext(bindImports(src, real || SCENE, '__shared'), ctx, { filename: filename });
}

// moduleOf runs an ES module that is not shared/'s (a page's own component,
// by its path on disk) in `ctx` once, as realm runs a shared file: its imports
// of the shared files `load` runs bound to them, any other blanked, and its
// exports returned. A page that imports it reads those names off ctx, since
// runPage blanks that import: ctx.name = moduleOf(...).name.
function moduleOf(ctx, load, file, real) {
    const names = [];
    let src = bindImports(fs.readFileSync(file, 'utf8'), real || SCENE, '__shared');
    src = src.replace(EXPORT_RE, function (m, kw, n) { names.push(n); return kw + ' ' + n; });
    if (!names.length) throw new Error(file + ' declares no "export function/const/let/var/class" this loader can read');
    return vm.runInContext('(function (__shared) {\'use strict\';' + src + '\nreturn { ' + names.join(', ') + ' };\n})', ctx,
        { filename: path.basename(file) })(load);
}

module.exports = { realm: realm, runPage: runPage, moduleOf: moduleOf, bindImports: bindImports, SCENE: SCENE, SHARED: SHARED };
