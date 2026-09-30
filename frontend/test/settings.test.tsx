import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { beforeEach, expect, test, vi } from 'vitest'
import { SettingsPage } from '@/pages/settings'
import type { SettingsResponse } from '@/lib/types'

const mock = vi.hoisted(() => ({ api: vi.fn(), success: vi.fn(), error: vi.fn(), setKey: vi.fn() }))
vi.mock('@/lib/api', () => ({ api: mock.api, errMsg: (e: Error) => e.message }))
vi.mock('@/lib/admin-key', () => ({ adminKey: { set: mock.setKey } }))
vi.mock('sonner', () => ({ toast: { success: mock.success, error: mock.error } }))

let backend: SettingsResponse
beforeEach(() => {
  backend = {
    admin_key: 'old-admin', gateway_key: 'gateway', quota_refresh_interval: 60,
    claim_auto_enabled: true, claim_schedule_enabled: false, claim_schedule_time: '23:00',
    claim_captcha_cooldown: 3600, claim_retry_cooldown: 600, claim_preview_cooldown: 60,
    proxy_health_enabled: true, proxy_health_interval: 30, risk_cooling_steps: '300,900,3600',
    upstream_503_cooling_steps: '30,60,120', async_force_direct: false,
    line_truncate_strikes: 3, line_truncate_avoid_seconds: 60,
  }
  mock.api.mockReset()
  mock.api.mockImplementation(async (method: string, _path: string, updates?: Partial<SettingsResponse>) => {
    if (method === 'GET') return { ...backend }
    Object.assign(backend, updates)
    return { ok: true }
  })
})

async function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(<QueryClientProvider client={client}><SettingsPage /></QueryClientProvider>)
  const admin = screen.getByLabelText('後台密碼') as HTMLInputElement
  await waitFor(() => expect(admin.value).toBe('old-admin'))
  return admin
}

test('保存其他分组不会覆盖尚未提交的密码草稿', async () => {
  const admin = await mount()
  fireEvent.change(admin, { target: { value: 'unsaved-admin' } })
  const strikes = screen.getByLabelText('熔斷閾值（同線路連續斷流次數，0=關閉）') as HTMLInputElement
  fireEvent.change(strikes, { target: { value: '5' } })
  fireEvent.submit(strikes.closest('form')!)
  await waitFor(() => expect(mock.api.mock.calls.filter(([method]) => method === 'GET').length).toBeGreaterThan(1))
  await waitFor(() => expect(strikes.value).toBe('5'))
  expect(admin.value).toBe('unsaved-admin')
  expect(backend.admin_key).toBe('old-admin')
})

test('保存期间的新编辑不被迟到的成功响应覆盖', async () => {
  const admin = await mount()
  let finish: (() => void) | undefined
  mock.api.mockImplementation((method: string, _path: string, updates?: Partial<SettingsResponse>) => {
    if (method === 'GET') return Promise.resolve({ ...backend })
    return new Promise((resolve) => { finish = () => { Object.assign(backend, updates); resolve({ ok: true }) } })
  })
  fireEvent.change(admin, { target: { value: 'submitted-admin' } })
  fireEvent.submit(admin.closest('form')!)
  await waitFor(() => expect(finish).toBeDefined())
  fireEvent.change(admin, { target: { value: 'newer-draft' } })
  finish!()
  await waitFor(() => expect(mock.success).toHaveBeenCalled())
  expect(mock.setKey).toHaveBeenCalledWith('submitted-admin')
  expect(admin.value).toBe('newer-draft')
})

test('保存失败后草稿仍然保留', async () => {
  const admin = await mount()
  mock.api.mockImplementation(async (method: string) => {
    if (method === 'GET') return { ...backend }
    throw new Error('数据库失败')
  })
  fireEvent.change(admin, { target: { value: 'failed-draft' } })
  fireEvent.submit(admin.closest('form')!)
  await waitFor(() => expect(mock.error).toHaveBeenCalled())
  expect(admin.value).toBe('failed-draft')
})
