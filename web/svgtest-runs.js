'use strict';

// OmniProxy SVG Model Test — run/history/render half. Shares svgtestState with
// svgtest.js. A run fires one POST per (model, account) pair; the server groups
// each result under the prompt's hash and stores it keyed by (model, account),
// so re-reading the group shows the archive as a model x provider grid rather
// than the transient responses.

// Cross-product of the two selected axes. Each pair is a distinct comparison
// cell — the same model on two providers is the whole point of the feature.
function svgtestPlannedPairs() {
  const models = Array.from(svgtestState.selModels);
  const accounts = Array.from(svgtestState.selAccounts);
  const pairs = [];
  for (const m of models) {
    for (const id of accounts) {
      const a = svgtestAccountById(id);
      if (a && (a.models || []).includes(m)) pairs.push({ model: m, accountId: id });
    }
  }
  return pairs;
}

async function runSvgTest() {
  if (svgtestState.running) return;
  const pairs = svgtestPlannedPairs();
  if (pairs.length === 0) { toast(t('svgtest.needSelection'), 'warning'); return; }
  const promptBox = document.getElementById('svgtestPrompt');
  const prompt = (promptBox ? promptBox.value : '').trim() || svgtestState.defaultPrompt;

  svgtestState.running = true;
  const runBtn = document.getElementById('svgtestRunBtn');
  const status = document.getElementById('svgtestStatus');
  if (runBtn) runBtn.disabled = true;
  const total = pairs.length;
  let done = 0;
  if (status) status.textContent = t('svgtest.running', '0', String(total));

  // Every POST response echoes the promptKey the server derived from this
  // prompt; capture one so the run can select its own group afterwards.
  let reportedKey = '';
  const tasks = pairs.map(p => api('/test-model-svg', {
    method: 'POST',
    body: JSON.stringify({ model: p.model, prompt: prompt, accountId: p.accountId }),
  }).then(res => res.json().catch(() => ({}))).then(d => {
    if (d && d.promptKey) reportedKey = d.promptKey;
  }).catch(() => {}).finally(() => {
    done++;
    if (status) status.textContent = t('svgtest.running', String(done), String(total));
  }));
  await Promise.all(tasks);

  svgtestState.running = false;
  if (runBtn) runBtn.disabled = false;
  if (status) status.textContent = t('svgtest.done');
  // Re-read history, then land on the group the server just wrote. Every POST
  // response carries the same promptKey (it is derived from the prompt), so one
  // of them identifies the group without re-hashing on the client.
  const groups = await loadSvgTestGroups();
  const known = new Set(groups.map(g => g.promptKey));
  const key = known.has(reportedKey) ? reportedKey : (groups[0] ? groups[0].promptKey : '');
  selectSvgTestGroup(key);
}

async function loadSvgTestGroups() {
  const sel = document.getElementById('svgtestGroupHistory');
  let groups = [];
  try {
    const res = await api('/test-model-svg/groups');
    const d = await res.json();
    groups = Array.isArray(d.groups) ? d.groups : [];
  } catch (e) { groups = []; }
  if (sel) {
    sel.replaceChildren();
    const ph = document.createElement('option');
    ph.value = '';
    ph.textContent = t('svgtest.groupHistory');
    sel.appendChild(ph);
    for (const g of groups) sel.appendChild(svgtestGroupOption(g));
  }
  return groups;
}

function svgtestGroupOption(g) {
  const opt = document.createElement('option');
  opt.value = g.promptKey;
  const when = g.savedAt ? new Date(g.savedAt * 1000).toLocaleString() : '';
  const head = (g.prompt || '').replace(/\s+/g, ' ').slice(0, 40);
  opt.textContent = head + ' — ' + (g.resultCount || 0) + ' ' + t('svgtest.entries') +
    (when ? ' — ' + when : '');
  return opt;
}

function selectSvgTestGroup(key) {
  const sel = document.getElementById('svgtestGroupHistory');
  if (sel) sel.value = key;
  renderSvgTestGroup(key);
}

