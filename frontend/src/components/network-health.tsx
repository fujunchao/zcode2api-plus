import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { api, errMsg } from '@/lib/api'
import type { NetworkHealthRecord, ProxyProfile, UpstreamProbe } from '@/lib/types'

const states: Record<string, string> = { healthy: '网络可达', waiting: '等待复核', recovered: '复核已恢复', unconfirmed: '缺少健康参考，保留', confirmed_failure: '网络异常已确认', removed: '已按策略移除', interrupted: '旧轮次已终止', changed: '线路已变化，待重新检测' }
const stamp = (at?: number) => at ? new Date(at).toLocaleString('zh-CN', { hour12: false }) : '—'
function detail(r?: UpstreamProbe) { return r ? (r.targets ?? []).map((t) => `${t.host} ${t.status ?? '无响应'}${t.blocked ? ' 防护拦截' : ''} ${t.ms ?? 0}ms`).join('；') || r.error || '未取得结果' : '未执行' }

export function NetworkHealth({ profiles, enabled }: { profiles: ProxyProfile[]; enabled: boolean }) {
  const qc = useQueryClient()
  const [busy, setBusy] = useState('')
  const [expanded, setExpanded] = useState('')
  const { data, error } = useQuery({ queryKey: ['network-health'], queryFn: () => api<{ network: NetworkHealthRecord[] }>('GET', '/proxies/health'), refetchInterval: 10_000 })
  const records = data?.network ?? []
  async function check(id: string) {
    setBusy(id)
    try { await api('POST', '/proxies/' + encodeURIComponent(id) + '/health-check'); void qc.invalidateQueries({ queryKey: ['network-health'] }); toast.success('已加入统一健康检查队列，首次失败不会删除') }
    catch (e) { toast.error('无法开始检查：' + errMsg(e)) }
    finally { setBusy('') }
  }
  return <section aria-label="基础网络巡检" className="space-y-3">
    <div className="text-sm font-semibold">网络可用性（无需账号）</div>
    <p className="text-xs text-muted-foreground">动态覆盖全部启用代理，后续新增也会自动纳入。首轮失败 → 独立复核 → 健康参考／无凭据直连核查；模型账号不足不影响此层。</p>
    {error && <p role="alert" className="text-sm text-destructive">读取网络结果失败：{errMsg(error)}</p>}
    <div className="max-h-96 overflow-auto rounded-lg border"><table className="w-full text-left text-xs"><thead><tr className="border-b bg-muted/30"><th className="p-2">代理</th><th className="p-2">网络状态</th><th className="p-2">最近检查／复核时间</th><th className="p-2">操作</th></tr></thead><tbody>
      {profiles.filter((p) => p.enabled).map((p) => { const r = records.find((v) => v.profile_id === p.id); const stale = !!r && Date.now() - r.at > 60 * 60_000; return <tr key={p.id} className="border-b last:border-0"><td className="p-2 font-medium">{p.name}</td><td className="p-2">{r ? (stale ? '记录已过期' : states[r.state] ?? r.state) : '尚未检测'}{expanded === p.id && r && <div className="mt-2 space-y-1 text-muted-foreground"><p>首轮：{detail(r.first)}</p><p>复核：{detail(r.second)}</p><p>参考：{detail(r.control)}</p><p>{r.removed ? '已执行移除' : '未执行移除'}</p></div>}</td><td className="p-2 tabular-nums">{stamp(r?.at)}{r?.recheck_at ? <div>复核：{stamp(r.recheck_at)}</div> : null}</td><td className="p-2"><div className="flex gap-1"><Button size="sm" variant="outline" disabled={!enabled || !!busy || r?.state === 'waiting'} onClick={() => void check(p.id)}>立即健康检查</Button><Button size="sm" variant="ghost" onClick={() => setExpanded(expanded === p.id ? '' : p.id)}>明细</Button></div></td></tr> })}
    </tbody></table>{!profiles.some((p) => p.enabled) && <p className="p-4 text-sm text-muted-foreground">没有启用的代理。</p>}</div>
  </section>
}
