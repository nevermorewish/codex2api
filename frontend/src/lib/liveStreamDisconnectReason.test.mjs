import assert from 'node:assert/strict'
import { test } from 'node:test'
import { liveStreamDisconnectReasonKey } from './liveStreamDisconnectReason.ts'

test('every known upstream error kind maps to its own i18n key', () => {
  const known = [
    'transport', 'timeout', 'server', 'client', 'local', 'continuous_retry_timeout',
    'message_too_big', 'ws_busy_acquire', 'usage_limit', 'usage_limited', 'rate_limited',
    'rate_limited_model', 'rate_limited_5h', 'rate_limited_7d', 'unauthorized',
    'deactivated_workspace', 'payment_required_unknown', 'agent_runtime_deleted',
    'forbidden', 'version_required', 'cyber_policy', 'safety_policy', 'quality_degraded',
    'content_policy', 'upstream_error',
  ]
  for (const reason of known) {
    assert.equal(liveStreamDisconnectReasonKey(` ${reason} `), `liveStreams.disconnectReasons.${reason}`)
  }
})

test('missing or blank reasons are reported as not recorded, not unknown', () => {
  assert.equal(liveStreamDisconnectReasonKey(), 'liveStreams.disconnectReasons.not_recorded')
  assert.equal(liveStreamDisconnectReasonKey('  '), 'liveStreams.disconnectReasons.not_recorded')
})

test('an unrecognized upstream error kind falls back to unknown', () => {
  assert.equal(liveStreamDisconnectReasonKey('future_kind'), 'liveStreams.disconnectReasons.unknown')
})
