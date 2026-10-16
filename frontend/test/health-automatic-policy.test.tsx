import { expect, test } from 'vitest'
import { defaults, healthStatus } from '@/components/health/labels'
import type { ModelProbeSummary } from '@/lib/types'

function summary(): ModelProbeSummary {
  return { settings: { ...defaults, enabled: true }, total_targets: 2, observed_targets: 0,
    runs: [], accounts: [], used_requests: 0, remaining_requests: 200, paused_until: 0,
    next_due: {}, active: 0, rule_version: 'synthetic' }
}

test('自动巡检没有观测、没有合格账号时不能报告正常', () => {
  const data = summary()
  expect(healthStatus(data).level).not.toBe('ok')
  expect(healthStatus(data).text).toContain('账号')
})

test('自动巡检预算耗尽需要报告等待而非正常', () => {
  const data = summary()
  data.used_requests = 200
  data.remaining_requests = 0
  data.accounts = [{ id: 'a', name: '合成账号', eligible: true, rank: 1, recent_success: 1, recent_total: 1 }]
  expect(healthStatus(data).level).not.toBe('ok')
  expect(healthStatus(data).text).toContain('预算')
})

test('有合格账号但没有完成巡检时应显示覆盖不足', () => {
  const data = summary()
  data.accounts = [{ id: 'a', name: '合成账号', eligible: true, rank: 1, recent_success: 1, recent_total: 1 }]
  expect(healthStatus(data).level).not.toBe('ok')
  expect(healthStatus(data).text).toContain('覆盖')
})

test('网络异常不能被模型成功掩盖，网络单独启用不声称模型健康', () => {
  const data = summary()
  data.settings.network_enabled = true
  data.checked_targets = 2
  data.eligible_accounts = 1
  data.network = { total: 2, checked: 2, failed: 1 }
  expect(healthStatus(data).text).toContain('网络异常')
  data.settings.enabled = false
  data.network.failed = 0
  expect(healthStatus(data).text).toBe('网络正常（模型未启用）')
})

test('主动覆盖完整但成功记录过期，不能继续报告正常', () => {
  const data = summary()
  data.checked_targets = 2
  data.eligible_accounts = 1
  data.fresh_healthy_targets = 0
  expect(healthStatus(data).level).not.toBe('ok')
  expect(healthStatus(data).text).toContain('过期')
})
