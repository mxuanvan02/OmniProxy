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
let testModalImageModels = [];
let testModalLoadingImageModels = false;
let testModalImageModelError = false;
let testModalImageSupported = true;
let testModalImageReason = '';
let testModalRunning = false;
let testModalMode = 'chat';
let gommoPlaygroundId = '';
let gommoPlaygroundModels = { image: [], video: [], audio: [] };
let gommoPlaygroundKind = 'image';
let gommoPlaygroundRunning = false;
let gommoPlaygroundResult = null;
let gommoPlaygroundVoice = '';
let collapsedGroups = loadCollapsedGroups();

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
  // { category, label, icon, accounts, subgroups } in display order.
  function getFilteredAccountsGrouped() {
    const filtered = getFilteredAccounts();
    const order = ['kiro', 'codex', 'search', 'image', 'media', 'external', 'other'];
    const groups = {};
    for (const cat of order) groups[cat] = [];
    for (const a of filtered) {
      // A category this list does not name (a provider bucket added later) is
      // filed under "other" rather than throwing on a missing key.
      const cat = accountCategory(a);
      (groups[cat] || groups.other).push(a);
    }
    const out = [];
    for (const cat of order) {
      if (groups[cat].length === 0) continue;
      out.push({
        category: cat,
        label: categoryLabel(cat),
        icon: categoryIcon(cat),
        accounts: groups[cat],
        subgroups: cat === 'external' ? externalSubgroups(groups[cat]) : null,
      });
    }
    return out;
  }

  // externalSubgroups splits the external bucket by upstream. Accounts sharing a
  // base URL share a gateway, a quota and a failure mode, so grouping by it is
  // what keeps the section readable once several gateways are configured. One
  // upstream needs no extra header level, so a single group returns null.
  function externalSubgroups(accounts) {
    const byUpstream = new Map();
    for (const a of accounts) {
      const key = externalGroupKey(a);
      if (!byUpstream.has(key)) byUpstream.set(key, []);
      byUpstream.get(key).push(a);
    }
    if (byUpstream.size < 2) return null;
    return Array.from(byUpstream.entries())
      .sort((x, y) => x[0].localeCompare(y[0]))
      .map(([label, list]) => ({ key: label, label, accounts: list }));
  }

  // externalGroupKey normalizes a base URL into a group label. The path is kept:
  // two providers often share a host and differ only by prefix
  // (/v1 vs /openai/v1), and collapsing those into one group would hide it.
  function externalGroupKey(a) {
    const raw = String((a && a.baseUrl) || '').trim();
    if (!raw) return t('accounts.noBaseUrl') || '(no base URL)';
    return raw.replace(/^https?:\/\//i, '').replace(/\/+$/, '');
  }

  // Collapsed groups persist per browser: an operator who keeps one provider
  // collapsed wants it collapsed on the next visit too, not reset by a reload.
  function loadCollapsedGroups() {
    try {
      const stored = JSON.parse(localStorage.getItem('kiro_collapsedGroups') || '[]');
      return new Set(Array.isArray(stored) ? stored : []);
    } catch (e) {
      return new Set();
    }
  }
  function isGroupCollapsed(key) { return collapsedGroups.has(key); }
  function toggleGroupCollapsed(key) {
    if (collapsedGroups.has(key)) collapsedGroups.delete(key);
    else collapsedGroups.add(key);
    try {
      localStorage.setItem('kiro_collapsedGroups', JSON.stringify(Array.from(collapsedGroups)));
    } catch (e) { /* storage full or blocked: collapse still works for this session */ }
    renderAccounts();
  }
  // onFilterChange is wired to both the keyword input (one event per keystroke)
  // and the status/category selects (one event per choice). renderAccounts()
  // rebuilds the entire grouped list via innerHTML, so repainting per keystroke
  // makes typing in the filter box feel sticky once the pool grows past a few
  // dozen accounts.
  //
  // The select paths render immediately — a single discrete choice should not
  // feel delayed. Only the free-text path is debounced.
  const FILTER_DEBOUNCE_MS = 150;
  let filterDebounceTimer = null;

  function onFilterChange(opts) {
    filterKeyword = $('filterSearch').value;
    filterStatus = $('filterStatusSelect').value;
    const catSel = $('filterCategorySelect');
    if (catSel) filterCategory = catSel.value;
    if (opts && opts.debounce) {
      if (filterDebounceTimer) clearTimeout(filterDebounceTimer);
      filterDebounceTimer = setTimeout(() => {
        filterDebounceTimer = null;
        renderAccounts();
      }, FILTER_DEBOUNCE_MS);
      return;
    }
    if (filterDebounceTimer) {
      clearTimeout(filterDebounceTimer);
      filterDebounceTimer = null;
    }
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
    if (normalized === 'agentrouter' || normalized === 'external_agentrouter') return t('auth.agentrouter');
    if (normalized === 'codex') return t('auth.codex');
    if (normalized === 'antigravity') return t('auth.antigravity');
    if (normalized === 'gommo') return t('auth.gommo');
    if (normalized === 'builderid') return 'BuilderID';
    if (normalized === 'github') return t('local.providerGithub');
    if (normalized === 'google') return t('local.providerGoogle');
    return method;
  }

  // Capability label lookup. Rendering reads this table instead of a hard-coded
  // search/image pair so a capability discovered from a provider catalog shows a
  // real label the moment the backend reports it.
  const CAPABILITY_LABEL_KEYS = {
    'chat': 'category.chat',
    'search': 'category.search',
    'image': 'category.image',
    'embedding': 'category.embedding',
    'audio-stt': 'category.audioStt',
    'audio-tts': 'category.audioTts',
    'moderation': 'category.moderation',
    'video': 'category.video'
  };

  function capabilityLabel(capability) {
    const key = CAPABILITY_LABEL_KEYS[String(capability || '').toLowerCase()];
    // Unknown capabilities still render (raw id) rather than disappearing, so a
    // new backend capability is visible before the locale catches up.
    return key ? t(key) : String(capability || '');
  }

  // accountCapabilities merges three sources: explicitly configured
  // capabilities, the legacy providerKind field, and capabilities discovered
  // from the provider's own /v1/models catalog. Configured values win on order;
  // discovered values are appended so they never mask an explicit setting.
  function accountCapabilities(a) {
    const capabilities = Array.isArray(a && a.capabilities) ? a.capabilities : [];
    const discovered = Array.isArray(a && a.discoveredCapabilities) ? a.discoveredCapabilities : [];
    const kind = String((a && a.providerKind) || '').toLowerCase();
    const result = capabilities.map(value => String(value).toLowerCase().trim()).filter(Boolean);
    if (kind === 'search' && !result.includes('search')) result.push('search');
    if (kind === 'image' && !result.includes('image')) result.push('image');
    discovered.forEach(value => {
      const normalized = String(value).toLowerCase().trim();
      if (normalized && !result.includes(normalized)) result.push(normalized);
    });
    return result;
  }
  // hasConfiguredCapability answers "is this account a service provider of this
  // kind", which is a different question from "can it do this". It matches the
  // backend's accountHasCapability: only providerKind and the explicit
  // Capabilities list count, never discoveredCapabilities.
  //
  // Discovery must stay out: an OpenAI-compatible gateway whose catalog happens
  // to list an image model gets "image" discovered, and treating that as a
  // service kind filed a chat provider under Image Generation and removed it
  // from its own External group — while routing still sent it chat traffic.
  function hasConfiguredCapability(a, capability) {
    const wanted = String(capability || '').toLowerCase().trim();
    if (!wanted) return false;
    if (String((a && a.providerKind) || '').toLowerCase().trim() === wanted) return true;
    const capabilities = Array.isArray(a && a.capabilities) ? a.capabilities : [];
    return capabilities.some(value => String(value).toLowerCase().trim() === wanted);
  }

  // capabilityProbeState answers a question the catalog cannot: did the endpoint
  // actually answer? A provider can advertise an embedding model with no channel
  // behind it (503) or not implement the path at all (404), so a badge derived
  // only from /v1/models overstates what is callable.
  //   'verified' - a probe got 2xx
  //   'failed'   - a probe ran and did not get 2xx
  //   ''         - never probed; the badge stays neutral rather than claiming
  //                either way
  function capabilityProbeState(a, capability) {
    const probes = (a && a.capabilityProbes) || null;
    if (!probes || typeof probes !== 'object') return '';
    const probe = probes[String(capability || '').toLowerCase()];
    if (!probe || typeof probe !== 'object') return '';
    return probe.ok ? 'verified' : 'failed';
  }

  // capabilityBadge renders one capability with its verification state encoded
  // in colour, and the probe outcome in the tooltip so an operator can tell
  // "never probed" from "probed and broken" without opening the console.
  function capabilityBadge(a, capability) {
    const state = capabilityProbeState(a, capability);
    const probe = ((a && a.capabilityProbes) || {})[String(capability || '').toLowerCase()] || {};
    let cls = 'badge-info';
    let mark = '';
    let title = t('capability.notProbed');
    if (state === 'verified') {
      cls = 'badge-success';
      mark = ' \u2713';
      title = t('capability.verified') + (probe.latencyMs ? ' (' + probe.latencyMs + 'ms)' : '');
    } else if (state === 'failed') {
      cls = 'badge-danger';
      mark = ' \u2717';
      title = t('capability.probeFailed') +
        (probe.status ? ' HTTP ' + probe.status : '') +
        (probe.detail ? ' \u2014 ' + String(probe.detail).slice(0, 160) : '');
    }
    return '<span class="badge ' + cls + '" title="' + escapeHtml(title) + '">' +
      escapeHtml(capabilityLabel(capability)) + mark + '</span>';
  }
  function isServiceAccountUI(a) {
    return hasConfiguredCapability(a, 'search') || hasConfiguredCapability(a, 'image');
  }
  function isCodexAccountUI(a) {
    return String((a && a.authMethod) || '').toLowerCase() === 'codex';
  }
  function isAgentRouterAccountUI(a) {
    const method = String((a && a.authMethod) || '').toLowerCase();
    return method === 'agentrouter' || method === 'external_agentrouter';
  }

  // accountCategory returns the category bucket for an account. Service
  // accounts are identified by their explicit capability metadata, not by
  // authMethod (all 9router service accounts use an API key).
  function accountCategory(a) {
    // Gommo is checked before the capability buckets: it carries the "image"
    // capability, so a capability-first test would file a media provider under
    // the search/image service bucket and hide it from its own group.
    if (String((a && a.authMethod) || '').toLowerCase() === 'gommo') return 'media';
    if (hasConfiguredCapability(a, 'search')) return 'search';
    if (hasConfiguredCapability(a, 'image')) return 'image';
    const m = String(a.authMethod || '').toLowerCase();
    if (m === 'codex') return 'codex';
    if (m === 'antigravity') return 'external';
    if (m === 'external_openai' || m === 'agentrouter' || m === 'external_agentrouter') return 'external';
    if (m === 'social' || m === 'idc' || m === 'external_idp' ||
        m === 'builderid' || m === 'api_key' || m === '' ) return 'kiro';
    return 'other';
  }
  function categoryLabel(cat) {
    if (cat === 'kiro') return t('category.kiro');
    if (cat === 'codex') return t('category.codex');
    if (cat === 'search') return t('category.search');
    if (cat === 'image') return t('category.image');
    if (cat === 'external') return t('category.external');
    if (cat === 'media') return t('category.media');
    return t('category.other');
  }
  function categoryIcon(cat) {
    if (cat === 'kiro') return 'fa-solid fa-cloud';
    if (cat === 'codex') return 'fa-solid fa-robot';
    if (cat === 'search') return 'fa-solid fa-magnifying-glass';
    if (cat === 'image') return 'fa-solid fa-image';
    if (cat === 'external') return 'fa-solid fa-plug';
    if (cat === 'media') return 'fa-solid fa-photo-film';
    return 'fa-solid fa-circle-question';
  }
  function getStatusBadge(a) {
    const out = [];
    // Reason tooltip: without it, BANNED and REAUTH_REQUIRED look equally
    // opaque and the operator cannot tell an upstream termination from an
    // expired session without opening the detail drawer.
    const reasonAttr = a.banReason
      ? ' title="' + escapeAttr(String(a.banReason)) + '"'
      : '';
    if (a.banStatus === 'REAUTH_REQUIRED') {
      out.push('<span class="badge badge-reauth-required"' + reasonAttr + '>' + escapeHtml(t('accounts.reauthRequired')) + '</span>');
      out.push('<span class="badge badge-warning">' + escapeHtml(t('accounts.disabled')) + '</span>');
      return out.join('');
    }
    const isBanned = a.banStatus && a.banStatus !== 'ACTIVE';
    if (isBanned) {
      if (a.banStatus === 'BANNED') out.push('<span class="badge badge-banned"' + reasonAttr + '>' + escapeHtml(t('accounts.banned')) + '</span>');
      else if (a.banStatus === 'SUSPENDED') out.push('<span class="badge badge-suspended"' + reasonAttr + '>' + escapeHtml(t('accounts.suspended')) + '</span>');
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
  // Full operational label for identifying the account behind an error.
  // Uses non-secret metadata only; credentials are never rendered here.
  // opts.includeId controls whether the full account ID is appended.
  // Card headers stay readable (name + email only) while detail views, test
  // logs, and error surfaces keep the full ID so an operator can trace the
  // exact account behind a failure. The card still carries the full identity
  // in its title attribute, so no traceability is lost.
  function getAccountIdentityLabel(a, fallbackId, opts) {
    const includeId = !opts || opts.includeId !== false;
    if (!a) return fallbackId || '-';
    const name = a.nickname || a.name || '';
    const email = a.email ? getDisplayEmail(a.email, null) : '';
    const id = a.id || fallbackId || '';
    const parts = [];
    if (name) parts.push(name);
    if (email && email !== name) parts.push(email);
    if (includeId && id && id !== name && id !== email) parts.push('ID: ' + id);
    if (parts.length === 0 && id) parts.push(id);
    return parts.join(' · ') || '-';
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

  // groupHeader renders one collapsible section header. The whole header is the
  // toggle, so the click target matches what the operator sees; the icon slot is
  // omitted for subgroups, which sit under a category that already carries one.
  function groupHeader(key, label, count, collapsed, icon, cls) {
    return '<div class="' + cls + '" role="button" tabindex="0" data-group-toggle="' + escapeAttr(key) + '"' +
      ' aria-expanded="' + (!collapsed) + '">' +
      '<span class="account-category-chevron"><i class="fa-solid fa-chevron-' + (collapsed ? 'right' : 'down') + '" aria-hidden="true"></i></span>' +
      (icon ? '<span class="account-category-icon"><i class="' + icon + '" aria-hidden="true"></i></span>' : '') +
      '<span class="account-category-title">' + escapeHtml(label) + '</span>' +
      '<span class="account-category-count">' + count + '</span>' +
      '</div>';
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
      const collapsed = isGroupCollapsed(g.category);
      let inner = '';
      if (!collapsed) {
        if (g.subgroups) {
          for (const sub of g.subgroups) {
            const subKey = g.category + ':' + sub.key;
            const subCollapsed = isGroupCollapsed(subKey);
            inner += '<div class="account-subgroup">' +
              groupHeader(subKey, sub.label, sub.accounts.length, subCollapsed, null, 'account-subgroup-header') +
              (subCollapsed ? '' : sub.accounts.map(renderAccountCard).join('')) +
              '</div>';
          }
        } else {
          inner = g.accounts.map(renderAccountCard).join('');
        }
      }
      html += '<div class="account-category-group" data-category="' + escapeAttr(g.category) + '">' +
        groupHeader(g.category, g.label, g.accounts.length, collapsed, g.icon, 'account-category-header') +
        inner +
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
      const isSearch = hasConfiguredCapability(a, 'search');
      const isImage = hasConfiguredCapability(a, 'image');
      const isService = isSearch || isImage;
      const isGommo = String(a.authMethod || '').toLowerCase() === 'gommo';
      const extLimit = a.extCreditLimit || 0;
      const extUsed = a.extCreditsUsed || 0;
      const extRemaining = a.extCreditsRemaining || 0;
      const extPct = extLimit > 0 ? (extUsed / extLimit) * 100 : 0;
      const extClass = extPct > 90 ? 'critical' : extPct > 70 ? 'high' : '';
      const isSelected = selectedAccounts.has(a.id);
      const weight = a.weight || 0;
      // Kiro-only badges: subscription type, trial, weight, overage.
      // Codex/external/service accounts have their own metadata and don't use
      // Kiro's overage/weight/machine-id system.
      const isKiroNative = !isCodex && !isExternal && !isService;
      // Weight applies to the whole chat pool, not just Kiro: pool/account.go
      // expands each candidate effectiveWeight(a.Weight) times regardless of
      // authMethod. Only service accounts (search/image) ignore it.
      const weightBadge = !isService && weight >= 2 ? '<span class="badge badge-warning">' + escapeHtml(t('accounts.weightShort')) + ':' + weight + '</span>' : '';
      const overageBadge = isKiroNative ? renderOverageBadge(a) : '';
      const reauthRequired = isCodex && a.banStatus === 'REAUTH_REQUIRED';
      const banned = a.banStatus && a.banStatus !== 'ACTIVE';
      const idAttr = escapeAttr(a.id);
      const displayEmail = getDisplayEmail(a.email, a.id);
      const accountIdentity = getAccountIdentityLabel(a, a.id);
      // Card header shows name/email only. The raw UUID is noise at a glance;
      // it stays in the title attribute and in detail/test/error surfaces.
      const accountIdentityShort = getAccountIdentityLabel(a, a.id, { includeId: false });
      const selectLabel = t('accounts.selectAccount', accountIdentityShort);

      const refreshSvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M23 4v6h-6M1 20v-6h6"/><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15"/></svg>';
      const userSvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2"/><circle cx="12" cy="7" r="4"/></svg>';
      const copySvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>';
      const keySvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 2l-2 2m-7.61 7.61a5.5 5.5 0 1 1-7.778 7.778 5.5 5.5 0 0 1 7.777-7.777zm0 0L15.5 8.5m0 0l3 3L22 8l-3-3m-3.5 3.5L19 5"/></svg>';
      const externalLinkSvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M15 3h6v6"/><path d="M10 14 21 3"/><path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6"/></svg>';

      // Codex accounts: show plan + active-limit + chatgpt_account_id badges.
      // These replace the Kiro subscription badge (which defaults to "Free"
      // when subscriptionType is empty — misleading for Codex Plus/Pro plans).
      // Raw identifiers are not rendered on the card. They stay reachable via
      // the account detail view (and the card's title attribute), so the card
      // shows only human-meaningful metadata.
      const codexBadge = isCodex ?
        (getCodexPlanBadge(a.codexPlanType) +
         getCodexLimitBadge(a.codexActiveLimit))
        : '';
      const serviceBadges = isService ?
        '<span class="badge badge-info">' + escapeHtml(a.provider || a.providerKind || t('accounts.serviceProvider')) + '</span>' +
        accountCapabilities(a).map(cap => capabilityBadge(a, cap)).join('') : '';

      return '' +
        '<div class="account-card' + (isSelected ? ' selected' : '') + (reauthRequired ? ' reauth-required' : '') + '" data-id="' + idAttr + '">' +
        '<div class="account-header">' +
        '<div class="account-info">' +
        '<input type="checkbox" class="account-checkbox" ' + (isSelected ? 'checked' : '') + ' data-id="' + idAttr + '" aria-label="' + escapeAttr(selectLabel) + '" />' +
        '<div class="account-info-text">' +
        '<div class="account-email" title="' + escapeAttr(accountIdentity) + '">' + escapeHtml(accountIdentityShort) + '</div>' +
        '<div class="account-nickname">' + (a.nickname ? '<span class="nickname-badge">' + escapeHtml(a.nickname) + '</span>' : '') + '</div>' +
        '<div class="account-meta">' +
        (isKiroNative ? getSubBadge(a.subscriptionType) : '') +
        (isKiroNative ? getTrialBadge(a) : '') +
        weightBadge +
        overageBadge +
        '<span class="badge badge-info">' + escapeHtml(formatAuthMethod(a.provider || a.authMethod)) + '</span>' +
        codexBadge +
        serviceBadges +
        getStatusBadge(a) +
        '</div>' +
        '</div>' +
        '</div>' +
        '<div class="account-actions">' +
        '<button class="btn btn-icon btn-sm btn-ghost" data-action="refresh" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.refresh')) + '">' + refreshSvg + '</button>' +
        (a.refreshToken && !isService ? '<button class="btn btn-icon btn-sm btn-ghost" data-action="refreshToken" data-id="' + idAttr + '" title="' + escapeAttr(t('detail.refreshToken')) + '">' + keySvg + '</button>' : '') +
        (isCodex ? '<button class="btn btn-icon btn-sm btn-ghost" data-action="changeCodexPassword" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.changeCodexPassword')) + '">' + externalLinkSvg + '</button>' : '') +
        '<button class="btn btn-icon btn-sm btn-ghost" data-action="detail" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.detail')) + '">' + userSvg + '</button>' +
        '<button class="btn btn-icon btn-sm btn-ghost" data-action="copyJSON" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.copyJSON')) + '">' + copySvg + '</button>' +
        // Probing only makes sense for OpenAI-compatible providers: Kiro/Codex
        // speak proprietary protocols, so an /v1/embeddings shaped request would
        // fail for reasons unrelated to capability.
        (isExternal ? '<button class="btn btn-xs btn-outline" data-action="probeCapabilities" data-id="' + idAttr + '" title="' + escapeAttr(t('capability.notProbed')) + '">' + escapeHtml(t('accounts.probeCapabilities')) + '</button>' : '') +
        // The generic "test" button only checks the credential. Media output has
        // to be seen to be verified, so Gommo gets its own run-a-prompt view.
        (isGommo ? '<button class="btn btn-xs btn-outline" data-action="gommoPlayground" data-id="' + idAttr + '">' + escapeHtml(t('gommo.playground') || 'Playground') + '</button>' : '') +
        (reauthRequired ? '<button class="btn btn-sm btn-danger" data-action="loginAgain" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.reauthRequiredHint')) + '">' + escapeHtml(t('accounts.loginAgain')) + '</button>' : '') +
        (banned ? '' :
          '<button class="btn btn-sm ' + (a.enabled ? 'btn-outline' : 'btn-primary') + '" data-action="toggle" data-id="' + idAttr + '" data-enabled="' + (!a.enabled) + '">' +
          escapeHtml(a.enabled ? t('accounts.disable') : t('accounts.enable')) +
          '</button>') +
        '<button class="btn btn-sm ' + (banned ? 'btn-primary' : 'btn-secondary') + '" data-action="test" data-id="' + idAttr + '" id="test-' + idAttr + '" title="' + escapeAttr(banned ? t('accounts.testToClearBan') : t('accounts.test')) + '">' + escapeHtml(banned ? t('accounts.testRecover') : t('accounts.test')) + '</button>' +
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
        '<div class="account-stat"><div class="account-stat-value">' + ((a.requestCount || 0) + (a.serviceRequestCount || 0)) + '</div><div class="account-stat-label">' + escapeHtml(t('accounts.requests')) + '</div></div>' +
        '<div class="account-stat"><div class="account-stat-value">' + formatNum(a.totalTokens || 0) + '</div><div class="account-stat-label">' + escapeHtml(t('accounts.tokens')) + '</div></div>' +
        '<div class="account-stat"><div class="account-stat-value">' + (a.totalCredits || 0).toFixed(1) + '</div><div class="account-stat-label">' + escapeHtml(t('accounts.credits')) + '</div></div>' +
        '<div class="account-stat"><div class="account-stat-value">' + escapeHtml(formatTokenExpiry(a.expiresAt)) + '</div><div class="account-stat-label">' + escapeHtml(t('accounts.expiry')) + '</div></div>' +
        '</div>' +
        '</div>';
  }

  // Account actions
  // refreshAccount refreshes the OAuth token + usage for one account. It does
  // NOT clear a ban — Test does that. Previously a separate "reauth" button hit
  // this same endpoint for banned Codex accounts; that duplicate was removed and
  // its confirmation prompt folded in here, shown only when the account is banned.
  async function refreshAccount(id, card) {
    const acc = accountsData.find(a => a.id === id);
    const isBanned = !!(acc && acc.banStatus && acc.banStatus !== 'ACTIVE');
    if (isBanned) {
      const ok = await confirmAction(t('accounts.confirmReauth'), {
        title: t('accounts.reauth'),
        confirmText: t('accounts.reauth'),
        variant: 'warning'
      });
      if (!ok) return;
    }
    if (card) card.classList.add('loading');
    try {
      const res = await api('/accounts/' + id + '/refresh', { method: 'POST' });
      const d = await res.json();
      if (d.success) {
        loadAccounts();
        if (isBanned) toast(d.message || t('accounts.reauthDone'), 'success');
        else if (d.message) toast(t('accounts.refreshed') + ': ' + d.message, 'success');
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
  async function restoreCodexRefreshToken(id) {
    const input = $('restoreCodexRefreshTokenInput');
    const refreshToken = input ? input.value.trim() : '';
    if (!refreshToken) {
      toastWarning(t('detail.restoreRefreshTokenRequired'));
      return;
    }
    const ok = await confirmAction(t('detail.restoreRefreshTokenConfirm'), {
      title: t('detail.restoreRefreshToken'),
      confirmText: t('detail.restoreRefreshToken'),
      variant: 'warning'
    });
    if (!ok) return;

    const dismiss = toast(t('detail.restoreRefreshToken') + '...', 'info', { duration: 0 });
    try {
      const res = await api('/accounts/' + encodeURIComponent(id) + '/restore-refresh-token', {
        method: 'POST',
        body: JSON.stringify({ refreshToken })
      });
      const d = await res.json();
      dismiss();
      if (d.success) {
        if (input) input.value = '';
        toast(t('detail.restoreRefreshTokenSuccess'), 'success');
        await loadAccounts();
        if (accountsData.some(a => a.id === id)) showDetail(id);
        return;
      }
      toastError(t('detail.restoreRefreshTokenFailed') + ': ' + (d.error || ''));
    } catch (e) {
      dismiss();
      toastError(t('detail.restoreRefreshTokenFailed'));
    }
  }
  // toggleAccount enables/disables an account. Disabling removes the account
  // from the routing pool immediately, so it is confirmed to avoid a stray
  // click silently dropping a provider. Enabling is non-destructive.
  async function toggleAccount(id, enabled) {
    if (!enabled) {
      const ok = await confirmAction(t('accounts.confirmDisable'), {
        title: t('accounts.disable'),
        confirmText: t('accounts.disable'),
        variant: 'danger'
      });
      if (!ok) return;
    }
    await api('/accounts/' + id, { method: 'PUT', body: JSON.stringify({ enabled }) });
    loadAccounts();
  }
  // probeAccountCapabilities calls each advertised capability endpoint to find
  // out whether it actually answers. The catalog cannot tell us this: a model
  // may be listed with no channel behind it (503), or the endpoint path may not
  // exist at all (404). Only the cheap endpoints are probed by default so a
  // diagnostic click never bills meaningful usage.
  async function probeAccountCapabilities(id, btn) {
    const ok = await confirmAction(t('accounts.confirmProbeCapabilities'), {
      title: t('accounts.probeCapabilities'),
      confirmText: t('common.confirm'),
      variant: 'primary'
    });
    if (!ok) return;
    const card = btn ? btn.closest('.account-card') : null;
    if (card) card.classList.add('loading');
    const dismiss = toast(t('accounts.probeCapabilities') + '…', 'info', { duration: 0 });
    try {
      const res = await api('/accounts/' + id + '/probe-capabilities', { method: 'POST' });
      const d = await res.json();
      dismiss();
      if (d.success) {
        const verified = Array.isArray(d.verified) ? d.verified : [];
        const failed = Array.isArray(d.failed) ? d.failed : [];
        const parts = [];
        if (verified.length) parts.push('✓ ' + verified.join(', '));
        if (failed.length) parts.push('✗ ' + failed.join(', '));
        toast(t('accounts.probeDone') + (parts.length ? ': ' + parts.join(' · ') : ''),
          failed.length && !verified.length ? 'warning' : 'success');
        loadAccounts();
      } else {
        toastError(d.error || t('common.failed'));
      }
    } catch (e) {
      dismiss();
      toastError(t('common.failed'));
    }
    if (card) card.classList.remove('loading');
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
  // NOTE: the former reauthAccount() was removed — it hit the same
  // POST /accounts/{id}/refresh endpoint as refreshAccount(), which now carries
  // its confirmation prompt for banned accounts. See refreshAccount() above.
  function loginCodexAgain(id) {
    const account = accountsData.find(a => a.id === id);
    if (!account) return;
    showModal('codexLogin');
    const nicknameInput = $('codexLoginNickname');
    if (nicknameInput) nicknameInput.value = account.nickname || account.email || '';
  }
  async function changeCodexPassword(id) {
    const dismiss = toast(t('accounts.openingCodexSecurity'), 'info', { duration: 0 });
    try {
      const res = await api('/accounts/' + encodeURIComponent(id) + '/codex-security', { method: 'POST' });
      const data = await res.json().catch(() => ({}));
      if (!res.ok || data.success === false) throw new Error(data.error || t('common.failed'));
      if (data.setupRequired) {
        dismiss();
        toast(t('accounts.codexProfileSetup'), 'info', { duration: 5000 });
        pollCodexSecuritySetup(id);
        return;
      }
      dismiss();
      toast(t('accounts.codexSecurityOpened'), 'success');
    } catch (e) {
      dismiss();
      toastError((e && e.message) || t('common.failed'));
    }
  }

  function pollCodexSecuritySetup(id) {
    setTimeout(async () => {
      try {
        const res = await api('/auth/codex/poll', { method: 'POST', body: JSON.stringify({}) });
        const data = await res.json().catch(() => ({}));
        if (data.pending) {
          pollCodexSecuritySetup(id);
          return;
        }
        if (!res.ok || data.success === false) throw new Error(data.error || t('common.failed'));
        await loadAccounts();
        toast(t('accounts.codexProfileLinked'), 'success');
      } catch (e) {
        toastError((e && e.message) || t('common.failed'));
      }
    }, 2000);
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
      const reauthRequired = d.reauthRequired || 0;
      if (reauthRequired > 0) {
        toast(msg + ' (' + reauthRequired + ' ' + t('accounts.reauthRequired') + ')', 'warning');
      } else if (banned > 0) {
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
  function detailItem(label, value, titleValue) {
    // titleValue lets a row show a short, readable value while keeping the full
    // (longer) form reachable on hover — used for account identity so the raw
    // UUID is not rendered inline but is still available for tracing.
    const titleAttr = titleValue ? ' title="' + escapeAttr(titleValue) + '"' : '';
    return '<div class="detail-item"><div class="detail-label">' + escapeHtml(label) + '</div><div class="detail-value"' + titleAttr + '>' + escapeHtml(value) + '</div></div>';
  }
  function showDetail(id) {
    const a = accountsData.find(x => x.id === id);
    if (!a) return;
    const idAttr = escapeAttr(id);
    const isCodex = String(a.authMethod || '').toLowerCase() === 'codex';
    const isExternal = String(a.authMethod || '').toLowerCase() === 'external_openai';
    const isSearch = hasConfiguredCapability(a, 'search');
    const isImage = hasConfiguredCapability(a, 'image');
    const isService = isSearch || isImage;
    // Kiro-native accounts use Machine ID, Weight, Overage, and the Kiro
    // subscription/quota system. Codex and external providers don't.
    const isKiroNative = !isCodex && !isExternal && !isService;
    $('detailBody').innerHTML =
      '<div class="detail-section"><h4>' + escapeHtml(t('detail.basicInfo')) + '</h4><div class="detail-grid">' +
      detailItem(t('detail.email'), getDisplayEmail(a.email, null)) +
      detailItem(t('detail.accountIdentity'), getAccountIdentityLabel(a, a.id, { includeId: false }), getAccountIdentityLabel(a, a.id)) +
      detailItem(t('detail.userId'), a.userId || '-') +
      detailItem(t('detail.authMethod'), formatAuthMethod(a.provider || a.authMethod)) +
      detailItem(t('detail.region'), a.region || 'us-east-1') +
      (isService ? detailItem(t('detail.providerKind'), a.providerKind || '-') : '') +
      (isService ? detailItem(t('detail.capabilities'), accountCapabilities(a).join(', ') || '-') : '') +
      (isService ? detailItem(t('detail.sourceId'), a.sourceId || '-') : '') +
      (a.baseUrl ? detailItem(t('external.baseUrlLabel'), a.baseUrl) : '') +
      (isCodex && a.codexEmail ? detailItem(t('detail.codexEmail'), a.codexEmail) : '') +
      (isCodex && a.codexName ? detailItem(t('detail.codexName'), a.codexName) : '') +
      (isCodex && a.chatgptAccountId ? detailItem(t('detail.codexChatGPTId'), a.chatgptAccountId) : '') +
      '</div></div>' +

      (isService ?
        '<div class="detail-section"><h4>' + escapeHtml(t('detail.service')) + '</h4><div class="detail-grid">' +
        detailItem(t('accounts.serviceProvider'), a.provider || '-') +
        detailItem(t('accounts.serviceCapabilities'), accountCapabilities(a).join(', ') || '-') +
        (a.sourceId ? detailItem(t('detail.sourceId'), a.sourceId) : '') +
        (a.baseUrl ? detailItem(t('external.baseUrlLabel'), a.baseUrl) : '') +
        '</div>' +
        (isSearch ? '<div class="detail-grid" style="margin-top:0.5rem">' +
          detailItem(t('detail.serviceRequests'), a.serviceRequestCount || 0) +
          detailItem(t('detail.serviceErrors'), a.serviceErrorCount || 0) +
          detailItem(t('detail.serviceQuotaErrors'), a.serviceQuotaErrorCount || 0) +
          detailItem(t('detail.serviceLastStatus'), a.serviceLastStatus || '-') +
          detailItem(t('detail.serviceRateLimit'), a.serviceRateLimit || '-') +
          detailItem(t('detail.serviceRateRemaining'), a.serviceRateLimitRemaining || '-') +
          detailItem(t('detail.serviceRateReset'), a.serviceRateLimitReset || '-') +
          detailItem(t('detail.serviceRetryAfter'), a.serviceRetryAfter || '-') +
          (a.serviceUsageCheckedAt ? detailItem(t('detail.serviceLastChecked'), new Date(a.serviceUsageCheckedAt * 1000).toLocaleString()) : detailItem(t('detail.serviceLastChecked'), '-')) +
          '</div>' : '') +
        '<p class="help-block">' + escapeHtml(t('detail.serviceNoModels')) + '</p></div>' : '') +

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

      // Weight — every chat-capable account. The pool selector applies
      // effectiveWeight(a.Weight) to the whole chat pool (Kiro, Codex and
      // external providers alike), so hiding this from non-Kiro accounts made
      // a working backend feature unreachable. Service accounts (search/image)
      // are appended directly by the selector and ignore weight.
      (!isService ?
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
        // Bank-reset credits: always shown for Codex accounts, including 0,
        // so the operator can see the real count instead of a missing row.
        detailItem(t('detail.codexResetCredits'), String(a.codexResetCreditsAvailable || 0)) +
        (a.codexUsageCheckedAt ? detailItem(t('detail.codexLastChecked'), new Date(a.codexUsageCheckedAt * 1000).toLocaleString()) : '') +
        '</div>' +
        '<p class="help-block">' + escapeHtml(t('detail.codexUsageHint')) + '</p>' +
        '</div>' : '') +

      '<div class="detail-section"><h4>' + escapeHtml(t('accounts.imageModel')) + '</h4>' +
      '<div class="machine-id-row">' +
      '<input type="text" id="imageModelInput" value="' + escapeAttr(a.imageModel || a.codexImageModel || '') + '" placeholder="gpt-image-2" />' +
      '<button class="btn btn-sm btn-primary" data-detail-action="saveImageModel" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.save')) + '</button>' +
      '</div>' +
      '<p class="help-block">' + escapeHtml(t('accounts.imageModelHint')) + '</p></div>' +

      // Token section — shown for all account types. Includes a manual
      // "Refresh token" button that forces the OAuth refresh-token flow
      // regardless of expiry, plus the last-refreshed timestamp. External
      // OpenAI-compatible providers use a static API key (no refresh token),
      // so the button is hidden for them.
      '<div class="detail-section"><h4>' + escapeHtml(t('detail.tokenSection')) +
      (!isExternal && !isService && a.refreshToken ? ' <button class="btn btn-sm btn-outline" data-detail-action="refreshToken" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.refreshToken')) + '</button>' : '') +
      '</h4><div class="detail-grid">' +
      detailItem(t('detail.tokenExpiry'), formatTokenExpiry(a.expiresAt)) +
      (a.expiresAt ? detailItem(t('detail.tokenExpiryAbs'), new Date(a.expiresAt * 1000).toLocaleString()) : '') +
      (a.tokenRefreshedAt ? detailItem(t('detail.tokenRefreshedAt'), new Date(a.tokenRefreshedAt * 1000).toLocaleString()) : '') +
      '</div></div>' +

      (isCodex ? '<div class="detail-section"><h4>' + escapeHtml(t('detail.restoreRefreshToken')) + '</h4>' +
      '<div class="form-group"><label for="restoreCodexRefreshTokenInput">' + escapeHtml(t('codex.refreshTokenLabel')) + '</label>' +
      '<textarea id="restoreCodexRefreshTokenInput" class="font-mono" rows="2" autocomplete="off" spellcheck="false" placeholder="' + escapeAttr(t('detail.restoreRefreshTokenPlaceholder')) + '"></textarea></div>' +
      '<button class="btn btn-sm btn-warning" data-detail-action="restoreCodexRefreshToken" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.restoreRefreshToken')) + '</button>' +
      '<p class="help-block">' + escapeHtml(t('detail.restoreRefreshTokenHint')) + '</p></div>' : '') +

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

      (isService ? '' : '<div class="detail-section">' +
      '<h4>' + escapeHtml(t('detail.models')) +
      ' <button class="btn btn-sm btn-outline" data-detail-action="loadModels" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.loadModels')) + '</button>' +
      ' <button class="btn btn-sm btn-outline" data-detail-action="refreshModels" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.refreshModelCache')) + '</button>' +
      '</h4>' +
      '<div id="modelsList" class="model-list"></div>' +
      '</div>');

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
  async function saveCodexImageModel(id) {
    const model = $('codexImageModelInput').value.trim();
    await putAccount(id, { codexImageModel: model }, t('detail.saved'));
  }
  async function saveImageModel(id) {
    const input = $('imageModelInput');
    await putAccount(id, { imageModel: input ? input.value.trim() : '' }, t('detail.saved'));
  }
  function closeDetailModal() { closeDialog('detailModal'); }

  // Test flow
  function getTestAccount(id) {
    return accountsData.find(a => a.id === id) || null;
  }
  function getTestRequest() {
    const acc = getTestAccount(testModalAccountId);
    if (testModalMode === 'image') {
      const customModel = $('testImageModelCustom') && $('testImageModelCustom').value.trim();
      const selectedModel = $('testImageModelChoice') && $('testImageModelChoice').value;
      return {
        capability: 'image',
        prompt: (($('testImagePrompt') && $('testImagePrompt').value.trim()) || t('accounts.imagePromptDefault')),
        model: customModel || selectedModel || ''
      };
    }
    if (testModalMode === 'search' && acc && hasConfiguredCapability(acc, 'search')) {
      return {
        capability: 'search',
        query: (($('testSearchQuery') && $('testSearchQuery').value.trim()) || 'OmniProxy health check'),
        url: (($('testSearchURL') && $('testSearchURL').value.trim()) || '')
      };
    }
    // Chat test must use the model the operator picked in the modal. The
    // first cached model is only a fallback for when the select is absent
    // (models still loading, or the account exposes no cached model list).
    // Do not infer a model from provider/account type.
    const picked = $('testModelChoice') ? String($('testModelChoice').value || '').trim() : '';
    const model = picked || String(testModalModels[0] || '').trim();
    return { model: model };
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
    // Visible label stays name/email only; the raw UUID is noise in the modal
    // header. The full identity (including ID) remains in the title attribute.
    const accountIdentity = getAccountIdentityLabel(acc, testModalAccountId);
    const accountIdentityShort = getAccountIdentityLabel(acc, testModalAccountId, { includeId: false });
    const proxy = acc ? (acc.proxyURL || t('accounts.testLog.globalProxy')) : '?';
    const isSearch = acc && hasConfiguredCapability(acc, 'search');
    const isImage = acc && hasConfiguredCapability(acc, 'image');
    const isCodex = isCodexAccountUI(acc);
    const isImageTest = testModalMode === 'image';
    const isService = isSearch || isImage;
    const statusText = isImageTest
        ? (testModalLoadingImageModels ? t('accounts.imageModelsLoading') : testModalImageModelError ? t('accounts.imageModelsFallback') : testModalImageSupported ? t('accounts.imageTestReady') : t('accounts.imageGenerationUnsupported'))
      : testModalMode === 'search'
        ? t('accounts.searchTestReady')
      : testModalLoadingModels
        ? t('accounts.testModelsLoading')
      : testModalModelError
        ? t('accounts.testModelsFallback')
        : t('accounts.testModelsReady', testModalModels.length);
    const modeField = '<div class="segmented-control test-mode-control" role="tablist">' +
        '<button type="button" class="btn btn-sm ' + (testModalMode === 'chat' ? 'btn-primary' : 'btn-outline') + '" data-test-mode="chat">' + escapeHtml(t('accounts.testChatMode')) + '</button>' +
        '<button type="button" class="btn btn-sm ' + (testModalMode === 'image' ? 'btn-primary' : 'btn-outline') + '" data-test-mode="image">' + escapeHtml(t('accounts.testImageMode')) + '</button>' +
        (isSearch ? '<button type="button" class="btn btn-sm ' + (testModalMode === 'search' ? 'btn-primary' : 'btn-outline') + '" data-test-mode="search">' + escapeHtml(t('accounts.testSearchMode')) + '</button>' : '') +
        '</div>';
    const imageModel = acc && (acc.imageModel || acc.codexImageModel) || '';
    const imageModelOptions = ['<option value="">' + escapeHtml(t('accounts.selectTestModel')) + '</option>'];
    const imageModelIds = new Set();
    testModalImageModels.forEach(m => {
      const modelId = String((m && m.id) || m || '').trim();
      if (!modelId || imageModelIds.has(modelId)) return;
      imageModelIds.add(modelId);
      imageModelOptions.push('<option value="' + escapeAttr(modelId) + '">' + escapeHtml((m && m.name) || modelId) + '</option>');
    });
    if (imageModel && !imageModelIds.has(imageModel)) {
      imageModelOptions.push('<option value="' + escapeAttr(imageModel) + '">' + escapeHtml(imageModel) + '</option>');
    }
    const modelField = testModalMode === 'search' && isSearch
      ? '<div class="form-group"><label for="testSearchQuery">' + escapeHtml(t('accounts.searchQuery')) + '</label><input type="text" id="testSearchQuery" value="OmniProxy health check" /></div>' +
        '<div class="form-group"><label for="testSearchURL">' + escapeHtml(t('accounts.searchURL')) + '</label><input type="url" id="testSearchURL" placeholder="https://example.com/docs" /></div>'
      : isImageTest
        ? '<div class="form-group"><label for="testImagePrompt">' + escapeHtml(t('accounts.imagePrompt')) + '</label><textarea id="testImagePrompt" rows="3">' + escapeHtml(t('accounts.imagePromptDefault')) + '</textarea></div>' +
          '<div class="form-group"><label for="testImageModelChoice">' + escapeHtml(t('accounts.selectTestModel')) + '</label>' +
          (testModalLoadingImageModels ? '<div class="test-model-loading">' + escapeHtml(t('accounts.imageModelsLoading')) + '</div>' : '') +
          '<select id="testImageModelChoice">' + imageModelOptions.join('') + '</select>' +
          '<label for="testImageModelCustom">' + escapeHtml(t('accounts.imageModelCustom')) + '</label>' +
          '<input type="text" id="testImageModelCustom" value="" placeholder="' + escapeAttr(imageModel || t('accounts.imageModelCustom')) + '" />' +
          (!testModalImageSupported && testModalImageReason ? '<p class="help-block">' + escapeHtml(testModalImageReason) + '</p>' : '') +
          '<button type="button" class="btn btn-sm btn-outline" data-test-action="refresh-image-models">' + escapeHtml(t('accounts.refreshImageModels')) + '</button></div>'
      : testModalLoadingModels
      ? '<div class="test-model-loading">' + escapeHtml(t('accounts.testModelsLoading')) + '</div>'
      : testModalModels.length
        ? '<select id="testModelChoice">' +
        testModalModels.map(m => '<option value="' + escapeAttr(m) + '">' + escapeHtml(m) + '</option>').join('') +
        '</select>'
        : '<div class="test-model-empty">' + escapeHtml(t('accounts.testModelsFallback')) + '</div>';

    body.innerHTML =
      '<div class="test-modal-account">' +
      '<div class="test-modal-account-main">' +
      '<div class="test-modal-email" title="' + escapeAttr(accountIdentity) + '">' + escapeHtml(accountIdentityShort) + '</div>' +
      '<div class="test-modal-meta">' +
      '<span>' + escapeHtml(formatAuthMethod(acc && (acc.provider || acc.authMethod))) + '</span>' +
      '<span>' + escapeHtml(proxy) + '</span>' +
      '</div>' +
      '</div>' +
      '<span class="test-modal-status">' + escapeHtml(statusText) + '</span>' +
      '</div>' +
      '<div class="test-modal-grid">' +
      '<div class="form-group test-model-field">' +
      modeField +
      (testModalMode === 'chat' ? '<label for="testModelChoice">' + escapeHtml(t('accounts.selectModel')) + '</label>' : '') +
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
      '<button class="btn btn-primary" id="testRunBtn" data-id="' + idAttr + '" type="button" ' + ((testModalMode === 'chat' && (testModalLoadingModels || !testModalModels[0])) || (testModalMode === 'image' && testModalLoadingImageModels) ? 'disabled' : '') + '>' + escapeHtml(t('accounts.test')) + '</button>' +
      '</div>';

    if (!testModalLoadingModels && !testModalLoadingImageModels) enhanceCustomSelects(body);
    renderTestLog();
  }
  async function testAccount(id) {
    testModalAccountId = id;
    const acc = getTestAccount(id);
    testModalModels = [];
    testModalImageModels = [];
    testModalMode = 'chat';
    testModalLoadingModels = true;
    testModalModelError = false;
    testModalLoadingImageModels = true;
    testModalImageModelError = false;
    testModalImageSupported = true;
    testModalImageReason = '';
    testModalRunning = false;
    testLogs = [];
    renderTestModal();
    openDialog('testModal');
    const encodedId = encodeURIComponent(id);
    await Promise.all([
      (async () => {
        try {
          const res = await api('/accounts/' + encodedId + '/models/cached');
          const d = await res.json();
          testModalModels = Array.isArray(d.models) ? d.models.slice().sort() : [];
        } catch (e) { testModalModelError = true; }
        testModalLoadingModels = false;
        renderTestModal();
      })(),
      (async () => {
        try {
          const res = await api('/accounts/' + encodedId + '/image-models');
          const d = await res.json();
          testModalImageModels = Array.isArray(d.models) ? d.models : [];
          testModalImageSupported = d.supported !== false;
          testModalImageReason = d.reason || '';
        } catch (e) {
          testModalImageModelError = true;
          testModalImageSupported = false;
          testModalImageReason = t('accounts.imageModelsFallback');
        }
        testModalLoadingImageModels = false;
        renderTestModal();
      })()
    ]);
  }
  async function refreshTestImageModels(id) {
    const encodedId = encodeURIComponent(id);
    const prompt = $('testImagePrompt') ? $('testImagePrompt').value : '';
    const customModel = $('testImageModelCustom') ? $('testImageModelCustom').value : '';
    const selectedModel = $('testImageModelChoice') ? $('testImageModelChoice').value : '';
    testModalLoadingImageModels = true;
    testModalImageModelError = false;
    renderTestModal();
    try {
      const res = await api('/accounts/' + encodedId + '/image-models');
      const d = await res.json();
      testModalImageModels = Array.isArray(d.models) ? d.models : [];
      testModalImageSupported = d.supported !== false;
      testModalImageReason = d.reason || '';
    } catch (e) {
      testModalImageModelError = true;
      testModalImageSupported = false;
      testModalImageReason = t('accounts.imageModelsFallback');
    } finally {
      testModalLoadingImageModels = false;
      renderTestModal();
      if ($('testImagePrompt')) $('testImagePrompt').value = prompt;
      if ($('testImageModelCustom')) $('testImageModelCustom').value = customModel;
      if ($('testImageModelChoice')) $('testImageModelChoice').value = selectedModel;
    }
  }
  function closeTestModal() {
    closeAllCustomSelects();
    closeDialog('testModal');
  }

  // ==================== Gommo playground ====================

  // The playground targets one named account rather than the pool: an operator
  // verifying the credential they just added needs that account exercised, not
  // whichever one rotation happens to pick.
  async function gommoPlayground(id) {
    gommoPlaygroundId = id;
    gommoPlaygroundKind = 'image';
    gommoPlaygroundModels = { image: [], video: [], audio: [] };
    gommoPlaygroundResult = null;
    gommoPlaygroundRunning = false;
    gommoPlaygroundVoice = '';
    renderGommoModal();
    openDialog('gommoModal');
    try {
      const res = await api('/accounts/' + encodeURIComponent(id) + '/gommo-models');
      const d = await res.json();
      if (d.success) {
        gommoPlaygroundModels = {
          image: d.models.image || [],
          video: d.models.video || [],
          audio: d.models.audio || [],
        };
        gommoPlaygroundVoice = d.voiceId || '';
      } else {
        gommoPlaygroundResult = { error: d.error || t('common.failed') };
      }
    } catch (e) {
      gommoPlaygroundResult = { error: e.message || String(e) };
    }
    renderGommoModal();
  }

  function closeGommoModal() { closeDialog('gommoModal'); }

  function gommoModelOptions(kind) {
    const list = gommoPlaygroundModels[kind === 'tts' ? 'audio' : kind] || [];
    return list.map(m =>
      '<option value="' + escapeAttr(m.id) + '">' + escapeHtml(m.name || m.id) + '</option>'
    ).join('');
  }

  function renderGommoModal() {
    const body = $('gommoBody');
    if (!body) return;
    const kind = gommoPlaygroundKind;
    const acc = accountsData.find(a => a.id === gommoPlaygroundId);
    const label = acc ? (acc.nickname || acc.email || acc.id) : gommoPlaygroundId;
    const tab = (value, text) =>
      '<button type="button" class="btn btn-sm ' + (kind === value ? 'btn-primary' : 'btn-outline') +
      '" data-gommo-kind="' + value + '">' + escapeHtml(text) + '</button>';

    body.innerHTML =
      '<p class="help-block">' + escapeHtml(label) + '</p>' +
      '<div class="flex gap-2 flex-wrap mb-3">' +
      tab('image', t('category.image')) +
      tab('video', t('category.video')) +
      tab('tts', t('category.audioTts')) +
      tab('video-status', t('gommo.jobLookup') || 'Job lookup') +
      '</div>' +
      (kind === 'video-status'
        ? '<div class="form-group"><label>' + escapeHtml(t('gommo.jobIdLabel') || 'Job ID') + '</label>' +
          '<input type="text" id="gommoJobId" class="font-mono" /></div>'
        : '<div class="form-group"><label>' + escapeHtml(t('gommo.promptLabel') || 'Prompt') + '</label>' +
          '<textarea id="gommoPrompt" rows="3">' + escapeHtml(gommoDefaultPrompt(kind)) + '</textarea></div>' +
          '<div class="form-group"><label>' + escapeHtml(t('gommo.modelLabel') || 'Model') + '</label>' +
          '<select id="gommoModel">' + gommoModelOptions(kind) + '</select></div>' +
          (kind === 'image'
            ? '<div class="form-group"><label>' + escapeHtml(t('gommo.sizeLabel') || 'Ratio') + '</label>' +
              '<select id="gommoSize"><option value="1:1">1:1</option><option value="16:9">16:9</option><option value="9:16">9:16</option></select></div>'
            : '') +
          (kind === 'video'
            ? '<div class="form-group"><label>' + escapeHtml(t('gommo.sizeLabel') || 'Ratio') + '</label>' +
              '<select id="gommoRatio"><option value="16_9">16:9</option><option value="9_16">9:16</option><option value="1_1">1:1</option></select></div>'
            : '') +
          // Speech needs a voice id: the upstream rejects a synthesis request
          // without one, so it is asked for here rather than failing later.
          (kind === 'tts'
            ? '<div class="form-group"><label>' + escapeHtml(t('gommo.voiceLabel') || 'Voice ID') + '</label>' +
              '<input type="text" id="gommoVoice" class="font-mono" value="' + escapeAttr(gommoPlaygroundVoice) + '" /></div>'
            : '')) +
      '<div id="gommoResult" class="mt-2">' + renderGommoResult() + '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" id="gommoCancelBtn" type="button">' + escapeHtml(t('common.close')) + '</button>' +
      '<button class="btn btn-primary" id="gommoRunBtn" type="button" ' + (gommoPlaygroundRunning ? 'disabled' : '') + '>' +
      escapeHtml(gommoPlaygroundRunning ? (t('common.loading') || '...') : (t('gommo.run') || 'Generate')) + '</button>' +
      '</div>';

    $('gommoCancelBtn').addEventListener('click', closeGommoModal);
    $('gommoRunBtn').addEventListener('click', runGommoPlayground);
    qsa('[data-gommo-kind]', body).forEach(btn => btn.addEventListener('click', () => {
      gommoPlaygroundKind = btn.dataset.gommoKind;
      gommoPlaygroundResult = null;
      renderGommoModal();
    }));
  }

  function gommoDefaultPrompt(kind) {
    if (kind === 'tts') return t('gommo.samplePromptTts') || 'Xin chào, đây là bản thử giọng đọc.';
    if (kind === 'video') return t('gommo.samplePromptVideo') || 'A red fox running through falling snow, cinematic';
    return t('gommo.samplePromptImage') || 'A red fox in deep snow at golden hour, cinematic, sharp detail';
  }

  // Video renders can outlive the poll ceiling, in which case the job id is all
  // that comes back — it is shown so the operator can look the render up later
  // rather than paying for it again.
  function renderGommoResult() {
    const r = gommoPlaygroundResult;
    if (!r) return '';
    if (r.error) return '<p class="text-sm" style="color:var(--danger)">' + escapeHtml(r.error) + '</p>';
    let html = '';
    if (r.elapsedMs) html += '<p class="text-sm muted-text">' + Math.round(r.elapsedMs / 1000) + 's</p>';
    if (r.id) html += '<p class="text-sm font-mono">' + escapeHtml(r.id) + ' · ' + escapeHtml(r.status || '') + '</p>';
    for (const url of (r.urls || [])) {
      if (!url) continue;
      html += r.kind === 'video'
        ? '<video src="' + escapeAttr(url) + '" controls class="w-full mt-2"></video>'
        : '<img src="' + escapeAttr(url) + '" class="w-full mt-2" alt="" />';
      html += '<p class="text-sm"><a href="' + escapeAttr(url) + '" target="_blank" rel="noopener">' + escapeHtml(url) + '</a></p>';
    }
    if (r.dataUrl) html += '<audio src="' + escapeAttr(r.dataUrl) + '" controls class="w-full mt-2"></audio>';
    return html || '<p class="text-sm muted-text">' + escapeHtml(t('gommo.noArtifact') || 'No artifact returned') + '</p>';
  }

  async function runGommoPlayground() {
    if (gommoPlaygroundRunning) return;
    gommoPlaygroundRunning = true;
    gommoPlaygroundResult = null;
    const kind = gommoPlaygroundKind;
    const payload = {
      accountId: gommoPlaygroundId,
      kind,
      prompt: $('gommoPrompt') ? $('gommoPrompt').value.trim() : '',
      model: $('gommoModel') ? $('gommoModel').value : '',
      size: $('gommoSize') ? $('gommoSize').value : '',
      ratio: $('gommoRatio') ? $('gommoRatio').value : '',
      voice: $('gommoVoice') ? $('gommoVoice').value.trim() : '',
      jobId: $('gommoJobId') ? $('gommoJobId').value.trim() : '',
      n: 1,
    };
    renderGommoModal();
    try {
      const res = await api('/gommo/playground', { method: 'POST', body: JSON.stringify(payload) });
      const d = await res.json();
      gommoPlaygroundResult = d.success ? d : { error: d.error || t('common.failed') };
    } catch (e) {
      gommoPlaygroundResult = { error: e.message || String(e) };
    } finally {
      gommoPlaygroundRunning = false;
      renderGommoModal();
    }
  }

  async function runTestAccount(id, request) {
    if (testModalRunning) return;
    testModalRunning = true;
    const modalBtn = $('testRunBtn');
    if (modalBtn) modalBtn.setAttribute('aria-busy', 'true');
    const acc = accountsData.find(a => a.id === id);
    const email = acc ? getDisplayEmail(acc.email, acc.id) : id;
    // Test-log lines identify the account by name/email. The full ID is
    // available in the account detail view; repeating a UUID on every log line
    // makes the log unreadable.
    const accountIdentity = getAccountIdentityLabel(acc, id, { includeId: false });
    const proxy = acc ? (acc.proxyURL || t('accounts.testLog.globalProxy')) : '?';
    let startMessage;
    if (request.capability === 'image') {
      startMessage = t('accounts.testLog.startImage', accountIdentity, request.prompt || '-', request.model || t('accounts.testLog.defaultModel'), proxy);
    } else if (request.capability === 'search') {
      startMessage = t('accounts.testLog.startSearch', accountIdentity, request.query || request.url || '-', proxy);
    } else {
      startMessage = t('accounts.testLog.start', accountIdentity, request.model || '-', proxy);
    }
    addTestLog(startMessage, 'info');
    try {
      const startTime = Date.now();
      const res = await api('/accounts/' + encodeURIComponent(id) + '/test', { method: 'POST', body: JSON.stringify(request) });
      const elapsed = ((Date.now() - startTime) / 1000).toFixed(1);
      const d = await res.json();
      if (d.success) {
        addTestLog(t('accounts.testLog.success', accountIdentity, elapsed, d.reply), 'ok');
        // If the test cleared a ban, log it and reload to reflect the
        // new ACTIVE badge + enabled state immediately.
        if (d.banCleared) {
          addTestLog(t('accounts.testLog.banCleared', accountIdentity), 'ok');
          loadAccounts();
        }
      } else {
        addTestLog(t('accounts.testLog.failed', accountIdentity, elapsed, d.error || t('common.unknownError')), 'err');
        // If the account was banned during the test, reload accounts to reflect
        // the new BANNED badge and disable state immediately.
        if (d.banned || d.banStatus === 'BANNED') {
          loadAccounts();
        }
      }
    } catch (e) {
      addTestLog(t('accounts.testLog.error', accountIdentity, e.message), 'err');
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
    ninerouter: 'fa-solid fa-arrow-right-to-bracket',
    antigravity: 'fa-solid fa-rocket',
    antigravityLogin: 'fa-brands fa-google',
    antigravityImport: 'fa-solid fa-file-import',
    gommo: 'fa-solid fa-photo-film'
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
    if (type !== 'codexLogin') stopCodexLoginPolling();
    if (type !== 'antigravityLogin') stopAntigravityLoginPolling();
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
    else if (type === 'agentrouter') modalAgentRouter(title, body);
    else if (type === 'cookie') modalCookie(title, body);
    else if (type === 'codex') modalCodex(title, body);
    else if (type === 'codexLogin') modalCodexLogin(title, body);
    else if (type === 'ninerouter') modalNineRouter(title, body);
    else if (type === 'antigravity') modalAntigravity(title, body);
    else if (type === 'antigravityLogin') modalAntigravityLogin(title, body);
    else if (type === 'antigravityImport') modalAntigravityImport(title, body);
    else if (type === 'gommo') modalGommo(title, body);
    if (!modal.classList.contains('active')) openDialog('addModal');
    enhanceCustomSelects(body);
  }
  function closeModal() {
    stopCodexLoginPolling();
    stopAntigravityLoginPolling();
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
      // ── Codex category ──
      '<div class="add-category-section" data-cat="codex">' +
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
      // ── External & Gateway category (OpenAI-compatible, AgentRouter) ──
      '<div class="add-category-section" data-cat="external">' +
      '<div class="add-category-header">' +
      '<span class="add-category-icon"><i class="fa-solid fa-network-wired" aria-hidden="true"></i></span>' +
      '<span class="add-category-title">' + escapeHtml(t('category.external') || 'External & Gateways') + '</span>' +
      '<span class="add-category-desc">' + escapeHtml(t('modal.externalCategoryDesc') || 'OpenAI-compatible APIs & AgentRouter Gateway') + '</span>' +
      '</div>' +
      '<div class="method-list">' +
      methodCard('external', t('modal.externalTitle'), t('modal.externalDesc')) +
      methodCard('agentrouter', t('modal.agentrouterTitle'), t('modal.agentrouterDesc')) +
      methodCard('antigravity', t('modal.antigravityTitle'), t('modal.antigravityDesc')) +
      '</div>' +
      '</div>' +
      // ── Media category (image / video / speech generation) ──
      '<div class="add-category-section" data-cat="media">' +
      '<div class="add-category-header">' +
      '<span class="add-category-icon"><i class="fa-solid fa-photo-film" aria-hidden="true"></i></span>' +
      '<span class="add-category-title">' + escapeHtml(t('category.media')) + '</span>' +
      '<span class="add-category-desc">' + escapeHtml(t('modal.mediaCategoryDesc')) + '</span>' +
      '</div>' +
      '<div class="method-list">' +
      methodCard('gommo', t('modal.gommoTitle'), t('modal.gommoDesc')) +
      '</div>' +
      '</div>' +
      // ── Kiro category (Collapsible with primary methods + show more) ──
      '<div class="add-category-section add-category-collapsible" data-cat="kiro">' +
      '<div class="add-category-header clickable" id="kiroSectionToggle">' +
      '<span class="add-category-icon"><i class="fa-solid fa-cloud" aria-hidden="true"></i></span>' +
      '<span class="add-category-title">' + escapeHtml(t('category.kiro')) + '</span>' +
      '<span class="add-category-desc">' + escapeHtml(t('modal.kiroCategoryDesc')) + '</span>' +
      '<span class="add-category-toggle-icon" id="kiroToggleIcon"><i class="fa-solid fa-chevron-down" aria-hidden="true"></i></span>' +
      '</div>' +
      '<div class="method-list" id="kiroPrimaryMethods">' +
      methodCard('builderid', t('modal.builderIdTitle'), t('modal.builderIdDesc')) +
      methodCard('iam', t('modal.iamTitle'), t('modal.iamDesc')) +
      methodCard('social', t('modal.socialTitle'), t('modal.socialDesc')) +
      methodCard('kirossi', t('modal.kirossiTitle'), t('modal.kirossiDesc')) +
      '</div>' +
      '<div class="method-list hidden" id="kiroSecondaryMethods">' +
      methodCard('sso', t('modal.ssoTitle'), t('modal.ssoDesc')) +
      methodCard('kirocli', t('modal.kirocliTitle'), t('modal.kirocliDesc')) +
      methodCard('kiroAuto', t('kiroauto.title'), t('kiroauto.desc')) +
      methodCard('kiroToken', t('kirotoken.title'), t('kirotoken.desc')) +
      methodCard('ssocache', t('modal.ssocacheTitle'), t('modal.ssocacheDesc')) +
      methodCard('local', t('modal.localTitle'), t('modal.localDesc')) +
      methodCard('credentials', t('modal.credentialsTitle'), t('modal.credentialsDesc')) +
      methodCard('apikey', t('modal.apikeyTitle'), t('modal.apikeyDesc')) +
      methodCard('cookie', t('modal.cookieTitle'), t('modal.cookieDesc')) +
      '</div>' +
      '<div class="add-category-more-wrap">' +
      '<button type="button" class="btn btn-sm btn-ghost add-category-more-btn" id="kiroShowMoreBtn">' +
      '<i class="fa-solid fa-ellipsis" aria-hidden="true"></i> ' +
      '<span id="kiroShowMoreLabel">' + escapeHtml(t('modal.showMoreMethods') || 'Show 9 more Kiro methods...') + '</span>' +
      '</button>' +
      '</div>' +
      '</div>' +
      '<div class="modal-footer"><button class="btn btn-secondary" data-close-add="1" type="button">' + escapeHtml(t('common.cancel')) + '</button></div>';

    // Event listener for Kiro show more toggle
    var moreBtn = $('kiroShowMoreBtn');
    if (moreBtn) {
      moreBtn.addEventListener('click', function(e) {
        e.preventDefault();
        var sec = $('kiroSecondaryMethods');
        var icon = $('kiroToggleIcon');
        var label = $('kiroShowMoreLabel');
        if (!sec) return;
        var isHidden = sec.classList.contains('hidden');
        if (isHidden) {
          sec.classList.remove('hidden');
          if (icon) icon.innerHTML = '<i class="fa-solid fa-chevron-up" aria-hidden="true"></i>';
          if (label) label.textContent = t('modal.showLessMethods') || 'Show fewer Kiro methods';
        } else {
          sec.classList.add('hidden');
          if (icon) icon.innerHTML = '<i class="fa-solid fa-chevron-down" aria-hidden="true"></i>';
          if (label) label.textContent = t('modal.showMoreMethods') || 'Show 9 more Kiro methods...';
        }
      });
    }
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

  function modalAgentRouter(title, body) {
    title.textContent = t('modal.agentrouterTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.agentrouterDesc')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('external.baseUrlLabel')) + '</label>' +
      '<input type="text" id="agentrouterBaseUrl" class="font-mono" value="https://ps.air-outer.com" placeholder="https://ps.air-outer.com" /></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('external.apiKeyLabel')) + '</label>' +
      '<input type="password" id="agentrouterApiKey" class="font-mono" placeholder="sk-..." autocomplete="off" /></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('external.nameLabel')) + '</label>' +
      '<input type="text" id="agentrouterName" placeholder="AgentRouter Account" /></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importAgentRouterBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('importAgentRouterBtn').addEventListener('click', importAgentRouter);
  }

  async function importAgentRouter() {
    const baseUrl = $('agentrouterBaseUrl').value.trim() || 'https://ps.air-outer.com';
    const apiKey = $('agentrouterApiKey').value.trim();
    if (!apiKey) return toastWarning(t('external.apiKeyLabel') + ' is required');
    const name = $('agentrouterName').value.trim() || 'AgentRouter Account';
    const btn = $('importAgentRouterBtn');
    btn.disabled = true; btn.textContent = t('common.loading') || '...';
    try {
      const res = await api('/auth/external-provider', { method: 'POST', body: JSON.stringify({ baseUrl, apiKey, name, authMethod: 'agentrouter' }) });
      const d = await res.json();
      if (d.success) {
        closeModal(); loadAccounts(); loadStats();
        toastPrimary(t('external.importSuccess') + ': ' + (d.account?.email || d.account?.id), { duration: 5000 });
        autoRefreshNewAccount(d.account?.id);
      } else {
        toastWarning(d.error || 'Import failed');
      }
    } catch (e) {
      toastWarning(e.message);
    } finally {
      btn.disabled = false; btn.textContent = t('common.add');
    }
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
  var codexLoginGeneration = 0;
  function stopCodexLoginPolling() {
    codexLoginGeneration++;
    if (codexPollTimer) {
      clearTimeout(codexPollTimer);
      codexPollTimer = null;
    }
  }
  function modalCodexLogin(title, body) {
    title.textContent = t('codex.loginTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('codex.loginDesc')) + '</p>' +
      '<div id="codexStep1">' +
      '<div class="form-group"><label>' + escapeHtml(t('detail.nickname')) + '</label>' +
      '<input type="text" id="codexLoginNickname" placeholder="' + escapeAttr(t('codex.nicknamePlaceholder')) + '" /></div>' +
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
    const nicknameInput = $('codexLoginNickname');
    if (!btn || !nicknameInput) return;
    const nickname = nicknameInput.value.trim();
    stopCodexLoginPolling();
    const generation = codexLoginGeneration;
    btn.disabled = true; btn.textContent = t('common.loading') || '...';
    try {
      const res = await api('/auth/codex/login', { method: 'POST' });
      const d = await res.json();
      if (generation !== codexLoginGeneration) return;
      if (d.error) {
        toastError(d.error);
        return;
      }
      const step1 = $('codexStep1');
      const step2 = $('codexStep2');
      const authURL = $('codexAuthUrl');
      const openBtn = $('codexOpenBtn');
      const copyBtn = $('codexCopyBtn');
      const cancelBtn = $('codexCancelBtn');
      if (!step1 || !step2 || !authURL || !openBtn || !copyBtn || !cancelBtn) return;
      step1.classList.add('hidden');
      step2.classList.remove('hidden');
      authURL.textContent = d.authUrl;
      if (d.browserError) toastWarning(d.browserError);
      openBtn.addEventListener('click', openCodexLoginBrowser);
      copyBtn.addEventListener('click', async () => {
        await copyText(d.authUrl);
        toastPrimary(t('common.copied'));
      });
      cancelBtn.addEventListener('click', cancelCodexLogin);
      pollCodexLogin(nickname, generation);
    } catch (e) {
      toastError(t('common.failed') + ': ' + (e.message || e));
    } finally {
      btn.disabled = false; btn.textContent = t('codex.startLogin');
    }
  }
  async function openCodexLoginBrowser() {
    const btn = $('codexOpenBtn');
    btn.disabled = true;
    try {
      const res = await api('/auth/codex/open-browser', { method: 'POST' });
      const d = await res.json();
      if (d.success) return;
      toastError(d.error || t('common.failed'));
    } catch (e) {
      toastError(t('common.failed') + ': ' + (e.message || e));
    } finally {
      btn.disabled = false;
    }
  }
  function pollCodexLogin(nickname, generation) {
    if (codexPollTimer) clearTimeout(codexPollTimer);
    codexPollTimer = setTimeout(async () => {
      if (generation !== codexLoginGeneration) return;
      try {
        const res = await api('/auth/codex/poll', { method: 'POST', body: JSON.stringify({ nickname }) });
        const d = await res.json();
        if (generation !== codexLoginGeneration) return;
        if (d.pending) {
          const status = $('codexStatus');
          if (!status) return;
          status.textContent = t('builderid.waiting');
          pollCodexLogin(nickname, generation);
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
    stopCodexLoginPolling();
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

  // ==================== Antigravity (Google Cloud Code Assist) ====================

  // modalAntigravity — hub. Offers the OAuth login and, when this machine
  // already has credentials from an installed Antigravity / Gemini CLI, the
  // import path. The import is probed first because re-granting OAuth for an
  // account that is already authorised locally invalidates the IDE's session.
  function modalAntigravity(title, body) {
    title.textContent = t('antigravity.title') || 'Google Antigravity';
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('antigravity.desc') || '') + '</p>' +
      '<div class="message message-warning"><p>' + escapeHtml(t('antigravity.tosWarning') || '') + '</p></div>' +
      '<div class="method-list">' +
      methodCard('antigravityLogin', t('antigravity.loginTitle') || 'Sign in with Google', t('antigravity.loginDesc') || '') +
      methodCard('antigravityImport', t('antigravity.importTitle') || 'Import local credentials', t('antigravity.importDesc') || '') +
      '</div>' +
      '<div class="modal-footer"><button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button></div>';
    qsa('.method-card', body).forEach(card => {
      card.addEventListener('click', () => {
        const m = card.dataset.method;
        if (m === 'antigravityLogin') modalAntigravityLogin(title, body);
        else if (m === 'antigravityImport') modalAntigravityImport(title, body);
      });
    });
  }

  // modalAntigravityLogin — PKCE browser flow. The authorize URL is shown for
  // the operator to open; unlike Codex there is no isolated browser profile to
  // launch, so no server-side browser is started.
  var antigravityPollTimer = null;
  var antigravityLoginGeneration = 0;
  function stopAntigravityLoginPolling() {
    antigravityLoginGeneration++;
    if (antigravityPollTimer) {
      clearTimeout(antigravityPollTimer);
      antigravityPollTimer = null;
    }
  }
  function modalAntigravityLogin(title, body) {
    title.textContent = t('antigravity.loginTitle') || 'Sign in with Google';
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('antigravity.loginDesc') || '') + '</p>' +
      '<div id="agStep1">' +
      '<div class="form-group"><label>' + escapeHtml(t('detail.nickname')) + '</label>' +
      '<input type="text" id="agLoginNickname" placeholder="' + escapeAttr(t('antigravity.nicknamePlaceholder') || '') + '" /></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="antigravity" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="agStartLoginBtn" type="button">' + escapeHtml(t('antigravity.startLogin') || t('codex.startLogin')) + '</button>' +
      '</div>' +
      '</div>' +
      '<div id="agStep2" class="hidden">' +
      '<div class="form-group"><label>' + escapeHtml(t('antigravity.authUrl') || t('codex.authUrl')) + '</label>' +
      '<div class="endpoint"><span id="agAuthUrl" class="font-mono text-xs"></span></div>' +
      '<div class="flex gap-2 mt-2">' +
      '<button class="btn btn-sm btn-outline flex-1" id="agOpenBtn" type="button">' + escapeHtml(t('builderid.open')) + '</button>' +
      '<button class="btn btn-sm btn-outline flex-1" id="agCopyBtn" type="button">' + escapeHtml(t('common.copy')) + '</button>' +
      '</div>' +
      '</div>' +
      '<p id="agStatus" class="text-center text-sm mt-4 muted-text">' + escapeHtml(t('builderid.waiting')) + '</p>' +
      '<div class="modal-footer"><button class="btn btn-secondary" id="agCancelBtn" type="button">' + escapeHtml(t('common.cancel')) + '</button></div>' +
      '</div>';
    $('agStartLoginBtn').addEventListener('click', startAntigravityLogin);
  }

  async function startAntigravityLogin() {
    const btn = $('agStartLoginBtn');
    const nicknameInput = $('agLoginNickname');
    if (!btn || !nicknameInput) return;
    const nickname = nicknameInput.value.trim();
    stopAntigravityLoginPolling();
    const generation = antigravityLoginGeneration;
    btn.disabled = true; btn.textContent = t('common.loading') || '...';
    try {
      const res = await api('/auth/antigravity/login', { method: 'POST' });
      const d = await res.json();
      if (generation !== antigravityLoginGeneration) return;
      if (d.error) { toastError(d.error); return; }
      const step1 = $('agStep1'), step2 = $('agStep2'), authURL = $('agAuthUrl');
      const openBtn = $('agOpenBtn'), copyBtn = $('agCopyBtn'), cancelBtn = $('agCancelBtn');
      if (!step1 || !step2 || !authURL || !openBtn || !copyBtn || !cancelBtn) return;
      step1.classList.add('hidden');
      step2.classList.remove('hidden');
      authURL.textContent = d.authUrl;
      openBtn.addEventListener('click', () => { window.open(d.authUrl, '_blank', 'noopener'); });
      copyBtn.addEventListener('click', async () => {
        await copyText(d.authUrl);
        toastPrimary(t('common.copied'));
      });
      cancelBtn.addEventListener('click', cancelAntigravityLogin);
      pollAntigravityLogin(nickname, generation);
    } catch (e) {
      toastError(t('common.failed') + ': ' + (e.message || e));
    } finally {
      btn.disabled = false;
      btn.textContent = t('antigravity.startLogin') || t('codex.startLogin');
    }
  }
  async function cancelAntigravityLogin() {
    stopAntigravityLoginPolling();
    try { await api('/auth/antigravity/cancel', { method: 'POST' }); } catch {}
    showModal('antigravity');
  }
  function pollAntigravityLogin(nickname, generation) {
    if (antigravityPollTimer) clearTimeout(antigravityPollTimer);
    antigravityPollTimer = setTimeout(async () => {
      if (generation !== antigravityLoginGeneration) return;
      try {
        const res = await api('/auth/antigravity/poll', { method: 'POST', body: JSON.stringify({ nickname }) });
        const d = await res.json();
        if (generation !== antigravityLoginGeneration) return;
        if (d.pending) {
          const status = $('antigravityStatus');
          if (!status) return;
          status.textContent = t('builderid.waiting');
          pollAntigravityLogin(nickname, generation);
          return;
        }
        if (d.error) { toastError(d.error); return; }
        if (d.success) {
          closeModal();
          loadAccounts(); loadStats();
          finishAntigravityAdd(d);
        }
      } catch (e) {
        toastError(t('common.failed') + ': ' + (e.message || e));
      }
    }, 2000);
  }

  // finishAntigravityAdd reports the outcome of a login or import. A project
  // failure is surfaced as a warning rather than swallowed: without a
  // cloudaicompanion project the account is saved but cannot serve a request,
  // and the reason is what tells the operator whether retrying will help.
  function finishAntigravityAdd(d) {
    const account = d.account || {};
    const label = account.email || account.nickname || account.id || '';
    toastPrimary((t('antigravity.addSuccess') || t('codex.importSuccess')) + ': ' + label, { duration: 5000 });
    if (account.projectError) {
      toastWarning((t('antigravity.projectFailed') || 'Project discovery failed') + ': ' + account.projectError, { duration: 8000 });
    }
    autoRefreshNewAccount(account.id);
  }

  // modalAntigravityImport reuses credentials an installed Antigravity or
  // Gemini CLI already wrote on this machine. The local file is probed first so
  // the common case is a single click with no token handling by the operator.
  function modalAntigravityImport(title, body) {
    title.textContent = t('antigravity.importTitle') || 'Import local credentials';
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('antigravity.importDesc') || '') + '</p>' +
      '<div id="antigravityLocalBox"><p class="text-center muted-text">' + escapeHtml(t('common.loading') || '...') + '</p></div>' +
      '<details class="mt-3"><summary class="text-sm muted-text cursor-pointer">' + escapeHtml(t('antigravity.manualToggle') || 'Paste tokens manually') + '</summary>' +
      '<div class="form-group mt-2"><label>' + escapeHtml(t('codex.refreshTokenLabel')) + '</label>' +
      '<textarea id="antigravityRefreshToken" class="font-mono" rows="2" placeholder="1//0g..."></textarea></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('antigravity.projectLabel') || 'Project ID') + ' <span class="muted-text">(' + escapeHtml(t('common.optional') || 'optional') + ')</span></label>' +
      '<input type="text" id="antigravityProjectId" class="font-mono" placeholder="' + escapeAttr(t('antigravity.projectPlaceholder') || 'resolved automatically') + '" /></div>' +
      '</details>' +
      '<div class="form-group mt-3"><label>' + escapeHtml(t('detail.nickname')) + '</label>' +
      '<input type="text" id="antigravityNickname" placeholder="' + escapeAttr(t('codex.nicknamePlaceholder')) + '" /></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="antigravity" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importAntigravityBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('importAntigravityBtn').addEventListener('click', importAntigravityCreds);
    loadAntigravityLocalCreds();
  }
  async function loadAntigravityLocalCreds() {
    const box = $('antigravityLocalBox');
    if (!box) return;
    try {
      const res = await api('/auth/antigravity/local', { method: 'GET' });
      const d = await res.json();
      if (!box.isConnected) return;
      if (!d.found) {
        box.innerHTML = '<div class="message message-info"><p>' + escapeHtml(d.error || t('antigravity.noLocalCreds') || '') + '</p></div>';
        return;
      }
      box.innerHTML = '<div class="message message-success">' +
        '<p><strong>' + escapeHtml(d.email || d.name || '') + '</strong></p>' +
        '<p class="text-xs muted-text">' + escapeHtml(t('ninerouter.pathLabel')) + ': <code>' + escapeHtml(d.path) + '</code></p>' +
        (d.projectId ? '<p class="text-xs muted-text">' + escapeHtml(t('antigravity.projectLabel') || 'Project ID') + ': <code>' + escapeHtml(d.projectId) + '</code></p>' : '') +
        '</div>';
    } catch (e) {
      box.innerHTML = '<div class="message message-error"><p>' + escapeHtml(e.message || String(e)) + '</p></div>';
    }
  }
  async function importAntigravityCreds() {
    const refreshToken = ($('antigravityRefreshToken') || {}).value || '';
    const projectId = ($('antigravityProjectId') || {}).value || '';
    const nickname = ($('antigravityNickname') || {}).value || '';
    const btn = $('importAntigravityBtn');
    btn.disabled = true; btn.textContent = t('common.loading') || '...';
    try {
      const res = await api('/auth/antigravity-import', {
        method: 'POST',
        body: JSON.stringify({
          refreshToken: refreshToken.trim(),
          projectId: projectId.trim(),
          nickname: nickname.trim(),
        }),
      });
      const d = await res.json();
      if (d.success) {
        closeModal(); loadAccounts(); loadStats();
        finishAntigravityAdd(d);
      } else {
        toastError(d.error || t('common.failed'));
      }
    } catch (e) {
      toastError(t('common.failed') + ': ' + (e.message || e));
    } finally {
      btn.disabled = false; btn.textContent = t('common.add');
    }
  }

  // ==================== Gommo AutoAI (media generation) ====================

  // modalGommo — Gommo / 79AI media provider. Both the token and the domain are
  // required: the API sends `domain` on every call and rejects a request that
  // omits it, so a saved account without one could never serve traffic.
  //
  // Capabilities are explicit checkboxes rather than inferred, because this
  // account must never enter the chat pool: it generates media and cannot answer
  // a completion, so a wrong capability would route chat traffic into a dead end.
  function modalGommo(title, body) {
    title.textContent = t('gommo.title') || 'Gommo AutoAI';
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('gommo.desc') || '') + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('gommo.tokenLabel') || 'Access token') + '</label>' +
      '<input type="password" id="gommoToken" class="font-mono" autocomplete="off" placeholder="' + escapeAttr(t('gommo.tokenPlaceholder') || '') + '" /></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('gommo.domainLabel') || 'Domain') + '</label>' +
      '<input type="text" id="gommoDomain" class="font-mono" value="79ai.net" /></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('gommo.capabilitiesLabel') || 'Capabilities') + '</label>' +
      '<div class="flex gap-3 flex-wrap">' +
      '<label class="flex items-center gap-1 text-sm"><input type="checkbox" class="gommoCap" value="image" checked /> ' + escapeHtml(t('category.image')) + '</label>' +
      '<label class="flex items-center gap-1 text-sm"><input type="checkbox" class="gommoCap" value="video" checked /> ' + escapeHtml(t('category.video')) + '</label>' +
      '<label class="flex items-center gap-1 text-sm"><input type="checkbox" class="gommoCap" value="audio-tts" checked /> ' + escapeHtml(t('category.audioTts')) + '</label>' +
      '</div></div>' +
      '<details class="mt-1"><summary class="text-sm muted-text cursor-pointer">' + escapeHtml(t('gommo.advancedToggle') || 'Advanced') + '</summary>' +
      '<div class="form-group mt-2"><label>' + escapeHtml(t('external.baseUrlLabel')) + ' <span class="muted-text">(' + escapeHtml(t('common.optional') || 'optional') + ')</span></label>' +
      '<input type="text" id="gommoBaseUrl" class="font-mono" placeholder="https://api.gommo.net" /></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('gommo.projectLabel') || 'Project ID') + '</label>' +
      '<input type="text" id="gommoProjectId" class="font-mono" placeholder="default" /></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('gommo.imageModelLabel') || 'Default image model') + '</label>' +
      '<input type="text" id="gommoImageModel" class="font-mono" placeholder="' + escapeAttr(t('gommo.imageModelPlaceholder') || '') + '" /></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('gommo.ttsModelLabel') || 'Default speech model') + '</label>' +
      '<input type="text" id="gommoTtsModel" class="font-mono" placeholder="eleven_flash_v2_5" /></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('gommo.voiceLabel') || 'Default voice ID') + '</label>' +
      '<input type="text" id="gommoVoiceId" class="font-mono" placeholder="' + escapeAttr(t('gommo.voicePlaceholder') || '') + '" /></div>' +
      '</details>' +
      '<div class="form-group mt-3"><label>' + escapeHtml(t('detail.nickname')) + '</label>' +
      '<input type="text" id="gommoNickname" /></div>' +
      '<label class="flex items-center gap-2 text-sm"><input type="checkbox" id="gommoTest" checked /> ' + escapeHtml(t('gommo.testLabel') || 'Verify the credential now') + '</label>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importGommoBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('importGommoBtn').addEventListener('click', importGommoProvider);
  }

  async function importGommoProvider() {
    const accessToken = $('gommoToken').value.trim();
    const domain = $('gommoDomain').value.trim();
    if (!accessToken) return toastWarning((t('gommo.tokenLabel') || 'Access token') + ' is required');
    // The API rejects any call without a domain, so an account saved without
    // one could never serve a request. Refuse it here instead.
    if (!domain) return toastWarning((t('gommo.domainLabel') || 'Domain') + ' is required');
    const capabilities = qsa('.gommoCap').filter(cb => cb.checked).map(cb => cb.value);
    if (capabilities.length === 0) return toastWarning(t('gommo.capabilitiesRequired') || 'Select at least one capability');

    const payload = {
      accessToken,
      domain,
      capabilities,
      baseUrl: ($('gommoBaseUrl') || {}).value ? $('gommoBaseUrl').value.trim() : '',
      projectId: ($('gommoProjectId') || {}).value ? $('gommoProjectId').value.trim() : '',
      imageModel: ($('gommoImageModel') || {}).value ? $('gommoImageModel').value.trim() : '',
      ttsModel: ($('gommoTtsModel') || {}).value ? $('gommoTtsModel').value.trim() : '',
      voiceId: ($('gommoVoiceId') || {}).value ? $('gommoVoiceId').value.trim() : '',
      nickname: $('gommoNickname').value.trim(),
      test: $('gommoTest').checked,
    };

    const btn = $('importGommoBtn');
    btn.disabled = true; btn.textContent = t('common.loading') || '...';
    try {
      const res = await api('/auth/gommo', { method: 'POST', body: JSON.stringify(payload) });
      const d = await res.json();
      if (!d.success) {
        toastError(d.error || t('common.failed'));
        return;
      }
      closeModal(); loadAccounts(); loadStats();
      const label = d.account?.nickname || d.account?.email || d.account?.id;
      toastPrimary((t('gommo.importSuccess') || t('external.importSuccess')) + ': ' + label, { duration: 5000 });
      // The verification result is reported separately: the account is saved
      // either way so the operator can correct a bad token by editing it.
      if (d.test && d.test.error) toastWarning(t('gommo.verifyFailed') + ': ' + d.test.error, { duration: 8000 });
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
      const providers = d.providers || [];
      const skipped = d.skipped || [];
      if (codex.length === 0 && kiro.length === 0 && providers.length === 0) {
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
          // The raw identifier moves to the row tooltip: it is only needed to
          // disambiguate two identically named accounts, which is rare, and it
          // otherwise crowds the import list.
          const codexRowTitle = [c.name || '(unnamed)', c.chatgptAccountId ? 'ID: ' + c.chatgptAccountId : ''].filter(Boolean).join(' · ');
          html += '<label class="ninerouter-row' + (c.hasToken ? '' : ' muted-text') + '" title="' + escapeAttr(codexRowTitle) + '">' +
            '<input type="checkbox" class="ninerouter-codex-cb" data-idx="' + i + '" data-source-id="' + escapeHtml(c.sourceId || '') + '" ' + checked + ' ' + disabled + ' />' +
            '<span class="ninerouter-name">' + escapeHtml(c.name || '(unnamed)') + '</span>' +
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
            '<input type="checkbox" class="ninerouter-kiro-cb" data-idx="' + i + '" data-source-id="' + escapeHtml(k.sourceId || '') + '" ' + checked + ' ' + disabled + ' />' +
            '<span class="ninerouter-name">' + escapeHtml(k.name || '(unnamed)') + '</span>' +
            (k.profileArn ? '<span class="text-xs">' + escapeHtml(k.profileArn.split('/').pop()) + '</span>' : '') +
            (k.hasToken ? '' : '<span class="text-xs">' + escapeHtml(t('ninerouter.noToken')) + '</span>') +
            '</label>';
        });
        html += '</div>';
      }
      // Generic provider group (search/image/chat connections).
      if (providers.length > 0) {
        html += '<div class="account-category-header" style="margin-top:0.75rem;">' +
          '<span class="account-category-icon"><i class="fa-solid fa-plug"></i></span>' +
          '<span class="account-category-title">Providers (' + providers.length + ')</span>' +
          '</div>';
        html += '<div class="ninerouter-list">';
        providers.forEach((p, i) => {
          const checked = p.hasToken ? 'checked' : '';
          const disabled = p.hasToken ? '' : 'disabled';
          const kind = p.providerKind ? ' · ' + p.providerKind : '';
          const caps = Array.isArray(p.capabilities) && p.capabilities.length > 0
            ? ' · ' + p.capabilities.join(', ')
            : '';
          html += '<label class="ninerouter-row' + (p.hasToken ? '' : ' muted-text') + '">' +
            '<input type="checkbox" class="ninerouter-provider-cb" data-idx="' + i + '" data-source-id="' + escapeHtml(p.sourceId || '') + '" ' + checked + ' ' + disabled + ' />' +
            '<span class="ninerouter-name">' + escapeHtml(p.name || p.provider || '(unnamed)') + '</span>' +
            '<span class="text-xs">' + escapeHtml((p.provider || '') + kind + caps) + '</span>' +
            (p.hasToken ? '' : '<span class="text-xs">' + escapeHtml(t('ninerouter.noToken')) + '</span>') +
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
  function selectedNineRouterAccounts(selector) {
    const sourceIds = [];
    const indexes = [];
    qsa(selector).forEach(function(cb) {
      if (!cb.checked) return;
      const sourceId = (cb.dataset.sourceId || '').trim();
      if (sourceId) sourceIds.push(sourceId);
      else indexes.push(Number(cb.dataset.idx));
    });
    return { sourceIds: sourceIds, indexes: indexes };
  }
  async function importFrom9Router() {
    const codexSelection = selectedNineRouterAccounts('.ninerouter-codex-cb');
    const kiroSelection = selectedNineRouterAccounts('.ninerouter-kiro-cb');
    const providerSelection = selectedNineRouterAccounts('.ninerouter-provider-cb');
    const importCodex = codexSelection.sourceIds.length > 0 || codexSelection.indexes.length > 0;
    const importKiro = kiroSelection.sourceIds.length > 0 || kiroSelection.indexes.length > 0;
    const importProviders = providerSelection.sourceIds.length > 0 || providerSelection.indexes.length > 0;
    const btn = $('import9RouterBtn');
    btn.disabled = true; btn.textContent = t('common.loading') || '...';
    try {
      const res = await api('/auth/import-9router', { method: 'POST', body: JSON.stringify({
        importCodex: importCodex,
        importKiro: importKiro,
        importProviders: importProviders,
        codexSourceIds: codexSelection.sourceIds,
        codexIndexes: codexSelection.indexes,
        kiroSourceIds: kiroSelection.sourceIds,
        kiroIndexes: kiroSelection.indexes,
        providerSourceIds: providerSelection.sourceIds,
        providerIndexes: providerSelection.indexes,
        refreshKiro: true
      }) });
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
