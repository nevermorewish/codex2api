const knownReasons = new Set([
  'relay_limit',
  'retry_budget',
  'retry_deadline',
  'rate_limit_budget',
  'affinity_capacity_full',
  'queue_threshold',
  'no_eligible_primary',
  'primary_wait_ended',
  'primary_unavailable',
  'oversized_request',
])

export function relayFallbackReasonKey(reason?: string): string {
  const code = reason?.trim() || ''
  if (!code) return 'concurrency.fallbackReasons.not_recorded'
  return `concurrency.fallbackReasons.${knownReasons.has(code) ? code : 'unknown'}`
}
