'use strict';

// OmniProxy Usage Page — real-time topology, chart, and tables
// Ported from 9Router's React-based usage page to vanilla JS

// ─── State ───────────────────────────────────────────────
let usageState = {
  period: '7d',
  chartView: 'tokens',
  tableView: 'model',
  tableViewMode: 'tokens',
  sortBy: {},
  sortOrder: {},
  stats: null,
  chartData: [],
  eventSource: null,
  refreshTimer: null,
  // SSE reconnect bookkeeping. `sseGeneration` is bumped on every explicit
  // disconnect so a retry timer scheduled by a *previous* connection cannot
  // resurrect the stream after the user left the page (zombie EventSource).
  sseGeneration: 0,
  sseRetryTimer: null,
  sseRetryDelay: 0,
  detailsData: [],
  detailsPagination: { page: 1, pageSize: 20, totalItems: 0, totalPages: 0 },
  detailsLoading: false,
  detailsProviders: [],
  detailsFilters: { provider: '', startDate: '', endDate: '' },
  selectedDetail: null,
  isDrawerOpen: false,
  activeTab: 'overview',
  // Details receives completed-request deltas from the shared Usage SSE stream.
  detailsLastEventId: 0,
  detailsPendingEvents: [],
  detailsSeenKeys: new Set(),
  detailsSeenOrder: [],
};

// Cache sub-tab state
let cacheState = {
  loading: false,
  data: null,
  // Usage stats for the same period, kept alongside the cache stats so the
  // hit ratio can be split by whether a model has known pricing. Null when
  // the usage fetch failed — the tab then falls back to the unsplit view.
  usage: null,
  period: '24h',
};

// Auto-refresh: refetch the active tab's data periodically so numbers
// update in real time without a manual reload.
const AUTO_REFRESH_INTERVAL_MS = 5000;
let autoRefreshEnabled = true;
let lastRefreshAt = 0;

let topoZoom = 1;
let topoPanX = 0;
let topoPanY = 0;
let topoSvgBuilt = false;
let topoDragRaf = null;
let topoListenersRegistered = false;
let topoDragState = { dragging: false, startX: 0, startY: 0, panStartX: 0, panStartY: 0 };

// Edge state tracking (9router-style)
const FE_ACTIVE_TIMEOUT_MS = 60000;
const FE_ACTIVE_TICK_MS = 1000;
let topoFirstSeen = {};
let topoTickTimer = null;
// Estimate text width in SVG based on character count and font size
function estimateTextWidth(text, fontSize) {
  if (!text) return 0;
  // Approximate: each char ~0.65 * fontSize for typical sans-serif
  return text.length * fontSize * 0.65;
}

// ─── Helpers ─────────────────────────────────────────────
function fmtNum(n) { return new Intl.NumberFormat().format(n || 0); }
function fmtCost(n) { return '$' + (n || 0).toFixed(2); }
function fmtTokens(n) {
  if (n >= 1000000) return (n / 1000000).toFixed(1) + 'M';
  if (n >= 1000) return (n / 1000).toFixed(1) + 'K';
  return String(n || 0);
}
function fmtTokenFull(n) { return fmtNum(n); }

function timeAgo(ts) {
  if (!ts) return (typeof t === 'function' ? t('usage.time.never') : 'Never');
  const diff = Math.floor((Date.now() - new Date(ts).getTime()) / 1000);
  if (diff < 10) return (typeof t === 'function' ? t('usage.time.justNow') : 'Just now');
  if (diff < 60) return (typeof t === 'function' ? t('usage.time.secondsAgo', diff) : 's ago');
  if (diff < 3600) return (typeof t === 'function' ? t('usage.time.minutesAgo', Math.floor(diff / 60)) : 'm ago');
  if (diff < 86400) return (typeof t === 'function' ? t('usage.time.hoursAgo', Math.floor(diff / 3600)) : 'h ago');
  return (typeof t === 'function' ? t('usage.time.daysAgo', Math.floor(diff / 86400)) : 'd ago');
}

function fmtTime(iso) {
  if (!iso) return (typeof t === 'function' ? t('usage.time.never') : 'Never');
  const diffMins = Math.floor((Date.now() - new Date(iso)) / 60000);
  if (diffMins < 1) return (typeof t === 'function' ? t('usage.time.justNow') : 'Just now');
  if (diffMins < 60) return (typeof t === 'function' ? t('usage.time.minutesAgo', diffMins) : 'm ago');
  if (diffMins < 1440) return Math.floor(diffMins / 60) + 'h ago';
  return new Date(iso).toLocaleDateString();
}

// setHtmlIfChanged writes innerHTML only when the rendered markup actually
// differs from what is already mounted. The live view re-renders every SSE
// event and every auto-refresh tick; unconditional innerHTML assignment
// destroys and rebuilds the subtree each time, which restarts CSS/SVG
// animations, drops hover/tooltip state, and causes the visible flicker.
// Comparing the string first makes an unchanged tick a no-op.
function setHtmlIfChanged(el, html) {
  if (!el) return false;
  if (el.dataset.renderHash === html) return false;
  el.dataset.renderHash = html;
  el.innerHTML = html;
  return true;
}

function createTimeAgoEl(ts) {
  const el = document.createElement('span');
  el.className = 'usage-time-ago';
  el.dataset.ts = ts;
  el.textContent = timeAgo(ts);
  return el;
}

function updateTimeAgoEls() {
  document.querySelectorAll('.usage-time-ago').forEach(el => {
    if (el.dataset.ts) el.textContent = timeAgo(el.dataset.ts);
  });
}

// ─── API ─────────────────────────────────────────────────
// lastStatsSignature holds the raw JSON text of the previous /usage/stats
// response. renderUsagePage() rebuilds large subtrees via innerHTML, which on
// every 5s tick discards focus, text selection and scroll position inside the
// tables. When the proxy is idle the payload is byte-identical, so comparing
// the serialized body lets an unchanged poll skip the repaint entirely.
//
// Note this is separate from the server-side ETag: a 304 still resolves to the
// cached body here, so the client comparison is what actually prevents the DOM
// churn. The ETag saves bandwidth; this saves the repaint.
let lastStatsSignature = null;

// invalidateStatsSignature forces the next fetch to repaint even if the payload
// is unchanged. Required whenever local view state (period, tab, filters)
// changes, because the same bytes must then render differently.
function invalidateStatsSignature() {
  lastStatsSignature = null;
}

async function fetchUsageStats(period) {
  try {
    const res = await api('/usage/stats?period=' + (period || usageState.period));
    if (res.ok) {
      const body = await res.text();
      const changed = body !== lastStatsSignature;
      lastStatsSignature = body;
      usageState.stats = JSON.parse(body);
      if (!changed) return;
      renderUsagePage();
    } else {
      console.error('[Usage] fetchStats failed:', res.status, res.statusText);
    }
  } catch (e) { console.error('[Usage] fetchStats error:', e); }
}

async function fetchUsageChart(period) {
  try {
    const res = await api('/usage/chart?period=' + (period || usageState.period));
    if (res.ok) {
      usageState.chartData = await res.json();
      renderChart();
    }
  } catch (e) { console.error('[Usage] fetchChart error:', e); }
}

// ─── SSE ─────────────────────────────────────────────────
// Reconnect policy: exponential backoff (3s → 6s → 12s → … capped at 60s)
// instead of a fixed 3s hammer. A flapping backend used to cost ~281 MB/h of
// full-buffer replays; backoff plus Last-Event-ID resume keeps that near zero.
const USAGE_SSE_RETRY_BASE_MS = 3000;
const USAGE_SSE_RETRY_MAX_MS = 60000;

function scheduleUsageSSERetry(generation) {
  // Never stack timers, and never retry for a generation that was already
  // torn down by disconnectUsageSSE().
  if (generation !== usageState.sseGeneration) return;
  if (usageState.sseRetryTimer) return;

  const prev = usageState.sseRetryDelay || 0;
  const delay = prev === 0
    ? USAGE_SSE_RETRY_BASE_MS
    : Math.min(prev * 2, USAGE_SSE_RETRY_MAX_MS);
  usageState.sseRetryDelay = delay;

  usageState.sseRetryTimer = setTimeout(() => {
    usageState.sseRetryTimer = null;
    // Re-check: the user may have left the page while this timer was pending.
    if (generation !== usageState.sseGeneration) return;
    connectUsageSSE();
  }, delay);
}

function connectUsageSSE() {
  if (usageState.eventSource) {
    usageState.eventSource.close();
  }
  if (usageState.sseRetryTimer) {
    clearTimeout(usageState.sseRetryTimer);
    usageState.sseRetryTimer = null;
  }
  const generation = usageState.sseGeneration;
  try {
    const es = new EventSource('/admin/api/usage/stream?token=' + encodeURIComponent(adminToken));
    usageState.eventSource = es;

    es.onopen = function () {
      // Successful connect resets the backoff ladder.
      usageState.sseRetryDelay = 0;
    };

    es.onmessage = function (e) {
      try {
        const data = JSON.parse(e.data);
        if (data.requestCompleted) {
          applyDetailsRequestCompleted(data.requestCompleted, data.eventId);
        }
        if (data.recentRequests || data.activeRequests) {
          if (usageState.stats) {
            if (data.recentRequests) usageState.stats.recentRequests = data.recentRequests;
            if (data.activeRequests) usageState.stats.activeRequests = data.activeRequests;
          } else {
            usageState.stats = data;
          }
          if (usageState.activeTab === 'overview') {
            try { renderRecentRequests(); } catch(e) { console.error('[Usage] SSE recentRequests:', e); }
            try { renderTopology(); } catch(e) { console.error('[Usage] SSE topology:', e); }
          }
        }
      } catch (err) { /* ignore parse errors */ }
    };

    es.onerror = function () {
      scheduleUsageSSERetry(generation);
    };
  } catch (e) {
    console.error('[Usage] SSE connect error:', e);
    scheduleUsageSSERetry(generation);
  }
}

function disconnectUsageSSE() {
  // Bump the generation first: any retry timer already scheduled by the
  // connection we are about to close becomes a no-op.
  usageState.sseGeneration++;
  usageState.sseRetryDelay = 0;
  if (usageState.sseRetryTimer) {
    clearTimeout(usageState.sseRetryTimer);
    usageState.sseRetryTimer = null;
  }
  if (usageState.eventSource) {
    usageState.eventSource.close();
    usageState.eventSource = null;
  }
}

// ─── Auto-refresh ────────────────────────────────────────
// Refetch the active tab's data so headline numbers update in real time.
// Details tab is skipped (user is paginating / filtering).
function refreshActiveTab() {
  if (!autoRefreshEnabled) return;
  // Hidden tab: nothing is on screen, so skip the fetch entirely. /usage/stats
  // is the heaviest payload in the dashboard (~205 KB), and at 5s that is
  // ~140 MB/h burned on a tab nobody is looking at. onUsageVisible() below does
  // a single catch-up fetch when the tab comes back.
  if (document.hidden) return;
  const tab = usageState.activeTab;
  if (tab === 'overview') {
    fetchUsageStats(usageState.period);
    fetchUsageChart(usageState.period);
  } else if (tab === 'cache') {
    fetchCacheStats();
  } else if (tab === 'compression') {
    refreshCompressionStatsOnly();
  } else if (tab === 'pool') {
    fetchPoolStrategy();
  }
  // 'details' intentionally skipped — auto-refresh would reset pagination.
  lastRefreshAt = Date.now();
  updateLiveIndicator();
}

// Compression: refresh only the stats numbers, leave toggles/inputs untouched
// so user interaction (typing URL, clicking toggles) is not disrupted.
async function refreshCompressionStatsOnly() {
  try {
    const res = await api('/compression/stats');
    if (!res.ok) return;
    compressionState.data = await res.json();
    const section = document.getElementById('compStatsSection');
    if (section) section.innerHTML = renderCompressionStatsHtml(compressionState.data);
  } catch (e) { /* silent — next tick retries */ }
}

function renderCompressionStatsHtml(d) {
  const s = (d && d.stats) || {};
  const tokensIn = s.toolTokensIn || 0;
  const tokensOut = s.toolTokensOut || 0;
  const toolSaved = tokensIn - tokensOut;
  const toolSavedPct = tokensIn > 0 ? (toolSaved / tokensIn * 100) : 0;
  return `
    <div class="usage-overview-grid">
      <div class="usage-overview-card">
        <div class="usage-overview-card-value">${s.toolCompressed || 0}</div>
        <div class="usage-overview-card-label">Tool Outputs Compressed</div>
      </div>
      <div class="usage-overview-card">
        <div class="usage-overview-card-value" style="color:#22c55e">${fmtTokens(toolSaved)}</div>
        <div class="usage-overview-card-label">Tool Tokens Saved</div>
      </div>
      <div class="usage-overview-card">
        <div class="usage-overview-card-value" style="color:#22c55e">${fmtPct(toolSavedPct)}</div>
        <div class="usage-overview-card-label">Tool Savings</div>
      </div>
      <div class="usage-overview-card">
        <div class="usage-overview-card-value">${s.headroomReqs || 0}</div>
        <div class="usage-overview-card-label">Headroom Requests</div>
      </div>
      <div class="usage-overview-card">
        <div class="usage-overview-card-value">${s.cavemanReqs || 0}</div>
        <div class="usage-overview-card-label">Caveman Requests</div>
      </div>
    </div>`;
}

