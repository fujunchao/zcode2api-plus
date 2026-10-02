import { render, screen, within } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { expect, test, vi } from 'vitest'
import { DashboardPage } from '@/pages/dashboard'

vi.mock('@/lib/api', () => ({
  api: async (_method: string, path: string) => {
    const stats = { total: 0, calls: 100, fail: 100 }
    if (path === '/accounts') return { accounts: [], stats, providers: [], ts: 1 }
    if (path === '/status') return { quota_pool: {}, gateway_key_set: true, model_quotas: [
      { model: 'GLM-5.3', total: 100, used: 30, remaining: 70, accounts: 1, items: 1, partial: false },
      { model: 'glm-5.3-flash', total: 1000, used: 400, remaining: 600, accounts: 1, items: 1, partial: false },
    ] }
    return { summary: stats, ranking: [], requests: { total: 2, succeeded: 1, failed: 1, active: 0, retries: 0, success_rate: 50 } }
  },
}))

test('成功率使用请求统计，不再从账号历史调用数相减', async () => {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(<MemoryRouter><QueryClientProvider client={client}><DashboardPage /></QueryClientProvider></MemoryRouter>)
  expect(await screen.findByText('50.0%')).toBeTruthy()
})

test('仪表板使用后端模型汇总，不再显示跨模型相加的剩余额度', async () => {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(<MemoryRouter><QueryClientProvider client={client}><DashboardPage /></QueryClientProvider></MemoryRouter>)
  const main = within(await screen.findByRole('region', { name: 'GLM-5.3 额度' }))
  const flash = within(screen.getByRole('region', { name: 'glm-5.3-flash 额度' }))
  expect(main.getByText('70')).toBeTruthy()
  expect(flash.getByText('600')).toBeTruthy()
  expect(screen.queryByText('670')).toBeNull()
  expect(screen.queryByText('剩餘額度')).toBeNull()
})
