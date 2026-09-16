'use strict';

// OmniProxy SVG Model Test — results grid half. Shares svgtestState with
// svgtest.js / svgtest-runs.js. Renders one prompt group as a model x provider
// grid and lets the operator drop a stored result (usually a failed attempt)
// so it stops cluttering the comparison.

async function renderSvgTestGroup(key) {
  const box = document.getElementById('svgtestResults');
  if (!box) return;
  svgtestState.currentGroup = key || '';
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
  const del = document.createElement('button');
  del.type = 'button';
  del.className = 'btn btn-ghost btn-sm svgtest-card-del';
  del.title = t('svgtest.delete');
  del.setAttribute('aria-label', t('svgtest.delete'));
  del.innerHTML = '<i class="fa-solid fa-trash" aria-hidden="true"></i>';
  del.addEventListener('click', () => deleteSvgTestEntry(e, del));
  head.append(model, name, prov);
  const dialect = svgtestDialectBadge(e.dialect);
  if (dialect) head.appendChild(dialect);
  head.append(state, del);
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

// Shared render primitives: the pickers (svgtest.js) and this grid both show
// placeholder and empty rows, so they live here once for both.
function svgtestEmpty(key) {
  const el = document.createElement('div');
  el.className = 'empty-state';
  el.textContent = t(key);
  return el;
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
