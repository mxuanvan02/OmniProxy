import { describe, expect, it } from 'vitest'
import { canonicalModel, classifyModelKind, groupModelsByKind, MODEL_KIND_ORDER, modelKindLabel } from './model-kind'

describe('canonicalModel', () => {
  it('strips the vendor prefix so classification sees the bare model name', () => {
    expect(canonicalModel('cx/gpt-5.6-sol')).toBe('gpt-5.6-sol')
  })

  it('strips the [1m] context-window suffix', () => {
    expect(canonicalModel('claude-sonnet-5[1m]')).toBe('claude-sonnet-5')
  })
})

describe('classifyModelKind', () => {
  it('classifies each kind from the ID alone', () => {
    expect(classifyModelKind('text-embedding-3-large')).toBe('embedding')
    expect(classifyModelKind('dall-e-3')).toBe('image')
    expect(classifyModelKind('veo-3')).toBe('video')
    expect(classifyModelKind('whisper-1')).toBe('voice')
    expect(classifyModelKind('omni-moderation-latest')).toBe('moderation')
    expect(classifyModelKind('gpt-5.6-sol')).toBe('chat')
  })

  it('falls back to chat for an unrecognised ID rather than dropping it', () => {
    expect(classifyModelKind('some-new-model-2027')).toBe('chat')
  })

  it('prefers the more specific kind when keywords overlap', () => {
    // "audio" would also match a loose image test; embedding wins over chat.
    expect(classifyModelKind('gpt-4o-audio-preview')).toBe('voice')
    expect(classifyModelKind('gpt-4o-realtime-preview')).toBe('voice')
  })

  it('keeps vendor prefixes and the [1m] suffix out of the decision', () => {
    expect(classifyModelKind('openai/dall-e-3')).toBe('image')
    expect(classifyModelKind('cx/text-embedding-3-small[1m]')).toBe('embedding')
  })
})

describe('groupModelsByKind', () => {
  const ids = ['gpt-5.6-sol', 'dall-e-3', 'whisper-1', 'text-embedding-3-large', 'gpt-4o']

  it('groups by kind in MODEL_KIND_ORDER and skips empty kinds', () => {
    const groups = groupModelsByKind(ids)
    expect(groups.map((g) => g.kind)).toEqual(['chat', 'image', 'voice', 'embedding'])
    expect(groups.map((g) => g.label)).toEqual(['Chat', 'Tạo ảnh', 'Âm thanh', 'Embedding'])
  })

  it('preserves the caller order within each kind', () => {
    const chat = groupModelsByKind(ids).find((g) => g.kind === 'chat')
    expect(chat?.ids).toEqual(['gpt-5.6-sol', 'gpt-4o'])
  })

  it('returns no groups for an empty list', () => {
    expect(groupModelsByKind([])).toEqual([])
  })
})

describe('modelKindLabel', () => {
  it('labels every kind in the order list', () => {
    for (const kind of MODEL_KIND_ORDER) expect(modelKindLabel(kind)).toBeTruthy()
  })
})