#!/usr/bin/env node
'use strict';

/*
 * check-admin-scripts.js — regression guard for the legacy /admin UI.
 *
 * WHY THIS EXISTS
 * ---------------
 * Every /admin script is a CLASSIC script (not an ES module), loaded by a
 * dynamic async/unordered loader in web/index.html. Classic scripts share ONE
 * top-level lexical scope, so a `const`/`let`/`class` in one file that collides
 * with ANY top-level declaration of the same name in another file is a
 * SyntaxError that kills the ENTIRE later file: every function in it silently
 * ceases to exist and the UI half-renders.
 *
 * `node --check <file>` CANNOT catch this — the collision is BETWEEN files, not
 * inside one. That blind spot shipped a real regression: accounts.js died on a
 * `const classifyModelKind` alias colliding with model-kind.js, leaving the
 * Account List empty and "Available Models" stuck at 0 while the backend was
 * perfectly healthy.
 *
 * A regex scanner cannot replace this either: top-level declarations in these
 * files are indented inconsistently (accounts.js has `let` at column 0 but
 * `function` at indent 2), so scope cannot be inferred from indentation. The
 * only reliable check is to actually run them together in one context.
 *
 * USAGE
 *   node scripts/check-admin-scripts.js     # exit 0 = ok, 1 = broken
 */

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const REPO_DIR = path.join(__dirname, '..');
const WEB_DIR = path.join(REPO_DIR, 'web');
const INDEX_HTML = path.join(WEB_DIR, 'index.html');

// Symbols defined in one admin script and consumed by another. Each entry is a
// cross-file contract that silently breaks the UI when the defining file dies.
// Keep this list short and justified — `why` is printed on failure so the next
// person knows what the symbol is load-bearing for.
const CROSS_FILE_SYMBOLS = [
  ['escapeHtml', 'function', 'escape.js', 'every render path escapes HTML with it'],
  ['classifyModelKind', 'function', 'model-kind.js', 'accounts.js + app.js group models by kind'],
  ['MODEL_KIND_ORDER', 'object', 'model-kind.js', 'model-kind render order'],
  ['modelKindLabel', 'function', 'model-kind.js', 'model-kind section headers'],
  ['groupModelsByKind', 'function', 'model-kind.js', 'app.js models panel'],
  ['formatNum', 'function', 'accounts.js', 'app.js loadStats() writes statTokens with it'],
  ['loadAccounts', 'function', 'accounts.js', 'Account List data load'],
  ['renderAccounts', 'function', 'accounts.js', 'Account List render'],
  ['showExportModal', 'function', 'accounts.js', 'app.js bindAccountEvents() binds it to #exportBtn'],
];

// Load order is read from index.html so it can never drift from what the
// browser actually does. The two document.write() refs (escape.js,
// model-kind.js) sit earlier in the file than the dynamic loader array, so
// first-appearance order is the real execution order.
function adminScriptOrder() {
  const html = fs.readFileSync(INDEX_HTML, 'utf8');
  const order = [];
  const seen = new Set();
  const re = /\/admin\/([A-Za-z0-9_.-]+\.js)/g;
  for (let m; (m = re.exec(html)); ) {
    if (seen.has(m[1])) continue;
    seen.add(m[1]);
    order.push(m[1]);
  }
  return order;
}