function updateLiveIndicator() {
  const el = document.getElementById('usageLiveIndicator');
  if (!el) return;
  const dot = autoRefreshEnabled ? '<span class="live-dot live-dot-on"></span>' : '<span class="live-dot live-dot-off"></span>';
  const label = autoRefreshEnabled
    ? (typeof t === 'function' ? t('usage.liveAuto') : 'Live')
    : (typeof t === 'function' ? t('usage.livePaused') : 'Paused');
  el.innerHTML = dot + '<span class="live-label">' + label + '</span>';
}

function toggleAutoRefresh() {
  autoRefreshEnabled = !autoRefreshEnabled;
  updateLiveIndicator();
  if (autoRefreshEnabled) refreshActiveTab();
}

// ─── Topology (SVG) ──────────────────────────────────────

// Compute intersection point of a ray from rectangle center to its boundary
function rectIntersect(cx, cy, w, h, angle) {
  const dx = Math.cos(angle);
  const dy = Math.sin(angle);
  const hw = w / 2;
  const hh = h / 2;
  var tx = Infinity, ty = Infinity;
  if (dx > 0) tx = hw / dx;
  else if (dx < 0) tx = -hw / dx;
  if (dy > 0) ty = hh / dy;
  else if (dy < 0) ty = -hh / dy;
  var t = Math.min(tx, ty);
  if (!isFinite(t)) t = Math.max(tx, ty);
  return { x: cx + t * dx, y: cy + t * dy };
}


function renderTopology() {
  const container = document.getElementById('usageTopology');
  if (!container) return;
  const topoStats = usageState.stats;
  if (!topoStats) {
    container.innerHTML = '<div class="usage-loading">Loading...</div>';
    topoSvgBuilt = false;
    return;
  }
  // Build a deduplicated account map keyed by accountId.
    // We use accountId as the key to ensure each account appears once.
    const stats = topoStats;
    const activeReqs = stats.activeRequests || [];
    const recentReqs = stats.recentRequests || [];
    const accountMap = {};

    // First, populate from byAccount (keys are already accountIds from backend)
    for (const [accountId, val] of Object.entries(stats.byAccount || {})) {
        if (accountId && !accountMap[accountId]) {
            accountMap[accountId] = { displayName: accountId };
        }
    }

    // Add accounts from active requests
    for (const r of activeReqs) {
        if (r.accountId && !accountMap[r.accountId]) {
            accountMap[r.accountId] = { displayName: r.account || r.accountId };
        }
    }

    // Add accounts from recent requests
    for (const r of recentReqs) {
        if (r.accountId && !accountMap[r.accountId]) {
            accountMap[r.accountId] = { displayName: r.account || r.accountId };
        }
    }

    // Set proper display names from accountNames (emails, nicknames, etc.)
    if (stats.accountNames) {
        for (const [accId, name] of Object.entries(stats.accountNames)) {
            if (!accountMap[accId]) accountMap[accId] = {};
            accountMap[accId].displayName = name;
        }
    }

  const accounts = Object.keys(accountMap);
  const activeReqsSet = new Set();
  for (const r of activeReqs) {
    if (r.accountId) activeReqsSet.add(r.accountId);
  }
  const recentReqsSet = new Set();
  for (const r of recentReqs) {
    if (r.accountId) recentReqsSet.add(r.accountId);
  }

  // Tick timer for active timeout (create once)
  if (!topoTickTimer) {
    topoTickTimer = setInterval(function() {
      const now = Date.now();
      const s = usageState.stats;
      if (!s) return;

      const aReqs = s.activeRequests || [];
      const rReqs = s.recentRequests || [];

      // Only check for timeouts if we have tracked accounts
      if (Object.keys(topoFirstSeen).length === 0) return;

      // Find if any account has timed out (>60s since last activity)
      let needsRerender = false;
      for (const accountId of Object.keys(topoFirstSeen)) {
        const ts = topoFirstSeen[accountId];
        if (ts && now - ts >= FE_ACTIVE_TIMEOUT_MS) {
          needsRerender = true;
          break;
        }
      }

      if (needsRerender) {
        renderTopology();
      }
    }, FE_ACTIVE_TICK_MS);
  }

  // Track first-seen per account (for 60s timeout).
  // Refresh timestamp for currently active accounts AND recently completed ones.
  const now = Date.now();
  for (const a of activeReqsSet) {
    topoFirstSeen[a] = now;
  }
  for (const a of recentReqsSet) {
    // Always refresh timestamp for recent accounts to reset the 60s timeout
    topoFirstSeen[a] = now;
  }
  // Remove accounts not seen in either set for longer than the timeout
  const stillSeen = new Set([...activeReqsSet, ...recentReqsSet]);
  for (const a of Object.keys(topoFirstSeen)) {
    if (!stillSeen.has(a) && now - (topoFirstSeen[a] || 0) >= FE_ACTIVE_TIMEOUT_MS) {
      delete topoFirstSeen[a];
    }
  }
  // Apply timeout: accounts with firstSeen > 60s ago are excluded
  // Also track accounts in final 10s (fade-out phase)
  const activeAccounts = new Set();
  const fadingAccounts = new Set();
  for (const a of Object.keys(topoFirstSeen)) {
    const ts = topoFirstSeen[a];
    const age = now - ts;
    if (!ts || age < FE_ACTIVE_TIMEOUT_MS) {
      activeAccounts.add(a);
      // Mark as fading if in the last 10 seconds before timeout
      if (age >= FE_ACTIVE_TIMEOUT_MS - 10000) {
        fadingAccounts.add(a);
      }
    }
  }

  // activeAccounts contains connectionIds that are active; ensure they're in accountMap
  for (const a of activeAccounts) {
    if (!accountMap[a]) {
      const name = stats.accountNames && stats.accountNames[a] ? stats.accountNames[a] : a;
      accountMap[a] = { displayName: name };
    }
  }

  const allAccounts = new Set([...accounts, ...activeAccounts]);
  const accountsList = Array.from(allAccounts);
  // Use accountsList as accounts (connectionId-based, one per account)
  // Replace the accounts variable so downstream code uses deduplicated list
  accounts.length = 0;
  accounts.push(...accountsList);
  const activeSet = activeAccounts;

  // Determine "last" account (most recent request)
  const lastAccountId = (recentReqs.length > 0) ? recentReqs[0].accountId : null;



  if (accounts.length === 0) {
    container.innerHTML = '<div class="usage-empty-state">' + (typeof t === 'function' ? t('usage.noAccountsConnected') : 'No accounts connected') + '</div>';
    topoSvgBuilt = false;
    return;
  }

  // Use account names from backend (populated by GetStats)

  const width = container.clientWidth || 600;
  const height = container.clientHeight || 340;
  const cx = width / 2;
  const cy = height / 2;
  const rx = Math.max(width * 0.3, 200);
  const ry = Math.max(height * 0.25, 120);
  const n = accounts.length;

  // Flex width calculation — fit to text
  const nodeFontSize = 11;
  const centerFontSize = 13;
  
  // Account nodes: width = text + padding + indicator space
  const accountNodeH = 44;
  const centerNodeH = 46;

  // Calculate each account node width dynamically from display text (3px padding each side)
  const accountNodeWidths = accounts.map((id, i) => {
    const rawName = accountMap[id] ? accountMap[id].displayName : id;
    const dispName = rawName.length > 6 ? rawName.substring(0, 6) + '\u2026' : rawName;
    const textW = estimateTextWidth(dispName, nodeFontSize);
    return Math.max(40, Math.ceil(textW + 6));
  });

  // Center node width: fit "OmniProxy" + badge space
  const centerTextW = estimateTextWidth("OmniProxy", centerFontSize);
  const centerTextPad = 20;
  const centerBadgeSpace = activeSet.size > 0 ? 28 : 0;
  const centerNodeW = Math.max(80, Math.ceil(centerTextW + centerTextPad + centerBadgeSpace));

  // Build zoom bar HTML
  let html = '<div class="usage-topo-zoom-bar">' +
    '<button class="usage-topo-zoom-btn" data-zoom="in" title="' + (typeof t === 'function' ? t('usage.zoomIn') : 'Zoom in') + '">' +
    '<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2"><circle cx="11" cy="11" r="8"/><line x1="21" y1="21" x2="16.65" y2="16.65"/><line x1="11" y1="8" x2="11" y2="14"/><line x1="8" y1="11" x2="14" y2="11"/></svg></button>' +
    '<span class="usage-topo-zoom-label" id="topoZoomLabel">' + Math.round(topoZoom * 100) + '%</span>' +
    '<button class="usage-topo-zoom-btn" data-zoom="out" title="' + (typeof t === 'function' ? t('usage.zoomOut') : 'Zoom out') + '">' +
    '<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2"><circle cx="11" cy="11" r="8"/><line x1="21" y1="21" x2="16.65" y2="16.65"/><line x1="8" y1="11" x2="14" y2="11"/></svg></button>' +
    '<button class="usage-topo-zoom-btn" data-zoom="reset" title="' + (typeof t === 'function' ? t('usage.resetView') : 'Reset view') + '">' +
    '<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2"><path d="M3 12a9 9 0 1 0 9-9 9.75 9.75 0 0 0-6.74 2.74L3 8"/><path d="M3 3v5h5"/></svg></button>' +
    '</div>';

  let svg = '<svg width="100%" height="100%" viewBox="0 0 ' + width + ' ' + height + '" class="usage-topology-svg">' +
    '<defs><filter id="activeGlow"><feGaussianBlur stdDeviation="2.5" result="blur"/><feMerge><feMergeNode in="blur"/><feMergeNode in="SourceGraphic"/></feMerge></filter></defs>';

  // Transform group
  svg += '<g id="topo-transform" transform="translate(' + topoPanX + ',' + topoPanY + ') scale(' + topoZoom + ')">';

  // Connection lines — 4 edge states (error/active/last/default) + smart handles
  for (let i = 0; i < n; i++) {
    const acctId = accounts[i];
    const angle = (2 * Math.PI * i) / n - Math.PI / 2;
    const ax = cx + rx * Math.cos(angle);
    const ay = cy + ry * Math.sin(angle);
    const acctW = accountNodeWidths[i];

    // Determine edge state
    const isActive = activeSet.has(acctId);
    const isFading = fadingAccounts.has(acctId);
    const isLast = !isActive && lastAccountId === acctId;
    // Error: check if any recent request for this account has non-success status
    const hasError = !isActive && (stats.recentRequests || []).some(r =>
      r.accountId === acctId && r.status && r.status !== 'success' && r.status !== 'ok'
    );

    // Edge style matching 9router
    let edgeColor, edgeWidth, edgeOpacity;
    if (hasError) {
      edgeColor = '#ef4444'; edgeWidth = 2.5; edgeOpacity = 0.9;
    } else if (isActive) {
      edgeColor = '#22c55e'; edgeWidth = 2.5; edgeOpacity = 0.9;
    } else if (isLast) {
      edgeColor = '#f59e0b'; edgeWidth = 2; edgeOpacity = 0.7;
    } else {
      edgeColor = 'var(--color-border)'; edgeWidth = 1; edgeOpacity = 0.3;
    }

    // Smart handle positioning matching 9router
    let sourceHandle, targetHandle;
    // angle is from -π/2 (top) clockwise
    if (Math.abs(angle + Math.PI / 2) < Math.PI / 4 || Math.abs(angle - 3 * Math.PI / 2) < Math.PI / 4) {
      // Top quadrant — node above router
      sourceHandle = 'top'; targetHandle = 'bottom';
    } else if (Math.abs(angle - Math.PI / 2) < Math.PI / 4) {
      // Bottom quadrant — node below router
      sourceHandle = 'bottom'; targetHandle = 'top';
    } else if (ax > cx) {
      // Right side
      sourceHandle = 'right'; targetHandle = 'left';
    } else {
      // Left side
      sourceHandle = 'left'; targetHandle = 'right';
    }

    // Compute start/end points at the selected edges of each box
    const edgeOffsets = {
      top:    { x: 0, y: -centerNodeH/2 },
      bottom: { x: 0, y: centerNodeH/2 },
      left:   { x: -centerNodeW/2, y: 0 },
      right:  { x: centerNodeW/2, y: 0 },
    };
    const acctEdgeOffsets = {
      top:    { x: 0, y: -accountNodeH/2 },
      bottom: { x: 0, y: accountNodeH/2 },
      left:   { x: -acctW/2, y: 0 },
      right:  { x: acctW/2, y: 0 },
    };
    const startOff = edgeOffsets[sourceHandle];
    const endOff = acctEdgeOffsets[targetHandle];
    const sx = cx + startOff.x, sy = cy + startOff.y;
    const ex = ax + endOff.x, ey = ay + endOff.y;

    svg += '<line x1="' + sx + '" y1="' + sy + '" x2="' + ex + '" y2="' + ey + '" ' +
      'stroke="' + edgeColor + '" stroke-width="' + edgeWidth + '" ' +
      'stroke-opacity="' + edgeOpacity + '" ' +
      'stroke-dasharray="' + (isActive ? '8 4' : 'none') + '" ' +
      'class="usage-topo-edge' + (isActive ? ' active' : '') + (isFading ? ' fading' : '') + '" />';

    if (isActive) {
      svg += '<circle cx="' + sx + '" cy="' + sy + '" r="4" fill="#22c55e" class="usage-topo-flow-dot">' +
        '<animate attributeName="cx" values="' + sx + ';' + ex + '" dur="1.5s" repeatCount="indefinite"/>' +
        '<animate attributeName="cy" values="' + sy + ';' + ey + '" dur="1.5s" repeatCount="indefinite"/>' +
        '<animate attributeName="opacity" values="1;0.3;1" dur="1.5s" repeatCount="indefinite"/>' +
        '</circle>';
    }
  }

  // Account nodes
  for (let i = 0; i < n; i++) {
    const angle = (2 * Math.PI * i) / n - Math.PI / 2;
    const ax = cx + rx * Math.cos(angle);
    const ay = cy + ry * Math.sin(angle);
    const acctId = accounts[i];
    const isActive = activeSet.has(acctId);
    const isFading = fadingAccounts.has(acctId);
    const fullName = accountMap[acctId] ? accountMap[acctId].displayName : acctId;
    const displayName = fullName.length > 6 ? fullName.substring(0, 6) + '\u2026' : fullName;
    const nodeW = accountNodeWidths[i];

    svg += '<g class="usage-topo-node' + (isActive ? ' active' : '') + (isFading ? ' fading' : '') + '" data-account="' + escAttr(acctId) + '">';
    // Tooltip
    svg += '<title>' + escHtml(fullName) + '</title>';
    // Rectangle background
    svg += '<rect x="' + (ax - nodeW / 2) + '" y="' + (ay - accountNodeH / 2) + '" width="' + nodeW + '" height="' + accountNodeH + '" rx="10" ry="10" ' +
      'fill="none" stroke="' + (isActive ? '#22c55e' : 'var(--foreground)') + '" stroke-width="' + (isActive ? '2.5' : '2') + '"' +
      (isActive ? ' filter="url(#activeGlow)"' : '') + '/>';
    // Avatar circle with initial
    // Name text — centered in node like OmniProxy
    svg += '<text x="' + ax + '" y="' + (ay + 4) + '" text-anchor="middle" fill="var(--foreground)" font-size="' + nodeFontSize + '" font-weight="500">' + escHtml(displayName) + '</text>';
    // Active indicator
    if (isActive) {
      // Ping-style active indicator (two rings + dot) like 9router
      const dotX = ax + nodeW / 2 - 10;
      // Outer ping ring
      svg += '<circle cx="' + dotX + '" cy="' + ay + '" r="6" fill="none" stroke="#22c55e" stroke-width="1.5" opacity="0.4">' +
        '<animate attributeName="r" values="4;10;4" dur="1.5s" repeatCount="indefinite"/>' +
        '<animate attributeName="opacity" values="0.6;0;0.6" dur="1.5s" repeatCount="indefinite"/>' +
        '</circle>';
      // Solid dot
      svg += '<circle cx="' + dotX + '" cy="' + ay + '" r="4" fill="#22c55e">' +
        '<animate attributeName="opacity" values="1;0.4;1" dur="2s" repeatCount="indefinite"/></circle>';
    }
    svg += '</g>';
  }

  // Center OmniProxy node
  svg += '<g class="usage-topo-center">';
  svg += '<title>' + (typeof t === 'function' ? t('usage.omniProxyRouter') : 'OmniProxy Router') + '</title>';
  svg += '<rect x="' + (cx - centerNodeW / 2) + '" y="' + (cy - centerNodeH / 2) + '" width="' + centerNodeW + '" height="' + centerNodeH + '" ' +
    'fill="none" stroke="var(--primary)" stroke-width="2.5" rx="12" ry="12"/>';
  svg += '<text x="' + cx + '" y="' + (cy + 5) + '" text-anchor="middle" fill="var(--foreground)" font-size="' + centerFontSize + '" font-weight="700">' + (typeof t === 'function' ? t('usage.omniProxy') : 'OmniProxy') + '</text>';
  if (activeSet.size > 0) {
    const badgeX = cx + centerNodeW / 2 - 10;
    const badgeW = 20;
    svg += '<rect x="' + (badgeX - badgeW / 2) + '" y="' + (cy - centerNodeH / 2 - 8) + '" width="' + badgeW + '" height="18" rx="9" ry="9" fill="var(--destructive)"/>';
    svg += '<text x="' + badgeX + '" y="' + (cy - centerNodeH / 2 + 5) + '" text-anchor="middle" fill="var(--foreground)" font-size="10" font-weight="700">' + activeSet.size + '</text>';
  }
  svg += '</g>';

  svg += '</g></svg>';

  html += svg;
  // Rebuilding the topology SVG on every tick restarts each <animate> element
  // (flow dots, ping rings) from zero and re-creates the drag target, which is
  // what made the live view stutter. Skip the write when nothing changed.
  const topoChanged = setHtmlIfChanged(container, html);
  topoSvgBuilt = true;

  applyTopoTransform();
  if (topoChanged) bindTopoEvents(container);
  
  // Setup ResizeObserver for auto-fit (9router-style)
  if (container._topoResizeObserver) container._topoResizeObserver.disconnect();
  try {
    container._topoResizeObserver = new ResizeObserver(function() {
      requestAnimationFrame(function() {
        // Re-center transform after resize
        const newW = container.clientWidth || 600;
        const newH = container.clientHeight || 340;
        if (newW > 0 && newH > 0) {
          applyTopoTransform();
        }
      });
    });
    container._topoResizeObserver.observe(container);
  } catch(e) { /* ResizeObserver not supported */ }
}
function applyTopoTransform() {
  const g = document.getElementById('topo-transform');
  if (g) {
    g.setAttribute('transform', 'translate(' + topoPanX + ',' + topoPanY + ') scale(' + topoZoom + ')');
  }
  const label = document.getElementById('topoZoomLabel');
  if (label) {
    label.textContent = Math.round(topoZoom * 100) + '%';
  }
}

