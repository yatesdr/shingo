// jsundeclared.js — the js-undeclared rule's scope walk (jsundeclared.go runs
// it). Reads {files: [{path, src}], templates: [html]} on stdin and prints
// {checked, skipped, findings: [{path, line, name}]}: every identifier a file
// reads or writes that no scope declares and that is not a browser global.
//
// THE PARSER IS NODE'S OWN. Node bundles acorn for its REPL, and
// --expose-internals lends it out; the repo has no package.json and no
// node_modules, and this keeps it that way. Exit 3 when this node does not
// carry it, which the Go side reports as a failure, not a skip.
//
// Scoping follows the module (strict) rules: var and function declarations
// hoist to their function, let, const and class to their block, a function
// declared in a block stays in it. A file that parses as neither a module nor a
// script is js-parse's finding, not this rule's, and is skipped.

'use strict';

const fs = require('fs');

let acorn;
try {
  acorn = require('internal/deps/acorn/acorn/dist/acorn');
} catch (e) {
  process.stderr.write('node ' + process.version + ' does not carry its bundled parser at internal/deps/acorn');
  process.exit(3);
}

// The globals a page may read without declaring. A FIXED LIST, not node's
// globalThis: which names node defines moves with its version, and the answer
// must not.
const KNOWN = new Set([
  // The language.
  'globalThis', 'Infinity', 'NaN', 'undefined', 'eval', 'isFinite', 'isNaN', 'parseFloat', 'parseInt',
  'decodeURI', 'decodeURIComponent', 'encodeURI', 'encodeURIComponent', 'AggregateError', 'Array',
  'ArrayBuffer', 'Atomics', 'BigInt', 'BigInt64Array', 'BigUint64Array', 'Boolean', 'DataView', 'Date',
  'Error', 'EvalError', 'FinalizationRegistry', 'Float32Array', 'Float64Array', 'Function', 'Int8Array',
  'Int16Array', 'Int32Array', 'Intl', 'Iterator', 'JSON', 'Map', 'Math', 'Number', 'Object', 'Promise',
  'Proxy', 'RangeError', 'ReferenceError', 'Reflect', 'RegExp', 'Set', 'SharedArrayBuffer', 'String',
  'Symbol', 'SyntaxError', 'TypeError', 'Uint8Array', 'Uint8ClampedArray', 'Uint16Array', 'Uint32Array',
  'URIError', 'WeakMap', 'WeakRef', 'WeakSet',
  // The window and what hangs off it by bare name.
  'window', 'self', 'document', 'navigator', 'location', 'history', 'screen', 'console', 'localStorage',
  'sessionStorage', 'indexedDB', 'caches', 'crypto', 'performance', 'fetch', 'alert', 'confirm', 'prompt',
  'print', 'open', 'close', 'focus', 'blur', 'scroll', 'scrollTo', 'scrollBy', 'setTimeout', 'clearTimeout',
  'setInterval', 'clearInterval', 'requestAnimationFrame', 'cancelAnimationFrame', 'requestIdleCallback',
  'cancelIdleCallback', 'queueMicrotask', 'structuredClone', 'atob', 'btoa', 'getComputedStyle',
  'getSelection', 'matchMedia', 'innerWidth', 'innerHeight', 'outerWidth', 'outerHeight', 'devicePixelRatio',
  'scrollX', 'scrollY', 'pageXOffset', 'pageYOffset', 'top', 'parent', 'frames', 'opener', 'origin',
  'isSecureContext', 'event',
  // The platform's constructors and namespaces.
  'CSS', 'URL', 'URLSearchParams', 'Blob', 'File', 'FileReader', 'FormData', 'Headers', 'Request',
  'Response', 'AbortController', 'AbortSignal', 'TextEncoder', 'TextDecoder', 'Event', 'CustomEvent',
  'EventTarget', 'EventSource', 'WebSocket', 'MessageChannel', 'BroadcastChannel', 'Worker',
  'XMLHttpRequest', 'DOMParser', 'XMLSerializer', 'Node', 'NodeList', 'Element', 'HTMLElement',
  'HTMLInputElement', 'HTMLSelectElement', 'HTMLTextAreaElement', 'HTMLFormElement', 'HTMLCanvasElement',
  'HTMLImageElement', 'HTMLButtonElement', 'HTMLTemplateElement', 'SVGElement', 'Document',
  'DocumentFragment', 'Text', 'Image', 'Audio', 'Option', 'KeyboardEvent', 'MouseEvent', 'PointerEvent',
  'TouchEvent', 'FocusEvent', 'InputEvent', 'WheelEvent', 'MutationObserver', 'ResizeObserver',
  'IntersectionObserver', 'Notification', 'ClipboardItem', 'DOMRect', 'Path2D', 'ImageData',
  'OffscreenCanvas',
  // The vendored scripts' globals (static/vendor, static/js/htmx.min.js).
  'htmx', 'Chart',
  // A dual-use file's CommonJS export for its node test, always behind
  // typeof module !== 'undefined'.
  'module',
]);

