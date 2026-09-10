import { lazy, Suspense, useCallback, useEffect, useState } from 'react'
import { Login } from './components/Login'
import { Shell, type Section } from './components/Shell'
import { Overview } from './components/Overview'
import { AccountsView } from './components/AccountsView'

import { QuotaView } from './components/QuotaView'
import { ApiView } from './components/ApiView'
import { SettingsView } from './components/SettingsView'
import { LogsView } from './components/LogsView'
import { ApiError, api, getToken, logout, type Account, type ChartPoint, type QuotaOverview, type Settings, type Status, type UsageStats } from './lib/api'

const VALID_SECTIONS:Section[]=['overview','accounts','usage','quota','api','settings','logs']
const UsageView = lazy(() => import('./components/UsageView').then(module => ({ default: module.UsageView })))
function initialSection():Section{const v=location.hash.replace('#','') as Section;return VALID_SECTIONS.includes(v)?v:'overview'}

export function App(){
 const [authed,setAuthed]=useState(()=>Boolean(getToken()));const [section,setSectionState]=useState<Section>(initialSection)
 const [accounts,setAccounts]=useState<Account[]>([]);const [status,setStatus]=useState<Status|null>(null);const [usage,setUsage]=useState<UsageStats|null>(null);const [chart,setChart]=useState<ChartPoint[]>([]);const [quota,setQuota]=useState<QuotaOverview|null>(null);const [settings,setSettings]=useState<Settings|null>(null);const [cliStatus,setCliStatus]=useState<Record<string,unknown>|null>(null)
 const [busy,setBusy]=useState(false);const [error,setError]=useState('');const [updatedAt,setUpdatedAt]=useState<number|null>(null);const [usagePeriod,setUsagePeriod]=useState('24h')
 const onAuthError=useCallback((err:unknown)=>{if(err instanceof ApiError&&err.status===401){setAuthed(false);setAccounts([])}setError(err instanceof Error?err.message:String(err))},[])
 const setSection=(s:Section)=>{setSectionState(s);history.replaceState(null,'',`#${s}`)}
 const loadCore=useCallback(async()=>{const [a,s]=await Promise.all([api.accounts(),api.status()]);setAccounts(Array.isArray(a)?a:[]);setStatus(s)},[])
 const load=useCallback(async()=>{if(!authed)return;setBusy(true);try{await loadCore();if(section==='overview')setUsage(await api.usage('24h'));if(section==='usage'){const [nextUsage,nextChart]=await Promise.all([api.usage(usagePeriod),api.usageChart(usagePeriod)]);setUsage(nextUsage);setChart(nextChart)}if(section==='quota')setQuota(await api.quota());if(section==='api'){const [cfg,cli]=await Promise.all([api.settings(),api.cliStatus()]);setSettings(cfg);setCliStatus(cli)}if(section==='settings')setSettings(await api.settings());setUpdatedAt(Date.now());setError('')}catch(err){onAuthError(err)}finally{setBusy(false)}},[authed,section,usagePeriod,loadCore,onAuthError])
 useEffect(()=>{const id=window.setTimeout(()=>void load(),0);return()=>window.clearTimeout(id)},[load])
 useEffect(()=>{if(!authed||!['accounts','overview'].includes(section))return;const id=window.setInterval(()=>{if(!document.hidden)void load()},15000);return()=>clearInterval(id)},[authed,section,load])
 if(!authed)return <Login onDone={()=>setAuthed(true)}/>
 const settingsKey=`${settings?.requireApiKey??'loading'}:${settings?.allowOverUsage??'loading'}:${settings?.apiKey?'configured':'empty'}`
 const content=section==='overview'?<Overview status={status} usage={usage} accounts={accounts} onNavigate={setSection}/>:section==='accounts'?<AccountsView accounts={accounts} onReload={load}/>:section==='usage'?<Suspense fallback={<div className="grid min-h-96 place-items-center text-sm text-slate-500">Đang tải biểu đồ…</div>}><UsageView usage={usage} chart={chart} period={usagePeriod} onPeriod={setUsagePeriod}/></Suspense>:section==='quota'?<QuotaView data={quota} onReload={load}/>:section==='api'?<ApiView settings={settings} cliStatus={cliStatus} models={status?.modelIds||[]}/>:section==='settings'?<SettingsView key={settingsKey} settings={settings} onReload={load}/>:<LogsView/>
 return <Shell section={section} onSection={setSection} onRefresh={()=>void load()} onLogout={()=>void logout().then(()=>setAuthed(false))} busy={busy} updatedAt={updatedAt}>{error?<div role="alert" className="mx-auto mb-4 max-w-7xl rounded-lg border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-800">{error}</div>:null}{busy&&!updatedAt?<div className="grid min-h-96 place-items-center text-sm text-slate-500">Đang tải dữ liệu vận hành…</div>:content}</Shell>
}
