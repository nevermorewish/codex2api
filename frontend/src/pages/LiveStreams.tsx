import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import type { TFunction } from 'i18next'
import { Activity, ArrowRight, CheckCircle2, ChevronDown, CircleHelp, CircleSlash, LoaderCircle, Radio, XCircle } from 'lucide-react'
import { api } from '../api'
import type { LiveStream, RelayAttempt } from '../types'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import { StatTile } from '../components/StatTile'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { getErrorMessage } from '../utils/error'
import { cn } from '@/lib/utils'
import { liveStreamDisconnectReasonKey } from '@/lib/liveStreamDisconnectReason'
import { liveStreamElapsedMs, liveStreamIsDisconnected, liveStreamStatus } from '@/lib/liveStreamStatus'
import { createSerialPoller } from '@/lib/serialPoller'

// A stream leaves the backend's in-flight registry the instant its final
// attempt is recorded, so a poll only ever gets one chance to see it in a
// disconnected state before it stops being returned at all. Retaining it
// client-side for this long keeps the disconnect reason visible without any
// server-side history buffer (see the design plan's grace-window discussion).
const RECENTLY_DISCONNECTED_RETENTION_MS = 30_000

function StreamStatusBadge({ stream, t }: { stream: LiveStream; t: TFunction }) {
  const status = liveStreamStatus(stream)
  const Icon = status === 'success' ? CheckCircle2 : status === 'failed' ? XCircle : status === 'in_progress' ? LoaderCircle : status === 'canceled' ? CircleSlash : CircleHelp
  return (
    <span className={cn('inline-flex items-center gap-1 text-xs font-medium', status === 'success' ? 'text-emerald-600 dark:text-emerald-400' : status === 'failed' ? 'text-red-600 dark:text-red-400' : status === 'in_progress' ? 'text-blue-600 dark:text-blue-400' : 'text-muted-foreground')}>
      <Icon className={cn('size-4 shrink-0', status === 'in_progress' && 'animate-spin')} />
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

function formatStreamTime(value: string): string {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value || '-'
  return date.toLocaleString(undefined, {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit',
  })
}

function StreamDetails({ stream, t }: { stream: LiveStream; t: TFunction }) {
  const disconnected = liveStreamIsDisconnected(stream)
  return (
    <div className="border-t border-border bg-muted/15 px-4 py-4 pl-11">
      <div className="grid grid-cols-2 gap-3 text-xs sm:grid-cols-3">
        <div className="min-w-0">
          <div className="text-muted-foreground">{t('liveStreams.requestId')}</div>
          <div className="mt-1 truncate font-mono text-foreground" title={stream.request_id}>{stream.request_id}</div>
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
      <div className="mt-4 overflow-x-auto rounded-md border border-border bg-background/50">
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
            {stream.attempts.map((attempt) => {
              const statusOK = attempt.status_code >= 200 && attempt.status_code < 300
              return (
                <TableRow key={`${stream.request_id}-detail-${attempt.seq}`}>
                  <TableCell className="tabular-nums text-muted-foreground">{attempt.seq}</TableCell>
                  <TableCell className="max-w-56 font-medium"><StreamAccountLabel attempt={attempt} t={t} /></TableCell>
                  <TableCell className={cn('tabular-nums', attempt.status_code === 499 ? 'text-muted-foreground' : statusOK ? 'text-emerald-600 dark:text-emerald-400' : attempt.status_code >= 400 ? 'text-red-600 dark:text-red-400' : 'text-muted-foreground')}>{attempt.status_code || '-'}</TableCell>
                  <TableCell>
                    <span className={cn('rounded border px-1.5 py-0.5 text-[11px]', attempt.decision === 'canceled' ? 'border-border bg-muted text-muted-foreground' : attempt.decision === 'success' ? 'border-emerald-500/25 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300' : attempt.decision === 'failed' ? 'border-red-500/25 bg-red-500/10 text-red-700 dark:text-red-300' : 'border-amber-500/25 bg-amber-500/10 text-amber-700 dark:text-amber-300')}>
                      {t(`concurrency.relayDecisionValues.${attempt.decision}`, { defaultValue: attempt.decision || '-' })}
                    </span>
                  </TableCell>
                  <TableCell className="whitespace-nowrap tabular-nums text-muted-foreground">{formatDuration(attempt.duration_ms)}</TableCell>
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
  // Recently-disconnected streams are retained client-side for a short grace
  // window after the server stops returning them (see the constant above).
  const recentlyGoneRef = useRef(new Map<string, { stream: LiveStream; disconnectedAt: number }>())

  const refreshRef = useRef<() => void>(() => {})
  const refresh = useCallback(() => { setLoading(true); refreshRef.current() }, [])

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

  const sorted = useMemo(
    () => [...streams].sort((a, b) => Date.parse(b.started_at) - Date.parse(a.started_at)),
    [streams],
  )
  const activeStreamCount = useMemo(() => streams.filter((s) => liveStreamStatus(s) === 'in_progress').length, [streams])
  const activeAttemptCount = useMemo(
    () => streams.reduce((sum, s) => sum + (liveStreamStatus(s) === 'in_progress' ? 1 : 0), 0),
    [streams],
  )
  const disconnectedCount = useMemo(() => streams.filter((s) => liveStreamIsDisconnected(s)).length, [streams])

  return (
    <div className="mx-auto w-full max-w-[1800px]">
      <PageHeader
        title={t('liveStreams.title')}
        description={t('liveStreams.description')}
        onRefresh={refresh}
        actionMeta={updatedAt ? t('liveStreams.updatedAt', { time: updatedAt }) : undefined}
      />
      <StateShell
        variant="page"
        loading={loading && streams.length === 0}
        error={error && streams.length === 0 ? error : null}
        onRetry={refresh}
        loadingTitle={t('liveStreams.loading')}
        errorTitle={t('liveStreams.loadFailed')}
      >
        <div className="space-y-5">
          <div className="grid grid-cols-2 gap-2.5 lg:grid-cols-3">
            <StatTile label={t('liveStreams.activeStreams')} value={activeStreamCount} icon={<Radio className="size-4" />} tone="info" />
            <StatTile label={t('liveStreams.activeAttempts')} value={activeAttemptCount} icon={<Activity className="size-4" />} tone="success" />
            <StatTile label={t('liveStreams.disconnectedRecently')} value={disconnectedCount} icon={<CircleSlash className="size-4" />} tone={disconnectedCount > 0 ? 'warning' : 'neutral'} />
          </div>
          {truncated ? (
            <div className="rounded-md border border-amber-500/25 bg-amber-500/10 px-3 py-2 text-xs text-amber-700 dark:text-amber-300">
              {t('liveStreams.truncatedHint')}
            </div>
          ) : null}
          <section className="overflow-hidden rounded-lg border border-border bg-card/70">
            {sorted.length > 0 ? (
              <div className="divide-y divide-border">
                {sorted.map((stream) => {
                  const isExpanded = expanded === stream.request_id
                  return (
                    <div key={stream.request_id} className="group">
                      <button
                        type="button"
                        className="flex w-full flex-wrap items-center gap-2 px-2 py-3 text-left transition-colors hover:bg-muted/35"
                        onClick={() => setExpanded(isExpanded ? null : stream.request_id)}
                        aria-expanded={isExpanded}
                      >
                        <ChevronDown className={cn('size-4 shrink-0 text-muted-foreground transition-transform', isExpanded && 'rotate-180')} />
                        <div className="min-w-0 flex-1 basis-40">
                          <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
                            <span className="text-xs tabular-nums text-muted-foreground">{formatStreamTime(stream.started_at)}</span>
                            <span className="min-w-0 break-all font-medium text-foreground">{stream.model || '-'}</span>
                            <span className="rounded border border-border bg-muted/40 px-1.5 py-0.5 text-[11px] uppercase text-muted-foreground">{stream.protocol || 'openai'}</span>
                          </div>
                          <div className="mt-1 flex min-w-0 flex-wrap items-center gap-x-1 gap-y-1 text-xs text-muted-foreground">
                            {stream.attempts.map((attempt, index) => (
                              <span key={`${stream.request_id}-${attempt.seq}`} className="inline-flex min-w-0 items-center gap-1">
                                {index > 0 ? <ArrowRight className="size-3 shrink-0 text-muted-foreground/70" /> : null}
                                <StreamAccountLabel attempt={attempt} t={t} />
                              </span>
                            ))}
                            {stream.attempts.length === 0 ? <span>{t('concurrency.noAttempts')}</span> : null}
                          </div>
                          {liveStreamIsDisconnected(stream) && stream.disconnect_reason ? (
                            <span className="mt-1 block whitespace-pre-wrap break-words text-xs leading-relaxed text-red-700 [overflow-wrap:anywhere] dark:text-red-300">
                              {t(liveStreamDisconnectReasonKey(stream.disconnect_reason))}
                            </span>
                          ) : null}
                        </div>
                        <div className="ml-auto flex shrink-0 items-center gap-2">
                          <span className="text-xs text-muted-foreground">{t('liveStreams.attempts')}: {stream.attempt_count}</span>
                          <StreamStatusBadge stream={stream} t={t} />
                          <span className="text-right text-xs tabular-nums text-muted-foreground">{formatDuration(liveStreamElapsedMs(stream, now))}</span>
                        </div>
                      </button>
                      {isExpanded ? <StreamDetails stream={stream} t={t} /> : null}
                    </div>
                  )
                })}
              </div>
            ) : (
              <div className="px-4 py-10 text-center text-sm text-muted-foreground">{t('liveStreams.noStreams')}</div>
            )}
          </section>
        </div>
      </StateShell>
    </div>
  )
}
