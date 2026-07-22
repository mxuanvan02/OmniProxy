'use strict';

// OmniProxy Quota Tracker Page — 9router-style block-per-account layout.
// Each account is a card ("block") with one or more quota rows inside.
// Each quota row has: name, progress bar (color-coded), used/total, remaining %,
// reset countdown ("in 5h 30m"), and emoji indicator (🟢🟡🔴).

// ─── State ───────────────────────────────────────────────
let quotaState = {
  loading: false,
  refreshTimer: null,
  data: null,
  cacheData: null,
  filter: 'all',        // 'all' | 'active' | 'inactive'
  providerFilter: 'all', // 'all' | 'codex' | 'kiro' | 'external' | 'trial'
  sortMode: 'expiring',  // 'expiring' | 'remaining-asc' | 'remaining-desc'
  page: 1,
  pageSize: 20,
};

// ─── Helpers ─────────────────────────────────────────────
function fmtPct(n) { return (n || 0).toFixed(1) + '%'; }
function fmtNum(n) { return new Intl.NumberFormat().format(n || 0); }
function fmtTokens(n) {
  n = n || 0;
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'K';
  return String(n);
}

// Reset countdown: "in 5h 30m" / "in 2d 4h" / "expired"
function fmtCountdown(resetAt) {
  if (!resetAt) return null;
  const d = new Date(resetAt * 1000);
  const now = new Date();
  const diff = d - now;
  if (diff <= 0) return 'expired';
  const totalMin = Math.ceil(diff / 60000);
  if (totalMin < 60) return totalMin + 'm';
  const hours = Math.floor(totalMin / 60);
  const mins = totalMin % 60;
  if (hours < 24) return hours + 'h' + (mins > 0 ? ' ' + mins + 'm' : '');
  const days = Math.floor(hours / 24);
  const remHours = hours % 24;
  return days + 'd' + (remHours > 0 ? ' ' + remHours + 'h' : '');
}

// Absolute reset date: "Today, 3:45 PM" / "Tomorrow, 9:00 AM" / "Jul 25, 2:30 PM"
function fmtResetAbsolute(resetAt) {
  if (!resetAt) return null;
  const b = new Date(resetAt * 1000);
  const now = new Date();
  const today = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  const tomorrow = new Date(today);
  tomorrow.setDate(tomorrow.getDate() + 1);
  let dayLabel;
  if (b >= today && b < tomorrow) dayLabel = 'Today';
  else if (b >= tomorrow && b < new Date(tomorrow.getTime() + 86400000)) dayLabel = 'Tomorrow';
  else dayLabel = b.toLocaleDateString('en-US', { month: 'short', day: 'numeric' });
  const timeLabel = b.toLocaleTimeString('en-US', { hour: 'numeric', minute: '2-digit', hour12: true });
  return dayLabel + ', ' + timeLabel;
}

function fmtDateStr(s) {
  if (!s) return '—';
  try { return new Date(s).toLocaleString(); } catch { return s; }
}

// Color + emoji by remaining % (9router convention: >70 green, 30-70 yellow, <30 red)
function quotaStyle(remaining) {
  if (remaining > 70) return { text: '#16a34a', bg: '#22c55e', bgLight: 'rgba(34,197,94,0.18)', emoji: '🟢' };
  if (remaining >= 30) return { text: '#d97706', bg: '#f59e0b', bgLight: 'rgba(245,158,11,0.18)', emoji: '🟡' };
  return { text: '#dc2626', bg: '#ef4444', bgLight: 'rgba(239,68,68,0.18)', emoji: '🔴' };
}

function escapeHtml(s) {
  if (!s) return '';
  return String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
}

