import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { expect, test, vi } from 'vitest'
import { AccountsPage } from '@/pages/accounts'

const mock = vi.hoisted(() => ({ api: vi.fn(), success: vi.fn(), error: vi.fn() }))
vi.mock('@/lib/api', () => ({ api: mock.api, errMsg: (e: Error) => e.message }))
vi.mock('sonner', () => ({ toast: { success: mock.success, error: mock.error } }))

/* 行渲染会访问的字段给全；风控失效 = status=invalid 且 last_error_kind=risk_control。 */
function account(over: Record<string, unknown>) {
  return {
    id: 'a1', name: 'n', email: null, provider: 'zai', mode: 'jwt', token_masked: '',
    enabled: true, status: 'invalid', quota: {}, exhausted_models: [], disabled_models: [],
    plan: null, plans: [], plan_name: '', plan_is_trial: false,
    use_count: 0, fail_count: 0, stream_truncate_count: 0, rate_limit_streak: 0,
    total_tokens: { input: 0, output: 0, cache_creation: 0, cache_read: 0 },
    last_used_at: null, last_checked_at: null, cooling_until: null,
    last_error: null, last_error_kind: null, last_error_at: null,
    proxy_url: null, proxy_id: null, created_at: 0, archived_at: null,
    user_id: null, virtual_device_mid: null, claim: null,
    ...over,
  }
}

function fixture() {
  return {
    accounts: [
      account({ id: 'risk', name: '風控號', last_error_kind: 'risk_control', last_error_at: Date.now() / 1000 - 3600 }),
      account({ id: 'auth', name: '憑證號', last_error_kind: 'auth_failed' }),
      // 冷却中的风控命中不在恢复范围内（会自然到期）。
      account({ id: 'cool', name: '冷卻號', status: 'cooling', cooling_until: Date.now() / 1000 + 600, last_error_kind: 'risk_control' }),
    ],
    model_quotas: [], stats: {}, models: [], proxies: [], ts: 1,
  }
}

function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })
  render(<QueryClientProvider client={client}><AccountsPage /></QueryClientProvider>)
  return client
}

test('手動恢復按鈕僅對風控失效帳號顯示', async () => {
  mock.api.mockReset().mockResolvedValue(fixture())
  const client = mount()
  await screen.findByText('風控號')
  await screen.findByText('憑證號')
  // 憑證異常與冷卻中的行都不顯示恢復按鈕，全表只有「風控失效」一行有。
  expect(screen.getAllByTitle('手動恢復（風控失效）')).toHaveLength(1)
  client.clear()
})

test('單號手動恢復：確認框提示上游標記限制，確認後調用 recover 端點', async () => {
  const data = fixture()
  mock.api.mockReset().mockImplementation(async (method: string, path: string) => {
    if (method === 'GET' && path === '/accounts') return structuredClone(data)
    if (method === 'POST' && path === '/accounts/risk/recover') return { ok: true }
    throw new Error(`未预期的请求：${method} ${path}`)
  })
  const client = mount()
  fireEvent.click(await screen.findByTitle('手動恢復（風控失效）'))
  const dialog = await screen.findByRole('alertdialog')
  expect(dialog.textContent).toContain('不會解除上游仍生效的風控標記')
  expect(dialog.textContent).toContain('最近風控')
  fireEvent.click(within(dialog).getByRole('button', { name: '確認恢復', exact: true }))
  await waitFor(() => expect(mock.api).toHaveBeenCalledWith('POST', '/accounts/risk/recover'))
  await waitFor(() => expect(mock.success).toHaveBeenCalledWith('已恢復'))
  expect(mock.error).not.toHaveBeenCalled()
  client.clear()
})

test('批量恢復：勾選後對所選集合調用批量端點', async () => {
  const data = fixture()
  mock.api.mockReset().mockImplementation(async (method: string, path: string) => {
    if (method === 'GET' && path === '/accounts') return structuredClone(data)
    if (method === 'POST' && path === '/accounts/batch/recover') {
      return {
        total: 1, succeeded: 1, duplicated: 0, not_found: 0, failed: 0, ids: ['risk'],
        items: [{ index: 0, id: 'risk', status: 'ok' }],
      }
    }
    throw new Error(`未预期的请求：${method} ${path}`)
  })
  const client = mount()
  await screen.findByText('風控號')
  fireEvent.click(screen.getByRole('checkbox', { name: '選取 風控號' }))
  fireEvent.click(await screen.findByRole('button', { name: /批量恢復/ }))
  const dialog = await screen.findByRole('alertdialog')
  fireEvent.click(within(dialog).getByRole('button', { name: '確認恢復', exact: true }))
  await waitFor(() =>
    expect(mock.api).toHaveBeenCalledWith('POST', '/accounts/batch/recover', { ids: ['risk'], missing_ok: true }),
  )
  await waitFor(() => expect(mock.success).toHaveBeenCalledWith('批量恢復完成：成功 1'))
  client.clear()
})
