'use strict';

// OmniProxy SVG Model Test — effort curve. Answers the question the comparison
// grid cannot: given one prompt, does a model actually buy quality with more
// reasoning, or does it spend tokens for nothing? One line per (model, account)
// pair, plotted against the reasoning effort each sweep rung ran at. Shares
// svgtestState with the other svgtest files and is drawn from stored results, so
// the curve survives a reload instead of living only in the sweep's responses.

const SVGTEST_CURVE_LEVELS = ['low', 'medium', 'high'];

// Line colours for up to eight pairs. A comparison rarely selects more, and
// past this the legend repeats rather than inventing indistinguishable hues.
const SVGTEST_CURVE_COLORS = ['#6366f1', '#f59e0b', '#10b981', '#ef4444', '#8b5cf6', '#06b6d4', '#ec4899', '#84cc16'];

function hideSvgTestCurve() {
  const card = document.getElementById('svgtestCurveCard');
  if (card) card.hidden = true;
}

// svgtestCurveSeries collapses a group's entries into one line per pair. Only
// successful think-mode rungs are plotted: a failed rung produced no drawing, so
// its zero is the absence of a measurement rather than a measured zero quality,
// and plotting it would fake a dive. Omitted rungs are reported instead.
function svgtestCurveSeries(entries) {
  const byPair = new Map();
  let omitted = 0;
  for (const e of entries || []) {
    if ((e.mode || '').toLowerCase() !== 'think') continue;
    const rung = SVGTEST_CURVE_LEVELS.indexOf((e.effort || '').toLowerCase());
    if (rung < 0) continue;
    const id = (e.model || '') + '::' + (e.accountId || '');
    if (!byPair.has(id)) {
      byPair.set(id, {
        model: e.model || '?',
        account: e.accountName || e.accountId || '?',
        dialect: (e.dialect || '').toLowerCase(),
        points: [],
      });
    }
    if (!(e.success && e.svg)) { omitted++; continue; }
    byPair.get(id).points.push({ rung: rung, score: Number(e.score) || 0, tokens: Number(e.tokensUsed) || 0 });
  }
  // A single rung is a point, not a curve, and drawing a line through it would
  // imply a trend the data does not contain.
  const series = [];
  for (const s of byPair.values()) {
    s.points.sort((a, b) => a.rung - b.rung);
    if (s.points.length >= 2) series.push(s);
  }
  return { series: series, omitted: omitted };
}

// svgtestCurveScale picks the vertical window. A fixed 0-100 axis hides the
// 80-100 spread real results occupy, so the window is padded around the data and
// always labelled with its real numbers — the reader sees the range, so a small
// difference cannot masquerade as a large one.
function svgtestCurveScale(scores) {
  let min = Math.floor(Math.min.apply(null, scores) / 10) * 10 - 10;
  let max = Math.ceil(Math.max.apply(null, scores) / 10) * 10 + 10;
  if (max - min < 20) {
    const mid = (max + min) / 2;
    min = mid - 10;
    max = mid + 10;
  }
  return { min: Math.max(0, min), max: Math.min(100, max) };
}

function renderSvgTestCurve(entries) {
  const card = document.getElementById('svgtestCurveCard');
  const box = document.getElementById('svgtestCurve');
  if (!card || !box) return;
  card.hidden = false;

  const built = svgtestCurveSeries(entries);
  if (built.series.length === 0) {
    box.replaceChildren(svgtestEmpty('svgtest.curveEmpty'));
    return;
  }

  const scores = built.series.flatMap(s => s.points.map(p => p.score));
  const scale = svgtestCurveScale(scores);
  const width = Math.min((box.clientWidth || 700), 700);
  const height = 240;
  const pad = { top: 16, right: 16, bottom: 34, left: 42 };
  const cw = width - pad.left - pad.right;
  const ch = height - pad.top - pad.bottom;
  const span = Math.max(scale.max - scale.min, 1);
  const xAt = rung => pad.left + (rung / (SVGTEST_CURVE_LEVELS.length - 1)) * cw;
  const yAt = score => pad.top + ch - ((score - scale.min) / span) * ch;

  let html = '<svg width="100%" height="' + height + '" viewBox="0 0 ' + width + ' ' + height +
    '" class="svgtest-curve-svg" role="img" aria-label="' + escAttr(t('svgtest.curveTitle')) + '">';
  for (let i = 0; i <= 4; i++) {
    const y = pad.top + (i / 4) * ch;
    html += '<line x1="' + pad.left + '" y1="' + y + '" x2="' + (pad.left + cw) + '" y2="' + y +
      '" stroke="var(--border)" stroke-opacity="0.3" stroke-width="1"/>';
    const val = scale.max - (i / 4) * span;
    html += '<text x="' + (pad.left - 6) + '" y="' + (y + 4) + '" text-anchor="end" fill="var(--muted-foreground)" font-size="10">' +
      Math.round(val) + '</text>';
  }
  SVGTEST_CURVE_LEVELS.forEach((level, rung) => {
    html += '<text x="' + xAt(rung) + '" y="' + (height - 8) + '" text-anchor="middle" fill="var(--muted-foreground)" font-size="10">' +
      escHtml(level) + '</text>';
  });

  built.series.forEach((s, i) => {
    const color = SVGTEST_CURVE_COLORS[i % SVGTEST_CURVE_COLORS.length];
    const line = s.points.map((p, j) => (j === 0 ? 'M' : 'L') + xAt(p.rung) + ',' + yAt(p.score)).join('');
    html += '<path d="' + line + '" fill="none" stroke="' + color + '" stroke-width="2"/>';
    for (const p of s.points) {
      const tip = s.model + ' · ' + s.account + ' · ' + SVGTEST_CURVE_LEVELS[p.rung] + ' · ' +
        p.score + '/100 · ' + p.tokens + ' ' + t('svgtest.tokens');
      html += '<circle cx="' + xAt(p.rung) + '" cy="' + yAt(p.score) + '" r="3.5" fill="' + color + '">' +
        '<title>' + escHtml(tip) + '</title></circle>';
    }
  });
  html += '</svg>';

  const legend = document.createElement('div');
  legend.className = 'svgtest-curve-legend';
  built.series.forEach((s, i) => {
    const item = document.createElement('span');
    item.className = 'svgtest-curve-key';
    const swatch = document.createElement('i');
    swatch.style.background = SVGTEST_CURVE_COLORS[i % SVGTEST_CURVE_COLORS.length];
    item.append(swatch, document.createTextNode(s.model + ' · ' + s.account));
    legend.appendChild(item);
  });

  box.replaceChildren();
  box.insertAdjacentHTML('beforeend', html);
  box.appendChild(legend);

  // Say plainly what the curve cannot show, rather than let the operator read a
  // flat line as a measured result. Messages-dialect accounts hold the reasoning
  // budget at a fixed value and only check that effort is non-empty, so their
  // rungs are identical requests by construction.
  const notes = [];
  if (built.omitted > 0) notes.push(t('svgtest.curveOmitted', String(built.omitted)));
  if (built.series.some(s => s.dialect === 'anthropic')) notes.push(t('svgtest.curveFlatNote'));
  if (notes.length > 0) {
    const note = document.createElement('div');
    note.className = 'svgtest-curve-note';
    note.textContent = notes.join(' · ');
    box.appendChild(note);
  }
}
