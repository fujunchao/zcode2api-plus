import { useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { api, errMsg } from '@/lib/api'
import type { Account, ProxyProfile } from '@/lib/types'

const reasons: Record<string, string> = {
  queued: '等待初始化', running: '正在取额', ready: '固定出口已就绪', no_candidates: '等待可用取额节点',
  candidates_exhausted: '本轮节点已尝试完毕', attempts_exhausted: '已达到尝试上限', budget_exhausted: '本轮时限已到',
  default_unavailable: '等待固定业务代理', quota_confirmed: '额度已到账，等待固定绑定', quota_unknown: '等待有效额度查询',
  quota_pending: '先前领取结果待确认', no_plans: '未获得可领取套餐', claimed: '已领取，等待确认额度', already_claimed: '已领取，等待复查',
  ineligible: '上游未提供领取资格', rate_limited: '等待上游额度或限流恢复', captcha_required: '等待验证码',
  proxy_unreachable: '取额节点不可达', activation_failed: '激活上报失败', request_failed: '领取结果待检查',
  credentials_changed: '凭据已变更，请人工确认', route_changed: '线路已变更，请复查', account_disabled: '账号已停用',
  automation_disabled: '自动领取已关闭', historical_quota: '已有配额，不执行新号领取',
}

export function InitializationProgress({ account, proxies }: { account: Account; proxies: ProxyProfile[] }) {
  const qc = useQueryClient()
  const [busy, setBusy] = useState(false)
  const state = account.initialization
  if (!state?.managed) return null
  const label = state.paused ? '初始化已暂停' : reasons[state.reason] ?? state.reason
  const target = proxies.find((p) => p.id === state.default_proxy_id)?.name ?? '目标节点不可用'
  async function act(action: string, useCurrent = false) {
    if (!state) return
    setBusy(true)
    try {
      await api('POST', `/accounts/${encodeURIComponent(account.id)}/initialization/${action}`, { revision: state.revision, use_current_default: useCurrent })
      toast.success(action === 'pause' ? '初始化已暂停' : action === 'bind-default' ? '固定绑定已更新' : '初始化已重新排队')
    } catch (e) { toast.error(errMsg(e)) }
    finally { setBusy(false); void qc.invalidateQueries({ queryKey: ['accounts'] }) }
  }
  return <div className="mt-2 space-y-1 whitespace-normal rounded-md border bg-muted/20 p-2 text-xs" aria-live="polite">
    <div className="font-medium">{state.state === 'completed' && !state.blocked ? '固定出口 · ' + target : label}</div>
    {state.state !== 'completed' && <>
      <div className="text-muted-foreground">已尝试 {state.attempts}/{state.max_attempts} · 目标：{target}</div>
      {state.next_at != null && state.next_at > Date.now() && <div className="text-muted-foreground">下次允许检查：{new Date(state.next_at).toLocaleString()}</div>}
      <div className="flex flex-wrap gap-1">
        {state.granted_at ? <><Button size="xs" variant="outline" disabled={busy || !account.enabled} onClick={() => void act('bind-default')}>重试绑定</Button><Button size="xs" variant="outline" disabled={busy || !account.enabled} onClick={() => void act('bind-default', true)}>改用当前默认代理</Button></> : <Button size="xs" variant="outline" disabled={busy || !account.enabled || (state.next_at != null && state.next_at > Date.now()) || state.reason === 'ineligible'} onClick={() => void act('retry')}>重试初始化</Button>}
        {!state.paused && <Button size="xs" variant="ghost" disabled={busy} onClick={() => void act('pause')}>暂停</Button>}
      </div>
    </>}
  </div>
}
