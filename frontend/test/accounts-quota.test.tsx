import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { expect, test, vi } from 'vitest'
import { AccountsPage } from '@/pages/accounts'

const mock = vi.hoisted(() => ({ api: vi.fn() }))
vi.mock('@/lib/api', () => ({ api: mock.api, errMsg: (e: Error) => e.message }))

function fixture() {
  return {
    accounts: [{
      id: 'a1', name: '额度账号', mode: 'jwt', status: 'active', enabled: true, archived_at: null,
      plans: [], disabled_models: [], exhausted_models: [],
      quota: {
        'GLM-5.3 · 体验': { model: 'GLM-5.3', plan_name: '体验套餐', period: 'daily', total: 100, used: 100, remaining: 0 },
        'GLM-5.3-Flash': { model: 'GLM-5.3-Flash', total: 1000, used: 200, remaining: 800 },
      },
    }],
    // 故意与可见账号明细不同，验证页面读取全池后端汇总而非自行加总。
    model_quotas: [
      { model: 'GLM-5.3', total: 300, used: 300, remaining: 0, accounts: 2, items: 3, partial: false },
      { model: 'glm-5.3-flash', total: 2000, used: 400, remaining: 1600, accounts: 2, items: 2, partial: false },
    ],
    stats: {}, models: ['GLM-5.3', 'glm-5.3-flash'], proxies: [], ts: 1,
  }
}

function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(<QueryClientProvider client={client}><AccountsPage /></QueryClientProvider>)
  return client
}

test('账号池独立展示两模型全池汇总，筛选不改变总额且套餐明细保留', async () => {
  mock.api.mockReset().mockResolvedValue(fixture())
  const client = mount()
  const main = within(await screen.findByRole('region', { name: 'GLM-5.3 额度' }))
  const flash = within(screen.getByRole('region', { name: 'glm-5.3-flash 额度' }))
  expect(main.getByText('0')).toBeTruthy()
  expect(main.getAllByText('300')).toHaveLength(2)
  expect(flash.getByText('1.6k')).toBeTruthy()
  expect(flash.getByText('2.0k')).toBeTruthy()
  expect(screen.queryByText('總額度（剩餘）')).toBeNull()
  expect(screen.getByText('体验套餐')).toBeTruthy()
  fireEvent.click(screen.getByRole('button', { name: /^停用\s*0$/ }))
  expect(screen.queryByText('额度账号')).toBeNull()
  expect(main.getByText('0')).toBeTruthy()
  expect(flash.getByText('1.6k')).toBeTruthy()
  expect(mock.api.mock.calls.every(([method, path]) => method === 'GET' && path === '/accounts')).toBe(true)
  client.clear()
})

test('单号刷新完成后模型汇总随账号查询一并更新', async () => {
  const data = fixture()
  mock.api.mockReset().mockImplementation(async (method: string, path: string) => {
    if (method === 'GET' && path === '/accounts') return structuredClone(data)
    if (method === 'POST' && path === '/accounts/a1/refresh') {
      data.model_quotas[0] = { ...data.model_quotas[0], used: 100, remaining: 200 }
      return { ok: true }
    }
    throw new Error(`不应发送额外请求：${method} ${path}`)
  })
  const client = mount()
  const main = within(await screen.findByRole('region', { name: 'GLM-5.3 额度' }))
  expect(main.getByText('0')).toBeTruthy()
  fireEvent.click(screen.getByTitle('重新整理額度'))
  await waitFor(() => expect(main.getByText('200')).toBeTruthy())
  expect(within(screen.getByRole('region', { name: 'glm-5.3-flash 额度' })).getByText('1.6k')).toBeTruthy()
  expect(mock.api.mock.calls.some(([, path]) => path === '/status')).toBe(false)
  client.clear()
})
