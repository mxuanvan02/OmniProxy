'use strict';

// Shared HTML escaping for the admin dashboard.
// Loaded as the first script in index.html so every consumer (app.js,
// accounts.js, quota.js, usage.js, combos.js, apiCli.js, settings.js) sees it
// regardless of the dynamic loader's completion order.

var HTML_ESCAPES = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };

// Escapes & < > " ' so the result is safe in both element text and quoted
// attribute values. Numeric 0 / false are stringified, not dropped.
function escapeHtml(s) {
  if (s == null) return '';
  return String(s).replace(/[&<>"']/g, function (c) { return HTML_ESCAPES[c]; });
}

// Retained for existing call sites; escapeHtml already covers quotes.
function escapeAttr(s) {
  return escapeHtml(s);
}

window.escapeHtml = escapeHtml;
window.escapeAttr = escapeAttr;
