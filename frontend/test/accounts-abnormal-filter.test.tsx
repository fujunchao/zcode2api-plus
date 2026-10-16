import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { afterAll, afterEach, beforeAll, expect, test, vi } from 'vitest'
import { AccountsPage } from '@/pages/accounts'
import type { Account } from '@/lib/types'

const mock = vi.hoisted(() => ({ api: vi.fn(), success: vi.fn(), warning: vi.fn(), error: vi.fn() }))
vi.mock('@/lib/api', () => ({ api: mock.api, errMsg: (e: Error) => e.message }))
vi.mock('sonner', () => ({ toast: { success: mock.success, warning: mock.warning, error: mock.error } }))

// jsdom 没有布局滚动；仅补齐 Radix 键盘选择需要的 DOM 方法，不替换真实筛选组件。
const scrollIntoView = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'scrollIntoView')
beforeAll(() => Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', { configurable: true, value: vi.fn() }))
afterAll(() => {
  if (scrollIntoView) Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', scrollIntoView)
  else Reflect.deleteProperty(HTMLElement.prototype, 'scrollIntoView')
})

const clients: QueryClient[] = []
afterEach(() => clients.splice(0).forEach((client) => client.clear()))

function account(id: string, overrides: Partial<Account> = {}): Account {
  return {
    id, name: id, email: null, provider: 'zai', mode: 'jwt', token_masked: '',
    enabled: true, status: 'active', quota: {}, exhausted_models: [], disabled_models: [],
    plan: {}, plans: [], plan_name: '', plan_is_trial: false,
    use_count: 0, fail_count: 0, stream_truncate_count: 0,
    total_tokens: { input: 0, output: 0, cache_creation: 0, cache_read: 0 },
    last_used_at: null, last_checked_at: null, cooling_until: null,
    last_error: null, last_error_kind: null, last_error_at: null, last_risk_control_at: null,
    proxy_url: null, proxy_id: null, created_at: 0, archived_at: null,
    user_id: null, virtual_device_mid: null, claim: null,
    ...overrides,
  }
}

function fixture() {
  return {
    accounts: [
      account('risk-invalid', { status: 'invalid', last_error_kind: 'risk_control' }),
      account('auth-invalid', { status: 'invalid', last_error_kind: 'auth_failed' }),
      account('disabled-invalid', { status: 'invalid', enabled: false, last_error_kind: 'risk_control' }),
      account('unknown-invalid', { status: 'invalid' }),
      account('risk-cooling', { status: 'cooling', last_error_kind: 'risk_control' }),
      account('risk-history', { last_error_kind: 'risk_control' }),
      account('local-isolation', { local_scheduling_reason: 'risk_isolation' }),
      account('normal'),
      account('exhausted', { status: 'exhausted' }),
      account('disabled', { status: 'disabled', enabled: false }),
      account('archived-invalid', { status: 'invalid', enabled: false, archived_at: 1 }),
      account('initializing-invalid', {
        status: 'invalid',
        initialization: {
          state: 'pending', reason: 'queued', revision: 1, managed: true, blocked: true, paused: false,
          default_proxy_id: '', current_proxy_id: '', attempts: 0, max_attempts: 3,
        },
      }),
    ],
    model_quotas: [{ model: 'GLM-5.3', total: 1000, used: 100, remaining: 900, accounts: 2, items: 2, partial: false }],
    stats: {}, models: [], proxies: [], ts: 1,
  }
}

function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })
  clients.push(client)
  render(<QueryClientProvider client={client}><AccountsPage /></QueryClientProvider>)
  return client
}

function visibleIDs() {
  return screen.queryAllByRole('checkbox', { name: /^選取 / }).map((row) => row.getAttribute('aria-label')?.slice(3))
}