function newScope(parent, isFunction) { return { parent: parent, isFunction: isFunction, names: new Set() }; }
function functionScope(s) { while (!s.isFunction) s = s.parent; return s; }
function declares(s, name) { for (; s; s = s.parent) if (s.names.has(name)) return true; return false; }
function isNode(v) { return v !== null && typeof v === 'object' && typeof v.type === 'string'; }
function children(n, fn) {
  for (const k of Object.keys(n)) {
    if (k === 'loc') continue;
    const v = n[k];
    if (Array.isArray(v)) v.forEach(function(x) { if (isNode(x)) fn(x); });
    else if (isNode(v)) fn(v);
  }
}

// bindings appends the names a binding pattern declares.
function bindings(p, out) {
  if (!p) return out;
  if (p.type === 'Identifier') out.push(p.name);
  else if (p.type === 'ObjectPattern') p.properties.forEach(function(q) { bindings(q.type === 'RestElement' ? q.argument : q.value, out); });
  else if (p.type === 'ArrayPattern') p.elements.forEach(function(e) { bindings(e, out); });
  else if (p.type === 'RestElement') bindings(p.argument, out);
  else if (p.type === 'AssignmentPattern') bindings(p.left, out);
  return out;
}

// varsIn declares, in fnScope, every var under n that is not inside a nested
// function: a var in a loop or a branch belongs to the function.
function varsIn(n, fnScope) {
  if (/Function|^Class/.test(n.type)) return;
  if (n.type === 'VariableDeclaration' && n.kind === 'var') {
    n.declarations.forEach(function(d) { bindings(d.id, []).forEach(function(x) { fnScope.names.add(x); }); });
  }
  children(n, function(c) { varsIn(c, fnScope); });
}

// hoist declares a statement list's names before any of its references is
// checked: lexical ones in block, var ones in fnScope.
function hoist(body, block, fnScope) {
  body.forEach(function(st) {
    let d = st;
    if ((st.type === 'ExportNamedDeclaration' || st.type === 'ExportDefaultDeclaration') && st.declaration) d = st.declaration;
    if (d.type === 'VariableDeclaration') {
      const target = d.kind === 'var' ? fnScope : block;
      d.declarations.forEach(function(x) { bindings(x.id, []).forEach(function(y) { target.names.add(y); }); });
    } else if ((d.type === 'FunctionDeclaration' || d.type === 'ClassDeclaration') && d.id) {
      block.names.add(d.id.name);
    } else if (d.type === 'ImportDeclaration') {
      d.specifiers.forEach(function(sp) { block.names.add(sp.local.name); });
    }
    varsIn(st, fnScope);
  });
}

function parse(src) {
  const opts = { ecmaVersion: 'latest', locations: true, allowHashBang: true };
  try { return acorn.parse(src, Object.assign({ sourceType: 'module' }, opts)); } catch (e) { /* try a script */ }
  try { return acorn.parse(src, Object.assign({ sourceType: 'script', allowReturnOutsideFunction: true }, opts)); } catch (e) { return null; }
}

