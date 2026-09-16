import { useState } from 'react'
import type { SVGTestResult } from '../lib/api'
import { testModelSVG } from '../lib/api'
import { Card, PageHeader } from './Shell'

export function ModelTestView({ models }: { models: string[] }) {
  const [selectedModel, setSelectedModel] = useState(models[0] ?? '')
  const [customPrompt, setCustomPrompt] = useState('')
  const [result, setResult] = useState<SVGTestResult | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')

  async function runTest() {
    if (!selectedModel) return
    setLoading(true); setError(''); setResult(null)
    try {
      const res = await testModelSVG(selectedModel, customPrompt || undefined)
      setResult(res)
      if (!res.success && !res.svg) setError(res.error ?? 'Model không trả về SVG hợp lệ')
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="mx-auto max-w-4xl space-y-5">
      <PageHeader
        title="Kiểm tra Model"
        description="Gửi prompt yêu cầu model tạo SVG để đánh giá khả năng hiểu và sinh output có cấu trúc — vượt xa bài test 'say ok' chỉ kiểm tra kết nối."
      />

      {/* Controls */}
      <Card className="p-5 space-y-4">
        <div className="flex flex-wrap gap-3">
          <select
            value={selectedModel}
            onChange={(e) => setSelectedModel(e.target.value)}
            className="input min-w-56 flex-1"
            aria-label="Chọn model"
          >
            {models.length === 0 && <option value="">Chưa có model nào</option>}
            {models.map((m) => <option key={m} value={m}>{m}</option>)}
          </select>
          <button
            onClick={() => void runTest()}
            disabled={loading || !selectedModel}
            className="btn-primary"
          >
            {loading ? 'Đang kiểm tra…' : 'Chạy SVG Test'}
          </button>
        </div>
        <textarea
          value={customPrompt}
          onChange={(e) => setCustomPrompt(e.target.value)}
          placeholder="Prompt tuỳ chỉnh (tuỳ chọn — để trống dùng prompt mặc định)"
          className="input w-full min-h-20 resize-y text-sm"
          rows={3}
        />
      </Card>

      {/* Result card — dark theme */}
      {result && (
        <div className="rounded-xl border border-slate-700 bg-[#0f172a] p-5 text-white">
          <div className="mb-3 flex items-center gap-2 text-sm font-semibold text-amber-400">
            <span aria-hidden>⚠</span> SVG TEST
          </div>
          <div className="text-sm text-slate-300">{result.model}</div>
          <div className="mt-1 text-xs text-slate-400">
            {result.elapsedMs} ms · {result.tokensUsed} tokens
          </div>

          {/* SVG render area */}
          <div className="mt-4 rounded-lg border border-slate-600 bg-white p-2 overflow-hidden">
            {result.svg
              ? <div dangerouslySetInnerHTML={{ __html: result.svg }} />
              : <div className="p-8 text-center text-sm text-slate-500">Không có SVG trong response</div>
            }
          </div>

          {/* Raw reply toggle */}
          <details className="mt-3">
            <summary className="cursor-pointer text-xs text-slate-400 hover:text-slate-300">
              Xem raw response
            </summary>
            <pre className="mt-2 max-h-48 overflow-auto rounded bg-slate-800 p-3 text-xs text-slate-300 whitespace-pre-wrap break-words">
              {result.rawReply}
            </pre>
          </details>
        </div>
      )}

      {error && (
        <div role="alert" className="rounded-lg border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-800">
          {error}
        </div>
      )}
    </div>
  )
}
