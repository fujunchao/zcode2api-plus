import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { expect, test, vi } from 'vitest'
import { AccountsPage } from '@/pages/accounts'

const mock = vi.hoisted(() => ({ api: vi.fn(), success: vi.fn(), error: vi.fn() }))
vi.mock('@/lib/api', () => ({ api: mock.api, errMsg: (e: Error) => e.message }))
vi.mock('sonner', () => ({ toast: { success: mock.success, error: mock.error } }))

test.each([
  [{ ok: false, result: { error: '上游鉴权失败' } }, '上游鉴权失败'],
  [{ ok: false, message: '账号不可刷新' }, '账号不可刷新'],
  [new Error('网络失败'), '网络失败'],
  [{ ok: true }, null],
])('单账号刷新按业务结果显示提示：%j', async (response, error) => {
  mock.api.mockReset()
  mock.api.mockImplementation(async (method: string) => {
    if (method === 'GET') return {
      accounts: [{ id: 'a1', name: 'test', mode: 'jwt', status: 'active', enabled: true, quota: {}, plans: [], disabled_models: [], exhausted_models: [], archived_at: null }],
      stats: {}, models: [], proxies: [], ts: 1,
    }
    if (response instanceof Error) throw response
    return response
  })
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(<QueryClientProvider client={client}><AccountsPage /></QueryClientProvider>)
  fireEvent.click(await screen.findByTitle('重新整理額度'))
  if (error) {
    await waitFor(() => expect(mock.error).toHaveBeenCalledWith(expect.stringContaining(error)))
    expect(mock.success).not.toHaveBeenCalled()
  } else {
    await waitFor(() => expect(mock.success).toHaveBeenCalledWith('額度已重新整理'))
    expect(mock.error).not.toHaveBeenCalled()
  }
})
