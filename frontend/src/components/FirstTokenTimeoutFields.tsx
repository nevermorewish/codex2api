import { useCallback, useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { RotateCcw, Trash2 } from 'lucide-react'
import { api } from '../api'
import type { FirstTokenModelTimeouts, FirstTokenSizeBracket, FirstTokenTimeoutSettings } from '../types'
import { Button } from './ui/button'
import { DraftNumberInput } from './ui/draft-number-input'
import { Select } from './ui/select'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from './ui/table'

// 档位是固定的：每个模型的超时都要按这五个体积区间各配一个值。
// 标签与后端 database.FirstTokenSizeBracketLimitBytes 的上界一一对应。
const brackets: { key: FirstTokenSizeBracket; label: string }[] = [
  { key: 'under_50kb', label: '< 50 KB' },
  { key: 'under_100kb', label: '50 ≤ KB < 100' },
  { key: 'under_200kb', label: '100 ≤ KB < 200' },
  { key: 'under_500kb', label: '200 ≤ KB < 500' },
  { key: 'over_500kb', label: '≥ 500 KB' },
]

// 从全局档位表取一行，用于新增模型时的初值和与全局值的比对。
function globalRow(settings: FirstTokenTimeoutSettings): FirstTokenModelTimeouts {
  return {
    under_50kb: settings.under_50kb,
    under_100kb: settings.under_100kb,
    under_200kb: settings.under_200kb,
    under_500kb: settings.under_500kb,
    over_500kb: settings.over_500kb,
  }
}

// 服务端保证每个模型都是完整五行，但接口出问题时不要整页崩掉：缺档回落到全局值。
function modelRow(settings: FirstTokenTimeoutSettings, model: string): FirstTokenModelTimeouts {
  const fallback = globalRow(settings)
  const row = settings.model_timeouts?.[model]
  if (!row) return fallback
  return {
    under_50kb: row.under_50kb ?? fallback.under_50kb,
    under_100kb: row.under_100kb ?? fallback.under_100kb,
    under_200kb: row.under_200kb ?? fallback.under_200kb,
    under_500kb: row.under_500kb ?? fallback.under_500kb,
    over_500kb: row.over_500kb ?? fallback.over_500kb,
  }
}

function sameRow(a: FirstTokenModelTimeouts, b: FirstTokenModelTimeouts): boolean {
  return brackets.every(({ key }) => a[key] === b[key])
}

// 与已保存配置比对：全局五档是否有改动。
function globalDirty(draft: FirstTokenTimeoutSettings, saved: FirstTokenTimeoutSettings): boolean {
  return !(['under_50kb', 'under_100kb', 'under_200kb', 'under_500kb', 'over_500kb'] as const)
    .every((key) => draft[key] === saved[key])
}

// 与已保存配置比对：模型表是否有改动（含增删行）。
function modelsDirty(draft: FirstTokenTimeoutSettings, saved: FirstTokenTimeoutSettings): boolean {
  const current = draft.model_timeouts ?? {}
  const previous = saved.model_timeouts ?? {}
  const models = new Set([...Object.keys(current), ...Object.keys(previous)])
  for (const model of models) {
    if (!current[model] || !previous[model]) return true
    if (!sameRow(modelRow(draft, model), modelRow(saved, model))) return true
  }
  return false
}

export default function FirstTokenTimeoutFields() {
  const { t } = useTranslation()
  const [draft, setDraft] = useState<FirstTokenTimeoutSettings | null>(null)
  const [saved, setSaved] = useState<FirstTokenTimeoutSettings | null>(null)
  const [modelCandidates, setModelCandidates] = useState<string[]>([])
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')
  const [loaded, setLoaded] = useState(0)

  useEffect(() => {
    let active = true
    setError('')
    api.getFirstTokenTimeouts().then((data) => {
      if (active) { setDraft(data); setSaved(data) }
    }).catch((e: unknown) => { if (active) setError(e instanceof Error ? e.message : t('common.loadFailed')) })
    // 模型候选用请求面模型目录，与账号实际可用模型同源，避免手输模型名拼错导致配置静默失效。
    api.getModels().then((res) => {
      if (active) setModelCandidates(res.models || [])
    }).catch(() => { if (active) setModelCandidates([]) })
    return () => { active = false }
  }, [loaded, t])

  const configuredModels = useMemo(() => Object.keys(draft?.model_timeouts ?? {}).sort(), [draft])
  const addOptions = useMemo(
    () => modelCandidates
      .filter((model) => !configuredModels.includes(model))
      .map((model) => ({ value: model, label: model })),
    [modelCandidates, configuredModels],
  )
  const dirty = Boolean(draft && saved && (globalDirty(draft, saved) || modelsDirty(draft, saved)))

  const updateGlobal = useCallback((key: FirstTokenSizeBracket, value: number) => {
    setDraft((current) => current ? { ...current, [key]: value } : current)
  }, [])
  const updateModelBracket = useCallback((model: string, bracket: FirstTokenSizeBracket, value: number) => {
    setDraft((current) => {
      if (!current) return current
      const row = modelRow(current, model)
      return { ...current, model_timeouts: { ...current.model_timeouts, [model]: { ...row, [bracket]: value } } }
    })
  }, [])
  const resetModelToGlobal = useCallback((model: string) => {
    setDraft((current) => current ? { ...current, model_timeouts: { ...current.model_timeouts, [model]: globalRow(current) } } : current)
  }, [])
  const removeModel = useCallback((model: string) => {
    setDraft((current) => {
      if (!current) return current
      const next = { ...current.model_timeouts }
      delete next[model]
      return { ...current, model_timeouts: next }
    })
  }, [])
  const addModel = useCallback((model: string) => {
    setDraft((current) => current ? { ...current, model_timeouts: { ...current.model_timeouts, [model]: globalRow(current) } } : current)
  }, [])

  const save = async () => {
    if (!draft || saving) return
    setSaving(true)
    setError('')
    try {
      const data = await api.updateFirstTokenTimeouts(draft)
      setDraft(data)
      setSaved(data)
    } catch (e) {
      setError(e instanceof Error ? e.message : t('common.saveFailed'))
    } finally {
      setSaving(false)
    }
  }

  const secondsLabel = t('settings.firstTokenSizesSeconds')
  return <div className="col-span-full space-y-4 rounded-xl border p-4">
    <div>
      <h3 className="text-sm font-medium">{t('settings.firstTokenSizesTitle')}</h3>
      <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{t('settings.firstTokenSizesDesc')}</p>
    </div>

    <div className="space-y-2">
      <div className="text-xs font-semibold text-foreground">{t('settings.firstTokenSizesGlobalTitle')}</div>
      <p className="text-xs text-muted-foreground">{t('settings.firstTokenSizesGlobalDesc')}</p>
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-5">
        {brackets.map(({ key, label }) => <label key={key} className="space-y-1.5 text-xs">
          <span>{label} · {secondsLabel}</span>
          <DraftNumberInput aria-label={`${t('settings.firstTokenSizesGlobalTitle')} ${label}`} min={1} max={600} disabled={!draft || saving}
            value={draft?.[key] ?? 0}
            onValueChange={(value) => updateGlobal(key, value)} />
        </label>)}
      </div>
    </div>

    <div className="space-y-2 border-t border-border/80 pt-4">
      <div className="text-xs font-semibold text-foreground">{t('settings.firstTokenSizesModelTitle')}</div>
      <p className="text-xs text-muted-foreground">{t('settings.firstTokenSizesModelDesc')}</p>
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead className="min-w-[180px]">{t('settings.firstTokenSizesModelColumn')}</TableHead>
            {brackets.map(({ key, label }) => <TableHead key={key} className="min-w-[110px]">{label}</TableHead>)}
            <TableHead className="w-[90px] text-right">{t('settings.firstTokenSizesActions')}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {configuredModels.map((model) => {
            const row = draft ? modelRow(draft, model) : null
            const fallback = draft ? globalRow(draft) : null
            return <TableRow key={model}>
              <TableCell className="font-mono text-xs">{model}</TableCell>
              {brackets.map(({ key, label }) => {
                const overridden = Boolean(row && fallback && row[key] !== fallback[key])
                return <TableCell key={key}>
                  <DraftNumberInput
                    aria-label={`${model} ${label}`}
                    min={1}
                    max={600}
                    disabled={!draft || saving}
                    className={overridden ? 'border-primary/60' : undefined}
                    value={row?.[key] ?? 0}
                    onValueChange={(value) => updateModelBracket(model, key, value)} />
                </TableCell>
              })}
              <TableCell className="text-right">
                <div className="flex items-center justify-end gap-1">
                  <Button type="button" variant="ghost" size="sm" disabled={!row || !fallback || saving || sameRow(row, fallback)}
                    title={t('settings.firstTokenSizesResetToGlobal')}
                    aria-label={t('settings.firstTokenSizesResetToGlobal')}
                    onClick={() => resetModelToGlobal(model)}>
                    <RotateCcw className="size-3.5" />
                  </Button>
                  <Button type="button" variant="ghost" size="sm" disabled={saving}
                    title={t('settings.firstTokenSizesRemove')}
                    aria-label={t('settings.firstTokenSizesRemove')}
                    onClick={() => removeModel(model)}>
                    <Trash2 className="size-3.5" />
                  </Button>
                </div>
              </TableCell>
            </TableRow>
          })}
          {configuredModels.length === 0 && <TableRow>
            <TableCell colSpan={brackets.length + 2} className="text-center text-xs text-muted-foreground">
              {t('settings.firstTokenSizesModelEmpty')}
            </TableCell>
          </TableRow>}
        </TableBody>
      </Table>
      <Select
        compact
        value=""
        placeholder={t('settings.firstTokenSizesAddPlaceholder')}
        disabled={!draft || saving || addOptions.length === 0}
        options={addOptions}
        onValueChange={(model) => { if (model) addModel(model) }} />
    </div>

    {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
    <div className="flex items-center gap-3">
      <Button type="button" size="sm" disabled={!dirty || saving} onClick={() => void save()}>
        {t(saving ? 'common.saving' : 'settings.firstTokenSizesSave')}
      </Button>
      {!draft && error && <Button type="button" variant="outline" size="sm" onClick={() => setLoaded((n) => n + 1)}>{t('common.retry')}</Button>}
      {saved && !dirty && <span role="status" className="text-xs text-muted-foreground">{t('settings.firstTokenSizesSaved')}</span>}
    </div>
  </div>
}