// ─── API ─────────────────────────────────────────────────
async function loadQuotaData() {
  // Skip if a load is already in flight (5s timer can overlap with a
  // slow prior request). The next tick will pick up fresh data.
  if (quotaState.loading) return;
  quotaState.loading = true;
  // Show skeleton on first load (no data yet)
  const isFirstLoad = !quotaState.data;
  if (isFirstLoad) {
    const tableEl = $('quotaAccountsTable');
    if (tableEl) tableEl.innerHTML = renderSkeletonBlocks(8);
  }
  try {
    const [quotaRes, cacheRes] = await Promise.all([
      api('/quota/overview').then(r => r.ok ? r.json() : null),
      api('/cache/stats?period=24h').then(r => r.ok ? r.json() : null).catch(() => null),
    ]);
    quotaState.data = quotaRes;
    quotaState.cacheData = cacheRes;
    renderQuotaPage();
  } catch (e) {
    console.error('[Quota] load failed:', e);
    $('quotaProviderPills').innerHTML = '<span class="quota-error" style="padding:4px">Load failed</span>';
  } finally {
    quotaState.loading = false;
  }
}

// ─── Render: Provider pills (compact, horizontal) ────────
function renderProviderPills() {
  const container = $('quotaProviderPills');
  if (!container) return;
  const data = quotaState.data;
  if (!data || !data.providers) {
    container.innerHTML = '';
    return;
  }

  const providers = Object.values(data.providers).filter(p => p.accounts > 0);
  if (providers.length === 0) {
    container.innerHTML = '<span class="quota-empty" style="padding:4px">No accounts</span>';
    return;
  }

  container.innerHTML = providers.map(p => {
    const pct = p.usagePercent || 0;
    const remaining = Math.max(0, 100 - pct);
    const style = quotaStyle(remaining);
    return `
      <span class="quota-pill" data-provider="${p.provider}" title="${escapeHtml(p.label)}: ${p.activeAccounts}/${p.accounts} active, ${fmtPct(pct)} used">
        <span class="quota-pill-dot" style="background:${style.bg}"></span>
        <span class="quota-pill-label">${escapeHtml(p.label)}</span>
        <span class="quota-pill-meta">${p.activeAccounts}/${p.accounts}</span>
        <span class="quota-pill-pct" style="color:${style.text}">${fmtPct(pct)}</span>
      </span>`;
  }).join('');
}

// ─── Render: Cache pills (compact, horizontal) ───────────
function renderCachePills() {
  const container = $('quotaCachePills');
  if (!container) return;
  const c = quotaState.cacheData;
  if (!c) { container.innerHTML = ''; return; }

  const hitRatio = c.hitRatio || 0;
  const tokensSaved = c.tokensSaved || 0;
  const cacheRead = c.cacheRead || 0;
  const cachedTokens = c.cachedTokens || 0;

  container.innerHTML = `
    <span class="quota-cache-pill" title="Cache hit ratio (24h)">
      <i class="fa-solid fa-bullseye" style="color:#16a34a"></i>
      <span class="quota-cache-pill-label">Hit</span>
      <span class="quota-cache-pill-value" style="color:#16a34a">${fmtPct(hitRatio)}</span>
    </span>
    <span class="quota-cache-pill" title="Tokens saved via cache (24h)">
      <i class="fa-solid fa-piggy-bank" style="color:#2563eb"></i>
      <span class="quota-cache-pill-label">Saved</span>
      <span class="quota-cache-pill-value">${fmtTokens(tokensSaved)}</span>
    </span>
    <span class="quota-cache-pill" title="Claude cache read tokens (24h)">
      <span class="quota-cache-pill-label">Read</span>
      <span class="quota-cache-pill-value" style="color:#16a34a">${fmtTokens(cacheRead)}</span>
    </span>
    <span class="quota-cache-pill" title="OpenAI cached tokens (24h)">
      <span class="quota-cache-pill-label">Cached</span>
      <span class="quota-cache-pill-value" style="color:#16a34a">${fmtTokens(cachedTokens)}</span>
    </span>`;
}

