import type { CapabilitySummary } from '../lib/api'

const CAPABILITY_LABEL: Record<string, string> = {
  chat: 'Chat / Responses',
  search: 'Tìm kiếm',
  image: 'Tạo và sửa ảnh',
  video: 'Tạo video',
  'audio-stt': 'Nhận dạng giọng nói',
  'audio-tts': 'Tổng hợp giọng nói',
  'audio-music': 'Tạo nhạc',
  embedding: 'Embedding',
  moderation: 'Kiểm duyệt',
}

export function CapabilityCatalog({ capabilities }: { capabilities: CapabilitySummary[] }) {
  const endpointCount = capabilities.reduce((total, item) => total + (item.endpoints?.length ?? 0), 0)

  return (
    <details open className="mb-4 shrink-0 rounded-xl border border-neutral-200 bg-white dark:border-neutral-800 dark:bg-neutral-900">
      <summary className="cursor-pointer px-4 py-3 text-sm font-semibold text-neutral-900 dark:text-neutral-100">
        API & endpoint · {capabilities.length} capability · {endpointCount} URL
      </summary>
      <div className="grid max-h-64 grid-cols-1 gap-3 overflow-y-auto border-t border-neutral-200 p-4 dark:border-neutral-800 md:grid-cols-2 xl:grid-cols-3">
        {capabilities.map((item) => {
          const state = item.verified
            ? `Đã xác minh trên ${item.verifiedAccounts} account`
            : item.available
              ? 'Được quảng bá, chưa xác minh'
              : 'Không có account đang hỗ trợ'
          const tone = item.verified
            ? 'border-emerald-300 bg-emerald-50/60 dark:border-emerald-900 dark:bg-emerald-950/20'
            : item.available
              ? 'border-amber-300 bg-amber-50/60 dark:border-amber-900 dark:bg-amber-950/20'
              : 'border-neutral-200 bg-neutral-50 opacity-60 dark:border-neutral-800 dark:bg-neutral-950'

          return (
            <section key={item.capability} aria-disabled={!item.available} className={`rounded-lg border p-3 ${tone}`}>
              <div className="flex items-start justify-between gap-3">
                <div>
                  <h2 className="text-sm font-semibold">{CAPABILITY_LABEL[item.capability] ?? item.capability}</h2>
                  <p className="mt-0.5 text-xs text-neutral-600 dark:text-neutral-400">
                    {state} · {item.enabledAccounts}/{item.accounts} account bật
                  </p>
                </div>
                <code className="rounded bg-neutral-200/70 px-1.5 py-0.5 text-[11px] dark:bg-neutral-800">
                  {item.capability}
                </code>
              </div>
              <ul className="mt-2 space-y-1">
                {(item.endpoints ?? []).map((endpoint) => (
                  <li key={endpoint}>
                    <code className={`block break-all text-xs ${item.available ? 'text-neutral-800 dark:text-neutral-200' : 'text-neutral-400'}`}>
                      {endpoint}
                    </code>
                  </li>
                ))}
              </ul>
              {item.probeFailures > 0 || item.probeSkipped > 0 ? (
                <p className="mt-2 text-xs text-neutral-600 dark:text-neutral-400">
                  Probe lỗi: {item.probeFailures} · bỏ qua: {item.probeSkipped}
                </p>
              ) : null}
            </section>
          )
        })}
      </div>
    </details>
  )
}
