'use strict';

// Shared model-capability classifier for the admin dashboard. The catalog
// endpoints return only model IDs (no capability metadata), so kind is guessed
// from the ID alone. Loaded right after escape.js so every consumer
// (accounts.js, app.js) sees window.classifyModelKind regardless of the
// dynamic loader's completion order.

// Order matters: more specific kinds are tested before the chat fallback.
function classifyModelKind(id) {
  const s = String(id).toLowerCase();
  if (/(embed|embedding|rerank|\bbge\b|\bgte\b)/.test(s)) return 'embedding';
  if (/(tts|stt|\bvoice|audio|speech|whisper|realtime|voicedesign|sovits)/.test(s)) return 'voice';
  if (/(video|veo|sora|kling|runway|pika|\bwan[-\d])/.test(s)) return 'video';
  if (/(image|\bimg\b|flux|dall[\-\.]?e|dalle|stable[-\s]?diffusion|\bsd[-\d]|imagen|seedream|kolors|recraft|midjourney|\bdraw\b|paint)/.test(s)) return 'image';
  return 'chat';
}

const MODEL_KIND_ORDER = ['chat', 'image', 'voice', 'video', 'embedding'];

// Resolves the localized label lazily via window.t (set by app.js) so this
// module can load before app.js without ordering hazard — it is only called
// at render time.
function modelKindLabel(kind) {
  const tr = (typeof window !== 'undefined' && typeof window.t === 'function') ? window.t : (k => k);
  switch (kind) {
    case 'chat': return tr('detail.modelKindChat');
    case 'image': return tr('detail.modelKindImage');
    case 'voice': return tr('detail.modelKindVoice');
    case 'video': return tr('detail.modelKindVideo');
    case 'embedding': return tr('detail.modelKindEmbedding');
    default: return kind;
  }
}

// groupModelsByKind takes an array of model IDs and returns an ordered list of
// { kind, label, ids } sections (MODEL_KIND_ORDER, empty kinds skipped). Input
// order within each kind is preserved; callers sort beforehand if they want it.
function groupModelsByKind(ids) {
  const groups = {};
  (ids || []).forEach(id => {
    const kind = classifyModelKind(id);
    (groups[kind] = groups[kind] || []).push(id);
  });
  return MODEL_KIND_ORDER
    .filter(kind => groups[kind] && groups[kind].length)
    .map(kind => ({ kind: kind, label: modelKindLabel(kind), ids: groups[kind] }));
}

window.classifyModelKind = classifyModelKind;
window.MODEL_KIND_ORDER = MODEL_KIND_ORDER;
window.modelKindLabel = modelKindLabel;
window.groupModelsByKind = groupModelsByKind;
