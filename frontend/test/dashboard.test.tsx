import { render, screen } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { expect, test, vi } from 'vitest'
import { DashboardPage } from '@/pages/dashboard'

vi.mock('@/lib/api', () => ({
  api: async (_method: string, path: string) => {
    const stats = { total: 0, calls: 100, fail: 100 }
    if (path === '/accounts') return { accounts: [], stats, providers: [], ts: 1 }
    if (path === '/status') return { quota_pool: {}, gateway_key_set: true }
    return { summary: stats, ranking: [], requests: { total: 2, succeeded: 1, failed: 1, active: 0, retries: 0, success_rate: 50 } }
  },
}))

test('成功率使用请求统计，不再从账号历史调用数相减', async () => {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(<MemoryRouter><QueryClientProvider client={client}><DashboardPage /></QueryClientProvider></MemoryRouter>)
  expect(await screen.findByText('50.0%')).toBeTruthy()
})
