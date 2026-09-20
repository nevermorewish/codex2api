export interface RiskConfig {
  audit_engine: "chat";
  model_audit: ModelAuditPolicy;
  enabled: boolean;
  mode: "off" | "observe" | "pre_block";
  keyword_blocking_mode: "keyword_only" | "keyword_and_api" | "api_only";
  blocked_keywords: string[];
  sample_rate: number;
  fallback_on_block_enabled: boolean;
  pre_hash_check_enabled: boolean;
  record_non_hits: boolean;
  block_status: number;
  block_message: string;
  worker_count: number;
  queue_size: number;
  hit_retention_days: number;
  non_hit_retention_days: number;
}
export interface RiskConfigView {
  config: RiskConfig;
  audit_categories: string[];
  audit_default_prompt: string;
  audit_category_prompt: string;
}
export interface AuditNode {
  id: string;
  name: string;
  enabled: boolean;
  base_url: string;
  model: string;
  api_key?: string;
  has_api_key?: boolean;
  clear_api_key?: boolean;
  timeout_ms: number;
  max_input_chars: number;
}
export interface ModelAuditPolicy {
  nodes: AuditNode[];
  system_prompt: string;
  categorized: boolean;
  categories: string[];
  block_threshold: number;
  flag_threshold: number;
  fail_open: boolean;
}
export interface RiskStatus {
  total: number;
  hits: number;
  blocked: number;
  hashes: number;
  queue_length: number;
  active: number;
  checked: number;
  dropped: number;
  errors: number;
}
export interface RiskEvent {
  audit?: {
    risk: string;
    confidence: number;
    categories: string[];
    reason: string;
    node_id: string;
    model: string;
    chunks: number;
    would_block: boolean;
  };
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
}
export interface RiskLogPage {
  items: RiskEvent[];
  total: number;
  page: number;
  page_size: number;
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

// These IDs identify configuration rows, not credentials. randomUUID is absent
// on insecure HTTP origins, where administrators may still use the dashboard.
let auditNodeSequence = 0;
export function createAuditNodeID(cryptoAPI = globalThis.crypto): string {
  if (typeof cryptoAPI?.randomUUID === "function")
    return cryptoAPI.randomUUID();
  if (typeof cryptoAPI?.getRandomValues === "function") {
    const bytes = cryptoAPI.getRandomValues(new Uint8Array(16));
    return (
      "audit-" +
      Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("")
    );
  }
  return `audit-${Date.now().toString(36)}-${(++auditNodeSequence).toString(36)}-${Math.random().toString(36).slice(2)}`;
}