async function selectRiskFilter() {
  fireEvent.keyDown(screen.getByRole('combobox', { name: '依錯誤類型篩選' }), { key: 'ArrowDown' })
  fireEvent.keyDown(await screen.findByRole('option', { name: /^風控攔截/ }), { key: 'Enter' })
  await waitFor(() => expect(screen.queryByRole('listbox')).toBeNull())
}

test('异常入口按当前状态计数，排除冷却、历史错误、归档和初始化且不改变全池额度', async () => {
  mock.api.mockReset().mockResolvedValue(fixture())
  mount()
  fireEvent.click(await screen.findByRole('button', { name: /^异常\s*4$/ }))
  expect(visibleIDs()).toEqual(['risk-invalid', 'auth-invalid', 'disabled-invalid', 'unknown-invalid'])
  expect(screen.getByRole('button', { name: /^全部\s*11$/ })).toBeTruthy()
  expect(screen.getByRole('button', { name: /^初始化\s*1$/ })).toBeTruthy()
  expect(screen.getByRole('button', { name: /^用完\s*1$/ })).toBeTruthy()
  expect(screen.getByRole('button', { name: /^停用\s*1$/ })).toBeTruthy()
  expect(within(screen.getByRole('region', { name: 'GLM-5.3 额度' })).getByText('900')).toBeTruthy()
  expect(mock.api.mock.calls.every(([method, path]) => method === 'GET' && path === '/accounts')).toBe(true)
})

test('没有异常账号时仍显示零数量入口，筛选不展示正常账号', async () => {
  mock.api.mockReset().mockResolvedValue({ ...fixture(), accounts: [account('normal')] })
  mount()
  fireEvent.click(await screen.findByRole('button', { name: /^异常\s*0$/ }))
  expect(visibleIDs()).toEqual([])
  expect(screen.queryByText('normal')).toBeNull()
  expect(screen.queryByRole('button', { name: '批量停用' })).toBeNull()
})

test('异常与风控筛选取交集，状态和错误筛选切换均清空勾选', async () => {
  mock.api.mockReset().mockResolvedValue(fixture())
  mount()
  fireEvent.click(await screen.findByRole('checkbox', { name: '選取 normal' }))
  fireEvent.click(screen.getByRole('button', { name: /^异常\s*4$/ }))
  expect(screen.queryByRole('button', { name: '批量停用' })).toBeNull()
  fireEvent.click(screen.getByRole('checkbox', { name: '選取 auth-invalid' }))
  await selectRiskFilter()
  expect(visibleIDs()).toEqual(['risk-invalid', 'disabled-invalid'])
  expect(screen.getByRole('button', { name: /^异常\s*4$/ })).toBeTruthy()
  expect(screen.queryByRole('button', { name: '批量停用' })).toBeNull()
  fireEvent.click(screen.getByRole('checkbox', { name: '全選當前篩選結果' }))
  fireEvent.click(screen.getByRole('button', { name: /^全部\s*11$/ }))
  expect(visibleIDs()).toEqual(['risk-invalid', 'disabled-invalid', 'risk-cooling', 'risk-history'])
  expect(screen.queryByRole('button', { name: '批量停用' })).toBeNull()
  expect(screen.getByRole('checkbox', { name: '選取 risk-invalid' }).getAttribute('aria-checked')).toBe('false')
})