// ─── Render: Account blocks grouped by provider ─────────
function renderAccountBlocks() {
  const container = $('quotaAccountsTable');
  if (!container) return;
  const data = quotaState.data;
  if (!data || !data.accounts || data.accounts.length === 0) {
    container.innerHTML = '<div class="quota-empty">No accounts</div>';
    return;
  }

  // Apply filters
  let accounts = data.accounts.filter(a => {
    if (quotaState.filter === 'active' && !a.enabled) return false;
    if (quotaState.filter === 'inactive' && a.enabled) return false;
    if (quotaState.providerFilter !== 'all' && providerKeyOf(a) !== quotaState.providerFilter) return false;
    return true;
  });

  // Sort
  if (quotaState.sortMode === 'remaining-asc') {
    accounts.sort((a, b) => minRemaining(a) - minRemaining(b));
  } else if (quotaState.sortMode === 'remaining-desc') {
    accounts.sort((a, b) => minRemaining(b) - minRemaining(a));
  }
  // 'expiring' = backend sort order (already by earliest resetAt)

  // Pagination
  const total = accounts.length;
  const totalPages = Math.max(1, Math.ceil(total / quotaState.pageSize));
  if (quotaState.page > totalPages) quotaState.page = totalPages;
  const start = (quotaState.page - 1) * quotaState.pageSize;
  const pageAccounts = accounts.slice(start, start + quotaState.pageSize);

  // Render toolbar (filter + sort + pagination)
  const toolbarHtml = renderToolbar(total, start, totalPages);

  // Group by provider key (codex / kiro / external / trial / other)
  const groups = groupByProvider(pageAccounts);
  const groupsHtml = Object.keys(groups).map(key => {
    const g = groups[key];
    if (g.accounts.length === 0) return '';
    return renderProviderGroup(g);
  }).join('');

  container.innerHTML = toolbarHtml + groupsHtml + renderPagination(totalPages);
  wireToolbarEvents();
}

// providerKeyOf normalizes an account's provider to a group key.
function providerKeyOf(a) {
  const p = (a.provider || '').toLowerCase();
  const pl = (a.providerLabel || '').toLowerCase();
  if (p.includes('codex') || pl.includes('codex')) return 'codex';
  if (p.includes('kiro') || p.includes('builder') || pl.includes('kiro')) return 'kiro';
  if (p.includes('external') || pl.includes('external')) return 'external';
  if (p.includes('trial') || pl.includes('trial')) return 'trial';
  return 'other';
}

// groupByProvider groups accounts into ordered sections.
function groupByProvider(accounts) {
  const order = ['codex', 'kiro', 'external', 'trial', 'other'];
  const labels = {
    codex: 'Codex (ChatGPT)',
    kiro: 'Kiro / CodeWhisperer',
    external: 'External OpenAI-compatible',
    trial: 'Trial',
    other: 'Other',
  };
  const groups = {};
  for (const k of order) groups[k] = { key: k, label: labels[k], accounts: [] };
  for (const a of accounts) groups[providerKeyOf(a)].accounts.push(a);
  return groups;
}

function renderProviderGroup(g) {
  const active = g.accounts.filter(a => a.enabled).length;
  const total = g.accounts.length;
  // Aggregate remaining % across all accounts in group (min of each account's min)
  const groupMinRem = Math.min(...g.accounts.map(minRemaining));
  const style = quotaStyle(groupMinRem);

  const blocksHtml = g.accounts.map(renderAccountBlock).join('');
  return `
    <div class="quota-group" data-provider="${g.key}">
      <div class="quota-group-header">
        <span class="quota-group-dot" style="background:${style.bg}"></span>
        <span class="quota-group-title">${escapeHtml(g.label)}</span>
        <span class="quota-group-count">${active}/${total} active</span>
      </div>
      <div class="quota-blocks-grid">${blocksHtml}</div>
    </div>`;
}

function minRemaining(a) {
  if (!a.quotas || a.quotas.length === 0) return 100;
  let m = 100;
  let hasLimited = false;
  for (const q of a.quotas) {
    // Only limited rows (total > 0) count toward remaining %
    if (q.total > 0 && q.remaining < m) {
      m = q.remaining;
      hasLimited = true;
    }
  }
  return hasLimited ? m : 100;
}

