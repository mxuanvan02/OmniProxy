// Capabilities tab for the legacy /admin UI. Mirrors the /admin-next
// CapabilitiesView: capabilities grouped by real status (usable / unverified /
// no account), each card compact with endpoints collapsed behind a toggle.
(function () {
  'use strict';

  var CAPABILITY_LABEL = {
    chat: 'Chat / Responses',
    vision: 'Nhận ảnh (vision)',
    search: 'Tìm kiếm',
    image: 'Tạo và sửa ảnh',
    video: 'Tạo video',
    'audio-stt': 'Nhận dạng giọng nói',
    'audio-tts': 'Tổng hợp giọng nói',
    'audio-music': 'Tạo nhạc',
    embedding: 'Embedding',
    moderation: 'Kiểm duyệt'
  };

  var GROUP_META = {
    verified: { title: 'Dùng được', hint: 'Đã probe và nhận phản hồi thật', cls: 'cap-tone-verified' },
    available: { title: 'Chưa xác minh', hint: 'Catalog quảng bá nhưng chưa probe', cls: 'cap-tone-available' },
    none: { title: 'Chưa có account', hint: 'Route tồn tại nhưng không account nào phục vụ', cls: 'cap-tone-none' }
  };
  var GROUP_ORDER = ['verified', 'available', 'none'];

  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  function groupOf(item) {
    if (item.verified) return 'verified';
    if (item.available) return 'available';
    return 'none';
  }

  function cardHtml(item) {
    var label = CAPABILITY_LABEL[item.capability] || item.capability;
    var endpoints = item.endpoints || [];
    var notes = [];
    if (item.probeFailures > 0) notes.push('probe lỗi ' + item.probeFailures);
    if (item.probeSkipped > 0) notes.push('bỏ qua ' + item.probeSkipped);
    var meta = groupOf(item);

    var endpointsHtml = endpoints.length
      ? '<details class="cap-endpoints-wrap">' +
          '<summary>' + endpoints.length + ' endpoint</summary>' +
          '<ul class="cap-endpoints">' + endpoints.map(function (e) {
            return '<li><code>' + esc(e) + '</code></li>';
          }).join('') + '</ul>' +
        '</details>'
      : '';

    return '' +
      '<section class="cap-card ' + GROUP_META[meta].cls + '">' +
        '<div class="cap-card-head">' +
          '<h3 title="' + esc(label) + '">' + esc(label) + '</h3>' +
          '<span class="cap-count">' + (item.enabledAccounts || 0) + '/' + (item.accounts || 0) + '</span>' +
        '</div>' +
        '<div class="cap-card-sub">' +
          '<code class="cap-code">' + esc(item.capability) + '</code>' +
          (notes.length ? '<span class="cap-note">' + esc(notes.join(' · ')) + '</span>' : '') +
        '</div>' +
        endpointsHtml +
      '</section>';
  }

  function groupHtml(key, items) {
    var m = GROUP_META[key];
    var cards = items.map(cardHtml).join('');
    return '' +
      '<section class="cap-group">' +
        '<div class="cap-group-head">' +
          '<span class="cap-badge ' + m.cls + '"><span class="cap-dot"></span>' + esc(m.title) + '</span>' +
          '<span class="cap-group-hint">' + items.length + ' · ' + esc(m.hint) + '</span>' +
        '</div>' +
        '<div class="cap-grid">' + cards + '</div>' +
      '</section>';
  }

  function render(matrix) {
    var host = document.getElementById('capabilitiesList');
    if (!host) return;
    var items = (matrix && matrix.capabilities) || [];
    if (!items.length) {
      host.innerHTML = '<div class="cap-empty">Chưa có dữ liệu năng lực. Bấm Làm mới hoặc probe một account.</div>';
      return;
    }

    var buckets = { verified: [], available: [], none: [] };
    items.forEach(function (i) { buckets[groupOf(i)].push(i); });
    var endpoints = items.reduce(function (t, i) { return t + ((i.endpoints || []).length); }, 0);

    var stats = '' +
      '<div class="cap-stats">' +
        '<div class="cap-stat"><div class="cap-stat-value cap-stat-ok">' + buckets.verified.length + '</div><div class="cap-stat-label">Dùng được</div></div>' +
        '<div class="cap-stat"><div class="cap-stat-value cap-stat-warn">' + buckets.available.length + '</div><div class="cap-stat-label">Chưa xác minh</div></div>' +
        '<div class="cap-stat"><div class="cap-stat-value cap-stat-muted">' + buckets.none.length + '</div><div class="cap-stat-label">Chưa có account</div></div>' +
        '<div class="cap-stat"><div class="cap-stat-value">' + endpoints + '</div><div class="cap-stat-label">Endpoint</div></div>' +
      '</div>';

    var groups = GROUP_ORDER
      .filter(function (k) { return buckets[k].length; })
      .map(function (k) { return groupHtml(k, buckets[k]); })
      .join('');

    host.innerHTML = stats + groups;
  }

  var loaded = false;

  async function loadCapabilities() {
    var host = document.getElementById('capabilitiesList');
    if (!host) return;
    if (!loaded) host.innerHTML = '<div class="cap-empty">Đang tải…</div>';
    try {
      var res = await api('/capabilities');
      if (!res.ok) throw new Error('HTTP ' + res.status);
      var matrix = await res.json();
      render(matrix);
      loaded = true;
    } catch (e) {
      host.innerHTML = '<div class="cap-empty">Không tải được năng lực: ' + esc(e.message || e) + '</div>';
    }
  }

  window.initCapabilitiesPage = loadCapabilities;

  document.addEventListener('click', function (e) {
    var btn = e.target && e.target.closest ? e.target.closest('#capabilitiesRefreshBtn') : null;
    if (btn) { loaded = false; loadCapabilities(); }
  });
})();
