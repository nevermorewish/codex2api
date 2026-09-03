import { useCallback, useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Activity, Gauge, KeyRound, Layers3, Search, Users } from 'lucide-react'
import { api } from '../api'
import type { ConcurrencyAccountRow, ConcurrencySnapshot } from '../types'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import { StatTile } from '../components/StatTile'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { getErrorMessage } from '../utils/error'
import { cn } from '@/lib/utils'

function utilizationWidth(value: number): string {
  return `${Math.max(0, Math.min(100, value))}%`
}

function loadTone(row: ConcurrencyAccountRow): string {
  if (!row.available) return 'bg-muted-foreground/35'
  if (row.utilization >= 90) return 'bg-red-500'
  if (row.utilization >= 65) return 'bg-amber-500'
  return 'bg-emerald-500'
}

export default function Concurrency() {
  const { t } = useTranslation()
  const [snapshot, setSnapshot] = useState<ConcurrencySnapshot | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [search, setSearch] = useState('')
  const [hideIdle, setHideIdle] = useState(false)

  const refresh = useCallback(async (quiet = false) => {
    if (!quiet) setLoading(true)
    try {
      const data = await api.getConcurrency()
      setSnapshot(data)
      setError(null)
    } catch (err) {
      if (!quiet || !snapshot) setError(getErrorMessage(err))
    } finally {
      if (!quiet) setLoading(false)
    }
  }, [snapshot])

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
    <div className="mx-auto w-full max-w-[1500px]">
      <PageHeader
        title={t('concurrency.title')}
        description={t('concurrency.description')}
        onRefresh={() => void refresh(false)}
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

            <section className="overflow-hidden rounded-lg border border-border bg-card/70">
              <div className="flex flex-col gap-3 border-b border-border px-4 py-3 sm:flex-row sm:items-center sm:justify-between">
                <div>
                  <h3 className="font-semibold text-foreground">{t('concurrency.accountsTitle')}</h3>
                  <p className="text-xs text-muted-foreground">{t('concurrency.accountsCount', { visible: accounts.length, total: snapshot.accounts.length })}</p>
                </div>
                <div className="flex flex-col gap-2 sm:flex-row sm:items-center">
                  <label className="relative min-w-0 sm:w-64">
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
                      <TableHead>{t('concurrency.channel')}</TableHead>
                      <TableHead>{t('concurrency.groups')}</TableHead>
                      <TableHead className="min-w-52">{t('concurrency.pressure')}</TableHead>
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
                            <div className="font-medium text-foreground">{row.name}</div>
                            <div className="text-xs text-muted-foreground">#{row.id} · {row.status}</div>
                          </div>
                        </TableCell>
                        <TableCell>
                          <span className={cn('inline-flex rounded border px-2 py-0.5 text-xs font-medium', row.fallback ? 'border-amber-500/25 bg-amber-500/10 text-amber-700 dark:text-amber-300' : 'border-border bg-muted/50 text-muted-foreground')}>
                            {t(`concurrency.channels.${row.channel}`, { defaultValue: row.channel })}
                          </span>
                        </TableCell>
                        <TableCell className="max-w-56 text-xs text-muted-foreground">{row.group_names.join(', ') || '-'}</TableCell>
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
                      <TableRow><TableCell colSpan={7} className="h-24 text-center text-muted-foreground">{t('concurrency.noAccounts')}</TableCell></TableRow>
                    ) : null}
                  </TableBody>
                </Table>
              </div>
            </section>

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