function renderAccountBlock(a) {
  const name = a.nickname || a.email || a.id.slice(0, 8);
  const statusDot = a.enabled
    ? '<span class="quota-status-dot quota-status-active"></span>'
    : '<span class="quota-status-dot quota-status-disabled"></span>';
  const planBadge = a.codexPlanType ? `<span class="quota-badge quota-badge-plan">${escapeHtml(a.codexPlanType)}</span>` : '';
  const statusBadge = a.status ? `<span class="quota-badge quota-badge-${escapeHtml(a.status)}">${escapeHtml(a.status)}</span>` : '';

  // Quota rows (9router-style: one row per quota dimension)
  let quotasHtml;
  if (a.quotas && a.quotas.length > 0) {
    quotasHtml = a.quotas.map(q => renderQuotaRow(q)).join('');
  } else {
    const pct = a.usagePercent || 0;
    const remaining = a.usageLimit > 0 ? Math.max(0, 100 - pct) : 100;
    quotasHtml = renderQuotaRow({
      name: 'Usage',
      used: a.usageCurrent,
      total: a.usageLimit,
      remaining: remaining,
      recurring: true,
    });
  }

  // Per-block action buttons
  // - Refresh: available for all accounts (refreshes usage/quota from provider)
  // - Credits: only for External OpenAI-compatible accounts (checks credit balance)
  // - Bank Reset Quota: only for Codex accounts — consumes a bank-reset
  //   credit upstream to clear rate-limit windows. Button is always shown
  //   for Codex accounts; clicking it checks available credits first and
  //   shows a toast if none are available.
  const isExternal = (a.provider || '').toLowerCase().includes('external');
  const isCodex = (a.provider || '').toLowerCase().includes('codex') || a.codexPlanType;
  const refreshIcon = 'fa-solid fa-rotate';
  const refreshLabel = typeof t === 'function' ? t('quota.refreshAccount') : 'Refresh';
  const creditsLabel = typeof t === 'function' ? t('quota.checkCredits') : 'Credits';
  const resetCreditLabel = typeof t === 'function' ? t('quota.bankResetQuota') : 'Bank Reset';
  const creditsBtn = isExternal ? `
      <button class="quota-block-btn" data-action="check-credits" data-account-id="${escapeHtml(a.id)}" title="${escapeHtml(creditsLabel)}">
        <i class="fa-solid fa-coins"></i> <span>${escapeHtml(creditsLabel)}</span>
      </button>` : '';
  // Bank Reset button: always shown for Codex accounts. Displays the
  // cached available count as a badge. Disabled when count is 0 (no
  // credits to consume). Count is fetched during account refresh and
  // cached on the account — click the button to consume one credit.
  const resetCount = a.codexResetCreditsAvailable || 0;
  const resetDisabled = resetCount <= 0 ? 'disabled' : '';
  const resetBtnClass = resetCount > 0 ? 'quota-block-btn-warning' : '';
  const resetCountBadge = resetCount > 0
    ? `<span class="quota-reset-count">${resetCount}</span>`
    : '<span class="quota-reset-count quota-reset-count-zero">0</span>';
  const resetCreditBtn = isCodex ? `
      <button class="quota-block-btn ${resetBtnClass}" data-action="bank-reset" data-account-id="${escapeHtml(a.id)}" ${resetDisabled} title="${escapeHtml(resetCreditLabel)}">
        <i class="fa-solid fa-bolt"></i> <span>${escapeHtml(resetCreditLabel)}</span> ${resetCountBadge}
      </button>` : '';
  const actionsHtml = `
    <div class="quota-block-actions">
      <button class="quota-block-btn" data-action="refresh-quota" data-account-id="${escapeHtml(a.id)}" title="${escapeHtml(refreshLabel)}">
        <i class="${refreshIcon}"></i> <span>${escapeHtml(refreshLabel)}</span>
      </button>
      ${creditsBtn}
      ${resetCreditBtn}
    </div>`;

  return `
    <div class="quota-block ${a.enabled ? '' : 'quota-block-disabled'}" data-account-id="${escapeHtml(a.id)}">
      <div class="quota-block-header">
        <div class="quota-block-name">
          ${statusDot}
          <span class="quota-block-title" title="${escapeHtml(a.email || '')}">${escapeHtml(name)}</span>
        </div>
        <div class="quota-block-meta">
          ${statusBadge}
          ${planBadge}
        </div>
      </div>
      <div class="quota-block-rows">
        ${quotasHtml}
      </div>
      ${actionsHtml}
    </div>`;
}

