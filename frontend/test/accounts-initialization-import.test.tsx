import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { expect, test, vi } from 'vitest'
import { AccountsPage } from '@/pages/accounts'

const mock = vi.hoisted(() => ({ api: vi.fn(), success: vi.fn(), warning: vi.fn(), error: vi.fn() }))
vi.mock('@/lib/api', () => ({ api: mock.api, errMsg: (e: Error) => e.message }))
vi.mock('sonner', () => ({ toast: { success: mock.success, warning: mock.warning, error: mock.error } }))

test('选中旧版漏处理账号并确认后才补入初始化，不扫描其他账号', async () => {
  mock.api.mockReset()
  const accounts = ['selected', 'untouched'].map((id) => ({ id, name: id, mode: 'jwt', status: 'active', enabled: true, quota: {}, plans: [], disabled_models: [], exhausted_models: [], archived_at: null }))
  mock.api.mockImplementation(async (method: string, path: string) => {
    if (method === 'GET' && path === '/accounts') return { accounts, stats: {}, models: [], proxies: [], ts: 1 }
    if (method === 'POST' && path === '/accounts/batch/initialize') return { total: 1, succeeded: 1, duplicated: 0, not_found: 0, failed: 0, ids: ['selected'], items: [{ index: 0, id: 'selected', status: 'ok' }] }
    throw new Error(`不应调用：${method} ${path}`)
  })
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(<QueryClientProvider client={client}><AccountsPage /></QueryClientProvider>)
  fireEvent.click(await screen.findByRole('checkbox', { name: '選取 selected' }))
  fireEvent.click(await screen.findByRole('button', { name: '补入初始化', exact: true }))
  expect(mock.api.mock.calls.filter(([method]) => method === 'POST')).toHaveLength(0)
  const dialog = await screen.findByRole('alertdialog')
  expect(dialog.textContent).toContain('代理路由')
  fireEvent.click(within(dialog).getByRole('button', { name: '确认补入', exact: true }))
  await waitFor(() => expect(mock.api).toHaveBeenCalledWith('POST', '/accounts/batch/initialize', { ids: ['selected'], confirm_proxy_takeover: true }))
  client.clear()
})

test('导入结果明确区分新建、重复和进入初始化的账号', async () => {
  mock.api.mockReset()
  mock.success.mockClear()
  const payload = { providers: { zai: [{ name: 'synthetic', secret: 'synthetic.token.sig' }] } }
  mock.api.mockImplementation(async (method: string, path: string) => {
    if (method === 'GET') return { accounts: [], stats: {}, models: [], proxies: [], ts: 1 }
    if (path === '/import') return { count: 2, created: 1, duplicated: 1, initializing: 1 }
    throw new Error('不应发起其他操作')
  })
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(<QueryClientProvider client={client}><AccountsPage /></QueryClientProvider>)
  const file = new File(['{}'], 'synthetic.json', { type: 'application/json' })
  Object.defineProperty(file, 'text', { value: async () => JSON.stringify(payload) })
  fireEvent.change(screen.getByLabelText('导入账号文件'), { target: { files: [file] } })
  await waitFor(() => expect(mock.api).toHaveBeenCalledWith('POST', '/import', payload))
  await waitFor(() => expect(mock.success).toHaveBeenCalledWith(expect.stringContaining('1 个已进入初始化')))
  client.clear()
})
