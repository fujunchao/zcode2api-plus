import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { expect, test, vi } from 'vitest'
import { AccountsPage } from '@/pages/accounts'

const mock = vi.hoisted(() => ({ api: vi.fn(), warning: vi.fn(), success: vi.fn(), info: vi.fn(), error: vi.fn() }))
vi.mock('@/lib/api', () => ({ api: mock.api, errMsg: (e: Error) => e.message }))
vi.mock('sonner', () => ({ toast: { warning: mock.warning, success: mock.success, info: mock.info, error: mock.error } }))

test.each([true, false])('领取失败提示准确说明是否移除了代理：%s', async (removed) => {
  mock.api.mockImplementation(async (method: string) => method === 'GET' ? {
    accounts: [{ id: 'a1', name: 'test', mode: 'jwt', status: 'active', enabled: true, quota: {}, plans: [], disabled_models: [], exhausted_models: [], archived_at: null }],
    stats: {}, models: [], proxies: [], ts: 1,
  } : {
    outcomes: [{ ok: false, message: 'unusual activity', ...(removed ? { proxy_removed: 'bad', proxy_reassigned: 2, proxy_direct_fallback: 0 } : {}) }],
    summary: { ok: 0, fail: 1 },
  })
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(<QueryClientProvider client={client}><AccountsPage /></QueryClientProvider>)
  fireEvent.click(await screen.findByTitle('領取活動套餐'))
  await waitFor(() => expect(mock.warning).toHaveBeenCalled())
  const message = mock.warning.mock.calls[0][0]
  expect(message).toContain('unusual activity')
  if (removed) {
    expect(message).toContain('已移除问题代理，2 个账号已改派')
    expect(message).toContain('保留领取冷却')
  } else {
    expect(message).not.toContain('已移除')
  }
})