// walk reports each unresolved reference in ast to found(name, line), and each
// name assigned on window, self or globalThis to pageGlobal(name).
function walk(ast, found, pageGlobal) {
  const top = newScope(null, true);
  hoist(ast.body, top, top);

  function ref(id, s) { if (!declares(s, id.name)) found(id.name, id.loc.start.line); }

  function block(body, s) {
    const b = newScope(s, false);
    hoist(body, b, functionScope(s));
    body.forEach(function(x) { visit(x, b); });
  }

  function fn(n, s) {
    const f = newScope(s, true);
    if (n.type === 'FunctionExpression' && n.id) f.names.add(n.id.name);
    if (n.type !== 'ArrowFunctionExpression') f.names.add('arguments');
    n.params.forEach(function(p) { bindings(p, []).forEach(function(x) { f.names.add(x); }); });
    n.params.forEach(function(p) { defaults(p, f); });
    if (n.body.type === 'BlockStatement') {
      hoist(n.body.body, f, f);
      n.body.body.forEach(function(x) { visit(x, f); });
    } else {
      visit(n.body, f);
    }
  }

  // defaults visits what a binding pattern reads: its default values and
  // computed keys.
  function defaults(p, s) {
    if (!p) return;
    if (p.type === 'AssignmentPattern') { visit(p.right, s); defaults(p.left, s); }
    else if (p.type === 'ObjectPattern') p.properties.forEach(function(q) { if (q.computed) visit(q.key, s); defaults(q.type === 'RestElement' ? q.argument : q.value, s); });
    else if (p.type === 'ArrayPattern') p.elements.forEach(function(e) { defaults(e, s); });
    else if (p.type === 'RestElement') defaults(p.argument, s);
  }

  // target visits an assignment's left side: an identifier there is a write,
  // which needs a declaration as much as a read does.
  function target(p, s) {
    if (!p) return;
    if (p.type === 'Identifier') ref(p, s);
    else if (p.type === 'ObjectPattern') p.properties.forEach(function(q) { if (q.computed) visit(q.key, s); target(q.type === 'RestElement' ? q.argument : q.value, s); });
    else if (p.type === 'ArrayPattern') p.elements.forEach(function(e) { target(e, s); });
    else if (p.type === 'RestElement') target(p.argument, s);
    else if (p.type === 'AssignmentPattern') { target(p.left, s); visit(p.right, s); }
    else visit(p, s);
  }

  function visit(n, s) {
    if (!n) return;
    switch (n.type) {
      case 'Identifier': ref(n, s); return;
      case 'FunctionDeclaration': case 'FunctionExpression': case 'ArrowFunctionExpression': fn(n, s); return;
      case 'ClassDeclaration': case 'ClassExpression': {
        const c = newScope(s, false);
        if (n.id) c.names.add(n.id.name);
        visit(n.superClass, c);
        n.body.body.forEach(function(m) {
          if (m.type === 'StaticBlock') { block(m.body, c); return; }
          if (m.computed) visit(m.key, c);
          visit(m.value, c);
        });
        return;
      }
      case 'BlockStatement': case 'StaticBlock': block(n.body, s); return;
      case 'SwitchStatement': {
        visit(n.discriminant, s);
        const b = newScope(s, false);
        const all = [];
        n.cases.forEach(function(c) { all.push.apply(all, c.consequent); });
        hoist(all, b, functionScope(s));
        n.cases.forEach(function(c) { visit(c.test, b); c.consequent.forEach(function(x) { visit(x, b); }); });
        return;
      }
      case 'VariableDeclaration': n.declarations.forEach(function(d) { defaults(d.id, s); visit(d.init, s); }); return;
      case 'ForStatement': case 'ForInStatement': case 'ForOfStatement': {
        const l = newScope(s, false);
        const head = n.type === 'ForStatement' ? n.init : n.left;
        if (head && head.type === 'VariableDeclaration') {
          const into = head.kind === 'var' ? functionScope(s) : l;
          head.declarations.forEach(function(d) { bindings(d.id, []).forEach(function(x) { into.names.add(x); }); });
          visit(head, l);
        } else if (n.type === 'ForStatement') {
          visit(head, l);
        } else {
          target(head, l);
        }
        visit(n.test, l); visit(n.update, l); visit(n.right, l); visit(n.body, l);
        return;
      }
      case 'CatchClause': {
        const c = newScope(s, false);
        bindings(n.param, []).forEach(function(x) { c.names.add(x); });
        defaults(n.param, c);
        visit(n.body, c);
        return;
      }
      case 'MemberExpression': visit(n.object, s); if (n.computed) visit(n.property, s); return;
      case 'Property': case 'PropertyDefinition': case 'MethodDefinition': if (n.computed) visit(n.key, s); visit(n.value, s); return;
      case 'LabeledStatement': visit(n.body, s); return;
      case 'BreakStatement': case 'ContinueStatement': case 'MetaProperty': case 'ImportDeclaration': case 'ExportAllDeclaration': return;
      case 'AssignmentExpression': {
        const l = n.left;
        if (l.type === 'MemberExpression' && !l.computed && l.object.type === 'Identifier' &&
            /^(window|self|globalThis)$/.test(l.object.name) && !declares(s, l.object.name)) pageGlobal(l.property.name);
        target(l, s);
        visit(n.right, s);
        return;
      }
      case 'UpdateExpression': target(n.argument, s); return;
      // typeof is how a script asks whether a name exists.
      case 'UnaryExpression': if (n.operator === 'typeof' && n.argument.type === 'Identifier') return; visit(n.argument, s); return;
      case 'ExportNamedDeclaration':
        if (n.declaration) visit(n.declaration, s);
        else if (!n.source) n.specifiers.forEach(function(sp) { ref(sp.local, s); });
        return;
    }
    children(n, function(c) { visit(c, s); });
  }

  ast.body.forEach(function(x) { visit(x, top); });
}

// templateGlobals are the names a template's inline classic script declares at
// its start of line (var PAGE_NODES = {{.NodesJSON}};): globals for every page
// script. A module's inline script declares nothing global, and is left out.
function templateGlobals(html, add) {
  const scripts = /<script\b(?![^>]*\bsrc=)(?![^>]*type="module")[^>]*>([\s\S]*?)<\/script>/g;
  let m;
  while ((m = scripts.exec(html)) !== null) {
    const decl = /^\s*(?:var|let|const|function)\s+([A-Za-z_$][\w$]*)/mg;
    let d;
    while ((d = decl.exec(m[1])) !== null) add(d[1]);
  }
}

const input = JSON.parse(fs.readFileSync(0, 'utf8'));
const page = new Set();
input.templates.forEach(function(html) { templateGlobals(html, function(n) { page.add(n); }); });
const found = [];
let checked = 0;
let skipped = 0;
input.files.forEach(function(f) {
  const ast = parse(f.src);
  if (!ast) { skipped++; return; }
  checked++;
  const seen = new Set();
  walk(ast, function(name, line) {
    if (seen.has(name)) return;
    seen.add(name);
    found.push({ path: f.path, line: line, name: name });
  }, function(name) { page.add(name); });
});
const findings = found.filter(function(x) { return !KNOWN.has(x.name) && !page.has(x.name); });
process.stdout.write(JSON.stringify({ checked: checked, skipped: skipped, findings: findings }));