function bindTopoEvents(container) {
  // Zoom controls
  container.querySelectorAll('.usage-topo-zoom-btn').forEach(btn => {
    btn.addEventListener('click', function (e) {
      e.stopPropagation();
      const action = this.dataset.zoom;
      if (action === 'in') {
        topoZoom = Math.min(topoZoom * 1.3, 3);
      } else if (action === 'out') {
        topoZoom = Math.max(topoZoom / 1.3, 0.3);
      } else if (action === 'reset') {
        topoZoom = 1;
        topoPanX = 0;
        topoPanY = 0;
      }
      applyTopoTransform();
    });
  });

  // Use event delegation on container (never replaced) instead of SVG (replaced on SSE)
  container.addEventListener('mousedown', function (e) {
    // Only handle clicks directly on SVG or its children (not zoom bar)
    if (!e.target.closest('svg')) return;
    if (e.target.closest('.usage-topo-zoom-bar')) return;
    topoDragState.dragging = true;
    topoDragState.startX = e.clientX;
    topoDragState.startY = e.clientY;
    topoDragState.panStartX = topoPanX;
    topoDragState.panStartY = topoPanY;
    const svg = container.querySelector('svg');
    if (svg) svg.style.cursor = 'grabbing';
    e.preventDefault();
  });

  // Wheel zoom via delegation
  container.addEventListener('wheel', function (e) {
    if (!e.target.closest('svg')) return;
    if (e.target.closest('.usage-topo-zoom-bar')) return;
    e.preventDefault();
    const delta = e.deltaY > 0 ? 0.9 : 1.1;
    topoZoom = Math.max(0.3, Math.min(3, topoZoom * delta));
    applyTopoTransform();
  }, { passive: false });

  // Register document-level listeners only once
  if (topoListenersRegistered) return;
  topoListenersRegistered = true;
  
  document.addEventListener('mousemove', function (e) {
    if (!topoDragState.dragging) return;
    topoPanX = topoDragState.panStartX + (e.clientX - topoDragState.startX);
    topoPanY = topoDragState.panStartY + (e.clientY - topoDragState.startY);
    if (topoDragRaf) cancelAnimationFrame(topoDragRaf);
    topoDragRaf = requestAnimationFrame(function() {
      applyTopoTransform();
      topoDragRaf = null;
    });
    const svg = document.querySelector('#usageTopology svg');
    if (svg) svg.style.cursor = 'grabbing';
  });
  
  document.addEventListener('mouseup', function () {
    if (topoDragState.dragging) {
      topoDragState.dragging = false;
      const svg = document.querySelector('#usageTopology svg');
      if (svg) svg.style.cursor = '';
      if (topoDragRaf) {
        cancelAnimationFrame(topoDragRaf);
        topoDragRaf = null;
      }
      applyTopoTransform();
    }
  });
}


// ─── Overview Cards ──────────────────────────────────────
function renderOverviewCards() {
  const container = document.getElementById('usageOverviewCards');
  if (!container) return;

  const stats = usageState.stats;
  if (!stats) {
    container.innerHTML = '<div class="usage-loading">Loading...</div>';
    return;
  }

  const totalPromptTokens = stats.totalPromptTokens || 0;
  const totalCompletionTokens = stats.totalCompletionTokens || 0;
  const reportedEffectiveTokens = stats.totalEffectiveTokens || 0;
  // Legacy buckets can lack effective tokens. Treat them as uncached rather
  // than incorrectly claiming all input was served from cache.
  const effectiveTokens = reportedEffectiveTokens || (totalPromptTokens + totalCompletionTokens);
  const cacheHits = Math.max(0, totalPromptTokens + totalCompletionTokens - effectiveTokens);
  const realCost = stats.totalRealCost || 0;

  container.innerHTML =
    '<div class="usage-card overview-card"><div class="overview-card-title">' + (typeof t === 'function' ? t('usage.totalRequests') : 'Total Request') + '</div><div class="overview-card-value">' + fmtNum(stats.totalRequests) + '</div></div>' +
    '<div class="usage-card overview-card"><div class="overview-card-title">' + (typeof t === 'function' ? t('usage.inputTokens') : 'Input Tokens') + '</div><div class="overview-card-value text-primary">' + fmtTokenFull(stats.totalPromptTokens) + '</div></div>' +
    '<div class="usage-card overview-card"><div class="overview-card-title">' + (typeof t === 'function' ? t('usage.outputTokens') : 'Output Tokens') + '</div><div class="overview-card-value text-success">' + fmtTokenFull(stats.totalCompletionTokens) + '</div></div>' +
    '<div class="usage-card overview-card"><div class="overview-card-title">Cached Tokens</div><div class="overview-card-value" style="color:#16a34a">' + fmtTokenFull(cacheHits) + '</div></div>' +
    '<div class="usage-card overview-card"><div class="overview-card-title">Effective Tokens</div><div class="overview-card-value" style="color:#0ea5e9" title="' + (typeof t === 'function' ? t('usage.effectiveTokensHint') : '(Input - Cached) + Output — tokens actually processed') + '">' + fmtTokenFull(effectiveTokens) + '</div></div>' +
    '<div class="usage-card overview-card"><div class="overview-card-title">' + (typeof t === 'function' ? t('usage.realCost') : 'Real Cost') + '</div><div class="overview-card-value text-warning">' + fmtCost(realCost) + '</div><div class="overview-card-sub">' + (typeof t === 'function' ? t('usage.realCostDisclaimer') : 'Computed from official pricing × tokens') + '</div></div>';
}

