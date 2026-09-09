import { useEffect, useState } from 'react'
import { Activity, RefreshCw } from 'lucide-react'
import { api } from '../api'
import type { AccountFirstTokenStat } from '../types'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import { Card, CardContent } from '@/components/ui/card'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'

export default function FirstTokenAnalysis() {
  const [rows, setRows] = useState<AccountFirstTokenStat[]>([]); const [error, setError] = useState<string | null>(null); const [loading, setLoading] = useState(false)
  const load = async () => { setLoading(true); try { setRows((await api.getFirstTokenStats()).stats ?? []); setError(null) } catch (e) { setError(e instanceof Error ? e.message : '加载失败') } finally { setLoading(false) } }
  useEffect(() => { void load() }, [])
  return <div className="mx-auto w-full max-w-[1800px] pb-8"><PageHeader title="首 Token 分析" description="按账号统计今日首 Token 延迟、超时与上游错误，评分越高表示近期表现越稳定。" actions={<button className="inline-flex items-center gap-2 rounded-lg border px-3 py-2 text-sm" onClick={() => void load()} disabled={loading}><RefreshCw className={`size-4 ${loading ? 'animate-spin' : ''}`} />刷新</button>} /><StateShell variant="page" error={error} onRetry={() => void load()}><Card><CardContent className="p-0"><Table><TableHeader><TableRow><TableHead>账号</TableHead><TableHead>样本</TableHead><TableHead>P50</TableHead><TableHead>P90</TableHead><TableHead>首字超时</TableHead><TableHead>上游 500</TableHead><TableHead>上游 502</TableHead><TableHead>上游 503</TableHead><TableHead>延迟评分</TableHead></TableRow></TableHeader><TableBody>{rows.map((r) => <TableRow key={r.account_id}><TableCell><div className="font-medium">{r.account_name || `账号 #${r.account_id}`}</div><div className="text-xs text-muted-foreground">{r.account_email || '—'}</div></TableCell><TableCell>{r.samples}</TableCell><TableCell>{Math.round(r.p50_ms)} ms</TableCell><TableCell className={r.p90_ms > 30000 ? 'font-semibold text-red-600' : ''}>{Math.round(r.p90_ms)} ms</TableCell><TableCell>{r.timeout_count}</TableCell><TableCell>{r.upstream_500_count}</TableCell><TableCell>{r.upstream_502_count}</TableCell><TableCell>{r.upstream_503_count}</TableCell><TableCell className="font-semibold">{r.score.toFixed(1)}</TableCell></TableRow>)}{!rows.length && <TableRow><TableCell colSpan={9} className="h-32 text-center text-muted-foreground"><Activity className="mx-auto mb-2 size-5" />今日暂无首 Token 样本</TableCell></TableRow>}</TableBody></Table></CardContent></Card></StateShell></div>
}
