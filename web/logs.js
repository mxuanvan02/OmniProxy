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

// Throttled render: coalesce rapid SSE bursts into a single render frame so
// the page doesn't freeze when hundreds of log lines arrive in quick succession.
function scheduleRender() {
  if (logsState.renderPending) return;
  logsState.renderPending = true;
  requestAnimationFrame(() => {
    logsState.renderPending = false;
    renderLogs();
  });
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

function connectLogsSSE() {
  if (logsState.eventSource) {
    logsState.eventSource.close();
  }
  try {
    const es = new EventSource('/admin/api/logs/stream?pwd=' + encodeURIComponent(password));
    logsState.eventSource = es;

    es.onmessage = function (e) {
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
      setTimeout(() => { if (logsState.eventSource) connectLogsSSE(); }, 3000);
    };
  } catch (e) {
    console.error('[Logs] SSE connect error:', e);
    setTimeout(() => { if (logsState.eventSource) connectLogsSSE(); }, 5000);
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
    renderLogs();
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
  fetchLogsSnapshot();
  connectLogsSSE();
}

function destroyLogsPage() {
  if (logsState.eventSource) {
    logsState.eventSource.close();
    logsState.eventSource = null;
  }
}
