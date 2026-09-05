import { useCallback, useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import type { TFunction } from 'i18next'
import { Activity, ArrowRight, CheckCircle2, ChevronDown, Gauge, GitBranch, KeyRound, Layers3, Search, Users, XCircle } from 'lucide-react'
import { api } from '../api'
import type { ConcurrencyAccountRow, ConcurrencySnapshot, RelayAttempt, RelayChain } from '../types'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import Pagination from '../components/Pagination'
import { StatTile } from '../components/StatTile'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { getErrorMessage } from '../utils/error'
import { cn } from '@/lib/utils'

const RELAY_PAGE_SIZE = 20

function utilizationWidth(value: number): string {
  return `${Math.max(0, Math.min(100, value))}%`
}

function loadTone(row: ConcurrencyAccountRow): string {
  if (!row.available) return 'bg-muted-foreground/35'
  if (row.utilization >= 90) return 'bg-red-500'
  if (row.utilization >= 65) return 'bg-amber-500'
  return 'bg-emerald-500'
}

function formatDuration(value: number): string {
  if (!Number.isFinite(value) || value < 0) return '-'
  if (value >= 1000) return `${(value / 1000).toFixed(value >= 10_000 ? 0 : 1)} s`
  return `${Math.round(value)} ms`
}

function formatRelayTime(value: string): string {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value || '-'
  return date.toLocaleString(undefined, {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit',
  })
}

function RelayChainDetails({ chain, t }: { chain: RelayChain; t: TFunction }) {
  return (
    <div className="border-t border-border bg-muted/15 px-4 py-4 pl-11">
      <div className="grid grid-cols-2 gap-3 text-xs sm:grid-cols-3">
        <div className="min-w-0">
          <div className="text-muted-foreground">{t('concurrency.relayRequestId')}</div>
          <div className="mt-1 truncate font-mono text-foreground" title={chain.request_id}>{chain.request_id}</div>
        </div>
        <div>
          <div className="text-muted-foreground">{t('concurrency.relayProtocol')}</div>
          <div className="mt-1 font-medium uppercase text-foreground">{chain.protocol || 'openai'}</div>
        </div>
        <div className="min-w-0">
          <div className="text-muted-foreground">{t('concurrency.apiKey')}</div>
          <div className="mt-1 truncate text-foreground">{chain.api_key_name || (chain.api_key_id ? `#${chain.api_key_id}` : '-')}</div>
        </div>
        <div>
          <div className="text-muted-foreground">{t('concurrency.relayResult')}</div>
          <div className={cn('mt-1 font-medium', chain.final_ok ? 'text-emerald-600 dark:text-emerald-400' : 'text-red-600 dark:text-red-400')}>
            {chain.final_ok ? t('concurrency.relaySuccess') : t('concurrency.relayFailed')}
          </div>
        </div>
        <div>
          <div className="text-muted-foreground">{t('concurrency.relayAttemptsLabel')}</div>
          <div className="mt-1 font-medium tabular-nums text-foreground">{chain.attempts.length}</div>
        </div>
        <div>
          <div className="text-muted-foreground">{t('concurrency.relaySwitchesLabel')}</div>
          <div className="mt-1 font-medium tabular-nums text-foreground">{chain.switch_count}</div>
        </div>
      </div>

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
            {chain.attempts.map((attempt: RelayAttempt) => {
              const statusOK = attempt.status_code >= 200 && attempt.status_code < 300
              return (
                <TableRow key={`${chain.request_id}-detail-${attempt.seq}`}>
                  <TableCell className="tabular-nums text-muted-foreground">{attempt.seq}</TableCell>
                  <TableCell className="max-w-56 truncate font-medium" title={attempt.account_name}>{attempt.account_name || (attempt.account_id ? `#${attempt.account_id}` : t('concurrency.unknownAccount'))}</TableCell>
                  <TableCell className={cn('tabular-nums', statusOK ? 'text-emerald-600 dark:text-emerald-400' : attempt.status_code >= 400 ? 'text-red-600 dark:text-red-400' : 'text-muted-foreground')}>{attempt.status_code || '-'}</TableCell>
                  <TableCell>
                    <span className={cn('rounded border px-1.5 py-0.5 text-[11px]', attempt.decision === 'success' ? 'border-emerald-500/25 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300' : attempt.decision === 'failed' ? 'border-red-500/25 bg-red-500/10 text-red-700 dark:text-red-300' : 'border-amber-500/25 bg-amber-500/10 text-amber-700 dark:text-amber-300')}>
                      {t(`concurrency.relayDecisionValues.${attempt.decision}`, { defaultValue: attempt.decision || '-' })}
                    </span>
                  </TableCell>
                  <TableCell className="whitespace-nowrap tabular-nums text-muted-foreground">{formatDuration(attempt.duration_ms)}</TableCell>
                  <TableCell className="max-w-72 truncate text-xs text-red-700 dark:text-red-300" title={attempt.error || undefined}>{attempt.error || '-'}</TableCell>
                </TableRow>
              )
            })}
          </TableBody>
        </Table>
      </div>
    </div>
  )
}

export default function Concurrency() {
  const { t } = useTranslation()
  const [snapshot, setSnapshot] = useState<ConcurrencySnapshot | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [search, setSearch] = useState('')
  const [hideIdle, setHideIdle] = useState(false)
  const [relayChains, setRelayChains] = useState<RelayChain[]>([])
  const [expandedRelay, setExpandedRelay] = useState<string | null>(null)
  const [relayPage, setRelayPage] = useState(1)
  const [relayTotal, setRelayTotal] = useState(0)
  const [relayLoading, setRelayLoading] = useState(true)
  const [relayError, setRelayError] = useState<string | null>(null)
  const [relayRefresh, setRelayRefresh] = useState(0)

  const refresh = useCallback(async (quiet = false) => {
    if (!quiet) setLoading(true)
    try {
      setSnapshot(await api.getConcurrency())
      setError(null)
    } catch (err) {
      setError(getErrorMessage(err))
    } finally {
      if (!quiet) setLoading(false)
    }
  }, [])

  useEffect(() => {
    let active = true
    let inFlight = false
    setRelayLoading(true)
    setExpandedRelay(null)
    const load = async () => {
      if (inFlight) return
      inFlight = true
      try {
        const result = await api.getRelayChains(relayPage)
        if (!active) return
        const lastPage = Math.max(1, Math.ceil(result.total / RELAY_PAGE_SIZE))
        if (relayPage > lastPage) {
          setRelayPage(lastPage)
          return
        }
        setRelayChains(result.chains ?? [])
        setRelayTotal(result.total)
        setRelayError(null)
      } catch (err) {
        if (active) setRelayError(getErrorMessage(err))
      } finally {
        inFlight = false
        if (active) setRelayLoading(false)
      }
    }
    void load()
    const poll = () => {
      if (document.visibilityState === 'visible') void load()
    }
    const timer = window.setInterval(poll, 2000)
    document.addEventListener('visibilitychange', poll)
    return () => {
      active = false
      window.clearInterval(timer)
      document.removeEventListener('visibilitychange', poll)
    }
  }, [relayPage, relayRefresh])

  useEffect(() => {
    void refresh(false)
  }, []) // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    const poll = () => {
      if (document.visibilityState === 'visible') void refresh(true)
    }
    const timer = window.setInterval(poll, 2000)
    document.addEventListener('visibilitychange', poll)
    return () => {
      window.clearInterval(timer)
      document.removeEventListener('visibilitychange', poll)
    }
  }, [refresh])

  const accounts = useMemo(() => {
    const query = search.trim().toLowerCase()
    return (snapshot?.accounts ?? []).filter((row) => {
      if (hideIdle && row.occupied === 0) return false
      if (!query) return true
      return [row.name, row.channel, String(row.id), ...row.group_names]
        .some((value) => value.toLowerCase().includes(query))
    })
  }, [snapshot, search, hideIdle])

  const updatedAt = snapshot?.collected_at
    ? new Date(snapshot.collected_at).toLocaleTimeString()
    : '-'

  return (
    <div className="mx-auto w-full max-w-[1800px]">
      <PageHeader
        title={t('concurrency.title')}
        description={t('concurrency.description')}
        onRefresh={() => { void refresh(false); setRelayRefresh((value) => value + 1) }}
        actionMeta={t('concurrency.updatedAt', { time: updatedAt })}
      />
      <StateShell
        variant="page"
        loading={loading && !snapshot}
        error={error && !snapshot ? error : null}
        onRetry={() => void refresh(false)}
        loadingTitle={t('concurrency.loading')}
        errorTitle={t('concurrency.loadFailed')}
      >
        {snapshot ? (
          <div className="space-y-5">
            <div className="grid grid-cols-2 gap-2.5 lg:grid-cols-5">
              <StatTile label={t('concurrency.globalActive')} value={snapshot.global_active} icon={<Activity className="size-4" />} tone="info" />
              <StatTile label={t('concurrency.queueDepth')} value={snapshot.queue_depth} icon={<Layers3 className="size-4" />} tone={snapshot.queue_depth > 0 ? 'warning' : 'neutral'} />
              <StatTile label={t('concurrency.active')} value={snapshot.total_active} icon={<Gauge className="size-4" />} tone="success" />
              <StatTile label={t('concurrency.occupied')} value={snapshot.total_occupied} sub={t('concurrency.bufferedCount', { count: Math.max(0, snapshot.total_occupied - snapshot.total_active) })} icon={<Users className="size-4" />} />
              <StatTile label={t('concurrency.capacity')} value={snapshot.capacity} sub={t('concurrency.usableCapacity')} icon={<Gauge className="size-4" />} />
            </div>

            <div className="grid items-start gap-5 lg:grid-cols-2">
            <section className="min-w-0" aria-label={t('concurrency.relayChainsTitle')}>
              <div className="flex flex-wrap items-center justify-between gap-3 border-b border-border py-3">
                <div className="flex items-start gap-2">
                  <GitBranch className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
                  <div>
                    <h3 className="font-semibold text-foreground">{t('concurrency.relayChainsTitle')}</h3>
                    <p className="text-xs text-muted-foreground">{t('concurrency.relayChainsDescription')}</p>
                  </div>
                </div>
                <div className="flex items-center gap-3">
                  <span className="text-xs text-muted-foreground">{t('concurrency.relayChainsRecords', { count: relayTotal })}</span>
                </div>
              </div>
              <StateShell loading={relayLoading} error={relayError} onRetry={() => setRelayRefresh((value) => value + 1)}>
              {relayChains.length > 0 ? (
                <div className="divide-y divide-border">
                  {relayChains.map((chain) => {
                    const expanded = expandedRelay === chain.request_id
                    return (
                      <div key={chain.request_id} className="group">
                        <button
                          type="button"
                          className="flex w-full flex-wrap items-center gap-2 px-2 py-3 text-left transition-colors hover:bg-muted/35"
                          onClick={() => setExpandedRelay(expanded ? null : chain.request_id)}
                          aria-expanded={expanded}
                        >
                          <ChevronDown className={cn('size-4 shrink-0 text-muted-foreground transition-transform', expanded && 'rotate-180')} />
                          <div className="min-w-0 flex-1 basis-40">
                            <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
                              <span className="text-xs tabular-nums text-muted-foreground">{formatRelayTime(chain.started_at)}</span>
                              <span className="min-w-0 break-all font-medium text-foreground">{chain.model || '-'}</span>
                              <span className="rounded border border-border bg-muted/40 px-1.5 py-0.5 text-[11px] uppercase text-muted-foreground">{chain.protocol || 'openai'}</span>
                            </div>
                            <div className="mt-1 flex min-w-0 flex-wrap items-center gap-x-1 gap-y-1 text-xs text-muted-foreground">
                              {chain.attempts.map((attempt, index) => (
                                <span key={`${chain.request_id}-${attempt.seq}`} className="inline-flex min-w-0 items-center gap-1">
                                  {index > 0 ? <ArrowRight className="size-3 shrink-0 text-muted-foreground/70" /> : null}
                                  <span className="max-w-44 truncate">{attempt.account_name || (attempt.account_id ? `#${attempt.account_id}` : t('concurrency.unknownAccount'))}</span>
                                </span>
                              ))}
                              {chain.attempts.length === 0 ? <span>{t('concurrency.noAttempts')}</span> : null}
                            </div>
                          </div>
                          <div className="ml-auto flex shrink-0 items-center gap-2">
                            <span className="hidden text-xs text-muted-foreground sm:inline">{t('concurrency.relayAttempts', { count: chain.attempts.length })}</span>
                            <span className={cn('inline-flex items-center gap-1 text-xs font-medium', chain.final_ok ? 'text-emerald-600 dark:text-emerald-400' : 'text-red-600 dark:text-red-400')}>
                              {chain.final_ok ? <CheckCircle2 className="size-4" /> : <XCircle className="size-4" />}
                              <span className="hidden sm:inline">{chain.final_ok ? t('concurrency.relaySuccess') : t('concurrency.relayFailed')}</span>
                            </span>
                            <span className="w-14 text-right text-xs tabular-nums text-muted-foreground">{formatDuration(chain.total_ms)}</span>
                          </div>
                        </button>
                        {expanded ? <RelayChainDetails chain={chain} t={t} /> : null}
                      </div>
                    )
                  })}
                </div>
              ) : (
                <div className="px-4 py-10 text-center text-sm text-muted-foreground">{t('concurrency.noRelayChains')}</div>
              )}
              </StateShell>
              <Pagination page={relayPage} totalPages={Math.ceil(relayTotal / RELAY_PAGE_SIZE)} totalItems={relayTotal} pageSize={RELAY_PAGE_SIZE} onPageChange={setRelayPage} />
            </section>

            <section className="min-w-0" aria-label={t('concurrency.accountsTitle')}>
              <div className="flex flex-col gap-3 border-b border-border py-3">
                <div>
                  <h3 className="font-semibold text-foreground">{t('concurrency.accountsTitle')}</h3>
                  <p className="text-xs text-muted-foreground">{t('concurrency.accountsCount', { visible: accounts.length, total: snapshot.accounts.length })}</p>
                </div>
                <div className="flex flex-wrap items-center gap-2">
                  <label className="relative min-w-0 flex-1 basis-40">
                    <Search className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                    <Input value={search} onChange={(event) => setSearch(event.target.value)} placeholder={t('concurrency.searchPlaceholder')} className="pl-8" />
                  </label>
                  <label className="flex h-9 items-center gap-2 whitespace-nowrap text-sm text-muted-foreground">
                    <Switch checked={hideIdle} onCheckedChange={setHideIdle} />
                    {t('concurrency.hideIdle')}
                  </label>
                </div>
              </div>
              <div className="overflow-x-auto">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>{t('concurrency.account')}</TableHead>
                      <TableHead className="min-w-28">{t('concurrency.pressure')}</TableHead>
                      <TableHead className="text-right">{t('concurrency.active')}</TableHead>
                      <TableHead className="text-right">{t('concurrency.occupied')}</TableHead>
                      <TableHead className="text-right">{t('concurrency.limit')}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {accounts.map((row) => (
                      <TableRow key={`${row.fallback ? 'f' : 'a'}-${row.id}`}>
                        <TableCell>
                          <div className="min-w-36">
                            <div className="max-w-48 truncate font-medium text-foreground" title={row.name}>{row.name}</div>
                            <div className="text-xs text-muted-foreground">#{row.id} · {row.status}</div>
                            <div className="mt-1 flex max-w-48 items-center gap-1.5 text-xs text-muted-foreground">
                              <span className={cn('shrink-0', row.fallback && 'text-amber-700 dark:text-amber-300')}>
                                {t(`concurrency.channels.${row.channel}`, { defaultValue: row.channel })}
                              </span>
                              <span className="truncate" title={row.group_names.join(', ')}>{row.group_names.join(', ') || '-'}</span>
                            </div>
                          </div>
                        </TableCell>
                        <TableCell>
                          <div className="flex items-center gap-2">
                            <div className="h-2 flex-1 overflow-hidden rounded-full bg-muted">
                              <div className={cn('h-full rounded-full transition-[width]', loadTone(row))} style={{ width: utilizationWidth(row.utilization) }} />
                            </div>
                            <span className="w-11 text-right text-xs tabular-nums text-muted-foreground">{Math.round(row.utilization)}%</span>
                          </div>
                          {row.buffered > 0 ? <div className="mt-1 text-[11px] text-amber-600 dark:text-amber-400">{t('concurrency.bufferedCount', { count: row.buffered })}</div> : null}
                        </TableCell>
                        <TableCell className="text-right tabular-nums">{row.active}</TableCell>
                        <TableCell className="text-right tabular-nums">{row.occupied}</TableCell>
                        <TableCell className="text-right tabular-nums">{row.limit}</TableCell>
                      </TableRow>
                    ))}
                    {accounts.length === 0 ? (
                      <TableRow><TableCell colSpan={5} className="h-24 text-center text-muted-foreground">{t('concurrency.noAccounts')}</TableCell></TableRow>
                    ) : null}
                  </TableBody>
                </Table>
              </div>
            </section>
            </div>

            <div className="grid gap-5 xl:grid-cols-2">
              <section className="overflow-hidden rounded-lg border border-border bg-card/70">
                <div className="border-b border-border px-4 py-3">
                  <h3 className="font-semibold text-foreground">{t('concurrency.groupsTitle')}</h3>
                </div>
                <div className="overflow-x-auto">
                  <Table>
                    <TableHeader><TableRow><TableHead>{t('concurrency.group')}</TableHead><TableHead className="text-right">{t('concurrency.accounts')}</TableHead><TableHead className="text-right">{t('concurrency.occupied')}</TableHead><TableHead className="text-right">{t('concurrency.capacity')}</TableHead></TableRow></TableHeader>
                    <TableBody>
                      {snapshot.groups.map((group) => (
                        <TableRow key={group.id}>
                          <TableCell><span className="inline-flex items-center gap-2"><span className="size-2.5 rounded-full border border-black/10" style={{ backgroundColor: group.color || '#64748b' }} />{group.name}</span></TableCell>
                          <TableCell className="text-right tabular-nums">{group.account_count}</TableCell>
                          <TableCell className="text-right tabular-nums">{group.occupied}</TableCell>
                          <TableCell className="text-right tabular-nums">{group.capacity}</TableCell>
                        </TableRow>
                      ))}
                      {snapshot.groups.length === 0 ? <TableRow><TableCell colSpan={4} className="h-20 text-center text-muted-foreground">{t('concurrency.noGroups')}</TableCell></TableRow> : null}
                    </TableBody>
                  </Table>
                </div>
              </section>

              <section className="overflow-hidden rounded-lg border border-border bg-card/70">
                <div className="flex items-center gap-2 border-b border-border px-4 py-3">
                  <KeyRound className="size-4 text-muted-foreground" />
                  <h3 className="font-semibold text-foreground">{t('concurrency.apiKeysTitle')}</h3>
                </div>
                <div className="overflow-x-auto">
                  <Table>
                    <TableHeader><TableRow><TableHead>{t('concurrency.apiKey')}</TableHead><TableHead className="text-right">{t('concurrency.active')}</TableHead><TableHead className="text-right">{t('concurrency.limit')}</TableHead><TableHead>{t('concurrency.status')}</TableHead></TableRow></TableHeader>
                    <TableBody>
                      {snapshot.api_keys.map((key) => (
                        <TableRow key={key.id}>
                          <TableCell><div className="font-medium">{key.name || `#${key.id}`}</div><div className="text-xs text-muted-foreground">#{key.id}</div></TableCell>
                          <TableCell className="text-right tabular-nums">{key.active}</TableCell>
                          <TableCell className="text-right tabular-nums">{key.limit > 0 ? key.limit : t('concurrency.unlimited')}</TableCell>
                          <TableCell><span className={cn('text-xs font-medium', key.enabled && !key.expired ? 'text-emerald-600 dark:text-emerald-400' : 'text-muted-foreground')}>{key.expired ? t('concurrency.expired') : key.enabled ? t('common.enabled') : t('common.disabled')}</span></TableCell>
                        </TableRow>
                      ))}
                      {snapshot.api_keys.length === 0 ? <TableRow><TableCell colSpan={4} className="h-20 text-center text-muted-foreground">{t('concurrency.noAPIKeys')}</TableCell></TableRow> : null}
                    </TableBody>
                  </Table>
                </div>
              </section>
            </div>
          </div>
        ) : null}
      </StateShell>
    </div>
  )
}
