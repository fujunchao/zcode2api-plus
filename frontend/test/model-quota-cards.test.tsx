import { render, screen, within } from '@testing-library/react'
import { expect, test } from 'vitest'
import { ModelQuotaCards } from '@/components/model-quota-cards'
import type { ModelQuotaSummary } from '@/lib/types'

// snapshot_accounts 是池级数值：池内已有额度快照的账号数，各模型项都相同。
const base: ModelQuotaSummary = { model: 'GLM-5.3', total: 100, used: 100, remaining: 0, accounts: 1, items: 2, partial: false, snapshot_accounts: 80 }
const flash: ModelQuotaSummary = { model: 'glm-5.3-flash', total: 1000, used: 200, remaining: 800, accounts: 3, items: 4, partial: false, snapshot_accounts: 80 }

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

test('有账号持有时画三列数字与剩余比例进度条，不出现任何无额度说明', () => {
  render(<ModelQuotaCards quotas={[{ ...base, accounts: 2, items: 2, total: 1000, used: 250, remaining: 750 }]} />)
  const card = within(screen.getByRole('region', { name: 'GLM-5.3 额度' }))
  expect(card.getByText('剩余额度')).toBeTruthy()
  expect(card.getByText('750')).toBeTruthy()
  expect(card.getByText('1.0k')).toBeTruthy()
  expect(card.getByText('250')).toBeTruthy()
  expect(card.getByText('2 个账号 · 2 个额度项目')).toBeTruthy()
  expect(card.getByRole('progressbar').getAttribute('aria-valuenow')).toBe('75')
  expect(card.queryAllByText('--')).toHaveLength(0)
  expect(card.queryByText(/可能已下架/)).toBeNull()
  expect(card.queryByText('尚未获取该模型额度，请在账号池刷新额度。')).toBeNull()
})

test('池内已刷新过额度但套餐不含该模型时，收缩为下架警示而非空额度列', () => {
  render(<ModelQuotaCards quotas={[{ ...base, model: 'GLM-5.4', total: null, used: null, remaining: null, accounts: 0, items: 0 }]} />)
  const card = within(screen.getByRole('region', { name: 'GLM-5.4 额度' }))
  expect(card.getByText('当前账号池的 80 个账号套餐均不含该模型（可能已下架），请求会直接失败。')).toBeTruthy()
  // 头部计数改成可读表述，不再出现「0 个账号 · 0 个额度项目」。
  expect(card.getByText('0 个账号持有 · 共 80 个账号已刷新额度')).toBeTruthy()
  expect(card.queryByText('0 个账号 · 0 个额度项目')).toBeNull()
  // 没有数字列与空进度条。
  expect(card.queryByText('剩余额度')).toBeNull()
  expect(card.queryAllByText('--')).toHaveLength(0)
  expect(card.queryByRole('progressbar')).toBeNull()
  expect(screen.queryByRole('progressbar')).toBeNull()
  // 不该再提示去刷新——刷多少次也不会回来。
  expect(card.queryByText('尚未获取该模型额度，请在账号池刷新额度。')).toBeNull()
})

test('池内一次额度都没刷到时仍提示去刷新，不说模型已下架', () => {
  render(<ModelQuotaCards quotas={[{ ...base, total: null, used: null, remaining: null, accounts: 0, items: 0, snapshot_accounts: 0 }]} />)
  const card = within(screen.getByRole('region', { name: 'GLM-5.3 额度' }))
  expect(card.getByText('尚未获取该模型额度，请在账号池刷新额度。')).toBeTruthy()
  expect(card.getByText('0 个账号 · 0 个额度项目')).toBeTruthy()
  expect(card.queryByText(/可能已下架/)).toBeNull()
  expect(card.queryByText(/已刷新额度/)).toBeNull()
  expect(card.queryByText('剩余额度')).toBeNull()
  expect(card.queryAllByText('--')).toHaveLength(0)
  expect(screen.queryByRole('progressbar')).toBeNull()
})

test('有账号持有但额度未知时显示未知而非零，零总额度不产生无效比例', () => {
  render(<ModelQuotaCards quotas={[
    { ...base, total: null, used: null, remaining: null },
    { ...flash, total: 0, used: 0, remaining: 0 },
  ]} />)
  const main = within(screen.getByRole('region', { name: 'GLM-5.3 额度' }))
  expect(main.getAllByText('--')).toHaveLength(3)
  expect(screen.queryAllByRole('progressbar')).toHaveLength(0)
})

test('部分数据只显示已知汇总，不计算可能误导的百分比', () => {
  render(<ModelQuotaCards quotas={[{ ...base, remaining: 20, used: null, partial: true }]} />)
  const card = within(screen.getByRole('region', { name: 'GLM-5.3 额度' }))
  expect(card.getByText('20')).toBeTruthy()
  expect(card.getByText('--')).toBeTruthy()
  expect(card.getByText('部分额度数据缺失，仅汇总已知数值。')).toBeTruthy()
  expect(screen.queryByRole('progressbar')).toBeNull()
})

test('刷新后各模型数据独立更新', () => {
  const view = render(<ModelQuotaCards quotas={[base, flash]} />)
  view.rerender(<ModelQuotaCards quotas={[{ ...base, used: 10, remaining: 90 }, flash]} />)
  expect(within(screen.getByRole('region', { name: 'GLM-5.3 额度' })).getByText('90')).toBeTruthy()
  expect(within(screen.getByRole('region', { name: 'glm-5.3-flash 额度' })).getByText('800')).toBeTruthy()
})

test('模型下架后重新上架时卡片恢复成数字视图', () => {
  const view = render(<ModelQuotaCards quotas={[{ ...base, model: 'GLM-5.4', total: null, used: null, remaining: null, accounts: 0, items: 0 }]} />)
  expect(screen.getByText(/可能已下架/)).toBeTruthy()
  view.rerender(<ModelQuotaCards quotas={[{ ...base, model: 'GLM-5.4', accounts: 1, items: 1, total: 500, used: 100, remaining: 400 }]} />)
  const card = within(screen.getByRole('region', { name: 'GLM-5.4 额度' }))
  expect(card.getByText('400')).toBeTruthy()
  expect(card.getByRole('progressbar').getAttribute('aria-valuenow')).toBe('80')
  expect(card.queryByText(/可能已下架/)).toBeNull()
})
