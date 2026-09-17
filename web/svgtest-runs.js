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
  const prompt = svgtestCurrentPrompt();

  // The UI expands a "both" selection into one POST per mode; a single-mode
  // selection posts once. The server runs exactly one mode per request and
  // stores each under its own mode-suffixed filename, so raw and think never
  // overwrite each other. A plain run leaves effort empty, which think mode
  // reads as its own default.
  const modes = svgtestState.mode === 'both' ? ['raw', 'think'] : [svgtestState.mode];
  const jobs = [];
  for (const p of pairs) {
    for (const mode of modes) jobs.push({ model: p.model, accountId: p.accountId, mode: mode, effort: '' });
  }
  await runSvgTestJobs(jobs, prompt, ['svgtestRunBtn'], false);
}

// runSvgTestSweep posts think mode once per effort rung for each pair, so one
// (model, account) accumulates a low→medium→high ladder in storage. That ladder
// is what the curve plots: it shows whether a model actually buys quality with
// more reasoning, or spends tokens for nothing. It is a separate button from Run
// because it is several times the calls, and an operator comparing two modes
// should not pay for a sweep they did not ask for.
async function runSvgTestSweep() {
  if (svgtestState.running) return;
  const pairs = svgtestPlannedPairs();
  if (pairs.length === 0) { toast(t('svgtest.needSelection'), 'warning'); return; }
  const prompt = svgtestCurrentPrompt();
  const levels = ['low', 'medium', 'high'];
  const jobs = [];
  for (const p of pairs) {
    for (const effort of levels) jobs.push({ model: p.model, accountId: p.accountId, mode: 'think', effort: effort });
  }
  await runSvgTestJobs(jobs, prompt, ['svgtestSweepBtn'], true);
}

function svgtestCurrentPrompt() {
  const promptBox = document.getElementById('svgtestPrompt');
  return (promptBox ? promptBox.value : '').trim() || svgtestState.defaultPrompt;
}

// runSvgTestJobs posts one job at a time and reports progress, then lands the UI
// on the group the server wrote. `curve` asks for the effort curve to be rendered
// afterwards, which only a sweep has the data for.
async function runSvgTestJobs(jobs, prompt, buttonIds, curve) {
  svgtestState.running = true;
  const status = document.getElementById('svgtestStatus');
  const buttons = buttonIds.map(id => document.getElementById(id)).filter(Boolean);
  buttons.forEach(b => { b.disabled = true; });
  const total = jobs.length;
  let done = 0;
  if (status) status.textContent = t('svgtest.running', '0', String(total));

  // Every POST response echoes the promptKey the server derived from this
  // prompt; capture one so the run can select its own group afterwards.
  let reportedKey = '';
  const tasks = jobs.map(job => api('/test-model-svg', {
    method: 'POST',
    body: JSON.stringify({ model: job.model, prompt: prompt, accountId: job.accountId, mode: job.mode, effort: job.effort }),
  }).then(res => res.json().catch(() => ({}))).then(d => {
    if (d && d.promptKey) reportedKey = d.promptKey;
  }).catch(() => {}).finally(() => {
    done++;
    if (status) status.textContent = t('svgtest.running', String(done), String(total));
  }));
  await Promise.all(tasks);

  svgtestState.running = false;
  buttons.forEach(b => { b.disabled = false; });
  if (status) status.textContent = t('svgtest.done');
  // Re-read history, then land on the group the server just wrote. Every POST
  // response carries the same promptKey (it is derived from the prompt), so one
  // of them identifies the group without re-hashing on the client.
  const groups = await loadSvgTestGroups();
  const known = new Set(groups.map(g => g.promptKey));
  const key = known.has(reportedKey) ? reportedKey : (groups[0] ? groups[0].promptKey : '');
  selectSvgTestGroup(key, curve);
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

function selectSvgTestGroup(key, curve) {
  const sel = document.getElementById('svgtestGroupHistory');
  if (sel) sel.value = key;
  renderSvgTestGroup(key, curve);
}

function bindSvgTestEvents() {
  if (svgtestState.bound) return;
  const byId = id => document.getElementById(id);
  const runBtn = byId('svgtestRunBtn');
  if (runBtn) runBtn.addEventListener('click', runSvgTest);
  const sweepBtn = byId('svgtestSweepBtn');
  if (sweepBtn) sweepBtn.addEventListener('click', runSvgTestSweep);
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
  bindSvgTestModeGroup(byId('svgtestModeGroup'));
  const rmf = byId('svgtestResultModelFilter');
  if (rmf) rmf.addEventListener('input', function () {
    svgtestState.resultModelFilter = this.value.trim().toLowerCase();
    renderSvgTestGroup(svgtestState.currentGroup);
  });
  svgtestState.bound = true;
}

// bindSvgTestModeGroup wires the Raw / Think / Both segmented control. One
// button is pressed at a time; aria-pressed drives the highlight in CSS so the
// active mode is exposed to assistive tech as well as visually.
function bindSvgTestModeGroup(group) {
  if (!group) return;
  const buttons = group.querySelectorAll('.svgtest-mode-btn');
  buttons.forEach(btn => btn.addEventListener('click', () => {
    svgtestState.mode = btn.dataset.mode;
    buttons.forEach(b => b.setAttribute('aria-pressed', String(b === btn)));
  }));
}

function initSvgTestPage() {
  bindSvgTestEvents();
  if (!svgtestState.loaded) loadSvgTestMatrix();
  else renderSvgTestPickers();
  loadSvgTestGroups();
}
