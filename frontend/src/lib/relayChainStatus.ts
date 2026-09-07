export type RelayChainStatus = 'in_progress' | 'success' | 'failed' | 'canceled' | 'incomplete'

export function relayChainStatus(chain: { status?: string; final_ok: boolean }): RelayChainStatus {
  switch (chain.status) {
    case 'in_progress':
    case 'success':
    case 'failed':
    case 'canceled':
    case 'incomplete':
      return chain.status
    case undefined:
      return chain.final_ok ? 'success' : 'failed'
    default:
      return 'incomplete'
  }
}

type ReasonAttempt = { seq: number; status_code: number; error?: string }
type ReasonChain = { status?: string; final_ok: boolean; attempts: ReasonAttempt[] }

// Terminal reasons must come from the final attempt, never a previous failure.
// While running, a completed attempt's error is useful but is not a final result.
export function relayChainReason(chain: ReasonChain) {
  const status = relayChainStatus(chain)
  if (status === 'success') return null
  const terminal = status === 'failed' || status === 'canceled'
  const attempt = terminal ? chain.attempts[chain.attempts.length - 1] : [...chain.attempts].reverse().find((item) => item.error?.trim() || item.status_code >= 400)
  return {
    kind: terminal ? status === 'canceled' ? 'canceled' : 'final' : 'latest',
    message: attempt?.error?.trim() ?? '',
    statusCode: attempt?.status_code ?? 0,
    seq: attempt?.seq ?? 0,
  } as const
}

export function relayElapsedMs(chain: { status?: string; final_ok: boolean; started_at: string; total_ms: number }, now: number) {
  if (relayChainStatus(chain) !== 'in_progress') return chain.total_ms
  const started = Date.parse(chain.started_at)
  return Number.isFinite(started) ? Math.max(chain.total_ms, now - started, 0) : chain.total_ms
}
