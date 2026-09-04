import { useCallback, useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { CheckCircle2, LifeBuoy, Loader2, Pencil, Play, Plus, RefreshCw, Save, Trash2, XCircle } from 'lucide-react'
import { api } from '../api'
import type { FallbackAccount, FallbackAccountPayload, FallbackPolicy } from '../types'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import Modal from '../components/Modal'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { useToast } from '../hooks/useToast'
import { useConfirmDialog } from '../hooks/useConfirmDialog'
import { getErrorMessage } from '../utils/error'
import { cn } from '@/lib/utils'
import ChipInput from '../components/ChipInput'

const emptyForm: FallbackAccountPayload = {
  name: '', protocol: 'openai_responses', base_url: 'https://api.openai.com', api_key: '',
  models: [], proxy_url: '', concurrency: 10, enabled: true,
}

export default function FallbackPool() {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const { confirm, confirmDialog } = useConfirmDialog()
  const [accounts, setAccounts] = useState<FallbackAccount[]>([])
  const [policy, setPolicy] = useState<FallbackPolicy>({
    enabled: false,
    relay_count: 3,
    queue_direct_fallback_threshold: 5,
    oversized_request_direct_fallback_enabled: false,
  })
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [savingPolicy, setSavingPolicy] = useState(false)
  const [editing, setEditing] = useState<FallbackAccount | null | undefined>(undefined)
  const [form, setForm] = useState<FallbackAccountPayload>(emptyForm)
  const [savingAccount, setSavingAccount] = useState(false)
  const [testingID, setTestingID] = useState<number | null>(null)
  const [modelOptions, setModelOptions] = useState<string[]>([])
  const [modelsLoading, setModelsLoading] = useState(false)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const [accountData, settings] = await Promise.all([api.listFallbackAccounts(), api.getFallbackSettings()])
      setAccounts(accountData.accounts)
      setPolicy(settings)
      setError(null)
    } catch (err) {
      setError(getErrorMessage(err))
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => { void load() }, [load])

  const openCreate = () => {
    setForm({ ...emptyForm })
    setModelOptions([])
    setEditing(null)
  }

  const openEdit = (account: FallbackAccount) => {
    setForm({
      name: account.name, protocol: 'openai_responses', base_url: account.base_url,
      api_key: '', models: account.models?.length ? [...account.models] : (account.model ? [account.model] : []), proxy_url: account.proxy_url,
      concurrency: account.concurrency, enabled: account.enabled,
    })
    setModelOptions(account.models?.length ? [...account.models] : (account.model ? [account.model] : []))
    setEditing(account)
  }

  const syncModels = async () => {
    if (!form.base_url.trim() || !form.api_key?.trim()) {
      showToast(t('fallback.modelsSyncNeedsKey'), 'error')
      return
    }
    setModelsLoading(true)
    try {
      const result = await api.fetchOpenAIResponsesModels({
        base_url: form.base_url,
        api_key: form.api_key,
        proxy_url: form.proxy_url,
      })
      const models = Array.from(new Set([...(modelOptions ?? []), ...(result.models ?? [])])).sort((a, b) => a.localeCompare(b))
      setModelOptions(models)
      showToast(t('fallback.modelsSyncDone', { count: result.models?.length ?? 0 }))
    } catch (err) {
      showToast(getErrorMessage(err), 'error')
    } finally {
      setModelsLoading(false)
    }
  }

  const savePolicy = async () => {
    setSavingPolicy(true)
    try {
      const updated = await api.updateFallbackSettings(policy)
      setPolicy(updated)
      showToast(t('fallback.policySaved'))
    } catch (err) {
      showToast(getErrorMessage(err), 'error')
    } finally {
      setSavingPolicy(false)
    }
  }

  const saveAccount = async () => {
    if (!form.name.trim() || !form.base_url.trim() || (!editing && !form.api_key?.trim())) {
      showToast(t('fallback.requiredFields'), 'error')
      return
    }
    setSavingAccount(true)
    try {
      if (editing) {
        await api.updateFallbackAccount(editing.id, form)
        showToast(t('fallback.accountUpdated'))
      } else {
        await api.createFallbackAccount(form)
        showToast(t('fallback.accountCreated'))
      }
      setEditing(undefined)
      await load()
    } catch (err) {
      showToast(getErrorMessage(err), 'error')
    } finally {
      setSavingAccount(false)
    }
  }

  const toggleAccount = async (account: FallbackAccount, enabled: boolean) => {
    try {
      await api.updateFallbackAccount(account.id, {
        name: account.name, protocol: 'openai_responses', base_url: account.base_url,
        models: account.models ?? (account.model ? [account.model] : []), proxy_url: account.proxy_url, concurrency: account.concurrency, enabled,
      })
      setAccounts((current) => current.map((item) => item.id === account.id ? { ...item, enabled } : item))
    } catch (err) {
      showToast(getErrorMessage(err), 'error')
    }
  }

  const deleteAccount = async (account: FallbackAccount) => {
    const approved = await confirm({
      title: t('fallback.deleteTitle'), description: t('fallback.deleteDescription', { name: account.name }),
      confirmText: t('common.delete'), tone: 'destructive', confirmVariant: 'destructive',
    })
    if (!approved) return
    try {
      await api.deleteFallbackAccount(account.id)
      setAccounts((current) => current.filter((item) => item.id !== account.id))
      showToast(t('fallback.accountDeleted'))
    } catch (err) {
      showToast(getErrorMessage(err), 'error')
    }
  }

  const testAccount = async (account: FallbackAccount) => {
    setTestingID(account.id)
    try {
      const result = await api.testFallbackAccount(account.id)
      if (result.success) {
        showToast(t('fallback.testSuccess', { latency: result.latency_ms }))
      } else {
        showToast(t('fallback.testFailed', { error: result.error || `HTTP ${result.status_code ?? 0}` }), 'error')
      }
    } catch (err) {
      showToast(getErrorMessage(err), 'error')
    } finally {
      setTestingID(null)
    }
  }

  return (
    <div className="mx-auto w-full max-w-[1450px]">
      <PageHeader
        title={t('fallback.title')}
        description={t('fallback.description')}
        onRefresh={() => void load()}
        actions={<Button onClick={openCreate}><Plus className="size-4" />{t('fallback.addAccount')}</Button>}
      />
      <StateShell variant="page" loading={loading && accounts.length === 0} error={error} onRetry={() => void load()} loadingTitle={t('fallback.loading')} errorTitle={t('fallback.loadFailed')}>
        <div className="space-y-5">
          <section className="rounded-lg border border-border bg-card/70">
            <div className="flex flex-col gap-4 px-4 py-4">
              <div className="flex min-w-0 items-start gap-3">
                <div className="flex size-10 shrink-0 items-center justify-center rounded-lg bg-amber-500/10 text-amber-600 dark:text-amber-300"><LifeBuoy className="size-5" /></div>
                <div>
                  <h3 className="font-semibold text-foreground">{t('fallback.policyTitle')}</h3>
                  <p className="mt-0.5 max-w-2xl text-xs leading-relaxed text-muted-foreground">{t('fallback.policyDescription')}</p>
                </div>
              </div>
              <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-[180px_minmax(180px,1fr)_minmax(230px,1fr)_180px_auto] xl:items-end">
                <label className="flex h-9 items-center justify-between gap-3 text-sm font-medium xl:justify-start">
                  <span>{t('fallback.masterSwitch')}</span>
                  <Switch checked={policy.enabled} onCheckedChange={(enabled) => setPolicy((current) => ({ ...current, enabled }))} aria-label={t('fallback.masterSwitch')} />
                </label>
                <label className="grid gap-1 text-xs font-medium text-muted-foreground">
                  {t('fallback.queueThreshold')}
                  <Input type="number" min={0} max={1000} value={policy.queue_direct_fallback_threshold} onChange={(event) => setPolicy((current) => ({ ...current, queue_direct_fallback_threshold: Math.max(0, Math.min(1000, Number(event.target.value) || 0)) }))} />
                </label>
                <label className="flex min-h-9 items-center justify-between gap-3 text-sm font-medium">
                  <span>{t('fallback.oversizedDirectFallback')}</span>
                  <Switch checked={policy.oversized_request_direct_fallback_enabled} onCheckedChange={(enabled) => setPolicy((current) => ({ ...current, oversized_request_direct_fallback_enabled: enabled }))} aria-label={t('fallback.oversizedDirectFallback')} />
                </label>
                <label className="grid gap-1 text-xs font-medium text-muted-foreground">
                  {t('fallback.relayCount')}
                  <Input type="number" min={1} max={1000} value={policy.relay_count} onChange={(event) => setPolicy((current) => ({ ...current, relay_count: Math.max(1, Math.min(1000, Number(event.target.value) || 1)) }))} />
                </label>
                <Button variant="outline" onClick={() => void savePolicy()} disabled={savingPolicy} className="sm:col-span-2 xl:col-span-1">
                  {savingPolicy ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}
                  {t('common.save')}
                </Button>
              </div>
            </div>
          </section>

          <section className="overflow-hidden rounded-lg border border-border bg-card/70">
            <div className="border-b border-border px-4 py-3">
              <h3 className="font-semibold text-foreground">{t('fallback.accountsTitle')}</h3>
              <p className="text-xs text-muted-foreground">{t('fallback.accountsCount', { count: accounts.length })}</p>
            </div>
            <div className="overflow-x-auto">
              <Table>
                <TableHeader><TableRow><TableHead>{t('fallback.account')}</TableHead><TableHead>{t('fallback.endpoint')}</TableHead><TableHead>{t('fallback.model')}</TableHead><TableHead className="text-right">{t('fallback.concurrency')}</TableHead><TableHead>{t('fallback.runtime')}</TableHead><TableHead>{t('fallback.enabled')}</TableHead><TableHead className="text-right">{t('fallback.actions')}</TableHead></TableRow></TableHeader>
                <TableBody>
                  {accounts.map((account) => (
                    <TableRow key={account.id}>
                      <TableCell><div className="font-medium text-foreground">{account.name}</div><div className="text-xs text-muted-foreground">#{account.id} · {account.api_key_masked}</div></TableCell>
                      <TableCell><div className="max-w-72 truncate font-mono text-xs" title={account.base_url}>{account.base_url}</div>{account.proxy_url ? <div className="mt-1 max-w-72 truncate text-[11px] text-muted-foreground" title={account.proxy_url}>{t('fallback.proxy')}: {account.proxy_url}</div> : null}</TableCell>
                      <TableCell className="font-mono text-xs"><div title={(account.models ?? (account.model ? [account.model] : [])).join(', ')}>{account.models?.length ? account.models.join(', ') : t('fallback.modelsAll')}</div></TableCell>
                      <TableCell className="text-right tabular-nums">{account.occupied}/{account.concurrency}</TableCell>
                      <TableCell><span className={cn('inline-flex items-center gap-1.5 text-xs font-medium', account.status === 'ready' ? 'text-emerald-600 dark:text-emerald-400' : 'text-muted-foreground')}>{account.status === 'ready' ? <CheckCircle2 className="size-3.5" /> : <XCircle className="size-3.5" />}{account.status || t('fallback.notLoaded')}</span></TableCell>
                      <TableCell><Switch checked={account.enabled} onCheckedChange={(enabled) => void toggleAccount(account, enabled)} aria-label={t('fallback.enabled')} /></TableCell>
                      <TableCell>
                        <div className="flex justify-end gap-1">
                          <Button variant="ghost" size="icon-sm" onClick={() => void testAccount(account)} disabled={testingID === account.id} title={t('fallback.test')} aria-label={t('fallback.test')}>{testingID === account.id ? <Loader2 className="size-4 animate-spin" /> : <Play className="size-4" />}</Button>
                          <Button variant="ghost" size="icon-sm" onClick={() => openEdit(account)} title={t('common.edit')} aria-label={t('common.edit')}><Pencil className="size-4" /></Button>
                          <Button variant="ghost" size="icon-sm" onClick={() => void deleteAccount(account)} title={t('common.delete')} aria-label={t('common.delete')} className="text-destructive hover:text-destructive"><Trash2 className="size-4" /></Button>
                        </div>
                      </TableCell>
                    </TableRow>
                  ))}
                  {accounts.length === 0 ? <TableRow><TableCell colSpan={7} className="h-28 text-center text-muted-foreground">{t('fallback.noAccounts')}</TableCell></TableRow> : null}
                </TableBody>
              </Table>
            </div>
          </section>
        </div>
      </StateShell>

      <Modal
        show={editing !== undefined}
        title={editing ? t('fallback.editTitle') : t('fallback.createTitle')}
        onClose={() => !savingAccount && setEditing(undefined)}
        footer={<><Button variant="outline" onClick={() => setEditing(undefined)} disabled={savingAccount}>{t('common.cancel')}</Button><Button onClick={() => void saveAccount()} disabled={savingAccount}>{savingAccount ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}{t('common.save')}</Button></>}
      >
        <div className="grid gap-4">
          <label className="grid gap-1.5 text-sm font-medium">{t('fallback.name')}<Input value={form.name} onChange={(event) => setForm((current) => ({ ...current, name: event.target.value }))} placeholder={t('fallback.namePlaceholder')} /></label>
          <label className="grid gap-1.5 text-sm font-medium">{t('fallback.baseURL')}<Input value={form.base_url} onChange={(event) => setForm((current) => ({ ...current, base_url: event.target.value }))} placeholder="https://api.openai.com" /></label>
          <label className="grid gap-1.5 text-sm font-medium">{editing ? t('fallback.replaceAPIKey') : t('fallback.apiKey')}<Input type="password" autoComplete="new-password" value={form.api_key ?? ''} onChange={(event) => setForm((current) => ({ ...current, api_key: event.target.value }))} placeholder={editing ? t('fallback.keepAPIKey') : 'sk-...'} />{editing ? <span className="text-xs font-normal text-muted-foreground">{t('fallback.keepAPIKeyHint')}</span> : null}</label>
          <div className="grid gap-1.5 text-sm font-medium">
            <div className="flex items-center justify-between gap-2"><span>{t('fallback.model')}</span><Button type="button" variant="outline" size="sm" onClick={() => void syncModels()} disabled={modelsLoading || !form.api_key?.trim()}><RefreshCw className={cn('size-3.5', modelsLoading && 'animate-spin')} />{modelsLoading ? t('fallback.modelsSyncing') : t('fallback.modelsSync')}</Button></div>
            <ChipInput value={form.models} onChange={(models) => setForm((current) => ({ ...current, models }))} options={modelOptions} disabled={savingAccount || modelsLoading} placeholder={t('fallback.modelsPlaceholder')} />
            <span className="text-xs font-normal text-muted-foreground">{t('fallback.modelHint')}</span>
          </div>
          <div className="grid gap-4 sm:grid-cols-[1fr_140px]">
            <label className="grid gap-1.5 text-sm font-medium">{t('fallback.proxyURL')}<Input value={form.proxy_url} onChange={(event) => setForm((current) => ({ ...current, proxy_url: event.target.value }))} placeholder="socks5://127.0.0.1:1080" /></label>
            <label className="grid gap-1.5 text-sm font-medium">{t('fallback.concurrency')}<Input type="number" min={1} max={1000} value={form.concurrency} onChange={(event) => setForm((current) => ({ ...current, concurrency: Math.max(1, Math.min(1000, Number(event.target.value) || 1)) }))} /></label>
          </div>
          <label className="flex items-center justify-between rounded-lg border border-border px-3 py-2.5 text-sm font-medium"><span>{t('fallback.enabledOnSave')}</span><Switch checked={form.enabled} onCheckedChange={(enabled) => setForm((current) => ({ ...current, enabled }))} /></label>
        </div>
      </Modal>
      {confirmDialog}
    </div>
  )
}
