import { useEffect, useMemo, useState } from 'react'
import { Bar, BarChart, CartesianGrid, Line, LineChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts'
import { api } from '../api'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import { Card, CardContent } from '@/components/ui/card'
import type { OpsErrorSummary, UsageLog, UsageStats } from '../types'

export default function ErrorAnalysis() {
  const [summary, setSummary] = useState<OpsErrorSummary | null>(null)
  const [logs, setLogs] = useState<UsageLog[]>([])
  const [stats, setStats] = useState<UsageStats | null>(null)
  const [trend, setTrend] = useState<Array<{ bucket: string; first: number; nonstream: number }>>([])
  const [error, setError] = useState<string | null>(null)
  const load = async () => {
    const now = new Date(); const start = new Date(now); start.setHours(0, 0, 0, 0)
    const range = { start: start.toISOString(), end: now.toISOString() }
    try {
      const [s, l, u, chart] = await Promise.all([
        api.getOpsErrorSummary(range), api.getOpsErrors({ ...range, page: 1, pageSize: 500 }),
        api.getUsageStats(range), api.getChartData({ ...range, bucketMinutes: 60 }),
      ])
      setSummary(s); setLogs(l.logs ?? []); setStats(u)
      const byHour = new Map<string, { first: number[]; nonstream: number[] }>()
      ;(l.logs ?? []).forEach((row) => { const key = new Date(row.created_at).toLocaleTimeString([], { hour: '2-digit' }); const item = byHour.get(key) ?? { first: [], nonstream: [] }; if (row.first_token_ms > 0) item.first.push(row.first_token_ms); if (!row.stream && row.duration_ms > 0) item.nonstream.push(row.duration_ms); byHour.set(key, item) })
      setTrend((chart.timeline ?? []).map((p) => { const key = new Date(p.bucket).toLocaleTimeString([], { hour: '2-digit' }); const item = byHour.get(key); return { bucket: key, first: item?.first.length ? item.first.reduce((a, b) => a + b, 0) / item.first.length : 0, nonstream: item?.nonstream.length ? item.nonstream.reduce((a, b) => a + b, 0) / item.nonstream.length : p.avg_latency } }))
      setError(null)
    } catch (e) { setError(e instanceof Error ? e.message : '加载失败') }
  }
  useEffect(() => { void load(); const timer = window.setInterval(() => void load(), 30000); return () => window.clearInterval(timer) }, [])
  const categories = useMemo(() => { const counts = new Map<string, number>(); logs.forEach((l) => { const k = l.upstream_error_kind || l.error_message || 'unknown'; counts.set(k, (counts.get(k) ?? 0) + 1) }); return [...counts].sort((a, b) => b[1] - a[1]).slice(0, 10).map(([name, count]) => ({ name, count })) }, [logs])
  return <div className="mx-auto w-full max-w-[1800px]"><PageHeader title="错误分析" description="汇总当天错误分类与请求延迟分布。" onRefresh={() => void load()} /><StateShell variant="page" error={error} onRetry={() => void load()}>{summary && <div className="space-y-5"><div className="grid grid-cols-2 gap-3 md:grid-cols-4"><Metric label="错误总数" value={summary.total_errors} /><Metric label="4xx" value={summary.status_4xx} /><Metric label="5xx" value={summary.status_5xx} /><Metric label="限流" value={summary.rate_limited} /><Metric label="首 Token 平均延迟" value={`${Math.round(stats?.avg_first_token_ms ?? 0)} ms`} /><Metric label="非流式平均延迟" value={`${Math.round(stats?.avg_duration_ms ?? 0)} ms`} /></div><div className="grid gap-5 lg:grid-cols-2"><Card><CardContent className="p-4"><h3 className="mb-3 font-semibold">错误分类</h3><div className="h-64"><ResponsiveContainer><BarChart data={categories} layout="vertical"><CartesianGrid horizontal={false} /><XAxis type="number" allowDecimals={false} /><YAxis dataKey="name" type="category" width={130} /><Tooltip /><Bar dataKey="count" fill="hsl(var(--destructive))" /></BarChart></ResponsiveContainer></div></CardContent></Card><Card><CardContent className="p-4"><h3 className="mb-3 font-semibold">当日延迟分布</h3><div className="h-64"><ResponsiveContainer><LineChart data={trend}><CartesianGrid vertical={false} /><XAxis dataKey="bucket" /><YAxis /><Tooltip /><Line dataKey="first" name="首 Token(ms)" stroke="hsl(var(--primary))" dot={false} /><Line dataKey="nonstream" name="非流式(ms)" stroke="hsl(36 90% 55%)" dot={false} /></LineChart></ResponsiveContainer></div></CardContent></Card></div></div>}</StateShell></div>
}
function Metric({ label, value }: { label: string; value: number | string }) { return <Card><CardContent className="p-4"><div className="text-xs text-muted-foreground">{label}</div><div className="mt-1 text-2xl font-bold tabular-nums">{value}</div></CardContent></Card> }
