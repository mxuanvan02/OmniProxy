'use strict';

// State — Accounts tab
let accountsData = [];
const selectedAccounts = new Set();
let filterKeyword = '';
let filterStatus = 'all';
let filterCategory = 'all';
let builderIdSession = '';
let builderIdPollTimer = null;
let iamSession = '';
let exportSelectedIds = new Set();
let testLogs = [];
let testModalAccountId = '';
let testModalModels = [];
let testModalLoadingModels = false;
let testModalModelError = false;
let testModalRunning = false;

  async function loadAccounts() {
    const res = await api('/accounts');
    accountsData = await res.json();
    renderAccounts();
  }

  // Account list
  function getFilteredAccounts() {
    return accountsData.filter(a => {
      if (filterStatus === 'enabled' && !a.enabled) return false;
      if (filterStatus === 'disabled' && (a.enabled || (a.banStatus && a.banStatus !== 'ACTIVE'))) return false;
      if (filterStatus === 'banned' && (!a.banStatus || a.banStatus === 'ACTIVE')) return false;
      if (filterCategory !== 'all' && accountCategory(a) !== filterCategory) return false;
      if (filterKeyword) {
        const kw = filterKeyword.toLowerCase();
        const emailMatch = (a.email || '').toLowerCase().includes(kw);
        const nicknameMatch = (a.nickname || '').toLowerCase().includes(kw);
        if (!emailMatch && !nicknameMatch) return false;
      }
      return true;
    });
  }
  // getFilteredAccountsGrouped returns the filtered accounts grouped by
  // category for the grouped list view. Returns an array of
  // { category, label, icon, accounts } in display order: kiro, codex, external, other.
  function getFilteredAccountsGrouped() {
    const filtered = getFilteredAccounts();
    const groups = { kiro: [], codex: [], external: [], other: [] };
    for (const a of filtered) {
      groups[accountCategory(a)].push(a);
    }
    const order = ['kiro', 'codex', 'external', 'other'];
    const out = [];
    for (const cat of order) {
      if (groups[cat].length > 0) {
        out.push({ category: cat, label: categoryLabel(cat), icon: categoryIcon(cat), accounts: groups[cat] });
      }
    }
    return out;
  }
  function onFilterChange() {
    filterKeyword = $('filterSearch').value;
    filterStatus = $('filterStatusSelect').value;
    const catSel = $('filterCategorySelect');
    if (catSel) filterCategory = catSel.value;
    renderAccounts();
  }
  function toggleSelectAll(checked) {
    const filtered = getFilteredAccounts();
    if (checked) filtered.forEach(a => selectedAccounts.add(a.id));
    else selectedAccounts.clear();
    renderAccounts();
    updateBatchBar();
  }
  function toggleSelectAccount(id) {
    if (selectedAccounts.has(id)) selectedAccounts.delete(id);
    else selectedAccounts.add(id);
    updateBatchBar();
  }
  function updateBatchBar() {
    const bar = $('batchBar');
    const count = selectedAccounts.size;
    const cb = $('selectAllCheckbox');
    if (cb) {
      const filtered = getFilteredAccounts();
      const selectedFiltered = filtered.filter(a => selectedAccounts.has(a.id)).length;
      cb.checked = filtered.length > 0 && selectedFiltered === filtered.length;
      cb.indeterminate = selectedFiltered > 0 && selectedFiltered < filtered.length;
    }
    if (count > 0) {
      bar.classList.remove('hidden');
      $('batchCount').textContent = String(count);
    } else {
      bar.classList.add('hidden');
    }
  }

  function formatSubscriptionLabel(type) {
    const s = (type || '').toUpperCase();
    if (s.includes('POWER')) return t('subscription.power');
    if (s.includes('PRO_PLUS') || s.includes('PROPLUS')) return t('subscription.proPlus');
    if (s.includes('PRO')) return t('subscription.pro');
    if (s.includes('FREE')) return t('subscription.free');
    return type || t('subscription.free');
  }
  function getSubBadge(type) {
    const s = (type || '').toUpperCase();
    if (s.includes('POWER')) return '<span class="badge badge-power">' + escapeHtml(formatSubscriptionLabel(type)) + '</span>';
    if (s.includes('PRO_PLUS') || s.includes('PROPLUS')) return '<span class="badge badge-proplus">' + escapeHtml(formatSubscriptionLabel(type)) + '</span>';
    if (s.includes('PRO')) return '<span class="badge badge-pro">' + escapeHtml(formatSubscriptionLabel(type)) + '</span>';
    return '<span class="badge badge-free">' + escapeHtml(formatSubscriptionLabel(type)) + '</span>';
  }
  // getCodexPlanBadge returns a colored badge for the Codex plan type
  // (free/plus/team/pro). Mirrors the Kiro subscription badge styling so
  // Codex cards have a comparable tier indicator.
  function getCodexPlanBadge(plan) {
    if (!plan) return '';
    var p = String(plan).toLowerCase();
    var cls = 'badge-free';
    if (p === 'pro') cls = 'badge-pro';
    else if (p === 'team') cls = 'badge-proplus';
    else if (p === 'plus') cls = 'badge-info';
    return '<span class="badge ' + cls + '">' + escapeHtml(formatCodexPlan(plan)) + '</span>';
  }
  // getCodexLimitBadge returns a badge for the Codex active limit
  // (premium/standard). Shown alongside the plan badge on Codex cards.
  function getCodexLimitBadge(limit) {
    if (!limit) return '';
    var l = String(limit).toLowerCase();
    var cls = l === 'premium' ? 'badge-pro' : 'badge-free';
    return '<span class="badge ' + cls + '">' + escapeHtml(limit) + '</span>';
  }
  function getTrialBadge(a) {
    if (a.trialStatus === 'ACTIVE' && a.trialUsageLimit > 0) {
      return '<span class="badge badge-trial">' + escapeHtml(t('accounts.trial')) + '</span>';
    }
    return '';
  }
  function formatTrialExpiry(ts) {
    if (!ts) return '';
    const date = new Date(ts * 1000);
    const diffDays = Math.ceil((date - new Date()) / (1000 * 60 * 60 * 24));
    if (diffDays < 0) return '(' + t('accounts.trialExpired') + ')';
    if (diffDays === 0) return '(' + t('accounts.trialToday') + ')';
    if (diffDays <= 7) return '(' + diffDays + t('accounts.trialDays') + ')';
    return '';
  }
  function formatAuthMethod(method) {
    if (!method) return '-';
    const normalized = String(method).toLowerCase();
    if (normalized === 'idc') return t('auth.enterprise');
    if (normalized === 'external_idp') return t('auth.enterpriseSso');
    if (normalized === 'social') return t('auth.social');
    if (normalized === 'external_openai') return t('auth.externalOpenai');
    if (normalized === 'codex') return t('auth.codex');
    if (normalized === 'builderid') return 'BuilderID';
    if (normalized === 'github') return t('local.providerGithub');
    if (normalized === 'google') return t('local.providerGoogle');
    return method;
  }

  // accountCategory returns the category bucket for an account:
  //   'kiro'     — native Kiro/AWS auth (social, idc, external_idp, builderid, api_key)
  //   'codex'    — ChatGPT subscription Codex OAuth
  //   'external' — external OpenAI-compatible provider
  //   'other'    — anything else
  function accountCategory(a) {
    const m = String(a.authMethod || '').toLowerCase();
    if (m === 'codex') return 'codex';
    if (m === 'external_openai') return 'external';
    if (m === 'social' || m === 'idc' || m === 'external_idp' ||
        m === 'builderid' || m === 'api_key' || m === '' ) return 'kiro';
    return 'other';
  }
  function categoryLabel(cat) {
    if (cat === 'kiro') return t('category.kiro');
    if (cat === 'codex') return t('category.codex');
    if (cat === 'external') return t('category.external');
    return t('category.other');
  }
  function categoryIcon(cat) {
    if (cat === 'kiro') return 'fa-solid fa-cloud';
    if (cat === 'codex') return 'fa-solid fa-robot';
    if (cat === 'external') return 'fa-solid fa-plug';
    return 'fa-solid fa-circle-question';
  }
  function getStatusBadge(a) {
    const out = [];
    const isBanned = a.banStatus && a.banStatus !== 'ACTIVE';
    if (isBanned) {
      if (a.banStatus === 'BANNED') out.push('<span class="badge badge-banned">' + escapeHtml(t('accounts.banned')) + '</span>');
      else if (a.banStatus === 'SUSPENDED') out.push('<span class="badge badge-suspended">' + escapeHtml(t('accounts.suspended')) + '</span>');
      out.push('<span class="badge badge-warning">' + escapeHtml(t('accounts.disabled')) + '</span>');
    } else {
      if (!a.hasToken)
        out.push('<span class="badge badge-error">' + escapeHtml(t('accounts.noToken')) + '</span>');
      else
        out.push('<span class="badge badge-success">' + escapeHtml(t('accounts.normal')) + '</span>');
      out.push(a.enabled
        ? '<span class="badge badge-info">' + escapeHtml(t('accounts.enabled')) + '</span>'
        : '<span class="badge badge-warning">' + escapeHtml(t('accounts.disabled')) + '</span>');
    }
    return out.join('');
  }
  function formatTokenExpiry(ts) {
    if (!ts) return '-';
    var d = new Date(ts * 1000);
    var diffMs = d - new Date();
    var diffMins = Math.ceil(diffMs / (1000 * 60));
    if (diffMs <= 0) return t('accounts.expired');
    if (diffMins < 60) return Math.ceil(diffMins) + 'm';
    var diffHrs = Math.floor(diffMins / 60);
    var remMins = diffMins % 60;
    if (diffHrs < 24) return diffHrs + 'h ' + remMins + 'm';
    var diffDays = Math.floor(diffHrs / 24);
    return diffDays + 'd ' + (diffHrs % 24) + 'h';
  }
  function formatNum(n) {
    if (n >= 1e6) return (n / 1e6).toFixed(1) + 'M';
    if (n >= 1e3) return (n / 1e3).toFixed(1) + 'K';
    return n.toString();
  }
  function formatCodexPlan(plan) {
    if (!plan) return '-';
    var labels = { 'free': 'Free', 'plus': 'Plus', 'team': 'Team', 'pro': 'Pro' };
    return labels[plan] || plan;
  }
  // formatCodexAccountLabel returns a friendly, identifiable label for a
  // freshly imported/logged-in Codex account. Preference order:
  //   name + email + plan  →  email + plan  →  name + plan  →  chatgptAccountId
  function formatCodexAccountLabel(acc) {
    if (!acc) return '';
    var parts = [];
    var name = acc.name || acc.nickname || '';
    var email = acc.email || '';
    var plan = acc.planType ? '[' + formatCodexPlan(acc.planType) + ']' : '';
    if (name && email) {
      parts.push(name, email);
    } else if (email) {
      parts.push(email);
    } else if (name) {
      parts.push(name);
    } else if (acc.chatgptAccountId) {
      parts.push('codex-' + String(acc.chatgptAccountId).slice(0, 8));
    } else if (acc.id) {
      parts.push(acc.id.slice(0, 12));
    }
    if (plan) parts.push(plan);
    return parts.join(' ');
  }
  function formatUsageBar(pct) {
    if (pct == null || pct === 0) return '-';
    var cls = pct >= 90 ? 'critical' : pct >= 70 ? 'warning' : 'ok';
    return '<span class="codex-usage-bar"><span class="codex-usage-fill ' + cls + '" style="width:' + pct + '%"></span><span class="codex-usage-label">' + pct + '%</span></span>';
  }
  function formatWindowMinutes(mins) {
    if (!mins) return '-';
    if (mins >= 1440) return (mins / 1440).toFixed(0) + ' days';
    if (mins >= 60) return (mins / 60).toFixed(0) + ' hours';
    return mins + ' mins';
  }
  function formatResetTime(ts) {
    if (!ts) return '-';
    var d = new Date(ts * 1000);
    var diffMs = d - new Date();
    if (diffMs <= 0) return t('accounts.expired');
    var diffHrs = Math.floor(diffMs / (1000 * 60 * 60));
    var diffDays = Math.floor(diffHrs / 24);
    if (diffDays > 0) return diffDays + 'd ' + (diffHrs % 24) + 'h (' + d.toLocaleDateString() + ')';
    return diffHrs + 'h (' + d.toLocaleString() + ')';
  }
  function applyUsageBars(root) {
    qsa('.usage-fill[data-usage-pct]', root).forEach(el => {
      const pct = Math.max(0, Math.min(100, parseFloat(el.dataset.usagePct) || 0));
      el.style.width = pct + '%';
    });
  }

  function renderAccounts() {
    const container = $('accountsList');
    if (!container) return;
    const filtered = getFilteredAccounts();
    if (filtered.length === 0) {
      container.innerHTML = '<div class="empty-state">' + escapeHtml(t('accounts.empty')) + '</div>';
      return;
    }
    // Group by category (kiro / codex / external / other) with section headers
    // so the operator can visually distinguish Kiro-subscription accounts
    // from Codex-subscription accounts at a glance.
    const groups = getFilteredAccountsGrouped();
    let html = '';
    for (const g of groups) {
      html += '<div class="account-category-group" data-category="' + escapeAttr(g.category) + '">' +
        '<div class="account-category-header">' +
        '<span class="account-category-icon"><i class="' + g.icon + '" aria-hidden="true"></i></span>' +
        '<span class="account-category-title">' + escapeHtml(g.label) + '</span>' +
        '<span class="account-category-count">' + g.accounts.length + '</span>' +
        '</div>' +
        g.accounts.map(renderAccountCard).join('') +
        '</div>';
    }
    container.innerHTML = html;
    applyUsageBars(container);
    enhanceCustomSelects(container);
  }

  // renderAccountCard produces the HTML for a single account row. Extracted
  // from renderAccounts so the grouped view can call it per-account without
  // duplicating the card markup.
  function renderAccountCard(a) {
      const usagePct = (a.usagePercent || 0) * 100;
      const usageClass = usagePct > 90 ? 'critical' : usagePct > 70 ? 'high' : '';
      const trialPct = (a.trialUsagePercent || 0) * 100;
      const trialClass = trialPct > 90 ? 'critical' : trialPct > 70 ? 'high' : '';
      const isExternal = (a.authMethod === 'external_openai');
      const isCodex = (a.authMethod === 'codex');
      const extLimit = a.extCreditLimit || 0;
      const extUsed = a.extCreditsUsed || 0;
      const extRemaining = a.extCreditsRemaining || 0;
      const extPct = extLimit > 0 ? (extUsed / extLimit) * 100 : 0;
      const extClass = extPct > 90 ? 'critical' : extPct > 70 ? 'high' : '';
      const isSelected = selectedAccounts.has(a.id);
      const weight = a.weight || 0;
      // Kiro-only badges: subscription type, trial, weight, overage.
      // Codex/external accounts have their own tier indicators and don't
      // use Kiro's overage/weight/machine-id system.
      const isKiroNative = !isCodex && !isExternal;
      const weightBadge = isKiroNative && weight >= 2 ? '<span class="badge badge-warning">' + escapeHtml(t('accounts.weightShort')) + ':' + weight + '</span>' : '';
      const overageBadge = isKiroNative ? renderOverageBadge(a) : '';
      const banned = a.banStatus && a.banStatus !== 'ACTIVE';
      const idAttr = escapeAttr(a.id);
      const displayEmail = getDisplayEmail(a.email, a.id);
      const selectLabel = t('accounts.selectAccount', displayEmail);

      const refreshSvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M23 4v6h-6M1 20v-6h6"/><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15"/></svg>';
      const userSvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2"/><circle cx="12" cy="7" r="4"/></svg>';
      const copySvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>';
      const keySvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 2l-2 2m-7.61 7.61a5.5 5.5 0 1 1-7.778 7.778 5.5 5.5 0 0 1 7.777-7.777zm0 0L15.5 8.5m0 0l3 3L22 8l-3-3m-3.5 3.5L19 5"/></svg>';

      // Codex accounts: show plan + active-limit + chatgpt_account_id badges.
      // These replace the Kiro subscription badge (which defaults to "Free"
      // when subscriptionType is empty — misleading for Codex Plus/Pro plans).
      const codexBadge = isCodex ?
        (getCodexPlanBadge(a.codexPlanType) +
         getCodexLimitBadge(a.codexActiveLimit) +
         (a.chatgptAccountId ? '<span class="badge badge-info">ID: ' + escapeHtml(String(a.chatgptAccountId).slice(0, 8)) + '</span>' : ''))
        : '';

      return '' +
        '<div class="account-card' + (isSelected ? ' selected' : '') + '" data-id="' + idAttr + '">' +
        '<div class="account-header">' +
        '<div class="account-info">' +
        '<input type="checkbox" class="account-checkbox" ' + (isSelected ? 'checked' : '') + ' data-id="' + idAttr + '" aria-label="' + escapeAttr(selectLabel) + '" />' +
        '<div class="account-info-text">' +
        '<div class="account-email">' + escapeHtml(displayEmail) + '</div>' +
        '<div class="account-nickname">' + (a.nickname ? '<span class="nickname-badge">' + escapeHtml(a.nickname) + '</span>' : '') + '</div>' +
        '<div class="account-meta">' +
        (isKiroNative ? getSubBadge(a.subscriptionType) : '') +
        (isKiroNative ? getTrialBadge(a) : '') +
        weightBadge +
        overageBadge +
        '<span class="badge badge-info">' + escapeHtml(formatAuthMethod(a.provider || a.authMethod)) + '</span>' +
        codexBadge +
        getStatusBadge(a) +
        '</div>' +
        '</div>' +
        '</div>' +
        '<div class="account-actions">' +
        '<button class="btn btn-icon btn-sm btn-ghost" data-action="refresh" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.refresh')) + '">' + refreshSvg + '</button>' +
        (a.refreshToken ? '<button class="btn btn-icon btn-sm btn-ghost" data-action="refreshToken" data-id="' + idAttr + '" title="' + escapeAttr(t('detail.refreshToken')) + '">' + keySvg + '</button>' : '') +
        '<button class="btn btn-icon btn-sm btn-ghost" data-action="detail" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.detail')) + '">' + userSvg + '</button>' +
        '<button class="btn btn-icon btn-sm btn-ghost" data-action="copyJSON" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.copyJSON')) + '">' + copySvg + '</button>' +
        (banned && isCodex ? '<button class="btn btn-sm btn-warning" data-action="reauth" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.reauth')) + '">' + escapeHtml(t('accounts.reauth')) + '</button>' : '') +
        (banned ? '' :
          '<button class="btn btn-sm ' + (a.enabled ? 'btn-outline' : 'btn-primary') + '" data-action="toggle" data-id="' + idAttr + '" data-enabled="' + (!a.enabled) + '">' +
          escapeHtml(a.enabled ? t('accounts.disable') : t('accounts.enable')) +
          '</button>') +
        '<button class="btn btn-sm btn-secondary" data-action="test" data-id="' + idAttr + '" id="test-' + idAttr + '">' + escapeHtml(t('accounts.test')) + '</button>' +
        '<button class="btn btn-sm btn-danger" data-action="delete" data-id="' + idAttr + '">' + escapeHtml(t('accounts.delete')) + '</button>' +
        '</div>' +
        '</div>' +
        (a.usageLimit > 0 ?
          '<div class="account-usage">' +
          '<div class="usage-label">' + escapeHtml(t('accounts.mainQuota')) + '</div>' +
          '<div class="usage-bar"><div class="usage-fill ' + usageClass + '" data-usage-pct="' + escapeAttr(usagePct) + '"></div></div>' +
          '<div class="usage-text"><span>' + (a.usageCurrent != null ? a.usageCurrent.toFixed(1) : 0) + ' / ' + (a.usageLimit != null ? a.usageLimit.toFixed(0) : 0) + '</span><span>' + usagePct.toFixed(1) + '%</span></div>' +
          '</div>' : '') +
        (a.trialUsageLimit > 0 ?
          '<div class="account-usage">' +
          '<div class="usage-label">' + escapeHtml(t('accounts.trialQuota')) + ' ' + escapeHtml(formatTrialExpiry(a.trialExpiresAt)) + '</div>' +
          '<div class="usage-bar"><div class="usage-fill ' + trialClass + '" data-usage-pct="' + escapeAttr(trialPct) + '"></div></div>' +
          '<div class="usage-text"><span>' + (a.trialUsageCurrent != null ? a.trialUsageCurrent.toFixed(1) : 0) + ' / ' + (a.trialUsageLimit != null ? a.trialUsageLimit.toFixed(0) : 0) + '</span><span>' + trialPct.toFixed(1) + '%</span></div>' +
          '</div>' : '') +
        (isExternal && extLimit > 0 ?
          '<div class="account-usage">' +
          '<div class="usage-label">' + escapeHtml(t('accounts.extCredits')) +
          ' <button class="btn btn-icon btn-sm btn-ghost" data-action="refreshCredits" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.refreshCredits')) + '">' + refreshSvg + '</button>' +
          (a.extStatus ? ' <span class="badge badge-info">' + escapeHtml(a.extStatus) + '</span>' : '') +
          '</div>' +
          '<div class="usage-bar"><div class="usage-fill ' + extClass + '" data-usage-pct="' + escapeAttr(extPct) + '"></div></div>' +
          '<div class="usage-text"><span>' + extRemaining.toFixed(2) + ' / ' + extLimit.toFixed(0) + ' (' + t('accounts.extUsed') + ' ' + extUsed.toFixed(2) + ')</span><span>' + extPct.toFixed(1) + '%</span></div>' +
          '</div>' : '') +
        (isExternal && extLimit === 0 && a.extCreditsCheckedAt ?
          '<div class="account-usage"><div class="usage-label">' + escapeHtml(t('accounts.extCredits')) +
          ' <button class="btn btn-icon btn-sm btn-ghost" data-action="refreshCredits" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.refreshCredits')) + '">' + refreshSvg + '</button>' +
          '</div><div class="usage-text"><span>' + escapeHtml(t('accounts.extCreditsNoLimit')) + '</span></div></div>' : '') +
        (isCodex && (a.codexPrimaryUsedPercent || a.codexSecondaryUsedPercent || a.codexUsageCheckedAt) ?
          '<div class="account-usage">' +
          '<div class="usage-label">' + escapeHtml(t('detail.codexUsage')) +
          ' <button class="btn btn-icon btn-sm btn-ghost" data-action="refresh" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.refresh')) + '">' + refreshSvg + '</button>' +
          (a.codexPrimaryUsedPercent >= 100 ? ' <button class="btn btn-xs btn-outline" data-action="resetQuota" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.resetQuota')) + '">' + escapeHtml(t('accounts.resetQuota')) + '</button>' : '') +
          '</div>' +
          (a.codexPrimaryUsedPercent ?
            '<div class="usage-bar"><div class="usage-fill ' + (a.codexPrimaryUsedPercent >= 90 ? 'critical' : a.codexPrimaryUsedPercent >= 70 ? 'high' : '') + '" data-usage-pct="' + escapeAttr(a.codexPrimaryUsedPercent) + '"></div></div>' +
            '<div class="usage-text"><span>' + escapeHtml(t('detail.codexPrimaryUsed')) + (a.codexPrimaryResetAt ? ' · ' + escapeHtml(formatResetTime(a.codexPrimaryResetAt)) : '') + '</span><span>' + a.codexPrimaryUsedPercent + '%</span></div>'
            : '<div class="usage-text"><span>' + escapeHtml(t('detail.codexUsageHint')) + '</span></div>') +
          (a.codexSecondaryUsedPercent ?
            '<div class="usage-bar" style="margin-top:0.25rem"><div class="usage-fill ' + (a.codexSecondaryUsedPercent >= 90 ? 'critical' : a.codexSecondaryUsedPercent >= 70 ? 'high' : '') + '" data-usage-pct="' + escapeAttr(a.codexSecondaryUsedPercent) + '"></div></div>' +
            '<div class="usage-text"><span>' + escapeHtml(t('detail.codexSecondaryUsed')) + (a.codexSecondaryResetAt ? ' · ' + escapeHtml(formatResetTime(a.codexSecondaryResetAt)) : '') + '</span><span>' + a.codexSecondaryUsedPercent + '%</span></div>'
            : '') +
          '</div>' : '') +
        (isCodex && !a.codexPrimaryUsedPercent && !a.codexUsageCheckedAt ?
          '<div class="account-usage"><div class="usage-label">' + escapeHtml(t('detail.codexUsage')) +
          ' <button class="btn btn-icon btn-sm btn-ghost" data-action="refresh" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.refresh')) + '">' + refreshSvg + '</button>' +
          '</div><div class="usage-text"><span>' + escapeHtml(t('detail.codexUsageHint')) + '</span></div></div>' : '') +
        '<div class="account-stats">' +
        '<div class="account-stat"><div class="account-stat-value">' + (a.requestCount || 0) + '</div><div class="account-stat-label">' + escapeHtml(t('accounts.requests')) + '</div></div>' +
        '<div class="account-stat"><div class="account-stat-value">' + formatNum(a.totalTokens || 0) + '</div><div class="account-stat-label">' + escapeHtml(t('accounts.tokens')) + '</div></div>' +
        '<div class="account-stat"><div class="account-stat-value">' + (a.totalCredits || 0).toFixed(1) + '</div><div class="account-stat-label">' + escapeHtml(t('accounts.credits')) + '</div></div>' +
        '<div class="account-stat"><div class="account-stat-value">' + escapeHtml(formatTokenExpiry(a.expiresAt)) + '</div><div class="account-stat-label">' + escapeHtml(t('accounts.expiry')) + '</div></div>' +
        '</div>' +
        '</div>';
  }

  // Account actions
  async function refreshAccount(id, card) {
    if (card) card.classList.add('loading');
    try {
      const res = await api('/accounts/' + id + '/refresh', { method: 'POST' });
      const d = await res.json();
      if (d.success) {
        loadAccounts();
        if (d.message) toast(t('accounts.refreshed') + ': ' + d.message, 'success');
      } else {
        toastError(t('accounts.refreshFailed') + ': ' + (d.error || ''));
      }
    } catch (e) {
      toastError(t('accounts.refreshFailed'));
    }
    if (card) card.classList.remove('loading');
  }
  // refreshAccountToken forces an OAuth refresh-token flow for the account.
  // Used by the "Refresh token" button in the detail panel. Returns true
  // on success so the caller can reload the detail modal with fresh data.
  async function refreshAccountToken(id) {
    const dismiss = toast(t('detail.refreshToken') + '…', 'info', { duration: 0 });
    try {
      const res = await api('/accounts/' + id + '/refresh-token', { method: 'POST' });
      const d = await res.json();
      dismiss();
      if (d.success) {
        toast(t('detail.tokenRefreshed'), 'success');
        await loadAccounts();
        // Reopen detail modal with fresh data
        const a = accountsData.find(x => x.id === id);
        if (a) showDetail(id);
        return true;
      }
      toastError(t('detail.tokenRefreshFailed') + ': ' + (d.error || ''));
    } catch (e) {
      dismiss();
      toastError(t('detail.tokenRefreshFailed'));
    }
    return false;
  }
  async function toggleAccount(id, enabled) {
    await api('/accounts/' + id, { method: 'PUT', body: JSON.stringify({ enabled }) });
    loadAccounts();
  }
  // resetAccountQuota clears the Codex primary/secondary usage counters so
  // the pool treats the account as fully available again. Only meaningful
  // for Codex accounts whose codexPrimaryUsedPercent has hit 100.
  async function resetAccountQuota(id, card) {
    const ok = await confirmAction(t('accounts.confirmResetQuota'), {
      title: t('accounts.resetQuota'),
      confirmText: t('common.confirm'),
      variant: 'primary'
    });
    if (!ok) return;
    if (card) card.classList.add('loading');
    try {
      const res = await api('/accounts/' + id + '/reset-quota', { method: 'POST' });
      const d = await res.json();
      if (d.success) {
        toast(d.message || t('accounts.resetQuotaDone'), 'success');
        loadAccounts(); loadStats();
      } else {
        toastError((d.error || t('common.failed')));
      }
    } catch (e) {
      toastError(t('common.failed'));
    }
    if (card) card.classList.remove('loading');
  }
  // reauthAccount force-clears the ban, refreshes the OAuth token, and
  // re-fetches usage for a single banned Codex account. If the refresh
  // token is also dead, the account needs a full re-login via the Add
  // Account → Codex Login flow.
  async function reauthAccount(id, btn) {
    const ok = await confirmAction(t('accounts.confirmReauth'), {
      title: t('accounts.reauth'),
      confirmText: t('accounts.reauth'),
      variant: 'warning'
    });
    if (!ok) return;
    if (btn) { btn.disabled = true; btn.textContent = t('common.loading') || '...'; }
    try {
      // apiRefreshAccount already force-unbans + refreshes token + usage
      const res = await api('/accounts/' + id + '/refresh', { method: 'POST' });
      const d = await res.json();
      if (d.success) {
        toast(d.message || t('accounts.reauthDone'), 'success');
        loadAccounts(); loadStats();
      } else {
        toastError((d.error || t('accounts.reauthFailed')));
      }
    } catch (e) {
      toastError(t('accounts.reauthFailed'));
    }
    if (btn) { btn.disabled = false; btn.textContent = t('accounts.reauth'); }
  }
  // reauthAllBanned bulk-recovers every banned Codex account in one call.
  async function reauthAllBanned() {
    const ok = await confirmAction(t('accounts.confirmReauthAll'), {
      title: t('accounts.reauthAll'),
      confirmText: t('accounts.reauthAll'),
      variant: 'warning'
    });
    if (!ok) return;
    const dismiss = toast(t('accounts.reauthAllProcessing'), 'info', { duration: 0 });
    try {
      const res = await api('/accounts/reauth-all-banned', { method: 'POST' });
      const d = await res.json();
      dismiss();
      if (d.success) {
        toast(d.message || t('accounts.reauthAllDone'), 'success');
        loadAccounts(); loadStats();
      } else {
        toastError((d.error || t('common.failed')));
      }
    } catch (e) {
      dismiss();
      toastError(t('common.failed'));
    }
  }
  async function deleteAccount(id) {
    const ok = await confirmAction(t('accounts.confirmDelete'), {
      title: t('accounts.delete'),
      confirmText: t('accounts.delete'),
      variant: 'danger'
    });
    if (!ok) return;
    try {
      const res = await api('/accounts/' + id, { method: 'DELETE' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.failed'));
      toast(t('accounts.deleteSuccess'), 'danger', { icon: 'fa-solid fa-trash' });
      loadAccounts(); loadStats();
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }
  async function copyAccountJSON(id, btn) {
    try {
      const jsonPromise = api('/accounts/' + id + '/full').then(async res => {
        if (!res.ok) throw new Error('Failed');
        const a = await res.json();
        const { clientId, clientSecret, accessToken, refreshToken } = a;
        return JSON.stringify({ clientId, clientSecret, accessToken, refreshToken }, null, 2);
      });
      await copyText(jsonPromise);
      flashCopySuccess(btn);
      toastPrimary(t('accounts.copyJSONSuccess'));
    } catch (e) {
      toastError(t('common.failed'));
    }
  }
  function flashCopySuccess(btn) {
    if (!btn) return;
    const html = btn.innerHTML, cls = btn.className;
    btn.disabled = true;
    btn.className = 'btn btn-icon btn-sm btn-success';
    btn.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="20 6 9 17 4 12"/></svg>';
    setTimeout(() => { btn.disabled = false; btn.className = cls; btn.innerHTML = html; }, 800);
  }

  // Batch actions
  async function batchAction(action) {
    const ids = Array.from(selectedAccounts);
    if (!ids.length) return;
    const confirmKey = 'batch.confirm' + action.charAt(0).toUpperCase() + action.slice(1);
    const ok = await confirmAction(t(confirmKey, ids.length), {
      title: t('common.confirm'),
      confirmText: t('common.confirm'),
      variant: action === 'disable' ? 'danger' : 'primary'
    });
    if (!ok) return;
    const dismiss = toast(t('batch.processing'), 'info', { duration: 0 });
    try {
      const res = await api('/accounts/batch', { method: 'POST', body: JSON.stringify({ ids, action }) });
      const d = await res.json();
      if (!res.ok || !d.success) throw new Error(d.error || t('common.failed'));
      dismiss();
      if (action === 'refresh') {
        toast(t('batch.refreshResult', d.refreshed || 0, d.failed || 0), d.failed ? 'warning' : 'success');
      } else if (action === 'enable') {
        toast(t('batch.enableResult', d.count || ids.length), 'success');
      } else if (action === 'disable') {
        toast(t('batch.disableResult', d.count || ids.length), 'success');
      } else {
        toast(t('batch.done'), 'success');
      }
      selectedAccounts.clear();
      updateBatchBar();
      loadAccounts(); loadStats();
    } catch (e) {
      dismiss();
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }
  async function batchRefreshModels() {
    const ids = Array.from(selectedAccounts);
    if (!ids.length) return;
    const confirmed = await confirmAction(t('batch.confirmRefreshModels', ids.length), {
      title: t('models.refreshAll'),
      confirmText: t('common.confirm')
    });
    if (!confirmed) return;
    const dismiss = toast(t('detail.refreshModelCache') + '…', 'info', { duration: 0 });
    let ok = 0, fail = 0;
    for (const id of ids) {
      try {
        const res = await api('/accounts/' + id + '/models/refresh', { method: 'POST' });
        const d = await res.json();
        if (d.success) ok++; else fail++;
      } catch { fail++; }
    }
    dismiss();
    toast(t('batch.refreshModelsResult', ok, fail), fail ? 'warning' : 'success');
    selectedAccounts.clear();
    updateBatchBar();
    loadAccounts();
  }
  async function batchDelete() {
    const ids = Array.from(selectedAccounts);
    if (!ids.length) return;
    const confirmed = await confirmAction(t('batch.confirmDelete', ids.length), {
      title: t('accounts.delete'),
      confirmText: t('accounts.delete'),
      variant: 'danger'
    });
    if (!confirmed) return;
    const dismiss = toast(t('batch.deleting'), 'info', { duration: 0 });
    let ok = 0, fail = 0;
    for (const id of ids) {
      try {
        const res = await api('/accounts/' + id, { method: 'DELETE' });
        const d = await res.json().catch(() => ({}));
        if (res.ok && d.success !== false) ok++; else fail++;
      } catch { fail++; }
    }
    dismiss();
    toast(t('batch.deleteResult', ok, fail), fail ? 'warning' : 'success', { icon: 'fa-solid fa-trash' });
    selectedAccounts.clear();
    updateBatchBar();
    loadAccounts(); loadStats();
  }
  async function refreshAllModels() {
    const ok = await confirmAction(t('models.confirmRefreshAll'), {
      title: t('models.refreshAll'),
      confirmText: t('models.refreshAll')
    });
    if (!ok) return;
    const dismiss = toast(t('detail.refreshModelCache') + '…', 'info', { duration: 0 });
    try {
      const res = await api('/accounts/models/refresh', { method: 'POST' });
      const d = await res.json();
      dismiss();
      toast(t('models.refreshAllDone', d.refreshed || 0), 'success');
    } catch (e) {
      dismiss();
      toast(t('common.failed'), 'error');
    }
  }
  async function refreshAllAccounts() {
    const ok = await confirmAction(t('accounts.confirmRefreshAll'), {
      title: t('accounts.refreshAll'),
      confirmText: t('accounts.refreshAll')
    });
    if (!ok) return;
    const dismiss = toast(t('accounts.refreshAll') + '…', 'info', { duration: 0 });
    try {
      const res = await api('/accounts/refresh-all', { method: 'POST' });
      const d = await res.json();
      dismiss();
      loadAccounts();
      const msg = d.message || t('accounts.refreshAllDone', d.refreshed || 0);
      const banned = d.banned || 0;
      if (banned > 0) {
        toast(msg + ' (' + banned + ' banned)', 'warning');
      } else {
        toast(msg, 'success');
      }
    } catch (e) {
      dismiss();
      toast(t('common.failed'), 'error');
    }
  }
  async function refreshAccountModels(id) {
    const dismiss = toast(t('detail.refreshModelCache') + '…', 'info', { duration: 0 });
    try {
      const res = await api('/accounts/' + id + '/models/refresh', { method: 'POST' });
      const d = await res.json();
      dismiss();
      if (d.success) toast(t('detail.refreshModelCache') + ' · ' + (d.count || 0), 'success');
      else toast(t('common.failed') + (d.error ? ': ' + d.error : ''), 'error');
    } catch (e) {
      dismiss();
      toast(t('common.failed'), 'error');
    }
  }
  async function refreshAccountCredits(id, card) {
    if (card) card.classList.add('loading');
    const dismiss = toast(t('accounts.refreshCredits') + '…', 'info', { duration: 0 });
    try {
      const res = await api('/accounts/' + id + '/credits', { method: 'POST' });
      const d = await res.json();
      dismiss();
      if (d.success) {
        const c = d.credits || {};
        toast(t('accounts.extCredits') + ': ' + (c.creditsRemaining != null ? c.creditsRemaining.toFixed(2) : '?') + ' / ' + (c.creditLimit != null ? c.creditLimit.toFixed(0) : '?'), 'success');
        loadAccounts();
      } else {
        toastError(t('accounts.refreshCreditsFailed') + (d.error ? ': ' + d.error : ''));
      }
    } catch (e) {
      dismiss();
      toastError(t('accounts.refreshCreditsFailed'));
    }
    if (card) card.classList.remove('loading');
  }

  // Detail modal
  function detailItem(label, value) {
    return '<div class="detail-item"><div class="detail-label">' + escapeHtml(label) + '</div><div class="detail-value">' + escapeHtml(value) + '</div></div>';
  }
  function showDetail(id) {
    const a = accountsData.find(x => x.id === id);
    if (!a) return;
    const idAttr = escapeAttr(id);
    const isCodex = a.authMethod === 'codex';
    const isExternal = a.authMethod === 'external_openai';
    // Kiro-native accounts use Machine ID, Weight, Overage, and the Kiro
    // subscription/quota system. Codex and external providers don't.
    const isKiroNative = !isCodex && !isExternal;
    $('detailBody').innerHTML =
      '<div class="detail-section"><h4>' + escapeHtml(t('detail.basicInfo')) + '</h4><div class="detail-grid">' +
      detailItem(t('detail.email'), getDisplayEmail(a.email, null)) +
      detailItem(t('detail.userId'), a.userId || '-') +
      detailItem(t('detail.authMethod'), formatAuthMethod(a.provider || a.authMethod)) +
      detailItem(t('detail.region'), a.region || 'us-east-1') +
      (a.baseUrl ? detailItem(t('external.baseUrlLabel'), a.baseUrl) : '') +
      (isCodex && a.codexEmail ? detailItem(t('detail.codexEmail'), a.codexEmail) : '') +
      (isCodex && a.codexName ? detailItem(t('detail.codexName'), a.codexName) : '') +
      (isCodex && a.chatgptAccountId ? detailItem(t('detail.codexChatGPTId'), a.chatgptAccountId) : '') +
      '</div></div>' +

      '<div class="detail-section"><h4>' + escapeHtml(t('detail.nickname')) + '</h4><div class="machine-id-row">' +
      '<input type="text" id="nicknameInput" value="' + escapeAttr(a.nickname || '') + '" placeholder="' + escapeHtml(t('detail.nicknamePlaceholder')) + '" maxlength="30" />' +
      '<button class="btn btn-sm btn-primary" data-detail-action="saveNickname" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.save')) + '</button>' +
      '</div></div>' +

      // Machine ID — Kiro only (used for CodeWhisperer request tracking)
      (isKiroNative ?
        '<div class="detail-section"><h4>' + escapeHtml(t('detail.machineId')) + '</h4><div class="machine-id-row">' +
        '<input type="text" id="machineIdInput" value="' + escapeAttr(a.machineId || '') + '" placeholder="UUID" />' +
        '<button class="btn btn-sm btn-outline" id="generateMachineIdBtn" type="button">' + escapeHtml(t('detail.generate')) + '</button>' +
        '<button class="btn btn-sm btn-primary" data-detail-action="saveMachineId" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.save')) + '</button>' +
        '</div></div>' : '') +

      // Weight — Kiro only (load balancing priority for Kiro pool)
      (isKiroNative ?
        '<div class="detail-section"><h4>' + escapeHtml(t('detail.weight')) + '</h4>' +
        '<div class="form-group">' +
        '<input type="number" id="weightInput" value="' + (a.weight || 0) + '" min="0" max="10" />' +
        '<small>' + escapeHtml(t('detail.weightHint')) + '</small>' +
        '</div>' +
        '<button class="btn btn-sm btn-primary" data-detail-action="saveWeight" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.save')) + '</button>' +
        '</div>' : '') +

      // Overage — Kiro only (AWS Q billing feature, not applicable to
      // Codex/external providers)
      (isKiroNative ?
        '<div class="detail-section">' +
        '<h4>' + escapeHtml(t('detail.overage')) +
        ' <button class="btn btn-sm btn-outline" data-detail-action="refreshOverage" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.overageRefresh')) + '</button>' +
        '</h4>' +
        '<p class="help-block">' + escapeHtml(t('detail.overageHint')) + '</p>' +
        renderOverageBlock(a, idAttr) +
        '</div>' : '') +

      (isExternal ?
        '<div class="detail-section"><h4>' + escapeHtml(t('detail.extCredits')) +
        ' <button class="btn btn-sm btn-outline" data-detail-action="refreshCredits" data-id="' + idAttr + '" type="button">' + escapeHtml(t('accounts.refreshCredits')) + '</button>' +
        '</h4><div class="detail-grid">' +
        detailItem(t('detail.extCreditLimit'), (a.extCreditLimit || 0).toFixed(2)) +
        detailItem(t('detail.extCreditsRemaining'), (a.extCreditsRemaining || 0).toFixed(2)) +
        detailItem(t('detail.extCreditsUsed'), (a.extCreditsUsed || 0).toFixed(2)) +
        detailItem(t('detail.extRequestsCount'), (a.extRequestsCount || 0)) +
        detailItem(t('detail.extTokensUsed'), formatNum(a.extTokensUsed || 0)) +
        detailItem(t('detail.extStatus'), a.extStatus || '-') +
        detailItem(t('detail.extKeyMasked'), a.extKeyMasked || '-') +
        (a.extLastUsedAt ? detailItem(t('detail.extLastUsedAt'), new Date(a.extLastUsedAt * 1000).toLocaleString()) : '') +
        (a.extCreditsCheckedAt ? detailItem(t('detail.extCheckedAt'), new Date(a.extCreditsCheckedAt * 1000).toLocaleString()) : '') +
        '</div></div>' : '') +

      (isCodex ?
        '<div class="detail-section"><h4>' + escapeHtml(t('detail.codexUsage')) +
        ' <button class="btn btn-sm btn-outline" data-detail-action="refresh" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.refreshUsage')) + '</button>' +
        '</h4><div class="detail-grid">' +
        detailItem(t('detail.codexPlanType'), formatCodexPlan(a.codexPlanType)) +
        detailItem(t('detail.codexActiveLimit'), a.codexActiveLimit || '-') +
        '</div>' +
        '<div class="detail-grid" style="margin-top:0.5rem">' +
        detailItem(t('detail.codexPrimaryUsed'), formatUsageBar(a.codexPrimaryUsedPercent || 0)) +
        detailItem(t('detail.codexSecondaryUsed'), formatUsageBar(a.codexSecondaryUsedPercent || 0)) +
        detailItem(t('detail.codexPrimaryWindow'), formatWindowMinutes(a.codexPrimaryWindowMinutes || 0)) +
        detailItem(t('detail.codexPrimaryResetAt'), formatResetTime(a.codexPrimaryResetAt)) +
        detailItem(t('detail.codexSecondaryResetAt'), formatResetTime(a.codexSecondaryResetAt)) +
        '</div>' +
        '<div class="detail-grid" style="margin-top:0.5rem">' +
        (a.codexCreditsKnown ?
          detailItem(t('detail.codexCreditsBalance'), a.codexCreditsBalance || 0) +
          detailItem(t('detail.codexCreditsUnlimited'), a.codexCreditsUnlimited ? '✓' : '✗')
          : detailItem(t('detail.codexCreditsBalance'), t('detail.codexCreditsNA')) +
            detailItem(t('detail.codexCreditsUnlimited'), t('detail.codexCreditsNA'))) +
        (a.codexUsageCheckedAt ? detailItem(t('detail.codexLastChecked'), new Date(a.codexUsageCheckedAt * 1000).toLocaleString()) : '') +
        '</div>' +
        '<p class="help-block">' + escapeHtml(t('detail.codexUsageHint')) + '</p>' +
        '</div>' : '') +

      // Token section — shown for all account types. Includes a manual
      // "Refresh token" button that forces the OAuth refresh-token flow
      // regardless of expiry, plus the last-refreshed timestamp. External
      // OpenAI-compatible providers use a static API key (no refresh token),
      // so the button is hidden for them.
      '<div class="detail-section"><h4>' + escapeHtml(t('detail.tokenSection')) +
      (!isExternal ? ' <button class="btn btn-sm btn-outline" data-detail-action="refreshToken" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.refreshToken')) + '</button>' : '') +
      '</h4><div class="detail-grid">' +
      detailItem(t('detail.tokenExpiry'), formatTokenExpiry(a.expiresAt)) +
      (a.expiresAt ? detailItem(t('detail.tokenExpiryAbs'), new Date(a.expiresAt * 1000).toLocaleString()) : '') +
      (a.tokenRefreshedAt ? detailItem(t('detail.tokenRefreshedAt'), new Date(a.tokenRefreshedAt * 1000).toLocaleString()) : '') +
      '</div></div>' +

      // Proxy URL — Kiro only (Codex/external don't use per-account proxy
      // for upstream calls; they have BaseURL instead)
      (isKiroNative ?
        '<div class="detail-section"><h4>' + escapeHtml(t('detail.proxyURL')) + '</h4><div class="machine-id-row">' +
        '<input type="text" id="proxyURLInput" value="' + escapeAttr(a.proxyURL || '') + '" placeholder="socks5://host:port" />' +
        '<button class="btn btn-sm btn-primary" data-detail-action="saveProxyURL" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.save')) + '</button>' +
        '</div><p class="help-block">' + escapeHtml(t('detail.proxyHint')) + '</p></div>' : '') +

      // Subscription section — Kiro only. Codex account info (plan, email,
      // name, ChatGPT ID, credits) is already shown in Basic Info + Codex
      // Usage sections above, so no separate subscription section needed.
      (isKiroNative ?
        '<div class="detail-section"><h4>' + escapeHtml(t('detail.subscription')) + '</h4><div class="detail-grid">' +
        detailItem(t('detail.subscriptionType'), a.subscriptionTitle || (a.subscriptionType ? formatSubscriptionLabel(a.subscriptionType) : '-')) +
        detailItem(t('detail.mainQuota'), (a.usageCurrent != null ? a.usageCurrent.toFixed(1) : 0) + ' / ' + (a.usageLimit != null ? a.usageLimit.toFixed(0) : 0)) +
        detailItem(t('detail.resetDate'), a.nextResetDate || '-') +
        (a.trialUsageLimit > 0 ?
          detailItem(t('detail.trialQuota'), (a.trialUsageCurrent != null ? a.trialUsageCurrent.toFixed(1) : 0) + ' / ' + a.trialUsageLimit.toFixed(0)) +
          detailItem(t('detail.trialStatus'), a.trialStatus || '-') +
          detailItem(t('detail.trialExpiry'), '-')
          : '') +
        '</div></div>' : '') +

      '<div class="detail-section"><h4>' + escapeHtml(t('detail.statistics')) + '</h4><div class="detail-grid">' +
      detailItem(t('detail.requestCount'), a.requestCount || 0) +
      detailItem(t('detail.errorCount'), a.errorCount || 0) +
      detailItem(t('detail.totalTokens'), formatNum(a.totalTokens || 0)) +
      detailItem(t('detail.totalCredits'), (a.totalCredits || 0).toFixed(2)) +
      '</div></div>' +

      '<div class="detail-section">' +
      '<h4>' + escapeHtml(t('detail.models')) +
      ' <button class="btn btn-sm btn-outline" data-detail-action="loadModels" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.loadModels')) + '</button>' +
      ' <button class="btn btn-sm btn-outline" data-detail-action="refreshModels" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.refreshModelCache')) + '</button>' +
      '</h4>' +
      '<div id="modelsList" class="model-list"></div>' +
      '</div>';

    openDialog('detailModal');
  }
  async function loadModels(id) {
    const c = $('modelsList');
    c.innerHTML = '<p class="empty-state">' + escapeHtml(t('detail.loading')) + '</p>';
    try {
      const res = await api('/accounts/' + id + '/models');
      const d = await res.json();
      if (d.success && d.models) {
        const sorted = d.models.slice().sort((a, b) => {
          if (a.modelId === 'auto') return -1;
          if (b.modelId === 'auto') return 1;
          return (a.rateMultiplier || 1) - (b.rateMultiplier || 1);
        });
        c.innerHTML = sorted.map(m => {
          const ratio = m.rateMultiplier || 1;
          return '<div class="model-item">' +
            '<div class="model-name">' + escapeHtml(m.modelId) + '</div>' +
            '<div class="model-credit"><span class="credit-ratio">' + escapeHtml(t('detail.creditMultiplier', ratio)) + '</span></div>' +
            '<div class="model-info">' + escapeHtml(m.description || '') + '</div>' +
            '</div>';
        }).join('') || '<p class="empty-state">' + escapeHtml(t('detail.noModels')) + '</p>';
      } else {
        c.innerHTML = '<p class="message message-error">' + escapeHtml(t('detail.loadFailed')) + ': ' + escapeHtml(d.error || '') + '</p>';
        toast(t('detail.loadFailed') + (d.error ? ': ' + d.error : ''), 'error');
      }
    } catch (e) {
      c.innerHTML = '<p class="message message-error">' + escapeHtml(t('detail.loadFailed')) + '</p>';
      toast(t('detail.loadFailed'), 'error');
    }
  }
  async function generateMachineId() {
    try {
      const res = await api('/generate-machine-id');
      const d = await res.json();
      if (d.machineId) $('machineIdInput').value = d.machineId;
    } catch (e) {
      toast(t('detail.generateFailed'), 'error');
    }
  }
  async function putAccount(id, body, successMsg) {
    try {
      const res = await api('/accounts/' + id, { method: 'PUT', body: JSON.stringify(body) });
      const d = await res.json();
      if (d.success) {
        toast(successMsg, 'success');
        loadAccounts();
      } else {
        toast(t('detail.saveFailed') + (d.error ? ': ' + d.error : ''), 'error');
      }
    } catch (e) {
      toast(t('detail.saveFailed'), 'error');
    }
  }
  async function saveMachineId(id) {
    const m = $('machineIdInput').value.trim();
    if (m && !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(m) && !/^[0-9a-f]{32}$/i.test(m)) {
      toast(t('detail.machineIdError'), 'warning'); return;
    }
    await putAccount(id, { machineId: m }, t('detail.saved'));
  }
  async function saveWeight(id) {
    const weight = parseInt($('weightInput').value, 10) || 0;
    await putAccount(id, { weight }, t('detail.saved'));
  }
  function renderOverageBadge(a) {
    const status = (a.overageStatus || '').toUpperCase();
    if (status === 'ENABLED') {
      return '<span class="badge badge-warning">' + escapeHtml(t('accounts.overageOn')) + '</span>';
    }
    if (status === 'DISABLED') {
      return '<span class="badge badge-muted">' + escapeHtml(t('accounts.overageOff')) + '</span>';
    }
    return '';
  }
  function renderOverageBlock(a, idAttr) {
    const status = (a.overageStatus || '').toUpperCase();
    const capable = !a.overageCapability || a.overageCapability === 'OVERAGE_CAPABLE';
    const checked = status === 'ENABLED';
    const checkedAt = a.overageCheckedAt ? new Date(a.overageCheckedAt * 1000).toLocaleString() : '-';
    const statusText = status === 'ENABLED' ? t('detail.overageEnabled')
      : status === 'DISABLED' ? t('detail.overageDisabled')
      : t('detail.overageUnknown');
    const disabledAttr = capable ? '' : ' disabled';
    return '<div class="form-group flex items-center gap-2">' +
      '<label class="switch"><input type="checkbox" id="overageSwitchInput-' + idAttr + '" data-detail-action="toggleOverage" data-id="' + idAttr + '" ' + (checked ? 'checked' : '') + disabledAttr + ' /><span class="slider"></span></label>' +
      '<span id="overageSwitchLabel-' + idAttr + '">' + escapeHtml(statusText) + '</span>' +
      '</div>' +
      (capable ? '' : '<p class="help-block" style="color:#ef4444">' + escapeHtml(t('detail.overageNotCapable')) + '</p>') +
      '<div class="detail-grid">' +
      detailItem(t('detail.overageStatus'), status || '-') +
      detailItem(t('detail.overageCap'), a.overageCap ? '$' + Number(a.overageCap).toFixed(2) : '-') +
      detailItem(t('detail.overageRate'), a.overageRate ? '$' + Number(a.overageRate).toFixed(4) : '-') +
      detailItem(t('detail.overageCurrent'), a.currentOverages ? '$' + Number(a.currentOverages).toFixed(4) : '$0') +
      detailItem(t('detail.overageCheckedAt'), checkedAt) +
      '</div>';
  }
  async function toggleOverageSwitch(id, inputEl) {
    const desired = inputEl.checked;
    const labelEl = $('overageSwitchLabel-' + id);
    const oldLabel = labelEl ? labelEl.textContent : '';
    inputEl.disabled = true;
    if (labelEl) labelEl.textContent = t('detail.overageSwitching');
    try {
      const res = await api('/accounts/' + encodeURIComponent(id) + '/overage', {
        method: 'POST',
        body: JSON.stringify({ enabled: desired }),
      });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) {
        throw new Error(d.error || t('accounts.overageSwitchFailed'));
      }
      if (labelEl) {
        labelEl.textContent = d.overageStatus === 'ENABLED' ? t('detail.overageEnabled')
          : d.overageStatus === 'DISABLED' ? t('detail.overageDisabled')
          : t('detail.overageUnknown');
      }
      inputEl.checked = d.overageStatus === 'ENABLED';
      await loadAccounts();
    } catch (e) {
      inputEl.checked = !desired;
      if (labelEl) labelEl.textContent = oldLabel;
      toast(t('accounts.overageSwitchFailed') + ': ' + (e.message || e), 'warning');
    } finally {
      inputEl.disabled = false;
    }
  }
  async function refreshAccountOverage(id) {
    try {
      const res = await api('/accounts/' + encodeURIComponent(id) + '/overage', { method: 'GET' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) {
        throw new Error(d.error || t('accounts.overageSwitchFailed'));
      }
      await loadAccounts();
      showDetail(id);
    } catch (e) {
      toast(t('accounts.overageSwitchFailed') + ': ' + (e.message || e), 'warning');
    }
  }
  async function saveProxyURL(id) {
    const url = $('proxyURLInput').value.trim();
    if (url && !/^(socks5|socks5h|http|https):\/\//.test(url)) {
      toast(t('detail.proxyFormatError'), 'warning'); return;
    }
    await putAccount(id, { proxyURL: url }, t('detail.proxySaved'));
  }
  async function saveNickname(id) {
    const nickname = $('nicknameInput').value.trim();
    await putAccount(id, { nickname: nickname }, t('detail.saved'));
  }
  function closeDetailModal() { closeDialog('detailModal'); }

  // Test flow
  function getTestAccount(id) {
    return accountsData.find(a => a.id === id) || null;
  }
  function getTestModelValue() {
    const choice = $('testModelChoice');
    return (choice && choice.value.trim()) || 'claude-sonnet-4';
  }
  function renderTestLog() {
    const c = $('testModalLog');
    if (!c) return;
    if (!testLogs.length) {
      c.innerHTML = '<div class="test-log-empty">' + escapeHtml(t('accounts.testLog.empty')) + '</div>';
      return;
    }
    c.innerHTML = testLogs.map(log =>
      '<div class="test-log-line ' + escapeAttr(log.type || 'info') + '">' +
      '<span class="test-log-time">' + escapeHtml(log.time) + '</span>' +
      '<span class="test-log-message">' + escapeHtml(log.msg) + '</span>' +
      '</div>'
    ).join('');
    c.scrollTop = c.scrollHeight;
  }
  function addTestLog(msg, type) {
    const time = new Date().toLocaleTimeString();
    testLogs.push({ time, msg, type });
    if (testLogs.length > 100) testLogs.shift();
    renderTestLog();
  }
  function clearTestLog() {
    testLogs = [];
    renderTestLog();
  }
  function renderTestModal() {
    const body = $('testBody');
    if (!body) return;
    const acc = getTestAccount(testModalAccountId);
    const idAttr = escapeAttr(testModalAccountId);
    const email = acc ? getDisplayEmail(acc.email, acc.id) : testModalAccountId;
    const proxy = acc ? (acc.proxyURL || t('accounts.testLog.globalProxy')) : '?';
    const statusText = testModalLoadingModels
      ? t('accounts.testModelsLoading')
      : testModalModelError
        ? t('accounts.testModelsFallback')
        : t('accounts.testModelsReady', testModalModels.length);
    const modelField = testModalLoadingModels
      ? '<div class="test-model-loading">' + escapeHtml(t('accounts.testModelsLoading')) + '</div>'
      : testModalModels.length
        ? '<select id="testModelChoice">' +
        testModalModels.map(m => '<option value="' + escapeAttr(m) + '">' + escapeHtml(m) + '</option>').join('') +
        '</select>'
        : '<input type="text" id="testModelChoice" placeholder="claude-sonnet-4" value="claude-sonnet-4" />';

    body.innerHTML =
      '<div class="test-modal-account">' +
      '<div class="test-modal-account-main">' +
      '<div class="test-modal-email">' + escapeHtml(email) + '</div>' +
      '<div class="test-modal-meta">' +
      '<span>' + escapeHtml(formatAuthMethod(acc && (acc.provider || acc.authMethod))) + '</span>' +
      '<span>' + escapeHtml(proxy) + '</span>' +
      '</div>' +
      '</div>' +
      '<span class="test-modal-status">' + escapeHtml(statusText) + '</span>' +
      '</div>' +
      '<div class="test-modal-grid">' +
      '<div class="form-group test-model-field">' +
      '<label for="testModelChoice">' + escapeHtml(t('accounts.selectModel')) + '</label>' +
      modelField +
      '</div>' +
      '<div class="test-log-card">' +
      '<div class="test-log-header">' +
      '<span class="test-log-title">' + escapeHtml(t('accounts.testLog.title')) + '</span>' +
      '<button class="btn btn-xs btn-outline test-log-clear" id="testLogClear" type="button">' + escapeHtml(t('accounts.testLog.clear')) + '</button>' +
      '</div>' +
      '<div class="test-log-content" id="testModalLog"></div>' +
      '</div>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" id="testModalCancelBtn" type="button">' + escapeHtml(t('common.close')) + '</button>' +
      '<button class="btn btn-primary" id="testRunBtn" data-id="' + idAttr + '" type="button" ' + (testModalLoadingModels ? 'disabled' : '') + '>' + escapeHtml(t('accounts.test')) + '</button>' +
      '</div>';

    if (!testModalLoadingModels) enhanceCustomSelects(body);
    renderTestLog();
  }
  async function testAccount(id) {
    testModalAccountId = id;
    testModalModels = [];
    testModalLoadingModels = true;
    testModalModelError = false;
    testModalRunning = false;
    testLogs = [];
    renderTestModal();
    openDialog('testModal');
    try {
      const res = await api('/accounts/' + id + '/models/cached');
      const d = await res.json();
      testModalModels = Array.isArray(d.models) ? d.models.slice().sort() : [];
    } catch (e) {
      testModalModelError = true;
    } finally {
      testModalLoadingModels = false;
      renderTestModal();
    }
  }
  function closeTestModal() {
    closeAllCustomSelects();
    closeDialog('testModal');
  }
  async function runTestAccount(id, model) {
    if (testModalRunning) return;
    testModalRunning = true;
    const modalBtn = $('testRunBtn');
    if (modalBtn) modalBtn.setAttribute('aria-busy', 'true');
    const acc = accountsData.find(a => a.id === id);
    const email = acc ? getDisplayEmail(acc.email, acc.id) : id;
    const proxy = acc ? (acc.proxyURL || t('accounts.testLog.globalProxy')) : '?';
    addTestLog(t('accounts.testLog.start', email, model, proxy), 'info');
    try {
      const startTime = Date.now();
      const res = await api('/accounts/' + id + '/test', { method: 'POST', body: JSON.stringify({ model }) });
      const elapsed = ((Date.now() - startTime) / 1000).toFixed(1);
      const d = await res.json();
      if (d.success) {
        addTestLog(t('accounts.testLog.success', email, elapsed, d.reply), 'ok');
      } else {
        addTestLog(t('accounts.testLog.failed', email, elapsed, d.error || t('common.unknownError')), 'err');
        // If the account was banned during the test, reload accounts to reflect
        // the new BANNED badge and disable state immediately.
        if (d.banned || d.banStatus === 'BANNED') {
          loadAccounts();
        }
      }
    } catch (e) {
      addTestLog(t('accounts.testLog.error', email, e.message), 'err');
    }
    testModalRunning = false;
    if (modalBtn) modalBtn.removeAttribute('aria-busy');
  }

  // Add-account modal templates
  var METHOD_ICONS = {
    builderid: 'fa-solid fa-id-card',
    iam: 'fa-solid fa-key',
    sso: 'fa-solid fa-shield-halved',
    social: 'fa-brands fa-google',
    kirocli: 'fa-solid fa-database',
    ssocache: 'fa-solid fa-folder-tree',
    local: 'fa-solid fa-folder-open',
    credentials: 'fa-solid fa-code',
    cookie: 'fa-solid fa-cookie-bite',
    codex: 'fa-solid fa-robot',
    ninerouter: 'fa-solid fa-arrow-right-to-bracket'
  };
  function methodCard(type, title, desc) {
    var icon = METHOD_ICONS[type] || 'fa-solid fa-circle-plus';
    return '<button type="button" class="method-card" data-method="' + escapeAttr(type) + '">' +
      '<span class="method-icon"><i class="' + icon + '" aria-hidden="true"></i></span>' +
      '<span class="method-body">' +
      '<span class="method-title">' + escapeHtml(title) + '</span>' +
      '<span class="method-desc">' + escapeHtml(desc) + '</span>' +
      '</span>' +
      '<span class="method-arrow" aria-hidden="true"><i class="fa-solid fa-chevron-right"></i></span>' +
      '</button>';
  }
  function showModal(type) {
    const modal = $('addModal');
    const title = $('modalTitle');
    const body = $('modalBody');
    if (type === 'add') modalAdd(title, body);
    else if (type === 'builderid') modalBuilderId(title, body);
    else if (type === 'iam') modalIam(title, body);
    else if (type === 'sso') modalSso(title, body);
    else if (type === 'social') modalSocial(title, body);
    else if (type === 'kirossi') modalKiroSso(title, body);
    else if (type === 'kirocli') modalKiroCli(title, body);
    else if (type === 'kiroAuto') modalKiroAuto(title, body);
    else if (type === 'kiroToken') modalKiroToken(title, body);
    else if (type === 'ssocache') modalSSOCache(title, body);
    else if (type === 'local') modalLocal(title, body);
    else if (type === 'credentials') modalCredentials(title, body);
    else if (type === 'apikey') modalApiKey(title, body);
    else if (type === 'external') modalExternal(title, body);
    else if (type === 'cookie') modalCookie(title, body);
    else if (type === 'codex') modalCodex(title, body);
    else if (type === 'ninerouter') modalNineRouter(title, body);
    if (!modal.classList.contains('active')) openDialog('addModal');
    enhanceCustomSelects(body);
  }
  function closeModal() {
    closeDialog('addModal');
    iamSession = '';
    if (builderIdPollTimer) { clearTimeout(builderIdPollTimer); builderIdPollTimer = null; }
    builderIdSession = '';
    if (socialPollTimer) { clearTimeout(socialPollTimer); socialPollTimer = null; }
    socialDeviceCode = '';
  }
  function modalAdd(title, body) {
    title.textContent = t('modal.addAccount');
    body.innerHTML =
      // ── Kiro category ──
      '<div class="add-category-section">' +
      '<div class="add-category-header">' +
      '<span class="add-category-icon"><i class="fa-solid fa-cloud" aria-hidden="true"></i></span>' +
      '<span class="add-category-title">' + escapeHtml(t('category.kiro')) + '</span>' +
      '<span class="add-category-desc">' + escapeHtml(t('modal.kiroCategoryDesc')) + '</span>' +
      '</div>' +
      '<div class="method-list">' +
      methodCard('builderid', t('modal.builderIdTitle'), t('modal.builderIdDesc')) +
      methodCard('iam', t('modal.iamTitle'), t('modal.iamDesc')) +
      methodCard('sso', t('modal.ssoTitle'), t('modal.ssoDesc')) +
      methodCard('social', t('modal.socialTitle'), t('modal.socialDesc')) +
      methodCard('kirossi', t('modal.kirossiTitle'), t('modal.kirossiDesc')) +
      methodCard('kirocli', t('modal.kirocliTitle'), t('modal.kirocliDesc')) +
      methodCard('kiroAuto', t('kiroauto.title'), t('kiroauto.desc')) +
      methodCard('kiroToken', t('kirotoken.title'), t('kirotoken.desc')) +
      methodCard('ssocache', t('modal.ssocacheTitle'), t('modal.ssocacheDesc')) +
      methodCard('local', t('modal.localTitle'), t('modal.localDesc')) +
      methodCard('credentials', t('modal.credentialsTitle'), t('modal.credentialsDesc')) +
      methodCard('apikey', t('modal.apikeyTitle'), t('modal.apikeyDesc')) +
      methodCard('external', t('modal.externalTitle'), t('modal.externalDesc')) +
      methodCard('cookie', t('modal.cookieTitle'), t('modal.cookieDesc')) +
      '</div>' +
      '</div>' +
      // ── Codex category ──
      '<div class="add-category-section">' +
      '<div class="add-category-header">' +
      '<span class="add-category-icon"><i class="fa-solid fa-robot" aria-hidden="true"></i></span>' +
      '<span class="add-category-title">' + escapeHtml(t('category.codex')) + '</span>' +
      '<span class="add-category-desc">' + escapeHtml(t('modal.codexCategoryDesc')) + '</span>' +
      '</div>' +
      '<div class="method-list">' +
      methodCard('codex', t('modal.codexTitle'), t('modal.codexDesc')) +
      methodCard('ninerouter', t('modal.ninerouterTitle'), t('modal.ninerouterDesc')) +
      '</div>' +
      '</div>' +
      '<div class="modal-footer"><button class="btn btn-secondary" data-close-add="1" type="button">' + escapeHtml(t('common.cancel')) + '</button></div>';
  }
  function modalBuilderId(title, body) {
    title.textContent = t('modal.builderIdTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.builderIdDesc')) + '</p>' +
      '<div id="builderIdStep1">' +
      '<div class="form-group"><label>' + escapeHtml(t('detail.region')) + '</label><input type="text" id="builderIdRegion" value="us-east-1" /></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="startBuilderIdBtn" type="button">' + escapeHtml(t('builderid.startLogin')) + '</button>' +
      '</div>' +
      '</div>' +
      '<div id="builderIdStep2" class="hidden">' +
      '<div class="message message-info message-center"><p class="builder-code" id="builderIdUserCode"></p><p class="text-xs mt-2">' + escapeHtml(t('builderid.verifyCode')) + '</p></div>' +
      '<div class="form-group mt-4"><label>' + escapeHtml(t('builderid.verifyUrl')) + '</label>' +
      '<div class="endpoint"><span id="builderIdVerifyUrl" class="font-mono text-xs"></span></div>' +
      '<div class="flex gap-2 mt-2">' +
      '<button class="btn btn-sm btn-outline flex-1" id="builderIdOpenBtn" type="button">' + escapeHtml(t('builderid.open')) + '</button>' +
      '<button class="btn btn-sm btn-outline flex-1" id="builderIdCopyBtn" type="button">' + escapeHtml(t('common.copy')) + '</button>' +
      '</div>' +
      '</div>' +
      '<p id="builderIdStatus" class="text-center text-sm mt-4 muted-text">' + escapeHtml(t('builderid.waiting')) + '</p>' +
      '<div class="modal-footer"><button class="btn btn-secondary" id="builderIdCancelBtn" type="button">' + escapeHtml(t('common.cancel')) + '</button></div>' +
      '</div>';
    $('startBuilderIdBtn').addEventListener('click', startBuilderIdLogin);
  }
  function modalIam(title, body) {
    title.textContent = t('modal.iamTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.iamDesc')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('iam.startUrl')) + '</label><input type="text" id="iamStartUrl" placeholder="https://xxx.awsapps.com/start" /></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('detail.region')) + '</label><input type="text" id="iamRegion" value="us-east-1" /></div>' +
      '<div id="iamStep2" class="hidden">' +
      '<div class="form-group"><label>' + escapeHtml(t('iam.loginUrl')) + '</label>' +
      '<div class="endpoint"><span id="iamAuthUrl" class="font-mono text-xs"></span></div>' +
      '<div class="flex gap-2 mt-2">' +
      '<button class="btn btn-sm btn-outline flex-1" id="iamOpenBtn" type="button">' + escapeHtml(t('builderid.open')) + '</button>' +
      '<button class="btn btn-sm btn-outline flex-1" id="iamCopyBtn" type="button">' + escapeHtml(t('common.copy')) + '</button>' +
      '</div>' +
      '</div>' +
      '<p class="text-sm mt-3 success-text">' + escapeHtml(t('iam.completeLogin')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('iam.callbackUrl')) + '</label><input type="text" id="iamCallback" placeholder="http://127.0.0.1:xxx/?code=..." /></div>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="iamBtn" type="button">' + escapeHtml(t('builderid.startLogin')) + '</button>' +
      '</div>';
    $('iamBtn').addEventListener('click', startIamSso);
  }
  function modalSso(title, body) {
    title.textContent = t('modal.ssoTitle');
    body.innerHTML =
      '<div class="help-block">' +
      '<b>' + escapeHtml(t('sso.howToGet')) + '</b>' +
      '<ol class="steps-list">' +
      '<li>' + escapeHtml(t('sso.step1')) + ' <code class="code-inline">view.awsapps.com/start</code></li>' +
      '<li>' + escapeHtml(t('sso.step2')) + '</li>' +
      '<li>' + escapeHtml(t('sso.step3')) + ' <code class="code-inline">x-amz-sso_authn</code></li>' +
      '</ol>' +
      '</div>' +
      '<div class="form-group"><label>' + escapeHtml(t('sso.tokenLabel')) + ' <small>' + escapeHtml(t('sso.tokenHint')) + '</small></label>' +
      '<textarea id="ssoToken" placeholder="' + escapeAttr(t('sso.tokenPlaceholder')) + '"></textarea></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('detail.region')) + '</label><input type="text" id="ssoRegion" value="us-east-1" /></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importSsoBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('importSsoBtn').addEventListener('click', importSsoToken);
  }

  function modalSocial(title, body) {
    title.textContent = t('modal.socialTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.socialDesc')) + '</p>' +
      '<div id="socialStep1">' +
      '<div class="form-group"><label>' + escapeHtml(t('social.provider')) + '</label>' +
      '<select id="socialProvider">' +
      '<option value="google">' + escapeHtml(t('local.providerGoogle')) + '</option>' +
      '<option value="github">' + escapeHtml(t('local.providerGithub')) + '</option>' +
      '</select></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="startSocialBtn" type="button">' + escapeHtml(t('builderid.startLogin')) + '</button>' +
      '</div>' +
      '</div>' +
      '<div id="socialStep2" class="hidden">' +
      '<div class="message message-info message-center"><p class="builder-code" id="socialUserCode"></p><p class="text-xs mt-2">' + escapeHtml(t('social.instructions')) + '</p></div>' +
      '<div class="form-group mt-4"><label>' + escapeHtml(t('builderid.verifyUrl')) + '</label>' +
      '<div class="endpoint"><span id="socialVerifyUrl" class="font-mono text-xs"></span></div>' +
      '<div class="flex gap-2 mt-2">' +
      '<button class="btn btn-sm btn-outline flex-1" id="socialOpenBtn" type="button">' + escapeHtml(t('builderid.open')) + '</button>' +
      '<button class="btn btn-sm btn-outline flex-1" id="socialCopyBtn" type="button">' + escapeHtml(t('common.copy')) + '</button>' +
      '</div>' +
      '</div>' +
      '<p id="socialStatus" class="text-center text-sm mt-4 muted-text">' + escapeHtml(t('builderid.waiting')) + '</p>' +
      '<div class="modal-footer"><button class="btn btn-secondary" id="socialCancelBtn" type="button">' + escapeHtml(t('common.cancel')) + '</button></div>' +
      '</div>';
    $('startSocialBtn').addEventListener('click', startSocialLogin);
  }

  let kiroSsoSessionId = '';
  function modalKiroSso(title, body) {
    title.textContent = t('modal.kirossiTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.kirossiDesc')) + '</p>' +
      '<div id="kiroSsoStep1">' +
      '<div class="form-group"><label>' + escapeHtml(t('detail.region')) + '</label><input type="text" id="kiroSsoRegion" value="us-east-1" /></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="startKiroSsoBtn" type="button">' + escapeHtml(t('kirossi.open')) + '</button>' +
      '</div></div>' +
      '<div id="kiroSsoStep2" class="hidden">' +
      '<p class="help-block">' + escapeHtml(t('kirossi.step1')) + '</p>' +
      '<div class="message message-info"><p class="font-mono text-xs break-all" id="kiroSsoAuthUrl"></p></div>' +
      '<div class="flex gap-2 mt-2">' +
      '<button class="btn btn-sm btn-outline flex-1" id="kiroSsoOpenBtn" type="button">' + escapeHtml(t('kirossi.open')) + '</button>' +
      '<button class="btn btn-sm btn-outline flex-1" id="kiroSsoCopyBtn" type="button">' + escapeHtml(t('common.copy')) + '</button>' +
      '</div>' +
      '<p class="help-block mt-3">' + escapeHtml(t('kirossi.afterSignIn')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('kirossi.callbackLabel')) + '</label>' +
      '<input type="text" id="kiroSsoCallback" placeholder="' + escapeAttr(t('kirossi.callbackPlaceholder')) + '" /></div>' +
      '<div id="kiroSsoEnterpriseRow" class="hidden">' +
      '<p class="message message-warning">' + escapeHtml(t('kirossi.enterpriseNote')) + '</p>' +
      '<div class="message message-info"><p class="font-mono text-xs break-all" id="kiroSsoIdpUrl"></p></div>' +
      '<div class="flex gap-2 mt-2">' +
      '<button class="btn btn-sm btn-outline flex-1" id="kiroSsoIdpOpenBtn" type="button">' + escapeHtml(t('kirossi.open')) + '</button>' +
      '<button class="btn btn-sm btn-outline flex-1" id="kiroSsoIdpCopyBtn" type="button">' + escapeHtml(t('common.copy')) + '</button>' +
      '</div>' +
      '<div class="form-group"><label>' + escapeHtml(t('kirossi.idpCallbackLabel')) + '</label>' +
      '<input type="text" id="kiroSsoIdpCallback" placeholder="' + escapeAttr(t('kirossi.idpCallbackPlaceholder')) + '" /></div>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="completeKiroSsoBtn" type="button">' + escapeHtml(t('kirossi.submit')) + '</button>' +
      '</div></div>';
    $('startKiroSsoBtn').addEventListener('click', startKiroSso);
    $('completeKiroSsoBtn').addEventListener('click', completeKiroSso);
    $('kiroSsoOpenBtn').addEventListener('click', () => { const el = $('kiroSsoAuthUrl'); if (el) window.open(el.textContent, '_blank'); });
    $('kiroSsoCopyBtn').addEventListener('click', async () => { const el = $('kiroSsoAuthUrl'); if (el) await copyText(el.textContent); });
    $('kiroSsoIdpOpenBtn').addEventListener('click', () => { const el = $('kiroSsoIdpUrl'); if (el) window.open(el.textContent, '_blank'); });
    $('kiroSsoIdpCopyBtn').addEventListener('click', async () => { const el = $('kiroSsoIdpUrl'); if (el) await copyText(el.textContent); });
  }
  async function startKiroSso() {
    const region = ($('kiroSsoRegion') && $('kiroSsoRegion').value) || 'us-east-1';
    try {
      const res = await api('/auth/kiro-sso/start', {
        method: 'POST',
        body: JSON.stringify({ region }),
      });
      const d = await res.json();
      if (!d.sessionId) { toastError(t('kirossi.failed') + ': ' + (d.error || 'no session')); return; }
      kiroSsoSessionId = d.sessionId;
      const authUrlEl = $('kiroSsoAuthUrl');
      if (authUrlEl) authUrlEl.textContent = d.authUrl;
      const step1 = $('kiroSsoStep1');
      const step2 = $('kiroSsoStep2');
      if (step1) step1.classList.add('hidden');
      if (step2) step2.classList.remove('hidden');
    } catch (e) { toastError(t('kirossi.failed') + ': ' + (e.message || e)); }
  }
  async function completeKiroSso() {
    const callbackEl = $('kiroSsoCallback');
    const callbackUrl = callbackEl ? callbackEl.value.trim() : '';
    if (!callbackUrl) { toastWarning(t('kirossi.callbackLabel') + ' required'); return; }

    // Check if enterprise descriptor (has issuer_url but no code)
    // We try exchange first; if the backend detects enterprise descriptor, it tells us.
    try {
      let sessionId = kiroSsoSessionId;
      let finalCallback = callbackUrl;

      // Check for IdP callback FIRST (enterprise step 2) — if the user has already
      // pasted the IdP callback, skip enterprise detection even if kiroSsoCallback
      // still contains the old descriptor URL.
      const idpCallbackEl = $('kiroSsoIdpCallback');
      const idpCallback = idpCallbackEl ? idpCallbackEl.value.trim() : '';
      if (idpCallback) {
        finalCallback = idpCallback;
      } else {
        // Check for enterprise descriptor in URL parameters (enterprise step 1)
        const urlObj = new URL(callbackUrl.startsWith('http') ? callbackUrl : 'http://x?' + callbackUrl);
        const issuerUrl = urlObj.searchParams.get('issuer_url');
        const loginOption = urlObj.searchParams.get('login_option');

        if (issuerUrl || loginOption === 'external_idp') {
          // Enterprise flow: step 1 — get IdP auth URL
          const entRes = await api('/auth/kiro-sso/enterprise-start', {
            method: 'POST',
            body: JSON.stringify({ sessionId, callbackUrl }),
          });
          const entData = await entRes.json();
          if (entData.idpAuthUrl) {
            // Show IdP auth URL, expect user to paste IdP callback
            const idpUrlEl = $('kiroSsoIdpUrl');
            if (idpUrlEl) idpUrlEl.textContent = entData.idpAuthUrl;
            const entRow = $('kiroSsoEnterpriseRow');
            if (entRow) entRow.classList.remove('hidden');
            toastPrimary(t('kirossi.enterpriseNote'));
            return;
          } else {
            toastError(t('kirossi.failed') + ': ' + (entData.error || 'No IdP URL'));
            return;
          }
        }
      }

      // Exchange code for tokens
      const res = await api('/auth/kiro-sso/exchange', {
        method: 'POST',
        body: JSON.stringify({ sessionId, callbackUrl: finalCallback }),
      });
      const d = await res.json();
      if (d.success) {
        closeModal(); loadAccounts(); loadStats();
        toastPrimary(t('kirossi.importSuccess') + ': ' + (d.account?.email || d.account?.id));
        if (d.account?.id) autoRefreshNewAccount(d.account.id);
      } else {
        toastError(t('kirossi.failed') + ': ' + (d.error || ''));
      }
    } catch (e) {
      console.error('kiroSso:', e);
      toastError(t('kirossi.failed') + ': ' + (e.message || e));
    }
  }

  async function autoImportKiro() {
    try {
      toastPrimary(t('kiroauto.scanning'));
      const res = await api('/auth/kiro-auto-import');
      const d = await res.json();
      if (d.found) {
        closeModal(); loadAccounts(); loadStats();
        toastPrimary(t('kirocli.importSuccess') + ': ' + (d.account?.email || d.account?.id));
        autoRefreshNewAccount(d.account?.id);
      } else {
        toastWarning(d.error || t('kiroauto.notFound'));
      }
    } catch (e) {
      console.error('autoImportKiro:', e);
      toastError(t('common.failed') + ': ' + (e.message || e));
    }
  }

  function modalKiroAuto(title, body) {
    title.textContent = t('kiroauto.title');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('kiroauto.desc')) + '</p>' +
      '<div class="help-block">' +
      '<p><b>' + escapeHtml(t('kirocli.fileLocation')) + '</b></p>' +
      '<p>' + escapeHtml(t('kirocli.linux')) + ': <code class="code-inline">~/.local/share/kiro-cli/data.sqlite3</code></p>' +
      '<p>' + escapeHtml(t('kirocli.windows')) + ': <code class="code-inline">%APPDATA%\\kiro\\storage.db</code></p>' +
      '<p>' + escapeHtml(t('ssocache.path')) + ': <code class="code-inline">~/.aws/sso/cache/</code></p>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="autoImportKiroBtn" type="button">' + escapeHtml(t('kiroauto.button')) + '</button>' +
      '</div>';
    $('autoImportKiroBtn').addEventListener('click', autoImportKiro);
  }

  function modalKiroCli(title, body) {
    title.textContent = t('modal.kirocliTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.kirocliDesc')) + '</p>' +
      '<div class="help-block">' +
      '<p><b>' + escapeHtml(t('kirocli.fileLocation')) + '</b></p>' +
      '<p>' + escapeHtml(t('kirocli.linux')) + ': <code class="code-inline">~/.local/share/kiro-cli/data.sqlite3</code></p>' +
      '<p>' + escapeHtml(t('kirocli.windows')) + ': <code class="code-inline">%APPDATA%\\kiro\\storage.db</code></p>' +
      '<p><i class="fa-solid fa-circle-info"></i> ' + escapeHtml(t('kirocli.uploadHint')) + '</p>' +
      '</div>' +
      '<div class="form-group">' +
      '<label>' + escapeHtml(t('kirocli.uploadLabel')) + '</label>' +
      '<div class="input-row">' +
      '<span id="kiroCliFileName" class="file-name-display"></span>' +
      '<label class="btn btn-primary btn-sm" style="margin-left:auto;white-space:nowrap">' + escapeHtml(t('kirocli.browse')) +
      '<input type="file" accept=".sqlite3,.db,.sqlite" id="kiroCliFile" class="file-input-hidden" />' +
      '</label>' +
      '</div>' +
      '</div>' +
      '<div class="form-group"><label>' + escapeHtml(t('detail.region')) + '</label>' +
      '<input type="text" id="kiroCliRegion" value="us-east-1" /></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importKiroCliBtn" type="button">' + escapeHtml(t('common.import')) + '</button>' +
      '</div>';
    $('importKiroCliBtn').addEventListener('click', importKiroCli);
    $('kiroCliFile').addEventListener('change', function() {
      var nameSpan = $('kiroCliFileName');
      nameSpan.textContent = this.files && this.files[0] ? this.files[0].name : '';
      nameSpan.style.color = this.files && this.files[0] ? 'var(--text-color, inherit)' : 'var(--text-secondary, #888)';
    });
  }

  function modalSSOCache(title, body) {
    title.textContent = t('modal.ssocacheTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.ssocacheDesc')) + '</p>' +
      '<div class="help-block">' +
      '<p><b>' + escapeHtml(t('kirocli.fileLocation')) + '</b></p>' +
      '<p>' + escapeHtml(t('local.macosLinux')) + ': <code class="code-inline">~/.aws/sso/cache/</code></p>' +
      '<p>' + escapeHtml(t('local.windows')) + ': <code class="code-inline">%USERPROFILE%\\.aws\\sso\\cache\\</code></p>' +
      '</div>' +
      '<div class="form-group"><label>' + escapeHtml(t('detail.region')) + '</label><input type="text" id="ssocacheRegion" value="us-east-1" /></div>' +
      '<p class="message message-info">' + escapeHtml(t('ssocache.hint')) + '</p>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importSSOCacheBtn" type="button">' + escapeHtml(t('common.import')) + '</button>' +
      '</div>';
    $('importSSOCacheBtn').addEventListener('click', importSSOCache);
  }

  function modalLocal(title, body) {
    title.textContent = t('modal.localTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.localDesc')) + '</p>' +
      '<div class="help-block">' +
      '<p><b>' + escapeHtml(t('local.fileLocation')) + '</b></p>' +
      '<p>' + escapeHtml(t('local.windows')) + ': <code class="code-inline">%USERPROFILE%\\.aws\\sso\\cache\\</code></p>' +
      '<p>' + escapeHtml(t('local.macosLinux')) + ': <code class="code-inline">~/.aws/sso/cache/</code></p>' +
      '</div>' +
      '<div class="form-group"><label>' + escapeHtml(t('local.loginChannel')) + '</label>' +
      '<select id="localProvider">' +
      '<option value="BuilderId">' + escapeHtml(t('local.providerBuilderId')) + '</option>' +
      '<option value="Enterprise">' + escapeHtml(t('local.providerEnterprise')) + '</option>' +
      '<option value="Google">' + escapeHtml(t('local.providerGoogle')) + '</option>' +
      '<option value="Github">' + escapeHtml(t('local.providerGithub')) + '</option>' +
      '</select>' +
      '</div>' +
      '<div class="form-group"><label>' + escapeHtml(t('detail.region')) + '</label>' +
      '<input type="text" id="localRegion" placeholder="us-east-1" /></div>' +
      '<div class="form-group">' +
      '<label>' + escapeHtml(t('local.tokenFile')) + ' <small>' + escapeHtml(t('local.tokenRequired')) + '</small></label>' +
      '<div class="input-row">' +
      '<textarea id="localTokenJson" placeholder="' + escapeAttr(t('local.pasteOrUpload')) + '" class="font-mono"></textarea>' +
      '<label class="btn btn-outline btn-sm">' + escapeHtml(t('local.upload')) +
      '<input type="file" accept=".json" id="localTokenFile" class="file-input-hidden" />' +
      '</label>' +
      '</div>' +
      '</div>' +
      '<div id="localClientGroup" class="form-group">' +
      '<label>' + escapeHtml(t('local.clientFile')) + ' <small>' + escapeHtml(t('local.clientRequired')) + '</small></label>' +
      '<div class="input-row">' +
      '<textarea id="localClientJson" placeholder="' + escapeAttr(t('local.pasteOrUpload')) + '" class="font-mono"></textarea>' +
      '<label class="btn btn-outline btn-sm">' + escapeHtml(t('local.upload')) +
      '<input type="file" accept=".json" id="localClientFile" class="file-input-hidden" />' +
      '</label>' +
      '</div>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importLocalBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('localProvider').addEventListener('change', updateLocalFields);
    $('localTokenFile').addEventListener('change', e => loadLocalFile(e.target, 'localTokenJson'));
    $('localClientFile').addEventListener('change', e => loadLocalFile(e.target, 'localClientJson'));
    $('importLocalBtn').addEventListener('click', importLocalKiro);
  }
  function modalCredentials(title, body) {
    title.textContent = t('modal.credentialsTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.credentialsDesc')) + '</p>' +
      '<p class="help-block">' + escapeHtml(t('credentials.batchHint')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('credentials.label')) + '</label>' +
      '<textarea id="credJson" class="font-mono" placeholder=\'[{"refreshToken":"xxx","provider":"BuilderID"}]&#10;or&#10;email----password----refreshToken----clientId----clientSecret\'></textarea>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importCredBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('importCredBtn').addEventListener('click', importCredentials);
  }
  function modalCookie(title, body) {
    title.textContent = t('modal.cookieTitle');
    body.innerHTML =
      '<div class="help-block">' +
      '<p><b>' + escapeHtml(t('cookie.howToGet')) + '</b></p>' +
      '<ol class="steps-list">' +
      '<li>' + escapeHtml(t('cookie.step1')) + ' <a href="' + escapeAttr(t('cookie.link')) + '" target="_blank">' + escapeHtml(t('cookie.link')) + '</a></li>' +
      '<li>' + escapeHtml(t('cookie.step2')) + '</li>' +
      '<li>' + escapeHtml(t('cookie.step3')) + '</li>' +
      '</ol>' +
      '</div>' +
      '<div class="form-group"><label>' + escapeHtml(t('cookie.provider')) + '</label>' +
      '<select id="cookieProvider">' +
      '<option value="Google">' + escapeHtml(t('cookie.google')) + '</option>' +
      '<option value="Github">' + escapeHtml(t('cookie.github')) + '</option>' +
      '</select>' +
      '</div>' +
      '<div class="form-group"><label>' + escapeHtml(t('cookie.refreshToken')) + '</label>' +
      '<textarea id="cookieRefreshToken" class="font-mono" placeholder="' + escapeAttr(t('cookie.refreshTokenPlaceholder')) + '"></textarea>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importCookieBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('importCookieBtn').addEventListener('click', importFromCookie);
  }
  function modalApiKey(title, body) {
    title.textContent = t('modal.apikeyTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.apikeyDesc')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('apikey.keyLabel')) + '</label>' +
      '<textarea id="apikeyValue" class="font-mono" placeholder="' + escapeAttr(t('apikey.keyPlaceholder')) + '"></textarea></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('detail.region')) + '</label>' +
      '<select id="apikeyRegion"><option value="us-east-1">us-east-1</option><option value="eu-central-1">eu-central-1</option></select></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importApiKeyBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('importApiKeyBtn').addEventListener('click', importApiKey);
  }
  function modalExternal(title, body) {
    title.textContent = t('modal.externalTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.externalDesc')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('external.baseUrlLabel')) + '</label>' +
      '<input type="text" id="externalBaseUrl" class="font-mono" placeholder="' + escapeAttr(t('external.baseUrlPlaceholder')) + '" /></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('external.apiKeyLabel')) + '</label>' +
      '<input type="password" id="externalApiKey" class="font-mono" placeholder="' + escapeAttr(t('external.apiKeyPlaceholder')) + '" autocomplete="off" /></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('external.nameLabel')) + '</label>' +
      '<input type="text" id="externalName" placeholder="' + escapeAttr(t('external.namePlaceholder')) + '" /></div>' +
      '<div class="form-group"><label class="flex items-center gap-2"><input type="checkbox" id="externalTest" checked /> ' + escapeHtml(t('external.testNow')) + '</label>' +
      '<span class="help-block text-xs">' + escapeHtml(t('external.testHelp')) + '</span></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importExternalBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('importExternalBtn').addEventListener('click', importExternal);
  }
  async function importExternal() {
    const baseUrl = $('externalBaseUrl').value.trim();
    const apiKey = $('externalApiKey').value.trim();
    if (!baseUrl) return toastWarning(t('external.baseUrlLabel') + ' is required');
    if (!apiKey) return toastWarning(t('external.apiKeyLabel') + ' is required');
    const name = $('externalName').value.trim();
    const test = $('externalTest').checked;
    const btn = $('importExternalBtn');
    btn.disabled = true; btn.textContent = t('common.loading') || '...';
    try {
      const res = await api('/auth/external-provider', { method: 'POST', body: JSON.stringify({ baseUrl, apiKey, name, test }) });
      const d = await res.json();
      if (d.success) {
        closeModal(); loadAccounts(); loadStats();
        let msg = t('external.importSuccess') + ': ' + (d.account?.email || d.account?.id);
        if (test && d.test) {
          if (d.test.error) msg += ' ⚠️ ' + t('external.testFailed') + ': ' + d.test.error;
          else if (d.test.latencyMs) msg += ' (' + d.test.latencyMs + 'ms)';
        }
        toastPrimary(msg, { duration: 6000 });
        autoRefreshNewAccount(d.account?.id);
      } else toastError(t('common.failed') + ': ' + (d.error || ''));
    } catch (e) {
      toastError(t('common.failed') + ': ' + (e.message || e));
    } finally {
      btn.disabled = false; btn.textContent = t('common.add');
    }
  }

  // ==================== Codex (ChatGPT subscription) ====================

  // modalCodex offers two paths: (1) OAuth PKCE login (opens browser to
  // auth.openai.com, callback on port 1455), (2) manual token import for
  // users who already have a ~/.codex/auth.json from the official Codex CLI.
  function modalCodex(title, body) {
    title.textContent = t('modal.codexTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.codexDesc')) + '</p>' +
      '<div class="method-list">' +
      methodCard('codexLogin', t('codex.loginTitle'), t('codex.loginDesc')) +
      methodCard('codexImport', t('codex.importTitle'), t('codex.importDesc')) +
      '</div>' +
      '<div class="modal-footer"><button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button></div>';
    // Wire sub-method cards.
    qsa('.method-card', body).forEach(card => {
      card.addEventListener('click', () => {
        const m = card.dataset.method;
        if (m === 'codexLogin') modalCodexLogin(title, body);
        else if (m === 'codexImport') modalCodexImport(title, body);
      });
    });
  }

  // modalCodexLogin — PKCE browser flow. Starts the OAuth session, shows
  // the authorize URL, polls until the user completes browser auth.
  var codexPollTimer = null;
  function modalCodexLogin(title, body) {
    title.textContent = t('codex.loginTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('codex.loginDesc')) + '</p>' +
      '<div id="codexStep1">' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="codex" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="startCodexLoginBtn" type="button">' + escapeHtml(t('codex.startLogin')) + '</button>' +
      '</div>' +
      '</div>' +
      '<div id="codexStep2" class="hidden">' +
      '<div class="form-group"><label>' + escapeHtml(t('codex.authUrl')) + '</label>' +
      '<div class="endpoint"><span id="codexAuthUrl" class="font-mono text-xs"></span></div>' +
      '<div class="flex gap-2 mt-2">' +
      '<button class="btn btn-sm btn-outline flex-1" id="codexOpenBtn" type="button">' + escapeHtml(t('builderid.open')) + '</button>' +
      '<button class="btn btn-sm btn-outline flex-1" id="codexCopyBtn" type="button">' + escapeHtml(t('common.copy')) + '</button>' +
      '</div>' +
      '</div>' +
      '<p id="codexStatus" class="text-center text-sm mt-4 muted-text">' + escapeHtml(t('builderid.waiting')) + '</p>' +
      '<div class="modal-footer"><button class="btn btn-secondary" id="codexCancelBtn" type="button">' + escapeHtml(t('common.cancel')) + '</button></div>' +
      '</div>';
    $('startCodexLoginBtn').addEventListener('click', startCodexLogin);
  }
  async function startCodexLogin() {
    const btn = $('startCodexLoginBtn');
    btn.disabled = true; btn.textContent = t('common.loading') || '...';
    try {
      const res = await api('/auth/codex/login', { method: 'POST' });
      const d = await res.json();
      if (d.error) { toastError(d.error); return; }
      $('codexStep1').classList.add('hidden');
      $('codexStep2').classList.remove('hidden');
      $('codexAuthUrl').textContent = d.authUrl;
      $('codexOpenBtn').addEventListener('click', () => window.open(d.authUrl, '_blank'));
      $('codexCopyBtn').addEventListener('click', async () => {
        await copyText(d.authUrl);
        toastPrimary(t('common.copied'));
      });
      $('codexCancelBtn').addEventListener('click', cancelCodexLogin);
      pollCodexLogin();
    } catch (e) {
      toastError(t('common.failed') + ': ' + (e.message || e));
    } finally {
      btn.disabled = false; btn.textContent = t('codex.startLogin');
    }
  }
  function pollCodexLogin() {
    if (codexPollTimer) clearTimeout(codexPollTimer);
    codexPollTimer = setTimeout(async () => {
      try {
        const res = await api('/auth/codex/poll', { method: 'POST' });
        const d = await res.json();
        if (d.pending) {
          $('codexStatus').textContent = t('builderid.waiting');
          pollCodexLogin();
          return;
        }
        if (d.error) {
          toastError(d.error);
          return;
        }
        if (d.success) {
          closeModal();
          loadAccounts(); loadStats();
          toastPrimary(t('codex.importSuccess') + ': ' + formatCodexAccountLabel(d.account));
          autoRefreshNewAccount(d.account?.id);
        }
      } catch (e) {
        toastError(t('common.failed') + ': ' + (e.message || e));
      }
    }, 2000);
  }
  async function cancelCodexLogin() {
    if (codexPollTimer) { clearTimeout(codexPollTimer); codexPollTimer = null; }
    try { await api('/auth/codex/cancel', { method: 'POST' }); } catch {}
    showModal('codex');
  }

  // modalCodexImport — manual paste of access_token + refresh_token from
  // ~/.codex/auth.json (for users who already logged in via the official
  // Codex CLI on this or another machine).
  function modalCodexImport(title, body) {
    title.textContent = t('codex.importTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('codex.importDesc')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('codex.accessTokenLabel')) + '</label>' +
      '<textarea id="codexAccessToken" class="font-mono" rows="3" placeholder="' + escapeAttr(t('codex.accessTokenPlaceholder')) + '"></textarea></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('codex.refreshTokenLabel')) + '</label>' +
      '<textarea id="codexRefreshToken" class="font-mono" rows="2" placeholder="' + escapeAttr(t('codex.refreshTokenPlaceholder')) + '"></textarea></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('detail.nickname')) + '</label>' +
      '<input type="text" id="codexNickname" placeholder="' + escapeAttr(t('codex.nicknamePlaceholder')) + '" /></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="codex" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importCodexBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('importCodexBtn').addEventListener('click', importCodexTokens);
  }
  async function importCodexTokens() {
    const accessToken = $('codexAccessToken').value.trim();
    const refreshToken = $('codexRefreshToken').value.trim();
    const nickname = $('codexNickname').value.trim();
    if (!accessToken) return toastWarning(t('codex.accessTokenLabel') + ' is required');
    if (!refreshToken) return toastWarning(t('codex.refreshTokenLabel') + ' is required');
    const btn = $('importCodexBtn');
    btn.disabled = true; btn.textContent = t('common.loading') || '...';
    try {
      const res = await api('/auth/codex-import', { method: 'POST', body: JSON.stringify({ accessToken, refreshToken, nickname }) });
      const d = await res.json();
      if (d.success) {
        closeModal(); loadAccounts(); loadStats();
        toastPrimary(t('codex.importSuccess') + ': ' + formatCodexAccountLabel(d.account));
        autoRefreshNewAccount(d.account?.id);
      } else toastError(d.error || t('common.failed'));
    } catch (e) {
      toastError(t('common.failed') + ': ' + (e.message || e));
    } finally {
      btn.disabled = false; btn.textContent = t('common.add');
    }
  }

  // ==================== 9router import ====================

  // modalNineRouter reads ~/.9router/db.json and shows a preview of the
  // codex + kiro accounts found, with checkboxes to select which to import.
  function modalNineRouter(title, body) {
    title.textContent = t('modal.ninerouterTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('ninerouter.desc')) + '</p>' +
      '<div id="ninerouterPreview"><p class="text-center muted-text">' + escapeHtml(t('common.loading') || '...') + '</p></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '</div>';
    loadNineRouterPreview();
  }
  async function loadNineRouterPreview() {
    const container = $('ninerouterPreview');
    if (!container) return;
    try {
      const res = await api('/auth/import-9router/preview', { method: 'POST' });
      const d = await res.json();
      if (d.error) {
        container.innerHTML = '<div class="message message-error"><p>' + escapeHtml(d.error) + '</p></div>';
        return;
      }
      const codex = d.codex || [];
      const kiro = d.kiro || [];
      const skipped = d.skipped || [];
      if (codex.length === 0 && kiro.length === 0) {
        container.innerHTML = '<div class="message message-info"><p>' + escapeHtml(t('ninerouter.noAccounts')) + '</p></div>';
        return;
      }
      let html = '<p class="text-xs muted-text">' + escapeHtml(t('ninerouter.pathLabel')) + ': <code>' + escapeHtml(d.path) + '</code></p>';
      // Codex group
      if (codex.length > 0) {
        html += '<div class="account-category-header" style="margin-top:0.5rem;">' +
          '<span class="account-category-icon"><i class="fa-solid fa-robot"></i></span>' +
          '<span class="account-category-title">' + escapeHtml(t('category.codex')) + ' (' + codex.length + ')</span>' +
          '</div>';
        html += '<div class="ninerouter-list">';
        codex.forEach((c, i) => {
          const checked = c.hasToken ? 'checked' : '';
          const disabled = c.hasToken ? '' : 'disabled';
          html += '<label class="ninerouter-row' + (c.hasToken ? '' : ' muted-text') + '">' +
            '<input type="checkbox" class="ninerouter-codex-cb" data-idx="' + i + '" ' + checked + ' ' + disabled + ' />' +
            '<span class="ninerouter-name">' + escapeHtml(c.name || '(unnamed)') + '</span>' +
            (c.chatgptAccountId ? '<span class="badge badge-info">ID: ' + escapeHtml(String(c.chatgptAccountId).slice(0, 8)) + '</span>' : '') +
            (c.planType ? '<span class="badge badge-free">' + escapeHtml(c.planType) + '</span>' : '') +
            (c.hasToken ? '' : '<span class="text-xs">' + escapeHtml(t('ninerouter.noToken')) + '</span>') +
            '</label>';
        });
        html += '</div>';
      }
      // Kiro group
      if (kiro.length > 0) {
        html += '<div class="account-category-header" style="margin-top:0.75rem;">' +
          '<span class="account-category-icon"><i class="fa-solid fa-cloud"></i></span>' +
          '<span class="account-category-title">' + escapeHtml(t('category.kiro')) + ' (' + kiro.length + ')</span>' +
          '</div>';
        html += '<div class="ninerouter-list">';
        kiro.forEach((k, i) => {
          const checked = k.hasToken ? 'checked' : '';
          const disabled = k.hasToken ? '' : 'disabled';
          html += '<label class="ninerouter-row' + (k.hasToken ? '' : ' muted-text') + '">' +
            '<input type="checkbox" class="ninerouter-kiro-cb" data-idx="' + i + '" ' + checked + ' ' + disabled + ' />' +
            '<span class="ninerouter-name">' + escapeHtml(k.name || '(unnamed)') + '</span>' +
            (k.profileArn ? '<span class="text-xs">' + escapeHtml(k.profileArn.split('/').pop()) + '</span>' : '') +
            (k.hasToken ? '' : '<span class="text-xs">' + escapeHtml(t('ninerouter.noToken')) + '</span>') +
            '</label>';
        });
        html += '</div>';
      }
      // Skipped providers
      if (skipped.length > 0) {
        html += '<p class="text-xs muted-text" style="margin-top:0.5rem;">' + escapeHtml(t('ninerouter.skipped') + ': ' + skipped.join(', ')) + '</p>';
      }
      html += '<div class="modal-footer" style="margin-top:0.75rem;">' +
        '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
        '<button class="btn btn-primary" id="import9RouterBtn" type="button">' + escapeHtml(t('ninerouter.importSelected')) + '</button>' +
        '</div>';
      container.innerHTML = html;
      $('import9RouterBtn').addEventListener('click', importFrom9Router);
    } catch (e) {
      container.innerHTML = '<div class="message message-error"><p>' + escapeHtml(t('common.failed') + ': ' + (e.message || e)) + '</p></div>';
    }
  }
  async function importFrom9Router() {
    const importCodex = Array.from(qsa('.ninerouter-codex-cb')).some(cb => cb.checked);
    const importKiro = Array.from(qsa('.ninerouter-kiro-cb')).some(cb => cb.checked);
    const btn = $('import9RouterBtn');
    btn.disabled = true; btn.textContent = t('common.loading') || '...';
    try {
      const res = await api('/auth/import-9router', { method: 'POST', body: JSON.stringify({ importCodex, importKiro, refreshKiro: true }) });
      const d = await res.json();
      if (d.success) {
        closeModal(); loadAccounts(); loadStats();
        const ok = d.importedCount || 0;
        const skipped = d.skippedCount || 0;
        let msg = t('ninerouter.importDone') + ': ' + ok + ' imported';
        if (skipped > 0) msg += ', ' + skipped + ' skipped';
        // List imported account names so the user can identify them
        const imported = (d.imported || []).filter(x => x.status === 'imported');
        if (imported.length > 0) {
          var names = imported.map(function(x) {
            var label = x.name || x.email || '';
            if (x.planType) label += ' [' + formatCodexPlan(x.planType) + ']';
            return label || (x.source === 'codex' ? 'codex-' + (x.accountId || '').slice(0,8) : x.email);
          }).filter(Boolean);
          if (names.length > 0) {
            msg += ' — ' + names.join(', ');
          }
        }
        // Surface any per-account errors
        const errors = (d.imported || []).filter(x => x.status === 'error');
        if (errors.length > 0) {
          msg += ' ⚠️ ' + errors.length + ' errors';
        }
        toastPrimary(msg, { duration: 8000 });
      } else toastError(d.error || t('common.failed'));
    } catch (e) {
      toastError(t('common.failed') + ': ' + (e.message || e));
    } finally {
      btn.disabled = false; btn.textContent = t('ninerouter.importSelected');
    }
  }

  function updateLocalFields() {
    const p = $('localProvider').value;
    $('localClientGroup').classList.toggle('hidden', p === 'Google' || p === 'Github');
  }
  function loadLocalFile(input, targetId) {
    const file = input.files[0];
    if (!file) return;
    const r = new FileReader();
    r.onload = e => { $(targetId).value = e.target.result; };
    r.readAsText(file);
  }

  // Import handlers
  async function importLocalKiro() {
    const provider = $('localProvider').value;
    const tokenJson = $('localTokenJson').value.trim();
    const clientJson = $('localClientJson').value.trim();
    const isSocial = provider === 'Google' || provider === 'Github';
    if (!tokenJson) return toastWarning(t('local.tokenMissing'));
    let tokenData, clientData;
    try { tokenData = JSON.parse(tokenJson); } catch { return toastWarning(t('local.tokenInvalid')); }
    if (!tokenData.refreshToken) return toastWarning(t('local.refreshTokenMissing'));
    // Enterprise external IdP (e.g. Kiro IDE via Microsoft/Azure AD): the token
    // file itself carries authMethod=external_idp plus tokenEndpoint/scopes and
    // its own clientId. Refresh goes to the IdP token endpoint, NOT AWS OIDC, so
    // the {hash}.json registration file is irrelevant and must not be required.
    const isExternalIdp = tokenData.authMethod === 'external_idp' || !!tokenData.tokenEndpoint;
    if (!isSocial && !isExternalIdp) {
      if (!clientJson) return toastWarning(t('local.clientMissing'));
      try { clientData = JSON.parse(clientJson); } catch { return toastWarning(t('local.clientInvalid')); }
      if (!clientData.clientId || !clientData.clientSecret) return toastWarning(t('local.clientSecretMissing'));
    }
    const authMethod = isExternalIdp ? 'external_idp' : (clientData ? 'idc' : 'social');
    // Region resolution: explicit input wins, then the token file, then the
    // client/registration file ({hash}.json), which is where AWS SSO cache
    // usually stores it. Without the correct region (e.g. eu-central-1 for
    // many enterprise IdC tenants), requests hit us-east-1 and the upstream
    // rejects the foreign-region profileArn with HTTP 403.
    const regionInput = ($('localRegion')?.value || '').trim();
    const region = regionInput || tokenData.region || clientData?.region || '';
    const payload = {
      refreshToken: tokenData.refreshToken,
      accessToken: tokenData.accessToken || '',
      clientId: isExternalIdp ? (tokenData.clientId || '') : (clientData?.clientId || ''),
      clientSecret: isExternalIdp ? '' : (clientData?.clientSecret || ''),
      region,
      authMethod,
      provider: isExternalIdp ? (tokenData.provider || 'ExternalIdp') : provider,
      tokenEndpoint: isExternalIdp ? (tokenData.tokenEndpoint || '') : '',
      issuerUrl: isExternalIdp ? (tokenData.issuerUrl || '') : '',
      scopes: isExternalIdp ? (tokenData.scopes || '') : ''
    };
    const res = await api('/auth/credentials', { method: 'POST', body: JSON.stringify(payload) });
    const d = await res.json();
    if (d.success) {
      closeModal(); loadAccounts(); loadStats();
      toastPrimary(t('local.importSuccess') + ': ' + (d.account?.email || d.account?.id));
      autoRefreshNewAccount(d.account?.id);
    } else toastError(t('common.failed') + ': ' + (d.error || ''));
  }
  async function importCredentials() {
    const raw = $('credJson').value.trim();
    if (!raw) { toastWarning(t('credentials.jsonError')); return; }
    let items;
    let skipped = 0;
    try {
      const json = JSON.parse(raw);
      if (json.accounts && Array.isArray(json.accounts)) {
        items = json.accounts.map(a => {
          const c = a.credentials || {};
          return {
            refreshToken: c.refreshToken || a.refreshToken,
            clientId: c.clientId || a.clientId,
            clientSecret: c.clientSecret || a.clientSecret,
            region: c.region || a.region,
            authMethod: c.authMethod || a.authMethod,
            provider: c.provider || a.provider || a.idp
          };
        });
      } else {
        items = Array.isArray(json) ? json : [json];
      }
    } catch {
      const parsed = parseLineCredentials(raw);
      items = parsed.items;
      skipped = parsed.skipped;
      if (items.length === 0 && skipped === 0) {
        toastWarning(t('credentials.jsonError'));
        return;
      }
      if (items.length === 0) {
        toastWarning(t('credentials.lineParseAllSkipped', skipped));
        return;
      }
    }
    let ok = 0, fail = 0, newIds = [];
    for (const item of items) {
      if (!item.refreshToken) { fail++; continue; }
      let authMethod = item.authMethod || '';
      if (item.clientId && item.clientSecret) authMethod = 'idc';
      else if (!authMethod || authMethod === 'social') authMethod = 'social';
      else authMethod = authMethod.toLowerCase() === 'idc' ? 'idc' : 'social';
      let provider = item.provider || '';
      if (!provider && authMethod === 'social') provider = 'Google';
      if (!provider && authMethod === 'idc') provider = 'BuilderId';
      const payload = {
        refreshToken: item.refreshToken,
        accessToken: item.accessToken || '',
        clientId: item.clientId || '',
        clientSecret: item.clientSecret || '',
        authMethod, provider,
        region: item.region || 'us-east-1'
      };
      try {
        const res = await api('/auth/credentials', { method: 'POST', body: JSON.stringify(payload) });
        const d = await res.json();
        if (d.success) { ok++; if (d.account?.id) newIds.push(d.account.id); }
        else fail++;
      } catch { fail++; }
    }
    closeModal(); loadAccounts(); loadStats();
    let msg = t('sso.importSuccess', ok);
    if (fail > 0) msg += t('sso.importPartial', fail);
    if (skipped > 0) msg += t('credentials.lineParseSkipped', skipped);
    toastPrimary(msg, { duration: 5200 });
    newIds.forEach(autoRefreshNewAccount);
  }
  function parseLineCredentials(text) {
    const items = [];
    let skipped = 0;
    for (const line of text.split(/\r?\n/)) {
      const trimmed = line.trim();
      if (!trimmed) continue;
      let parts;
      if (trimmed.includes('----')) {
        parts = trimmed.split('----').map(s => s.trim());
      } else if (trimmed.includes('\t')) {
        parts = trimmed.split(/\t+/).map(s => s.trim());
      } else {
        parts = trimmed.split(/\s+/).map(s => s.trim());
      }
      if (parts.length < 5) { skipped++; continue; }
      const refreshToken = parts[2];
      if (!refreshToken) { skipped++; continue; }
      items.push({
        refreshToken,
        clientId: parts[3],
        clientSecret: parts[4],
      });
    }
    return { items, skipped };
  }
  async function importFromCookie() {
    const refreshToken = $('cookieRefreshToken').value.trim();
    if (!refreshToken) return toastWarning(t('cookie.refreshTokenMissing'));
    const provider = $('cookieProvider').value;
    const payload = { refreshToken, accessToken: '', clientId: '', clientSecret: '', authMethod: 'social', provider };
    const res = await api('/auth/credentials', { method: 'POST', body: JSON.stringify(payload) });
    const d = await res.json();
    if (d.success) {
      closeModal(); loadAccounts(); loadStats();
      toastPrimary(t('cookie.importSuccess') + ': ' + (d.account?.email || d.account?.id));
      autoRefreshNewAccount(d.account?.id);
    } else toastError(t('common.failed') + ': ' + (d.error || ''));
  }
  async function importApiKey() {
    const apiKey = $('apikeyValue').value.trim();
    if (!apiKey) return toastWarning(t('apikey.keyLabel') + ' is required');
    const region = $('apikeyRegion').value.trim() || 'us-east-1';
    const res = await api('/auth/kiro-api-key', { method: 'POST', body: JSON.stringify({ apiKey, region }) });
    const d = await res.json();
    if (d.success) {
      closeModal(); loadAccounts(); loadStats();
      toastPrimary(t('apikey.importSuccess') + ': ' + (d.account?.email || d.account?.id));
      autoRefreshNewAccount(d.account?.id);
    } else toastError(t('apikey.importFailed') + ': ' + (d.error || ''));
  }
  async function importSsoToken() {
    const res = await api('/auth/sso-token', {
      method: 'POST', body: JSON.stringify({
        bearerToken: $('ssoToken').value,
        region: $('ssoRegion').value
      })
    });
    const d = await res.json();
    if (d.success) {
      closeModal(); loadAccounts(); loadStats();
      const count = d.accounts?.length || 0;
      const errs = d.errors?.length || 0;
      let msg = t('sso.importSuccess', count);
      if (errs > 0) msg += t('sso.importPartial', errs);
      toastPrimary(msg, { duration: 5200 });
      if (d.accounts) d.accounts.forEach(a => autoRefreshNewAccount(a.id));
    } else toastError(t('common.failed') + ': ' + (d.error || ''));
  }
  var socialPollTimer = null;
  var socialDeviceCode = '';
  async function importKiroCli() {
    try {
      const fileInput = $('kiroCliFile');
      const region = ($('kiroCliRegion') && $('kiroCliRegion').value) || 'us-east-1';
      let payload = { region };

      if (fileInput && fileInput.files && fileInput.files[0]) {
        const file = fileInput.files[0];
        if (file.size > 50 * 1024 * 1024) return toastWarning(t('kirocli.fileTooLarge'));
        const b64 = await new Promise((resolve, reject) => {
          const r = new FileReader();
          r.onload = () => {
            const data = r.result.split(',')[1] || r.result;
            resolve(data);
          };
          r.onerror = () => reject(r.error);
          r.readAsDataURL(file);
        });
        payload.fileContent = b64;
        payload.fileName = file.name;
      }

      const res = await api('/auth/kiro-cli', { method: 'POST', body: JSON.stringify(payload) });
      const d = await res.json();
      if (d.success) {
        closeModal(); loadAccounts(); loadStats();
        toastPrimary(t('kirocli.importSuccess') + ': ' + (d.account?.email || d.account?.id));
        autoRefreshNewAccount(d.account?.id);
      } else toastError(t('common.failed') + ': ' + (d.error || ''));
    } catch (e) {
      console.error('importKiroCli:', e);
      toastError(t('common.failed') + ': ' + (e.message || e));
    }
  }
  async function importSSOCache() {
    const region = ($('ssocacheRegion') && $('ssocacheRegion').value) || 'us-east-1';
    const res = await api('/auth/sso-cache?region=' + encodeURIComponent(region), { method: 'POST' });
    const d = await res.json();
    if (d.success) {
      closeModal(); loadAccounts(); loadStats();
      toastPrimary(t('ssocache.importSuccess') + ': ' + (d.account?.email || d.account?.id));
      autoRefreshNewAccount(d.account?.id);
    } else toastError(t('common.failed') + ': ' + (d.error || ''));
  }
  function modalKiroToken(title, body) {
    title.textContent = t('kirotoken.title');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('kirotoken.desc')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('kirotoken.label')) + '</label>' +
      '<input type="text" id="kiroTokenInput" class="form-control" placeholder="' + escapeHtml(t('kirotoken.placeholder')) + '" /></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('detail.region')) + '</label>' +
      '<input type="text" id="kiroTokenRegion" value="us-east-1" /></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importKiroTokenBtn" type="button">' + escapeHtml(t('kirotoken.button')) + '</button>' +
      '</div>';
    $('importKiroTokenBtn').addEventListener('click', importKiroToken);
  }
  async function importKiroToken() {
    try {
      const refreshToken = $('kiroTokenInput').value.trim();
      const region = ($('kiroTokenRegion') && $('kiroTokenRegion').value) || 'us-east-1';
      if (!refreshToken) return toastWarning(t('kirotoken.label') + ' required');
      const res = await api('/auth/kiro-import', {
        method: 'POST',
        body: JSON.stringify({ refreshToken, region }),
      });
      const d = await res.json();
      if (d.success) {
        closeModal(); loadAccounts(); loadStats();
        toastPrimary(t('kirocli.importSuccess') + ': ' + (d.account?.email || d.account?.id));
        autoRefreshNewAccount(d.account?.id);
      } else toastError(t('common.failed') + ': ' + (d.error || ''));
    } catch (e) {
      console.error('importKiroToken:', e);
      toastError(t('common.failed') + ': ' + (e.message || e));
    }
  }
  async function startSocialLogin() {
    const provider = $('socialProvider').value;
    const res = await api('/auth/social/start', { method: 'POST', body: JSON.stringify({ provider }) });
    const d = await res.json();
    if (d.authUrl) {
      socialDeviceCode = d.deviceCode;
      $('socialUserCode').textContent = d.userCode;
      $('socialVerifyUrl').textContent = d.authUrl;
      $('socialStep1').classList.add('hidden');
      $('socialStep2').classList.remove('hidden');
      $('socialOpenBtn').addEventListener('click', () => window.open($('socialVerifyUrl').textContent, '_blank'));
      $('socialCopyBtn').addEventListener('click', async () => {
        await copyText($('socialVerifyUrl').textContent);
        toast(t('common.copied'), 'primary');
      });
      $('socialCancelBtn').addEventListener('click', cancelSocialLogin);
      pollSocialAuth(d.interval || 5);
    } else toastError(t('common.failed') + ': ' + (d.error || ''));
  }
  function pollSocialAuth(interval) {
    socialPollTimer = setTimeout(async () => {
      const res = await api('/auth/social/poll', { method: 'POST', body: JSON.stringify({ deviceCode: socialDeviceCode, provider: $('socialProvider').value }) });
      const d = await res.json();
      if (d.success) {
        closeModal(); loadAccounts(); loadStats();
        toastPrimary(t('builderid.success') + ': ' + (d.account?.email || d.account?.id));
        autoRefreshNewAccount(d.account?.id);
        socialDeviceCode = '';
      } else if (d.pending) {
        $('socialStatus').textContent = t('builderid.waiting');
        pollSocialAuth(interval);
      } else {
        toastError(t('common.failed') + ': ' + (d.error || ''));
        cancelSocialLogin();
      }
    }, interval * 1000);
  }
  function cancelSocialLogin() {
    if (socialPollTimer) { clearTimeout(socialPollTimer); socialPollTimer = null; }
    socialDeviceCode = '';
    showModal('add');
  }
  async function startBuilderIdLogin() {
    const region = $('builderIdRegion').value || 'us-east-1';
    const res = await api('/auth/builderid/start', { method: 'POST', body: JSON.stringify({ region }) });
    const d = await res.json();
    if (d.sessionId) {
      builderIdSession = d.sessionId;
      $('builderIdUserCode').textContent = d.userCode;
      $('builderIdVerifyUrl').textContent = d.verificationUri;
      $('builderIdStep1').classList.add('hidden');
      $('builderIdStep2').classList.remove('hidden');
      $('builderIdOpenBtn').addEventListener('click', () => window.open($('builderIdVerifyUrl').textContent, '_blank'));
      $('builderIdCopyBtn').addEventListener('click', async () => {
        await copyText($('builderIdVerifyUrl').textContent);
        toast(t('common.copied'), 'primary');
      });
      $('builderIdCancelBtn').addEventListener('click', cancelBuilderIdLogin);
      pollBuilderIdAuth(d.interval || 5);
    } else toastError(t('common.failed') + ': ' + (d.error || ''));
  }
  function pollBuilderIdAuth(interval) {
    builderIdPollTimer = setTimeout(async () => {
      const res = await api('/auth/builderid/poll', { method: 'POST', body: JSON.stringify({ sessionId: builderIdSession }) });
      const d = await res.json();
      if (d.completed) {
        closeModal(); loadAccounts(); loadStats();
        toastPrimary(t('builderid.success') + ': ' + (d.account?.email || d.account?.id));
        autoRefreshNewAccount(d.account?.id);
      } else if (d.success && !d.completed) {
        $('builderIdStatus').textContent = t('builderid.waiting');
        pollBuilderIdAuth(d.interval || interval);
      } else {
        toastError(t('common.failed') + ': ' + (d.error || ''));
        cancelBuilderIdLogin();
      }
    }, interval * 1000);
  }
  function cancelBuilderIdLogin() {
    if (builderIdPollTimer) { clearTimeout(builderIdPollTimer); builderIdPollTimer = null; }
    builderIdSession = '';
    showModal('add');
  }
  async function startIamSso() {
    if (iamSession) {
      const res = await api('/auth/iam-sso/complete', {
        method: 'POST', body: JSON.stringify({
          sessionId: iamSession, callbackUrl: $('iamCallback').value
        })
      });
      const d = await res.json();
      if (d.success) {
        closeModal(); loadAccounts(); loadStats();
        toastPrimary(t('builderid.success') + ': ' + (d.account?.email || d.account?.id));
        autoRefreshNewAccount(d.account?.id);
      } else toastError(t('common.failed') + ': ' + (d.error || ''));
    } else {
      const res = await api('/auth/iam-sso/start', {
        method: 'POST', body: JSON.stringify({
          startUrl: $('iamStartUrl').value, region: $('iamRegion').value
        })
      });
      const d = await res.json();
      if (d.authorizeUrl) {
        iamSession = d.sessionId;
        $('iamAuthUrl').textContent = d.authorizeUrl;
        $('iamStep2').classList.remove('hidden');
        $('iamBtn').textContent = t('iam.complete');
        $('iamOpenBtn').addEventListener('click', () => window.open($('iamAuthUrl').textContent, '_blank'));
        $('iamCopyBtn').addEventListener('click', async () => {
          await copyText($('iamAuthUrl').textContent);
          toast(t('common.copied'), 'primary');
        });
      } else toastError(t('common.failed') + ': ' + (d.error || ''));
    }
  }
  async function autoRefreshNewAccount(id) {
    if (!id) return;
    try { await api('/accounts/' + id + '/refresh', { method: 'POST' }); } catch (e) { }
    loadAccounts();
  }

  // Export modal
  function showExportModal() {
    if (!accountsData.length) return toastWarning(t('accounts.empty'));
    exportSelectedIds = new Set(accountsData.map(a => a.id));
    renderExportModal();
    openDialog('exportModal');
  }
  function closeExportModal() { closeDialog('exportModal'); }
  function renderExportModal() {
    const body = $('exportBody');
    const all = exportSelectedIds.size === accountsData.length;
    body.innerHTML =
      '<div class="flex items-center justify-between mb-3">' +
      '<span class="text-sm muted-text">' + escapeHtml(t('export.selected', exportSelectedIds.size)) + '</span>' +
      '<button class="btn btn-sm btn-outline" id="exportToggleAllBtn" type="button">' + escapeHtml(all ? t('export.deselectAll') : t('export.selectAll')) + '</button>' +
      '</div>' +
      '<div class="export-list">' +
      accountsData.map(a => {
        const checked = exportSelectedIds.has(a.id);
        return '<label class="export-row' + (checked ? ' selected' : '') + '">' +
          '<input type="checkbox" ' + (checked ? 'checked' : '') + ' data-export-toggle="' + escapeAttr(a.id) + '" />' +
          '<div class="export-row-text">' +
          '<div class="export-row-email">' + escapeHtml(getDisplayEmail(a.email, a.id)) + '</div>' +
          '<div class="export-row-meta">' + escapeHtml(formatAuthMethod(a.provider || a.authMethod)) + ' · ' + escapeHtml(formatSubscriptionLabel(a.subscriptionType)) + '</div>' +
          '</div>' +
          '</label>';
      }).join('') +
      '</div>' +
      '<div id="exportJsonPreview" class="hidden mb-3"><textarea id="exportJsonText" readonly class="font-mono"></textarea></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" id="exportCloseBtn" type="button">' + escapeHtml(t('common.cancel')) + '</button>' +
      '<button class="btn btn-outline" id="exportShowJsonBtn" type="button">' + escapeHtml(t('export.showJson')) + '</button>' +
      '<button class="btn btn-outline" id="exportCopyJsonBtn" type="button">' + escapeHtml(t('export.copyJson')) + '</button>' +
      '<button class="btn btn-primary" id="exportDownloadBtn" type="button">' + escapeHtml(t('export.downloadJson')) + '</button>' +
      '</div>';
    $('exportToggleAllBtn').addEventListener('click', () => {
      if (exportSelectedIds.size === accountsData.length) exportSelectedIds.clear();
      else exportSelectedIds = new Set(accountsData.map(a => a.id));
      renderExportModal();
    });
    $('exportCloseBtn').addEventListener('click', closeExportModal);
    $('exportShowJsonBtn').addEventListener('click', exportShowJson);
    $('exportCopyJsonBtn').addEventListener('click', exportCopyJson);
    $('exportDownloadBtn').addEventListener('click', exportDownloadJson);
    qsa('[data-export-toggle]', body).forEach(cb => cb.addEventListener('change', e => {
      const id = e.target.dataset.exportToggle;
      if (exportSelectedIds.has(id)) exportSelectedIds.delete(id);
      else exportSelectedIds.add(id);
      renderExportModal();
    }));
  }
  async function getExportData() {
    if (exportSelectedIds.size === 0) { toastWarning(t('export.noSelection')); return null; }
    const res = await api('/export', { method: 'POST', body: JSON.stringify({ ids: Array.from(exportSelectedIds) }) });
    if (!res.ok) {
      const err = await res.json().catch(() => ({}));
      toastError(t('common.failed') + ': ' + (err.error || t('common.unknownError')));
      return null;
    }
    return res.json();
  }
  async function exportShowJson() {
    const data = await getExportData();
    if (!data) return;
    $('exportJsonPreview').classList.remove('hidden');
    $('exportJsonText').value = JSON.stringify(data, null, 2);
  }
  async function exportCopyJson() {
    if (exportSelectedIds.size === 0) { toastWarning(t('export.noSelection')); return; }
    const jsonPromise = getExportData().then(data => {
      if (!data) throw new Error('no-data');
      const filtered = (data.accounts || []).map(a => {
        const { clientId, clientSecret, accessToken, refreshToken } = a.credentials || {};
        return { clientId, clientSecret, accessToken, refreshToken };
      });
      return JSON.stringify(filtered, null, 2);
    });
    try {
      await copyText(jsonPromise);
      toast(t('export.copied'), 'primary');
    } catch (e) {
      if (e && e.message !== 'no-data') toastError(t('common.failed'));
    }
  }
  async function exportDownloadJson() {
    const data = await getExportData();
    if (!data) return;
    const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = 'kiro-accounts-' + new Date().toISOString().slice(0, 10) + '.json';
    a.click();
    URL.revokeObjectURL(url);
  }

