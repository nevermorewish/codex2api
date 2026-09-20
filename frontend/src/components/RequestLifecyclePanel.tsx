import { useMemo, useState } from 'react'
import { Activity, ArrowRight, Clock3, Hourglass, Layers3, RotateCw } from 'lucide-react'
import type { ConcurrencySnapshot, RequestLifecycle } from '../types'
import { StatTile } from './StatTile'
import { Input } from './ui/input'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from './ui/table'
import { cn } from '../lib/utils'
import { Select } from './ui/select'

const states: Record<string, string> = {
  preparing: '接收 / 校验', scheduler_wait: '调度队列', retry_wait: '重试等待',
  upstream: '上游处理中', fallback: '兜底处理中', finishing: '响应收尾',
  completed: '已完成', failed: '失败', canceled: '已取消',
}
const duration = (ms: number) => ms < 1000 ? `${Math.max(0, ms)} ms` : `${(ms / 1000).toFixed(1)} s`

function FirstToken({ row }: { row: RequestLifecycle }) {
  if (row.first_token_ms != null) return <span>{duration(row.first_token_ms)}</span>
  if (row.waiting_first_token) return <span className="text-amber-600 dark:text-amber-400">已等 {duration(row.first_token_wait_ms)}</span>
  return <span className="text-muted-foreground">{row.attempt === 0 ? '尚未分发' : '未观测到'}</span>
}