// ─── Recent Requests Table ───────────────────────────────
function renderRecentRequests() {
  const container = document.getElementById('usageRecentRequests');
  if (!container) return;

  const stats = usageState.stats;
  if (!stats) {
    container.innerHTML = '<div class="usage-loading">Loading...</div>';
    return;
  }

  const requests = stats.recentRequests || [];

  let html = '<div class="usage-recent-header">' + (typeof t === 'function' ? t('usage.recentRequests') : 'Recent Requests') + '</div>';

  if (requests.length === 0) {
    html += '<div class="usage-empty-state">No requests yet.</div>';
  } else {
    html += '<div class="usage-recent-table-wrap"><table class="usage-recent-table">' +
      '<thead><tr><th></th><th>' + (typeof t === 'function' ? t('usage.tabModel') : 'Model') + '</th><th class="text-right">' + (typeof t === 'function' ? t('usage.inOut') : 'In / Out') + '</th><th class="text-right">' + (typeof t === 'function' ? t('usage.when') : 'When') + '</th></tr></thead><tbody>';

    for (const r of requests) {
      const isError = r.status === 'error';
      html += '<tr' + (isError ? ' class="usage-recent-error-row"' : '') + '>' +
        '<td><span class="usage-status-dot ' + (isError ? 'error' : 'success') + '"></span></td>' +
        '<td class="usage-recent-model" title="' + escAttr(r.model) + '">' + escHtml(r.model || '-');
      if (isError && r.error) {
        // An error is only actionable when the operator knows WHICH account
        // produced it. Prefix the account label (nickname/email, falling back
        // to the account ID) so failures can be traced without opening the
        // details drawer.
        const errAccount = getUsageAccountName(r);
        const errTitle = (errAccount !== '-' ? errAccount + ' — ' : '') + r.error;
        html += '<div class="usage-recent-error" title="' + escAttr(errTitle) + '">' +
          (errAccount !== '-' ? '<span class="usage-recent-error-account">' + escHtml(errAccount) + '</span> ' : '') +
          escHtml(r.error) + '</div>';
      }
      html += '</td>' +
        '<td class="text-right whitespace-nowrap"><span class="text-primary">' + fmtNum(r.inputTokens) + '↑</span> <span class="text-success">' + fmtNum(r.outputTokens) + '↓</span></td>' +
        '<td class="text-right text-text-muted">';
      html += '<span class="usage-time-ago" data-ts="' + r.timestamp + '">' + timeAgo(r.timestamp) + '</span>';
      html += '</td></tr>';
    }

    html += '</tbody></table></div>';
  }

  // Only touch the DOM when the table actually changed. Recent Requests is
  // re-rendered on every SSE event; rewriting identical markup made rows
  // flicker and reset the row hover/selection state mid-read.
  setHtmlIfChanged(container, html);
}

// ─── Chart (SVG Area) ────────────────────────────────────
function renderChart() {
  const container = document.getElementById('usageChart');
  if (!container) return;

  const data = usageState.chartData || [];
  const hasData = data.some(d => d.tokens > 0 || d.cost > 0);

  if (!hasData) {
    container.innerHTML =
      '<div class="usage-chart-controls">' +
      '<div class="usage-view-toggle"><button class="usage-toggle-btn active" data-chart-view="tokens">' + (typeof t === 'function' ? t('usage.viewTokens') : 'Tokens') + '</button><button class="usage-toggle-btn" data-chart-view="cost">' + (typeof t === 'function' ? t('usage.viewCost') : 'Cost') + '</button></div>' +
      '</div>' +
      '<div class="usage-empty-state" style="height:200px;display:flex;align-items:center;justify-content:center">' + (typeof t === 'function' ? t('usage.noDataForPeriod') : 'No data for this period') + '</div>';
    bindChartToggle();
    return;
  }

  const viewMode = usageState.chartView;
  const values = data.map(d => viewMode === 'tokens' ? d.tokens : d.cost);
  const maxVal = Math.max(...values, 1);

  const width = Math.min(container.clientWidth || 700, 700);
  const height = 200;
  const padding = { top: 20, right: 10, bottom: 30, left: 50 };
  const chartW = width - padding.left - padding.right;
  const chartH = height - padding.top - padding.bottom;

  const points = data.map((d, i) => ({
    x: padding.left + (i / Math.max(data.length - 1, 1)) * chartW,
    y: padding.top + chartH - ((viewMode === 'tokens' ? d.tokens : d.cost) / maxVal) * chartH,
    label: d.label,
    val: viewMode === 'tokens' ? d.tokens : d.cost,
  }));

  let areaPath = 'M' + points[0].x + ',' + (padding.top + chartH);
  let linePath = '';
  for (let i = 0; i < points.length; i++) {
    const cmd = i === 0 ? 'M' : 'L';
    linePath += cmd + points[i].x + ',' + points[i].y;
    areaPath += cmd + points[i].x + ',' + points[i].y;
  }
  areaPath += 'L' + points[points.length - 1].x + ',' + (padding.top + chartH) + 'Z';

  const strokeColor = viewMode === 'tokens' ? '#6366f1' : '#f59e0b';
  const fillColor = viewMode === 'tokens' ? 'rgba(99,102,241,0.15)' : 'rgba(245,158,11,0.15)';

  let html = '<div class="usage-chart-controls">' +
    '<div class="usage-view-toggle"><button class="usage-toggle-btn' + (viewMode === 'tokens' ? ' active' : '') + '" data-chart-view="tokens">' + (typeof t === 'function' ? t('usage.viewTokens') : 'Tokens') + '</button><button class="usage-toggle-btn' + (viewMode === 'cost' ? ' active' : '') + '" data-chart-view="cost">' + (typeof t === 'function' ? t('usage.viewCost') : 'Cost') + '</button></div>' +
    '</div>';

  html += '<svg width="100%" height="' + height + '" viewBox="0 0 ' + width + ' ' + height + '" class="usage-chart-svg">';

  for (let i = 0; i <= 4; i++) {
    const y = padding.top + (i / 4) * chartH;
    html += '<line x1="' + padding.left + '" y1="' + y + '" x2="' + (padding.left + chartW) + '" y2="' + y + '" stroke="var(--border)" stroke-opacity="0.3" stroke-width="1"/>';
    const val = maxVal * (1 - i / 4);
    html += '<text x="' + (padding.left - 5) + '" y="' + (y + 4) + '" text-anchor="end" fill="var(--muted-foreground)" font-size="10">' + (viewMode === 'tokens' ? fmtTokens(Math.round(val)) : fmtCost(val)) + '</text>';
  }

  html += '<path d="' + areaPath + '" fill="' + fillColor + '"/>';
  html += '<path d="' + linePath + '" fill="none" stroke="' + strokeColor + '" stroke-width="2"/>';

  for (const p of points) {
    if (p.val > 0) {
      html += '<circle cx="' + p.x + '" cy="' + p.y + '" r="3" fill="' + strokeColor + '"/>';
    }
  }

  const labelInterval = Math.max(1, Math.floor(points.length / 6));
  for (let i = 0; i < points.length; i += labelInterval) {
    html += '<text x="' + points[i].x + '" y="' + (height - 5) + '" text-anchor="middle" fill="var(--muted-foreground)" font-size="9">' + escHtml(points[i].label) + '</text>';
  }

  html += '</svg>';

  container.innerHTML = html;
  bindChartToggle();
}

function bindChartToggle() {
  document.querySelectorAll('.usage-toggle-btn[data-chart-view]').forEach(btn => {
    btn.addEventListener('click', function () {
      const view = this.dataset.chartView;
      if (view === usageState.chartView) return;
      usageState.chartView = view;
      renderChart();
    });
  });
}

// ─── Usage Table (Grouped, Expandable) ───────────────────
function renderUsageTable() {
  const container = document.getElementById('usageTable');
  if (!container) return;

  const stats = usageState.stats;
  if (!stats) {
    container.innerHTML = '<div class="usage-loading">Loading...</div>';
    return;
  }

  const tableView = usageState.tableView;
  const viewMode = usageState.tableViewMode;

  let groupMap = {};
  let columns = [];

  switch (tableView) {
    case 'model':
      groupMap = stats.byModel || {};
      columns = [
        { field: 'key', label: (typeof t === 'function' ? t('usage.tabModel') : 'Model') },
        { field: 'requests', label: (typeof t === 'function' ? t('usage.requests') : 'Requests'), align: 'right' },
      ];
      break;
    case 'account':
      groupMap = stats.byAccount || {};
      columns = [
        { field: 'key', label: (typeof t === 'function' ? t('usage.tabAccount') : 'Account') },
        { field: 'requests', label: (typeof t === 'function' ? t('usage.requests') : 'Requests'), align: 'right' },
      ];
      break;
    case 'apiKey':
      groupMap = stats.byApiKey || {};
      columns = [
        { field: 'key', label: (typeof t === 'function' ? t('usage.tabApiKey') : 'API Key') },
        { field: 'requests', label: (typeof t === 'function' ? t('usage.requests') : 'Requests'), align: 'right' },
      ];
      break;
    case 'endpoint':
      groupMap = stats.byEndpoint || {};
      columns = [
        { field: 'key', label: (typeof t === 'function' ? t('usage.tabEndpoint') : 'Endpoint') },
        { field: 'requests', label: (typeof t === 'function' ? t('usage.requests') : 'Requests'), align: 'right' },
      ];
      break;
  }

  const sortField = usageState.sortBy[tableView] || 'requests';
  const sortOrder = usageState.sortOrder[tableView] || 'desc';

  let rows = Object.entries(groupMap).map(([key, val]) => {
    const promptTokens = val.promptTokens || 0;
    const completionTokens = val.completionTokens || 0;
    // Effective tokens are calculated per request by the backend. A missing
    // legacy value is conservatively treated as no cache hit.
    const effectiveTokens = val.effectiveTokens || (promptTokens + completionTokens);
    const cached = Math.max(0, promptTokens + completionTokens - effectiveTokens);
    const realCost = val.realCost || 0;
    // Cost breakdown from backend (computed at ingestion). Falls back to 0
    // for legacy data — UI shows realCost only in that case.
    const inputCost = val.inputCost || 0;
    const cachedCost = val.cachedCost || 0;
    const outputCost = val.outputCost || 0;
    return {
    key,
    requests: val.requests || 0,
    promptTokens,
    completionTokens,
    totalTokens: promptTokens + completionTokens,
    effectiveTokens,
    cost: val.cost || 0,
    realCost,
    inputCost,
    cachedCost,
    outputCost,
    cachedTokens: cached,
  };
  });

  rows.sort((a, b) => {
    let va = a[sortField] || 0;
    let vb = b[sortField] || 0;
    if (typeof va === 'string') va = va.toLowerCase();
    if (typeof vb === 'string') vb = vb.toLowerCase();
    if (va < vb) return sortOrder === 'asc' ? -1 : 1;
    if (va > vb) return sortOrder === 'asc' ? 1 : -1;
    return 0;
  });

  const totalRow = rows.reduce((acc, r) => {
    acc.requests += r.requests;
    acc.promptTokens += r.promptTokens;
    acc.completionTokens += r.completionTokens;
    acc.totalTokens += r.totalTokens;
    acc.effectiveTokens += r.effectiveTokens;
    acc.cost += r.cost;
    acc.realCost += r.realCost;
    acc.inputCost += r.inputCost;
    acc.cachedCost += r.cachedCost;
    acc.outputCost += r.outputCost;
    acc.cachedTokens += r.cachedTokens;
    return acc;
  }, { requests: 0, promptTokens: 0, completionTokens: 0, totalTokens: 0, effectiveTokens: 0, cost: 0, realCost: 0, inputCost: 0, cachedCost: 0, outputCost: 0, cachedTokens: 0 });

  const valueColumns = viewMode === 'tokens'
    ? [
        { field: 'promptTokens', label: (typeof t === 'function' ? t('usage.inputTokensCol') : 'Input Tokens'), align: 'right' },
        { field: 'completionTokens', label: (typeof t === 'function' ? t('usage.outputTokensCol') : 'Output Tokens'), align: 'right' },
        { field: 'totalTokens', label: (typeof t === 'function' ? t('usage.totalTokens') : 'Total Tokens'), align: 'right' },
        { field: 'cachedTokens', label: 'Cached Tokens', align: 'right' },
        { field: 'effectiveTokens', label: 'Effective Tokens', align: 'right' },
      ]
    : [
        { field: 'inputCost', label: 'Input Cost', align: 'right' },
        { field: 'cachedCost', label: 'Cached Cost', align: 'right' },
        { field: 'outputCost', label: 'Output Cost', align: 'right' },
        { field: 'realCost', label: (typeof t === 'function' ? t('usage.realCost') : 'Real Cost'), align: 'right' },
      ];

  const allCols = columns.concat(valueColumns);

  let html =
    '<div class="usage-table-toolbar">' +
    '<select class="usage-table-select" id="usageTableView">' +
    '<option value="model"' + (tableView === 'model' ? ' selected' : '') + '>' + (typeof t === 'function' ? t('usage.usageByModel') : 'Usage by Model') + '</option>' +
    '<option value="account"' + (tableView === 'account' ? ' selected' : '') + '>' + (typeof t === 'function' ? t('usage.usageByAccount') : 'Usage by Account') + '</option>' +
    '<option value="apiKey"' + (tableView === 'apiKey' ? ' selected' : '') + '>' + (typeof t === 'function' ? t('usage.usageByApiKey') : 'Usage by API Key') + '</option>' +
    '<option value="endpoint"' + (tableView === 'endpoint' ? ' selected' : '') + '>' + (typeof t === 'function' ? t('usage.usageByEndpoint') : 'Usage by Endpoint') + '</option>' +
    '</select>' +
    '<div class="usage-view-toggle">' +
    '<button class="usage-toggle-btn' + (viewMode === 'tokens' ? ' active' : '') + '" data-table-view-mode="tokens">' + (typeof t === 'function' ? t('usage.viewTokens') : 'Tokens') + '</button>' +
    '<button class="usage-toggle-btn' + (viewMode === 'costs' ? ' active' : '') + '" data-table-view-mode="costs">' + (typeof t === 'function' ? t('usage.costs') : 'Costs') + '</button>' +
    '</div>' +
    '</div>' +
    '<div class="usage-table-wrap"><table class="usage-data-table"><thead><tr>';

  for (const col of allCols) {
    const cls = 'sortable' + (col.align === 'right' ? ' text-right' : '');
    html += '<th class="' + cls + '" data-sort="' + col.field + '">' + escHtml(col.label) + ' <span class="sort-icon">' + getSortIcon(col.field) + '</span></th>';
  }

  html += '</tr></thead><tbody>';

  for (const row of rows) {
    html += '<tr>';
    html += '<td class="usage-row-key" title="' + escAttr(row.key) + '">' + escHtml(row.key) + '</td>';
    html += '<td class="text-right">' + fmtNum(row.requests) + '</td>';

    if (viewMode === 'tokens') {
      html +=
        '<td class="text-right text-text-muted">' + fmtTokenFull(row.promptTokens) + '</td>' +
        '<td class="text-right text-text-muted">' + fmtTokenFull(row.completionTokens) + '</td>' +
        '<td class="text-right">' + fmtTokenFull(row.totalTokens) + '</td>' +
        '<td class="text-right" style="color:#16a34a">' + (row.cachedTokens ? fmtTokenFull(row.cachedTokens) : '—') + '</td>' +
        '<td class="text-right" style="color:#0ea5e9" title="' + escAttr((typeof t === 'function' ? t('usage.effectiveTokensHint') : '(Input - Cached) + Output')) + '">' + fmtTokenFull(row.effectiveTokens) + '</td>';
    } else {
      html +=
        '<td class="text-right text-text-muted">' + fmtCost(row.inputCost) + '</td>' +
        '<td class="text-right" style="color:#16a34a">' + fmtCost(row.cachedCost) + '</td>' +
        '<td class="text-right text-text-muted">' + fmtCost(row.outputCost) + '</td>' +
        '<td class="text-right text-warning"><strong>' + fmtCost(row.realCost) + '</strong></td>';
    }

    html += '</tr>';
  }

  // Total row
  html += '<tr class="usage-summary-row">';
  html += '<td><strong>' + (typeof t === 'function' ? t('usage.total') : 'Total') + '</strong></td>';
  html += '<td class="text-right"><strong>' + fmtNum(totalRow.requests) + '</strong></td>';
  if (viewMode === 'tokens') {
    html +=
      '<td class="text-right text-text-muted"><strong>' + fmtTokenFull(totalRow.promptTokens) + '</strong></td>' +
      '<td class="text-right text-text-muted"><strong>' + fmtTokenFull(totalRow.completionTokens) + '</strong></td>' +
      '<td class="text-right"><strong>' + fmtTokenFull(totalRow.totalTokens) + '</strong></td>' +
      '<td class="text-right" style="color:#16a34a"><strong>' + (totalRow.cachedTokens ? fmtTokenFull(totalRow.cachedTokens) : '—') + '</strong></td>' +
      '<td class="text-right" style="color:#0ea5e9"><strong>' + fmtTokenFull(totalRow.effectiveTokens) + '</strong></td>';
  } else {
    html +=
      '<td class="text-right text-text-muted"><strong>' + fmtCost(totalRow.inputCost) + '</strong></td>' +
      '<td class="text-right" style="color:#16a34a"><strong>' + fmtCost(totalRow.cachedCost) + '</strong></td>' +
      '<td class="text-right text-text-muted"><strong>' + fmtCost(totalRow.outputCost) + '</strong></td>' +
      '<td class="text-right text-warning"><strong>' + fmtCost(totalRow.realCost) + '</strong></td>';
  }
  html += '</tr>';

  html += '</tbody></table></div>';

  container.innerHTML = html;
  bindTableEvents();
}

