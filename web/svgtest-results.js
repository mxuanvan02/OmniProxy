'use strict';

// OmniProxy SVG Model Test — results grid half. Shares svgtestState with
// svgtest.js / svgtest-runs.js. Renders one prompt group as a model x provider
// grid and lets the operator drop a stored result (usually a failed attempt)
// so it stops cluttering the comparison.

async function renderSvgTestGroup(key) {
  const box = document.getElementById('svgtestResults');
  if (!box) return;
  svgtestState.currentGroup = key || '';
  if (!key) {
    box.replaceChildren();
    if (typeof hideSvgTestScatter === 'function') hideSvgTestScatter();
    return;
  }
  box.replaceChildren(svgtestEmpty('svgtest.loading'));
  let data;
  try {
    const res = await api('/test-model-svg/groups/' + encodeURIComponent(key));
    data = await res.json();
  } catch (e) { box.replaceChildren(svgtestEmpty('svgtest.loadError')); return; }
  const entries = Array.isArray(data.entries) ? data.entries : [];
  box.replaceChildren();
  const kw = svgtestState.resultModelFilter;
  const shown = kw ? entries.filter(e => (e.model || '').toLowerCase().includes(kw)) : entries;
  if (shown.length === 0) { box.replaceChildren(svgtestEmpty('svgtest.noResults')); }
  else for (const e of shown) box.appendChild(buildSvgTestCard(e));
  // The scatter belongs to whichever group is open, not only to one a run just
  // produced: it is drawn from stored results, so a group picked from the history
  // has everything the chart needs. It reads the full entry set, not the filtered
  // view — the chart compares models against each other, and narrowing the grid by
  // name must not drop one and leave a misleading spread.
  if (typeof renderSvgTestScatter === 'function') renderSvgTestScatter(entries);
  else if (typeof hideSvgTestScatter === 'function') hideSvgTestScatter();
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
  const del = document.createElement('button');
  del.type = 'button';
  del.className = 'btn btn-ghost btn-sm svgtest-card-del';
  del.title = t('svgtest.delete');
  del.setAttribute('aria-label', t('svgtest.delete'));
  del.innerHTML = '<i class="fa-solid fa-trash" aria-hidden="true"></i>';
  del.addEventListener('click', () => deleteSvgTestEntry(e, del));
  // A failed run has no drawing to grade, so its "failed" badge already says it
  // all; the score badge only appears on a real result.
  const badges = [prov, svgtestDialectBadge(e.dialect)];
  if (ok) badges.push(svgtestScoreBadge(e.score));
  badges.push(state, del);
  head.append(model, name, ...badges.filter(Boolean));
  const meta = document.createElement('div');
  meta.className = 'svgtest-card-meta';
  meta.textContent = svgtestCardMeta(e, ok);
  // The rubric's lost-point reasons are the only way to explain a middling score,
  // so surface them on hover rather than widening the card for every result.
  if (ok && Array.isArray(e.scoreReasons) && e.scoreReasons.length > 0) {
    meta.title = t('svgtest.scoreReasons') + ': ' + e.scoreReasons.join(', ');
  }
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

// deleteSvgTestEntry drops one stored (model, account) result after a styled
// confirmation. Deleting a group's last entry removes the group too, so the
// history is reloaded and the picker falls back to whatever remains.
async function deleteSvgTestEntry(e, btn) {
  const key = svgtestState.currentGroup;
  if (!key || svgtestState.deleting) return;
  const asked = typeof confirmAction === 'function'
    ? await confirmAction(t('svgtest.confirmDelete', e.model || '?', e.accountName || e.accountId || '?'),
      { variant: 'danger', confirmText: t('svgtest.delete') })
    : false;
  if (!asked) return;
  svgtestState.deleting = true;
  if (btn) btn.disabled = true;
  const url = '/test-model-svg/groups/' + encodeURIComponent(key) + '/entries' +
    '?model=' + encodeURIComponent(e.model || '') +
    '&accountId=' + encodeURIComponent(e.accountId || '');
  let res = null;
  try { res = await api(url, { method: 'DELETE' }); } catch (err) { res = null; }
  svgtestState.deleting = false;
  if (!res || !res.ok) {
    toast(t('svgtest.deleteError'), 'error');
    if (btn) btn.disabled = false;
    return;
  }
  const groups = await loadSvgTestGroups();
  const still = groups.some(g => g.promptKey === key);
  selectSvgTestGroup(still ? key : (groups.length > 0 ? groups[0].promptKey : ''));
}

// svgtestCardMeta builds the one-line summary under a card: score, efficiency,
// latency, and token spend. Efficiency is score per thousand tokens — the figure
// that answers "which model bought the most quality for its spend". It renders
// "n/a" rather than a number whenever the token count is missing, because some
// chat gateways ignore stream_options.include_usage and report no usage at all;
// a division by zero or a fabricated rate there would be a lie in the grid.
// Latency goes through fmtElapsed: the real spread runs from a 403ms refusal to a
// 451s generation, and a bare millisecond count cannot be compared at a glance.
function svgtestCardMeta(e, ok) {
  const parts = [];
  if (ok) {
    parts.push((Number(e.score) || 0) + '/100');
    const tokens = Number(e.tokensUsed) || 0;
    if (tokens > 0) {
      const eff = (Number(e.score) || 0) / tokens * 1000;
      parts.push(t('svgtest.efficiency', eff.toFixed(1)));
    } else {
      parts.push(t('svgtest.efficiencyNA'));
    }
  }
  parts.push(fmtElapsed(e.elapsedMs));
  if (e.tokensUsed) parts.push(fmtTokens(e.tokensUsed) + ' ' + t('svgtest.tokens'));
  return parts.join(' · ');
}

// Shared render primitives: the pickers (svgtest.js), this grid and the scatter
// all need these, so they live here once for all three. This file loads before
// svgtest-scatter.js, which is the direction the dependency has to point.
function svgtestEmpty(key) {  const el = document.createElement('div');
  el.className = 'empty-state';
  el.textContent = t(key);
  return el;
}

// fmtElapsed renders the wall time a run actually took. The spread is enormous
// in practice — 403ms for a refused call up to 451321ms for a model that thought
// for seven and a half minutes — so the unit follows the magnitude. A bare
// millisecond count cannot be compared at a glance, and comparing time between
// models is half of what this feature measures.
//
// Both branches round through the value they are about to print. Without that a
// time just under the next unit carries past it: 59999ms would render as "60.0s"
// and 119900ms as "1m 60s", which read as errors rather than as roundings.
function fmtElapsed(ms) {
  const n = Number(ms) || 0;
  if (n < 1000) return n + 'ms';
  const secs = (n / 1000).toFixed(1);
  if (Number(secs) < 60) return secs + 's';
  const whole = Math.round(n / 1000);
  return Math.floor(whole / 60) + 'm ' + (whole % 60) + 's';
}

function svgtestSkeleton(n) {
  const wrap = document.createElement('div');
  for (let i = 0; i < n; i++) {
    const row = document.createElement('div');
    row.className = 'svgtest-skeleton';
    wrap.appendChild(row);
  }
  return wrap;
}
