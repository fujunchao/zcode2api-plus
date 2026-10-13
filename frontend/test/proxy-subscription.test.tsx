import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { expect, test, vi } from 'vitest'
import { ProxySubscription } from '@/components/proxy-subscription'
import type { SubscriptionRun, SubscriptionView } from '@/lib/types'

const mock = vi.hoisted(() => ({ api: vi.fn(), success: vi.fn(), error: vi.fn() }))
vi.mock('@/lib/api', () => ({ api: mock.api, errMsg: (e: Error) => e.message }))
vi.mock('sonner', () => ({ toast: { success: mock.success, error: mock.error } }))

function view(): SubscriptionView {
  return {
    config: { revision: 1, configured: true, source_display: 'http://fixture/…', auto: false,
      interval_minutes: 10, max_age_minutes: 120, candidates: 50, import_limit: 10, capacity: 50,
      concurrency: 4, budget_seconds: 180, enable_imported: false },
    current: null, last_result: null, runs: [], owned: 0, next_run_at: 0, import_paused: false, target_supported: true,
  }
}

function run(): SubscriptionRun {
  return { id: 'synthetic-run', mode: 'preview', automatic: false, status: 'probing', started_at: 1791892313000,
    finished_at: 0, received: 10, rejected: 0, expired: 0, existing: 0, blocked: 0, candidates: 10,
    probed: 2, passed: 1, added: 0, commit_skipped: 0, details: [] }
}

function mount(data: SubscriptionView) {
  mock.api.mockReset()
  mock.api.mockImplementation(async (method: string, path: string) => {
    if (method === 'GET' && path === '/proxies/subscription') return data
    if (method === 'GET' && path === '/proxies/subscription/blocks') return { blocks: [] }
    return {}
  })
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })
  render(<QueryClientProvider client={client}><ProxySubscription /></QueryClientProvider>)
  return client
}

test('收起时不发请求，配置默认关闭且保存不回传隐藏地址', async () => {
  const client = mount(view())
  expect(mock.api).not.toHaveBeenCalled()
  fireEvent.click(screen.getByRole('button', { name: '配置自动补充' }))
  const form = await screen.findByRole('form', { name: '订阅配置' })
  expect((screen.getByLabelText('自动同步') as HTMLInputElement).checked).toBe(false)
  expect((screen.getByLabelText('新增线路自动启用') as HTMLInputElement).checked).toBe(false)
  expect((screen.getByLabelText('JSON 订阅地址') as HTMLInputElement).value).toBe('')
  fireEvent.change(screen.getByLabelText('每轮复检上限'), { target: { value: '200' } })
  fireEvent.submit(form)
  await waitFor(() => expect(mock.api).toHaveBeenCalledWith('PUT', '/proxies/subscription',
    expect.objectContaining({ revision: 1, candidates: 200, enable_imported: false })))
  const payload = mock.api.mock.calls.find(([method]) => method === 'PUT')![2]
  expect(payload).not.toHaveProperty('url')
  expect(payload).not.toHaveProperty('source_display')
  client.clear()
})

test('预览不导入；导入需要明确确认并说明新线路停用', async () => {
  const client = mount(view())
  fireEvent.click(screen.getByRole('button', { name: '配置自动补充' }))
  fireEvent.click(await screen.findByRole('button', { name: '预览并复检（不导入）' }))
  await waitFor(() => expect(mock.api).toHaveBeenCalledWith('POST', '/proxies/subscription/runs', { mode: 'preview' }))
  const importButton = screen.getByRole('button', { name: '立即导入', exact: true })
  await waitFor(() => expect((importButton as HTMLButtonElement).disabled).toBe(false))
  fireEvent.click(importButton)
  const dialog = await screen.findByRole('alertdialog')
  expect(dialog.textContent).toContain('保持停用')
  expect(dialog.textContent).toContain('不会直接改派现有账号')
  expect(mock.api).not.toHaveBeenCalledWith('POST', '/proxies/subscription/runs', { mode: 'import' })
  fireEvent.click(within(dialog).getByRole('button', { name: '开始导入' }))
  await waitFor(() => expect(mock.api).toHaveBeenCalledWith('POST', '/proxies/subscription/runs', { mode: 'import' }))
  client.clear()
})

test('刷新后显示在途任务并能取消，不能重复启动', async () => {
  const d = view(); d.current = run()
  const client = mount(d)
  fireEvent.click(screen.getByRole('button', { name: '配置自动补充' }))
  const cancel = await screen.findByRole('button', { name: '取消当前任务' })
  expect((screen.getByRole('button', { name: '立即导入', exact: true }) as HTMLButtonElement).disabled).toBe(true)
  expect(screen.getByText('已复检 2')).toBeTruthy()
  fireEvent.click(cancel)
  await waitFor(() => expect(mock.api).toHaveBeenCalledWith('POST', '/proxies/subscription/runs/synthetic-run/cancel'))
  client.clear()
})

test('清空地址必须显式选择，保存时不附加旧 URL', async () => {
  const client = mount(view())
  fireEvent.click(screen.getByRole('button', { name: '配置自动补充' }))
  fireEvent.click(await screen.findByLabelText('清空地址并关闭自动同步'))
  fireEvent.submit(screen.getByRole('form', { name: '订阅配置' }))
  await waitFor(() => expect(mock.api).toHaveBeenCalledWith('PUT', '/proxies/subscription',
    expect.objectContaining({ clear_url: true, auto: false })))
  client.clear()
})
