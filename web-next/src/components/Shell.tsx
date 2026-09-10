import type { ReactNode } from 'react'

export type Section = 'overview'|'accounts'|'usage'|'quota'|'api'|'settings'|'logs'
const items: Array<{id:Section; label:string; hint:string; icon:string}> = [
  {id:'overview',label:'Tổng quan',hint:'Sức khoẻ hệ thống',icon:'⌂'},
  {id:'accounts',label:'Tài khoản',hint:'Pool & model',icon:'◎'},
  {id:'usage',label:'Sử dụng',hint:'Token & chi phí',icon:'↗'},
  {id:'quota',label:'Hạn mức',hint:'Quota & reset',icon:'◔'},
  {id:'api',label:'API & CLI',hint:'Endpoint & công cụ',icon:'⌘'},
  {id:'settings',label:'Thiết lập',hint:'Proxy & routing',icon:'⚙'},
  {id:'logs',label:'Nhật ký',hint:'Sự kiện thời gian thực',icon:'≡'},
]

export function Shell({section,onSection,children,onRefresh,onLogout,busy,updatedAt}:{section:Section;onSection:(s:Section)=>void;children:ReactNode;onRefresh:()=>void;onLogout:()=>void;busy:boolean;updatedAt:number|null}) {
  return <div className="flex h-dvh overflow-hidden bg-[#f4f5f7] text-slate-950">
    <aside className="hidden w-60 shrink-0 flex-col border-r border-slate-200 bg-[#101827] text-white md:flex">
      <div className="border-b border-white/10 px-5 py-5"><div className="text-base font-semibold tracking-tight">OmniProxy</div><div className="mt-1 text-xs text-slate-400">Trung tâm vận hành</div></div>
      <nav className="flex-1 space-y-1 p-3" aria-label="Điều hướng chính">{items.map(i=><button key={i.id} onClick={()=>onSection(i.id)} className={`flex w-full items-center gap-3 rounded-lg px-3 py-2.5 text-left ${section===i.id?'bg-white text-slate-950':'text-slate-300 hover:bg-white/8 hover:text-white'}`}><span className="w-5 text-center text-base" aria-hidden>{i.icon}</span><span><span className="block text-sm font-medium">{i.label}</span><span className={`block text-[11px] ${section===i.id?'text-slate-500':'text-slate-500'}`}>{i.hint}</span></span></button>)}</nav>
      <div className="border-t border-white/10 p-3"><a href="/admin/" className="block rounded-lg px-3 py-2 text-xs text-slate-400 hover:bg-white/8 hover:text-white">Mở giao diện cũ ↗</a><button onClick={onLogout} className="mt-1 w-full rounded-lg px-3 py-2 text-left text-xs text-slate-400 hover:bg-white/8 hover:text-white">Đăng xuất</button></div>
    </aside>
    <div className="flex min-w-0 flex-1 flex-col">
      <header className="flex min-h-16 items-center gap-3 border-b border-slate-200 bg-white px-4 md:px-7">
        <select aria-label="Chọn khu vực" value={section} onChange={e=>onSection(e.target.value as Section)} className="rounded-lg border border-slate-300 bg-white px-3 py-2 text-sm md:hidden">{items.map(i=><option key={i.id} value={i.id}>{i.label}</option>)}</select>
        <div className="ml-auto flex items-center gap-3"><span className="hidden text-xs text-slate-500 sm:block">{updatedAt?`Cập nhật ${new Date(updatedAt).toLocaleTimeString('vi-VN')}`:'Chưa đồng bộ'}</span><button onClick={onRefresh} disabled={busy} className="rounded-lg border border-slate-300 bg-white px-3 py-2 text-sm font-medium hover:bg-slate-50 disabled:opacity-50">{busy?'Đang tải…':'Làm mới'}</button></div>
      </header>
      <main className="min-h-0 flex-1 overflow-auto p-4 md:p-7">{children}</main>
    </div>
  </div>
}

export function PageHeader({title,description,action}:{title:string;description:string;action?:ReactNode}) { return <div className="mb-6 flex items-start justify-between gap-4"><div><h1 className="text-2xl font-semibold tracking-tight">{title}</h1><p className="mt-1 max-w-3xl text-sm text-slate-600">{description}</p></div>{action}</div> }
export function Card({children,className=''}:{children:ReactNode;className?:string}) { return <section className={`rounded-xl border border-slate-200 bg-white shadow-[0_1px_2px_rgba(15,23,42,.03)] ${className}`}>{children}</section> }
