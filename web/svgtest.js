'use strict';

// OmniProxy SVG Model Test — picker half. One GET /test-model-svg/matrix returns
// every eligible account together with the models it can actually serve, so both
// filter directions (model -> accounts, account -> models) are plain client-side
// set operations: ticking a box never costs a round trip.
// Run/history/render logic lives in svgtest-runs.js; both share svgtestState.

let svgtestState = {
  accounts: [],            // [{id,name,provider,catalogState,models[]}]
  models: [],              // union of servable model ids
  selModels: new Set(),
  selAccounts: new Set(),
  modelFilter: '',
  accountFilter: '',
  defaultPrompt: '',
  promptDirty: false,      // operator typed; stop overwriting the box on refresh
  currentGroup: '',        // prompt key the results grid is showing
  loaded: false,
  loading: false,
  bound: false,
  running: false,
  deleting: false,
};

async function loadSvgTestMatrix() {
  if (svgtestState.loading) return;
  svgtestState.loading = true;
  renderSvgTestPickers();
  try {
    const res = await api('/test-model-svg/matrix');
    const d = await res.json();
    svgtestState.accounts = Array.isArray(d.accounts) ? d.accounts : [];
    svgtestState.models = Array.isArray(d.models) ? d.models : [];
    svgtestState.defaultPrompt = d.defaultPrompt || '';
    svgtestState.loaded = true;
  } catch (e) {
    svgtestState.accounts = [];
    svgtestState.models = [];
  }
  svgtestState.loading = false;
  const box = document.getElementById('svgtestPrompt');
  if (box && !svgtestState.promptDirty) box.value = svgtestState.defaultPrompt;
  pruneSvgTestSelections();
  renderSvgTestPickers();
}

// accountsServingModel / modelsOfAccount are the two edges of the same relation;
// the pickers walk it in whichever direction the operator started from.
function svgtestAccountById(id) {
  return svgtestState.accounts.find(a => a.id === id) || null;
}

function svgtestVisibleAccounts() {
  const kw = svgtestState.accountFilter.toLowerCase();
  const wanted = svgtestState.selModels;
  return svgtestState.accounts.filter(a => {
    if (wanted.size > 0 && !(a.models || []).some(m => wanted.has(m))) return false;
    if (!kw) return true;
    return (a.name || a.id).toLowerCase().includes(kw) ||
      (a.provider || '').toLowerCase().includes(kw);
  }).sort((x, y) => (x.provider || '').localeCompare(y.provider || '') ||
    (x.name || x.id).localeCompare(y.name || y.id));
}

function svgtestVisibleModels() {
  const kw = svgtestState.modelFilter.toLowerCase();
  const wanted = svgtestState.selAccounts;
  return svgtestState.models.filter(m => {
    if (wanted.size > 0) {
      const any = Array.from(wanted).some(id => {
        const a = svgtestAccountById(id);
        return a && (a.models || []).includes(m);
      });
      if (!any) return false;
    }
    return !kw || m.toLowerCase().includes(kw);
  });
}

// A selection hidden by the other axis can never produce a result, so drop it
// rather than let the run button queue work that is known to fail.
function pruneSvgTestSelections() {
  const keepModels = new Set(svgtestVisibleModels());
  svgtestState.selModels.forEach(m => { if (!keepModels.has(m)) svgtestState.selModels.delete(m); });
  const keepAccounts = new Set(svgtestVisibleAccounts().map(a => a.id));
  svgtestState.selAccounts.forEach(id => { if (!keepAccounts.has(id)) svgtestState.selAccounts.delete(id); });
}

function renderSvgTestPickers() {
  renderSvgTestModels();
  renderSvgTestAccounts();
  updateSvgTestCounts();
}

function renderSvgTestModels() {
  const box = document.getElementById('svgtestModelList');
  if (!box) return;
  const rows = svgtestVisibleModels();
  box.replaceChildren();
  if (svgtestState.loading) { box.appendChild(svgtestSkeleton(4)); return; }
  if (rows.length === 0) { box.appendChild(svgtestEmpty('svgtest.noModels')); return; }
  const frag = document.createDocumentFragment();
  for (const m of rows) frag.appendChild(buildSvgTestModelRow(m));
  box.appendChild(frag);
}

