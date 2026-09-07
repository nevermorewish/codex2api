// Mirrors relayFallbackReason.ts's approach: a known-value Set plus an
// i18n key prefix, with anything unrecognized falling back to "unknown".
// The values here are the streamOutcome.failureKind / UsageLogInput's
// UpstreamErrorKind enum already established on the backend (proxy/handler.go);
// this list only needs updating if that enum grows, not the lookup logic.
const knownDisconnectReasons = new Set([
  'transport',
  'timeout',
  'server',
  'client',
  'local',
  'continuous_retry_timeout',
  'message_too_big',
  'ws_busy_acquire',
  'usage_limit',
  'usage_limited',
  'rate_limited',
  'rate_limited_model',
  'rate_limited_5h',
  'rate_limited_7d',
  'unauthorized',
  'deactivated_workspace',
  'payment_required_unknown',
  'agent_runtime_deleted',
  'forbidden',
  'version_required',
  'cyber_policy',
  'safety_policy',
  'quality_degraded',
  'content_policy',
  'upstream_error',
])

export function liveStreamDisconnectReasonKey(kind?: string): string {
  const code = kind?.trim() || ''
  if (!code) return 'liveStreams.disconnectReasons.not_recorded'
  return `liveStreams.disconnectReasons.${knownDisconnectReasons.has(code) ? code : 'unknown'}`
}
