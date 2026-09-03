'use strict';

  // CLI Tools
  // ---- CLI Tool State ----
  let cliApiKeyCache = {};
  let cliToolDetailId = null;

  // ---- Helper: get API key from select/input ----
  async function getCliApiKey(selectId, customId) {
    var sel = $(selectId);
    if (!sel) return '';
    if (sel.value === 'custom') {
      var inp = $(customId);
      return inp ? inp.value.trim() : '';
    }
    if (sel.value) {
      var keyId = sel.value;
      if (cliApiKeyCache[keyId]) return cliApiKeyCache[keyId];
      try {
        var res = await api('/cli-tools/apikey/' + encodeURIComponent(keyId));
        if (res.ok) {
          var d = await res.json();
          cliApiKeyCache[keyId] = d.key;
          return d.key;
        }
      } catch (e) {}
    }
    return '';
  }

  // ---- Helper: render endpoint select + API key select (used by many tools) ----
  function renderEndpointApiKeyFields(prefix) {
    var localUrl = (baseUrl || location.origin) + '/v1';
    var html = '';
    html += '<div class="form-group"><label data-i18n="cliTools.endpoint"></label>' +
      '<select id="' + prefix + '_ep" class="form-control" data-native-select="true">' +
      '<option value="local">' + escapeHtml(localUrl) + '</option>' +
      '<option value="custom" data-i18n="cliTools.customUrl"></option></select>' +
      '<input type="text" id="' + prefix + '_epCustom" class="form-control hidden" style="margin-top:0.4rem;" data-i18n-placeholder="cliTools.endpointPlaceholder" placeholder="https://example.com/v1" autocomplete="off" /></div>';
    html += '<div class="form-group"><label data-i18n="cliTools.apiKey"></label>' +
      '<select id="' + prefix + '_ak" class="form-control" data-native-select="true">' +
      '<option value="">--</option><option value="custom" data-i18n="cliTools.customUrl"></option></select>' +
      '<input type="text" id="' + prefix + '_akCustom" class="form-control hidden" style="margin-top:0.4rem;" data-i18n-placeholder="cliTools.apiKeyPlaceholder" placeholder="sk-..." autocomplete="off" /></div>';
    return html;
  }

  function bindEndpointApiKeyEvents(prefix) {
    var ep = $(prefix + '_ep');
    var ak = $(prefix + '_ak');
    if (ep) ep.addEventListener('change', function () {
      var c = $(prefix + '_epCustom');
      if (c) c.classList.toggle('hidden', this.value !== 'custom');
    });
    if (ak) ak.addEventListener('change', function () {
      var c = $(prefix + '_akCustom');
      if (c) c.classList.toggle('hidden', this.value !== 'custom');
    });
  }

  function populateAkSelect(prefix) {
    var ak = $(prefix + '_ak');
    if (!ak) return;
    var prevVal = ak.value;
    while (ak.options.length > 2) ak.remove(2);
    (apiKeysCache || []).forEach(function (k) {
      var opt = document.createElement('option');
      opt.value = k.id;
      opt.textContent = (k.name ? k.name + ' - ' : '') + k.keyMasked;
      ak.appendChild(opt);
    });
    if (prevVal && Array.from(ak.options).some(function (o) { return o.value === prevVal; })) {
      ak.value = prevVal;
    } else {
      ak.value = '';
      var c = $(prefix + '_akCustom');
      if (c) { c.classList.add('hidden'); c.value = ''; }
    }
  }

  function getEndpointValue(prefix) {
    var ep = $(prefix + '_ep');
    if (!ep) return '';
    if (ep.value === 'custom') {
      var c = $(prefix + '_epCustom');
      return c ? c.value.trim() : '';
    }
    return ep.options[ep.selectedIndex].textContent;
  }

  function getAkValue(prefix) {
    var ak = $(prefix + '_ak');
    if (!ak) return '';
    if (ak.value === 'custom') {
      var c = $(prefix + '_akCustom');
      return c ? c.value.trim() : '';
    }
    if (ak.value) return ak.value;
    return '';
  }

  // ---- Helper: render manual config modal ----
  function showManualConfigModal(configs) {
    var body = $('cliManualConfigBody');
    body.innerHTML = '<p class="cli-manual-config-hint" data-i18n="cliTools.manualConfigHint"></p>';
    applyTranslations();
    configs.forEach(function (cfg) {
      var div = document.createElement('div');
      div.className = 'cli-manual-config-entry';
      var filenameDiv = document.createElement('div');
      filenameDiv.className = 'cli-manual-config-filename';
      filenameDiv.innerHTML = '<i class="fa-regular fa-file-lines"></i> ' + escapeHtml(cfg.filename) +
        ' <button class="cli-manual-config-copy-btn" type="button"><i class="fa-regular fa-copy"></i> <span data-i18n="cliTools.copyConfig"></span></button>';
      div.appendChild(filenameDiv);
      var code = document.createElement('pre');
      code.className = 'cli-manual-config-code';
      code.textContent = cfg.content;
      div.appendChild(code);
      body.appendChild(div);
    });
    applyTranslations();
    body.querySelectorAll('.cli-manual-config-copy-btn').forEach(function (btn, i) {
      btn.addEventListener('click', function () {
        var code = body.querySelectorAll('.cli-manual-config-code')[i];
        if (!code) return;
        copyText(code.textContent).then(function () {
          var span = btn.querySelector('span');
          if (span) span.textContent = t('cliTools.copied');
          setTimeout(function () {
            if (span) span.textContent = t('cliTools.copyConfig');
          }, 2000);
        }).catch(function () {});
      });
    });
    openDialog('cliManualConfigModal');
  }

  // ---- Helper: show tool status in expanded card ----
  function showToolStatus(prefix, message, type) {
    var el = $(prefix + '_status');
    if (!el) return;
    el.className = 'cli-tool-status-' + (type || 'success') + ' hidden';
    el.textContent = message || '';
    el.classList.remove('hidden');
  }

  function claudeManualConfig(endpoint, apiKey) {
    endpoint = endpoint.replace(/\/v1$/, "");
    var env = { ANTHROPIC_BASE_URL: endpoint, ANTHROPIC_API_KEY: apiKey };
    return [{ filename: '~/.claude/settings.json', content: JSON.stringify({ hasCompletedOnboarding: true, env: env }, null, 2) }];
  }

  // ================================================================
  // CLAUDE CODE — dedicated config
  // ================================================================
  // Effort levels Claude Code's settings accept. An unrecognized value is
  // discarded by the client, so this list must not gain entries the server does
  // not also accept (see normalizeClaudeEffort).
  var CLAUDE_EFFORTS = [
    { value: 'low', label: 'Low — fast & cheap' },
    { value: 'medium', label: 'Medium — balanced' },
    { value: 'high', label: 'High — deep reasoning' },
    { value: 'xhigh', label: 'Extra High — deeper reasoning' },
    { value: 'max', label: 'Max — hardest problems' }
  ];

  function renderClaudeConfig() {
    var prefix = 'claude';
    var state = window.__claudeState || { model: '', reasoningEffort: 'high' };
    var html = '<div style="padding:1rem;border-top:1px solid var(--border);">';
    html += renderEndpointApiKeyFields(prefix);
    html += '<div class="form-group"><label data-i18n="cliTools.model"></label>' +
      '<div style="display:flex;gap:0.5rem;align-items:center;">' +
      '<div style="flex:1;position:relative;">' +
      '<input type="text" id="claudeModel" class="form-control" style="width:100%;" data-i18n-placeholder="cliTools.modelPlaceholder" placeholder="model-id" value="' + escapeAttr(state.model) + '" autocomplete="off" />' +
      (state.model ? '<button onclick="claudeClearModel()" type="button" style="position:absolute;right:4px;top:50%;transform:translateY(-50%);padding:2px 6px;border:none;background:none;cursor:pointer;font-size:0.85rem;color:var(--muted-foreground);">&times;</button>' : '') +
      '</div>' +
      '<button class="btn btn-outline btn-sm" onclick="claudePickModel()" type="button" data-i18n="cliTools.selectModel"></button></div></div>';
    html += '<div class="form-group"><label>Reasoning Effort</label>' +
      '<select id="claudeEffort" class="form-control" data-native-select="true">' +
      CLAUDE_EFFORTS.map(function (opt) {
        return '<option value="' + opt.value + '"' +
          (state.reasoningEffort === opt.value ? ' selected' : '') + '>' + opt.label + '</option>';
      }).join('') +
      '</select>' +
      '<small style="display:block;margin-top:0.25rem;color:var(--muted-foreground);">Sets <code>effortLevel</code> in ~/.claude/settings.json, for the session and for the selected model.</small></div>';
    html += '<div id="' + prefix + '_status" class="hidden" style="margin-top:0.75rem;padding:0.5rem 0.75rem;border-radius:6px;font-size:0.8125rem;"></div>';
    html += '<div style="margin-top:1rem;display:flex;gap:0.5rem;">' +
      '<button class="btn btn-secondary" onclick="claudeShowManual()" type="button" data-i18n="cliTools.manualConfig"></button>' +
      '<button class="btn btn-outline" onclick="claudeReset()" type="button" data-i18n="cliTools.reset"></button>' +
      '<button class="btn btn-primary" onclick="claudeApply()" type="button" data-i18n="cliTools.apply"></button>' +
      '</div></div>';
    return html;
  }

  window.claudeSlotState = {};
  window.__claudeState = { model: '', reasoningEffort: 'high' };
  window.claudePickModel = function () {
    if (typeof openModelPicker === 'function') {
      window.__cliModelCallback = function (model) {
        window.__claudeState.model = model;
        closeDialog('modelPickerModal');
        reRenderDetailBody();
      };
      openModelPicker();
    }
  };
  window.claudeClearModel = function () {
    window.__claudeState.model = '';
    reRenderDetailBody();
  };
  window.claudeApply = async function () {
    var prefix = 'claude';
    populateAkSelect(prefix);
    var endpoint = getEndpointValue(prefix);
    if (!endpoint) { showToolStatus(prefix, 'Please select an endpoint', 'error'); return; }
    var apiKey = await getCliApiKey(prefix + '_ak', prefix + '_akCustom');
    var env = { ANTHROPIC_BASE_URL: endpoint, ANTHROPIC_API_KEY: apiKey };
    var model = window.__claudeState.model || '';
    var effortEl = $('claudeEffort');
    var effort = effortEl ? effortEl.value : (window.__claudeState.reasoningEffort || 'high');
    window.__claudeState.reasoningEffort = effort;
    var applyBtn = document.querySelector('.cli-tool-detail-body .btn-primary');
    if (applyBtn) { applyBtn.disabled = true; applyBtn.textContent = t('cliTools.applying'); }
    try {
      var res = await api('/cli-tools/claude', { method: 'POST', body: JSON.stringify({ env: env, model: model, reasoningEffort: effort }) });
      if (res.ok) {
        showToolStatus(prefix, t('cliTools.configSuccess'), 'success');
        updateCliBadge('claude', true);
      } else {
        var d = await res.json().catch(function () { return {}; });
        showToolStatus(prefix, (d && d.error) || t('cliTools.configFailed'), 'error');
      }
    } catch (e) {
      showToolStatus(prefix, e.message || t('cliTools.configFailed'), 'error');
    } finally {
      if (applyBtn) { applyBtn.disabled = false; applyBtn.textContent = t('cliTools.apply'); }
    }
  };
  window.claudeShowManual = async function () {
    populateAkSelect('claude');
    var endpoint = getEndpointValue('claude');
    var apiKey = await getCliApiKey('claude_ak', 'claude_akCustom');
    showManualConfigModal(claudeManualConfig(endpoint, apiKey || 'sk-your-api-key'));
  };
  window.claudeReset = async function () {
    var confirmed = await confirmAction('Reset Claude Code configuration?', { confirmText: 'Reset', variant: 'danger' });
    if (!confirmed) return;
    try {
      var res = await api('/cli-tools/claude', { method: 'DELETE' });
      if (res.ok) { showToolStatus('claude', 'Configuration reset', 'success'); updateCliBadge('claude', false); }
    } catch (e) {}
  };

  // ================================================================
  // OPENCODE — dedicated config
  // ================================================================
  function renderOpenCodeConfig() {
    var prefix = 'opencode';
    var state = window.__openCodeState || { models: [], activeModel: '', subagentModel: '' };
    var html = '<div style="padding:1rem;border-top:1px solid var(--border);">';
    html += renderEndpointApiKeyFields(prefix);
    html += '<div class="form-group"><label data-i18n="cliTools.model"></label>';
    html += '<div id="ocModels" style="display:flex;flex-wrap:wrap;gap:0.4rem;padding:0.5rem;border:1px solid var(--border);border-radius:6px;margin-bottom:0.5rem;min-height:2rem;">';
    if (state.models.length === 0) html += '<span style="font-size:0.8rem;color:var(--muted-foreground);" data-i18n="cliTools.noModels">No models selected.</span>';
    state.models.forEach(function (m) {
      var isActive = m === state.activeModel;
      html += '<span style="display:inline-flex;align-items:center;gap:0.25rem;padding:0.15rem 0.5rem;font-size:0.75rem;border-radius:4px;cursor:pointer;' + (isActive ? 'background:var(--primary);color:#fff;' : 'background:var(--muted);color:var(--foreground);border:1px solid var(--border);') + '" onclick="ocToggleActive(\'' + escapeAttr(m) + '\')">' +
        (isActive ? '<i class="fa-solid fa-star" style="font-size:0.6rem;"></i> ' : '') + escapeHtml(m) +
        '<i class="fa-solid fa-xmark" style="cursor:pointer;margin-left:0.2rem;font-size:0.65rem;" onclick="event.stopPropagation();ocRemoveModel(\'' + escapeAttr(m) + '\')"></i></span>';
    });
    html += '</div>';
    html += '<div style="margin-bottom:0.5rem;">' +
      '<button class="btn btn-outline btn-sm" onclick="ocSelectModel()" type="button" data-i18n="cliTools.selectModels">Select Models</button></div>';
    html += '<div style="display:flex;gap:0.5rem;align-items:center;margin-bottom:0.4rem;">' +
      '<span style="font-size:0.75rem;font-weight:600;color:var(--muted-foreground);min-width:5rem;" data-i18n="cliTools.active">Active:</span>' +
      '<input type="text" class="form-control" style="flex:1;" value="' + escapeAttr(state.activeModel) + '" oninput="window.__openCodeState.activeModel=this.value" data-i18n-placeholder="cliTools.modelPlaceholder" placeholder="provider/model-id" /></div>';
    html += '<div style="display:flex;gap:0.5rem;align-items:center;">' +
      '<span style="font-size:0.75rem;font-weight:600;color:var(--muted-foreground);min-width:5rem;" data-i18n="cliTools.subagent">Subagent:</span>' +
      '<input type="text" id="ocSubagentInput" class="form-control" style="flex:1;" value="' + escapeAttr(state.subagentModel) + '" oninput="window.__openCodeState.subagentModel=this.value" data-i18n-placeholder="cliTools.subagentPlaceholder" placeholder="Same as active" />' +
      '<button class="btn btn-outline btn-sm" onclick="ocSelectSubagentModel()" type="button" data-i18n="cliTools.selectModel">Select Model</button>' +
      (state.subagentModel ? '<button class="btn btn-outline btn-sm" onclick="ocClearSubagent()" type="button" style="padding:0.25rem 0.5rem;" title="" data-i18n-title="cliTools.clear">&times;</button>' : '') +
      '</div>';
    html += '</div>';
    html += '<div id="' + prefix + '_status" class="hidden" style="margin-top:0.75rem;padding:0.5rem 0.75rem;border-radius:6px;font-size:0.8125rem;"></div>';
    html += '<div style="margin-top:1rem;display:flex;gap:0.5rem;">' +
      '<button class="btn btn-secondary" onclick="ocShowManual()" type="button" data-i18n="cliTools.manualConfig"></button>' +
      '<button class="btn btn-outline" onclick="ocReset()" type="button" data-i18n="cliTools.reset"></button>' +
      '<button class="btn btn-primary" onclick="ocApply()" type="button" data-i18n="cliTools.apply"></button>' +
      '</div></div>';
    return html;
  }

  window.__openCodeState = { models: [], activeModel: '', subagentModel: '' };
  window.ocToggleActive = function (model) {
    var s = window.__openCodeState;
    s.activeModel = s.activeModel === model ? '' : model;
    reRenderDetailBody();
  };
  window.ocRemoveModel = function (model) {
    var s = window.__openCodeState;
    s.models = s.models.filter(function (m) { return m !== model; });
    if (s.activeModel === model) s.activeModel = s.models.length > 0 ? s.models[0] : '';
    reRenderDetailBody();
  };
  window.ocSelectModel = function () {
    openModelPicker();
  };
  window.ocSelectSubagentModel = function () {
    window.__cliModelCallback = function (model) {
      window.__openCodeState.subagentModel = model;
      closeDialog('modelPickerModal');
      reRenderDetailBody();
    };
    openModelPicker();
  };
  window.ocClearSubagent = function () {
    window.__openCodeState.subagentModel = '';
    var inp = document.getElementById('ocSubagentInput');
    if (inp) inp.value = '';
    reRenderDetailBody();
  };
  function bindOcEvents() {
    var prefix = 'opencode';
    bindEndpointApiKeyEvents(prefix);
  }
  window.ocApply = async function () {
    var prefix = 'opencode';
    populateAkSelect(prefix);
    var endpoint = getEndpointValue(prefix);
    if (!endpoint) { showToolStatus(prefix, 'Please select an endpoint', 'error'); return; }
    var apiKey = await getCliApiKey(prefix + '_ak', prefix + '_akCustom');
    var s = window.__openCodeState;
    var payload = { baseUrl: endpoint, apiKey: apiKey || null, models: s.models, activeModel: s.activeModel, subagentModel: s.subagentModel };
    var applyBtn = document.querySelector('.cli-tool-detail-body .btn-primary');
    if (applyBtn) { applyBtn.disabled = true; applyBtn.textContent = t('cliTools.applying'); }
    try {
      var res = await api('/cli-tools/opencode', { method: 'POST', body: JSON.stringify(payload) });
      if (res.ok) { showToolStatus(prefix, t('cliTools.configSuccess'), 'success'); updateCliBadge('opencode', true); }
      else { var d = await res.json().catch(function () { return {}; }); showToolStatus(prefix, (d && d.error) || t('cliTools.configFailed'), 'error'); }
    } catch (e) { showToolStatus(prefix, e.message || t('cliTools.configFailed'), 'error'); }
    finally { if (applyBtn) { applyBtn.disabled = false; applyBtn.textContent = t('cliTools.apply'); } }
  };
  window.ocShowManual = async function () {
    populateAkSelect('opencode');
    var endpoint = getEndpointValue('opencode');
    var apiKey = await getCliApiKey('opencode_ak', 'opencode_akCustom');
    var s = window.__openCodeState;
    var models = s.models.length > 0 ? s.models : ['provider/model-id'];
    var activeModel = s.activeModel || models[0];
    var subagentModel = s.subagentModel || activeModel;
    var modelsObj = {};
    models.forEach(function (m) { modelsObj[m] = { name: m, modalities: { input: ['text', 'image'], output: ['text'] } }; });
    showManualConfigModal([{
      filename: '~/.config/opencode/opencode.json',
      content: JSON.stringify({
        provider: { omniproxy: { npm: '@ai-sdk/openai-compatible', options: { baseURL: endpoint, apiKey: apiKey }, models: modelsObj } },
        model: 'omniproxy/' + activeModel,
        agent: { explorer: { description: 'Fast explorer subagent for codebase exploration', mode: 'subagent', model: 'omniproxy/' + subagentModel } }
      }, null, 2)
    }]);
  };
  window.ocReset = async function () {
    var confirmed = await confirmAction('Reset OpenCode configuration?', { confirmText: 'Reset', variant: 'danger' });
    if (!confirmed) return;
    try { var res = await api('/cli-tools/opencode', { method: 'DELETE' }); if (res.ok) { showToolStatus('opencode', 'Configuration reset', 'success'); updateCliBadge('opencode', false); } } catch (e) {}
  };

  // ================================================================
  // CLINE — dedicated config
  // ================================================================
  function renderClineConfig() {
    var prefix = 'cline';
    var state = window.__clineState || { model: '' };
    var html = '<div style="padding:1rem;border-top:1px solid var(--border);">';
    html += renderEndpointApiKeyFields(prefix);
    html += '<div class="form-group"><label data-i18n="cliTools.model"></label>' +
      '<div style="display:flex;gap:0.5rem;">' +
      '<input type="text" id="clineModel" class="form-control" style="flex:1;" data-i18n-placeholder="cliTools.modelPlaceholder" placeholder="provider/model-id" value="' + escapeAttr(state.model) + '" autocomplete="off" />' +
      '<button class="btn btn-outline btn-sm" onclick="clinePickModel()" type="button" data-i18n="cliTools.selectModel"></button></div></div>';
    html += '<div id="' + prefix + '_status" class="hidden" style="margin-top:0.75rem;padding:0.5rem 0.75rem;border-radius:6px;font-size:0.8125rem;"></div>';
    html += '<div style="margin-top:1rem;display:flex;gap:0.5rem;">' +
      '<button class="btn btn-secondary" onclick="clineShowManual()" type="button" data-i18n="cliTools.manualConfig"></button>' +
      '<button class="btn btn-outline" onclick="clineReset()" type="button" data-i18n="cliTools.reset"></button>' +
      '<button class="btn btn-primary" onclick="clineApply()" type="button" data-i18n="cliTools.apply"></button>' +
      '</div></div>';
    return html;
  }
  window.__clineState = { model: '' };
  window.clinePickModel = function () {
    if (typeof openModelPicker === 'function') {
      window.__cliModelCallback = function (model) {
        var inp = document.getElementById('clineModel');
        if (inp) inp.value = model;
        window.__clineState.model = model;
        closeDialog('modelPickerModal');
      };
      openModelPicker();
    }
  };
  window.clineApply = async function () {
    var prefix = 'cline';
    populateAkSelect(prefix);
    var endpoint = getEndpointValue(prefix);
    if (!endpoint) { showToolStatus(prefix, 'Please select an endpoint', 'error'); return; }
    var apiKey = await getCliApiKey(prefix + '_ak', prefix + '_akCustom');
    var model = window.__clineState.model || '';
    var applyBtn = document.querySelector('.cli-tool-detail-body .btn-primary');
    if (applyBtn) { applyBtn.disabled = true; applyBtn.textContent = t('cliTools.applying'); }
    try {
      var res = await api('/cli-tools/cline', { method: 'POST', body: JSON.stringify({ baseUrl: endpoint, apiKey: apiKey || null, model: model }) });
      if (res.ok) { showToolStatus(prefix, t('cliTools.configSuccess'), 'success'); updateCliBadge('cline', true); }
      else { var d = await res.json().catch(function () { return {}; }); showToolStatus(prefix, (d && d.error) || t('cliTools.configFailed'), 'error'); }
    } catch (e) { showToolStatus(prefix, e.message || t('cliTools.configFailed'), 'error'); }
    finally { if (applyBtn) { applyBtn.disabled = false; applyBtn.textContent = t('cliTools.apply'); } }
  };
  window.clineShowManual = async function () {
    populateAkSelect('cline');
    var endpoint = getEndpointValue('cline');
    var apiKey = await getCliApiKey('cline_ak', 'cline_akCustom');
    var model = window.__clineState.model || 'provider/model-id';
    var baseWithoutV1 = endpoint.replace(/\/v1\/?$/, '').replace(/\/+$/, '');
    showManualConfigModal([
      { filename: '~/.cline/data/globalState.json', content: JSON.stringify({ actModeApiProvider: 'openai', planModeApiProvider: 'openai', openAiBaseUrl: baseWithoutV1, openAiModelId: model, planModeOpenAiModelId: model }, null, 2) },
      { filename: '~/.cline/data/secrets.json', content: JSON.stringify({ openAiApiKey: apiKey }, null, 2) }
    ]);
  };
  window.clineReset = async function () {
    var confirmed = await confirmAction('Reset Cline configuration?', { confirmText: 'Reset', variant: 'danger' });
    if (!confirmed) return;
    try { var res = await api('/cli-tools/cline', { method: 'DELETE' }); if (res.ok) { showToolStatus('cline', 'Configuration reset', 'success'); updateCliBadge('cline', false); } } catch (e) {}
  };

  // ================================================================
  // CODEX — dedicated config
  // ================================================================
  // Effort levels Codex can render. The catalog OmniProxy writes narrows this
  // per model family, and the server steps the value down when the configured
  // model's family offers less, so a deeper choice here is never silently lost.
  var CODEX_EFFORTS = [
    { value: 'low', label: 'Low — fast & cheap' },
    { value: 'medium', label: 'Medium — balanced' },
    { value: 'high', label: 'High — deep reasoning' },
    { value: 'xhigh', label: 'Extra High — deeper reasoning' },
    { value: 'max', label: 'Max — hardest problems' },
    { value: 'ultra', label: 'Ultra — max reasoning with delegation' }
  ];

  function renderCodexConfig() {
    var prefix = 'codex';
    var state = window.__codexState || { model: '', subagentModel: '', reasoningEffort: 'medium' };
    var html = '<div style="padding:1rem;border-top:1px solid var(--border);">';
    html += renderEndpointApiKeyFields(prefix);
    html += '<div class="form-group"><label data-i18n="cliTools.model"></label>' +
      '<div style="display:flex;gap:0.5rem;align-items:center;">' +
      '<div style="flex:1;position:relative;">' +
      '<input type="text" id="codexModel" class="form-control" style="width:100%;" data-i18n-placeholder="cliTools.modelPlaceholder" placeholder="provider/model-id" value="' + escapeAttr(state.model) + '" autocomplete="off" />' +
      (state.model ? '<button onclick="codexClearModel()" type="button" style="position:absolute;right:4px;top:50%;transform:translateY(-50%);padding:2px 6px;border:none;background:none;cursor:pointer;font-size:0.85rem;color:var(--muted-foreground);">&times;</button>' : '') +
      '</div>' +
      '<button class="btn btn-outline btn-sm" onclick="codexPickModel()" type="button" data-i18n="cliTools.selectModel"></button></div></div>';
    html += '<div class="form-group"><label data-i18n="cliTools.codexSubagentModel">Subagent Model</label>' +
      '<div style="display:flex;gap:0.5rem;align-items:center;">' +
      '<div style="flex:1;position:relative;">' +
      '<input type="text" id="codexSubagent" class="form-control" style="width:100%;" data-i18n-placeholder="cliTools.codexSubagentPlace" placeholder="Same as main model" value="' + escapeAttr(state.subagentModel) + '" autocomplete="off" />' +
      (state.subagentModel ? '<button onclick="codexClearSubagent()" type="button" style="position:absolute;right:4px;top:50%;transform:translateY(-50%);padding:2px 6px;border:none;background:none;cursor:pointer;font-size:0.85rem;color:var(--muted-foreground);">&times;</button>' : '') +
      '</div>' +
      '<button class="btn btn-outline btn-sm" onclick="codexPickSubagent()" type="button" data-i18n="cliTools.codexSelect">Select</button></div></div>';
    html += '<div class="form-group"><label>Reasoning Effort</label>' +
      '<select id="codexEffort" class="form-control" data-native-select="true">' +
      CODEX_EFFORTS.map(function (opt) {
        return '<option value="' + opt.value + '"' +
          (state.reasoningEffort === opt.value ? ' selected' : '') + '>' + opt.label + '</option>';
      }).join('') +
      '</select>' +
      '<small style="display:block;margin-top:0.25rem;color:var(--muted-foreground);">Sets <code>model_reasoning_effort</code> in config.toml. Controls how many reasoning tokens the model spends before answering.</small></div>';
    html += '<div id="' + prefix + '_status" class="hidden" style="margin-top:0.75rem;padding:0.5rem 0.75rem;border-radius:6px;font-size:0.8125rem;"></div>';
    html += '<div style="margin-top:1rem;display:flex;gap:0.5rem;">' +
      '<button class="btn btn-secondary" onclick="codexShowManual()" type="button" data-i18n="cliTools.manualConfig"></button>' +
      '<button class="btn btn-outline" onclick="codexReset()" type="button" data-i18n="cliTools.reset"></button>' +
      '<button class="btn btn-primary" onclick="codexApply()" type="button" data-i18n="cliTools.apply"></button>' +
      '</div></div>';
    return html;
  }
  window.__codexState = { model: '', subagentModel: '', reasoningEffort: 'medium' };
  window.codexPickModel = function () {
    if (typeof openModelPicker === 'function') {
      window.__cliModelCallback = function (model) {
        window.__codexState.model = model;
        if (!window.__codexState.subagentModel) {
          window.__codexState.subagentModel = model;
        }
        closeDialog('modelPickerModal');
        reRenderDetailBody();
      };
      openModelPicker();
    }
  };
  window.codexClearModel = function () {
    window.__codexState.model = '';
    reRenderDetailBody();
  };
  window.codexPickSubagent = function () {
    if (typeof openModelPicker === 'function') {
      window.__cliModelCallback = function (model) {
        window.__codexState.subagentModel = model;
        closeDialog('modelPickerModal');
        reRenderDetailBody();
      };
      openModelPicker();
    }
  };
  window.codexClearSubagent = function () {
    window.__codexState.subagentModel = '';
    reRenderDetailBody();
  };
  window.codexApply = async function () {
    var prefix = 'codex';
    populateAkSelect(prefix);
    var endpoint = getEndpointValue(prefix);
    if (!endpoint) { showToolStatus(prefix, 'Please select an endpoint', 'error'); return; }
    var apiKey = await getCliApiKey(prefix + '_ak', prefix + '_akCustom');
    var model = window.__codexState.model || '';
    var subagentModel = window.__codexState.subagentModel || model;
    var effortEl = $('codexEffort');
    var effort = effortEl ? effortEl.value : 'medium';
    window.__codexState.reasoningEffort = effort;
    var applyBtn = document.querySelector('.cli-tool-detail-body .btn-primary');
    if (applyBtn) { applyBtn.disabled = true; applyBtn.textContent = t('cliTools.applying'); }
    try {
      var res = await api('/cli-tools/codex', { method: 'POST', body: JSON.stringify({ baseUrl: endpoint, apiKey: apiKey || null, model: model, subagentModel: subagentModel, reasoningEffort: effort }) });
      if (res.ok) { showToolStatus(prefix, t('cliTools.configSuccess'), 'success'); updateCliBadge('codex', true); }
      else { var d = await res.json().catch(function () { return {}; }); showToolStatus(prefix, (d && d.error) || t('cliTools.configFailed'), 'error'); }
    } catch (e) { showToolStatus(prefix, e.message || t('cliTools.configFailed'), 'error'); }
    finally { if (applyBtn) { applyBtn.disabled = false; applyBtn.textContent = t('cliTools.apply'); } }
  };
  window.codexShowManual = async function () {
    populateAkSelect('codex');
    var endpoint = getEndpointValue('codex');
    var apiKey = await getCliApiKey('codex_ak', 'codex_akCustom');
    var model = window.__codexState.model || 'provider/model-id';
    var subagentModel = window.__codexState.subagentModel || model;
    var effort = window.__codexState.reasoningEffort || 'medium';
    var effortLine = (effort && effort !== 'medium') ? 'model_reasoning_effort = "' + effort + '"\n' : '';
    var configContent = '# OmniProxy Configuration for Codex CLI\nmodel = "' + model + '"\nmodel_provider = "omniproxy"\n' + effortLine + '\n[model_providers.omniproxy]\nname = "OmniProxy"\nbase_url = "' + endpoint + '"\nwire_api = "responses"\nrequires_openai_auth = true\n\n[agents.subagent]\nmodel = "' + subagentModel + '"\n';
    showManualConfigModal([
      { filename: '~/.codex/config.toml', content: configContent },
      { filename: '~/.codex/auth.json', content: JSON.stringify({ auth_mode: 'apikey', OPENAI_API_KEY: apiKey }, null, 2) }
    ]);
  };
  window.codexReset = async function () {
    var confirmed = await confirmAction('Reset Codex configuration?', { confirmText: 'Reset', variant: 'danger' });
    if (!confirmed) return;
    try { var res = await api('/cli-tools/codex', { method: 'DELETE' }); if (res.ok) { showToolStatus('codex', 'Configuration reset', 'success'); updateCliBadge('codex', false); } } catch (e) {}
  };

  // ================================================================
  // KILO CODE — dedicated config
  // ================================================================
  function renderKiloConfig() {
    var prefix = 'kilo';
    var state = window.__kiloState || { model: '' };
    var html = '<div style="padding:1rem;border-top:1px solid var(--border);">';
    html += renderEndpointApiKeyFields(prefix);
    html += '<div class="form-group"><label data-i18n="cliTools.model"></label>' +
      '<div style="display:flex;gap:0.5rem;">' +
      '<input type="text" id="kiloModel" class="form-control" style="flex:1;" data-i18n-placeholder="cliTools.modelPlaceholder" placeholder="provider/model-id" value="' + escapeAttr(state.model) + '" autocomplete="off" />' +
      '<button class="btn btn-outline btn-sm" onclick="kiloPickModel()" type="button" data-i18n="cliTools.selectModel"></button></div></div>';
    html += '<div id="' + prefix + '_status" class="hidden" style="margin-top:0.75rem;padding:0.5rem 0.75rem;border-radius:6px;font-size:0.8125rem;"></div>';
    html += '<div style="margin-top:1rem;display:flex;gap:0.5rem;">' +
      '<button class="btn btn-secondary" onclick="kiloShowManual()" type="button" data-i18n="cliTools.manualConfig"></button>' +
      '<button class="btn btn-outline" onclick="kiloReset()" type="button" data-i18n="cliTools.reset"></button>' +
      '<button class="btn btn-primary" onclick="kiloApply()" type="button" data-i18n="cliTools.apply"></button>' +
      '</div></div>';
    return html;
  }
  window.__kiloState = { model: '' };
  window.kiloPickModel = function () {
    if (typeof openModelPicker === 'function') {
      window.__cliModelCallback = function (model) {
        document.getElementById('kiloModel').value = model;
        window.__kiloState.model = model;
        closeDialog('modelPickerModal');
      };
      openModelPicker();
    }
  };
  window.kiloApply = async function () {
    var prefix = 'kilo';
    populateAkSelect(prefix);
    var endpoint = getEndpointValue(prefix);
    if (!endpoint) { showToolStatus(prefix, 'Please select an endpoint', 'error'); return; }
    var apiKey = await getCliApiKey(prefix + '_ak', prefix + '_akCustom');
    var model = window.__kiloState.model || '';
    var applyBtn = document.querySelector('.cli-tool-detail-body .btn-primary');
    if (applyBtn) { applyBtn.disabled = true; applyBtn.textContent = t('cliTools.applying'); }
    try {
      var res = await api('/cli-tools/kilocode', { method: 'POST', body: JSON.stringify({ baseUrl: endpoint, apiKey: apiKey || null, model: model }) });
      if (res.ok) { showToolStatus(prefix, t('cliTools.configSuccess'), 'success'); updateCliBadge('kilo', true); }
      else { var d = await res.json().catch(function () { return {}; }); showToolStatus(prefix, (d && d.error) || t('cliTools.configFailed'), 'error'); }
    } catch (e) { showToolStatus(prefix, e.message || t('cliTools.configFailed'), 'error'); }
    finally { if (applyBtn) { applyBtn.disabled = false; applyBtn.textContent = t('cliTools.apply'); } }
  };
  window.kiloShowManual = async function () {
    populateAkSelect('kilo');
    var endpoint = getEndpointValue('kilo');
    var apiKey = await getCliApiKey('kilo_ak', 'kilo_akCustom');
    var model = window.__kiloState.model || 'provider/model-id';
    showManualConfigModal([{ filename: '~/.local/share/kilo/auth.json', content: JSON.stringify({ 'openai-compatible': { type: 'api-key', apiKey: apiKey, baseUrl: endpoint, model: model } }, null, 2) }]);
  };
  window.kiloReset = async function () {
    var confirmed = await confirmAction('Reset Kilo Code configuration?', { confirmText: 'Reset', variant: 'danger' });
    if (!confirmed) return;
    try { var res = await api('/cli-tools/kilocode', { method: 'DELETE' }); if (res.ok) { showToolStatus('kilo', 'Configuration reset', 'success'); updateCliBadge('kilo', false); } } catch (e) {}
  };

  // ================================================================
  // CONTINUE — guide config
  // ================================================================
  function renderContinueConfig() {
    var prefix = 'continue';
    var state = window.__continueState || { model: '', apiKey: '' };
    var html = '<div style="padding:1rem;border-top:1px solid var(--border);">';
    html += renderEndpointApiKeyFields(prefix);
    html += '<div class="form-group"><label data-i18n="cliTools.model"></label>' +
      '<div style="display:flex;gap:0.5rem;">' +
      '<input type="text" id="continueModel" class="form-control" style="flex:1;" data-i18n-placeholder="cliTools.modelPlaceholder" placeholder="provider/model-id" value="' + escapeAttr(state.model) + '" autocomplete="off" />' +
      '<button class="btn btn-outline btn-sm" onclick="continuePickModel()" type="button" data-i18n="cliTools.selectModel"></button></div></div>';
    html += '<div style="margin-top:1rem;">' +
      '<p style="font-size:0.8rem;color:var(--muted-foreground);margin-bottom:0.5rem;">' + t('cliTools.addModelConfig') + ' <code>~/.continue/config.json</code>:</p>' +
      '<pre class="cli-manual-config-code" id="continueCodeBlock" style="margin-bottom:0.5rem;"></pre>' +
      '<button class="btn btn-outline btn-sm" onclick="continueCopyCode()" type="button"><i class="fa-regular fa-copy"></i> <span data-i18n="cliTools.copyConfig"></span></button></div>';
    html += '</div>';
    return html;
  }
  window.__continueState = { model: '', apiKey: '' };
  window.continuePickModel = function () {
    if (typeof openModelPicker === 'function') {
      window.__cliModelCallback = function (model) {
        document.getElementById('continueModel').value = model;
        window.__continueState.model = model;
        updateContinueCode();
        closeDialog('modelPickerModal');
      };
      openModelPicker();
    }
  };
  function updateContinueCode() {
    var endpoint = getEndpointValue('continue');
    var apiKey = document.getElementById('continue_ak') ? document.getElementById('continue_ak').value : '';
    var model = window.__continueState.model || 'provider/model-id';
    var code = document.getElementById('continueCodeBlock');
    if (code) code.textContent = JSON.stringify({
      models: [{ title: 'OmniProxy', provider: 'openai', model: model, apiKey: apiKey, baseUrl: endpoint }],
      tabAutocompleteModel: { title: 'OmniProxy', provider: 'openai', model: model, apiKey: apiKey, baseUrl: endpoint }
    }, null, 2);
  }
  window.continueCopyCode = function () {
    var code = document.getElementById('continueCodeBlock');
    if (code) copyText(code.textContent).then(function () { toast(t('cliTools.copied'), 'primary'); }).catch(function () {});
  };

  // ================================================================
  // ROO — guide config
  // ================================================================
  function renderRooConfig() {
    var prefix = 'roo';
    var state = window.__rooState || { model: '' };
    var html = '<div style="padding:1rem;border-top:1px solid var(--border);">';
    html += renderEndpointApiKeyFields(prefix);
    html += '<div class="form-group"><label data-i18n="cliTools.model"></label>' +
      '<div style="display:flex;gap:0.5rem;">' +
      '<input type="text" id="rooModel" class="form-control" style="flex:1;" data-i18n-placeholder="cliTools.modelPlaceholder" placeholder="provider/model-id" value="' + escapeAttr(state.model) + '" autocomplete="off" />' +
      '<button class="btn btn-outline btn-sm" onclick="rooPickModel()" type="button" data-i18n="cliTools.selectModel"></button></div></div>';
    html += '<div id="' + prefix + '_status" class="hidden" style="margin-top:0.75rem;padding:0.5rem 0.75rem;border-radius:6px;font-size:0.8125rem;"></div>';
    html += '<div style="margin-top:1rem;display:flex;gap:0.5rem;">' +
      '<button class="btn btn-secondary" onclick="rooShowManual()" type="button" data-i18n="cliTools.manualConfig"></button>' +
      '</div></div>';
    return html;
  }
  window.__rooState = { model: '' };
  window.rooPickModel = function () {
    if (typeof openModelPicker === 'function') {
      window.__cliModelCallback = function (model) {
        document.getElementById('rooModel').value = model;
        window.__rooState.model = model;
        closeDialog('modelPickerModal');
      };
      openModelPicker();
    }
  };
  window.rooShowManual = async function () {
    populateAkSelect('roo');
    var endpoint = getEndpointValue('roo');
    var apiKey = await getCliApiKey('roo_ak', 'roo_akCustom');
    var model = window.__rooState.model || 'provider/model-id';
    showManualConfigModal([{
      filename: '~/.roo/config.json',
      content: JSON.stringify({ apiProvider: 'openai', openAiBaseUrl: endpoint, openAiModelId: model, openAiApiKey: apiKey }, null, 2)
    }]);
  };

  // ================================================================
  // DEEPSEEK TUI — dedicated config
  // ================================================================
  function renderDeepSeekConfig() {
    var prefix = 'deepseek';
    var state = window.__deepSeekState || { model: '' };
    var html = '<div style="padding:1rem;border-top:1px solid var(--border);">';
    html += renderEndpointApiKeyFields(prefix);
    html += '<div class="form-group"><label data-i18n="cliTools.model"></label>' +
      '<div style="display:flex;gap:0.5rem;">' +
      '<input type="text" id="deepseekModel" class="form-control" style="flex:1;" data-i18n-placeholder="cliTools.modelPlaceholder" placeholder="provider/model-id" value="' + escapeAttr(state.model) + '" autocomplete="off" />' +
      '<button class="btn btn-outline btn-sm" onclick="deepSeekPickModel()" type="button" data-i18n="cliTools.selectModel"></button></div></div>';
    html += '<div id="' + prefix + '_status" class="hidden" style="margin-top:0.75rem;padding:0.5rem 0.75rem;border-radius:6px;font-size:0.8125rem;"></div>';
    html += '<div style="margin-top:1rem;display:flex;gap:0.5rem;">' +
      '<button class="btn btn-secondary" onclick="deepSeekShowManual()" type="button" data-i18n="cliTools.manualConfig"></button>' +
      '<button class="btn btn-outline" onclick="deepSeekReset()" type="button" data-i18n="cliTools.reset"></button>' +
      '<button class="btn btn-primary" onclick="deepSeekApply()" type="button" data-i18n="cliTools.apply"></button>' +
      '</div></div>';
    return html;
  }
  window.__deepSeekState = { model: '' };
  window.deepSeekPickModel = function () {
    if (typeof openModelPicker === 'function') {
      window.__cliModelCallback = function (model) {
        document.getElementById('deepseekModel').value = model;
        window.__deepSeekState.model = model;
        closeDialog('modelPickerModal');
      };
      openModelPicker();
    }
  };
  window.deepSeekApply = async function () {
    var prefix = 'deepseek';
    populateAkSelect(prefix);
    var endpoint = getEndpointValue(prefix);
    if (!endpoint) { showToolStatus(prefix, 'Please select an endpoint', 'error'); return; }
    var apiKey = await getCliApiKey(prefix + '_ak', prefix + '_akCustom');
    var model = window.__deepSeekState.model || '';
    var applyBtn = document.querySelector('.cli-tool-detail-body .btn-primary');
    if (applyBtn) { applyBtn.disabled = true; applyBtn.textContent = t('cliTools.applying'); }
    try {
      var res = await api('/cli-tools/deepseek', { method: 'POST', body: JSON.stringify({ baseUrl: endpoint, apiKey: apiKey || null, model: model }) });
      if (res.ok) { showToolStatus(prefix, t('cliTools.configSuccess'), 'success'); updateCliBadge('deepseek', true); }
      else { var d = await res.json().catch(function () { return {}; }); showToolStatus(prefix, (d && d.error) || t('cliTools.configFailed'), 'error'); }
    } catch (e) { showToolStatus(prefix, e.message || t('cliTools.configFailed'), 'error'); }
    finally { if (applyBtn) { applyBtn.disabled = false; applyBtn.textContent = t('cliTools.apply'); } }
  };
  window.deepSeekShowManual = async function () {
    populateAkSelect('deepseek');
    var endpoint = getEndpointValue('deepseek');
    var apiKey = await getCliApiKey('deepseek_ak', 'deepseek_akCustom');
    var model = window.__deepSeekState.model || 'provider/model-id';
    var tomlContent = 'provider = "openai"\n\n[providers.openai]\nbase_url = "' + endpoint + '"\napi_key = "' + (apiKey || 'sk-your-api-key') + '"\nmodel = "' + model + '"\n';
    showManualConfigModal([{ filename: '~/.deepseek/config.toml', content: tomlContent }]);
  };
  window.deepSeekReset = async function () {
    var confirmed = await confirmAction('Reset DeepSeek TUI configuration?', { confirmText: 'Reset', variant: 'danger' });
    if (!confirmed) return;
    try { var res = await api('/cli-tools/deepseek', { method: 'DELETE' }); if (res.ok) { showToolStatus('deepseek', 'Configuration reset', 'success'); updateCliBadge('deepseek', false); } } catch (e) {}
  };

  // ================================================================
  // JCODE — dedicated config
  // ================================================================
  function renderJcodeConfig() {
    var prefix = 'jcode';
    var state = window.__jcodeState || { models: [] };
    var html = '<div style="padding:1rem;border-top:1px solid var(--border);">';
    html += renderEndpointApiKeyFields(prefix);
    html += '<div class="form-group"><label data-i18n="cliTools.model"></label>' +
      '<div style="display:flex;flex-wrap:wrap;gap:0.4rem;padding:0.5rem;border:1px solid var(--border);border-radius:6px;margin-bottom:0.5rem;min-height:2rem;">';
    if (state.models.length === 0) html += '<span style="font-size:0.8rem;color:var(--muted-foreground);" data-i18n="cliTools.noModels">No models selected.</span>';
    state.models.forEach(function (m) {
      html += '<span style="display:inline-flex;align-items:center;gap:0.25rem;padding:0.15rem 0.5rem;font-size:0.75rem;border-radius:4px;background:var(--muted);color:var(--foreground);border:1px solid var(--border);">' + escapeHtml(m) +
        '<i class="fa-solid fa-xmark" style="cursor:pointer;margin-left:0.2rem;font-size:0.65rem;" onclick="jcodeRemoveModel(\'' + escapeAttr(m) + '\')"></i></span>';
    });
    html += '</div><div style="display:flex;gap:0.5rem;margin-bottom:0.5rem;">' +
      '<input type="text" id="jcodeNewModel" class="form-control" style="flex:1;" data-i18n-placeholder="cliTools.typeModelAdd" placeholder="Type model then press Add" autocomplete="off" />' +
      '<button class="btn btn-outline btn-sm" onclick="jcodeAddModel()" type="button" data-i18n="cliTools.addModel">Add Model</button></div></div>';
    html += '<div id="' + prefix + '_status" class="hidden" style="margin-top:0.75rem;padding:0.5rem 0.75rem;border-radius:6px;font-size:0.8125rem;"></div>';
    html += '<div style="margin-top:1rem;display:flex;gap:0.5rem;">' +
      '<button class="btn btn-secondary" onclick="jcodeShowManual()" type="button" data-i18n="cliTools.manualConfig"></button>' +
      '<button class="btn btn-outline" onclick="jcodeReset()" type="button" data-i18n="cliTools.reset"></button>' +
      '<button class="btn btn-primary" onclick="jcodeApply()" type="button" data-i18n="cliTools.apply"></button>' +
      '</div></div>';
    return html;
  }
  window.__jcodeState = { models: [] };
  window.jcodeAddModel = function () {
    var inp = document.getElementById('jcodeNewModel');
    if (!inp || !inp.value.trim()) return;
    var s = window.__jcodeState;
    var m = inp.value.trim();
    if (s.models.indexOf(m) === -1) s.models.push(m);
    inp.value = '';
    reRenderDetailBody();
  };
  window.jcodeRemoveModel = function (model) {
    window.__jcodeState.models = window.__jcodeState.models.filter(function (m) { return m !== model; });
    reRenderDetailBody();
  };
  window.jcodeApply = async function () {
    var prefix = 'jcode';
    populateAkSelect(prefix);
    var endpoint = getEndpointValue(prefix);
    if (!endpoint) { showToolStatus(prefix, 'Please select an endpoint', 'error'); return; }
    var apiKey = await getCliApiKey(prefix + '_ak', prefix + '_akCustom');
    var models = window.__jcodeState.models || [];
    var applyBtn = document.querySelector('.cli-tool-detail-body .btn-primary');
    if (applyBtn) { applyBtn.disabled = true; applyBtn.textContent = t('cliTools.applying'); }
    try {
      var res = await api('/cli-tools/jcode', { method: 'POST', body: JSON.stringify({ baseUrl: endpoint, apiKey: apiKey || null, models: models }) });
      if (res.ok) { showToolStatus(prefix, t('cliTools.configSuccess'), 'success'); updateCliBadge('jcode', true); }
      else { var d = await res.json().catch(function () { return {}; }); showToolStatus(prefix, (d && d.error) || t('cliTools.configFailed'), 'error'); }
    } catch (e) { showToolStatus(prefix, e.message || t('cliTools.configFailed'), 'error'); }
    finally { if (applyBtn) { applyBtn.disabled = false; applyBtn.textContent = t('cliTools.apply'); } }
  };
  window.jcodeShowManual = async function () {
    populateAkSelect('jcode');
    var endpoint = getEndpointValue('jcode');
    var apiKey = await getCliApiKey('jcode_ak', 'jcode_akCustom');
    var models = window.__jcodeState.models.length > 0 ? window.__jcodeState.models : ['provider/model-id'];
    var defaultModel = models[0];
    var modelsToml = models.map(function (m) { return '[[providers.9router.models]]\nid = "' + m + '"'; }).join('\n');
    var tomlContent = '[providers.9router]\ntype = "openai-compatible"\nbase_url = "' + endpoint + '"\nauth = "bearer"\napi_key_env = "JCODE_9ROUTER_API_KEY"\nenv_file = "provider-9router.env"\ndefault_model = "' + defaultModel + '"\nrequires_api_key = true\n' + modelsToml + '\n';
    showManualConfigModal([
      { filename: '~/.jcode/config.toml', content: tomlContent },
      { filename: '~/.config/jcode/provider-9router.env', content: '# jcode provider environment variables\nJCODE_9ROUTER_API_KEY="' + (apiKey || 'sk-your-api-key') + '"\n' }
    ]);
  };
  window.jcodeReset = async function () {
    var confirmed = await confirmAction('Reset jcode configuration?', { confirmText: 'Reset', variant: 'danger' });
    if (!confirmed) return;
    try { var res = await api('/cli-tools/jcode', { method: 'DELETE' }); if (res.ok) { showToolStatus('jcode', 'Configuration reset', 'success'); updateCliBadge('jcode', false); } } catch (e) {}
  };

  // ================================================================
  // HERMES — dedicated config
  // ================================================================
  function renderHermesConfig() {
    var prefix = 'hermes';
    var state = window.__hermesState || { model: '' };
    var html = '<div style="padding:1rem;border-top:1px solid var(--border);">';
    html += renderEndpointApiKeyFields(prefix);
    html += '<div class="form-group"><label data-i18n="cliTools.model"></label>' +
      '<div style="display:flex;gap:0.5rem;">' +
      '<input type="text" id="hermesModel" class="form-control" style="flex:1;" data-i18n-placeholder="cliTools.modelPlaceholder" placeholder="provider/model-id" value="' + escapeAttr(state.model) + '" autocomplete="off" />' +
      '<button class="btn btn-outline btn-sm" onclick="hermesPickModel()" type="button" data-i18n="cliTools.selectModel"></button></div></div>';
    html += '<div id="' + prefix + '_status" class="hidden" style="margin-top:0.75rem;padding:0.5rem 0.75rem;border-radius:6px;font-size:0.8125rem;"></div>';
    html += '<div style="margin-top:1rem;display:flex;gap:0.5rem;">' +
      '<button class="btn btn-secondary" onclick="hermesShowManual()" type="button" data-i18n="cliTools.manualConfig"></button>' +
      '<button class="btn btn-outline" onclick="hermesReset()" type="button" data-i18n="cliTools.reset"></button>' +
      '<button class="btn btn-primary" onclick="hermesApply()" type="button" data-i18n="cliTools.apply"></button>' +
      '</div></div>';
    return html;
  }
  window.__hermesState = { model: '' };
  window.hermesPickModel = function () {
    if (typeof openModelPicker === 'function') {
      window.__cliModelCallback = function (model) {
        document.getElementById('hermesModel').value = model;
        window.__hermesState.model = model;
        closeDialog('modelPickerModal');
      };
      openModelPicker();
    }
  };
  window.hermesApply = async function () {
    var prefix = 'hermes';
    populateAkSelect(prefix);
    var endpoint = getEndpointValue(prefix);
    if (!endpoint) { showToolStatus(prefix, 'Please select an endpoint', 'error'); return; }
    var apiKey = await getCliApiKey(prefix + '_ak', prefix + '_akCustom');
    var model = window.__hermesState.model || '';
    var applyBtn = document.querySelector('.cli-tool-detail-body .btn-primary');
    if (applyBtn) { applyBtn.disabled = true; applyBtn.textContent = t('cliTools.applying'); }
    try {
      var res = await api('/cli-tools/hermes', { method: 'POST', body: JSON.stringify({ baseUrl: endpoint, apiKey: apiKey || null, model: model }) });
      if (res.ok) { showToolStatus(prefix, t('cliTools.configSuccess'), 'success'); updateCliBadge('hermes', true); }
      else { var d = await res.json().catch(function () { return {}; }); showToolStatus(prefix, (d && d.error) || t('cliTools.configFailed'), 'error'); }
    } catch (e) { showToolStatus(prefix, e.message || t('cliTools.configFailed'), 'error'); }
    finally { if (applyBtn) { applyBtn.disabled = false; applyBtn.textContent = t('cliTools.apply'); } }
  };
  window.hermesShowManual = async function () {
    populateAkSelect('hermes');
    var endpoint = getEndpointValue('hermes');
    var apiKey = await getCliApiKey('hermes_ak', 'hermes_akCustom');
    var model = window.__hermesState.model || 'provider/model-id';
    var yamlContent = 'providers:\n  omniproxy:\n    base_url: ' + endpoint + '\n    api_key: ' + (apiKey || 'sk-your-api-key') + '\n    api_mode: openai\n    discover_models: false\n    models:\n    - ' + model + '\n';
    showManualConfigModal([
      { filename: '~/.hermes/config.yaml', content: yamlContent },
      { filename: '~/.hermes/.env', content: 'OPENAI_API_KEY=' + (apiKey || 'sk-your-api-key') + '\n' }
    ]);
  };
  window.hermesReset = async function () {
    var confirmed = await confirmAction('Reset Hermes configuration?', { confirmText: 'Reset', variant: 'danger' });
    if (!confirmed) return;
    try { var res = await api('/cli-tools/hermes', { method: 'DELETE' }); if (res.ok) { showToolStatus('hermes', 'Configuration reset', 'success'); updateCliBadge('hermes', false); } } catch (e) {}
  };

  // ================================================================
  // FACTORY DROID — dedicated config
  // ================================================================
  function renderDroidConfig() {
    var prefix = 'droid';
    var state = window.__droidState || { models: [], activeModel: '' };
    var html = '<div style="padding:1rem;border-top:1px solid var(--border);">';
    html += renderEndpointApiKeyFields(prefix);
    html += '<div class="form-group"><label data-i18n="cliTools.model"></label>' +
      '<div style="display:flex;flex-wrap:wrap;gap:0.4rem;padding:0.5rem;border:1px solid var(--border);border-radius:6px;margin-bottom:0.5rem;min-height:2rem;">';
    if (state.models.length === 0) html += '<span style="font-size:0.8rem;color:var(--muted-foreground);" data-i18n="cliTools.noModels">No models selected.</span>';
    state.models.forEach(function (m) {
      var isActive = m === state.activeModel;
      html += '<span style="display:inline-flex;align-items:center;gap:0.25rem;padding:0.15rem 0.5rem;font-size:0.75rem;border-radius:4px;cursor:pointer;' + (isActive ? 'background:var(--primary);color:#fff;' : 'background:var(--muted);color:var(--foreground);border:1px solid var(--border);') + '" onclick="droidToggleActive(\'' + escapeAttr(m) + '\')">' +
        (isActive ? '<i class="fa-solid fa-star" style="font-size:0.6rem;"></i> ' : '') + escapeHtml(m) +
        '<i class="fa-solid fa-xmark" style="cursor:pointer;margin-left:0.2rem;font-size:0.65rem;" onclick="event.stopPropagation();droidRemoveModel(\'' + escapeAttr(m) + '\')"></i></span>';
    });
    html += '</div><div style="display:flex;gap:0.5rem;margin-bottom:0.5rem;">' +
      '<input type="text" id="droidNewModel" class="form-control" style="flex:1;" placeholder="Type model then press Add" autocomplete="off" />' +
      '<button class="btn btn-outline btn-sm" onclick="droidAddModel()" type="button" data-i18n="cliTools.addModel">Add Model</button></div>' +
      '<div style="display:flex;gap:0.5rem;align-items:center;margin-bottom:0.4rem;">' +
      '<span style="font-size:0.75rem;font-weight:600;color:var(--muted-foreground);min-width:5rem;" data-i18n="cliTools.active">Active:</span>' +
      '<input type="text" class="form-control" style="flex:1;" value="' + escapeAttr(state.activeModel) + '" onchange="window.__droidState.activeModel=this.value" data-i18n-placeholder="cliTools.modelPlaceholder" placeholder="provider/model-id" /></div></div>';
    html += '<div id="' + prefix + '_status" class="hidden" style="margin-top:0.75rem;padding:0.5rem 0.75rem;border-radius:6px;font-size:0.8125rem;"></div>';
    html += '<div style="margin-top:1rem;display:flex;gap:0.5rem;">' +
      '<button class="btn btn-secondary" onclick="droidShowManual()" type="button" data-i18n="cliTools.manualConfig"></button>' +
      '<button class="btn btn-outline" onclick="droidReset()" type="button" data-i18n="cliTools.reset"></button>' +
      '<button class="btn btn-primary" onclick="droidApply()" type="button" data-i18n="cliTools.apply"></button>' +
      '</div></div>';
    return html;
  }
  window.__droidState = { models: [], activeModel: '' };
  window.droidToggleActive = function (model) {
    var s = window.__droidState;
    s.activeModel = s.activeModel === model ? '' : model;
    reRenderDetailBody();
  };
  window.droidRemoveModel = function (model) {
    var s = window.__droidState;
    s.models = s.models.filter(function (m) { return m !== model; });
    if (s.activeModel === model) s.activeModel = s.models.length > 0 ? s.models[0] : '';
    reRenderDetailBody();
  };
  window.droidAddModel = function () {
    var inp = document.getElementById('droidNewModel');
    if (!inp || !inp.value.trim()) return;
    var s = window.__droidState;
    var m = inp.value.trim();
    if (s.models.indexOf(m) === -1) { s.models.push(m); if (!s.activeModel) s.activeModel = m; }
    inp.value = '';
    reRenderDetailBody();
  };
  window.droidApply = async function () {
    var prefix = 'droid';
    populateAkSelect(prefix);
    var endpoint = getEndpointValue(prefix);
    if (!endpoint) { showToolStatus(prefix, 'Please select an endpoint', 'error'); return; }
    var apiKey = await getCliApiKey(prefix + '_ak', prefix + '_akCustom');
    var s = window.__droidState;
    var applyBtn = document.querySelector('.cli-tool-detail-body .btn-primary');
    if (applyBtn) { applyBtn.disabled = true; applyBtn.textContent = t('cliTools.applying'); }
    try {
      var res = await api('/cli-tools/droid', { method: 'POST', body: JSON.stringify({ baseUrl: endpoint, apiKey: apiKey || null, models: s.models, activeModel: s.activeModel }) });
      if (res.ok) { showToolStatus(prefix, t('cliTools.configSuccess'), 'success'); updateCliBadge('droid', true); }
      else { var d = await res.json().catch(function () { return {}; }); showToolStatus(prefix, (d && d.error) || t('cliTools.configFailed'), 'error'); }
    } catch (e) { showToolStatus(prefix, e.message || t('cliTools.configFailed'), 'error'); }
    finally { if (applyBtn) { applyBtn.disabled = false; applyBtn.textContent = t('cliTools.apply'); } }
  };
  window.droidShowManual = async function () {
    populateAkSelect('droid');
    var endpoint = getEndpointValue('droid');
    var apiKey = await getCliApiKey('droid_ak', 'droid_akCustom');
    var s = window.__droidState;
    var models = s.models.length > 0 ? s.models : ['provider/model-id'];
    var activeModel = s.activeModel || models[0];
    var customModels = models.map(function (m, i) {
      return { model: m, id: 'custom:9Router-' + i, index: m === activeModel ? 0 : i + 1, baseUrl: endpoint, apiKey: apiKey, displayName: m, maxOutputTokens: 131072, noImageSupport: false, provider: 'openai' };
    });
    showManualConfigModal([{ filename: '~/.factory/settings.json', content: JSON.stringify({ customModels: customModels }, null, 2) }]);
  };
  window.droidReset = async function () {
    var confirmed = await confirmAction('Reset Factory Droid configuration?', { confirmText: 'Reset', variant: 'danger' });
    if (!confirmed) return;
    try { var res = await api('/cli-tools/droid', { method: 'DELETE' }); if (res.ok) { showToolStatus('droid', 'Configuration reset', 'success'); updateCliBadge('droid', false); } } catch (e) {}
  };

  // ================================================================
  // OPEN CLAW — dedicated config
  // ================================================================
  function renderOpenClawConfig() {
    var prefix = 'openclaw';
    var state = window.__openClawState || { model: '', agentModels: {} };
    var html = '<div style="padding:1rem;border-top:1px solid var(--border);">';
    html += renderEndpointApiKeyFields(prefix);
    html += '<div class="form-group"><label data-i18n="cliTools.model"></label>' +
      '<div style="display:flex;gap:0.5rem;">' +
      '<input type="text" id="openclawModel" class="form-control" style="flex:1;" data-i18n-placeholder="cliTools.modelPlaceholder" placeholder="provider/model-id" value="' + escapeAttr(state.model) + '" autocomplete="off" />' +
      '<button class="btn btn-outline btn-sm" onclick="openClawPickModel()" type="button" data-i18n="cliTools.selectModel"></button></div></div>';
    var agentIds = Object.keys(state.agentModels || {}).filter(function (id) { return id !== 'default'; });
    if (agentIds.length > 0) {
      html += '<div class="form-group"><label data-i18n="cliTools.subagent"></label>';
      agentIds.forEach(function (id) {
        var inputId = 'openclawAgent_' + id.replace(/[^a-zA-Z0-9_-]/g, '_');
        html += '<div style="display:flex;gap:0.5rem;margin-top:0.4rem;align-items:center;">' +
          '<span style="min-width:7rem;font-size:0.8rem;" title="' + escapeAttr(id) + '">' + escapeHtml(id) + '</span>' +
          '<input type="text" id="' + escapeAttr(inputId) + '" class="form-control" style="flex:1;" value="' + escapeAttr(state.agentModels[id]) + '" oninput="window.__openClawState.agentModels[' + JSON.stringify(id) + ']=this.value" />' +
          '<button class="btn btn-outline btn-sm" onclick="openClawPickAgentModel(' + JSON.stringify(id) + ')" type="button" data-i18n="cliTools.selectModel"></button></div>';
      });
      html += '</div>';
    }
    html += '<div id="' + prefix + '_status" class="hidden" style="margin-top:0.75rem;padding:0.5rem 0.75rem;border-radius:6px;font-size:0.8125rem;"></div>';
    html += '<div style="margin-top:1rem;display:flex;gap:0.5rem;">' +
      '<button class="btn btn-secondary" onclick="openClawShowManual()" type="button" data-i18n="cliTools.manualConfig"></button>' +
      '<button class="btn btn-outline" onclick="openClawReset()" type="button" data-i18n="cliTools.reset"></button>' +
      '<button class="btn btn-primary" onclick="openClawApply()" type="button" data-i18n="cliTools.apply"></button>' +
      '</div></div>';
    return html;
  }
  window.__openClawState = { model: '', agentModels: {} };
  window.openClawPickModel = function () {
    if (typeof openModelPicker === 'function') {
      window.__cliModelCallback = function (model) {
        document.getElementById('openclawModel').value = model;
        window.__openClawState.model = model;
        closeDialog('modelPickerModal');
      };
      openModelPicker();
    }
  };
  window.openClawPickAgentModel = function (agentId) {
    if (typeof openModelPicker !== 'function') return;
    window.__cliModelCallback = function (model) {
      window.__openClawState.agentModels[agentId] = model;
      closeDialog('modelPickerModal');
      reRenderDetailBody();
    };
    openModelPicker();
  };
  window.openClawApply = async function () {
    var prefix = 'openclaw';
    populateAkSelect(prefix);
    var endpoint = getEndpointValue(prefix);
    if (!endpoint) { showToolStatus(prefix, 'Please select an endpoint', 'error'); return; }
    var apiKey = await getCliApiKey(prefix + '_ak', prefix + '_akCustom');
    var model = window.__openClawState.model || '';
    var applyBtn = document.querySelector('.cli-tool-detail-body .btn-primary');
    if (applyBtn) { applyBtn.disabled = true; applyBtn.textContent = t('cliTools.applying'); }
    try {
      var res = await api('/cli-tools/openclaw', { method: 'POST', body: JSON.stringify({ baseUrl: endpoint, apiKey: apiKey || null, model: model, agentModels: window.__openClawState.agentModels || {} }) });
      if (res.ok) { showToolStatus(prefix, t('cliTools.configSuccess'), 'success'); updateCliBadge('openclaw', true); }
      else { var d = await res.json().catch(function () { return {}; }); showToolStatus(prefix, (d && d.error) || t('cliTools.configFailed'), 'error'); }
    } catch (e) { showToolStatus(prefix, e.message || t('cliTools.configFailed'), 'error'); }
    finally { if (applyBtn) { applyBtn.disabled = false; applyBtn.textContent = t('cliTools.apply'); } }
  };
  window.openClawShowManual = async function () {
    populateAkSelect('openclaw');
    var endpoint = getEndpointValue('openclaw');
    var apiKey = await getCliApiKey('openclaw_ak', 'openclaw_akCustom');
    var model = window.__openClawState.model || 'provider/model-id';
    var ocConfig = {
      models: { providers: { omniproxy: { baseUrl: endpoint, apiKey: apiKey, api: 'openai-completions', models: [{ id: model, name: model }] } } }
    };
    showManualConfigModal([{ filename: '~/.openclaw/openclaw.json', content: JSON.stringify(ocConfig, null, 2) }]);
  };
  window.openClawReset = async function () {
    var confirmed = await confirmAction('Reset Open Claw configuration?', { confirmText: 'Reset', variant: 'danger' });
    if (!confirmed) return;
    try { var res = await api('/cli-tools/openclaw', { method: 'DELETE' }); if (res.ok) { showToolStatus('openclaw', 'Configuration reset', 'success'); updateCliBadge('openclaw', false); } } catch (e) {}
  };

  // ================================================================
  // CURSOR — guide config
  // ================================================================
  function renderCursorConfig() {
    var prefix = 'cursor';
    var html = '<div style="padding:1rem;border-top:1px solid var(--border);">';
    html += renderEndpointApiKeyFields(prefix);
    html += '<ol style="margin:0.75rem 0;padding-left:1.25rem;font-size:0.8125rem;line-height:1.6;">' +
      '<li>' + t('cliTools.cursorStep1') + '</li>' +
      '<li>' + t('cliTools.cursorStep2') + '</li>' +
      '<li>' + t('cliTools.cursorStep3') + '</li>' +
      '<li>' + t('cliTools.cursorStep4') + '</li>' +
      '<li>' + t('cliTools.cursorStep5') + '</li>' +
      '</ol>';
    html += '<p style="font-size:0.75rem;color:var(--muted-foreground);margin-top:0.5rem;">' +
      '<i class="fa-solid fa-circle-info"></i> ' + t('cliTools.cursorInfo') + '</p>';
    html += '</div>';
    return html;
  }

  // ================================================================
  // AMP CLI — guide config
  // ================================================================
  function renderAmpConfig() {
    var prefix = 'amp';
    var state = window.__ampState || { model: '' };
    var html = '<div style="padding:1rem;border-top:1px solid var(--border);">';
    html += renderEndpointApiKeyFields(prefix);
    html += '<div class="form-group"><label data-i18n="cliTools.model"></label>' +
      '<div style="display:flex;gap:0.5rem;">' +
      '<input type="text" id="ampModel" class="form-control" style="flex:1;" data-i18n-placeholder="cliTools.modelPlaceholder" placeholder="provider/model-id" value="' + escapeAttr(state.model) + '" autocomplete="off" />' +
      '<button class="btn btn-outline btn-sm" onclick="ampPickModel()" type="button" data-i18n="cliTools.selectModel"></button></div></div>';
    html += '<div style="margin-top:0.75rem;">';
    html += '<pre class="cli-manual-config-code" id="ampCodeBlock" style="margin-bottom:0.5rem;"></pre>';
    html += '<button class="btn btn-outline btn-sm" onclick="ampCopyCode()" type="button"><i class="fa-regular fa-copy"></i> <span data-i18n="cliTools.copyConfig"></span></button></div>';
    html += '</div>';
    return html;
  }
  window.__ampState = { model: '' };
  window.ampPickModel = function () {
    if (typeof openModelPicker === 'function') {
      window.__cliModelCallback = function (model) {
        document.getElementById('ampModel').value = model;
        window.__ampState.model = model;
        updateAmpCode();
        closeDialog('modelPickerModal');
      };
      openModelPicker();
    }
  };
  function updateAmpCode() {
    var endpoint = getEndpointValue('amp');
    var ak = document.getElementById('amp_ak');
    var apiKey = ak ? ak.value : '';
    var model = window.__ampState.model || 'provider/model-id';
    var code = document.getElementById('ampCodeBlock');
    if (code) code.textContent = 'export OPENAI_API_KEY="' + apiKey + '"\nexport OPENAI_BASE_URL="' + endpoint + '"\namp --model "' + model + '"';
  }
  window.ampCopyCode = function () {
    var code = document.getElementById('ampCodeBlock');
    if (code) copyText(code.textContent).then(function () { toast(t('cliTools.copied'), 'primary'); }).catch(function () {});
  };

  // ================================================================
  // QWEN CODE — guide config
  // ================================================================
  function renderQwenConfig() {
    var prefix = 'qwen';
    var state = window.__qwenState || { model: '' };
    var html = '<div style="padding:1rem;border-top:1px solid var(--border);">';
    html += renderEndpointApiKeyFields(prefix);
    html += '<div class="form-group"><label data-i18n="cliTools.model"></label>' +
      '<div style="display:flex;gap:0.5rem;">' +
      '<input type="text" id="qwenModel" class="form-control" style="flex:1;" data-i18n-placeholder="cliTools.modelPlaceholder" placeholder="provider/model-id" value="' + escapeAttr(state.model) + '" autocomplete="off" />' +
      '<button class="btn btn-outline btn-sm" onclick="qwenPickModel()" type="button" data-i18n="cliTools.selectModel"></button></div></div>';
    html += '<div style="margin-top:0.75rem;">';
    html += '<p style="font-size:0.8rem;color:var(--muted-foreground);margin-bottom:0.5rem;">' + t('cliTools.saveTo') + ' <code>~/.qwen/settings.json</code>:</p>';
    html += '<pre class="cli-manual-config-code" id="qwenCodeBlock" style="margin-bottom:0.5rem;"></pre>';
    html += '<button class="btn btn-outline btn-sm" onclick="qwenCopyCode()" type="button"><i class="fa-regular fa-copy"></i> <span data-i18n="cliTools.copyConfig"></span></button></div>';
    html += '</div>';
    return html;
  }
  window.__qwenState = { model: '' };
  window.qwenPickModel = function () {
    if (typeof openModelPicker === 'function') {
      window.__cliModelCallback = function (model) {
        document.getElementById('qwenModel').value = model;
        window.__qwenState.model = model;
        updateQwenCode();
        closeDialog('modelPickerModal');
      };
      openModelPicker();
    }
  };
  function updateQwenCode() {
    var endpoint = getEndpointValue('qwen');
    var ak = document.getElementById('qwen_ak');
    var apiKey = ak ? ak.value : '';
    var model = window.__qwenState.model || 'provider/model-id';
    var code = document.getElementById('qwenCodeBlock');
    if (code) code.textContent = JSON.stringify({
      security: { auth: { selectedType: 'openai', apiKey: apiKey, baseUrl: endpoint } },
      model: { name: model }
    }, null, 2);
  }
  window.qwenCopyCode = function () {
    var code = document.getElementById('qwenCodeBlock');
    if (code) copyText(code.textContent).then(function () { toast(t('cliTools.copied'), 'primary'); }).catch(function () {});
  };

  // ================================================================
  // CLAUDE COWORK — guide config (MCP plugins not yet supported)
  // ================================================================
  function renderCoworkConfig() {
    var prefix = 'cowork';
    var state = window.__coworkState || { model: '' };
    var html = '<div style="padding:1rem;border-top:1px solid var(--border);">';
    html += renderEndpointApiKeyFields(prefix);
    html += '<div class="form-group"><label data-i18n="cliTools.model"></label>' +
      '<div style="display:flex;gap:0.5rem;">' +
      '<input type="text" id="coworkModel" class="form-control" style="flex:1;" data-i18n-placeholder="cliTools.modelPlaceholder" placeholder="provider/model-id" value="' + escapeAttr(state.model) + '" autocomplete="off" />' +
      '<button class="btn btn-outline btn-sm" onclick="coworkPickModel()" type="button" data-i18n="cliTools.selectModel"></button></div></div>';
    html += '<p style="font-size:0.8rem;color:var(--muted-foreground);margin:0.75rem 0 0;">' + t('cliTools.mcpComingSoon') + '</p>';
    html += '</div>';
    return html;
  }
  window.__coworkState = { model: '' };
  window.coworkPickModel = function () {
    if (typeof openModelPicker === 'function') {
      window.__cliModelCallback = function (model) {
        document.getElementById('coworkModel').value = model;
        window.__coworkState.model = model;
        closeDialog('modelPickerModal');
      };
      openModelPicker();
    }
  };

  // ================================================================
  // MITM SERVER — infrastructure management
  // ================================================================
  function renderMitmServerConfig() {
    var html = '<div style="padding:1rem;border-top:1px solid var(--border);">';
    html += '<div class="form-group"><label data-i18n="cliTools.apiKey">API Key</label>' +
      '<select id="mitm_ak" class="form-control" data-native-select="true">' +
      '<option value="">--</option><option value="custom" data-i18n="cliTools.customUrl"></option></select>' +
      '<input type="text" id="mitm_akCustom" class="form-control hidden" style="margin-top:0.4rem;" data-i18n-placeholder="cliTools.apiKeyPlaceholder" placeholder="sk-..." autocomplete="off" /></div>';
    html += '<div class="form-group"><label data-i18n="cliTools.sudoPassword">sudo password (for DNS + cert)</label>' +
      '<input type="password" id="mitmSudoPass" class="form-control" data-i18n-placeholder="cliTools.sudoPlaceholder" placeholder="sudo password" autocomplete="off" /></div>';
    html += '<div id="mitm_status" class="hidden" style="margin-top:0.75rem;padding:0.5rem 0.75rem;border-radius:6px;font-size:0.8125rem;"></div>';
    html += '<div style="margin-top:1rem;display:flex;gap:0.5rem;flex-wrap:wrap;">' +
      '<button class="btn btn-primary" onclick="mitmStart()" type="button" data-i18n="cliTools.startServer">Start Server</button>' +
      '<button class="btn btn-outline" onclick="mitmStop()" type="button" data-i18n="cliTools.stopServer">Stop Server</button>' +
      '<button class="btn btn-secondary" onclick="mitmRefreshStatus()" type="button" data-i18n="cliTools.refreshStatus">Refresh Status</button>' +
      '</div>';
    html += '<div id="mitmDnsStatus" style="margin-top:0.75rem;font-size:0.8125rem;"></div>';
    html += '</div>';
    return html;
  }
  window.mitmStart = async function () {
    var ak = document.getElementById('mitm_ak');
    var apiKey = '';
    if (ak && ak.value === 'custom') {
      var c = document.getElementById('mitm_akCustom');
      apiKey = c ? c.value.trim() : '';
    } else if (ak && ak.value) {
      apiKey = await getCliApiKey('mitm_ak', 'mitm_akCustom');
    }
    var sudoPass = document.getElementById('mitmSudoPass') ? document.getElementById('mitmSudoPass').value : '';
    if (!apiKey) { showToolStatus('mitm', 'API Key required', 'error'); return; }
    try {
      var res = await api('/cli-tools/mitm/server', { method: 'POST', body: JSON.stringify({ apiKey: apiKey, sudoPassword: sudoPass }) });
      if (res.ok) { showToolStatus('mitm', 'MITM server started', 'success'); mitmRefreshStatus(); }
      else { var d = await res.json().catch(function(){}); showToolStatus('mitm', (d&&d.error)||'Failed to start', 'error'); }
    } catch(e) { showToolStatus('mitm', e.message, 'error'); }
  };
  window.mitmStop = async function () {
    try {
      var res = await api('/cli-tools/mitm/server', { method: 'DELETE' });
      if (res.ok) { showToolStatus('mitm', 'MITM server stopped', 'success'); mitmRefreshStatus(); }
    } catch(e) { showToolStatus('mitm', e.message, 'error'); }
  };
  window.mitmRefreshStatus = async function () {
    try {
      var res = await api('/cli-tools/mitm/status');
      if (res.ok) {
        var d = await res.json();
        var el = document.getElementById('mitmDnsStatus');
        if (el) {
          var html = '<strong>Server:</strong> ' + (d.running ? '<span style="color:#16a34a;">Running</span>' : '<span style="color:#dc2626;">Stopped</span>');
          html += '<br><strong>DNS:</strong> ';
          var tools = { antigravity: 'Antigravity', copilot: 'Copilot', kiro: 'Kiro' };
          for (var t in tools) {
            html += '<span style="margin-right:0.5rem;">' + tools[t] + ': ' + (d.dns && d.dns[t] ? '<span style="color:#16a34a;">ON</span>' : '<span style="color:#dc2626;">OFF</span>') + '</span>';
          }
          el.innerHTML = html;
        }
      }
    } catch(e) {}
  };

  // ================================================================
  // ANTIGRAVITY — MITM tool with model aliases
  // ================================================================
  function renderAntigravityConfig() {
    var prefix = 'antigravity';
    var state = window.__antigravityState || { models: {} };
    var aliasKeys = ['gemini-3-flash-agent','gemini-3.5-flash-low','gemini-3.5-flash-extra-low','gemini-pro-agent','gemini-3.1-pro-low','claude-opus-4-6-thinking','claude-sonnet-4-6','gpt-oss-120b-medium'];
    var html = '<div style="padding:1rem;border-top:1px solid var(--border);">';
    html += '<div class="form-group"><label data-i18n="cliTools.apiKey">API Key</label>' +
      '<select id="antigravity_ak" class="form-control" data-native-select="true">' +
      '<option value="">--</option><option value="custom" data-i18n="cliTools.customUrl"></option></select>' +
      '<input type="text" id="antigravity_akCustom" class="form-control hidden" style="margin-top:0.4rem;" data-i18n-placeholder="cliTools.apiKeyPlaceholder" placeholder="sk-..." autocomplete="off" /></div>';
    html += '<div class="form-group"><label data-i18n="cliTools.modelMappings">Model Mappings</label>';
    aliasKeys.forEach(function (k, idx) {
      var val = state.models[k] || '';
      html += '<div style="display:flex;gap:0.5rem;align-items:center;margin-top:' + (idx > 0 ? '0.35rem' : '0.35rem') + ';">' +
        '<span style="font-size:0.75rem;font-weight:600;color:var(--muted-foreground);min-width:7rem;">' + escapeHtml(k) + '</span>' +
        '<input type="text" class="form-control ag-alias-input" style="flex:1;" data-i18n-placeholder="cliTools.modelPlaceholder" placeholder="provider/model-id" value="' + escapeAttr(val) + '" autocomplete="off" data-alias="' + escapeAttr(k) + '" />' +
        (val ? '<button class="btn btn-outline btn-sm ag-alias-clear" type="button" data-alias="' + escapeAttr(k) + '" style="padding:0.25rem 0.5rem;" title="Clear">&times;</button>' : '') +
        '<button class="btn btn-outline btn-sm ag-alias-select" type="button" data-alias="' + escapeAttr(k) + '" data-i18n="cliTools.selectModel"></button>' +
        '</div>';
    });
    html += '</div>';
    html += '<div id="' + prefix + '_status" class="hidden" style="margin-top:0.75rem;padding:0.5rem 0.75rem;border-radius:6px;font-size:0.8125rem;"></div>';
    html += '<div style="margin-top:1rem;display:flex;gap:0.5rem;flex-wrap:wrap;">' +
      '<button class="btn btn-primary" onclick="antigravityApply()" type="button" data-i18n="cliTools.apply"></button>' +
      '<button class="btn btn-secondary" onclick="antigravityShowManual()" type="button" data-i18n="cliTools.manualConfig"></button>' +
      '<button class="btn btn-outline" onclick="antigravityToggleDns()" type="button" data-i18n="cliTools.toggleDns">Toggle DNS</button>' +
      '</div></div>';
    return html;
  }
  window.__antigravityState = { models: {} };
  window.antigravityApply = async function () {
    var prefix = 'antigravity';
    var s = window.__antigravityState;
    document.querySelectorAll('.ag-alias-input').forEach(function (inp) {
      s.models[inp.dataset.alias] = inp.value;
    });
    try {
      var res = await api('/cli-tools/mitm/aliases', { method: 'PUT', body: JSON.stringify({ tool: 'antigravity', mappings: s.models }) });
      if (res.ok) { showToolStatus(prefix, 'Aliases saved', 'success'); updateCliBadge('antigravity', true); }
      else { showToolStatus(prefix, 'Failed to save', 'error'); }
    } catch(e) { showToolStatus(prefix, e.message, 'error'); }
  };
  window.antigravityShowManual = function () {
    var s = window.__antigravityState;
    showManualConfigModal([{ filename: t('cliTools.modelMappings'), content: JSON.stringify(s.models, null, 2) }]);
  };
  window.antigravityToggleDns = async function () {
    var el = document.getElementById('antigravity_status');
    if (el) { el.className = 'cli-tool-status-success'; el.textContent = 'Toggling DNS...'; el.classList.remove('hidden'); }
    try {
      var statusRes = await api('/cli-tools/mitm/status');
      var dnsOn = false;
      if (statusRes.ok) { var st = await statusRes.json(); dnsOn = st.dns && st.dns.antigravity; }
      var res = await api('/cli-tools/mitm/dns', { method: 'PATCH', body: JSON.stringify({ tool: 'antigravity', action: dnsOn ? 'disable' : 'enable', sudoPassword: '' }) });
      if (res.ok) { showToolStatus('antigravity', 'DNS ' + (dnsOn ? 'disabled' : 'enabled'), 'success'); }
    } catch(e) { showToolStatus('antigravity', e.message, 'error'); }
  };

  // ================================================================
  // COPILOT — MITM tool with endpoint + API key + models
  // ================================================================
  function renderCopilotConfig() {
    var prefix = 'copilot';
    var state = window.__copilotState || { models: [] };
    var html = '<div style="padding:1rem;border-top:1px solid var(--border);">';
    html += renderEndpointApiKeyFields(prefix);
    html += '<div class="form-group"><label data-i18n="cliTools.model"></label>' +
      '<div style="display:flex;flex-wrap:wrap;gap:0.4rem;padding:0.5rem;border:1px solid var(--border);border-radius:6px;margin-bottom:0.5rem;min-height:2rem;">';
    if (state.models.length === 0) html += '<span style="font-size:0.8rem;color:var(--muted-foreground);" data-i18n="cliTools.noModels">No models selected.</span>';
    state.models.forEach(function (m) {
      html += '<span style="display:inline-flex;align-items:center;gap:0.25rem;padding:0.15rem 0.5rem;font-size:0.75rem;border-radius:4px;background:var(--muted);color:var(--foreground);border:1px solid var(--border);">' + escapeHtml(m) +
        '<i class="fa-solid fa-xmark" style="cursor:pointer;margin-left:0.2rem;font-size:0.65rem;" onclick="copilotRemoveModel(\'' + escapeAttr(m) + '\')"></i></span>';
    });
    html += '</div><div style="display:flex;gap:0.5rem;margin-bottom:0.5rem;">' +
      '<input type="text" id="copilotNewModel" class="form-control" style="flex:1;" placeholder="Type model then press Add" autocomplete="off" />' +
      '<button class="btn btn-outline btn-sm" onclick="copilotAddModel()" type="button" data-i18n="cliTools.addModel">Add Model</button>' +
      '<button class="btn btn-outline btn-sm" onclick="copilotSelectModel()" type="button" data-i18n="cliTools.selectModel">Select Model</button></div></div>';
    html += '<div id="' + prefix + '_status" class="hidden" style="margin-top:0.75rem;padding:0.5rem 0.75rem;border-radius:6px;font-size:0.8125rem;"></div>';
    html += '<div style="margin-top:1rem;display:flex;gap:0.5rem;flex-wrap:wrap;">' +
      '<button class="btn btn-primary" onclick="copilotApply()" type="button" data-i18n="cliTools.apply"></button>' +
      '<button class="btn btn-outline" onclick="copilotReset()" type="button" data-i18n="cliTools.reset"></button>' +
      '<button class="btn btn-secondary" onclick="copilotToggleDns()" type="button" data-i18n="cliTools.toggleDns">Toggle DNS</button>' +
      '</div></div>';
    return html;
  }
  window.__copilotState = { models: [] };
  window.copilotAddModel = function () {
    var inp = document.getElementById('copilotNewModel');
    if (!inp || !inp.value.trim()) return;
    var s = window.__copilotState;
    var m = inp.value.trim();
    if (s.models.indexOf(m) === -1) s.models.push(m);
    inp.value = '';
    reRenderDetailBody();
  };
  window.copilotRemoveModel = function (model) {
    window.__copilotState.models = window.__copilotState.models.filter(function (m) { return m !== model; });
    reRenderDetailBody();
  };
  window.copilotSelectModel = function () {
    window.__cliModelCallback = function (model) {
      var s = window.__copilotState;
      if (s.models.indexOf(model) === -1) { s.models.push(model); }
      reRenderDetailBody();
      closeDialog('modelPickerModal');
    };
    openModelPicker();
  };
  window.copilotApply = async function () {
    var prefix = 'copilot';
    populateAkSelect(prefix);
    var endpoint = getEndpointValue(prefix);
    if (!endpoint) { showToolStatus(prefix, 'Please select an endpoint', 'error'); return; }
    var apiKey = await getCliApiKey(prefix + '_ak', prefix + '_akCustom');
    var models = window.__copilotState.models || [];
    var applyBtn = document.querySelector('.cli-tool-detail-body .btn-primary');
    if (applyBtn) { applyBtn.disabled = true; applyBtn.textContent = t('cliTools.applying'); }
    try {
      var res = await api('/cli-tools/copilot-settings', { method: 'POST', body: JSON.stringify({ baseUrl: endpoint, apiKey: apiKey || null, models: models }) });
      if (res.ok) { showToolStatus(prefix, t('cliTools.configSuccess'), 'success'); updateCliBadge('copilot', true); }
      else { var d = await res.json().catch(function(){}); showToolStatus(prefix, (d&&d.error)||t('cliTools.configFailed'), 'error'); }
    } catch(e) { showToolStatus(prefix, e.message||t('cliTools.configFailed'), 'error'); }
    finally { if (applyBtn) { applyBtn.disabled = false; applyBtn.textContent = t('cliTools.apply'); } }
  };
  window.copilotReset = async function () {
    var confirmed = await confirmAction('Reset Copilot configuration?', { confirmText: 'Reset', variant: 'danger' });
    if (!confirmed) return;
    try { var res = await api('/cli-tools/copilot-settings', { method: 'DELETE' }); if (res.ok) { showToolStatus('copilot', 'Configuration reset', 'success'); updateCliBadge('copilot', false); } } catch(e) {}
  };
  window.copilotToggleDns = async function () {
    try {
      var statusRes = await api('/cli-tools/mitm/status');
      var dnsOn = false;
      if (statusRes.ok) { var st = await statusRes.json(); dnsOn = st.dns && st.dns.copilot; }
      var res = await api('/cli-tools/mitm/dns', { method: 'PATCH', body: JSON.stringify({ tool: 'copilot', action: dnsOn ? 'disable' : 'enable', sudoPassword: '' }) });
      if (res.ok) { showToolStatus('copilot', 'DNS ' + (dnsOn ? 'disabled' : 'enabled'), 'success'); }
    } catch(e) { showToolStatus('copilot', e.message, 'error'); }
  };

  // ================================================================
  // KIRO — MITM tool with model mappings
  // ================================================================
  function renderKiroConfig() {
    var prefix = 'kiro';
    var state = window.__kiroState || { models: {} };
    var mappingKeys = Object.keys(state.models);
    var html = '<div style="padding:1rem;border-top:1px solid var(--border);">';
    html += '<div class="form-group"><label data-i18n="cliTools.modelMappings">Model Mappings</label>' +
      '<div style="font-size:0.75rem;color:var(--muted-foreground);margin-bottom:0.5rem;">' + t('cliTools.mappingDesc') + '</div>';
    html += '<div id="kiroMappings">';
    if (mappingKeys.length === 0) {
      html += '<span style="font-size:0.8rem;color:var(--muted-foreground);" data-i18n="cliTools.noMappings">No mappings yet. Add one below.</span>';
    } else {
      mappingKeys.forEach(function (k, idx) {
        html += '<div style="display:flex;gap:0.5rem;align-items:center;margin-top:' + (idx > 0 ? '0.35rem' : '0') + ';" class="kiro-mapping-row">' +
          '<input type="text" class="form-control kiro-from" style="flex:1;" data-i18n-placeholder="cliTools.cwModelPlaceholder" placeholder="CodeWhisperer model ID" value="' + escapeAttr(k) + '" autocomplete="off" />' +
          '<span style="font-size:0.75rem;color:var(--muted-foreground);">&rarr;</span>' +
          '<input type="text" class="form-control kiro-to" style="flex:1;" data-i18n-placeholder="cliTools.skModelPlaceholder" placeholder="OmniProxy model" value="' + escapeAttr(state.models[k]) + '" autocomplete="off" />' +
          '<button class="btn btn-outline btn-sm" onclick="kiroSelectModel(this)" type="button" data-i18n="cliTools.selectModel">Select</button>' +
          '<button class="btn btn-outline btn-sm" onclick="kiroRemoveMapping(this)" type="button" style="padding:0.25rem 0.5rem;">&times;</button>' +
          '</div>';
      });
    }
    html += '</div>';
    html += '<div style="display:flex;gap:0.5rem;margin-top:0.5rem;">' +
      '<button class="btn btn-outline btn-sm" onclick="kiroAddMapping()" type="button" data-i18n="cliTools.addMapping">+ Add Mapping</button></div></div>';
    html += '<div id="' + prefix + '_status" class="hidden" style="margin-top:0.75rem;padding:0.5rem 0.75rem;border-radius:6px;font-size:0.8125rem;"></div>';
    html += '<div style="margin-top:1rem;display:flex;gap:0.5rem;flex-wrap:wrap;">' +
      '<button class="btn btn-primary" onclick="kiroApply()" type="button" data-i18n="cliTools.apply"></button>' +
      '<button class="btn btn-secondary" onclick="kiroToggleDns()" type="button" data-i18n="cliTools.toggleDns">Toggle DNS</button>' +
      '</div></div>';
    return html;
  }
  window.__kiroState = { models: {} };
  window.kiroAddMapping = function () {
    var container = document.getElementById('kiroMappings');
    if (!container) return;
    var empty = container.querySelector('.kiro-mapping-row');
    if (!empty || empty.querySelector('.kiro-from').value !== '' || empty.querySelector('.kiro-to').value !== '') {
      var div = document.createElement('div');
      div.className = 'kiro-mapping-row';
      div.style.cssText = 'display:flex;gap:0.5rem;align-items:center;margin-top:0.35rem;';
      div.innerHTML = '<input type="text" class="form-control kiro-from" style="flex:1;" data-i18n-placeholder="cliTools.cwModelPlaceholder" placeholder="CodeWhisperer model ID" autocomplete="off" />' +
        '<span style="font-size:0.75rem;color:var(--muted-foreground);">&rarr;</span>' +
        '<input type="text" class="form-control kiro-to" style="flex:1;" data-i18n-placeholder="cliTools.skModelPlaceholder" placeholder="OmniProxy model" autocomplete="off" />' +
        '<button class="btn btn-outline btn-sm" onclick="kiroSelectModel(this)" type="button" data-i18n="cliTools.selectModel">Select</button>' +
        '<button class="btn btn-outline btn-sm" onclick="kiroRemoveMapping(this)" type="button" style="padding:0.25rem 0.5rem;">&times;</button>';
      container.appendChild(div);
    }
  };
  window.kiroRemoveMapping = function (btn) {
    var row = btn.closest('.kiro-mapping-row');
    if (row) row.remove();
  };
  window.kiroSelectModel = function (btn) {
    var row = btn.closest('.kiro-mapping-row');
    window.__cliModelCallback = function (model) {
      if (row) {
        var to = row.querySelector('.kiro-to');
        if (to) to.value = model;
      }
      closeDialog('modelPickerModal');
    };
    openModelPicker();
  };
  window.kiroApply = async function () {
    var prefix = 'kiro';
    var mappings = {};
    document.querySelectorAll('.kiro-mapping-row').forEach(function (row) {
      var from = row.querySelector('.kiro-from');
      var to = row.querySelector('.kiro-to');
      if (from && to && from.value.trim() && to.value.trim()) {
        mappings[from.value.trim()] = to.value.trim();
      }
    });
    window.__kiroState.models = mappings;
    try {
      var res = await api('/cli-tools/mitm/aliases', { method: 'PUT', body: JSON.stringify({ tool: 'kiro', mappings: mappings }) });
      if (res.ok) { showToolStatus(prefix, 'Mappings saved', 'success'); updateCliBadge('kiro', true); }
      else { showToolStatus(prefix, 'Failed to save', 'error'); }
    } catch(e) { showToolStatus(prefix, e.message, 'error'); }
  };
  window.kiroToggleDns = async function () {
    try {
      var statusRes = await api('/cli-tools/mitm/status');
      var dnsOn = false;
      if (statusRes.ok) { var st = await statusRes.json(); dnsOn = st.dns && st.dns.kiro; }
      var res = await api('/cli-tools/mitm/dns', { method: 'PATCH', body: JSON.stringify({ tool: 'kiro', action: dnsOn ? 'disable' : 'enable', sudoPassword: '' }) });
      if (res.ok) { showToolStatus('kiro', 'DNS ' + (dnsOn ? 'disabled' : 'enabled'), 'success'); }
    } catch(e) { showToolStatus('kiro', e.message, 'error'); }
  };

  // ================================================================
  // CLI TOOLS — Grid + Detail view
  // ================================================================
  var CLI_TOOL_META = [
    { id: 'claude', nameKey: 'cliTools.toolClaude', descKey: 'cliTools.descClaude', icon: 'claude.png', render: renderClaudeConfig },
    { id: 'opencode', nameKey: 'cliTools.toolOpenCode', descKey: 'cliTools.descOpenCode', icon: 'opencode.png', render: renderOpenCodeConfig },
    { id: 'cline', nameKey: 'cliTools.toolCline', descKey: 'cliTools.descCline', icon: 'cline.png', render: renderClineConfig },
    { id: 'codex', nameKey: 'cliTools.toolCodex', descKey: 'cliTools.descCodex', icon: 'codex.png', render: renderCodexConfig },
    { id: 'kilo', nameKey: 'cliTools.toolKiloCode', descKey: 'cliTools.descKiloCode', icon: 'kilocode.png', render: renderKiloConfig },
    { id: 'continue', nameKey: 'cliTools.toolContinue', descKey: 'cliTools.descContinue', icon: 'continue.png', render: renderContinueConfig },
    { id: 'roo', nameKey: 'cliTools.toolRoo', descKey: 'cliTools.descRoo', icon: 'roo.png', render: renderRooConfig },
    { id: 'deepseek', nameKey: 'cliTools.toolDeepSeek', descKey: 'cliTools.descDeepSeek', icon: 'deepseek.png', render: renderDeepSeekConfig },
    { id: 'jcode', nameKey: 'cliTools.toolJcode', descKey: 'cliTools.descJcode', icon: 'jcode.png', render: renderJcodeConfig },
    { id: 'hermes', nameKey: 'cliTools.toolHermes', descKey: 'cliTools.descHermes', icon: 'hermes.png', render: renderHermesConfig },
    { id: 'droid', nameKey: 'cliTools.toolDroid', descKey: 'cliTools.descDroid', icon: 'droid.png', render: renderDroidConfig },
    { id: 'openclaw', nameKey: 'cliTools.toolOpenClaw', descKey: 'cliTools.descOpenClaw', icon: 'openclaw.png', render: renderOpenClawConfig },
    { id: 'cursor', nameKey: 'cliTools.toolCursor', descKey: 'cliTools.descCursor', icon: 'cursor.png', render: renderCursorConfig },
    { id: 'amp', nameKey: 'cliTools.toolAmp', descKey: 'cliTools.descAmp', icon: 'amp.png', render: renderAmpConfig },
    { id: 'qwen', nameKey: 'cliTools.toolQwen', descKey: 'cliTools.descQwen', icon: 'qwen.png', render: renderQwenConfig },
    { id: 'cowork', nameKey: 'cliTools.toolCowork', descKey: 'cliTools.descCowork', icon: 'fa-people-arrows', render: renderCoworkConfig },
    // MITM tools
    { id: 'mitm', nameKey: 'cliTools.toolMitmServer', descKey: 'cliTools.descMitmServer', icon: 'fa-server', render: renderMitmServerConfig },
    { id: 'antigravity', nameKey: 'cliTools.toolAntigravity', descKey: 'cliTools.descAntigravity', icon: 'antigravity.png', render: renderAntigravityConfig },
    { id: 'copilot', nameKey: 'cliTools.toolCopilot', descKey: 'cliTools.descCopilot', icon: 'copilot.png', render: renderCopilotConfig },
    { id: 'kiro', nameKey: 'cliTools.toolKiro', descKey: 'cliTools.descKiro', icon: 'kiro.png', render: renderKiroConfig }
  ];

  var cliToolStatuses = {};

  function getCliToolStatus(toolId) {
    var s = cliToolStatuses[toolId];
    if (!s) return 'unknown';
    if (!s.installed) return 'not_installed';
    if (!s.hasOmniProxy) return 'not_configured';
    return 'connected';
  }

  async function loadCliToolStatus() {
    CLI_TOOL_META.forEach(function(m) { delete cliToolStatuses[m.id]; });
    var grid = $('cliToolsGrid');
    if (grid) renderCliToolGrid(grid);
    try {
      var res = await api('/cli-tools/status');
      var data = await res.json();
      for (var k in data) {
        if (data.hasOwnProperty(k)) {
          cliToolStatuses[k] = { installed: !!data[k].installed, hasOmniProxy: !!data[k].hasOmniProxy };
        }
      }
    } catch (e) {}
    if (grid) renderCliToolGrid(grid);
  }

  function updateCliBadge(toolId, connected) {
    var existing = cliToolStatuses[toolId] || { installed: true };
    existing.hasOmniProxy = connected;
    cliToolStatuses[toolId] = existing;
    var card = document.querySelector('.cli-tool-card[data-cli-tool="' + toolId + '"]');
    if (!card) return;
    var badge = card.querySelector('.cli-tool-status');
    if (badge) {
      var status = getCliToolStatus(toolId);
      badge.className = 'cli-tool-status ' + (status === 'connected' ? 'cli-tool-status-connected' : 'cli-tool-status-notconfigured');
      badge.textContent = t(status === 'connected' ? 'cliTools.connected' : 'cliTools.notConfigured');
    }
  }

  function renderCliTools() {
    var grid = $('cliToolsGrid');
    var detail = $('cliToolDetail');
    if (!grid || !detail) return;

    if (cliToolDetailId) {
      grid.style.display = 'none';
      detail.classList.remove('hidden');
      renderCliToolDetail(detail, cliToolDetailId);
    } else {
      grid.style.display = '';
      detail.classList.add('hidden');
      renderCliToolGrid(grid);
    }
  }

  function renderToolIcon(icon) {
    if (icon.startsWith('fa-')) return '<i class="fa-solid ' + icon + '"></i>';
    return '<img src="/admin/clitools/' + icon + '" alt="" style="width:1.25rem;height:1.25rem;object-fit:contain;">';
  }

  function renderCliToolGrid(grid) {
    grid.innerHTML = '';
    CLI_TOOL_META.forEach(function (meta) {
      var card = document.createElement('div');
      card.className = 'cli-tool-card';
      card.dataset.cliTool = meta.id;
      var status = getCliToolStatus(meta.id);
      var statusClass, statusLabel;
      switch (status) {
        case 'connected': statusClass = 'cli-tool-status-connected'; statusLabel = t('cliTools.connected'); break;
        case 'not_configured': statusClass = 'cli-tool-status-notconfigured'; statusLabel = t('cliTools.notConfigured'); break;
        case 'not_installed': statusClass = 'cli-tool-status-notinstalled'; statusLabel = t('cliTools.notInstalled'); break;
        default: statusClass = 'cli-tool-status-unknown'; statusLabel = t('cliTools.unknown');
      }
      card.innerHTML =
        '<span class="cli-tool-icon">' + renderToolIcon(meta.icon) + '</span>' +
        '<div class="cli-tool-info">' +
        '<div class="cli-tool-name" data-i18n="' + meta.nameKey + '"></div>' +
        '<div class="cli-tool-desc" data-i18n="' + meta.descKey + '"></div>' +
        '</div>' +
        '<span class="cli-tool-status ' + statusClass + '">' + statusLabel + '</span>';
      card.addEventListener('click', function () {
        cliToolDetailId = meta.id;
        renderCliTools();
      });
      grid.appendChild(card);
    });
    applyTranslations();
  }

  async function renderCliToolDetail(detail, toolId) {
    var meta = CLI_TOOL_META.find(function (m) { return m.id === toolId; });
    if (!meta) { cliToolDetailId = null; renderCliTools(); return; }

    detail.innerHTML =
      '<div class="cli-tool-detail-back" onclick="window.backToCliTools()">' +
      '<i class="fa-solid fa-arrow-left"></i> ' + escapeHtml(t('common.back')) +
      '</div>' +
      '<div class="cli-tool-detail-header" style="display:flex;align-items:center;gap:0.75rem;">' +
      '<span class="cli-tool-icon">' + renderToolIcon(meta.icon) + '</span>' +
      '<div><h2>' + escapeHtml(t(meta.nameKey)) + '</h2>' +
      '<p class="cli-tool-detail-desc">' + escapeHtml(t(meta.descKey)) + '</p></div>' +
      '</div>' +
      '<div class="cli-tool-detail-body">' +
      meta.render() +
      '</div>';

    applyTranslations();

    if (toolId === 'claude') bindClaudeEvents();
    else if (toolId === 'opencode') bindOcEvents();
    else if (toolId === 'cline') bindClineEvents();
    else if (toolId === 'codex') bindCodexEvents();
    else if (toolId === 'kilo') bindKiloEvents();
    else if (toolId === 'continue') bindContinueEvents();
    else if (toolId === 'roo') bindRooEvents();
    else if (toolId === 'deepseek') bindDeepSeekEvents();
    else if (toolId === 'jcode') bindJcodeEvents();
    else if (toolId === 'hermes') bindHermesEvents();
    else if (toolId === 'droid') bindDroidEvents();
    else if (toolId === 'openclaw') bindOpenClawEvents();
    else if (toolId === 'cursor') bindCursorEvents();
    else if (toolId === 'amp') bindAmpEvents();
    else if (toolId === 'qwen') bindQwenEvents();
    else if (toolId === 'cowork') bindCoworkEvents();
    else if (toolId === 'mitm') bindMitmEvents();
    else if (toolId === 'antigravity') bindAntigravityEvents();
    else if (toolId === 'copilot') bindCopilotEvents();
    else if (toolId === 'kiro') bindKiroEvents();

    populateAkSelect(toolId);
    if (apiKeysCache.length === 0) { await loadApiKeys(); populateAkSelect(toolId); }
    loadCliToolSettings(toolId);
  }

  function populateEndpointField(prefix, baseUrl) {
    if (!baseUrl) return;
    var ep = $(prefix + '_ep');
    if (!ep) return;
    var localUrl = ep.options[0].textContent;
    var norm = function(u) { return u.replace(/\/+$/, '').replace(/\/v1$/, ''); };
    if (norm(baseUrl) === norm(localUrl)) {
      ep.value = 'local';
    } else {
      ep.value = 'custom';
      var c = $(prefix + '_epCustom');
      if (c) { c.value = baseUrl; c.classList.remove('hidden'); }
    }
  }

  function populateApiKeyField(prefix, apiKey) {
    if (!apiKey) return;
    var ak = $(prefix + '_ak');
    if (!ak) return;
    ak.value = 'custom';
    var c = $(prefix + '_akCustom');
    if (c) { c.value = apiKey; c.classList.remove('hidden'); }
  }

  async function loadCliToolSettings(toolId) {
    var res = await api('/cli-tools/' + toolId);
    if (!res.ok) return;
    var s = await res.json();
    if (!s || s.error) return;

    var prefix = toolId;
    var needsReRender = false;

    switch (toolId) {
      case 'claude':
        // Restore what is already on disk, or Apply would overwrite the
        // operator's model and effort with this form's defaults.
        if (s.model || s.reasoningEffort) {
          var cls = window.__claudeState || { model: '', reasoningEffort: 'high' };
          if (s.model) cls.model = s.model;
          if (s.reasoningEffort) cls.reasoningEffort = s.reasoningEffort;
          window.__claudeState = cls;
          needsReRender = true;
        }
        break;
      case 'opencode':
        if (s.models || s.activeModel || s.subagentModel) {
          var st = window.__openCodeState || { models: [], activeModel: '', subagentModel: '' };
          if (s.models) st.models = s.models;
          if (s.activeModel) st.activeModel = s.activeModel;
          if (s.subagentModel) st.subagentModel = s.subagentModel;
          window.__openCodeState = st;
          needsReRender = true;
        }
        break;
      case 'copilot':
        if (s.models) {
          var cps = window.__copilotState || { models: [] };
          cps.models = s.models;
          window.__copilotState = cps;
          needsReRender = true;
        }
        break;
      case 'codex':
        if (s.model) {
          var cs = window.__codexState || {};
          cs.model = s.model;
          if (s.subagentModel) cs.subagentModel = s.subagentModel;
          window.__codexState = cs;
          needsReRender = true;
        }
        break;
      case 'cline':
        if (s.model) {
          if (!window.__clineState) window.__clineState = { model: '' };
          window.__clineState.model = s.model;
          var inp = document.getElementById('clineModel');
          if (inp) inp.value = s.model;
        }
        break;
      case 'deepseek':
        if (s.model) {
          var inp = document.getElementById('deepseekModel');
          if (inp) inp.value = s.model;
        }
        break;
      case 'kilo':
        if (s.model) {
          var inp = document.getElementById('kiloModel');
          if (inp) inp.value = s.model;
        }
        break;
      case 'hermes':
        if (s.model) {
          var inp = document.getElementById('hermesModel');
          if (inp) inp.value = s.model;
        }
        break;
      case 'jcode':
        if (s.models) {
          if (!window.__jcodeState) window.__jcodeState = { models: [] };
          window.__jcodeState.models = s.models;
          needsReRender = true;
        }
        break;
      case 'droid':
        if (s.models || s.activeModel) {
          if (!window.__droidState) window.__droidState = { models: [], activeModel: '', subagentModel: '' };
          if (s.models) window.__droidState.models = s.models;
          if (s.activeModel) window.__droidState.activeModel = s.activeModel;
          needsReRender = true;
        }
        break;
      case 'openclaw':
        if (s.model || s.agentModels) {
          if (!window.__openClawState) window.__openClawState = { model: '', agentModels: {} };
          if (s.model) window.__openClawState.model = s.model;
          if (s.agentModels) window.__openClawState.agentModels = s.agentModels;
          needsReRender = true;
        }
        break;
    }

    if (s.config) {
      if (toolId === 'codex') {
        var modelMatch = s.config.match(/^model\s*=\s*"([^"]+)"/m);
        if (modelMatch && !window.__codexState?.model) {
          if (!window.__codexState) window.__codexState = {};
          window.__codexState.model = modelMatch[1];
          needsReRender = true;
        }
        var subMatch = s.config.match(/\[agents\.subagent\]\s*\n\s*model\s*=\s*"([^"]+)"/m);
        if (subMatch && !window.__codexState?.subagentModel) {
          if (!window.__codexState) window.__codexState = {};
          window.__codexState.subagentModel = subMatch[1];
          needsReRender = true;
        }
      } else if (toolId === 'deepseek') {
        var urlMatch = s.config.match(/base_url\s*=\s*"([^"]+)"/);
        if (urlMatch && !s.baseUrl) s.baseUrl = urlMatch[1];
        var dsModel = s.config.match(/model\s*=\s*"([^"]+)"/m);
        if (dsModel && !s.model) {
          var inp = document.getElementById('deepseekModel');
          if (inp) inp.value = dsModel[1];
        }
      } else if (toolId === 'jcode') {
        var urlMatch = s.config.match(/base_url\s*=\s*"([^"]+)"/);
        if (urlMatch && !s.baseUrl) s.baseUrl = urlMatch[1];
      }
    }

    populateEndpointField(prefix, s.baseUrl);
    populateApiKeyField(prefix, s.apiKey);

    if (needsReRender) {
      reRenderDetailBody();
      populateEndpointField(prefix, s.baseUrl);
      populateApiKeyField(prefix, s.apiKey);
    }
  }

  window.backToCliTools = function () {
    cliToolDetailId = null;
    renderCliTools();
  };

  function reRenderDetailBody() {
    if (!cliToolDetailId) return;
    var meta = CLI_TOOL_META.find(function (m) { return m.id === cliToolDetailId; });
    if (!meta) return;
    var body = document.querySelector('.cli-tool-detail-body');
    if (!body) return;
    body.innerHTML = meta.render();
    applyTranslations();
    var toolId = cliToolDetailId;
    if (toolId === 'claude') bindClaudeEvents();
    else if (toolId === 'opencode') bindOcEvents();
    else if (toolId === 'cline') bindClineEvents();
    else if (toolId === 'codex') bindCodexEvents();
    else if (toolId === 'kilo') bindKiloEvents();
    else if (toolId === 'continue') bindContinueEvents();
    else if (toolId === 'roo') bindRooEvents();
    else if (toolId === 'deepseek') bindDeepSeekEvents();
    else if (toolId === 'jcode') bindJcodeEvents();
    else if (toolId === 'hermes') bindHermesEvents();
    else if (toolId === 'droid') bindDroidEvents();
    else if (toolId === 'openclaw') bindOpenClawEvents();
    else if (toolId === 'cursor') bindCursorEvents();
    else if (toolId === 'amp') bindAmpEvents();
    else if (toolId === 'qwen') bindQwenEvents();
    else if (toolId === 'cowork') bindCoworkEvents();
    else if (toolId === 'mitm') bindMitmEvents();
    else if (toolId === 'antigravity') bindAntigravityEvents();
    else if (toolId === 'copilot') bindCopilotEvents();
    else if (toolId === 'kiro') bindKiroEvents();
    populateAkSelect(cliToolDetailId);
  }

  function bindClaudeEvents() {
    bindEndpointApiKeyEvents('claude');
    var m = $('claudeModel');
    if (m) m.addEventListener('input', function () { window.__claudeState.model = this.value; });
  }
  function bindClineEvents() {
    bindEndpointApiKeyEvents('cline');
    var m = document.getElementById('clineModel');
    if (m) m.addEventListener('input', function () { window.__clineState.model = this.value; });
  }
  function bindCodexEvents() {
    bindEndpointApiKeyEvents('codex');
    var m = document.getElementById('codexModel');
    if (m) m.addEventListener('input', function () { window.__codexState.model = this.value; });
    var s = document.getElementById('codexSubagent');
    if (s) s.addEventListener('input', function () { window.__codexState.subagentModel = this.value; });
  }
  function bindKiloEvents() {
    bindEndpointApiKeyEvents('kilo');
    var m = document.getElementById('kiloModel');
    if (m) m.addEventListener('input', function () { window.__kiloState.model = this.value; });
  }
  function bindContinueEvents() {
    bindEndpointApiKeyEvents('continue');
    var m = document.getElementById('continueModel');
    if (m) m.addEventListener('input', function () { window.__continueState.model = this.value; updateContinueCode(); });
    var ak = document.getElementById('continue_ak');
    if (ak) ak.addEventListener('change', function () { window.__continueState.apiKey = this.value; updateContinueCode(); });
    var akC = document.getElementById('continue_akCustom');
    if (akC) akC.addEventListener('input', function () { window.__continueState.apiKey = this.value; updateContinueCode(); });
    updateContinueCode();
  }
  function bindRooEvents() {
    bindEndpointApiKeyEvents('roo');
    var m = document.getElementById('rooModel');
    if (m) m.addEventListener('input', function () { window.__rooState.model = this.value; });
  }
  function bindDeepSeekEvents() {
    bindEndpointApiKeyEvents('deepseek');
    var m = document.getElementById('deepseekModel');
    if (m) m.addEventListener('input', function () { window.__deepSeekState.model = this.value; });
  }
  function bindJcodeEvents() {
    bindEndpointApiKeyEvents('jcode');
  }
  function bindHermesEvents() {
    bindEndpointApiKeyEvents('hermes');
    var m = document.getElementById('hermesModel');
    if (m) m.addEventListener('input', function () { window.__hermesState.model = this.value; });
  }
  function bindDroidEvents() {
    bindEndpointApiKeyEvents('droid');
  }
  function bindOpenClawEvents() {
    bindEndpointApiKeyEvents('openclaw');
    var m = document.getElementById('openclawModel');
    if (m) m.addEventListener('input', function () { window.__openClawState.model = this.value; });
  }
  function bindCursorEvents() {
    bindEndpointApiKeyEvents('cursor');
  }
  function bindAmpEvents() {
    bindEndpointApiKeyEvents('amp');
    var m = document.getElementById('ampModel');
    if (m) m.addEventListener('input', function () { window.__ampState.model = this.value; updateAmpCode(); });
    var ak = document.getElementById('amp_ak');
    if (ak) ak.addEventListener('change', function () { updateAmpCode(); });
    var akC = document.getElementById('amp_akCustom');
    if (akC) akC.addEventListener('input', function () { updateAmpCode(); });
    updateAmpCode();
  }
  function bindQwenEvents() {
    bindEndpointApiKeyEvents('qwen');
    var m = document.getElementById('qwenModel');
    if (m) m.addEventListener('input', function () { window.__qwenState.model = this.value; updateQwenCode(); });
    var ak = document.getElementById('qwen_ak');
    if (ak) ak.addEventListener('change', function () { updateQwenCode(); });
    var akC = document.getElementById('qwen_akCustom');
    if (akC) akC.addEventListener('input', function () { updateQwenCode(); });
    updateQwenCode();
  }
  function bindCoworkEvents() {
    bindEndpointApiKeyEvents('cowork');
    var m = document.getElementById('coworkModel');
    if (m) m.addEventListener('input', function () { window.__coworkState.model = this.value; });
  }
  function bindMitmEvents() {
    var ak = document.getElementById('mitm_ak');
    if (ak) ak.addEventListener('change', function () {
      var c = document.getElementById('mitm_akCustom');
      if (c) c.classList.toggle('hidden', this.value !== 'custom');
    });
    populateAkSelect('mitm');
    mitmRefreshStatus();
  }
  function bindAntigravityEvents() {
    var ak = document.getElementById('antigravity_ak');
    if (ak) ak.addEventListener('change', function () {
      var c = document.getElementById('antigravity_akCustom');
      if (c) c.classList.toggle('hidden', this.value !== 'custom');
    });
    populateAkSelect('antigravity');
    // Sync alias inputs to state on change
    document.querySelectorAll('.ag-alias-input').forEach(function (inp) {
      inp.addEventListener('input', function () {
        if (!window.__antigravityState.models) window.__antigravityState.models = {};
        window.__antigravityState.models[this.dataset.alias] = this.value;
      });
    });
    // Clear alias button
    document.querySelectorAll('.ag-alias-clear').forEach(function (btn) {
      btn.addEventListener('click', function () {
        var alias = this.dataset.alias;
        if (!window.__antigravityState.models) window.__antigravityState.models = {};
        window.__antigravityState.models[alias] = '';
        reRenderDetailBody();
      });
    });
    // Select model button
    document.querySelectorAll('.ag-alias-select').forEach(function (btn) {
      btn.addEventListener('click', function () {
        if (typeof openModelPicker === 'function') {
          var alias = this.dataset.alias;
          window.__cliModelCallback = function (model) {
            if (!window.__antigravityState.models) window.__antigravityState.models = {};
            window.__antigravityState.models[alias] = model;
            reRenderDetailBody();
            closeDialog('modelPickerModal');
          };
          openModelPicker();
        }
      });
    });
  }
  function bindCopilotEvents() {
    bindEndpointApiKeyEvents('copilot');
  }
  function bindKiroEvents() {
    // Kiro doesn't need endpoint/ak bindings - just mapping rows
  }