export default function RequestLifecyclePanel({ snapshot }: { snapshot: ConcurrencySnapshot }) {
  const [recent, setRecent] = useState(false)
  const [query, setQuery] = useState('')
  const [stage, setStage] = useState('all')
  const [page, setPage] = useState(1)
  const rows = useMemo(() => {
    const q = query.trim().toLowerCase()
    return (recent ? snapshot.lifecycle_recent : snapshot.lifecycle_requests)?.filter(r =>
      (stage === 'all' || r.state === stage) && (!q || [r.request_id, r.model, r.endpoint, r.account_id, r.api_key_id].join(' ').toLowerCase().includes(q))) ?? []
  }, [snapshot, recent, query, stage])
  const pages = Math.max(1, Math.ceil(rows.length / 20))
  const visiblePage = Math.min(page, pages)
  const ready = snapshot.total_inflight !== undefined
  return <section className="space-y-4 rounded-xl border border-border bg-card p-4 sm:p-5">
    <div className="flex flex-wrap items-start justify-between gap-3">
      <div><h2 className="text-base font-semibold">请求生命周期</h2><p className="mt-1 text-xs leading-relaxed text-muted-foreground">每 2 秒更新。按逻辑请求统计；WebSocket 每一轮单独计数，空闲连接不计入。首字时间采用当前 attempt 的上游首 Token 口径，完整响应缓存模式下不代表客户端已经收到内容。</p></div>
      <span className="shrink-0 text-xs text-muted-foreground">采集于 {new Date(snapshot.collected_at).toLocaleTimeString()}</span>
    </div>
    {!ready ? <p className="text-sm text-amber-600">服务端暂未提供生命周期数据，无法显示详细状态。</p> : <>
      <div className="grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-5">
        <StatTile label="总处理中" value={snapshot.total_inflight!} sub="所有尚未结束的生成请求" icon={<Activity className="size-4" />} tone="info" />
        <StatTile label="调度队列" value={snapshot.scheduler_waiters ?? 0} sub="没有账号可分配，等待释放" icon={<Layers3 className="size-4" />} tone={snapshot.scheduler_waiters ? 'warning' : 'neutral'} />
        <StatTile label="重试等待" value={snapshot.retry_waiters ?? 0} sub="等待退避、下一次 attempt" icon={<RotateCw className="size-4" />} tone={snapshot.retry_waiters ? 'warning' : 'neutral'} />
        <StatTile label="上游处理中" value={snapshot.upstream_active ?? 0} sub="已分配本地账号，等待或接收响应" icon={<Clock3 className="size-4" />} tone="info" />
        <StatTile label="兜底处理中" value={snapshot.fallback_active ?? 0} sub="已分配外部兜底账号" icon={<ArrowRight className="size-4" />} tone="neutral" />
      </div>
      <div className="flex flex-wrap gap-x-5 gap-y-2 text-xs text-muted-foreground">
        <span>接收 / 校验：{snapshot.preparing ?? 0}</span><span>响应收尾：{snapshot.finishing ?? 0}</span>
        <span>累计进入调度等待：{snapshot.scheduler_wait_started ?? 0} 次</span><span>累计进入重试等待：{snapshot.retry_wait_started ?? 0} 次</span>
        <span>累计值和最近记录在进程重启后清零；兜底与本地上游计数互斥。</span>
      </div>
      <div className="flex flex-wrap items-center gap-3">
        <div className="flex rounded-lg border p-1 text-sm" role="group" aria-label="请求记录范围">
          {[false, true].map(value => <button key={String(value)} type="button" aria-pressed={recent === value} className={cn('rounded-md px-3 py-1.5', recent === value ? 'bg-primary text-primary-foreground' : 'text-muted-foreground')} onClick={() => { setRecent(value); setStage('all'); setPage(1) }}>{value ? '最近完成（100 条）' : '当前请求'}</button>)}
        </div>
        <Input className="w-full sm:w-64" aria-label="搜索请求" placeholder="请求 ID / 模型 / 账号 / Key ID" value={query} onChange={e => { setQuery(e.target.value); setPage(1) }} />
        <Select aria-label="筛选请求状态" className="w-auto" value={stage} onValueChange={value => { setStage(value); setPage(1) }} options={[{ value: 'all', label: '全部状态' }, ...Object.entries(states).filter(([k]) => recent === ['completed', 'failed', 'canceled'].includes(k)).map(([value, label]) => ({ value, label }))]} />
      </div>
      {snapshot.lifecycle_truncated && !recent && <p className="text-xs text-amber-600">明细显示等待最久的 200 个请求；顶部计数包含全部请求。</p>}
      <div className="overflow-x-auto rounded-lg border">
        <Table>
          <TableHeader><TableRow><TableHead>请求 / 模型</TableHead><TableHead>当前状态</TableHead><TableHead>账号 / Key</TableHead><TableHead>当前 attempt / 已重试</TableHead><TableHead>首字时间</TableHead><TableHead>累计调度 / 重试等待</TableHead><TableHead>总耗时</TableHead></TableRow></TableHeader>
          <TableBody>{rows.slice((visiblePage - 1) * 20, visiblePage * 20).map(row => <TableRow key={row.id}>
            <TableCell className="max-w-72"><div className="truncate font-mono text-xs" title={row.request_id}>{row.request_id || `#${row.id}`}</div><div className="mt-1 font-medium">{row.model || '解析中'}</div><div className="mt-1 text-xs text-muted-foreground">{row.endpoint}</div></TableCell>
            <TableCell className="whitespace-nowrap"><span className={row.state.endsWith('wait') ? 'text-amber-600 dark:text-amber-400' : ''}>{states[row.state] ?? row.state}</span><div className="mt-1 text-xs text-muted-foreground">{recent ? (row.status_code ? `HTTP ${row.status_code}` : '已结束') : `本阶段 ${duration(row.state_elapsed_ms)}`}</div></TableCell>
            <TableCell className="whitespace-nowrap text-xs"><div>{row.account_id ? `${row.fallback ? '兜底' : '账号'} #${Math.abs(row.account_id)}` : '待分配'}</div><div className="mt-1 text-muted-foreground">Key #{row.api_key_id || '—'}</div></TableCell>
            <TableCell className="whitespace-nowrap font-mono tabular-nums">{row.attempt || '—'} / {row.retries}</TableCell>
            <TableCell className="whitespace-nowrap text-xs tabular-nums"><FirstToken row={row} /></TableCell>
            <TableCell className="whitespace-nowrap text-xs tabular-nums">{duration(row.scheduler_wait_ms)} / {duration(row.retry_wait_ms)}</TableCell>
            <TableCell className="whitespace-nowrap font-mono text-xs tabular-nums">{duration(row.elapsed_ms)}</TableCell>
          </TableRow>)}{rows.length === 0 && <TableRow><TableCell colSpan={7} className="h-24 text-center text-muted-foreground"><Hourglass className="mr-2 inline size-4" />{recent ? '暂无匹配的完成记录' : '当前没有匹配的请求'}</TableCell></TableRow>}</TableBody>
        </Table>
      </div>
      <div className="flex items-center justify-between text-xs text-muted-foreground"><span>{rows.length} 条 · 第 {visiblePage} / {pages} 页</span><div className="flex gap-3"><button type="button" disabled={visiblePage <= 1} className="disabled:opacity-40" onClick={() => setPage(visiblePage - 1)}>上一页</button><button type="button" disabled={visiblePage >= pages} className="disabled:opacity-40" onClick={() => setPage(visiblePage + 1)}>下一页</button></div></div>
    </>}
  </section>
}
