import { Fragment, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import type { TFunction } from 'i18next'
import { ArrowRight, CheckCircle2, ChevronRight, CircleHelp, CircleSlash, RefreshCw, Radio, Search, XCircle, AlertTriangle, ExternalLink } from 'lucide-react'
import { api } from '../api'
import type { LiveStream, RelayAttempt, UsageLog } from '../types'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import { SegmentedTabs } from '../components/SegmentedTabs'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { getErrorMessage } from '../utils/error'
import { cn } from '@/lib/utils'
import { liveStreamDisconnectReasonKey } from '@/lib/liveStreamDisconnectReason'
import { liveStreamElapsedMs, liveStreamIsDisconnected, liveStreamStatus } from '@/lib/liveStreamStatus'
import { createSerialPoller } from '@/lib/serialPoller'

// Keep a short client-side cache for older servers that do not yet expose the
// lifecycle observer's persisted terminal snapshot.
const RECENTLY_DISCONNECTED_RETENTION_MS = 5 * 60_000

function StreamStatusBadge({ stream, t }: { stream: LiveStream; t: TFunction }) {
  const status = liveStreamStatus(stream)
  const Icon = status === 'success' ? CheckCircle2 : status === 'failed' ? XCircle : status === 'in_progress' ? Radio : status === 'canceled' ? CircleSlash : CircleHelp
  return (
    <span className={cn('inline-flex items-center gap-1.5 rounded-md px-2 py-1 text-xs font-medium', status === 'success' ? 'bg-emerald-500/10 text-emerald-700 dark:text-emerald-400' : status === 'failed' ? 'bg-red-500/10 text-red-700 dark:text-red-400' : status === 'in_progress' ? 'bg-blue-500/10 text-blue-700 dark:text-blue-400' : 'bg-muted text-muted-foreground')}>
      <Icon className="size-3.5 shrink-0" />
      <span>{t(`liveStreams.statuses.${status}`)}</span>
    </span>
  )
}

function StreamAccountLabel({ attempt, t }: { attempt: RelayAttempt; t: TFunction }) {
  const name = attempt.account_name || (attempt.account_id ? `#${attempt.account_id}` : t('concurrency.unknownAccount'))
  return (
    <span className="inline-flex min-w-0 max-w-full items-center gap-1" title={name}>
      <span className="truncate">{name}</span>
      {attempt.fallback || attempt.account_id < 0 ? (
        <span className="shrink-0 rounded border border-amber-500/25 bg-amber-500/10 px-1 py-0.5 text-[10px] font-normal text-amber-700 dark:text-amber-300">{t('concurrency.channels.fallback')}</span>
      ) : null}
    </span>
  )
}

function formatDuration(value: number): string {
  if (!Number.isFinite(value) || value < 0) return '-'
  if (value >= 1000) return `${(value / 1000).toFixed(value >= 10_000 ? 0 : 1)} s`
  return `${Math.round(value)} ms`
}

function streamDuration(stream: LiveStream, now: number): number {
  if (typeof stream.elapsed_ms === 'number' && Number.isFinite(stream.elapsed_ms)) return stream.elapsed_ms
  return liveStreamElapsedMs(stream, now)
}

function formatStreamTime(value: string): string {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value || '-'
  return date.toLocaleString(undefined, {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit',
  })
}

function StreamDetails({ stream, t }: { stream: LiveStream; t: TFunction }) {
  const disconnected = liveStreamIsDisconnected(stream)
  const [history, setHistory] = useState<RelayAttempt[]>([])
  const [loading, setLoading] = useState(true)
  const [historyError, setHistoryError] = useState(false)
  useEffect(() => {
    const controller = new AbortController()
    const load = async () => {
      try {
        const data = await api.getLiveStreamRequests(stream.stream_id || stream.request_id, controller.signal)
        const requests = (data.requests ?? data.items ?? []) as { request_id: string }[]
        const ids = requests.length ? requests.map((request) => request.request_id).filter(Boolean) : [stream.request_id]
        const results = await Promise.all(ids.map((id) => api.getLiveStreamRequestAttempts(id, controller.signal)))
        const attempts = results.flatMap((result) => (result.attempts ?? []) as (RelayAttempt & { attempt_seq?: number })[])
          .map((attempt, index) => ({ ...attempt, seq: index + 1, decision: attempt.decision || '' }))
        if (!controller.signal.aborted) setHistory(attempts)
      } catch {
        if (!controller.signal.aborted) setHistoryError(true)
      } finally {
        if (!controller.signal.aborted) setLoading(false)
      }
    }
    void load()
    return () => controller.abort()
  }, [stream.stream_id, stream.request_id])
  const attempts = history.length > stream.attempts.length ? history : stream.attempts
  return (
    <div className="min-w-0 bg-muted/20 p-4 sm:px-5">
      <div className="grid grid-cols-2 gap-3 text-xs sm:grid-cols-3">
        <div className="min-w-0">
          <div className="text-muted-foreground">{t('liveStreams.requestId')}</div>
          <div className="mt-1 break-all font-mono text-muted-foreground">{stream.request_id}</div>
        </div>
        <div>
          <div className="text-muted-foreground">{t('liveStreams.protocol')}</div>
          <div className="mt-1 font-medium uppercase text-foreground">{stream.protocol || 'openai'}</div>
        </div>
        <div className="min-w-0">
          <div className="text-muted-foreground">{t('liveStreams.apiKey')}</div>
          <div className="mt-1 truncate text-foreground">{stream.api_key_name || (stream.api_key_id ? `#${stream.api_key_id}` : '-')}</div>
        </div>
        <div>
          <div className="text-muted-foreground">{t('liveStreams.attemptCount')}</div>
          <div className="mt-1 font-medium tabular-nums text-foreground">{stream.attempt_count}</div>
        </div>
        <div>
          <div className="text-muted-foreground">{t('liveStreams.switchCount')}</div>
          <div className="mt-1 font-medium tabular-nums text-foreground">{stream.switch_count}</div>
        </div>
      </div>
      {disconnected && stream.disconnect_reason ? (
        <p className="mt-3 whitespace-pre-wrap break-words text-xs leading-relaxed text-red-700 [overflow-wrap:anywhere] dark:text-red-300">
          <span className="font-medium">{t('liveStreams.reason')}：</span>
          {t(liveStreamDisconnectReasonKey(stream.disconnect_reason))}
        </p>
      ) : null}
      <div className="my-3 flex items-center gap-2 text-xs text-muted-foreground" role="status">
        <span className="font-medium text-foreground">{t('liveStreams.attemptDetails')}</span>
        {loading ? <RefreshCw className="size-3 animate-spin" aria-label={t('common.loading')} /> : null}
        {historyError ? <span>{t('liveStreams.historyUnavailable')}</span> : null}
      </div>
      <div className="overflow-hidden rounded-md border border-border bg-background/70">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="w-16">#</TableHead>
              <TableHead>{t('concurrency.relayAccount')}</TableHead>
              <TableHead>{t('concurrency.relayStatusCode')}</TableHead>
              <TableHead>{t('concurrency.relayDecision')}</TableHead>
              <TableHead>{t('concurrency.relayDuration')}</TableHead>
              <TableHead>{t('concurrency.relayError')}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {attempts.length === 0 ? <TableRow><TableCell colSpan={6} className="py-6 text-center text-muted-foreground">{t('concurrency.noAttempts')}</TableCell></TableRow> : null}
            {attempts.map((attempt) => {
              const statusOK = attempt.status_code >= 200 && attempt.status_code < 300
              return (
                <TableRow key={`${stream.request_id}-detail-${attempt.seq}`}>
                  <TableCell className="tabular-nums text-muted-foreground">{attempt.seq}</TableCell>
                  <TableCell className="max-w-56 font-medium"><StreamAccountLabel attempt={attempt} t={t} /></TableCell>
                  <TableCell className={cn('tabular-nums', attempt.status_code === 499 ? 'text-muted-foreground' : statusOK ? 'text-emerald-600 dark:text-emerald-400' : attempt.status_code >= 400 ? 'text-red-600 dark:text-red-400' : 'text-muted-foreground')}>{attempt.status_code || '-'}</TableCell>
                  <TableCell>
                    <span className={cn('rounded border px-1.5 py-0.5 text-[11px]', attempt.decision === 'canceled' ? 'border-border bg-muted text-muted-foreground' : attempt.decision === 'success' ? 'border-emerald-500/25 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300' : attempt.decision === 'failed' ? 'border-red-500/25 bg-red-500/10 text-red-700 dark:text-red-300' : attempt.decision === 'partial_failed' ? 'border-orange-500/25 bg-orange-500/10 text-orange-700 dark:text-orange-300' : 'border-amber-500/25 bg-amber-500/10 text-amber-700 dark:text-amber-300')}>
                      {t(`concurrency.relayDecisionValues.${attempt.decision}`, { defaultValue: attempt.decision || '-' })}
                    </span>
                  </TableCell>
                  <TableCell className="whitespace-nowrap tabular-nums text-muted-foreground">{!attempt.status_code && !attempt.duration_ms ? '-' : formatDuration(attempt.duration_ms)}</TableCell>
                  <TableCell className="min-w-56 max-w-96 whitespace-pre-wrap break-words text-xs text-red-700 dark:text-red-300 [overflow-wrap:anywhere]">{attempt.error || (attempt.status_code >= 400 ? t('concurrency.relayReasonHTTPOnly', { code: attempt.status_code }) : '-')}</TableCell>
                </TableRow>
              )
            })}
          </TableBody>
        </Table>
      </div>
    </div>
  )
}

export default function LiveStreams() {
  const { t } = useTranslation()
  const [streams, setStreams] = useState<LiveStream[]>([])
  const [truncated, setTruncated] = useState(false)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [expanded, setExpanded] = useState<string | null>(null)
  const [now, setNow] = useState(Date.now)
  const [updatedAt, setUpdatedAt] = useState<string | null>(null)
  const [tab, setTab] = useState<'streams' | 'anomalies'>('streams')
  const [query, setQuery] = useState('')
  const [statusFilter, setStatusFilter] = useState('all')
  const [anomalies, setAnomalies] = useState<UsageLog[]>([])
  const [anomaliesLoading, setAnomaliesLoading] = useState(true)
  const [anomaliesError, setAnomaliesError] = useState<string | null>(null)
  const [anomaliesRefresh, setAnomaliesRefresh] = useState(0)
  // Older compatible servers may drop a terminal row immediately; retain it
  // briefly so the reason does not flash away between polls.
  const recentlyGoneRef = useRef(new Map<string, { stream: LiveStream; disconnectedAt: number }>())

  const refreshRef = useRef<() => void>(() => {})
  const refresh = useCallback(() => { setLoading(true); refreshRef.current(); setAnomaliesRefresh((value) => value + 1) }, [])

  useEffect(() => {
    const loader = createSerialPoller({
      request: (signal) => api.getLiveStreams(signal),
      onResult: (result) => {
        const seen = new Map((result.streams ?? []).map((stream) => [stream.request_id, stream]))
        const nowMs = Date.now()
        // Anything that was visible last poll but is missing now just left
        // the server's in-flight registry. Keep it around briefly so its
        // disconnect status/reason is not a one-frame flash.
        for (const [id, entry] of recentlyGoneRef.current) {
          if (nowMs - entry.disconnectedAt > RECENTLY_DISCONNECTED_RETENTION_MS) {
            recentlyGoneRef.current.delete(id)
          }
        }
        const merged = [...seen.values()]
        for (const [id, entry] of recentlyGoneRef.current) {
          if (!seen.has(id)) merged.push(entry.stream)
        }
        setStreams(merged)
        setTruncated(result.truncated)
        setError(null)
        setLoading(false)
        setUpdatedAt(new Date().toLocaleTimeString())
      },
      onError: (err) => {
        setError(getErrorMessage(err))
        setLoading(false)
      },
    })
    refreshRef.current = () => { void loader.load() }
    void loader.load()
    const poll = () => {
      if (document.visibilityState === 'visible') { setNow(Date.now()); void loader.load() }
    }
    const timer = window.setInterval(poll, 2000)
    document.addEventListener('visibilitychange', poll)
    return () => {
      loader.stop()
      refreshRef.current = () => {}
      window.clearInterval(timer)
      document.removeEventListener('visibilitychange', poll)
    }
  }, [])

  // Track which currently-visible streams just transitioned to disconnected,
  // so the next poll (where the server may have already dropped them) still
  // has a copy to keep showing for the retention window.
  useEffect(() => {
    for (const stream of streams) {
      if (liveStreamIsDisconnected(stream) && !recentlyGoneRef.current.has(stream.request_id)) {
        recentlyGoneRef.current.set(stream.request_id, { stream, disconnectedAt: Date.now() })
      }
    }
  }, [streams])

  const activeStreamCount = useMemo(() => streams.filter((s) => liveStreamStatus(s) === 'in_progress').length, [streams])
  const completedCount = useMemo(() => streams.filter((s) => liveStreamStatus(s) === 'success').length, [streams])
  const disconnectedCount = useMemo(() => streams.filter((s) => liveStreamIsDisconnected(s)).length, [streams])
  const search = query.trim().toLocaleLowerCase()
  const filteredStreams = useMemo(() => streams.filter((stream) => {
    if (statusFilter !== 'all' && liveStreamStatus(stream) !== statusFilter) return false
    return !search || [stream.model, stream.request_id, stream.stream_id, stream.protocol, stream.api_key_name,
      ...stream.attempts.map((attempt) => attempt.account_name || String(attempt.account_id))]
      .some((value) => value?.toLocaleLowerCase().includes(search))
  }).sort((a, b) => Date.parse(b.started_at) - Date.parse(a.started_at)), [streams, statusFilter, search])
  const filteredAnomalies = useMemo(() => anomalies.filter((log) => !search ||
    [log.model, log.parent_request_id, log.account_name, String(log.account_id), String(log.status_code), log.error_message, log.upstream_error_kind]
      .some((value) => value?.toLocaleLowerCase().includes(search))), [anomalies, search])

  useEffect(() => {
    if (tab !== 'anomalies') return
    setAnomaliesLoading(true)
    const loader = createSerialPoller({
      request: () => api.getUsageLogsPaged({
        start: new Date(Date.now() - 24 * 3600_000).toISOString(), end: new Date().toISOString(),
        page: 1, pageSize: 30, errorOnly: 'true', stream: 'true',
      }),
      onResult: (result) => { setAnomalies(result.logs ?? []); setAnomaliesError(null); setAnomaliesLoading(false) },
      onError: (err) => { setAnomaliesError(getErrorMessage(err)); setAnomaliesLoading(false) },
    })
    void loader.load()
    const timer = window.setInterval(() => {
      if (document.visibilityState === 'visible') void loader.load()
    }, 15_000)
    return () => { loader.stop(); window.clearInterval(timer) }
  }, [tab, anomaliesRefresh])

  const showStream = (stream: LiveStream) => {
    setQuery('')
    setStatusFilter('all')
    setExpanded(stream.request_id)
    setTab('streams')
  }

  return (
    <div className="mx-auto w-full max-w-[1800px] space-y-4">
      <PageHeader
        title={t('liveStreams.title')}
        description={t('liveStreams.description')}
        onRefresh={refresh}
        actionMeta={updatedAt ? t('liveStreams.updatedAt', { time: updatedAt }) : undefined}
        className="mb-0 sm:mb-0"
      />
      <div className="flex flex-wrap items-center gap-x-6 gap-y-2 rounded-lg border border-border bg-card/70 px-4 py-3 text-xs">
        <span className="inline-flex items-center gap-2"><span className="size-1.5 rounded-full bg-blue-500" />{t('liveStreams.activeStreams')}<strong className="text-sm tabular-nums">{activeStreamCount}</strong></span>
        <span className="inline-flex items-center gap-2"><span className="size-1.5 rounded-full bg-emerald-500" />{t('liveStreams.statuses.success')}<strong className="text-sm tabular-nums">{completedCount}</strong></span>
        <span className="inline-flex items-center gap-2"><span className="size-1.5 rounded-full bg-amber-500" />{t('liveStreams.disconnectedRecently')}<strong className="text-sm tabular-nums">{disconnectedCount}</strong></span>
        <span className={cn('inline-flex items-center gap-1.5 text-muted-foreground sm:ml-auto', error && 'text-amber-700 dark:text-amber-300')}>
          {error ? <AlertTriangle className="size-3.5" /> : <Radio className="size-3.5" />}
          {t(error ? 'liveStreams.refreshFailed' : 'liveStreams.autoRefresh')}
        </span>
      </div>
      <section className="min-w-0 overflow-hidden rounded-xl border border-border bg-card/80 shadow-sm">
        <div className="flex flex-col gap-3 border-b border-border p-3 lg:flex-row lg:items-center lg:justify-between">
          <SegmentedTabs
            value={tab}
            onValueChange={(value) => { setTab(value as 'streams' | 'anomalies'); setQuery('') }}
            size="sm"
            className="w-fit shrink-0 border-0 bg-muted/40 shadow-none"
            tabs={[
              { value: 'streams', label: <><Radio className="size-3.5" />{t('liveStreams.streamTab')}</> },
              { value: 'anomalies', label: <><AlertTriangle className="size-3.5" />{t('liveStreams.anomalyTab')}</> },
            ]}
          />
          <div className="flex min-w-0 flex-col gap-2 sm:flex-row sm:items-center">
            <div className="relative min-w-0 sm:w-72">
              <Search className="pointer-events-none absolute left-3 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
              <Input value={query} onChange={(event) => setQuery(event.target.value)}
                placeholder={t('liveStreams.searchPlaceholder')} aria-label={t('liveStreams.searchPlaceholder')}
                className="h-9 pl-9" />
            </div>
            {tab === 'streams' ? <Select compact value={statusFilter} onValueChange={setStatusFilter} className="sm:w-40"
              options={['all', 'in_progress', 'success', 'failed', 'canceled', 'incomplete'].map((value) => ({
                value, label: value === 'all' ? t('liveStreams.allStatuses') : t('liveStreams.statuses.' + value),
              }))} /> : null}
          </div>
        </div>
        {error && streams.length > 0 ? <div role="alert" className="border-b border-amber-500/20 bg-amber-500/5 px-4 py-2 text-xs text-amber-700 dark:text-amber-300">{t('liveStreams.staleData')} {error}</div> : null}
        {truncated && tab === 'streams' ? <div role="status" className="border-b border-amber-500/20 bg-amber-500/5 px-4 py-2 text-xs text-amber-700 dark:text-amber-300">{t('liveStreams.truncatedHint')}</div> : null}
        {tab === 'streams' ? (
          <StateShell loading={loading && streams.length === 0} error={error && streams.length === 0 ? error : null}
            onRetry={refresh} loadingTitle={t('liveStreams.loading')} errorTitle={t('liveStreams.loadFailed')}>
            <Table className="min-w-[1080px] table-fixed">
              <TableHeader className="bg-muted/35">
                <TableRow className="hover:bg-transparent">
                  <TableHead className="w-11"><span className="sr-only">{t('liveStreams.details')}</span></TableHead>
                  <TableHead className="w-40">{t('liveStreams.startedAt')}</TableHead>
                  <TableHead className="w-44">{t('liveStreams.model')}</TableHead>
                  <TableHead className="w-24">{t('liveStreams.protocol')}</TableHead>
                  <TableHead>{t('liveStreams.accountChain')}</TableHead>
                  <TableHead className="w-20 text-right">{t('liveStreams.requests')}</TableHead>
                  <TableHead className="w-20 text-right">{t('liveStreams.attempts')}</TableHead>
                  <TableHead className="w-36">{t('liveStreams.status')}</TableHead>
                  <TableHead className="w-24 text-right">{t('liveStreams.elapsed')}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {filteredStreams.map((stream) => {
                  const isExpanded = expanded === stream.request_id
                  const reason = liveStreamIsDisconnected(stream) && stream.disconnect_reason
                    ? t(liveStreamDisconnectReasonKey(stream.disconnect_reason)) : ''
                  return (
                    <Fragment key={stream.request_id}>
                      <TableRow className={cn('group', isExpanded && 'bg-muted/40')}>
                        <TableCell className="px-2">
                          <Button variant="ghost" size="icon-sm" onClick={() => setExpanded(isExpanded ? null : stream.request_id)}
                            aria-expanded={isExpanded} aria-controls={'stream-detail-' + stream.request_id}
                            aria-label={t(isExpanded ? 'liveStreams.collapseDetails' : 'liveStreams.expandDetails', { model: stream.model || stream.request_id })}>
                            <ChevronRight className={cn('size-4 text-muted-foreground transition-transform', isExpanded && 'rotate-90')} />
                          </Button>
                        </TableCell>
                        <TableCell className="text-xs tabular-nums text-muted-foreground">{formatStreamTime(stream.started_at)}</TableCell>
                        <TableCell><span className="block truncate font-medium" title={stream.model}>{stream.model || '-'}</span></TableCell>
                        <TableCell><span className="rounded border border-border bg-muted/30 px-1.5 py-0.5 text-[10px] font-medium uppercase text-muted-foreground">{stream.protocol || 'openai'}</span></TableCell>
                        <TableCell>
                          <div className="flex min-w-0 items-center gap-1 overflow-hidden text-xs" title={stream.attempts.map((a) => a.account_name || '#' + a.account_id).join(' ? ')}>
                            {stream.attempts.length > 1 ? <><span className="min-w-0 max-w-24 truncate text-muted-foreground">{stream.attempts[0].account_name || '#' + stream.attempts[0].account_id}</span><ArrowRight className="size-3 shrink-0 text-muted-foreground" /></> : null}
                            {stream.attempts.length ? <StreamAccountLabel attempt={stream.attempts[stream.attempts.length - 1]} t={t} /> : <span className="text-muted-foreground">{t('concurrency.noAttempts')}</span>}
                          </div>
                        </TableCell>
                        <TableCell className="text-right text-xs tabular-nums">{stream.request_count ?? 1}</TableCell>
                        <TableCell className="text-right text-xs tabular-nums">{stream.attempt_count}</TableCell>
                        <TableCell><StreamStatusBadge stream={stream} t={t} />{reason ? <span className="mt-1 block truncate text-[11px] text-muted-foreground" title={reason}>{reason}</span> : null}</TableCell>
                        <TableCell className="text-right font-mono text-xs tabular-nums text-muted-foreground">{formatDuration(streamDuration(stream, now))}</TableCell>
                      </TableRow>
                      {isExpanded ? <TableRow className="hover:bg-transparent"><TableCell colSpan={9} className="whitespace-normal p-0"><div id={'stream-detail-' + stream.request_id}><StreamDetails stream={stream} t={t} /></div></TableCell></TableRow> : null}
                    </Fragment>
                  )
                })}
                {filteredStreams.length === 0 ? <TableRow className="hover:bg-transparent"><TableCell colSpan={9} className="h-48 text-center">
                  <Radio className="mx-auto mb-3 size-6 text-muted-foreground/50" />
                  <p className="text-sm text-muted-foreground">{t(streams.length ? 'liveStreams.noMatches' : 'liveStreams.noStreams')}</p>
                  {streams.length ? <Button variant="link" size="sm" className="mt-2" onClick={() => { setQuery(''); setStatusFilter('all') }}>{t('liveStreams.clearFilters')}</Button> : null}
                </TableCell></TableRow> : null}
              </TableBody>
            </Table>
          </StateShell>
        ) : (
          <StateShell loading={anomaliesLoading && anomalies.length === 0} error={anomaliesError && anomalies.length === 0 ? anomaliesError : null}
            onRetry={() => setAnomaliesRefresh((value) => value + 1)}>
            {anomaliesError && anomalies.length > 0 ? <p role="alert" className="px-4 py-2 text-xs text-amber-700 dark:text-amber-300">{t('liveStreams.staleData')} {anomaliesError}</p> : null}
            <Table className="min-w-[960px] table-fixed">
              <TableHeader className="bg-muted/35"><TableRow className="hover:bg-transparent">
                <TableHead className="w-40">{t('liveStreams.startedAt')}</TableHead>
                <TableHead className="w-44">{t('liveStreams.model')}</TableHead>
                <TableHead className="w-40">{t('concurrency.relayAccount')}</TableHead>
                <TableHead className="w-24">{t('concurrency.relayStatusCode')}</TableHead>
                <TableHead>{t('concurrency.relayError')}</TableHead>
                <TableHead className="w-24 text-right">{t('liveStreams.details')}</TableHead>
              </TableRow></TableHeader>
              <TableBody>
                {filteredAnomalies.map((log) => {
                  const stream = streams.find((item) => item.request_id === log.parent_request_id || item.stream_id === log.parent_request_id)
                  const reason = log.error_message || log.upstream_error_kind || t('liveStreams.disconnectReasons.upstream_error')
                  return <TableRow key={log.id}>
                    <TableCell className="text-xs tabular-nums text-muted-foreground">{formatStreamTime(log.created_at)}</TableCell>
                    <TableCell><span className="block truncate font-medium" title={log.model}>{log.model || '-'}</span></TableCell>
                    <TableCell><span className="block truncate text-xs" title={log.account_name}>{log.account_name || '#' + log.account_id}</span></TableCell>
                    <TableCell><span className="rounded bg-red-500/10 px-2 py-1 font-mono text-xs text-red-700 dark:text-red-400">{log.status_code || '-'}</span></TableCell>
                    <TableCell><span className="block truncate text-xs text-muted-foreground" title={reason}>{reason}</span></TableCell>
                    <TableCell className="text-right"><Button variant="ghost" size="xs" disabled={!stream} title={!stream ? t('liveStreams.streamUnavailable') : undefined}
                      onClick={() => { if (stream) showStream(stream) }}>{t('liveStreams.viewStream')}<ExternalLink className="size-3" /></Button></TableCell>
                  </TableRow>
                })}
                {filteredAnomalies.length === 0 ? <TableRow className="hover:bg-transparent"><TableCell colSpan={6} className="h-48 text-center text-sm text-muted-foreground">{t(search ? 'liveStreams.noMatches' : 'liveStreams.noAnomalies')}</TableCell></TableRow> : null}
              </TableBody>
            </Table>
          </StateShell>
        )}
        <div className="flex flex-wrap items-center justify-between gap-2 border-t border-border bg-muted/15 px-4 py-2.5 text-xs text-muted-foreground">
          <span>{t('liveStreams.showingCount', { count: tab === 'streams' ? filteredStreams.length : filteredAnomalies.length, total: tab === 'streams' ? streams.length : anomalies.length })}</span>
          <span>{t(tab === 'streams' ? 'liveStreams.tableHint' : 'liveStreams.anomalyHint')}</span>
        </div>
      </section>
    </div>
  )
}
