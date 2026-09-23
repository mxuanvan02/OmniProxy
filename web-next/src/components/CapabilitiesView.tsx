import { useMemo } from 'react'
import type { CapabilityMatrix, CapabilitySummary } from '../lib/api'
import { Card, PageHeader } from './Shell'

const CAPABILITY_LABEL: Record<string, string> = {
  chat: 'Chat / Responses',
  vision: 'Nhận ảnh (vision)',
  search: 'Tìm kiếm',
  image: 'Tạo và sửa ảnh',
  video: 'Tạo video',
  'audio-stt': 'Nhận dạng giọng nói',
  'audio-tts': 'Tổng hợp giọng nói',
  'audio-music': 'Tạo nhạc',
  embedding: 'Embedding',
  moderation: 'Kiểm duyệt',
}

type GroupKey = 'verified' | 'available' | 'none'

const GROUP_META: Record<GroupKey, { title: string; hint: string; badge: string; dot: string; card: string }> = {
  verified: {
    title: 'Dùng được',
    hint: 'Đã probe và nhận phản hồi thật',
    badge: 'bg-emerald-100 text-emerald-800 dark:bg-emerald-950 dark:text-emerald-200',
    dot: 'bg-emerald-500',
    card: 'border-emerald-300 dark:border-emerald-900',
  },
  available: {
    title: 'Chưa xác minh',
    hint: 'Catalog quảng bá nhưng chưa probe',
    badge: 'bg-amber-100 text-amber-800 dark:bg-amber-950 dark:text-amber-200',
    dot: 'bg-amber-500',
    card: 'border-amber-300 dark:border-amber-900',
  },
  none: {
    title: 'Chưa có account',
    hint: 'Route tồn tại nhưng không account nào phục vụ',
    badge: 'bg-slate-100 text-slate-500 dark:bg-slate-800 dark:text-slate-400',
    dot: 'bg-slate-400',
    card: 'border-slate-200 opacity-70 dark:border-slate-800',
  },
}

function groupOf(item: CapabilitySummary): GroupKey {
  if (item.verified) return 'verified'
  if (item.available) return 'available'
  return 'none'
}

export function CapabilitiesView({ capabilities: matrix }: { capabilities: CapabilityMatrix }) {
  const groups = useMemo(() => {
    const buckets: Record<GroupKey, CapabilitySummary[]> = { verified: [], available: [], none: [] }
    for (const item of matrix.capabilities) buckets[groupOf(item)].push(item)
    return buckets
  }, [matrix])

  const endpoints = useMemo(
    () => matrix.capabilities.reduce((total, i) => total + (i.endpoints?.length ?? 0), 0),
    [matrix],
  )

  return (
    <div className="mx-auto max-w-7xl">
      <PageHeader
        title="Năng lực (Capabilities)"
        description="Nhóm theo trạng thái thực tế — dùng được, chưa xác minh, chưa có account. Bấm một thẻ để xem endpoint mà Agent/CLI trỏ vào."
      />
      <div className="mb-6 grid grid-cols-2 gap-3 sm:grid-cols-4">
        <StatCard label="Dùng được" value={String(groups.verified.length)} tone="text-emerald-600 dark:text-emerald-400" />
        <StatCard label="Chưa xác minh" value={String(groups.available.length)} tone="text-amber-600 dark:text-amber-400" />
        <StatCard label="Chưa có account" value={String(groups.none.length)} tone="text-slate-500" />
        <StatCard label="Endpoint" value={String(endpoints)} tone="text-slate-900 dark:text-slate-100" />
      </div>

      {matrix.capabilities.length === 0 ? (
        <Card className="p-8 text-center text-sm text-slate-500">Chưa có dữ liệu năng lực. Bấm Làm mới hoặc probe một account.</Card>
      ) : (
        (['verified', 'available', 'none'] as GroupKey[])
          .filter((key) => groups[key].length > 0)
          .map((key) => <Group key={key} groupKey={key} items={groups[key]} />)
      )}
    </div>
  )
}

function Group({ groupKey, items }: { groupKey: GroupKey; items: CapabilitySummary[] }) {
  const meta = GROUP_META[groupKey]
  return (
    <section className="mb-6">
      <div className="mb-3 flex items-center gap-2">
        <span className={`inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 text-xs font-semibold ${meta.badge}`}>
          <span className={`h-1.5 w-1.5 rounded-full ${meta.dot}`} aria-hidden />
          {meta.title}
        </span>
        <span className="text-xs text-slate-500">{items.length} · {meta.hint}</span>
      </div>
      <div className="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-3">
        {items.map((item) => (
          <CapCard key={item.capability} item={item} meta={meta} />
        ))}
      </div>
    </section>
  )
}

function CapCard({ item, meta }: { item: CapabilitySummary; meta: (typeof GROUP_META)[GroupKey] }) {
  const label = CAPABILITY_LABEL[item.capability] ?? item.capability
  const endpoints = item.endpoints ?? []
  const notes: string[] = []
  if (item.probeFailures > 0) notes.push(`probe lỗi ${item.probeFailures}`)
  if (item.probeSkipped > 0) notes.push(`bỏ qua ${item.probeSkipped}`)

  return (
    <Card className={`border ${meta.card} p-4`}>
      <div className="flex items-baseline justify-between gap-2">
        <h3 className="truncate text-sm font-semibold" title={label}>{label}</h3>
        <span className="shrink-0 text-xs font-medium tabular-nums text-slate-600 dark:text-slate-300">
          {item.enabledAccounts}/{item.accounts}
        </span>
      </div>
      <div className="mt-1 flex items-center gap-2">
        <code className="rounded bg-slate-100 px-1.5 py-0.5 text-[10px] text-slate-600 dark:bg-slate-800 dark:text-slate-300">
          {item.capability}
        </code>
        {notes.length > 0 ? <span className="text-[11px] text-slate-500">{notes.join(' · ')}</span> : null}
      </div>
      {endpoints.length > 0 ? (
        <details className="mt-3 border-t border-slate-100 pt-2 dark:border-slate-800">
          <summary className="cursor-pointer text-xs text-slate-500 hover:text-slate-700 dark:hover:text-slate-300">
            {endpoints.length} endpoint
          </summary>
          <ul className="mt-2 space-y-1">
            {endpoints.map((endpoint) => (
              <li key={endpoint}>
                <code className={`block break-all text-xs ${item.available ? 'text-slate-800 dark:text-slate-200' : 'text-slate-400'}`}>
                  {endpoint}
                </code>
              </li>
            ))}
          </ul>
        </details>
      ) : null}
    </Card>
  )
}

function StatCard({ label, value, tone }: { label: string; value: string; tone: string }) {
  return (
    <Card className="p-4">
      <div className={`text-2xl font-semibold tabular-nums ${tone}`}>{value}</div>
      <div className="mt-1 text-xs text-slate-500">{label}</div>
    </Card>
  )
}
