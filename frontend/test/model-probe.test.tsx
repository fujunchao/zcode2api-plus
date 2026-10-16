import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { expect, test, vi } from 'vitest'
import { ProxiesPage } from '@/pages/proxies'

const mock = vi.hoisted(() => ({ api: vi.fn(), success: vi.fn(), error: vi.fn() }))
vi.mock('@/lib/api', () => ({ api: mock.api, errMsg: (e: Error) => e.message }))
vi.mock('sonner', () => ({ toast: { success: mock.success, error: mock.error } }))

function summary() {
  return {
    settings: { enabled: false, mode: 'observe', model: 'glm-5.3-flash', interval_minutes: 30,
      concurrency: 1, timeout_seconds: 30, global_daily_limit: 200, account_daily_limit: 100,
      target_mode: 'manual' as 'auto' | 'manual', exclude_proxy_ids: [] as string[],
      account_mode: 'dedicated' as 'auto' | 'dedicated',
      account_ids: ['a'], target_proxy_ids: ['t'], reference_proxy_ids: ['r'],
      egress_groups: { t: '家庭出口', r: '参考出口' }, allow_direct_control: false, revision: 1 },
    runs: [] as Record<string, unknown>[], accounts: [] as Record<string, unknown>[],
    used_requests: 0, remaining_requests: 200, paused_until: 0,
    next_due: {}, active: 0, rule_version: 'proxy-model-probe-3012-v1',
  }
}

function mount(data = summary()) {
  mock.api.mockReset()
  mock.api.mockImplementation(async (method: string, path: string, body?: unknown) => {
    if (path === '/proxies' && method === 'GET') return { profiles: [
      { id: 't', name: '家庭线路', url: 'http://fixture:8001', enabled: true },
      { id: 'r', name: '参考线路', url: 'http://fixture:8002', enabled: true },
      { id: 'x', name: '停用线路', url: 'http://fixture:8003', enabled: false },
    ] }
    if (path === '/proxies/model-probe') return data
    if (path === '/proxies/model-probe/settings') return method === 'GET' ? data.settings : body
    if (path.startsWith('/accounts')) return { accounts: [{ id: 'a', name: '探测 A', mode: 'jwt', enabled: true, status: 'active', model_probe_only: true }] }
    if (path === '/proxies/new-account-policy') return { enabled: false, default_proxy_id: '', pool_mode: 'all_enabled', proxy_ids: [], max_attempts: 32, attempt_timeout_seconds: 120, round_budget_seconds: 900, revision: 0 }
    if (path.endsWith('/model-probe/history')) return { runs: data.runs }
    return {}
  })
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })
  render(<QueryClientProvider client={client}><ProxiesPage /></QueryClientProvider>)
  return client
}

test('巡检时间可自行设置，后台刷新不覆盖未保存的输入', async () => {
  const data = summary()
  const client = mount(data)
  fireEvent.click(await screen.findByRole('button', { name: '配置健康巡检' }))
  const interval = await screen.findByLabelText('模型巡检间隔（分钟）')
  expect((interval as HTMLInputElement).value).toBe('30')
  fireEvent.change(interval, { target: { value: '17' } })
  client.setQueryData(['model-probe'], { ...data, settings: { ...data.settings, revision: 2 } })
  expect((interval as HTMLInputElement).value).toBe('17')
  fireEvent.submit(screen.getByRole('form', { name: '模型巡检配置' }))
  await waitFor(() => expect(mock.api).toHaveBeenCalledWith('PUT', '/proxies/model-probe/settings', expect.objectContaining({ interval_minutes: 17, revision: 2 })))
  expect(mock.api.mock.calls.some(([method]) => method === 'POST')).toBe(false)
  client.clear()
})