function getSortIcon(field) {
  const tableView = usageState.tableView;
  if (usageState.sortBy[tableView] === field) {
    return usageState.sortOrder[tableView] === 'asc' ? '↑' : '↓';
  }
  return '⇅';
}

function bindTableEvents() {
  const sel = document.getElementById('usageTableView');
  if (sel) {
    sel.addEventListener('change', function () {
      usageState.tableView = this.value;
      renderUsageTable();
    });
  }

  document.querySelectorAll('.usage-toggle-btn[data-table-view-mode]').forEach(btn => {
    btn.addEventListener('click', function () {
      usageState.tableViewMode = this.dataset.tableViewMode;
      renderUsageTable();
    });
  });

  document.querySelectorAll('.usage-data-table th.sortable').forEach(th => {
    th.addEventListener('click', function () {
      const field = this.dataset.sort;
      const tableView = usageState.tableView;
      if (usageState.sortBy[tableView] === field) {
        usageState.sortOrder[tableView] = usageState.sortOrder[tableView] === 'asc' ? 'desc' : 'asc';
      } else {
        usageState.sortBy[tableView] = field;
        usageState.sortOrder[tableView] = 'desc';
      }
      renderUsageTable();
    });
  });
}

// ─── Period Selector ─────────────────────────────────────
function renderPeriodSelector() {
  const container = document.getElementById('usagePeriodSelector');
  if (!container) return;

  const periods = [
    { value: 'today', label: (typeof t === 'function' ? t('usage.period.today') : 'Today') },
    { value: '24h', label: '24h' },
    { value: '7d', label: '7D' },
    { value: '30d', label: '30D' },
    { value: '60d', label: '60D' },
  ];

  container.innerHTML = '<div class="usage-period-group">' +
    periods.map(p =>
      '<button class="usage-period-btn' + (usageState.period === p.value ? ' active' : '') + '" data-period="' + p.value + '">' + p.label + '</button>'
    ).join('') +
    '</div>';

  container.querySelectorAll('.usage-period-btn').forEach(btn => {
    btn.addEventListener('click', function () {
      const period = this.dataset.period;
      if (period === usageState.period) return;
      usageState.period = period;
      container.querySelectorAll('.usage-period-btn').forEach(b => b.classList.remove('active'));
      this.classList.add('active');
      // A different period is a different query, but the response can still be
      // byte-identical to the previous one (e.g. an idle proxy where 24h and 7d
      // both report zeros). Drop the signature so the switch always repaints.
      invalidateStatsSignature();
      fetchUsageStats(period);
      fetchUsageChart(period);
    });
  });
}

// ─── Tabs ────────────────────────────────────────────────
function renderUsageTabs() {
  const container = document.getElementById('usageTabs');
  if (!container) return;
  container.innerHTML =
    '<div class="usage-tabs-bar">' +
    '<button class="usage-tab-btn' + (usageState.activeTab === 'overview' ? ' active' : '') + '" data-tab="overview">' + (typeof t === 'function' ? t('usage.overview') : 'Overview') + '</button>' +
    '<button class="usage-tab-btn' + (usageState.activeTab === 'details' ? ' active' : '') + '" data-tab="details">' + (typeof t === 'function' ? t('usage.details') : 'Details') + '</button>' +
    '<button class="usage-tab-btn' + (usageState.activeTab === 'cache' ? ' active' : '') + '" data-tab="cache">Cache</button>' +
    '<button class="usage-tab-btn' + (usageState.activeTab === 'compression' ? ' active' : '') + '" data-tab="compression">Compression</button>' +
    '<button class="usage-tab-btn' + (usageState.activeTab === 'pool' ? ' active' : '') + '" data-tab="pool">Pool</button>' +
    '</div>';

  container.querySelectorAll('.usage-tab-btn').forEach(btn => {
    btn.addEventListener('click', function () {
      const tab = this.dataset.tab;
      if (tab === usageState.activeTab) return;
      usageState.activeTab = tab;
      container.querySelectorAll('.usage-tab-btn').forEach(b => b.classList.remove('active'));
      this.classList.add('active');
      // Same payload, different view: the next fetch must repaint even though
      // the bytes have not changed.
      invalidateStatsSignature();
      renderActiveTab();
    });
  });
}

function renderActiveTab() {
  const overviewEl = document.getElementById('usageOverviewContent');
  const detailsEl = document.getElementById('usageDetailsContent');
  const cacheEl = document.getElementById('usageCacheContent');
  const compEl = document.getElementById('usageCompressionContent');
  const poolEl = document.getElementById('usagePoolContent');
  if (!overviewEl || !detailsEl) return;

  // Hide all
  overviewEl.classList.add('hidden');
  detailsEl.classList.add('hidden');
  if (cacheEl) cacheEl.classList.add('hidden');
  if (compEl) compEl.classList.add('hidden');
  if (poolEl) poolEl.classList.add('hidden');

  if (usageState.activeTab === 'overview') {
    overviewEl.classList.remove('hidden');
    renderPeriodSelector();
    fetchUsageStats(usageState.period);
    fetchUsageChart(usageState.period);
    connectUsageSSE();
  } else if (usageState.activeTab === 'cache') {
    disconnectUsageSSE();
    if (cacheEl) cacheEl.classList.remove('hidden');
    fetchCacheStats();
  } else if (usageState.activeTab === 'compression') {
    disconnectUsageSSE();
    if (compEl) compEl.classList.remove('hidden');
    fetchCompressionStats();
  } else if (usageState.activeTab === 'pool') {
    disconnectUsageSSE();
    if (poolEl) poolEl.classList.remove('hidden');
    fetchPoolStrategy();
  } else {
    detailsEl.classList.remove('hidden');
    // Details uses the same one-way Usage SSE as Overview. Completed request
    // events are applied as deltas; the paginated request is only the initial
    // snapshot or an explicit filter/page refresh.
    connectUsageSSE();
    fetchRequestDetails();
  }
}

// ─── Compression Tab ─────────────────────────────────────
let compressionState = { data: null };

async function fetchCompressionStats() {
  const container = document.getElementById('usageCompressionContent');
  if (!container) return;
  container.innerHTML = '<div class="usage-loading">Loading compression stats...</div>';
  try {
    const res = await api('/compression/stats');
    if (!res.ok) throw new Error('HTTP ' + res.status);
    compressionState.data = await res.json();
    renderCompressionTab();
  } catch (e) {
    container.innerHTML = '<div class="usage-error">Failed to load: ' + escapeHtml(String(e)) + '</div>';
  }
}

function renderCompressionTab() {
  const container = document.getElementById('usageCompressionContent');
  if (!container || !compressionState.data) return;
  const d = compressionState.data;

  const toggle = (key, label, desc) => `
    <div class="comp-toggle-row">
      <div class="comp-toggle-info">
        <div class="comp-toggle-label">${label}</div>
        <div class="comp-toggle-desc">${desc}</div>
      </div>
      <label class="comp-switch">
        <input type="checkbox" data-key="${key}" ${d[key] ? 'checked' : ''}/>
        <span class="comp-slider"></span>
      </label>
    </div>`;

  container.innerHTML = `
    <div class="comp-page">
      <div class="comp-section">
        <div class="comp-section-title">Compression Features</div>
        ${toggle('toolOutputEnabled', 'Tool Output Compression (RTK)', 'Compresses git diff, grep, ls, and repetitive tool output to reduce input tokens.')}
        ${toggle('cavemanEnabled', 'Caveman (Terse Output)', 'Injects a system prompt suffix biasing the model toward ultra-terse responses (65-87% fewer output tokens).')}
        ${toggle('ponytailEnabled', 'Ponytail (Minimal Code)', 'Injects a system prompt suffix enforcing YAGNI: smallest correct change, reuse stdlib, no over-engineering.')}
        ${toggle('headroomEnabled', 'Headroom (Prompt Compression)', 'Routes requests through Headroom (localhost:8787) for ML-based prompt compression before forwarding upstream.')}
      </div>

      <div class="comp-section" id="compCavemanLevelRow" style="${d.cavemanEnabled ? '' : 'display:none'}">
        <div class="comp-section-title">Caveman Level</div>
        <div class="comp-level-row">
          <button class="comp-level-btn${d.cavemanLevel === 'light' ? ' active' : ''}" data-level="light">Light (1-2 sentence answers)</button>
          <button class="comp-level-btn${d.cavemanLevel === 'full' ? ' active' : ''}" data-level="full">Full (ultra-terse, symbols over words)</button>
        </div>
      </div>

      <div class="comp-section" id="compHeadroomURLRow" style="${d.headroomEnabled ? '' : 'display:none'}">
        <div class="comp-section-title">Headroom URL</div>
        <input type="text" id="compHeadroomURL" class="comp-input" value="${escapeHtml(d.headroomURL || '')}" placeholder="http://localhost:8787"/>
        <button class="btn-primary" id="compSaveHeadroomURL" style="margin-top:8px">Save URL</button>
      </div>

      <div class="comp-section">
        <div class="comp-section-title">Statistics</div>
        <div id="compStatsSection">${renderCompressionStatsHtml(d)}</div>
      </div>
    </div>`;

  // Wire toggles
  container.querySelectorAll('.comp-switch input[type="checkbox"]').forEach(cb => {
    cb.addEventListener('change', async function () {
      const key = this.dataset.key;
      const val = this.checked;
      const body = {};
      body[key] = val;
      try {
        await api('/compression/config', { method: 'PATCH', body: JSON.stringify(body) });
        showToast(key + ' ' + (val ? 'enabled' : 'disabled'));
        fetchCompressionStats();
      } catch (e) {
        showToast('Failed: ' + e);
        this.checked = !val;
      }
    });
  });

  // Wire caveman level buttons
  container.querySelectorAll('.comp-level-btn').forEach(btn => {
    btn.addEventListener('click', async function () {
      const level = this.dataset.level;
      try {
        await api('/compression/config', { method: 'PATCH', body: JSON.stringify({ cavemanLevel: level }) });
        showToast('Caveman level: ' + level);
        fetchCompressionStats();
      } catch (e) { showToast('Failed: ' + e); }
    });
  });

  // Wire headroom URL save
  const saveBtn = document.getElementById('compSaveHeadroomURL');
  if (saveBtn) {
    saveBtn.addEventListener('click', async function () {
      const url = document.getElementById('compHeadroomURL').value.trim();
      try {
        await api('/compression/config', { method: 'PATCH', body: JSON.stringify({ headroomURL: url }) });
        showToast('Headroom URL saved');
      } catch (e) { showToast('Failed: ' + e); }
    });
  }
}

