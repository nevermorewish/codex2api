import assert from 'node:assert/strict'
import { test } from 'node:test'
import { liveStreamElapsedMs, liveStreamIsDisconnected, liveStreamStatus } from './liveStreamStatus.ts'

test('status is a direct passthrough for every known value', () => {
  for (const status of ['in_progress', 'success', 'failed', 'canceled', 'incomplete']) {
    assert.equal(liveStreamStatus({ status }), status)
  }
})

test('an unrecognized status is treated as incomplete, not success or failure', () => {
  assert.equal(liveStreamStatus({ status: 'future_state' }), 'incomplete')
})

test('only in_progress and success count as not disconnected', () => {
  assert.equal(liveStreamIsDisconnected({ status: 'in_progress' }), false)
  assert.equal(liveStreamIsDisconnected({ status: 'success' }), false)
  assert.equal(liveStreamIsDisconnected({ status: 'failed' }), true)
  assert.equal(liveStreamIsDisconnected({ status: 'canceled' }), true)
  assert.equal(liveStreamIsDisconnected({ status: 'incomplete' }), true)
})

test('an in-flight stream reports live elapsed time from started_at', () => {
  const stream = { status: 'in_progress', started_at: '2026-09-08T05:00:00Z', total_ms: 12000 }
  assert.equal(liveStreamElapsedMs(stream, Date.parse('2026-09-08T05:01:30Z')), 90000)
})

test('a terminal stream keeps its recorded total_ms regardless of now', () => {
  const stream = { status: 'failed', started_at: '2026-09-08T05:00:00Z', total_ms: 12000 }
  assert.equal(liveStreamElapsedMs(stream, Date.parse('2026-09-08T06:00:00Z')), 12000)
})

test('an invalid started_at falls back to the recorded total_ms', () => {
  const stream = { status: 'in_progress', started_at: 'not-a-date', total_ms: 12000 }
  assert.equal(liveStreamElapsedMs(stream, Date.now()), 12000)
})