async function renderSvgTestGroup(key) {
  const box = document.getElementById('svgtestResults');
  if (!box) return;
  if (!key) { box.replaceChildren(); return; }
  box.replaceChildren(svgtestEmpty('svgtest.loading'));
  let data;
  try {
    const res = await api('/test-model-svg/groups/' + encodeURIComponent(key));
    data = await res.json();
  } catch (e) { box.replaceChildren(svgtestEmpty('svgtest.loadError')); return; }
  const entries = Array.isArray(data.entries) ? data.entries : [];
  box.replaceChildren();
  if (entries.length === 0) { box.replaceChildren(svgtestEmpty('svgtest.noResults')); return; }
  for (const e of entries) box.appendChild(buildSvgTestCard(e));
}

function buildSvgTestCard(e) {
  const card = document.createElement('div');
  card.className = 'svgtest-card';
  const head = document.createElement('div');
  head.className = 'svgtest-card-head';
  const model = document.createElement('span');
  model.className = 'svgtest-card-model';
  model.textContent = e.model || '?';
  const name = document.createElement('span');
  name.className = 'svgtest-card-name';
  name.textContent = e.accountName || e.accountId;
  const prov = document.createElement('span');
  prov.className = 'badge badge-info';
  prov.textContent = e.provider || '?';
  const ok = e.success && e.svg;
  const state = document.createElement('span');
  state.className = 'badge ' + (ok ? 'badge-success' : 'badge-error');
  state.textContent = ok ? t('svgtest.ok') : t('svgtest.failed');
  head.append(model, name, prov, state);
  const meta = document.createElement('div');
  meta.className = 'svgtest-card-meta';
  meta.textContent = (e.elapsedMs || 0) + 'ms' + (e.tokensUsed ? ' · ' + e.tokensUsed + ' tokens' : '');
  card.append(head, meta);

  const body = document.createElement('div');
  body.className = 'svgtest-card-body';
  if (e.svg) {
    // SVG is model-generated markup shown to the operator only; it is inserted
    // as-is so the illustration renders, matching the admin-next viewer.
    const art = document.createElement('div');
    art.className = 'svgtest-art';
    art.innerHTML = e.svg;
    body.appendChild(art);
  } else {
    const msg = document.createElement('div');
    msg.className = 'svgtest-error';
    msg.textContent = e.error || t('svgtest.noSvg');
    body.appendChild(msg);
  }
  card.appendChild(body);
  return card;
}

function bindSvgTestEvents() {
  if (svgtestState.bound) return;
  const byId = id => document.getElementById(id);
  const runBtn = byId('svgtestRunBtn');
  if (runBtn) runBtn.addEventListener('click', runSvgTest);
  const refresh = byId('svgtestRefreshBtn');
  if (refresh) refresh.addEventListener('click', loadSvgTestMatrix);
  const mf = byId('svgtestModelFilter');
  if (mf) mf.addEventListener('input', function () { svgtestState.modelFilter = this.value.trim(); renderSvgTestModels(); });
  const af = byId('svgtestAccountFilter');
  if (af) af.addEventListener('input', function () { svgtestState.accountFilter = this.value.trim(); renderSvgTestAccounts(); });
  const bindBulk = (allId, noneId, set, axis) => {
    const all = byId(allId), none = byId(noneId);
    if (all) all.addEventListener('click', () => {
      (axis === 'models' ? svgtestVisibleModels() : svgtestVisibleAccounts().map(a => a.id))
        .forEach(v => set.add(v));
      onSvgTestSelectionChanged();
    });
    if (none) none.addEventListener('click', () => { set.clear(); onSvgTestSelectionChanged(); });
  };
  bindBulk('svgtestModelsAll', 'svgtestModelsNone', svgtestState.selModels, 'models');
  bindBulk('svgtestAccountsAll', 'svgtestAccountsNone', svgtestState.selAccounts, 'accounts');
  const promptBox = byId('svgtestPrompt');
  if (promptBox) promptBox.addEventListener('input', () => { svgtestState.promptDirty = true; });
  const resetPrompt = byId('svgtestResetPrompt');
  if (resetPrompt) resetPrompt.addEventListener('click', () => {
    if (promptBox) promptBox.value = svgtestState.defaultPrompt;
    svgtestState.promptDirty = false;
  });
  const history = byId('svgtestGroupHistory');
  if (history) history.addEventListener('change', function () { renderSvgTestGroup(this.value); });
  svgtestState.bound = true;
}

function initSvgTestPage() {
  bindSvgTestEvents();
  if (!svgtestState.loaded) loadSvgTestMatrix();
  else renderSvgTestPickers();
  loadSvgTestGroups();
}