test.each([
  ['disable', '批量停用'],
  ['delete', '批量刪除'],
])('异常分组的批量 %s 只提交当前筛选集合，删除保留确认', async (operation, label) => {
  mock.api.mockReset().mockImplementation(async (method: string, path: string) => {
    if (method === 'GET' && path === '/accounts') return fixture()
    if (method === 'POST' && path === `/accounts/batch/${operation}`) return {
      total: 2, succeeded: 2, duplicated: 0, not_found: 0, failed: 0, ids: ['risk-invalid', 'disabled-invalid'], items: [],
    }
    throw new Error(`未预期的请求：${method} ${path}`)
  })
  mount()
  fireEvent.click(await screen.findByRole('button', { name: /^异常\s*4$/ }))
  await selectRiskFilter()
  fireEvent.click(screen.getByRole('checkbox', { name: '全選當前篩選結果' }))
  fireEvent.click(screen.getByRole('button', { name: label, exact: true }))
  if (operation === 'delete') {
    expect(mock.api.mock.calls.filter(([method]) => method === 'POST')).toHaveLength(0)
    fireEvent.click(within(await screen.findByRole('alertdialog')).getByRole('button', { name: '確認', exact: true }))
  }
  await waitFor(() => expect(mock.api).toHaveBeenCalledWith('POST', `/accounts/batch/${operation}`, {
    ids: ['risk-invalid', 'disabled-invalid'], missing_ok: true,
  }))
  await waitFor(() => expect(screen.queryByRole('button', { name: label, exact: true })).toBeNull())
})

test('异常组混合恢复保留服务端逐条判定，凭据异常仍可见且风控冷却退出分组', async () => {
  const data = fixture()
  mock.api.mockReset().mockImplementation(async (method: string, path: string) => {
    if (method === 'GET' && path === '/accounts') return structuredClone(data)
    if (method === 'POST' && path === '/accounts/batch/recover') {
      data.accounts[0].status = 'cooling'
      return {
        total: 2, succeeded: 1, duplicated: 0, not_found: 0, failed: 1, ids: ['risk-invalid'],
        items: [
          { index: 0, id: 'risk-invalid', status: 'ok', message: '已恢复；处于本地风控静默隔离，保护冷却中' },
          { index: 1, id: 'auth-invalid', status: 'error', message: '账号非风控失效状态' },
        ],
      }
    }
    throw new Error(`未预期的请求：${method} ${path}`)
  })
  mount()
  fireEvent.click(await screen.findByRole('button', { name: /^异常\s*4$/ }))
  fireEvent.click(screen.getByRole('checkbox', { name: '選取 risk-invalid' }))
  fireEvent.click(screen.getByRole('checkbox', { name: '選取 auth-invalid' }))
  fireEvent.click(screen.getByRole('button', { name: '批量恢復', exact: true }))
  const dialog = await screen.findByRole('alertdialog')
  expect(dialog.textContent).toContain('僅「風控失效」項會成功')
  expect(mock.api.mock.calls.filter(([method]) => method === 'POST')).toHaveLength(0)
  fireEvent.click(within(dialog).getByRole('button', { name: '確認恢復', exact: true }))
  await waitFor(() => expect(mock.api).toHaveBeenCalledWith('POST', '/accounts/batch/recover', {
    ids: ['risk-invalid', 'auth-invalid'], missing_ok: true,
  }))
  await screen.findByRole('button', { name: /^异常\s*3$/ })
  expect(visibleIDs()).toEqual(['auth-invalid', 'disabled-invalid', 'unknown-invalid'])
  expect(mock.warning).toHaveBeenCalledWith(expect.stringContaining('成功 1 · 失敗 1'))
  expect(mock.warning).toHaveBeenCalledWith(expect.stringContaining('風控保護冷卻'))
  expect(mock.error).toHaveBeenCalledWith('账号非风控失效状态')
  expect(mock.success).not.toHaveBeenCalled()
})

test('查询刷新后恢复正常的账号自动退出异常分组，不因历史风控记录继续计数', async () => {
  const data = fixture()
  mock.api.mockReset().mockImplementation(async () => structuredClone(data))
  const client = mount()
  fireEvent.click(await screen.findByRole('button', { name: /^异常\s*4$/ }))
  data.accounts[0].status = 'active'
  await act(async () => { await client.invalidateQueries({ queryKey: ['accounts'] }) })
  await screen.findByRole('button', { name: /^异常\s*3$/ })
  expect(visibleIDs()).toEqual(['auth-invalid', 'disabled-invalid', 'unknown-invalid'])
  expect(data.accounts[0].last_error_kind).toBe('risk_control')
})