// ─── Cache Tab ───────────────────────────────────────────
async function fetchCacheStats() {
  const container = document.getElementById('usageCacheContent');
  if (!container) return;
  container.innerHTML = '<div class="usage-loading">Loading cache stats...</div>';
  try {
    // /cache/stats carries token counts only. The paid/unpriced split needs
    // realCost per model, which lives in /usage/stats — both accept the same
    // period vocabulary, so they are fetched together and joined by model name.
    // The usage fetch is allowed to fail without failing the tab.
    const period = encodeURIComponent(cacheState.period);
    const [res, usageRes] = await Promise.all([
      api('/cache/stats?period=' + period),
      api('/usage/stats?period=' + period).catch(() => null),
    ]);
    if (!res.ok) throw new Error('HTTP ' + res.status);
    cacheState.data = await res.json();
    cacheState.usage = usageRes && usageRes.ok ? await usageRes.json().catch(() => null) : null;
    renderCacheTab();
  } catch (e) {
    container.innerHTML = '<div class="usage-error">Failed to load cache stats: ' + escapeHtml(String(e)) + '</div>';
  }
}

function renderCacheTab() {
  const container = document.getElementById('usageCacheContent');
  if (!container || !cacheState.data) return;
  const c = cacheState.data;

  const hitRatio = c.hitRatio || 0;
  const savingsPct = c.savingsPct || 0;
  const tokensSaved = c.tokensSaved || 0;
  const totalInput = c.totalInput || 0;
  const cacheRead = c.cacheRead || 0;
  const cacheCreate = c.cacheCreate || 0;
  const cachedTokens = c.cachedTokens || 0;
  const cacheHits = Number.isFinite(c.cacheHits) ? c.cacheHits : Math.max(cacheRead, cachedTokens);
  const uncached = c.uncached || 0;

  // Paid vs unpriced split.
  //
  // The headline ratio pools every model into one denominator, so traffic that
  // costs nothing dilutes it: a model carrying most of the input tokens but no
  // billed cost drags the number down even when the models that do cost money
  // are caching well. That single figure is what an operator reads to decide
  // whether caching needs work, so it is the wrong number to lead with.
  //
  // "Paid" here means realCost > 0 for that model over the same period, taken
  // from /usage/stats. Note what that does NOT mean: a model can report zero
  // cost simply because it has no entry in the server pricing table, in which
  // case its spend is unknown rather than zero. The second group is therefore
  // labelled "unpriced", not "free" — see LookupPricing in proxy/pricing.go.
  const usageByModel = (cacheState.usage && cacheState.usage.byModel) || null;
  let paidSplit = null;
  if (usageByModel) {
    const acc = {
      paidInput: 0, paidHits: 0, paidCost: 0,
      unpricedInput: 0, unpricedHits: 0,
      paidModels: [], unpricedModels: [],
    };
    Object.entries(c.byModel || {}).forEach(([model, s]) => {
      const input = s.promptTokens || 0;
      if (input <= 0) return;
      const hits = Number.isFinite(s.cacheHits) ? s.cacheHits : Math.max(s.cacheRead || 0, s.cachedTokens || 0);
      const cost = (usageByModel[model] && usageByModel[model].realCost) || 0;
      if (cost > 0) {
        acc.paidInput += input;
        acc.paidHits += hits;
        acc.paidCost += cost;
        acc.paidModels.push(model);
      } else {
        acc.unpricedInput += input;
        acc.unpricedHits += hits;
        acc.unpricedModels.push(model);
      }
    });
    // Only meaningful when both sides carry traffic; otherwise the split says
    // nothing the headline ratio does not already say.
    if (acc.paidInput > 0) paidSplit = acc;
  }

  // Period selector
  const periods = ['24h', '7d', '30d', 'all'];
  const periodBar = '<div class="usage-period-bar" style="margin-bottom:16px">' +
    periods.map(p => `<button class="usage-period-btn${cacheState.period === p ? ' active' : ''}" data-period="${p}">${p}</button>`).join('') +
    '</div>';

  // Summary cards. The paid-only ratio leads, because it is the one an
  // operator can act on; the pooled ratio stays for continuity but is no
  // longer the first number read.
  const cards = [];
  if (paidSplit) {
    const paidRatio = paidSplit.paidInput > 0 ? (paidSplit.paidHits / paidSplit.paidInput * 100) : 0;
    const unpricedRatio = paidSplit.unpricedInput > 0 ? (paidSplit.unpricedHits / paidSplit.unpricedInput * 100) : 0;
    cards.push({
      label: 'Hit Ratio (paid only)',
      value: fmtPct(paidRatio),
      color: '#22c55e',
      title: paidSplit.paidModels.join(', ') + ' — ' + fmtTokens(paidSplit.paidInput) +
        ' input, ' + fmtCost(paidSplit.paidCost),
    });
    if (paidSplit.unpricedInput > 0) {
      cards.push({
        label: 'Hit Ratio (unpriced)',
        value: fmtPct(unpricedRatio),
        color: '#6b7280',
        title: 'No pricing entry, so billed cost is unknown rather than zero: ' +
          paidSplit.unpricedModels.join(', ') + ' — ' + fmtTokens(paidSplit.unpricedInput) + ' input',
      });
    }
  }
  cards.push(
    { label: 'Cache Hit Ratio (all)', value: fmtPct(hitRatio), color: '#22c55e' },
    { label: 'Token Savings', value: fmtPct(savingsPct), color: '#3b82f6' },
    { label: 'Tokens Saved', value: fmtTokens(tokensSaved), color: '#22c55e' },
    { label: 'Total Input', value: fmtTokens(totalInput), color: '' },
    { label: 'Cache Read (Claude)', value: fmtTokens(cacheRead), color: '#22c55e' },
    { label: 'Cache Create (Claude)', value: fmtTokens(cacheCreate), color: '#f59e0b' },
    { label: 'Cached (OpenAI)', value: fmtTokens(cachedTokens), color: '#22c55e' },
    { label: 'Uncached', value: fmtTokens(uncached), color: '#6b7280' }
  );

  const cardsHtml = '<div class="usage-overview-grid">' + cards.map(card => `
    <div class="usage-overview-card"${card.title ? ' title="' + escapeHtml(card.title) + '"' : ''}>
      <div class="usage-overview-card-value" style="${card.color ? 'color:' + card.color : ''}">${card.value}</div>
      <div class="usage-overview-card-label">${card.label}</div>
    </div>`).join('') + '</div>';

  const total = cacheHits + cacheCreate + uncached;
  const barHtml = total > 0 ? `
    <div class="usage-card" style="margin-top:16px;padding:16px">
      <div class="usage-card-title" style="margin-bottom:12px">Cache Composition</div>
      <div class="cache-composition-bar">
        <div class="cache-seg cache-seg-read" style="width:${(cacheHits/total*100).toFixed(1)}%" title="Cache Hits: ${fmtTokens(cacheHits)}"></div>
        <div class="cache-seg cache-seg-create" style="width:${(cacheCreate/total*100).toFixed(1)}%" title="Cache Create: ${fmtTokens(cacheCreate)}"></div>
        <div class="cache-seg cache-seg-cached" style="width:0%" title="Cached (OpenAI): merged into Cache Hits"></div>
        <div class="cache-seg cache-seg-uncached" style="width:${(uncached/total*100).toFixed(1)}%" title="Uncached: ${fmtTokens(uncached)}"></div>
      </div>
      <div class="cache-legend">
        <span class="cache-legend-item"><span class="cache-dot cache-dot-read"></span> Cache Hits (${fmtPct(cacheHits/total*100)})</span>
        <span class="cache-legend-item"><span class="cache-dot cache-dot-create"></span> Cache Create (${fmtPct(cacheCreate/total*100)})</span>
        <span class="cache-legend-item"><span class="cache-dot cache-dot-uncached"></span> Uncached (${fmtPct(uncached/total*100)})</span>
      </div>
    </div>` : '';

  // Per-account cache table
  const accountRows = Object.entries(c.byAccount || {})
    .filter(([_, s]) => s.cacheRead > 0 || s.cacheCreate > 0 || s.cachedTokens > 0)
    .map(([id, s]) => {
      const name = (c.accountNames && c.accountNames[id]) || id.slice(0, 8);
      const acctSaved = Number.isFinite(s.cacheHits) ? s.cacheHits : Math.max(s.cacheRead || 0, s.cachedTokens || 0);
      const acctTotal = s.promptTokens || 0;
      const acctRatio = acctTotal > 0 ? (acctSaved / acctTotal * 100) : 0;
      return `<tr>
        <td>${escapeHtml(name)}</td>
        <td>${fmtTokens(s.promptTokens)}</td>
        <td style="color:#22c55e">${fmtTokens(s.cacheRead)}</td>
        <td style="color:#f59e0b">${fmtTokens(s.cacheCreate)}</td>
        <td style="color:#22c55e">${fmtTokens(s.cachedTokens)}</td>
        <td style="color:#22c55e">${fmtPct(acctRatio)}</td>
        <td>${s.requests}</td>
      </tr>`;
    }).join('');

  const accountTable = accountRows ? `
    <div class="usage-card" style="margin-top:16px">
      <div class="usage-card-title" style="padding:16px 16px 0">Cache by Account</div>
      <table class="usage-table" style="width:100%">
        <thead><tr>
          <th>Account</th><th>Input</th><th>Cache Read</th><th>Cache Create</th><th>Cached (OpenAI)</th><th>Save %</th><th>Requests</th>
        </tr></thead>
        <tbody>${accountRows}</tbody>
      </table>
    </div>` : '<div class="usage-card" style="margin-top:16px;padding:16px;color:var(--muted-foreground)">No cache activity yet. Cache hits appear here after Claude/OpenAI requests with prompt caching.</div>';

  // Per-model cache table
  const modelRows = Object.entries(c.byModel || {})
    .filter(([_, s]) => s.cacheRead > 0 || s.cacheCreate > 0 || s.cachedTokens > 0)
    .map(([model, s]) => {
      const mSaved = Number.isFinite(s.cacheHits) ? s.cacheHits : Math.max(s.cacheRead || 0, s.cachedTokens || 0);
      const mTotal = s.promptTokens || 0;
      const mRatio = mTotal > 0 ? (mSaved / mTotal * 100) : 0;
      return `<tr>
        <td>${escapeHtml(model)}</td>
        <td>${fmtTokens(s.promptTokens)}</td>
        <td style="color:#22c55e">${fmtTokens(s.cacheRead)}</td>
        <td style="color:#f59e0b">${fmtTokens(s.cacheCreate)}</td>
        <td style="color:#22c55e">${fmtTokens(s.cachedTokens)}</td>
        <td style="color:#22c55e">${fmtPct(mRatio)}</td>
        <td>${s.requests}</td>
      </tr>`;
    }).join('');

  const modelTable = modelRows ? `
    <div class="usage-card" style="margin-top:16px">
      <div class="usage-card-title" style="padding:16px 16px 0">Cache by Model</div>
      <table class="usage-table" style="width:100%">
        <thead><tr>
          <th>Model</th><th>Input</th><th>Cache Read</th><th>Cache Create</th><th>Cached (OpenAI)</th><th>Save %</th><th>Requests</th>
        </tr></thead>
        <tbody>${modelRows}</tbody>
      </table>
    </div>` : '';

  container.innerHTML = periodBar + cardsHtml + barHtml + accountTable + modelTable;

  // Wire period buttons
  container.querySelectorAll('.usage-period-btn').forEach(btn => {
    btn.addEventListener('click', function () {
      cacheState.period = this.dataset.period;
      fetchCacheStats();
    });
  });
}

function fmtPct(n) { return (n || 0).toFixed(1) + '%'; }

