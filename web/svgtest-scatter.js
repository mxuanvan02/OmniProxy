'use strict';

// OmniProxy SVG Model Test — capability scatter. Answers the question the
// results grid cannot: across one bare prompt, who got the best drawing for the
// least spend, and how long did they take? Every run sends the same prompt with
// no knobs, so a point's position is the model's own choice, not a setting we
// dialled. Top-left is the win — few tokens, high score.
//
// Position carries two axes (tokens, score) and the third measurement, wall
// time, is the point's size. Time is deliberately not a third axis: the point of
// the chart is comparing capability against spend, and a size encoding keeps
// that one glance instead of splitting it across two plots. Shares
// svgtestState with the other svgtest files and is drawn from stored results, so
// it survives a reload.

// Colours are per model, so one model run through several accounts reads as one
// family on the chart rather than as unrelated dots.
const SVGTEST_SCATTER_COLORS = ['#6366f1', '#f59e0b', '#10b981', '#ef4444', '#8b5cf6', '#06b6d4', '#ec4899', '#84cc16'];

// Point size range in SVG units. The floor keeps the fastest runs visible and
// the ceiling stops a seven-minute run from swallowing its neighbours.
const SVGTEST_SCATTER_R_MIN = 4;
const SVGTEST_SCATTER_R_MAX = 15;

function hideSvgTestScatter() {
  const card = document.getElementById('svgtestScatterCard');
  if (card) card.hidden = true;
}

// svgtestScatterPoints reduces a group's entries to plottable points. Only
// successful runs with a drawing and a real token count are plotted: a failed
// run produced nothing, so its zero score is the absence of a measurement rather
// than a measured zero, and a token count of zero cannot be placed on a log
// axis. Both are counted as omitted so the chart says what it dropped.
function svgtestScatterPoints(entries) {
  const points = [];
  let omitted = 0;
  const models = [];
  for (const e of entries || []) {
    const ok = e.success && e.svg;
    const tokens = Number(e.tokensUsed) || 0;
    if (!ok || tokens <= 0) { omitted++; continue; }
    const model = e.model || '?';
    if (!models.includes(model)) models.push(model);
    points.push({
      model: model,
      account: e.accountName || e.accountId || '?',
      score: Number(e.score) || 0,
      tokens: tokens,
      elapsed: Number(e.elapsedMs) || 0,
    });
  }
  points.sort((a, b) => models.indexOf(a.model) - models.indexOf(b.model));
  points.forEach(p => {
    p.color = SVGTEST_SCATTER_COLORS[models.indexOf(p.model) % SVGTEST_SCATTER_COLORS.length];
  });
  return { points: points, models: models, omitted: omitted };
}

// svgtestScatterWindow pads the vertical window around the data. A fixed 0-100
// axis hides the 80-100 band real results occupy, but every line is labelled
// with its true number, so a padded window cannot make a small gap read as a
// large one.
function svgtestScatterWindow(scores) {
  let min = Math.floor(Math.min.apply(null, scores) / 5) * 5 - 5;
  let max = Math.ceil(Math.max.apply(null, scores) / 5) * 5 + 5;
  if (max - min < 20) {
    const mid = (max + min) / 2;
    min = mid - 10;
    max = mid + 10;
  }
  return { min: Math.max(0, min), max: Math.min(100, max) };
}

// svgtestScatterTicks returns 1/2/5 × 10^n gridline values inside a log range.
// Round decades alone would leave a 1275→20151 spread with one line on it.
function svgtestScatterTicks(minLog, maxLog) {
  const ticks = [];
  for (let exp = Math.floor(minLog); exp <= Math.ceil(maxLog); exp++) {
    for (const m of [1, 2, 5]) {
      const v = m * Math.pow(10, exp);
      const lv = Math.log10(v);
      if (lv >= minLog - 1e-9 && lv <= maxLog + 1e-9) ticks.push(v);
    }
  }
  return ticks;
}

