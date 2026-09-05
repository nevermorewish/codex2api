import { useEffect, useState } from 'react'
import { api } from '../api'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import { Card, CardContent } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'
import type { SystemSettings } from '../types'

export default function RobotMonitor() {
  const [settings, setSettings] = useState<SystemSettings | null>(null); const [error, setError] = useState<string | null>(null); const [saving, setSaving] = useState(false)
  useEffect(() => { api.getSettings().then(setSettings).catch((e) => setError(e instanceof Error ? e.message : '加载失败')) }, [])
  const patch = async (data: Partial<SystemSettings>) => { setSaving(true); try { const next = await api.updateSettings(data); setSettings(next) } catch (e) { setError(e instanceof Error ? e.message : '保存失败') } finally { setSaving(false) } }
  if (!settings) return <StateShell variant="page" loading error={error} />
  return <div className="mx-auto w-full max-w-4xl"><PageHeader title="机器人监控" description="配置飞书机器人错误告警与首 Token 超时监控。" /><StateShell variant="page" error={error}><Card><CardContent className="space-y-5 p-5"><label className="flex items-center justify-between"><span><b>启用飞书机器人告警</b><span className="block text-xs text-muted-foreground">错误和首 Token 超时触发通知</span></span><Switch checked={settings.feishu_alert_enabled} onCheckedChange={(v) => void patch({ feishu_alert_enabled: v })} /></label><Field label="App ID" value={settings.feishu_app_id} onChange={(v) => setSettings({ ...settings, feishu_app_id: v })} onBlur={() => void patch({ feishu_app_id: settings.feishu_app_id.trim() })} /><Field label="App Secret（留空保持已配置值）" value="" type="password" onChange={(v) => { if (v) void patch({ feishu_app_secret: v }) }} onBlur={() => undefined} /><Field label="Chat ID（逗号分隔）" value={settings.feishu_chat_ids} onChange={(v) => setSettings({ ...settings, feishu_chat_ids: v })} onBlur={() => void patch({ feishu_chat_ids: settings.feishu_chat_ids.trim() })} /><Field label="错误码过滤" value={settings.feishu_alert_error_codes} onChange={(v) => setSettings({ ...settings, feishu_alert_error_codes: v })} onBlur={() => void patch({ feishu_alert_error_codes: settings.feishu_alert_error_codes.trim() })} /><Field label="首 Token 超时（秒）" value={String(settings.feishu_first_token_timeout_seconds)} onChange={(v) => setSettings({ ...settings, feishu_first_token_timeout_seconds: Number(v) || 1 })} onBlur={() => void patch({ feishu_first_token_timeout_seconds: settings.feishu_first_token_timeout_seconds })} /><div className="text-xs text-muted-foreground">{saving ? '保存中…' : settings.feishu_app_secret_configured ? 'App Secret 已配置' : '尚未配置 App Secret'}</div></CardContent></Card></StateShell></div>
}
function Field({ label, value, onChange, onBlur, type = 'text' }: { label: string; value: string; onChange: (v: string) => void; onBlur: () => void; type?: string }) { return <label className="block"><span className="mb-1 block text-sm font-medium">{label}</span><Input type={type} value={value} onChange={(e) => onChange(e.target.value)} onBlur={onBlur} /></label> }
