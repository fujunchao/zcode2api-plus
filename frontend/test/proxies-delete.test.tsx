import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { expect, test, vi } from 'vitest'
import { ProxiesPage } from '@/pages/proxies'

const mock = vi.hoisted(() => ({ api: vi.fn(), success: vi.fn(), error: vi.fn() }))
vi.mock('@/lib/api', () => ({ api: mock.api, errMsg: (e: Error) => e.message }))
vi.mock('sonner', () => ({ toast: { success: mock.success, error: mock.error } }))

test.each([
  { reassigned: 2, direct: 0, message: '代理已删除：2 个账号已改派到其他代理' },
  { reassigned: 0, direct: 1, message: '代理已删除：1 个账号无可用代理，已改为直连' },
  { reassigned: 2, direct: 1, message: '代理已删除：2 个账号已改派到其他代理，1 个账号无可用代理，已改为直连' },
  { reassigned: 0, direct: 0, message: '代理已删除' },
])('删除确认与结果提示遵循共享规则：$reassigned 个改派、$direct 个直连', async ({ reassigned, direct, message }) => {
  mock.api.mockReset()
  mock.api.mockImplementation(async (method: string, path: string) => {
    if (method === 'GET' && path === '/proxies') return {
      profiles: [{ id: 'p1', name: '待删除代理', url: 'http://127.0.0.1:18080', enabled: true }],
    }
    if (method === 'DELETE' && path === '/proxies/p1') return { ok: true, reassigned, direct_fallback: direct }
    throw new Error(`未预期的请求：${method} ${path}`)
  })
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })
  render(<QueryClientProvider client={client}><ProxiesPage /></QueryClientProvider>)
  fireEvent.click(await screen.findByTitle('刪除'))
  const dialog = await screen.findByRole('alertdialog')
  expect(dialog.textContent).toContain('优先改派到其他空闲代理')
  expect(dialog.textContent).toContain('没有空闲代理时，分配到绑定账号最少的启用代理')
  expect(dialog.textContent).toContain('只有没有可用代理时才改为直连')
  fireEvent.click(within(dialog).getByRole('button', { name: '確認', exact: true }))
  await waitFor(() => expect(mock.success).toHaveBeenCalledWith(message))
  expect(mock.api).toHaveBeenCalledWith('DELETE', '/proxies/p1')
  expect(mock.error).not.toHaveBeenCalled()
  client.clear()
})