function renderSvgTestScatter(entries) {
  const card = document.getElementById('svgtestScatterCard');
  const box = document.getElementById('svgtestScatter');
  if (!card || !box) return;
  card.hidden = false;

  const built = svgtestScatterPoints(entries);
  if (built.points.length === 0) {
    box.replaceChildren(svgtestEmpty('svgtest.scatterEmpty'));
    return;
  }

  const tokens = built.points.map(p => p.tokens);
  // Padded so no point sits on the frame, where its edge would be clipped.
  const padLog = 0.06 * (Math.log10(Math.max.apply(null, tokens)) - Math.log10(Math.min.apply(null, tokens)) || 1);
  const minLog = Math.log10(Math.min.apply(null, tokens)) - padLog;
  const maxLog = Math.log10(Math.max.apply(null, tokens)) + padLog;
  const win = svgtestScatterWindow(built.points.map(p => p.score));

  const width = Math.min((box.clientWidth || 700), 700);
  const height = 300;
  const pad = { top: 16, right: 18, bottom: 40, left: 42 };
  const cw = width - pad.left - pad.right;
  const ch = height - pad.top - pad.bottom;
  const span = Math.max(win.max - win.min, 1);
  const xAt = tk => pad.left + ((Math.log10(tk) - minLog) / (maxLog - minLog)) * cw;
  const yAt = sc => pad.top + ch - ((sc - win.min) / span) * ch;

  // Radius grows with the square root of elapsed time, so the area — what the
  // eye actually compares — is proportional to the seconds spent.
  const times = built.points.map(p => p.elapsed);
  const rAt = ms => {
    const lo = Math.sqrt(Math.min.apply(null, times));
    const hi = Math.sqrt(Math.max.apply(null, times));
    if (hi - lo < 1) return SVGTEST_SCATTER_R_MIN + 2;
    return SVGTEST_SCATTER_R_MIN + ((Math.sqrt(ms) - lo) / (hi - lo)) * (SVGTEST_SCATTER_R_MAX - SVGTEST_SCATTER_R_MIN);
  };

  let html = '<svg width="100%" height="' + height + '" viewBox="0 0 ' + width + ' ' + height +
    '" class="svgtest-scatter-svg" role="img" aria-label="' + escAttr(t('svgtest.scatterTitle')) + '">';
  for (let i = 0; i <= 4; i++) {
    const y = pad.top + (i / 4) * ch;
    html += '<line x1="' + pad.left + '" y1="' + y + '" x2="' + (pad.left + cw) + '" y2="' + y +
      '" stroke="var(--border)" stroke-opacity="0.3" stroke-width="1"/>';
    const val = win.max - (i / 4) * span;
    html += '<text x="' + (pad.left - 6) + '" y="' + (y + 4) + '" text-anchor="end" fill="var(--muted-foreground)" font-size="10">' +
      Math.round(val) + '</text>';
  }
  for (const v of svgtestScatterTicks(minLog, maxLog)) {
    const x = xAt(v);
    html += '<line x1="' + x + '" y1="' + pad.top + '" x2="' + x + '" y2="' + (pad.top + ch) +
      '" stroke="var(--border)" stroke-opacity="0.2" stroke-width="1"/>';
    html += '<text x="' + x + '" y="' + (height - 22) + '" text-anchor="middle" fill="var(--muted-foreground)" font-size="10">' +
      escHtml(fmtTokens(v)) + '</text>';
  }
  html += '<text x="' + (pad.left + cw / 2) + '" y="' + (height - 6) + '" text-anchor="middle" fill="var(--muted-foreground)" font-size="10">' +
    escHtml(t('svgtest.scatterAxisX')) + '</text>';
  html += '<text transform="translate(11,' + (pad.top + ch / 2) + ') rotate(-90)" text-anchor="middle" fill="var(--muted-foreground)" font-size="10">' +
    escHtml(t('svgtest.scatterAxisY')) + '</text>';

  // Drawn largest-first so a big slow point cannot bury a small fast one.
  const ordered = built.points.slice().sort((a, b) => b.elapsed - a.elapsed);
  for (const p of ordered) {
    const tip = p.model + ' · ' + p.account + ' — ' + p.score + '/100 · ' +
      fmtTokens(p.tokens) + ' ' + t('svgtest.tokens') + ' · ' + fmtElapsed(p.elapsed);
    html += '<circle cx="' + xAt(p.tokens).toFixed(1) + '" cy="' + yAt(p.score).toFixed(1) +
      '" r="' + rAt(p.elapsed).toFixed(1) + '" fill="' + p.color + '" fill-opacity="0.55" stroke="' +
      p.color + '" stroke-width="1.5"><title>' + escHtml(tip) + '</title></circle>';
  }
  html += '</svg>';

  const legend = document.createElement('div');
  legend.className = 'svgtest-scatter-legend';
  built.models.forEach((m, i) => {
    const item = document.createElement('span');
    item.className = 'svgtest-scatter-key';
    const swatch = document.createElement('i');
    swatch.style.background = SVGTEST_SCATTER_COLORS[i % SVGTEST_SCATTER_COLORS.length];
    item.append(swatch, document.createTextNode(m));
    legend.appendChild(item);
  });
  const sizeKey = document.createElement('span');
  sizeKey.className = 'svgtest-scatter-key svgtest-scatter-sizekey';
  sizeKey.textContent = t('svgtest.scatterSizeKey');
  legend.appendChild(sizeKey);

  box.replaceChildren();
  box.insertAdjacentHTML('beforeend', html);
  box.appendChild(legend);

  const notes = [];
  if (built.omitted > 0) notes.push(t('svgtest.scatterOmitted', String(built.omitted)));
  if (notes.length > 0) {
    const note = document.createElement('div');
    note.className = 'svgtest-scatter-note';
    note.textContent = notes.join(' · ');
    box.appendChild(note);
  }
}