// ─── Request Details Tab ─────────────────────────────────
async function fetchRequestDetails() {
  const container = document.getElementById('usageDetailsContent');
  if (!container) return;

  usageState.detailsLoading = true;
  renderRequestDetailsTable();

  try {
    const params = new URLSearchParams({
      page: usageState.detailsPagination.page.toString(),
      pageSize: usageState.detailsPagination.pageSize.toString()
    });
    if (usageState.detailsFilters.provider) params.append('provider', usageState.detailsFilters.provider);
    if (usageState.detailsFilters.startDate) params.append('startDate', usageState.detailsFilters.startDate);
    if (usageState.detailsFilters.endDate) params.append('endDate', usageState.detailsFilters.endDate);

    const res = await api('/usage/request-details?' + params.toString());
    if (res.ok) {
      const data = await res.json();
      usageState.detailsData = data.details || [];
      // Mark snapshot rows before flushing events that arrived while this
      // request was in flight. This prevents the same record from appearing
      // twice when it is present in both the snapshot and the SSE queue.
      usageState.detailsData.forEach(rememberDetailsKey);
      usageState.detailsPagination = { ...usageState.detailsPagination, ...data.pagination };
    }

    // Fetch providers list for filter
    const provRes = await api('/usage/providers');
    if (provRes.ok) {
      const provData = await provRes.json();
      usageState.detailsProviders = provData.providers || [];
    }
  } catch (e) {
    console.error('[Usage] fetchDetails error:', e);
  } finally {
    usageState.detailsLoading = false;
    mergePendingDetailsEvents();
    renderRequestDetailsTable();
  }
}

function getProviderDisplayName(providerId) {
  if (!providerId) return providerId;
  const providers = usageState.detailsProviders || [];
  for (const p of providers) {
    if (p.id === providerId) return p.name;
  }
  return providerId;
}

function getUsageAccountName(record) {
  if (!record) return '-';
  const nameMap = (usageState.stats || {}).accountNames || {};
  return record.accountName || nameMap[record.accountId] || record.accountId || '-';
}

// Keep the deduplication window bounded so a long-lived dashboard does not
// retain one key per request forever. The server event id is process-local;
// the record key also protects against a request appearing in the initial page
// snapshot and then arriving through SSE while that snapshot is loading.
const DETAILS_SEEN_KEY_LIMIT = 1000;
function detailsRecordKey(record) {
  if (!record) return '';
  const tokens = record.tokens || {};
  return [
    record.timestamp || '', record.model || '', record.provider || '',
    record.accountId || '', record.status || '', record.error || '',
    tokens.prompt_tokens || tokens.input_tokens || record.inputTokens || 0,
    tokens.completion_tokens || record.outputTokens || 0,
  ].join('|');
}

function rememberDetailsKey(key) {
  if (!key || usageState.detailsSeenKeys.has(key)) return;
  usageState.detailsSeenKeys.add(key);
  usageState.detailsSeenOrder.push(key);
  while (usageState.detailsSeenOrder.length > DETAILS_SEEN_KEY_LIMIT) {
    const old = usageState.detailsSeenOrder.shift();
    usageState.detailsSeenKeys.delete(old);
  }
}

function detailMatchesCurrentFilters(record) {
  const filters = usageState.detailsFilters;
  if (filters.provider && record.provider !== filters.provider) return false;
  if (filters.startDate && (!record.timestamp || record.timestamp < filters.startDate)) return false;
  if (filters.endDate && (!record.timestamp || record.timestamp > filters.endDate + 'T23:59:59Z')) return false;
  return true;
}

function normalizeLiveDetail(record) {
  if (!record) return null;
  return {
    timestamp: record.timestamp || new Date().toISOString(),
    model: record.model || '',
    provider: record.provider || '',
    accountId: record.accountId || '',
    accountName: record.accountName || '',
    status: record.status || 'success',
    error: record.error || '',
    tokens: {
      prompt_tokens: Number(record.inputTokens || 0),
      completion_tokens: Number(record.outputTokens || 0),
    },
    latency: record.latency || {},
  };
}

function applyDetailsRequestCompleted(record, eventId) {
  const detail = normalizeLiveDetail(record);
  if (!detail) return;

  const numericEventId = Number(eventId || 0);
  const recordKey = detailsRecordKey(detail);
  const eventKey = numericEventId > 0 ? 'event:' + numericEventId : '';
  if ((eventKey && usageState.detailsSeenKeys.has(eventKey)) ||
      (recordKey && usageState.detailsSeenKeys.has(recordKey))) return;
  if (eventKey) rememberDetailsKey(eventKey);
  rememberDetailsKey(recordKey);
  if (numericEventId > usageState.detailsLastEventId) {
    usageState.detailsLastEventId = numericEventId;
  }

  if (usageState.detailsLoading) {
    if (usageState.detailsPendingEvents.length < 100) {
      usageState.detailsPendingEvents.push({ record: detail, eventId: numericEventId });
    }
    return;
  }
  if (usageState.activeTab !== 'details' || document.hidden || !detailMatchesCurrentFilters(detail)) return;

  const page = usageState.detailsPagination;
  page.totalItems = Number(page.totalItems || 0) + 1;
  page.totalPages = Math.max(1, Math.ceil(page.totalItems / page.pageSize));
  if (page.page !== 1) return;

  usageState.detailsData = [detail, ...usageState.detailsData].slice(0, page.pageSize);
  renderRequestDetailsTable();
}

function mergePendingDetailsEvents() {
  const pending = usageState.detailsPendingEvents.splice(0);
  if (pending.length === 0) return;

  const page = usageState.detailsPagination;
  let added = 0;
  for (const item of pending) {
    const detail = item && item.record;
    if (!detail || !detailMatchesCurrentFilters(detail)) continue;

    // The initial snapshot may already contain this request. In that case the
    // event is acknowledged but must not increase the count or duplicate a row.
    const key = detailsRecordKey(detail);
    const alreadyLoaded = usageState.detailsData.some(row => detailsRecordKey(row) === key);
    if (alreadyLoaded) continue;

    page.totalItems = Number(page.totalItems || 0) + 1;
    added++;
    if (page.page === 1) {
      usageState.detailsData.unshift(detail);
    }
  }

  if (added > 0) {
    page.totalPages = Math.max(1, Math.ceil(page.totalItems / page.pageSize));
    if (page.page === 1) {
      usageState.detailsData = usageState.detailsData.slice(0, page.pageSize);
    }
  }
}

function collapsibleSection(title, content, defaultOpen, icon) {
  const isOpen = defaultOpen !== false;
  return '<div class="usage-drawer-collapsible">' +
    '<button type="button" class="usage-drawer-collapsible-header" onclick="' +
    "var c=this.nextElementSibling,p=this.querySelector('.chevron');" +
    "if(c){var o=c.style.display!=='none';c.style.display=o?'none':'block';" +
    "if(p)p.style.transform=o?'rotate(0deg)':'rotate(90deg)';}" +
    '">' +
    '<span class="usage-drawer-collapsible-title">' + escHtml(title) + '</span>' +
    '<span class="usage-drawer-collapsible-chevron chevron" style="transform:' + (isOpen ? 'rotate(90deg)' : 'rotate(0deg)') + '">\u25b6</span>' +
    '</button>' +
    '<div class="usage-drawer-collapsible-body" style="display:' + (isOpen ? 'block' : 'none') + '">' + content + '</div>' +
    '</div>';
}

function renderRequestDetailsTable() {
  const container = document.getElementById('usageDetailsContent');
  if (!container) return;

  const { detailsData, detailsPagination, detailsLoading, detailsFilters, detailsProviders } = usageState;

  let html = '';

  // Filters bar
  html += '<div class="usage-details-filters">' +
    '<select class="usage-details-filter-select" id="detailsProviderFilter">' +
    '<option value="">' + (typeof t === 'function' ? t('usage.allProviders') : 'All Providers') + '</option>';
  for (const p of detailsProviders) {
    html += '<option value="' + escAttr(p.id) + '"' + (detailsFilters.provider === p.id ? ' selected' : '') + '>' + escHtml(p.name) + '</option>';
  }
  html += '</select>' +
    '<input type="date" class="usage-details-filter-input" id="detailsStartDate" value="' + escAttr(detailsFilters.startDate) + '" placeholder="' + (typeof t === 'function' ? t('usage.placeholder.startDate') : 'Start date') + '">' +
    '<input type="date" class="usage-details-filter-input" id="detailsEndDate" value="' + escAttr(detailsFilters.endDate) + '" placeholder="' + (typeof t === 'function' ? t('usage.placeholder.endDate') : 'End date') + '">' +
    '<button class="usage-details-filter-btn" id="detailsFilterApply">' + (typeof t === 'function' ? t('usage.filter') : 'Filter') + '</button>' +
    '</div>';

  // Table
  if (detailsLoading) {
    html += '<div class="usage-loading">Loading...</div>';
  } else if (detailsData.length === 0) {
    html += '<div class="usage-empty-state">' + (typeof t === 'function' ? t('usage.noDetailsFound') : 'No request details found.') + '</div>';
  } else {
    html += '<div class="usage-details-table-wrap"><table class="usage-details-table">' +
      '<thead><tr>' +
      '<th>' + (typeof t === 'function' ? t('usage.tabModel') : 'Model') + '</th><th>' + (typeof t === 'function' ? t('usage.tabAccount') : 'Account') + '</th><th>' + (typeof t === 'function' ? t('usage.status') : 'Status') + '</th><th class="text-right">' + (typeof t === 'function' ? t('usage.input') : 'Input') + '</th><th class="text-right">' + (typeof t === 'function' ? t('usage.output') : 'Output') + '</th><th class="text-right">' + (typeof t === 'function' ? t('usage.when') : 'When') + '</th><th></th>' +
      '</tr></thead><tbody>';

    for (const d of detailsData) {
      const ok = d.status === 'success' || d.status === 'ok';
      const inputTokens = d.tokens?.prompt_tokens || d.tokens?.input_tokens || 0;
      const outputTokens = d.tokens?.completion_tokens || 0;

      html += '<tr class="usage-details-row" data-detail-idx="' + detailsData.indexOf(d) + '">' +
        '<td class="usage-details-model" title="' + escAttr(d.model) + '">' + escHtml(d.model || '-') + '</td>' +
        '<td class="usage-details-account" title="' + escAttr(getUsageAccountName(d)) + '">' + escHtml(getUsageAccountName(d)) + '</td>' +
        '<td><span class="usage-status-dot ' + (ok ? 'success' : 'error') + '"></span> ' + escHtml(translateStatus(d.status)) + '</td>' +
        '<td class="text-right">' + fmtNum(inputTokens) + '</td>' +
        '<td class="text-right">' + fmtNum(outputTokens) + '</td>' +
        '<td class="text-right text-text-muted whitespace-nowrap"><span class="usage-time-ago" data-ts="' + d.timestamp + '">' + timeAgo(d.timestamp) + '</span></td>' +
        '<td class="text-right"><button class="usage-details-view-btn" data-detail-idx="' + detailsData.indexOf(d) + '">' + (typeof t === 'function' ? t('usage.view') : 'View') + '</button></td>' +
        '</tr>';
    }

    html += '</tbody></table></div>';

    // Pagination
    if (detailsPagination.totalPages > 1) {
      html += '<div class="usage-details-pagination">' +
        '<button class="usage-page-btn" data-page="prev"' + (detailsPagination.page <= 1 ? ' disabled' : '') + '>' + (typeof t === 'function' ? t('usage.prev') : '← Prev') + '</button>' +
        '<span class="usage-page-info">' + (typeof t === 'function' ? t('usage.pageOf', detailsPagination.page, detailsPagination.totalPages) : 'Page ' + detailsPagination.page + ' of ' + detailsPagination.totalPages) + '</span>' +
        '<button class="usage-page-btn" data-page="next"' + (detailsPagination.page >= detailsPagination.totalPages ? ' disabled' : '') + '>' + (typeof t === 'function' ? t('usage.next') : 'Next →') + '</button>' +
        '</div>';
    }
  }

  container.innerHTML = html;
  bindDetailsEvents();
}

function bindDetailsEvents() {
  // Filter apply button
  const filterBtn = document.getElementById('detailsFilterApply');
  if (filterBtn) {
    filterBtn.addEventListener('click', function () {
      const prov = document.getElementById('detailsProviderFilter');
      const start = document.getElementById('detailsStartDate');
      const end = document.getElementById('detailsEndDate');
      usageState.detailsFilters.provider = prov ? prov.value : '';
      usageState.detailsFilters.startDate = start ? start.value : '';
      usageState.detailsFilters.endDate = end ? end.value : '';
      usageState.detailsPagination.page = 1;
      fetchRequestDetails();
    });
  }

  // Pagination buttons
  document.querySelectorAll('.usage-page-btn').forEach(btn => {
    btn.addEventListener('click', function () {
      if (this.disabled) return;
      const dir = this.dataset.page;
      if (dir === 'prev') usageState.detailsPagination.page = Math.max(1, usageState.detailsPagination.page - 1);
      else usageState.detailsPagination.page = Math.min(usageState.detailsPagination.totalPages, usageState.detailsPagination.page + 1);
      fetchRequestDetails();
    });
  });

  // View detail buttons
  document.querySelectorAll('.usage-details-view-btn').forEach(btn => {
    btn.addEventListener('click', function () {
      const idx = parseInt(this.dataset.detailIdx);
      const detail = usageState.detailsData[idx];
      if (detail) {
        usageState.selectedDetail = detail;
        usageState.isDrawerOpen = true;
        renderDetailDrawer();
      }
    });
  });


}

