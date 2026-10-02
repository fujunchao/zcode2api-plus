import { render, screen, within } from '@testing-library/react'
import { expect, test } from 'vitest'
import { ModelQuotaCards } from '@/components/model-quota-cards'
import type { ModelQuotaSummary } from '@/lib/types'

const base: ModelQuotaSummary = { model: 'GLM-5.3', total: 100, used: 100, remaining: 0, accounts: 1, items: 2, partial: false }
const flash: ModelQuotaSummary = { model: 'glm-5.3-flash', total: 1000, used: 200, remaining: 800, accounts: 3, items: 4, partial: false }

test('每个模型独立展示额度，单模型耗尽不清空另一个模型', () => {
  render(<ModelQuotaCards quotas={[base, flash]} />)
  const main = within(screen.getByRole('region', { name: 'GLM-5.3 额度' }))
  const fast = within(screen.getByRole('region', { name: 'glm-5.3-flash 额度' }))
  expect(main.getByText('0')).toBeTruthy()
  expect(main.queryByText('800')).toBeNull()
  expect(fast.getByText('800')).toBeTruthy()
  expect(fast.getByText('1.0k')).toBeTruthy()
  expect(fast.getByText('200')).toBeTruthy()
  expect(main.getByRole('progressbar').getAttribute('aria-valuenow')).toBe('0')
  expect(fast.getByRole('progressbar').getAttribute('aria-valuenow')).toBe('80')
  expect(fast.getByText('3 个账号 · 4 个额度项目')).toBeTruthy()
})

test('无快照显示未知而非零，零总额度不产生无效比例', () => {
  render(<ModelQuotaCards quotas={[
    { ...base, total: null, used: null, remaining: null, accounts: 0, items: 0 },
    { ...flash, total: 0, used: 0, remaining: 0 },
  ]} />)
  const main = within(screen.getByRole('region', { name: 'GLM-5.3 额度' }))
  expect(main.getAllByText('--')).toHaveLength(3)
  expect(main.getByText('尚未获取该模型额度，请在账号池刷新额度。')).toBeTruthy()
  expect(screen.queryAllByRole('progressbar')).toHaveLength(0)
})

test('部分数据只显示已知汇总，不计算可能误导的百分比', () => {
  render(<ModelQuotaCards quotas={[{ ...base, remaining: 20, used: null, partial: true }]} />)
  expect(screen.getByText('20')).toBeTruthy()
  expect(screen.getByText('--')).toBeTruthy()
  expect(screen.getByText('部分额度数据缺失，仅汇总已知数值。')).toBeTruthy()
  expect(screen.queryByRole('progressbar')).toBeNull()
})

test('刷新后各模型数据独立更新', () => {
  const view = render(<ModelQuotaCards quotas={[base, flash]} />)
  view.rerender(<ModelQuotaCards quotas={[{ ...base, used: 10, remaining: 90 }, flash]} />)
  expect(within(screen.getByRole('region', { name: 'GLM-5.3 额度' })).getByText('90')).toBeTruthy()
  expect(within(screen.getByRole('region', { name: 'glm-5.3-flash 额度' })).getByText('800')).toBeTruthy()
})
