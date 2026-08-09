'use strict';

  // =====================================================================
  // COMBOS TAB
  // =====================================================================

  let combosData = [];

  async function loadCombos() {
    const list = $('combosList');
    if (!list) return;
    try {
      const res = await api('/combos');
      if (!res.ok) throw new Error('HTTP ' + res.status);
      const d = await res.json();
      combosData = Array.isArray(d.combos) ? d.combos : [];
      renderCombos();
    } catch (e) {
      combosData = [];
      list.innerHTML = '<div class="muted-text" style="padding:0.5rem 0;">' + escapeHtml(t('combos.loadFailed')) + '</div>';
    }
  }

  function renderCombos() {
    const list = $('combosList');
    if (!list) return;
    if (!combosData.length) {
      list.innerHTML = '<div class="muted-text" style="padding:0.5rem 0;">' + escapeHtml(t('combos.empty')) + '</div>';
      return;
    }
    list.innerHTML = combosData.map(combo => {
      const models = Array.isArray(combo.models) ? combo.models : [];
      const strategy = combo.strategy || 'fallback';
      return '<div class="api-key-entry" style="margin-bottom:0.5rem;padding:0.75rem;border:1px solid var(--border);border-radius:6px;">' +
        '<div style="display:flex;justify-content:space-between;align-items:flex-start;flex-wrap:wrap;gap:0.5rem;">' +
        '<div>' +
        '<span style="font-weight:600;font-size:1rem;">' + escapeHtml(combo.name) + '</span>' +
        '<span style="margin-left:0.5rem;padding:0.1rem 0.4rem;border-radius:4px;font-size:0.75rem;background:var(--badge-bg,#e8f4fd);color:var(--badge-color,#1a73e8);">' + escapeHtml(strategy) + '</span>' +
        '</div>' +
        '<div style="display:flex;gap:0.4rem;align-items:center;">' +
        '<button class="btn btn-small" onclick="editCombo(\'' + escapeHtml(combo.id).replace(/'/g, '&#39;') + '\')" data-i18n="combos.actionEdit"></button>' +
        '<button class="btn btn-small" onclick="toggleComboStrategy(\'' + escapeHtml(combo.id).replace(/'/g, '&#39;') + '\')" title="' + escapeHtml(strategy === 'round-robin' ? (typeof t === 'function' ? t('combos.switchToFallback') : 'Switch to Fallback') : (typeof t === 'function' ? t('combos.switchToRoundRobin') : 'Switch to Round Robin')) + '" style="font-size:0.7rem;padding:0.25rem 0.5rem;background:' + (strategy === 'round-robin' ? '#7c3aed' : '#6366f1') + ';color:#fff;border:none;border-radius:9999px;cursor:pointer;font-weight:600;">' + escapeHtml(strategy === 'round-robin' ? 'RR' : 'FB') + '</button>' +
        '<button class="btn btn-small btn-danger" onclick="deleteCombo(\'' + escapeHtml(combo.id).replace(/'/g, '&#39;') + '\',\'' + escapeHtml(combo.name).replace(/'/g, '&#39;') + '\')" data-i18n="combos.delete"></button>' +
        '</div>' +
        '</div>' +
        '<div style="margin-top:0.5rem;display:flex;flex-wrap:wrap;align-items:center;gap:0.35rem;">' +
        models.map(function(m, i) { var c = ['#6366f1', '#0891b2', '#059669', '#d97706', '#dc2626', '#7c3aed', '#2563eb', '#16a34a', '#ea580c', '#9333ea'][i % 10]; return '<span style="display:inline-flex;align-items:center;padding:0.2rem 0.6rem;border-radius:9999px;font-size:0.75rem;font-weight:600;background:' + c + ';color:#fff;border:1px solid ' + c + ';">' + escapeHtml(m) + '</span>' + (i < models.length - 1 ? '<span style="color:var(--muted-foreground,#666);font-size:0.9rem;font-weight:700;">\u2192</span>' : ''); }).join('') +
        '</div>' +
        '</div>';
    }).join('');
    applyTranslations();
  }

  let comboEditId = null;
  let comboModelRows = [];
  let comboDragIndex = null;

  function openComboModal(combo) {
    comboEditId = combo ? combo.id : null;
    $('comboModalTitle').setAttribute('data-i18n', combo ? 'combos.modalTitleEdit' : 'combos.modalTitleCreate');
    $('comboForm_name').value = combo ? combo.name : '';
    $('comboForm_strategy').value = combo ? (combo.strategy || 'fallback') : 'fallback';
    comboModelRows = combo && Array.isArray(combo.models) ? [...combo.models] : [];
    renderComboModelRows();
    applyTranslations();
    openDialog('comboModal');
  }

  function renderComboModelRows() {
    const container = $('comboModelsContainer');
    if (!container) return;
    container.innerHTML =
      '<div class="combo-model-list">' +
      comboModelRows.map((m, i) =>
        '<div draggable="true" data-index="' + i + '" ' +
        'ondragstart="onComboRowDragStart(event)" ' +
        'ondragover="onComboRowDragOver(event)" ' +
        'ondrop="onComboRowDrop(event)" ' +
        'class="combo-model-row">' +
        '<span class="combo-model-grip">&#9776;</span>' +
        comboRowTestIcon(m) +
        '<span class="combo-model-name">' + escapeHtml(m) + '</span>' +
        '<button type="button" class="combo-model-test-btn" onclick="event.stopPropagation();window.testComboModel(\'' + escapeHtml(m).replace(/'/g, '&#39;') + '\')" title="' + escapeHtml(t('cliTools.testModel')) + '"><i class="fa-solid fa-flask"></i></button>' +
        '<button type="button" class="combo-model-remove" onclick="removeComboModelRow(' + i + ')" title="' + escapeHtml(t('common.remove')) + '">&times;</button>' +
        '</div>'
      ).join('') +
      '</div>' +
      '<button type="button" class="btn btn-small" onclick="openModelPicker()" style="margin-top:0.4rem;">' + escapeHtml(t('combos.addModel')) + '</button>';
  }

  function comboRowTestIcon(modelId) {
    const testRes = window.modelTestResults ? window.modelTestResults[modelId] : undefined;
    const testing = window.modelTesting ? window.modelTesting[modelId] : false;
    if (testing) {
      return '<span class="combo-model-test-icon" style="flex-shrink:0;width:1.1em;text-align:center;"><i class="fa-solid fa-spinner fa-spin" style="color:var(--text-secondary);font-size:0.7rem;"></i></span>';
    }
    if (testRes === 'ok') {
      return '<span class="combo-model-test-icon" style="flex-shrink:0;width:1.1em;text-align:center;"><i class="fa-solid fa-check-circle" style="color:#22c55e;font-size:0.7rem;"></i></span>';
    }
    if (testRes === 'error') {
      return '<span class="combo-model-test-icon" style="flex-shrink:0;width:1.1em;text-align:center;"><i class="fa-solid fa-times-circle" style="color:#ef4444;font-size:0.7rem;"></i></span>';
    }
    return '';
  }

  window.testComboModel = async function (modelId) {
    if (window.modelTesting[modelId]) return;
    window.modelTesting[modelId] = true;
    renderComboModelRows();
    try {
      const res = await api('/cli-tools/test-model', {
        method: 'POST',
        body: JSON.stringify({ model: modelId })
      });
      const data = await res.json();
      window.modelTestResults[modelId] = data.ok ? 'ok' : 'error';
    } catch (e) {
      window.modelTestResults[modelId] = 'error';
    }
    delete window.modelTesting[modelId];
    renderComboModelRows();
  };

  function removeComboModelRow(idx) {
    comboModelRows.splice(idx, 1);
    renderComboModelRows();
  }

  function onComboRowDragStart(e) {
    const el = e.target.closest('[draggable]');
    if (!el) return;
    comboDragIndex = parseInt(el.dataset.index, 10);
    e.dataTransfer.effectAllowed = 'move';
    e.dataTransfer.setData('text/plain', '');
    el.classList.add('combo-model-dragging');
  }

  function onComboRowDragOver(e) {
    e.preventDefault();
    e.dataTransfer.dropEffect = 'move';
    const target = e.target.closest('[draggable]');
    if (target) target.classList.add('combo-model-dragover');
  }

  function onComboRowDrop(e) {
    e.preventDefault();
    document.querySelectorAll('.combo-model-dragover, .combo-model-dragging').forEach(el => {
      el.classList.remove('combo-model-dragover', 'combo-model-dragging');
    });
    const target = e.target.closest('[draggable]');
    if (!target) return;
    let targetIndex = parseInt(target.dataset.index, 10);
    if (comboDragIndex === null || comboDragIndex === targetIndex) return;
    const item = comboModelRows.splice(comboDragIndex, 1)[0];
    if (targetIndex > comboDragIndex) targetIndex--;
    comboModelRows.splice(targetIndex, 0, item);
    comboDragIndex = null;
    renderComboModelRows();
  }

  // ── Model Picker (multi-select) ─────────────────────────────────
  const PROVIDER_ALIAS_NAMES = {
    cc: 'Claude Code', ag: 'Antigravity', cx: 'OpenAI Codex',
    if: 'iFlow AI', qw: 'Qwen Code', gc: 'Gemini CLI',
    gh: 'GitHub Copilot', kr: 'Kiro AI', 'kiro-proxy': 'Kiro AI',
    openrouter: 'OpenRouter', glm: 'GLM Coding', kimi: 'Kimi Coding',
    minimax: 'Minimax Coding', openai: 'OpenAI', anthropic: 'Anthropic',
    gemini: 'Gemini', 'openai-codex': 'OpenAI Codex (ChatGPT sub)'
  };
  const PROVIDER_ALIAS_ORDER = [
    'cc','ag','cx','if','qw','gc','gh','kr','kiro-proxy',
    'openrouter','glm','kimi','minimax','openai','openai-codex','anthropic','gemini'
  ];

  let pickerModels = [];
  let pickerCombos = [];
  let pickerSelection = new Set();

  window.modelTestResults = {};
  window.modelTesting = {};

  async function openModelPicker() {
    try {
      const res = await fetch('/v1/models');
      const data = await res.json();
      const models = data.data || [];
      pickerCombos = [];
      pickerModels = [];
      models.forEach(m => {
        if (m.owned_by === 'combo') {
          pickerCombos.push(m.id);
        } else {
          pickerModels.push({ id: m.id, provider: m.owned_by });
        }
      });
      pickerSelection = new Set(comboModelRows);
      $('modelPickerSearch').value = '';
      renderModelPicker('');
      openDialog('modelPickerModal');
    } catch (e) {
      toast(t('combos.loadModelsFailed'), 'error');
    }
  }

  window.testModel = async function (modelId) {
    if (window.modelTesting[modelId]) return;
    window.modelTesting[modelId] = true;
    renderModelPicker($('modelPickerSearch').value);
    try {
      const res = await api('/cli-tools/test-model', {
        method: 'POST',
        body: JSON.stringify({ model: modelId })
      });
      const data = await res.json();
      window.modelTestResults[modelId] = data.ok ? 'ok' : 'error';
    } catch (e) {
      window.modelTestResults[modelId] = 'error';
    }
    delete window.modelTesting[modelId];
    renderModelPicker($('modelPickerSearch').value);
  };

  function renderModelPicker(query) {
    const list = $('modelPickerList');
    if (!list) return;
    const q = (query || '').toLowerCase();
    let html = '';

    const filteredCombos = pickerCombos.filter(c => c.toLowerCase().includes(q));
    if (filteredCombos.length > 0) {
      html += '<div class="picker-group-header" style="color:var(--accent-color);">' + escapeHtml(t('combos.comboGroup')) + '</div>';
      filteredCombos.forEach(c => { html += renderModelItem(c); });
    }

    const sortedProviders = Object.keys(
      pickerModels.reduce((acc, m) => { acc[m.provider] = true; return acc; }, {})
    ).sort((a, b) => {
      const ia = PROVIDER_ALIAS_ORDER.indexOf(a);
      const ib = PROVIDER_ALIAS_ORDER.indexOf(b);
      return (ia === -1 ? 999 : ia) - (ib === -1 ? 999 : ib);
    });

    sortedProviders.forEach(provider => {
      const filtered = pickerModels.filter(m => m.provider === provider && m.id.toLowerCase().includes(q));
      if (filtered.length === 0) return;
      const displayName = PROVIDER_ALIAS_NAMES[provider] || provider;
      html += '<div class="picker-group-header" style="color:var(--text-secondary);">' + escapeHtml(displayName) + '</div>';
      filtered.forEach(m => { html += renderModelItem(m.id); });
    });

    if (!html) {
      html = '<div class="picker-empty">' + escapeHtml(t(q ? 'combos.noMatches' : 'combos.noModels')) + '</div>';
    }
    list.innerHTML = html;
  }

function renderModelItem(modelId) {
  const selected = pickerSelection.has(modelId);
  const testRes = window.modelTestResults ? window.modelTestResults[modelId] : undefined;
  const testing = window.modelTesting ? window.modelTesting[modelId] : false;
  var statusIcon = '';
  if (testing) {
    statusIcon = '<i class="fa-solid fa-spinner fa-spin" style="color:var(--text-secondary);font-size:0.7rem;"></i>';
  } else if (testRes === 'ok') {
    statusIcon = '<i class="fa-solid fa-check-circle" style="color:#22c55e;font-size:0.7rem;"></i>';
  } else if (testRes === 'error') {
    statusIcon = '<i class="fa-solid fa-times-circle" style="color:#ef4444;font-size:0.7rem;"></i>';
  } else {
    statusIcon = '<i class="fa-solid fa-robot" style="color:var(--text-secondary);font-size:0.7rem;"></i>';
  }
  return '<div class="model-picker-item' + (selected ? ' selected' : '') + '" onclick="toggleModelInPicker(\'' + escapeHtml(modelId).replace(/'/g, '&#39;') + '\')">' +
    '<span class="picker-check">' + (selected ? '&#10003;' : '') + '</span>' +
    '<span class="picker-status-icon" style="width:1.1em;text-align:center;flex-shrink:0;">' + statusIcon + '</span>' +
    '<span style="flex:1;">' + escapeHtml(modelId) + '</span>' +
    '<button class="picker-test-btn" onclick="event.stopPropagation();window.testModel(\'' + escapeHtml(modelId).replace(/'/g, '&#39;') + '\')" type="button" title="' + escapeHtml(t('cliTools.testModel')) + '"><i class="fa-solid fa-flask"></i></button>' +
    '</div>';
}

  window.toggleModelInPicker = function (modelId) {
    if (typeof window.__cliModelCallback === 'function') {
      window.__cliModelCallback(modelId);
      window.__cliModelCallback = null;
      return;
    }
    if (pickerSelection.has(modelId)) {
      pickerSelection.delete(modelId);
    } else {
      pickerSelection.add(modelId);
    }
    renderModelPicker($('modelPickerSearch').value);
  }

  function confirmModelPicker() {
    if (typeof window.__cliModelCallback === 'function') {
      window.__cliModelCallback = null;
      closeDialog('modelPickerModal');
      return;
    }
    if (cliToolDetailId === 'opencode') {
      var models = Array.from(pickerSelection);
      if (models.length > 0) {
        var s = window.__openCodeState;
        models.forEach(function (m) {
          if (s.models.indexOf(m) === -1) s.models.push(m);
        });
        if (!s.activeModel) s.activeModel = models[0];
        reRenderDetailBody();
      }
      closeDialog('modelPickerModal');
      return;
    }
    comboModelRows = Array.from(pickerSelection);
    renderComboModelRows();
    closeDialog('modelPickerModal');
  }

  function filterModelPicker() {
    renderModelPicker($('modelPickerSearch').value);
  }

  async function saveCombo() {
    const name = ($('comboForm_name').value || '').trim();
    const strategy = $('comboForm_strategy').value;
    if (!name) { toast(t('combos.nameRequired'), 'error'); return; }
    if (name.includes('/')) { toast(t('combos.nameSlash'), 'error'); return; }
    const models = comboModelRows.map(m => m.trim()).filter(Boolean);
    if (models.length < 1) { toast(t('combos.minModels'), 'error'); return; }
    try {
      let res;
      if (comboEditId) {
        res = await api('/combos/' + encodeURIComponent(comboEditId), {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ name, models, strategy })
        });
      } else {
        res = await api('/combos', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ name, models, strategy })
        });
      }
      const d = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(d.error || t('common.failed'));
      toast(comboEditId ? t('combos.updated') : t('combos.created'), 'success');
      closeDialog('comboModal');
      await loadCombos();
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }

  function editCombo(id) {
    const combo = combosData.find(c => c.id === id);
    if (!combo) return;
    openComboModal(combo);
  }

  async function deleteCombo(id, name) {
    const ok = await confirmAction(t('combos.confirmDelete', name), { title: t('combos.delete'), confirmText: t('combos.delete') });
    if (!ok) return;
    try {
      const res = await api('/combos/' + encodeURIComponent(id), { method: 'DELETE' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(d.error || t('common.failed'));
      toast(t('combos.deleted'), 'success');
      await loadCombos();
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }


  async function toggleComboStrategy(comboId) {
    var combo = combosData.find(function(c) { return c.id === comboId; });
    if (!combo) return;
    var newStrategy = combo.strategy === 'round-robin' ? 'fallback' : 'round-robin';
    try {
      var res = await api('/combos/' + encodeURIComponent(comboId), {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ strategy: newStrategy })
      });
      var d = await res.json().catch(function() { return {}; });
      if (!res.ok) throw new Error(d.error || t('common.failed'));
      toast(t('combos.strategyUpdated'), 'success');
      await loadCombos();
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }
  // Expose combo functions to global scope for inline onclick
  window.toggleComboStrategy = toggleComboStrategy;
  window.editCombo = editCombo;
  window.deleteCombo = deleteCombo;
  window.removeComboModelRow = removeComboModelRow;
  window.openModelPicker = openModelPicker;
  window.filterModelPicker = filterModelPicker;
  window.confirmModelPicker = confirmModelPicker;
  window.onComboRowDragStart = onComboRowDragStart;
  window.onComboRowDragOver = onComboRowDragOver;
  window.onComboRowDrop = onComboRowDrop;

  function bindComboEvents() {
    const createBtn = $('createComboBtn');
    if (createBtn) createBtn.addEventListener('click', () => openComboModal(null));
    const saveBtn = $('comboModalSaveBtn');
    if (saveBtn) saveBtn.addEventListener('click', saveCombo);
    const cancelBtn = $('comboModalCancelBtn');
    if (cancelBtn) cancelBtn.addEventListener('click', () => closeDialog('comboModal'));
    const closeBtn = $('comboModalClose');
    if (closeBtn) closeBtn.addEventListener('click', () => closeDialog('comboModal'));
    bindDialogBackdropClose('comboModal', () => closeDialog('comboModal'));
    const pickerCancel = $('modelPickerCancelBtn');
    if (pickerCancel) pickerCancel.addEventListener('click', () => closeDialog('modelPickerModal'));
    const pickerDone = $('modelPickerDoneBtn');
    if (pickerDone) pickerDone.addEventListener('click', confirmModelPicker);
    const pickerClose = $('modelPickerClose');
    if (pickerClose) pickerClose.addEventListener('click', () => closeDialog('modelPickerModal'));
    bindDialogBackdropClose('modelPickerModal', () => closeDialog('modelPickerModal'));
  }
