import { useEffect, useState } from 'react'
import { api } from '../api'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import { Card, CardContent } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'
import { Button } from '@/components/ui/button'
import type { SystemSettings } from '../types'

export default function RobotMonitor() {
  const [settings, setSettings] = useState<SystemSettings | null>(null)
  const [secret, setSecret] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const [saved, setSaved] = useState(false)

  useEffect(() => {
    api.getSettings()
      .then((next) => setSettings({ ...next, feishu_alert_error_codes: next.feishu_alert_error_codes?.trim() || '400-599' }))
      .catch((e) => setError(e instanceof Error ? e.message : '加载失败'))
  }, [])

  const edit = (data: Partial<SystemSettings>) => {
    setSettings((current) => current ? { ...current, ...data } : current)
    setSaved(false)
  }

  const save = async () => {
    if (!settings || saving) return
    setSaving(true)
    setSaved(false)
    setError(null)
    try {
      const next = await api.updateSettings({
        feishu_alert_enabled: settings.feishu_alert_enabled,
        feishu_app_id: settings.feishu_app_id.trim(),
        feishu_chat_ids: settings.feishu_chat_ids.trim(),
        feishu_alert_error_codes: settings.feishu_alert_error_codes.trim() || '400-599',
        feishu_first_token_timeout_seconds: settings.feishu_first_token_timeout_seconds,
        ...(secret.trim() ? { feishu_app_secret: secret.trim() } : {}),
      })
      setSettings(next)
      setSecret('')
      setSaved(true)
    } catch (e) {
      setError(e instanceof Error ? e.message : '保存失败，请重试')
    } finally {
      setSaving(false)
    }
  }

  if (!settings) {
    return <StateShell variant="page" loading={!error} error={error}><></></StateShell>
  }

  return (
    <div className="mx-auto w-full max-w-4xl">
      <PageHeader title="机器人监控" description="配置飞书机器人错误告警与首 Token 超时监控，修改后请点击保存。" />
      <Card>
        <CardContent className="p-5">
          <form onSubmit={(e) => { e.preventDefault(); void save() }}>
            <fieldset disabled={saving} className="min-w-0 space-y-5">
              <label className="flex items-center justify-between">
                <span>
                  <b>启用飞书机器人告警</b>
                  <span className="block text-xs text-muted-foreground">错误和首 Token 超时触发通知</span>
                </span>
                <Switch checked={settings.feishu_alert_enabled} onCheckedChange={(v) => edit({ feishu_alert_enabled: v })} />
              </label>
              <Field label="App ID" value={settings.feishu_app_id} onChange={(v) => edit({ feishu_app_id: v })} />
              <Field label="App Secret（明文，留空保持已配置值）" value={secret} onChange={(v) => { setSecret(v); setSaved(false) }} />
              <Field label="Chat ID（逗号分隔）" value={settings.feishu_chat_ids} onChange={(v) => edit({ feishu_chat_ids: v })} />
              <Field label="错误码过滤（默认 400-599）" value={settings.feishu_alert_error_codes} onChange={(v) => edit({ feishu_alert_error_codes: v })} />
              <label className="block">
                <span className="mb-1 block text-sm font-medium">首 Token 超时（秒）</span>
                <Input type="number" min={1} max={86400} step={1} required value={settings.feishu_first_token_timeout_seconds} onChange={(e) => edit({ feishu_first_token_timeout_seconds: Number(e.target.value) || 1 })} />
              </label>
              {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
              <div className="flex items-center justify-between gap-3">
                <div className="text-xs text-muted-foreground" role="status">
                  {saving ? '保存中…' : saved ? '保存成功' : '修改后请点击保存'}
                  <span className="ml-2">{settings.feishu_app_secret_configured ? 'App Secret 已配置' : '尚未配置 App Secret'}</span>
                </div>
                <Button type="submit" disabled={saving}>{saving ? '保存中…' : '保存'}</Button>
              </div>
            </fieldset>
          </form>
        </CardContent>
      </Card>
    </div>
  )
}

function Field({ label, value, onChange }: { label: string; value: string; onChange: (v: string) => void }) {
  return (
    <label className="block">
      <span className="mb-1 block text-sm font-medium">{label}</span>
      <Input type="text" value={value} onChange={(e) => onChange(e.target.value)} />
    </label>
  )
}
