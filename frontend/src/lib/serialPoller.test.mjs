import assert from 'node:assert/strict'
import { test } from 'node:test'
import { createSerialPoller } from './serialPoller.ts'

function deferred() {
  let resolve, reject
  const promise = new Promise((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}

test('polling and manual refresh cannot overlap a pending request', async () => {
  const pending = deferred()
  let calls = 0
  const results = []
  const loader = createSerialPoller({ request: () => { calls++; return pending.promise }, onResult: (x) => results.push(x), onError: assert.fail })
  const first = loader.load()
  await loader.load()
  assert.equal(calls, 1)
  pending.resolve('current')
  await first
  assert.deepEqual(results, ['current'])
  loader.stop()
})

test('a stalled response times out, permits retry and cannot overwrite the fresh result', async () => {
  const pending = deferred()
  let calls = 0, signal
  const results = [], errors = []
  const loader = createSerialPoller({
    timeoutMs: 10,
    request: (s) => { signal = s; return ++calls === 1 ? pending.promise : Promise.resolve('fresh') },
    onResult: (x) => results.push(x), onError: (e) => errors.push(e),
  })
  await loader.load()
  assert.equal(signal.aborted, true)
  assert.equal(errors.length, 1)
  await loader.load()
  pending.resolve('stale')
  await Promise.resolve()
  assert.deepEqual(results, ['fresh'])
  loader.stop()
})

test('leaving the page cancels a pending load and suppresses late state updates', async () => {
  const pending = deferred()
  let signal, calls = 0
  const loader = createSerialPoller({ request: (s) => { signal = s; calls++; return pending.promise }, onResult: assert.fail, onError: assert.fail })
  const first = loader.load()
  loader.stop()
  await first
  pending.resolve('old page')
  await loader.load()
  assert.equal(signal.aborted, true)
  assert.equal(calls, 1)
})

test('an ordinary failure releases the polling slot for the next refresh', async () => {
  let calls = 0
  const errors = [], results = []
  const loader = createSerialPoller({ request: async () => { if (++calls === 1) throw new Error('network'); return 'ok' }, onResult: (x) => results.push(x), onError: (e) => errors.push(e.message) })
  await loader.load()
  await loader.load()
  assert.deepEqual(errors, ['network'])
  assert.deepEqual(results, ['ok'])
  loader.stop()
})
