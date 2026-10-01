import { fireEvent, render, screen } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { expect, test, vi } from 'vitest'
import { AccountsPage } from '@/pages/accounts'

const mock = vi.hoisted(() => ({ api: vi.fn() }))
vi.mock('@/lib/api', () => ({ api: mock.api, errMsg: (e: Error) => e.message }))

test.each([
  [true, null, '自动（1 条空闲，超出后按最少绑定共享）'],
  [true, 'active', '自动（无空闲，分配绑定最少的代理）'],
  [true, 'archived', '自动（无空闲，分配绑定最少的代理）'],
  [false, null, '自动（无可用代理，将直连）'],
])('自动代理提示与后端绑定统计一致：enabled=%s account=%s', async (enabled, account, label) => {
  mock.api.mockResolvedValue({
    accounts: account ? [{
      id: 'a1', name: 'test', mode: 'jwt', status: 'active', enabled: true,
      quota: {}, plans: [], disabled_models: [], exhausted_models: [],
      archived_at: account === 'archived' ? 1 : null, proxy_id: 'p1',
    }] : [],
    stats: {}, models: [], ts: 1,
    proxies: [{ id: 'p1', name: '代理 1', url: 'http://127.0.0.1:18080', enabled }],
  })
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(<QueryClientProvider client={client}><AccountsPage /></QueryClientProvider>)
  fireEvent.click(await screen.findByRole('button', { name: '新增', exact: true }))
  expect(await screen.findByText(label)).toBeTruthy()
})