test('未启用和405都不能显示为模型健康，手动请求不能绕过总开关', async () => {
  const data = summary()
  data.runs = [{ id: 'run-405', target_id: 't', source: 'manual', state: 'completed', verdict: 'suspect', reason: 'no_reference', mode: 'observe', rule_version: data.rule_version, created_at: Date.now(), updated_at: Date.now(), next_at: 0, deadline: 0, attempts: 1, confirmed_rounds: 0,
    samples: [{ request_id: 'synthetic', account_ref: 'abc', route_id: 't', role: 'screen', round: 0, at: Date.now(), status: 405, business_code: '3012', kind: 'risk_control', complete: false, usage: { input: 0, output: 0 }, stale: false }] }]
  const client = mount(data)
  expect(await screen.findByText('模型可用性')).toBeTruthy()
  expect(await screen.findByText('待核查')).toBeTruthy()
  const checks = await screen.findAllByRole('button', { name: /立即模型检查/ })
  for (const check of checks) expect((check as HTMLButtonElement).disabled).toBe(true)
  expect(mock.api.mock.calls.some(([method]) => method === 'POST')).toBe(false)
  client.clear()
})

test('统一巡检允许不启用模型而配置网络复核时间', async () => {
  const data = { ...summary(), settings: { ...summary().settings, network_enabled: true, network_interval_minutes: 30, network_confirm_seconds: 10 } }
  const client = mount(data)
  fireEvent.click(await screen.findByRole('button', { name: /配置.*巡检/ }))
  const input = await screen.findByLabelText('网络复核等待（秒）')
  expect((input as HTMLInputElement).value).toBe('10')
  fireEvent.change(input, { target: { value: '7' } })
  fireEvent.submit(screen.getByRole('form', { name: /巡检配置/ }))
  await waitFor(() => expect(mock.api).toHaveBeenCalledWith('PUT', '/proxies/model-probe/settings', expect.objectContaining({ network_confirm_seconds: 7, network_enabled: true, enabled: false })))
  client.clear()
})

test('自动覆盖无需手工选择：表格列出全部启用线路并展示账号择优依据', async () => {
  const data = summary()
  data.settings = { ...data.settings, enabled: true, target_mode: 'auto', account_mode: 'auto', account_ids: [], target_proxy_ids: [], reference_proxy_ids: [] }
  data.accounts = [{ id: 'a', name: '探测 A', eligible: true, rank: 1, recent_success: 3, recent_total: 3, last_success_at: Date.now(), last_pick_at: 0 }]
  data.used_requests = 0
  data.remaining_requests = 200
  const client = mount(data)
  // 自动模式表格覆盖全部启用线路（含未手工勾选的参考线路），停用线路不出现。
  expect(await screen.findByRole('button', { name: '立即模型检查：家庭线路' })).toBeTruthy()
  expect(screen.getByRole('button', { name: '立即模型检查：参考线路' })).toBeTruthy()
  expect(screen.queryByRole('button', { name: '立即模型检查：停用线路' })).toBeNull()
  expect(screen.getByText('单线检测周期约 30 分钟')).toBeTruthy()
  fireEvent.click(await screen.findByRole('button', { name: '配置健康巡检' }))
  expect((await screen.findByLabelText('覆盖范围') as HTMLSelectElement).value).toBe('auto')
  expect(await screen.findByText('近期成功 3/3')).toBeTruthy()
  client.clear()
})

test('切换为自动择优需确认并提示解除专用角色', async () => {
  const data = summary()
  const client = mount(data)
  fireEvent.click(await screen.findByRole('button', { name: '配置健康巡检' }))
  fireEvent.change(await screen.findByLabelText('账号来源'), { target: { value: 'auto' } })
  fireEvent.submit(screen.getByRole('form', { name: '模型巡检配置' }))
  const dialog = await screen.findByRole('alertdialog')
  expect(dialog.textContent).toContain('解除现有探测专用角色')
  fireEvent.click(within(dialog).getByRole('button', { name: '确认并保存' }))
  await waitFor(() => expect(mock.api).toHaveBeenCalledWith('PUT', '/proxies/model-probe/settings', expect.objectContaining({ account_mode: 'auto' })))
  client.clear()
})
