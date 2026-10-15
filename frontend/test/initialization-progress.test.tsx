import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { expect, test, vi } from 'vitest'
import { InitializationProgress } from '@/components/initialization-progress'
import type { Account } from '@/lib/types'

const mock = vi.hoisted(() => ({ api: vi.fn().mockResolvedValue({ ok: true }) }))
vi.mock('@/lib/api', () => ({ api: mock.api, errMsg: (e: Error) => e.message }))

test('已经到账的等待账号只重试绑定，不重复领取', async () => {
  const account = { id: 'a1', enabled: true, initialization: { state: 'awaiting_default', reason: 'default_unavailable', revision: 7, managed: true, blocked: true, paused: false, default_proxy_id: 'fixed', current_proxy_id: '', attempts: 2, max_attempts: 32, granted_at: 1000 } } as Account
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(<QueryClientProvider client={client}><InitializationProgress account={account} proxies={[]} /></QueryClientProvider>)
  expect(screen.queryByRole('button', { name: '重试初始化' })).toBeNull()
  fireEvent.click(screen.getByRole('button', { name: '重试绑定' }))
  await waitFor(() => expect(mock.api).toHaveBeenCalledWith('POST', '/accounts/a1/initialization/bind-default', { revision: 7, use_current_default: false }))
  client.clear()
})