// Just enough DOM/BOM for the scripts' top-level statements to run. Nothing
// here needs to work — it only has to exist.
function makeContext() {
  const noop = () => {};
  const el = new Proxy(function () {}, {
    get: (t, k) =>
      k === 'classList'
        ? { add: noop, remove: noop, toggle: noop, contains: () => false }
        : k === 'style' || k === 'dataset'
          ? {}
          : el,
    set: () => true,
    apply: () => el,
  });
  const store = { getItem: () => null, setItem: noop, removeItem: noop };
  const ctx = vm.createContext({
    document: {
      getElementById: () => el,
      querySelector: () => el,
      querySelectorAll: () => [],
      createElement: () => el,
      addEventListener: noop,
      removeEventListener: noop,
      documentElement: el,
      body: el,
      head: el,
      hidden: false,
      readyState: 'complete',
      write: noop,
    },
    sessionStorage: store,
    localStorage: store,
    location: { origin: 'http://127.0.0.1:8080', hostname: '127.0.0.1', hash: '', pathname: '/admin' },
    navigator: { language: 'en' },
    fetch: () => Promise.resolve({ ok: false, json: () => Promise.resolve({}) }),
    setInterval: () => 0,
    clearInterval: noop,
    setTimeout: () => 0,
    clearTimeout: noop,
    addEventListener: noop,
    EventSource: function () {},
    Blob: function () {},
    matchMedia: () => ({ matches: false, addEventListener: noop, addListener: noop }),
    getComputedStyle: () => ({}),
    requestAnimationFrame: () => 0,
    Intl,
    URL,
    console,
  });
  ctx.window = ctx;
  ctx.globalThis = ctx;
  return ctx;
}

// When a file dies with "Identifier 'X' has already been declared", point at
// the file that already owns X — that pairing is the actual bug.
function collisionHint(err, alreadyLoaded) {
  const m = /Identifier '([^']+)' has already been declared/.exec(err.message);
  if (!m) return;
  const name = m[1];
  const re = new RegExp('(?:^|\\s)(?:function|const|let|class|var)\\s+' + name + '\\b');
  const owners = alreadyLoaded.filter((f) => re.test(fs.readFileSync(path.join(WEB_DIR, f), 'utf8')));
  if (owners.length === 0) return;
  console.log('      hint: "' + name + '" is already declared by ' + owners.join(', '));
  console.log('            classic scripts share one top-level scope — rename or drop the duplicate.');
}

async function main() {
  const order = adminScriptOrder();
  if (order.length === 0) {
    console.error('FAIL  no /admin/*.js references found in ' + path.relative(REPO_DIR, INDEX_HTML));
    return 1;
  }

  const ctx = makeContext();
  const loaded = [];
  const failed = [];

  for (const file of order) {
    const src = fs.readFileSync(path.join(WEB_DIR, file), 'utf8');
    try {
      vm.runInContext(src, ctx, { filename: file });
      loaded.push(file);
      console.log('ok    ' + file);
    } catch (err) {
      failed.push(file);
      console.log('FAIL  ' + file + '  ' + err.name + ': ' + err.message);
      collisionHint(err, loaded);
    }
  }

  // init() is async and runs on load, so a throw inside it surfaces as an
  // unhandled rejection rather than a load failure. Let the microtask queue
  // drain, then report it — that is how the original regression manifested.
  let asyncError = null;
  process.on('unhandledRejection', (err) => {
    asyncError = err;
  });
  await new Promise((resolve) => setImmediate(resolve));

  let missing = 0;
  console.log('\ncross-file contracts:');
  for (const [name, kind, owner, why] of CROSS_FILE_SYMBOLS) {
    const value = ctx[name];
    const ok = kind === 'object' ? typeof value === 'object' && value !== null : typeof value === kind;
    if (!ok) missing++;
    console.log(
      (ok ? '  ok       ' : '  MISSING  ') + name.padEnd(20) + '(' + owner + ' — ' + why + ')'
    );
  }

  const problems = failed.length + missing + (asyncError ? 1 : 0);
  console.log('');
  if (asyncError) {
    console.log('FAIL  init() threw during page bootstrap:');
    console.log('      ' + (asyncError.stack || asyncError).toString().split('\n').slice(0, 6).join('\n      '));
    console.log('');
  }
  if (problems > 0) {
    console.log(
      'FAILED — ' + failed.length + ' script(s) failed to load, ' + missing + ' contract(s) missing' +
      (asyncError ? ', init() threw' : '') + '.'
    );
    return 1;
  }
  console.log('OK — ' + order.length + ' scripts loaded, all cross-file contracts intact.');
  return 0;
}

main().then(
  (code) => process.exit(code),
  (err) => {
    console.error('checker crashed:', err);
    process.exit(2);
  }
);