function renderQuotaRow(q) {
  const remaining = q.remaining != null ? q.remaining : 100;
  const style = quotaStyle(remaining);
  const hasLimit = q.total > 0;
  const isOverdraft = hasLimit && q.used > q.total;
  const usedLabel = fmtNum(q.used);
  const unitSuffix = q.unit ? ' ' + escapeHtml(q.unit) : '';

  // ─── Unlimited row (total=0): compact stat, no progress bar ──
  if (!hasLimit) {
    return `
      <div class="quota-row-unlimited">
        <span class="quota-row-unlimited-name">
          <i class="fa-solid fa-infinity" style="font-size:10px;color:var(--muted-foreground)"></i>
          ${escapeHtml(q.name)}
        </span>
        <span class="quota-row-unlimited-value">${usedLabel}${unitSuffix}</span>
      </div>`;
  }

  // ─── Limited row: progress bar + remaining % ──────────────
  const totalLabel = fmtNum(q.total);

  // Reset time
  let resetHtml = '';
  const countdown = fmtCountdown(q.resetAt);
  const absolute = fmtResetAbsolute(q.resetAt);
  if (countdown || absolute) {
    const prefix = q.recurring ? 'in ' : 'expires in ';
    if (countdown && countdown !== 'expired') {
      resetHtml = `<div class="quota-row-reset">${escapeHtml(prefix + countdown)}</div>`;
    } else if (countdown === 'expired') {
      resetHtml = `<div class="quota-row-reset" style="color:#dc2626">expired</div>`;
    }
    if (absolute) {
      resetHtml += `<div class="quota-row-reset-abs">${escapeHtml(absolute)}</div>`;
    }
  } else if (!q.recurring) {
    resetHtml = `<div class="quota-row-reset-abs">one-time credits</div>`;
  }

  const barWidth = Math.min(Math.max(remaining, 0), 100);
  const usedDisplay = `${usedLabel} / ${totalLabel}${unitSuffix}`;

  return `
    <div class="quota-row">
      <div class="quota-row-header">
        <span class="quota-row-name">
          <span class="quota-row-emoji">${style.emoji}</span>
          ${escapeHtml(q.name)}
        </span>
        <span class="quota-row-value">
          <span class="quota-row-used${isOverdraft ? ' quota-row-overdraft' : ''}">${usedDisplay}</span>
          <span class="quota-row-remaining" style="color:${style.text}">${remaining}%</span>
        </span>
      </div>
      <div class="quota-row-bar" style="background:${style.bgLight};border-color:${remaining === 0 ? 'var(--border)' : 'transparent'}">
        <div class="quota-row-bar-fill" style="width:${barWidth}%;background:${style.bg}"></div>
      </div>
      ${resetHtml}
    </div>`;
}

