// A slow refresh must not overlap the next one or update an unmounted page.
// The deadline covers the entire request, including reading its response body.
export function createSerialPoller<T>(options: {
  request: (signal: AbortSignal) => Promise<T>
  onResult: (result: T) => void
  onError: (error: unknown) => void
  timeoutMs?: number
}) {
  let active = true
  let inFlight = false
  let controller: AbortController | undefined
  return {
    async load() {
      if (!active || inFlight) return
      inFlight = true
      const current = new AbortController()
      controller = current
      let timer: ReturnType<typeof setTimeout> | undefined
      const deadline = new Promise<never>((_, reject) => {
        current.signal.addEventListener('abort', () => reject(new Error('请求超时，请稍后重试')), { once: true })
        timer = setTimeout(() => current.abort(), options.timeoutMs ?? 15_000)
      })
      try {
        const result = await Promise.race([options.request(current.signal), deadline])
        if (active && !current.signal.aborted) options.onResult(result)
      } catch (error) {
        if (active) options.onError(current.signal.aborted ? new Error('请求超时，请稍后重试') : error)
      } finally {
        clearTimeout(timer)
        if (controller === current) controller = undefined
        inFlight = false
      }
    },
    stop() {
      active = false
      controller?.abort()
    },
  }
}
