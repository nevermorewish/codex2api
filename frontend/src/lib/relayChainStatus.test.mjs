import assert from 'node:assert/strict'
import { test } from 'node:test'
import { relayChainReason, relayChainStatus, relayElapsedMs } from './relayChainStatus.ts'

test('explicit state takes precedence over the legacy final_ok flag', () => {
  for (const status of ['in_progress', 'success', 'failed', 'canceled', 'incomplete']) {
    assert.equal(relayChainStatus({ status, final_ok: false }), status)
    assert.equal(relayChainStatus({ status, final_ok: true }), status)
  }
})

test('old servers remain compatible and unknown states are not called failures', () => {
  assert.equal(relayChainStatus({ final_ok: true }), 'success')
  assert.equal(relayChainStatus({ final_ok: false }), 'failed')
  assert.equal(relayChainStatus({ status: 'future_state', final_ok: false }), 'incomplete')
})

test('a single failed or canceled attempt exposes its final reason immediately', () => {
  const attempts = [{ seq: 1, status_code: 598, error: 'upstream stream ended early' }]
  assert.deepEqual(relayChainReason({ status: 'failed', final_ok: false, attempts }), {
    kind: 'final', message: 'upstream stream ended early', statusCode: 598, seq: 1,
  })
  assert.equal(relayChainReason({ status: 'canceled', final_ok: false, attempts: [{ seq: 1, status_code: 499, error: 'context canceled' }] }).kind, 'canceled')
})

test('a pending single attempt shows its known error but not as a final result', () => {
  const chain = { status: 'in_progress', final_ok: false, attempts: [{ seq: 1, status_code: 500, error: 'server overloaded' }] }
  assert.deepEqual(relayChainReason(chain), { kind: 'latest', message: 'server overloaded', statusCode: 500, seq: 1 })
  assert.equal(relayChainReason({ ...chain, status: 'incomplete' }).kind, 'latest')
  assert.equal(relayChainReason({ ...chain, status: 'success', final_ok: true }), null)
})

test('missing terminal error never borrows an earlier error as the final reason', () => {
  const chain = { status: 'failed', final_ok: false, attempts: [
    { seq: 1, status_code: 500, error: 'local overload' }, { seq: 2, status_code: 503 },
  ] }
  assert.deepEqual(relayChainReason(chain), { kind: 'final', message: '', statusCode: 503, seq: 2 })
  assert.equal(relayChainReason({ ...chain, attempts: [] }).message, '')
})

test('pending elapsed time includes the unlogged wait without changing terminal durations', () => {
  const chain = { status: 'in_progress', final_ok: false, started_at: '2026-09-07T05:00:00Z', total_ms: 12000 }
  assert.equal(relayElapsedMs(chain, Date.parse('2026-09-07T05:01:30Z')), 90000)
  assert.equal(relayElapsedMs({ ...chain, status: 'failed' }, Date.parse('2026-09-07T05:01:30Z')), 12000)
  assert.equal(relayElapsedMs({ ...chain, started_at: 'invalid' }, Date.now()), 12000)
})
