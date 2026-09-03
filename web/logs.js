'use strict';

// OmniProxy Logs Page — live-tail the proxy log via SSE, with level filter + search.

let logsState = {
  eventSource: null,
  lines: [],          // { level, text } in chronological order
  levelFilter: 'all',
  search: '',
  paused: false,
  bound: false,
  maxLines: 500,      // reduced from 2048 — fewer DOM nodes = less lag
  renderPending: false,  // throttle: coalesce rapid SSE bursts into one render
  // SSE reconnect bookkeeping — see usage.js for the rationale. The old
  // `if (logsState.eventSource)` guard stopped zombie streams but still
  // hammered every 3s and could stack multiple pending timers.
  sseGeneration: 0,
  sseRetryTimer: null,
  sseRetryDelay: 0,
  lastEventID: '',    // sent back as Last-Event-ID so the server resumes
};

// Map a raw log line's 6-char prefix to a level. Lines are formatted by the Go
// logger as "<PREFIX><timestamp> <msg>" where PREFIX is one of "DEBUG ",
// "INFO  ", "WARN  ", "ERROR " (padded to 6 chars). Unknown → "info".
function logLineLevel(line) {
  if (line.startsWith('ERROR ')) return 'error';
  if (line.startsWith('WARN  ')) return 'warn';
  if (line.startsWith('DEBUG ')) return 'debug';
  if (line.startsWith('INFO  ')) return 'info';
  return 'info';
}

function pushLogLine(raw) {
  if (!raw) return;
  const text = raw.endsWith('\n') ? raw.slice(0, -1) : raw;
  if (text === '') return;
  logsState.lines.push({ level: logLineLevel(text), text: text });
  if (logsState.lines.length > logsState.maxLines) {
    logsState.lines.splice(0, logsState.lines.length - logsState.maxLines);
  }
}

function logLineMatchesFilter(entry) {
  if (logsState.levelFilter !== 'all' && entry.level !== logsState.levelFilter) return false;
  if (logsState.search && entry.text.toLowerCase().indexOf(logsState.search) === -1) return false;
  return true;
}

const LOGS_RENDER_DEBOUNCE_MS = 150;
let logsRenderTimer = null;

function renderLogs() {
  const viewer = document.getElementById('logsViewer');
  if (!viewer) return;
  const atBottom = viewer.scrollHeight - viewer.scrollTop - viewer.clientHeight < 40;
  const frag = document.createDocumentFragment();
  let shown = 0;
  for (const entry of logsState.lines) {
    if (!logLineMatchesFilter(entry)) continue;
    const div = document.createElement('div');
    div.className = 'logs-line logs-' + entry.level;
    div.textContent = entry.text;
    frag.appendChild(div);
    shown++;
  }
  viewer.replaceChildren(frag);
  if (shown === 0) {
    const empty = document.createElement('div');
    empty.className = 'logs-empty';
    empty.textContent = (typeof t === 'function' ? t('logs.empty') : 'No log lines');
    viewer.replaceChildren(empty);
  }
  // Keep pinned to the newest line unless the user scrolled up.
  if (!logsState.paused && atBottom) {
    viewer.scrollTop = viewer.scrollHeight;
  }
}

// Throttled render: coalesce rapid render requests into a single repaint so the
// page doesn't freeze when hundreds of log lines arrive in quick succession.
//
// Two coalescing modes share this one entry point on purpose — an earlier pass
// added a second timer-based scheduler alongside this rAF one, which meant two
// independent pending-render flags could both fire for the same burst.
//
//   scheduleRender()      → next animation frame. For SSE line arrival, where
//                           the goal is "don't render 200x per second" but the
//                           newest line should still appear immediately.
//   scheduleRender(150)    → debounced. For the search box, where each keystroke
//                           invalidates the previous filter result, so rendering
//                           intermediate states is wasted work.
function scheduleRender(delayMs) {
  if (delayMs > 0) {
    // Debounce: restart the clock on every call so a burst of keystrokes
    // collapses into one repaint after the user pauses.
    if (logsRenderTimer) clearTimeout(logsRenderTimer);
    logsRenderTimer = setTimeout(() => {
      logsRenderTimer = null;
      renderLogs();
    }, delayMs);
    return;
  }
  if (logsState.renderPending) return;
  logsState.renderPending = true;
  requestAnimationFrame(() => {
    logsState.renderPending = false;
    renderLogs();
  });
}

// cancelScheduledRender drops any pending repaint. Called on teardown so a
// queued timer cannot fire against a viewer the user already navigated away
// from — the same class of bug as the SSE retry timers fixed earlier.
function cancelScheduledRender() {
  if (logsRenderTimer) {
    clearTimeout(logsRenderTimer);
    logsRenderTimer = null;
  }
  logsState.renderPending = false;
}

