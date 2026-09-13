import { useCallback, useEffect, useState } from 'react'
import { AlertTriangle } from 'lucide-react'
import { api } from '../api'
import type { UsageLog } from '../types'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import Pagination from '../components/Pagination'
import { Input } from '@/components/ui/input'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'

const formatDuration = (ms?: number) => {
  if (!Number.isFinite(ms) || (ms ?? 0) < 0) return '—'
  return (ms as number) >= 1000 ? `${((ms as number) / 1000).toFixed((ms as number) >= 10000 ? 0 : 2)} 秒` : `${Math.round(ms as number)} 毫秒`
}
const formatTime = (value: string) => { const d = new Date(value); return Number.isNaN(d.getTime()) ? value : d.toLocaleString() }
const getReason = (log: UsageLog) => log.error_message?.trim() || log.upstream_error_kind?.trim() || (log.status_code === 499 ? '客户端取消连接' : log.status_code >= 200 && log.status_code < 300 && !log.is_retry_attempt ? '请求完成' : log.is_retry_attempt ? '上游失败后重试' : log.status_code ? `HTTP ${log.status_code}` : '原因未记录')

export default function LiveStreams() {
  const [logs, setLogs] = useState<UsageLog[]>([])
  const [total, setTotal] = useState(0)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(50)
  const [query, setQuery] = useState('')
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [updatedAt, setUpdatedAt] = useState<string | null>(null)
  const load = useCallback(async () => {
    setLoading(true)
    try {
      const result = await api.getUsageLogsPaged({ start: new Date(Date.now() - 86400000).toISOString(), end: new Date().toISOString(), page, pageSize, stream: 'true', q: query.trim() || undefined })
      setLogs(result.logs ?? []); setTotal(result.total ?? 0); setError(null); setUpdatedAt(new Date().toLocaleTimeString())
    } catch (cause) { setError(cause instanceof Error ? cause.message : String(cause)) } finally { setLoading(false) }
  }, [page, pageSize, query])
  useEffect(() => { void load() }, [load])
  useEffect(() => { const timer = window.setInterval(() => { if (document.visibilityState === 'visible') void load() }, 15000); return () => window.clearInterval(timer) }, [load])
  const pages = Math.max(1, Math.ceil(total / pageSize))
  return <div className="mx-auto w-full max-w-[1800px] space-y-4">
    <PageHeader title="流请求统计" description="近 24 小时各个流请求的首 Token 延迟、首 Token 后持续时间及结束原因。每行对应一次上游尝试，包含重试记录。" onRefresh={() => void load()} actionMeta={updatedAt ? `更新时间：${updatedAt} · 每 15 秒刷新` : undefined} />
    <section className="overflow-hidden rounded-xl border border-border bg-card/80 shadow-sm">
      <div className="flex flex-wrap items-center gap-3 border-b border-border p-3"><Input className="h-9 max-w-sm" value={query} onChange={e => { setPage(1); setQuery(e.target.value) }} placeholder="搜索模型、请求 ID、账号或原因" /><span className="text-xs text-muted-foreground">共 {total.toLocaleString()} 条流请求记录</span></div>
      {error && logs.length > 0 ? <div className="flex items-center gap-2 border-b border-amber-500/20 bg-amber-500/5 px-4 py-2 text-xs text-amber-700"><AlertTriangle className="size-3.5" />{error}</div> : null}
      <StateShell loading={loading && logs.length === 0} error={error && logs.length === 0 ? error : null} onRetry={() => void load()}>
        <Table className="min-w-[1260px] table-fixed"><TableHeader className="bg-muted/35"><TableRow className="hover:bg-transparent"><TableHead className="w-40">时间</TableHead><TableHead className="w-44">模型</TableHead><TableHead className="w-32">请求 ID</TableHead><TableHead className="w-32 text-right">首 Token 延迟</TableHead><TableHead className="w-40 text-right">首 Token 后持续</TableHead><TableHead className="w-32 text-right">本轮总耗时</TableHead><TableHead className="w-24 text-center">状态</TableHead><TableHead className="w-24 text-center">尝试</TableHead><TableHead>结束原因</TableHead></TableRow></TableHeader><TableBody>
          {logs.map(log => { const post = log.first_token_ms > 0 ? Math.max(0, log.duration_ms - log.first_token_ms) : undefined; const ok = log.status_code >= 200 && log.status_code < 300 && !log.is_retry_attempt; return <TableRow key={log.id}><TableCell className="text-xs tabular-nums text-muted-foreground">{formatTime(log.created_at)}</TableCell><TableCell className="truncate font-medium" title={log.model}>{log.model || '—'}</TableCell><TableCell className="truncate font-mono text-[11px] text-muted-foreground" title={log.request_id || log.parent_request_id}>{(log.request_id || log.parent_request_id || '—').slice(0, 12)}</TableCell><TableCell className="text-right font-mono text-xs tabular-nums">{formatDuration(log.first_token_ms > 0 ? log.first_token_ms : undefined)}</TableCell><TableCell className="text-right font-mono text-xs tabular-nums">{formatDuration(post)}</TableCell><TableCell className="text-right font-mono text-xs tabular-nums">{formatDuration(log.duration_ms)}</TableCell><TableCell className={ok ? 'text-center text-xs text-emerald-600' : 'text-center text-xs text-red-600'}>{ok ? '完成' : log.status_code ? `HTTP ${log.status_code}` : '失败'}</TableCell><TableCell className="text-center text-xs tabular-nums">{log.attempt_index || 1}{log.is_retry_attempt ? '（重试）' : ''}</TableCell><TableCell className="max-w-[360px] truncate text-xs text-muted-foreground" title={getReason(log)}>{getReason(log)}</TableCell></TableRow> })}
          {logs.length === 0 && !loading ? <TableRow><TableCell colSpan={9} className="h-40 text-center text-sm text-muted-foreground">近 24 小时没有流请求记录</TableCell></TableRow> : null}
        </TableBody></Table>
        <div className="px-4 pb-3"><Pagination page={page} totalPages={pages} onPageChange={setPage} totalItems={total} pageSize={pageSize} onPageSizeChange={size => { setPage(1); setPageSize(size) }} pageSizeOptions={[25, 50, 100, 200]} /></div>
      </StateShell>
    </section>
  </div>
}
