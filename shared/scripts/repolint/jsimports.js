// jsimports.js — the js-imports rule's link check (jsimports.go runs it).
// Reads {files: [{path, src, importer}]} on stdin and prints {checked,
// skipped, findings: [{path, line, name, spec, why}]}: every name a module
// imports, or re-exports from another, that the module it names does not
// export, and every module it names that no file is.
//
// THE PARSER IS NODE'S OWN, as js-undeclared's is (jsundeclared.js says why).
// Exit 3 when this node does not carry it.
//
// A SPECIFIER RESOLVES as the browser fetches it from the servers' mounts:
// /static/shared/x.js is shared/x.js, any other /static/x.js is x.js under the
// importer's own www/static, and ./x.js or ../x.js is beside the importer. A
// bare name or a full URL is not ours and is left alone, and so is import():
// its names are read off the namespace when the call runs, so a missing one is
// undefined there, not a module that fails to link.
//
// A file that parses only as a classic script exports nothing anyone can
// link to, and is js-parse's to judge: a module importing from it is skipped.

'use strict';

const fs = require('fs');
const path = require('path').posix;

let acorn;
try {
  acorn = require('internal/deps/acorn/acorn/dist/acorn');
} catch (e) {
  process.stderr.write('node ' + process.version + ' does not carry its bundled parser at internal/deps/acorn');
  process.exit(3);
}

const opts = { ecmaVersion: 'latest', sourceType: 'module', locations: true, allowHashBang: true };

// nameOf is an import or export specifier's name: an identifier, or the
// string of `export { x as "a-b" }`.
function nameOf(n) { return n.type === 'Literal' ? String(n.value) : n.name; }

// resolve is the tree path a specifier names from the module at from, or ''
// when it is not one of the servers' own URLs.
function resolve(from, spec) {
  if (/[?#]/.test(spec)) return '';
  if (spec.indexOf('/static/shared/') === 0) return path.normalize('shared/' + spec.slice('/static/shared/'.length));
  if (spec.indexOf('/static/') === 0) {
    const i = from.indexOf('/www/static/');
    return i < 0 ? '' : path.normalize(from.slice(0, i) + '/www' + spec);
  }
  if (spec.indexOf('./') === 0 || spec.indexOf('../') === 0) return path.join(path.dirname(from), spec);
  return '';
}

// modules: path -> {own: Set, stars: [path], links: [{name, line, spec, to}]},
// or null for a file that does not parse as a module.
const input = JSON.parse(fs.readFileSync(0, 'utf8'));
const modules = new Map();
const importers = [];
let classic = 0; // importers that parse only as a classic script: they import nothing
input.files.forEach(function(f) {
  let ast = null;
  try { ast = acorn.parse(f.src, opts); } catch (e) { modules.set(f.path, null); if (f.importer) classic++; return; }
  const m = { own: new Set(), stars: [], links: [] };
  ast.body.forEach(function(st) {
    const line = st.loc.start.line;
    const link = function(name, spec) { m.links.push({ name: name, line: line, spec: spec, to: resolve(f.path, spec) }); };
    switch (st.type) {
      case 'ImportDeclaration':
        st.specifiers.forEach(function(sp) {
          if (sp.type === 'ImportSpecifier') link(nameOf(sp.imported), st.source.value);
          else if (sp.type === 'ImportDefaultSpecifier') link('default', st.source.value);
        });
        if (st.specifiers.length === 0) link('', st.source.value); // import 'x': the file must exist
        return;
      case 'ExportNamedDeclaration':
        if (st.declaration) {
          const d = st.declaration;
          if (d.type === 'VariableDeclaration') d.declarations.forEach(function(x) { names(x.id).forEach(function(n) { m.own.add(n); }); });
          else if (d.id) m.own.add(d.id.name);
        }
        st.specifiers.forEach(function(sp) {
          m.own.add(nameOf(sp.exported));
          if (st.source) link(nameOf(sp.local), st.source.value);
        });
        return;
      case 'ExportDefaultDeclaration':
        m.own.add('default');
        return;
      case 'ExportAllDeclaration':
        if (st.exported) m.own.add(nameOf(st.exported));
        else m.stars.push(resolve(f.path, st.source.value));
        link('', st.source.value);
        return;
    }
  });
  modules.set(f.path, m);
  if (f.importer) importers.push(f.path);
});

// names is what a binding pattern declares.
function names(p) {
  const out = [];
  (function walk(q) {
    if (!q) return;
    if (q.type === 'Identifier') out.push(q.name);
    else if (q.type === 'ObjectPattern') q.properties.forEach(function(x) { walk(x.type === 'RestElement' ? x.argument : x.value); });
    else if (q.type === 'ArrayPattern') q.elements.forEach(walk);
    else if (q.type === 'RestElement') walk(q.argument);
    else if (q.type === 'AssignmentPattern') walk(q.left);
  })(p);
  return out;
}

// exportsOf is every name the module at p exports, its own and those its
// export * lends (never their default); null when p is not a module.
const memo = new Map();
function exportsOf(p, seen) {
  if (memo.has(p)) return memo.get(p);
  const m = modules.get(p);
  if (!m) return null;
  seen = seen || new Set();
  if (seen.has(p)) return m.own;
  seen.add(p);
  const all = new Set(m.own);
  m.stars.forEach(function(s) {
    const more = exportsOf(s, seen);
    if (more) more.forEach(function(n) { if (n !== 'default') all.add(n); });
  });
  memo.set(p, all);
  return all;
}

const findings = [];
let skipped = 0;
importers.forEach(function(p) {
  modules.get(p).links.forEach(function(l) {
    if (!l.to) return;
    if (!modules.has(l.to)) {
      findings.push({ path: p, line: l.line, name: l.name, spec: l.spec,
        why: 'imports from ' + l.spec + ', which is no file in the tree (' + l.to + ')' });
      return;
    }
    const ex = exportsOf(l.to);
    if (ex === null) { skipped++; return; }
    if (l.name !== '' && !ex.has(l.name)) {
      findings.push({ path: p, line: l.line, name: l.name, spec: l.spec,
        why: 'imports ' + l.name + ' from ' + l.spec + ', which does not export it' });
    }
  });
});
process.stdout.write(JSON.stringify({ checked: importers.length, classic: classic, skipped: skipped, findings: findings }));
