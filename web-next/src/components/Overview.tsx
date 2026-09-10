import type { Account, Status, UsageStats } from '../lib/api'
import { compactNumber, errorRate, health, accountLabel } from '../lib/format'
import { Card, PageHeader, type Section } from './Shell'

export function Overview({status,usage,accounts,onNavigate}:{status:Status|null;usage:UsageStats|null;accounts:Account[];onNavigate:(s:Section)=>void}) {
  const problem = accounts.filter(a=>health(a)==='banned'||Boolean(a.catalogError)||((errorRate(a)||0)>.1))
  const active = accounts.filter(a=>health(a)==='active').length
  const failed = status?.failedRequests ?? 0; const total = status?.totalRequests ?? 0
  const successRate = total ? Math.max(0,100-failed/total*100) : 100
  const topModels = Object.entries(usage?.byModel||{}).sort((a,b)=>(b[1].requests||0)-(a[1].requests||0)).slice(0,5)
  return <div className="mx-auto max-w-7xl">
    <PageHeader title="Tổng quan vận hành" description="Chỉ hiển thị tín hiệu cần quyết định; dữ liệu chi tiết nằm trong từng khu vực." />
    <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
      <Metric label="Pool khả dụng" value={`${status?.available??0}/${status?.totalAccounts??accounts.length}`} note={`${active} tài khoản đang phục vụ`} tone={(status?.available??0)>0?'ok':'bad'} />
      <Metric label="Tỉ lệ thành công" value={`${successRate.toFixed(1)}%`} note={`${compactNumber(total)} request toàn kỳ`} tone={successRate>=95?'ok':successRate>=80?'warn':'bad'} />
      <Metric label="Token" value={compactNumber(status?.totalTokens??0)} note={`${compactNumber(usage?.totalPromptTokens??0)} vào · ${compactNumber(usage?.totalCompletionTokens??0)} ra / 24h`} />
      <Metric label="Model khả dụng" value={String(status?.availableModels??0)} note={`Uptime ${formatUptime(status?.uptime??0)}`} />
    </div>
    <div className="mt-5 grid gap-5 xl:grid-cols-[1.35fr_.65fr]">
      <Card><div className="flex items-center justify-between border-b border-slate-200 px-5 py-4"><div><h2 className="font-semibold">Cần chú ý</h2><p className="text-xs text-slate-500">Khoá, lỗi catalog hoặc tỉ lệ lỗi trên 10%</p></div><button onClick={()=>onNavigate('accounts')} className="text-sm font-medium text-blue-700">Xem pool →</button></div>
        <div className="divide-y divide-slate-100">{problem.length?problem.slice(0,7).map(a=><div key={a.id} className="flex items-center gap-3 px-5 py-3"><span className={`size-2 rounded-full ${health(a)==='banned'?'bg-red-500':'bg-amber-500'}`}/><div className="min-w-0 flex-1"><div className="truncate text-sm font-medium">{accountLabel(a)}</div><div className="truncate text-xs text-slate-500">{a.banReason||a.catalogError||`Tỉ lệ lỗi ${((errorRate(a)||0)*100).toFixed(1)}%`}</div></div><span className="text-xs text-slate-500">{a.provider||'—'}</span></div>):<Empty text="Không có bất thường nổi bật" />}</div>
      </Card>
      <Card><div className="border-b border-slate-200 px-5 py-4"><h2 className="font-semibold">Model hoạt động nhiều</h2><p className="text-xs text-slate-500">Theo request trong 24 giờ</p></div><div className="p-5">{topModels.length?<ol className="space-y-4">{topModels.map(([name,s],i)=><li key={name} className="grid grid-cols-[20px_1fr_auto] items-center gap-2"><span className="text-xs text-slate-400">{i+1}</span><div className="min-w-0"><div className="truncate font-mono text-xs">{name}</div><div className="mt-1 h-1.5 overflow-hidden rounded-full bg-slate-100"><div className="h-full rounded-full bg-blue-600" style={{width:`${Math.max(4,(s.requests/(topModels[0]?.[1].requests||1))*100)}%`}}/></div></div><span className="font-mono text-xs tabular-nums">{compactNumber(s.requests)}</span></li>)}</ol>:<Empty text="Chưa có dữ liệu 24 giờ" />}</div></Card>
    </div>
    <div className="mt-5 grid gap-3 sm:grid-cols-3"><Quick title="Sử dụng & chi phí" text={`Chi phí ước tính 24h: $${(usage?.totalRealCost??0).toFixed(3)}`} onClick={()=>onNavigate('usage')}/><Quick title="Hạn mức" text="Theo dõi quota, credit và thời điểm reset" onClick={()=>onNavigate('quota')}/><Quick title="Nhật ký" text="Điều tra request lỗi và sự kiện runtime" onClick={()=>onNavigate('logs')}/></div>
  </div>
}
function Metric({label,value,note,tone}:{label:string;value:string;note:string;tone?:'ok'|'warn'|'bad'}) { const c=tone==='bad'?'text-red-700':tone==='warn'?'text-amber-700':tone==='ok'?'text-emerald-700':'text-slate-950'; return <Card className="p-5"><div className="text-xs font-medium uppercase tracking-wider text-slate-500">{label}</div><div className={`mt-2 font-mono text-3xl font-semibold tabular-nums ${c}`}>{value}</div><div className="mt-2 text-xs text-slate-500">{note}</div></Card> }
function Quick({title,text,onClick}:{title:string;text:string;onClick:()=>void}) { return <button onClick={onClick} className="rounded-xl border border-slate-200 bg-white p-4 text-left hover:border-slate-400"><div className="text-sm font-semibold">{title} →</div><div className="mt-1 text-xs text-slate-500">{text}</div></button> }
function Empty({text}:{text:string}) { return <div className="px-5 py-8 text-center text-sm text-slate-500">{text}</div> }
function formatUptime(s:number){const d=Math.floor(s/86400),h=Math.floor(s%86400/3600);return d?`${d} ngày ${h} giờ`:`${h} giờ`}