function renderSvgTestAccounts() {
  const box = document.getElementById('svgtestAccountList');
  if (!box) return;
  const rows = svgtestVisibleAccounts();
  box.replaceChildren();
  if (svgtestState.loading) { box.appendChild(svgtestSkeleton(4)); return; }
  if (rows.length === 0) { box.appendChild(svgtestEmpty('svgtest.noAccounts')); return; }
  const frag = document.createDocumentFragment();
  for (const a of rows) frag.appendChild(buildSvgTestAccountRow(a));
  box.appendChild(frag);
}

function buildSvgTestModelRow(model) {
  const serving = svgtestState.accounts.filter(a => (a.models || []).includes(model));
  const label = document.createElement('label');
  label.className = 'svgtest-pick';
  const cb = document.createElement('input');
  cb.type = 'checkbox';
  cb.value = model;
  cb.checked = svgtestState.selModels.has(model);
  cb.addEventListener('change', () => {
    if (cb.checked) svgtestState.selModels.add(model); else svgtestState.selModels.delete(model);
    onSvgTestSelectionChanged();
  });
  const name = document.createElement('span');
  name.className = 'svgtest-pick-name';
  name.textContent = model;
  const count = document.createElement('span');
  count.className = 'badge badge-muted svgtest-pick-badge';
  count.textContent = String(serving.length);
  count.title = t('svgtest.accountsServing', String(serving.length));
  label.append(cb, name, count);
  return label;
}

function buildSvgTestAccountRow(a) {
  const wanted = svgtestState.selModels;
  const serves = wanted.size === 0 ? (a.models || []).length :
    (a.models || []).filter(m => wanted.has(m)).length;
  const label = document.createElement('label');
  label.className = 'svgtest-pick';
  if (a.catalogState === 'failed' || a.catalogState === 'empty') label.dataset.warn = a.catalogState;
  const cb = document.createElement('input');
  cb.type = 'checkbox';
  cb.value = a.id;
  cb.checked = svgtestState.selAccounts.has(a.id);
  cb.addEventListener('change', () => {
    if (cb.checked) svgtestState.selAccounts.add(a.id); else svgtestState.selAccounts.delete(a.id);
    onSvgTestSelectionChanged();
  });
  const name = document.createElement('span');
  name.className = 'svgtest-pick-name';
  name.textContent = a.name || a.id;
  const prov = document.createElement('span');
  prov.className = 'badge badge-info svgtest-pick-prov';
  prov.textContent = a.provider || '?';
  const dialect = svgtestDialectBadge(a.dialect);
  const count = document.createElement('span');
  count.className = 'badge badge-muted svgtest-pick-badge';
  count.textContent = String(serves);
  label.append(cb, name, prov);
  if (dialect) label.appendChild(dialect);
  label.appendChild(count);
  return label;
}

// svgtestDialectBadge returns a small badge distinguishing the call type an
// account uses (chat / responses / anthropic). Chat is the default and most
// common, so it gets no badge — only non-default dialects are tagged.
function svgtestDialectBadge(dialect) {
  const d = (dialect || '').toLowerCase();
  if (!d || d === 'chat') return null;
  const span = document.createElement('span');
  span.className = 'badge badge-warning svgtest-pick-dialect';
  span.textContent = d === 'responses' ? 'Responses' : d === 'anthropic' ? 'Messages' : d;
  return span;
}

// Selection changes filter the opposite axis; re-prune first so a model ticked
// while an unrelated account was selected does not strand that account.
function onSvgTestSelectionChanged() {
  pruneSvgTestSelections();
  renderSvgTestPickers();
}

function updateSvgTestCounts() {
  const m = document.getElementById('svgtestModelCount');
  if (m) m.textContent = t('svgtest.selectedCount', String(svgtestState.selModels.size));
  const a = document.getElementById('svgtestAccountCount');
  if (a) a.textContent = t('svgtest.selectedCount', String(svgtestState.selAccounts.size));
}