// ─── Toolbar (filter + sort + pagination) ────────────────
function renderToolbar(total, start, totalPages) {
  const tt = (k, fallback) => typeof t === 'function' ? t(k) : fallback;
  const providerOpts = [
    { v: 'all', l: tt('quota.allProviders', 'All providers') },
    { v: 'codex', l: 'Codex' },
    { v: 'kiro', l: 'Kiro' },
    { v: 'external', l: tt('quota.external', 'External') },
    { v: 'trial', l: tt('quota.trial', 'Trial') },
  ];
  const filterOpts = [
    { v: 'all', l: tt('quota.allAccounts', 'All accounts') },
    { v: 'active', l: tt('quota.active', 'Active') },
    { v: 'inactive', l: tt('quota.inactive', 'Turned off') },
  ];
  const sortOpts = [
    { v: 'expiring', l: tt('quota.expiringFirst', 'Expiring first') },
    { v: 'remaining-asc', l: tt('quota.remainingLow', '% quota: low to high') },
    { v: 'remaining-desc', l: tt('quota.remainingHigh', '% quota: high to low') },
  ];

  const sel = (opts, cur, id) => `<select id="${id}" class="quota-select">` +
    opts.map(o => `<option value="${o.v}"${o.v === cur ? ' selected' : ''}>${escapeHtml(o.l)}</option>`).join('') +
    '</select>';

  const end = Math.min(start + quotaState.pageSize, total);
  const showing = total > 0 ? `Showing ${start + 1}-${end} of ${total}` : 'No accounts';

  return `
    <div class="quota-toolbar-bar">
      <div class="quota-toolbar-filters">
        ${sel(providerOpts, quotaState.providerFilter, 'quotaProviderFilter')}
        ${sel(filterOpts, quotaState.filter, 'quotaFilter')}
        ${sel(sortOpts, quotaState.sortMode, 'quotaSort')}
      </div>
      <div class="quota-toolbar-info">
        <span class="quota-showing">${showing}</span>
      </div>
    </div>`;
}

function renderPagination(totalPages) {
  if (totalPages <= 1) return '';
  return `
    <div class="quota-pagination">
      <button class="quota-page-btn" id="quotaPrevPage" ${quotaState.page <= 1 ? 'disabled' : ''}>Prev</button>
      <span class="quota-page-info">Page ${quotaState.page} / ${totalPages}</span>
      <button class="quota-page-btn" id="quotaNextPage" ${quotaState.page >= totalPages ? 'disabled' : ''}>Next</button>
    </div>`;
}

function wireToolbarEvents() {
  const pf = $('quotaProviderFilter');
  if (pf) pf.onchange = () => { quotaState.providerFilter = pf.value; quotaState.page = 1; renderAccountBlocks(); };
  const f = $('quotaFilter');
  if (f) f.onchange = () => { quotaState.filter = f.value; quotaState.page = 1; renderAccountBlocks(); };
  const s = $('quotaSort');
  if (s) s.onchange = () => { quotaState.sortMode = s.value; renderAccountBlocks(); };
  const prev = $('quotaPrevPage');
  if (prev) prev.onclick = () => { if (quotaState.page > 1) { quotaState.page--; renderAccountBlocks(); } };
  const next = $('quotaNextPage');
  if (next) next.onclick = () => { quotaState.page++; renderAccountBlocks(); };

  // Per-block action buttons (refresh quota / check credits / bank reset)
  document.querySelectorAll('.quota-block-btn[data-action]').forEach(btn => {
    btn.onclick = async () => {
      if (btn.classList.contains('is-loading')) return;
      const action = btn.dataset.action;
      const accountId = btn.dataset.accountId;
      btn.classList.add('is-loading');
      btn.disabled = true;
      try {
        if (action === 'refresh-quota') {
          await api(`/accounts/${accountId}/refresh`, { method: 'POST' });
          await loadQuotaData();
        } else if (action === 'check-credits') {
          await api(`/accounts/${accountId}/credits`, { method: 'POST' });
          await loadQuotaData();
        } else if (action === 'bank-reset') {
          // First check if the account has any bank-reset credits available.
          const checkRes = await api(`/accounts/${accountId}/reset-credits/available`);
          const checkData = await checkRes.json();
          if (!checkData.available || checkData.available <= 0) {
            if (typeof toastError === 'function') {
              toastError(typeof t === 'function' ? t('quota.bankResetNone') : 'No bank-reset credits available');
            } else {
              alert('No bank-reset credits available for this account');
            }
            return;
          }
          // Confirm before consuming — this is a one-shot upstream action.
          const msg = typeof t === 'function'
            ? t('quota.confirmBankReset', String(checkData.available))
            : `Consume 1 of ${checkData.available} bank-reset credit(s)? This resets the rate-limit windows upstream.`;
          if (typeof confirmAction === 'function') {
            const ok = await confirmAction(msg, {
              title: typeof t === 'function' ? t('quota.bankResetQuota') : 'Bank Reset',
              confirmText: typeof t === 'function' ? t('common.confirm') : 'Confirm',
              variant: 'warning',
            });
            if (!ok) return;
          } else if (!confirm(msg)) {
            return;
          }
          const res = await api(`/accounts/${accountId}/reset-credits`, { method: 'POST' });
          const d = await res.json();
          if (d.success) {
            if (typeof toast === 'function') {
              toast(d.message || (typeof t === 'function' ? t('quota.bankResetDone') : 'Bank reset done'), 'success');
            }
            await loadQuotaData();
          } else {
            if (typeof toastError === 'function') {
              toastError(d.error || (typeof t === 'function' ? t('quota.bankResetFailed') : 'Bank reset failed'));
            }
          }
        }
      } catch (e) {
        console.error('[Quota] block action failed:', action, e);
        if (typeof toastError === 'function') {
          toastError(String(e.message || e));
        }
      } finally {
        btn.classList.remove('is-loading');
        btn.disabled = false;
      }
    };
  });
}