async function fetchLogsSnapshot() {
  try {
    const res = await api('/logs');
    if (res.ok) {
      const data = await res.json();
      logsState.lines = [];
      (data.lines || []).forEach(pushLogLine);
      renderLogs();
    }
  } catch (e) { console.error('[Logs] snapshot error:', e); }
}

const LOGS_SSE_RETRY_BASE_MS = 3000;
const LOGS_SSE_RETRY_MAX_MS = 60000;

function scheduleLogsSSERetry(generation) {
  if (generation !== logsState.sseGeneration) return;
  if (logsState.sseRetryTimer) return;

  const prev = logsState.sseRetryDelay || 0;
  const delay = prev === 0
    ? LOGS_SSE_RETRY_BASE_MS
    : Math.min(prev * 2, LOGS_SSE_RETRY_MAX_MS);
  logsState.sseRetryDelay = delay;

  logsState.sseRetryTimer = setTimeout(() => {
    logsState.sseRetryTimer = null;
    if (generation !== logsState.sseGeneration) return;
    connectLogsSSE();
  }, delay);
}

function connectLogsSSE() {
  if (logsState.eventSource) {
    logsState.eventSource.close();
  }
  if (logsState.sseRetryTimer) {
    clearTimeout(logsState.sseRetryTimer);
    logsState.sseRetryTimer = null;
  }
  const generation = logsState.sseGeneration;
  try {
    // A freshly constructed EventSource does NOT send the Last-Event-ID header
    // (the browser only does that on its own internal reconnect), so the
    // resume cursor is passed explicitly as a query parameter.
    let url = '/admin/api/logs/stream?token=' + encodeURIComponent(adminToken);
    if (logsState.lastEventID) {
      url += '&lastEventId=' + encodeURIComponent(logsState.lastEventID);
    } else {
      // First connect: only replay what the viewer can actually hold.
      url += '&tail=' + logsState.maxLines;
    }
    const es = new EventSource(url);
    logsState.eventSource = es;

    es.onopen = function () {
      logsState.sseRetryDelay = 0;
    };

    es.onmessage = function (e) {
      if (e.lastEventId) logsState.lastEventID = e.lastEventId;
      if (logsState.paused) return;
      try {
        const data = JSON.parse(e.data);
        if (data.line) {
          pushLogLine(data.line);
          scheduleRender();
        }
      } catch (err) { /* ignore parse errors (e.g. keepalive) */ }
    };

    es.onerror = function () {
      scheduleLogsSSERetry(generation);
    };
  } catch (e) {
    console.error('[Logs] SSE connect error:', e);
    scheduleLogsSSERetry(generation);
  }
}

function disconnectLogsSSE() {
  logsState.sseGeneration++;
  logsState.sseRetryDelay = 0;
  if (logsState.sseRetryTimer) {
    clearTimeout(logsState.sseRetryTimer);
    logsState.sseRetryTimer = null;
  }
  if (logsState.eventSource) {
    logsState.eventSource.close();
    logsState.eventSource = null;
  }
}

function bindLogsEvents() {
  if (logsState.bound) return;
  const levelSel = document.getElementById('logsLevelFilter');
  const searchInput = document.getElementById('logsSearch');
  const pauseBtn = document.getElementById('logsPauseBtn');
  const clearBtn = document.getElementById('logsClearBtn');

  if (levelSel) levelSel.addEventListener('change', function () {
    logsState.levelFilter = this.value;
    renderLogs();
  });
  if (searchInput) searchInput.addEventListener('input', function () {
    logsState.search = this.value.trim().toLowerCase();
    // Debounced: renderLogs() rebuilds up to maxLines (500) DOM nodes, so
    // repainting on every keystroke makes typing feel sticky. 150ms is below
    // the threshold where the delay is noticeable but collapses a burst of
    // keystrokes into a single repaint.
    scheduleRender(LOGS_RENDER_DEBOUNCE_MS);
  });
  if (pauseBtn) pauseBtn.addEventListener('click', function () {
    logsState.paused = !logsState.paused;
    this.classList.toggle('active', logsState.paused);
    const label = this.querySelector('.btn-text');
    if (label) label.textContent = logsState.paused
      ? (typeof t === 'function' ? t('logs.resume') : 'Resume')
      : (typeof t === 'function' ? t('logs.pause') : 'Pause');
    if (!logsState.paused) renderLogs();
  });
  if (clearBtn) clearBtn.addEventListener('click', function () {
    logsState.lines = [];
    renderLogs();
  });
  logsState.bound = true;
}

function initLogsPage() {
  bindLogsEvents();
  // No fetchLogsSnapshot() here: the SSE endpoint already replays the tail as
  // its initial burst. Doing both fetched the same ring buffer twice (~480 KB)
  // and could duplicate lines in the viewer.
  connectLogsSSE();
}

function destroyLogsPage() {
  disconnectLogsSSE();
  // Drop any pending repaint. Without this a debounced render queued by the
  // search box can fire after the user has switched tabs, rebuilding a viewer
  // that is no longer on screen.
  cancelScheduledRender();
}
