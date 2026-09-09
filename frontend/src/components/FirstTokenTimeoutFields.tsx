import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api } from '../api'
import { Button } from './ui/button'
import { DraftNumberInput } from './ui/draft-number-input'

type Timeouts = Awaited<ReturnType<typeof api.getFirstTokenTimeouts>>
const fields = [
  ['under_50kb', '< 50 KB'],
  ['under_100kb', '50 ≤ KB < 100'],
  ['under_200kb', '100 ≤ KB < 200'],
  ['under_500kb', '200 ≤ KB < 500'],
  ['over_500kb', '≥ 500 KB'],
] as const

export default function FirstTokenTimeoutFields() {
  const { t } = useTranslation()
  const [draft, setDraft] = useState<Timeouts | null>(null)
  const [saved, setSaved] = useState<Timeouts | null>(null)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')
  const [loaded, setLoaded] = useState(0)
  useEffect(() => {
    let active = true
    setError('')
    api.getFirstTokenTimeouts().then((data) => {
      if (active) { setDraft(data); setSaved(data) }
    }).catch((e: unknown) => { if (active) setError(e instanceof Error ? e.message : t('common.loadFailed')) })
    return () => { active = false }
  }, [loaded, t])
  const dirty = draft && fields.some(([key]) => draft[key] !== saved?.[key])
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
  return <div className="col-span-full space-y-3 rounded-xl border p-4">
    <div>
      <h3 className="text-sm font-medium">{t('settings.firstTokenSizesTitle')}</h3>
      <p className="mt-1 text-xs text-muted-foreground">{t('settings.firstTokenSizesDesc')}</p>
    </div>
    <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-5">
      {fields.map(([key, label]) => <label key={key} className="space-y-1.5 text-xs">
        <span>{label} · {t('settings.firstTokenSizesSeconds')}</span>
        <DraftNumberInput aria-label={label} min={1} max={600} disabled={!draft || saving} value={draft?.[key] ?? 0}
          onValueChange={(value) => setDraft((current) => current ? { ...current, [key]: value } : current)} />
      </label>)}
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
