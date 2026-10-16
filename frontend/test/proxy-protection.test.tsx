import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { expect, test, vi } from 'vitest'
import { ProxiesPage } from '@/pages/proxies'
import type { ProxyProfile } from '@/lib/types'

const mock = vi.hoisted(() => ({ api: vi.fn() }))
vi.mock('@/lib/api', () => ({ api: mock.api, errMsg: (e: Error) => e.message }))
vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))
vi.mock('@/components/proxy-subscription', () => ({ ProxySubscription: () => null }))
vi.mock('@/components/new-account-policy', () => ({ NewAccountPolicy: () => null }))
vi.mock('@/components/health/panel', () => ({ HealthPanel: () => null }))

function page(profiles: ProxyProfile[] = []) {
  mock.api.mockReset()
  mock.api.mockImplementation(async (method: string, path: string) => {
    if (method === 'GET' && path === '/proxies') return { profiles }
    if (method === 'POST' || method === 'PUT') return { ok: true }
    throw new Error(`未预期的请求：${method} ${path}`)
  })
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })
  render(<QueryClientProvider client={client}><ProxiesPage /></QueryClientProvider>)
  return client
}

test.each([true, false])('新增手工节点默认保护，允许明确选择 %s', async (protect) => {
  const client = page()
  fireEvent.click(screen.getByRole('button', { name: '新增代理' }))
  const dialog = await screen.findByRole('dialog')
  const checkbox = within(dialog).getByRole('checkbox', { name: '禁止自动删除' }) as HTMLInputElement
  expect(checkbox.checked).toBe(true)
  if (!protect) fireEvent.click(checkbox)
  fireEvent.change(within(dialog).getByLabelText('名稱'), { target: { value: '家庭服务器' } })
  fireEvent.change(within(dialog).getByLabelText('地址'), { target: { value: 'http://127.0.0.1:18080' } })
  fireEvent.click(within(dialog).getByRole('button', { name: '儲存' }))
  await waitFor(() => expect(mock.api).toHaveBeenCalledWith('POST', '/proxies', {
    name: '家庭服务器', url: 'http://127.0.0.1:18080', enabled: true, auto_delete_protected: protect,
  }))
  client.clear()
})

test('编辑已有保护节点可以解除手工保护，但不能顺手启用已停用的节点', async () => {
  const client = page([{ id: 'home', name: '家庭出口', url: 'http://127.0.0.1:18080', enabled: false, auto_delete_protected: true, auto_delete_protection: 'manual' }])
  expect(await screen.findByText('禁止自动删除 · 已开启手工保护')).toBeTruthy()
  fireEvent.click(screen.getByTitle('編輯'))
  const dialog = await screen.findByRole('dialog')
  const checkbox = within(dialog).getByRole('checkbox', { name: '禁止自动删除' }) as HTMLInputElement
  expect(checkbox.checked).toBe(true)
  expect(checkbox.disabled).toBe(false)
  fireEvent.click(checkbox)
  fireEvent.click(within(dialog).getByRole('button', { name: '儲存' }))
  await waitFor(() => expect(mock.api).toHaveBeenCalledWith('PUT', '/proxies/home', {
    name: '家庭出口', url: 'http://127.0.0.1:18080', enabled: false, auto_delete_protected: false,
  }))
  client.clear()
})

test('默认出口的有效保护不能用编辑开关绕过，也不误写成永久手工保护', async () => {
  const client = page([{ id: 'home', name: '家庭默认出口', url: 'http://127.0.0.1:18080', enabled: true, auto_delete_protected: false, auto_delete_protection: 'default_proxy' }])
  expect(await screen.findByText('禁止自动删除 · 默认业务出口，自动保护')).toBeTruthy()
  fireEvent.click(screen.getByTitle('編輯'))
  const dialog = await screen.findByRole('dialog')
  const checkbox = within(dialog).getByRole('checkbox', { name: '禁止自动删除' }) as HTMLInputElement
  expect(checkbox.checked).toBe(true)
  expect(checkbox.disabled).toBe(true)
  expect(dialog.textContent).toContain('需先调整对应默认出口')
  fireEvent.click(within(dialog).getByRole('button', { name: '儲存' }))
  await waitFor(() => expect(mock.api).toHaveBeenCalledWith('PUT', '/proxies/home', {
    name: '家庭默认出口', url: 'http://127.0.0.1:18080', enabled: true, auto_delete_protected: false,
  }))
  client.clear()
})