function renderDetailDrawer() {
  const overlay = document.getElementById('detailsDrawerOverlay');
  const drawer = document.getElementById('detailsDrawer');
  if (!overlay || !drawer) return;

  if (!usageState.isDrawerOpen || !usageState.selectedDetail) {
    overlay.classList.add('hidden');
    return;
  }

  // Click overlay to close
  overlay.onclick = function(e) {
    if (e.target === overlay) {
      usageState.isDrawerOpen = false;
      usageState.selectedDetail = null;
      overlay.classList.add('hidden');
    }
  };

  overlay.classList.remove('hidden');
  const d = usageState.selectedDetail;

  const inputTokens = d.tokens?.prompt_tokens || d.tokens?.input_tokens || 0;
  const outputTokens = d.tokens?.completion_tokens || 0;

  drawer.innerHTML =
    '<div class="usage-drawer-header">' +
    '<h3>' + (typeof t === 'function' ? t('usage.requestDetails') : 'Request Details') + '</h3>' +
    '<button id="detailsDrawerClose" class="usage-drawer-close">&times;</button>' +
    '</div>' +
    '<div class="usage-drawer-body">' +
    '<div class="usage-drawer-info-grid">' +
    '<div><span class="text-text-muted">' + (typeof t === 'function' ? t('usage.drawer.timestamp') : 'Timestamp:') + '</span> <span>' + new Date(d.timestamp).toLocaleString() + '</span></div>' +
    '<div><span class="text-text-muted">' + (typeof t === 'function' ? t('usage.drawer.account') : 'Account:') + '</span> <span class="font-medium usage-drawer-account-value" title="' + escAttr(getUsageAccountName(d)) + '">' + escHtml(getUsageAccountName(d)) + '</span></div>' +
    '<div><span class="text-text-muted">' + (typeof t === 'function' ? t('usage.drawer.model') : 'Model:') + '</span> <span class="font-mono">' + escHtml(d.model || '-') + '</span></div>' +
    '<div><span class="text-text-muted">' + (typeof t === 'function' ? t('usage.drawer.status') : 'Status:') + '</span> <span class="' + (d.status === 'success' ? 'text-success' : 'text-error') + '">' + escHtml(translateStatus(d.status)) + '</span></div>' +
    '<div><span class="text-text-muted">' + (typeof t === 'function' ? t('usage.drawer.latency') : 'Latency:') + '</span> <span class="font-mono">' + (typeof t === 'function' ? t('usage.drawer.ttft') : 'TTFT') + ' ' + (d.latency?.ttft || 0) + 'ms / Total ' + (d.latency?.total || 0) + 'ms</span></div>' +
    '<div><span class="text-text-muted">' + (typeof t === 'function' ? t('usage.drawer.inputTokens') : 'Input Tokens:') + '</span> <span class="font-mono">' + fmtNum(inputTokens) + '</span></div>' +
    '<div><span class="text-text-muted">' + (typeof t === 'function' ? t('usage.drawer.outputTokens') : 'Output Tokens:') + '</span> <span class="font-mono">' + fmtNum(outputTokens) + '</span></div>' +
    // Attribute the failure to a concrete account by name/email. The raw
    // account ID is kept only in the title attribute so it remains available
    // for tracing without cluttering the visible error line.
    (d.error
      ? '<div class="usage-drawer-error-row"><span class="text-text-muted">' + (typeof t === 'function' ? t('usage.drawer.error') : 'Error:') + '</span> ' +
        '<span class="text-error usage-drawer-error-value" title="' + escAttr(getUsageAccountName(d) + (d.accountId ? ' · ID: ' + d.accountId : '') + ' — ' + d.error) + '">' +
        '<span class="usage-recent-error-account">' + escHtml(getUsageAccountName(d)) + '</span>' +
        ' ' + escHtml(d.error) + '</span></div>'
      : '') +
    '</div>';

  // Request/Response sections (collapsible)
  if (d.request) {
    drawer.innerHTML += collapsibleSection((typeof t === 'function' ? t('usage.drawer.clientRequest') : 'Client Request (Input)'),
      '<pre class="usage-drawer-pre">' + escHtml(JSON.stringify(d.request, null, 2)) + '</pre>', true);
  }
  if (d.providerRequest) {
    drawer.innerHTML += collapsibleSection((typeof t === 'function' ? t('usage.drawer.providerRequest') : 'Provider Request (Translated)'),
      '<pre class="usage-drawer-pre">' + escHtml(JSON.stringify(d.providerRequest, null, 2)) + '</pre>', false);
  }
  if (d.providerResponse) {
    drawer.innerHTML += collapsibleSection((typeof t === 'function' ? t('usage.drawer.providerResponse') : 'Provider Response (Raw)'),
      '<pre class="usage-drawer-pre">' + escHtml(typeof d.providerResponse === 'object' ? JSON.stringify(d.providerResponse, null, 2) : d.providerResponse) + '</pre>', false);
  }
  if (d.response) {
    const respContent = d.response.content || JSON.stringify(d.response, null, 2);
    drawer.innerHTML += collapsibleSection((typeof t === 'function' ? t('usage.drawer.clientResponse') : 'Client Response (Final)'),
      '<pre class="usage-drawer-pre">' + escHtml(respContent) + '</pre>', true);
  }

  drawer.innerHTML += '</div>';

  // Wire up close button
  var closeBtn = document.getElementById('detailsDrawerClose');
  if (closeBtn) {
    closeBtn.onclick = function() {
      usageState.isDrawerOpen = false;
      usageState.selectedDetail = null;
      overlay.classList.add('hidden');
    };
  }
}

// ─── Main Render ─────────────────────────────────────────
function translateStatus(status) {
  var map = { 'success': 'usage.status.success', 'ok': 'usage.status.ok', 'error': 'usage.status.error', 'failed': 'usage.status.failed', 'pending': 'usage.status.pending' };
  var key = map[status];
  return key ? (typeof t === 'function' ? t(key) : status) : status;
}

function renderUsagePage() {
  // Re-render tab bar so Overview/Details labels get translated
  try { renderUsageTabs(); } catch(e) {}
  if (usageState.activeTab === 'overview') {
    try { renderOverviewCards(); } catch(e) { console.error('[Usage] overviewCards:', e); }
    try { renderTopology(); } catch(e) { console.error('[Usage] topology:', e); }
    try { renderRecentRequests(); } catch(e) { console.error('[Usage] recentRequests:', e); }
    try { renderUsageTable(); } catch(e) { console.error('[Usage] usageTable:', e); }
  } else if (usageState.activeTab === 'details') {
    try { renderRequestDetailsTable(); } catch(e) { console.error('[Usage] detailsTable:', e); }
  }
  // Chart is rendered separately via fetchUsageChart
}

// ─── Init / Destroy ──────────────────────────────────────
function initUsagePage() {
  usageState.sortBy = { model: 'requests', account: 'requests', apiKey: 'requests', endpoint: 'requests' };
  usageState.sortOrder = { model: 'desc', account: 'desc', apiKey: 'desc', endpoint: 'desc' };

  // Reset to overview sub-tab
  usageState.activeTab = 'overview';

  // Reset zoom and drag state
  topoZoom = 1;
  topoPanX = 0;
  topoPanY = 0;
  topoDragState.dragging = false;

  renderUsageTabs();
  // Ensure overview content is visible
  const ovEl = document.getElementById('usageOverviewContent');
  const dtEl = document.getElementById('usageDetailsContent');
  if (ovEl) ovEl.classList.remove('hidden');
  if (dtEl) dtEl.classList.add('hidden');
  renderPeriodSelector();
  fetchUsageStats(usageState.period);
  fetchUsageChart(usageState.period);
  connectUsageSSE();

  if (usageState.refreshTimer) clearInterval(usageState.refreshTimer);
  usageState.refreshTimer = setInterval(() => {
    updateTimeAgoEls();
    refreshActiveTab();
  }, AUTO_REFRESH_INTERVAL_MS);
  lastRefreshAt = Date.now();
  updateLiveIndicator();
  
  // Reset topology first-seen tracking
  topoFirstSeen = {};
}

// Catch-up on return to a visible tab: refreshActiveTab() skipped every tick
// while hidden, so fetch once immediately instead of waiting out the interval.
// The SSE stream is also re-opened, because the browser may have dropped it
// while the tab was backgrounded.
function onUsageVisible() {
  if (document.hidden) return;
  if (!usageState.refreshTimer) return; // page not active
  if (!usageState.eventSource) connectUsageSSE();
  refreshActiveTab();
}

document.addEventListener('visibilitychange', onUsageVisible);

function destroyUsagePage() {
  disconnectUsageSSE();
  if (usageState.refreshTimer) {
    clearInterval(usageState.refreshTimer);
    usageState.refreshTimer = null;
  }
  // Clean up topology resources
  if (topoTickTimer) {
    clearInterval(topoTickTimer);
    topoTickTimer = null;
  }
  const container = document.getElementById('usageTopology');
  if (container && container._topoResizeObserver) {
    container._topoResizeObserver.disconnect();
    container._topoResizeObserver = null;
  }
  topoListenersRegistered = false;
  topoDragState.dragging = false;
  topoFirstSeen = {};
}

// ─── Utility ─────────────────────────────────────────────
function escHtml(s) {
  const d = document.createElement('div');
  d.textContent = s == null ? '' : String(s);
  return d.innerHTML;
}
function escAttr(s) {
  return escHtml(s).replace(/"/g, '&quot;');
}

// ─── Pool Strategy Tab ───────────────────────────────────
let poolStrategyState = { data: null };

async function fetchPoolStrategy() {
  const container = document.getElementById('usagePoolContent');
  if (!container) return;
  container.innerHTML = '<div class="usage-loading">Loading pool strategy...</div>';
  try {
    const res = await api('/pool/strategy');
    if (!res.ok) throw new Error('HTTP ' + res.status);
    poolStrategyState.data = await res.json();
    renderPoolStrategyTab();
  } catch (e) {
    container.innerHTML = '<div class="usage-error">Failed to load: ' + escapeHtml(String(e)) + '</div>';
  }
}

function renderPoolStrategyTab() {
  const container = document.getElementById('usagePoolContent');
  if (!container || !poolStrategyState.data) return;
  const d = poolStrategyState.data;
  const current = d.strategy || 'round-robin';
  const minSize = d.minPoolSize || 20;
  const leadMin = d.resetAwareLeadMin || 30;
  const options = d.availableOptions || ['round-robin', 'cost-optimized', 'reset-aware'];

  const descFor = (s) => {
    switch (s) {
      case 'round-robin': return 'Default weighted round-robin. Best for small/medium pools with even quota. Zero overhead.';
      case 'cost-optimized': return 'Prefer accounts with most remaining quota (lowest CodexPrimaryUsedPercent / highest ExtCreditsRemaining). Activates only when pool has ≥ ' + minSize + ' accounts. Reduces mid-stream 429s.';
      case 'reset-aware': return 'Avoid accounts whose quota window resets within ' + leadMin + ' min. Falls back to cost-optimized ranking among safe accounts. Activates only when pool has ≥ ' + minSize + ' accounts.';
      default: return '';
    }
  };

  const cards = options.map(opt => {
    const active = opt === current;
    return `
      <div class="comp-toggle-row" style="cursor:pointer;${active ? 'border-color:#7c3aed;background:rgba(124,58,237,0.05)' : ''}" data-strategy="${escAttr(opt)}">
        <div class="comp-toggle-info">
          <div class="comp-toggle-label">${escHtml(opt)}${active ? ' <span style="color:#7c3aed;font-size:0.75rem">(active)</span>' : ''}</div>
          <div class="comp-toggle-desc">${escHtml(descFor(opt))}</div>
        </div>
        <input type="radio" name="poolStrategy" value="${escAttr(opt)}" ${active ? 'checked' : ''} style="accent-color:#7c3aed"/>
      </div>`;
  }).join('');

  container.innerHTML = `
    <div class="comp-page">
      <div class="comp-section">
        <div class="comp-section-title">Pool Routing Strategy</div>
        <div style="margin-bottom:12px;color:var(--text-muted,#888);font-size:0.85rem">
          Controls how the proxy selects an upstream account for each request.
          Strategies only activate when the pool has ≥ ${minSize} unique accounts;
          smaller pools always use round-robin (no overhead).
          Cache-sticky pinning always wins over the strategy — a cache hit saves
          more quota than any strategy choice.
        </div>
        ${cards}
      </div>
    </div>`;

  container.querySelectorAll('[data-strategy]').forEach(row => {
    row.addEventListener('click', async function () {
      const strategy = this.dataset.strategy;
      if (strategy === current) return;
      try {
        const res = await api('/pool/strategy', { method: 'PATCH', body: JSON.stringify({ strategy }) });
        if (!res.ok) throw new Error('HTTP ' + res.status);
        showToast('Pool strategy: ' + strategy);
        fetchPoolStrategy();
      } catch (e) {
        showToast('Failed: ' + e);
      }
    });
  });
}
