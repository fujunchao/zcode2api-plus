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

/* 有额度的普通账号：不应进「无额度」组。 */
function quotaWindow(): Account['quota'] {
  return {
    'GLM-5.3': {
      model: 'GLM-5.3', plan_name: '', plan_is_trial: false,
      total: 3_000_000, used: 0, remaining: 3_000_000, available: 3_000_000,
    },
  }
}

function account(id: string, overrides: Partial<Account> = {}): Account {
  return {
    id, name: id, email: null, provider: 'zai', mode: 'jwt', token_masked: '',
    enabled: true, status: 'active', quota: quotaWindow(),
    exhausted_models: [], disabled_models: [],
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

function initialization(overrides: Partial<NonNullable<Account['initialization']>> = {}): NonNullable<Account['initialization']> {
  return {
    state: 'pending', reason: 'queued', revision: 1, managed: true, blocked: true, paused: false,
    default_proxy_id: '', current_proxy_id: '', attempts: 0, max_attempts: 3,
    ...overrides,
  }
}

function fixture() {
  return {
    accounts: [
      account('hit-active', { quota: {} }),
      account('hit-cooling', { status: 'cooling', quota: {} }),
      account('hit-null', { quota: null as unknown as Account['quota'] }),
      account('miss-init', { quota: {}, initialization: initialization() }),
      account('miss-await-proxy', { quota: {}, initialization: initialization({ granted_at: 1_700_000_000_000 }) }),
      account('miss-isolation', { quota: {}, local_scheduling_reason: 'risk_isolation', local_recheck_at: 1_700_000_000 }),
      account('miss-probe', { quota: {}, model_probe_only: true }),
      account('miss-quota'),
      account('miss-apikey', { mode: 'apiKey' as const, quota: {} }),
      account('miss-disabled', { status: 'disabled', enabled: false, quota: {} }),
      account('miss-invalid', { status: 'invalid', quota: {} }),
      account('miss-exhausted', { status: 'exhausted', quota: {} }),
      account('archived-hit', { quota: {}, archived_at: 1_700_000_000 }),
    ],
    model_quotas: [{ model: 'GLM-5.3', total: 1000, used: 100, remaining: 900, accounts: 1, items: 1, partial: false }],
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

async function selectErrorFilter(label: RegExp) {
  fireEvent.keyDown(screen.getByRole('combobox', { name: '依錯誤類型篩選' }), { key: 'ArrowDown' })
  fireEvent.keyDown(await screen.findByRole('option', { name: label }), { key: 'Enter' })
  await waitFor(() => expect(screen.queryByRole('listbox')).toBeNull())
}

test('无额度分组按口径计数，只显示普通 JWT 账号，排除初始化/隔离/探测/停用/失效/用完', async () => {
  mock.api.mockReset().mockResolvedValue(fixture())
  mount()

  const group = await screen.findByRole('button', { name: /^无额度\s*3$/ })
  expect(screen.getByRole('button', { name: /^全部\s*12$/ })).toBeTruthy()
  expect(screen.getByRole('button', { name: /^初始化\s*2$/ })).toBeTruthy()
  expect(screen.getByRole('button', { name: /^异常\s*1$/ })).toBeTruthy()
  expect(screen.getByRole('button', { name: /^停用\s*1$/ })).toBeTruthy()
  expect(screen.getByRole('button', { name: /^用完\s*1$/ })).toBeTruthy()

  fireEvent.click(group)
  expect(screen.getByRole('button', { name: /^无额度\s*3$/, pressed: true })).toBeTruthy()
  expect(visibleIDs()).toEqual(['hit-active', 'hit-cooling', 'hit-null'])
  // 归档账号不参与筛选与计数；全池额度汇总不随筛选变化。
  expect(visibleIDs()).not.toContain('archived-hit')
  expect(within(screen.getByRole('region', { name: 'GLM-5.3 额度' })).getByText('900')).toBeTruthy()
  expect(mock.api.mock.calls.every(([method, path]) => method === 'GET' && path === '/accounts')).toBe(true)
})

test('没有命中账号时仍显示零数量入口，并使用无额度专属空态文案', async () => {
  mock.api.mockReset().mockResolvedValue({ ...fixture(), accounts: [account('normal', { quota: quotaWindow() })] })
  mount()
  fireEvent.click(await screen.findByRole('button', { name: /^无额度\s*0$/ }))
  expect(visibleIDs()).toEqual([])
  expect(screen.getByText('暂无符合筛选条件的无额度账号')).toBeTruthy()
  expect(screen.queryByText('尚無帳號，請點擊右上角「新增」')).toBeNull()
  expect(screen.queryByRole('button', { name: '批量停用' })).toBeNull()
})

test('无额度与错误类型筛选取交集，分组计数不随错误筛选变化', async () => {
  mock.api.mockReset().mockResolvedValue({
    ...fixture(),
    accounts: [
      account('hit-error', { quota: {}, last_error_kind: 'quota_query_failed', last_error: '额度查询失败 HTTP 405', last_error_at: 1_700_000_000 }),
      account('hit-plain', { quota: {} }),
      account('miss-quota'),
    ],
  })
  mount()
  fireEvent.click(await screen.findByRole('button', { name: /^无额度\s*2$/ }))
  await selectErrorFilter(/^額度查詢失敗/)
  expect(visibleIDs()).toEqual(['hit-error'])
  expect(screen.getByRole('button', { name: /^无额度\s*2$/ })).toBeTruthy()
})

test('切换分组或错误类型筛选都会清空勾选', async () => {
  mock.api.mockReset().mockResolvedValue(fixture())
  mount()
  fireEvent.click(await screen.findByRole('button', { name: /^无额度\s*3$/ }))
  fireEvent.click(screen.getByRole('checkbox', { name: '選取 hit-active' }))
  expect(screen.getByRole('button', { name: '批量停用' })).toBeTruthy()

  fireEvent.click(screen.getByRole('button', { name: /^全部\s*12$/ }))
  expect(screen.queryByRole('button', { name: '批量停用' })).toBeNull()
  expect(screen.getByRole('checkbox', { name: '選取 hit-active' }).getAttribute('aria-checked')).toBe('false')
})

test('批量删除只提交当前可见选中项，且必须先确认', async () => {
  mock.api.mockReset().mockImplementation(async (method: string, path: string) => {
    if (method === 'GET' && path === '/accounts') return fixture()
    if (method === 'POST' && path === '/accounts/batch/delete') {
      return { total: 2, succeeded: 2, duplicated: 0, not_found: 0, failed: 0, ids: ['hit-active', 'hit-cooling'], items: [] }
    }
    throw new Error('未预期的请求：' + method + ' ' + path)
  })
  mount()
  fireEvent.click(await screen.findByRole('button', { name: /^无额度\s*3$/ }))
  fireEvent.click(screen.getByRole('checkbox', { name: '全選當前篩選結果' }))
  fireEvent.click(screen.getByRole('button', { name: '批量刪除', exact: true }))

  expect(mock.api.mock.calls.filter(([method]) => method === 'POST')).toHaveLength(0)
  fireEvent.click(within(await screen.findByRole('alertdialog')).getByRole('button', { name: '確認', exact: true }))
  await waitFor(() => expect(mock.api).toHaveBeenCalledWith('POST', '/accounts/batch/delete', {
    ids: ['hit-active', 'hit-cooling', 'hit-null'], missing_ok: true,
  }))
})

test('额度恢复后账号自动退出无额度分组，计数同步更新', async () => {
  const data = fixture()
  mock.api.mockReset().mockImplementation(async () => structuredClone(data))
  const client = mount()
  fireEvent.click(await screen.findByRole('button', { name: /^无额度\s*3$/ }))
  expect(visibleIDs()).toEqual(['hit-active', 'hit-cooling', 'hit-null'])

  data.accounts[0].quota = quotaWindow()
  await act(async () => { await client.invalidateQueries({ queryKey: ['accounts'] }) })
  await screen.findByRole('button', { name: /^无额度\s*2$/ })
  expect(visibleIDs()).toEqual(['hit-cooling', 'hit-null'])
})
