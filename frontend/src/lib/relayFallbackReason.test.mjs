import assert from 'node:assert/strict'
import { test } from 'node:test'
import { relayFallbackReasonKey } from './relayFallbackReason.ts'

test('fallback reasons retain distinct routing causes', () => {
  for (const reason of ['relay_limit', 'retry_budget', 'retry_deadline', 'rate_limit_budget', 'affinity_capacity_full', 'queue_threshold', 'no_eligible_primary', 'primary_wait_ended', 'primary_unavailable', 'oversized_request']) {
    assert.equal(relayFallbackReasonKey(` ${reason} `), `concurrency.fallbackReasons.${reason}`)
  }
})

test('historical and unknown reasons are not inferred to be capacity failures', () => {
  assert.equal(relayFallbackReasonKey(), 'concurrency.fallbackReasons.not_recorded')
  assert.equal(relayFallbackReasonKey('  '), 'concurrency.fallbackReasons.not_recorded')
  assert.equal(relayFallbackReasonKey('future_reason'), 'concurrency.fallbackReasons.unknown')
})
