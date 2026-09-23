/** Model capability taxonomy for the next-gen UI.
 *
 *  The catalog endpoints return bare model IDs with no capability metadata, so
 *  the kind is inferred from the ID alone. This is the single source of truth:
 *  ApiView's catalog filter and AccountsView's model grouping both read it, so
 *  the two views cannot disagree about what a given model is.
 *
 *  Merged from two classifiers that had drifted apart — the legacy /admin one
 *  (web/model-kind.js) and the one that shipped inside ApiView. The union of
 *  both keyword sets is used so neither view loses coverage. */
export type ModelKind = 'chat' | 'image' | 'video' | 'voice' | 'embedding' | 'moderation'

/** Display order for grouped lists, and the order the kind filters appear in.
 *  This is NOT the order of the tests in classifyModelKind — those run
 *  most-specific-first so the chat fallback is reached last. */
export const MODEL_KIND_ORDER: ModelKind[] = ['chat', 'image', 'video', 'voice', 'embedding', 'moderation']

const LABELS: Record<ModelKind, string> = {
  chat: 'Chat',
  image: 'Tạo ảnh',
  video: 'Tạo video',
  voice: 'Âm thanh',
  embedding: 'Embedding',
  moderation: 'Kiểm duyệt',
}

export function modelKindLabel(kind: ModelKind): string {
  return LABELS[kind]
}

/** Drops the `[1m]` context-window suffix and any vendor prefix, so
 *  `cx/gpt-5.6-sol` and `openai/dall-e-3` are classified on the bare model
 *  name. Also used as the dedupe key when listing the catalog. */
export function canonicalModel(id: string): string {
  const value = id.trim().toLowerCase().replace(/\[1m\]$/i, '')
  const slash = value.indexOf('/')
  return slash >= 0 ? value.slice(slash + 1) : value
}

export function classifyModelKind(id: string): ModelKind {
  // Separators are normalised so the keyword tests only deal with dashes.
  const value = canonicalModel(id).replace(/[_. ]+/g, '-')
  if (/embedding|embed|rerank|bge-|gte-|e5-|voyage-/.test(value)) return 'embedding'
  if (/tts|stt|voice|audio|speech|whisper|transcrib|realtime|sovits/.test(value)) return 'voice'
  if (/video|veo-|sora|kling|runway|pika|wan[-\d]/.test(value)) return 'video'
  if (/image|\bimg\b|flux|dall-?e|dalle|stable-?diffusion|sd[-\d]|imagen|seedream|kolors|recraft|midjourney|\bdraw\b|paint/.test(value)) return 'image'
  if (/moderation|safety|guard/.test(value)) return 'moderation'
  return 'chat'
}

export interface ModelGroup {
  kind: ModelKind
  label: string
  ids: string[]
}

/** Splits model IDs into ordered { kind, label, ids } sections. Empty kinds are
 *  skipped, and the caller's order is preserved within each kind — sort first
 *  if the list should read alphabetically. */
export function groupModelsByKind(ids: string[]): ModelGroup[] {
  const groups = new Map<ModelKind, string[]>()
  for (const id of ids) {
    const kind = classifyModelKind(id)
    const bucket = groups.get(kind)
    if (bucket) bucket.push(id)
    else groups.set(kind, [id])
  }
  return MODEL_KIND_ORDER.filter((kind) => groups.has(kind)).map((kind) => ({
    kind,
    label: LABELS[kind],
    ids: groups.get(kind) as string[],
  }))
}
