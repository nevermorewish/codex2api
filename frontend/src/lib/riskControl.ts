export interface RiskConfig {
  enabled: boolean;
  mode: "off" | "observe" | "pre_block";
  keyword_blocking_mode: "keyword_only" | "keyword_and_api" | "api_only";
  blocked_keywords: string[];
  base_url: string;
  model: string;
  api_keys?: string[];
  timeout_ms: number;
  retry_count: number;
  proxy_url: string;
  sample_rate: number;
  model_filter: "all" | "include" | "exclude";
  models: string[];
  group_ids: number[];
  api_key_ids: number[];
  thresholds: Record<string, number>;
  pre_hash_check_enabled: boolean;
  record_non_hits: boolean;
  block_status: number;
  block_message: string;
  worker_count: number;
  queue_size: number;
  auto_ban_enabled: boolean;
  ban_threshold: number;
  violation_window_hours: number;
  hit_retention_days: number;
  non_hit_retention_days: number;
  email_on_hit: boolean;
  smtp_host: string;
  smtp_port: number;
  smtp_username: string;
  smtp_password?: string;
  email_from: string;
  email_to: string;
}
export interface RiskConfigView {
  config: RiskConfig;
  smtp_password_configured: boolean;
  categories: string[];
}
export interface RiskKeyHealth {
  id: string;
  hint: string;
  status: string;
  calls: number;
  errors: number;
  latency_ms: number;
  http_status: number;
  frozen_until: number;
}
export interface RiskStatus {
  total: number;
  hits: number;
  blocked: number;
  hashes: number;
  bans: number;
  queue_length: number;
  active: number;
  checked: number;
  dropped: number;
  errors: number;
  notification_errors: number;
  keys: RiskKeyHealth[];
}
export interface RiskEvent {
  id: string;
  created_at: number;
  api_key_id: number;
  api_key_name: string;
  endpoint: string;
  model: string;
  mode: string;
  action: string;
  flagged: boolean;
  blocked: boolean;
  input_hash: string;
  input_excerpt: string;
  matched_keyword?: string;
  scores?: Record<string, number>;
  error?: string;
  latency_ms: number;
  violation_count: number;
  auto_banned: boolean;
  email_sent: boolean;
}
export interface RiskLogPage {
  items: RiskEvent[];
  total: number;
  page: number;
  page_size: number;
}
export interface RiskBan {
  api_key_id: number;
  name: string;
  created_at: number;
  blocked: boolean;
}
export const parseRiskWords = (text: string) => [
  ...new Map(
    text
      .split(/\r?\n/)
      .map((v) => v.trim())
      .filter(Boolean)
      .map((v) => [v.toLowerCase(), v]),
  ).values(),
];
export function parseRiskIDs(text: string): number[] {
  if (!text.trim()) return [];
  const values = text
    .split(/[\s,，]+/)
    .filter(Boolean)
    .map(Number);
  if (values.some((v) => !Number.isSafeInteger(v) || v <= 0))
    throw new Error("invalid_scope_ids");
  return [...new Set(values)];
}
