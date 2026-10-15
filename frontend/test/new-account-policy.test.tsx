import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { expect, test, vi } from 'vitest'
import { ProxiesPage } from '@/pages/proxies'

const mock = vi.hoisted(() => ({ api: vi.fn() }))
vi.mock('@/lib/api', () => ({ api: mock.api, errMsg: (e: Error) => e.message }))
vi.mock('@/components/proxy-subscription', () => ({ ProxySubscription: () => null }))

test('在代理页指定本地服务器为默认业务代理，只保存独立策略', async () => {
  const policy = { enabled: false, default_proxy_id: '', pool_mode: 'all_enabled', proxy_ids: [], max_attempts: 32, attempt_timeout_seconds: 120, round_budget_seconds: 900, revision: 1 }
  mock.api.mockImplementation(async (method: string, path: string, payload?: object) => {
    if (path === '/proxies') return { profiles: [{ id: 'local', name: '本地服务器', url: 'http://127.0.0.1:18080', enabled: true }, { id: 'trial', name: '取额节点', url: 'http://127.0.0.1:18081', enabled: true }] }
    if (path === '/proxies/new-account-policy') return method === 'PUT' ? { ...payload, revision: 2 } : policy
    throw new Error('不应请求其他设置')
  })
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(<QueryClientProvider client={client}><ProxiesPage /></QueryClientProvider>)
  fireEvent.change(await screen.findByLabelText('默认业务代理'), { target: { value: 'local' } })
  fireEvent.click(screen.getByLabelText('启用新账号初始化'))
  fireEvent.click(screen.getByRole('button', { name: '保存初始化配置' }))
  await waitFor(() => expect(mock.api).toHaveBeenCalledWith('PUT', '/proxies/new-account-policy', expect.objectContaining({ enabled: true, default_proxy_id: 'local', revision: 1 })))
  expect(screen.getByText(/不会自动迁移已有账号/)).toBeTruthy()
  client.clear()
})