// ─── Full render ─────────────────────────────────────────
function renderQuotaPage() {
  renderProviderPills();
  renderCachePills();
  renderAccountBlocks();
  const lu = $('quotaLastUpdated');
  if (lu && quotaState.data) {
    const updatedLabel = typeof t === 'function' ? t('quota.updated') : 'Updated';
    lu.textContent = updatedLabel + ': ' + fmtDateStr(quotaState.data.timestamp);
  }
}

// ─── Lifecycle ───────────────────────────────────────────
function initQuotaPage() {
  // loadQuotaData now manages quotaState.loading internally; just trigger
  // an immediate load + start the 5s refresh timer.
  setRefreshBtnLoading(true);
  loadQuotaData().finally(() => setRefreshBtnLoading(false));

  if (quotaState.refreshTimer) clearInterval(quotaState.refreshTimer);
  // 5s refresh so token/request counters update in real time as traffic
  // flows through the proxy. The backend reads live in-memory pool stats
  // (not persisted config), so each poll reflects the latest counters.
  quotaState.refreshTimer = setInterval(() => {
    if (!$('tabQuota').classList.contains('hidden')) loadQuotaData();
  }, 5000);

  const btn = $('quotaRefreshBtn');
  if (btn) btn.onclick = () => {
    if (quotaState.loading) return;
    setRefreshBtnLoading(true);
    loadQuotaData().finally(() => setRefreshBtnLoading(false));
  };
}

function setRefreshBtnLoading(loading) {
  const btn = $('quotaRefreshBtn');
  if (!btn) return;
  if (loading) {
    btn.classList.add('is-loading');
    btn.disabled = true;
  } else {
    btn.classList.remove('is-loading');
    btn.disabled = false;
  }
}

// renderSkeletonBlocks shows shimmer placeholders while data loads.
function renderSkeletonBlocks(count) {
  const blocks = Array.from({ length: count || 8 }, () => `
    <div class="quota-block-skeleton">
      <div class="quota-skeleton-line medium"></div>
      <div class="quota-skeleton-line short"></div>
      <div class="quota-skeleton-line"></div>
      <div class="quota-skeleton-line"></div>
      <div class="quota-skeleton-line short"></div>
    </div>`).join('');
  return `<div class="quota-blocks-grid">${blocks}</div>`;
}

function destroyQuotaPage() {
  if (quotaState.refreshTimer) {
    clearInterval(quotaState.refreshTimer);
    quotaState.refreshTimer = null;
  }
}
