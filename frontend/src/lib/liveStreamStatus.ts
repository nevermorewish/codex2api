export type LiveStreamStatus = 'in_progress' | 'success' | 'failed' | 'canceled' | 'incomplete'

// LiveStream.status is always present (unlike RelayChain.status, which is
// optional and falls back to final_ok on older payloads), so this is a
// simple passthrough rather than relayChainStatus's inference logic.
export function liveStreamStatus(stream: { status: string }): LiveStreamStatus {
  switch (stream.status) {
    case 'in_progress':
    case 'success':
    case 'failed':
    case 'canceled':
    case 'incomplete':
      return stream.status
    default:
      return 'incomplete'
  }
}

// A stream counts as disconnected once it has left the in-flight registry
// without having completed successfully. While it is still in_progress,
// internal account-rotation failures do not count as a disconnect — that is
// the whole point of surfacing attempts separately from stream-level status.
export function liveStreamIsDisconnected(stream: { status: string }): boolean {
  const status = liveStreamStatus(stream)
  return status !== 'in_progress' && status !== 'success'
}

export function liveStreamElapsedMs(stream: { status: string; started_at: string; total_ms: number }, now: number): number {
  if (liveStreamStatus(stream) !== 'in_progress') return stream.total_ms
  const started = Date.parse(stream.started_at)
  return Number.isFinite(started) ? Math.max(stream.total_ms, now - started, 0) : stream.total_ms
}
