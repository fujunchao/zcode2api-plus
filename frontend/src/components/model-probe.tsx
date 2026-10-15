import { Activity, ArrowRight, Loader2, Settings2 } from 'lucide-react'
import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { useConfirm } from '@/components/confirm'
import { NetworkHealth } from '@/components/network-health'
import { Empty, PanelCard } from '@/components/panel'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { useDraftGroup } from '@/hooks/use-draft-group'
import { api, errMsg } from '@/lib/api'
import type { Account, ModelProbeRun, ModelProbeSettings, ModelProbeSummary, ProxyProfile } from '@/lib/types'

const defaults: ModelProbeSettings = { enabled: false, mode: 'observe', model: 'glm-5.3-flash', interval_minutes: 30, concurrency: 1, timeout_seconds: 30, global_daily_limit: 200, account_daily_limit: 100, account_ids: [], target_proxy_ids: [], reference_proxy_ids: [], egress_groups: {}, allow_direct_control: false, revision: 0 }
const labels: Record<string, string> = {
  unknown: '尚未检测', healthy_recent: '最近模型成功', suspect: '待核查', route_correlated: '线路相关证据成立',
  account_correlated: '更偏账号／身份相关', mixed: '有成功也有失败', inconclusive: '无法确认',
  queued: '等待执行', waiting: '等待对照', running: '正在检查', interrupted: '已中断',
  no_reference: '缺少可用参考线路', reference_unavailable: '参考组不可用，已停止扩散', no_eligible_account: '无符合条件的探测账号',
  reference_expired: '参考已过期，先重新确认参考线路', storage_error: '结果保存失败，已暂停新探测',
  awaiting_control: '等待账号冷却或下一组成对检查', diagnosis_rate_limit: '本目标一小时内已有诊断',
  account_not_ready: '账号仍受冷却、额度或停用限制', account_cooldown_exceeds_budget: '冷却超过本次诊断期限',
  budget_exhausted: '请求预算已用完', diagnosis_budget: '已到诊断次数或时间上限',
  evidence_incomplete_or_contradicted: '证据不足、过期或存在反证', qualified_observe: '已达标，观察模式未执行删除',
  retired: '已严格退役', decision_disabled: '自动决定门禁关闭', evidence_incomplete: '证据不完整，暂停删除',
  stale_snapshot: '账号、线路或策略已变化，旧结果未处置', account_or_target_changed: '账号或目标版本变化',
  policy_changed: '策略已变更', process_restarted: '重启终止旧诊断', process_stopped: '进程关停', persist_failed: '处置保存失败，请检查严格恢复状态',
  risk_control: '405 风控响应', authentication_failed: '账号鉴权失败', quota_exhausted: '额度不足', rate_limited: '上游限流',
  captcha_required: '上游要求验证', incomplete_response: '响应未完整收到', invalid_response: '不是完整模型消息',
  connection_failed: '连接失败', timeout: '请求超时', canceled: '请求取消', invalid_request: '请求构造不满足契约',
  upstream_error: '其他上游错误', redirect_or_invalid_status: '重定向或异常状态', skipped_recent_success: '近期业务已成功，本轮免测', success: '完整模型成功',
}
const numberFields = [
  { key: 'interval_minutes', label: '模型巡检间隔（分钟）', min: 1, max: 1440 },
  { key: 'concurrency', label: '并发检查数', min: 1, max: 2 },
  { key: 'timeout_seconds', label: '单次请求时限（秒）', min: 5, max: 60 },
  { key: 'global_daily_limit', label: '全局 24 小时请求预算', min: 1, max: 10000 },
  { key: 'account_daily_limit', label: '单账号 24 小时请求预算', min: 1, max: 10000 },
] as const
const selectClass = 'w-full rounded-md border bg-background px-3 py-2 text-sm focus-visible:outline-ring'
const time = (ms?: number) => ms ? new Date(ms).toLocaleString('zh-CN', { hour12: false }) : '—'
const toggle = (values: string[], id: string, checked: boolean) => checked ? [...new Set([...values, id])] : values.filter((v) => v !== id)
const sameIDs = (a: string[], b: string[]) => JSON.stringify([...a].sort()) === JSON.stringify([...b].sort())

function describe(run?: ModelProbeRun) {
  if (!run) return '尚未检测'
  if (['queued', 'waiting', 'running', 'interrupted'].includes(run.state)) return labels[run.state]
  if (run.verdict === 'healthy_recent' && Date.now() - (run.observed_at || run.updated_at) > 30 * 60_000) return '成功记录已过期'
  return labels[run.verdict] ?? '无法确认'
}

function confirmedUsage(run: ModelProbeRun) {
  const sum = run.samples.reduce((v, s) => ({ input: v.input + (s.usage?.Input ?? 0), output: v.output + (s.usage?.Output ?? 0), read: v.read + (s.usage?.CacheRead ?? 0), write: v.write + (s.usage?.CacheCreation ?? 0) }), { input: 0, output: 0, read: 0, write: 0 })
  return `已确认 token：输入 ${sum.input} / 输出 ${sum.output} / 缓存读 ${sum.read} / 缓存写 ${sum.write}`
}

export function ModelProbePanel({ profiles }: { profiles: ProxyProfile[] }) {
  const qc = useQueryClient()
  const { confirm, element } = useConfirm()
  const [open, setOpen] = useState(false)
  const [saving, setSaving] = useState(false)
  const [triggering, setTriggering] = useState('')
  const [historyTarget, setHistoryTarget] = useState('')
  const [search, setSearch] = useState('')
  const { data, error } = useQuery({ queryKey: ['model-probe'], queryFn: () => api<ModelProbeSummary>('GET', '/proxies/model-probe'), refetchInterval: 10_000 })
  const draft = useDraftGroup(data?.settings, defaults)
  const value = draft.value
  const { data: accounts, error: accountError } = useQuery({ queryKey: ['model-probe-accounts', search], enabled: open, queryFn: () => api<{ accounts: Account[] }>('GET', '/accounts?limit=200&mode=jwt&keyword=' + encodeURIComponent(search)) })
  const { data: history, error: historyError } = useQuery({ queryKey: ['model-probe-history', historyTarget], enabled: !!historyTarget, queryFn: () => api<{ runs: ModelProbeRun[] }>('GET', '/proxies/' + encodeURIComponent(historyTarget) + '/model-probe/history?limit=20'), refetchInterval: historyTarget ? 10_000 : false })
  const names = new Map(profiles.map((p) => [p.id, p.name]))
  const visibleIDs = [...new Set([...profiles.map((p) => p.id), ...value.target_proxy_ids, ...value.reference_proxy_ids])]
  const visibleAccounts = [...new Set([...(accounts?.accounts ?? []).filter((a) => a.mode === 'jwt').map((a) => a.id), ...value.account_ids])]
  const groupIDs = [...new Set([...value.target_proxy_ids, ...value.reference_proxy_ids, ...(value.allow_direct_control ? ['direct'] : [])])]

  async function save() {
    if (!data?.settings) return
    const version = draft.beginSave()
    setSaving(true)
    try {
      const latest = qc.getQueryData<ModelProbeSummary>(['model-probe'])?.settings ?? data.settings
      const saved = await api<ModelProbeSettings>('PUT', '/proxies/model-probe/settings', { ...value, revision: latest.revision })
      qc.setQueryData<ModelProbeSummary>(['model-probe'], (old) => old ? { ...old, settings: saved } : old)
      draft.accept(version)
      void qc.invalidateQueries({ queryKey: ['accounts'] })
      void qc.invalidateQueries({ queryKey: ['model-probe'] })
      void qc.invalidateQueries({ queryKey: ['network-health'] })
      toast.success('健康巡检配置已保存，后续轮次使用新间隔')
    } catch (e) { toast.error('保存失败：' + errMsg(e)) }
    finally { setSaving(false) }
  }

  function confirmSave() {
    if (!data?.settings) return
    const rolesChanged = !sameIDs(value.account_ids, data.settings.account_ids)
    const destructive = (value.enabled || value.network_enabled) && value.mode === 'auto_retire' && ((!data.settings.enabled && !data.settings.network_enabled) || data.settings.mode !== 'auto_retire')
    if (rolesChanged || destructive || (value.allow_direct_control && !data.settings.allow_direct_control)) {
      confirm({ title: '确认健康巡检策略', description: '选中的探测专用账号将退出普通业务选号。自动模式会处理经过二次复核的网络故障，或严格对照达标的模型 405；固定出口账号仍等待，不擅自换线。仅观察同时约束网络和模型巡检。', confirmText: '确认并保存', danger: !!destructive, onConfirm: save })
    } else void save()
  }

  async function check(id: string) {
    setTriggering(id)
    try {
      await api('POST', '/proxies/' + encodeURIComponent(id) + '/model-probe')
      void qc.invalidateQueries({ queryKey: ['model-probe'] })
      void qc.invalidateQueries({ queryKey: ['model-probe-history', id] })
      toast.success('已加入模型检查队列，重复任务会合并')
    } catch (e) { toast.error('无法开始检查：' + errMsg(e)) }
    finally { setTriggering('') }
  }

  return <PanelCard title="代理健康巡检" subtitle="统一调度：先检查网络，再按账号资格检查 5.3f；两层共用处置模式" badge={<Button variant="outline" size="sm" onClick={() => setOpen(!open)} aria-expanded={open}><Settings2 />配置健康巡检</Button>}>
    {error ? <p role="alert" className="text-sm text-destructive">读取巡检失败：{errMsg(error)}</p> : !data?.settings ? <p role="status" className="text-sm text-muted-foreground">正在读取巡检配置…</p> : <>
      {data.settings.migration_notice && <p role="status" className="rounded-lg border p-3 text-sm">{data.settings.migration_notice}</p>}
      <div className="text-xs font-medium">统一动作：{data.settings.mode === 'observe' ? '仅观察，两层均不自动移除' : '自动处置，网络须复核、模型须严格对照'}</div>
      <NetworkHealth profiles={profiles} enabled={data.settings.network_enabled ?? false} />
      <h3 className="border-t pt-4 text-sm font-semibold">模型可用性</h3>
      <div className="flex flex-wrap items-center gap-x-5 gap-y-2 rounded-lg border bg-muted/30 px-3 py-2 text-xs">
        <span className="flex items-center gap-1.5 font-medium"><Activity className="size-3.5" />{data.settings.enabled ? data.settings.mode === 'auto_retire' ? '自动严格处置' : '仅观察，不删除' : '未启用，不发送探测请求'}</span>
        <span>每 {data.settings.interval_minutes} 分钟 · 错峰执行</span>
        <span className="font-mono tabular-nums">24h {data.used_requests}/{data.settings.global_daily_limit} 次</span>
        <span>剩余 {data.remaining_requests} 次 · 在途 {data.active}</span>
        <span>近 30 分钟已观测 {data.observed_targets ?? 0}/{data.total_targets ?? data.settings.target_proxy_ids.length} 条</span>
      </div>
      {data.storage_error && <p role="alert" className="text-sm text-destructive">巡检结果保存失败，已暂停新探测。修复存储后重新保存配置或重启；旧请求不会自动重放。</p>}
      {(data.evidence_incomplete_until ?? 0) > Date.now() && <p role="status" className="text-xs text-muted-foreground">短窗口证据因重启或容量裁剪不完整，新的严格删除暂停至 {time(data.evidence_incomplete_until)}；已有禁止记录不受影响。</p>}
      {data.paused_until > Date.now() && <p role="status" className="text-sm text-amber-700">参考组不可用，主动对照暂停至 {time(data.paused_until)}；不会因此清空代理池。</p>}
      {data.settings.target_proxy_ids.length === 0 ? <Empty>尚未选择目标。先配置少量探测账号、目标与独立参考线路。</Empty> : <div className="overflow-x-auto"><table className="w-full text-left text-xs"><thead><tr className="border-b text-muted-foreground"><th className="pb-2 font-medium">目标线路</th><th className="pb-2 font-medium">模型结论</th><th className="pb-2 font-medium">最近记录／下一次</th><th className="pb-2 text-right font-medium">操作</th></tr></thead><tbody>
        {data.settings.target_proxy_ids.map((id) => {
          const run = data.runs.find((r) => r.target_id === id)
          const busy = !!run && ['queued', 'running', 'waiting'].includes(run.state)
          return <tr key={id} className="border-b last:border-0"><td className="py-3 pr-3 font-medium">{names.get(id) ?? '已删除线路 ' + id}</td><td className="py-3 pr-3"><div>{describe(run)}</div><div className="mt-1 max-w-72 text-muted-foreground">{run ? labels[run.reason] ?? run.reason : '网络连通不等于模型成功'}</div></td><td className="py-3 pr-3 tabular-nums"><div>{time(run?.updated_at)}</div><div className="mt-1 text-muted-foreground">{time(busy ? run?.next_at : data.next_due[id])}</div></td><td className="py-3 text-right"><div className="flex justify-end gap-2"><Button size="sm" variant="outline" disabled={!data.settings.enabled || busy || !!triggering || !profiles.find((p) => p.id === id)?.enabled} onClick={() => void check(id)} aria-label={'立即模型检查：' + (names.get(id) ?? id)}>{triggering === id && <Loader2 className="animate-spin motion-reduce:animate-none" />}立即模型检查</Button><Button size="sm" variant="ghost" onClick={() => setHistoryTarget(historyTarget === id ? '' : id)}>记录</Button></div></td></tr>
        })}
      </tbody></table></div>}
      {historyTarget && <section aria-label="模型对照记录" className="rounded-lg border p-3"><div className="mb-3 text-sm font-medium">{names.get(historyTarget) ?? historyTarget} · 最近 20 次诊断</div>{historyError ? <p role="alert">读取记录失败：{errMsg(historyError)}</p> : !history ? <p role="status">正在读取记录…</p> : history.runs.length === 0 ? <p className="text-xs text-muted-foreground">还没有模型检查记录。</p> : history.runs.map((run) => <div key={run.id} className="border-b py-3 last:border-0"><div className="flex flex-wrap gap-3 text-xs"><strong>{describe(run)}</strong><span>{time(run.created_at)}</span><span>{run.attempts}/8 次出站 · {run.confirmed_rounds}/2 轮对照</span></div><div className="my-2 flex flex-wrap items-center gap-2">{run.samples.map((s, i) => <div key={s.request_id || i} className="flex items-center gap-2">{i > 0 && <ArrowRight className="size-3 text-muted-foreground" />}<span className={'rounded-md border px-2 py-1 text-xs ' + (s.stale ? 'text-muted-foreground' : s.complete ? 'border-emerald-200 text-emerald-800' : s.status === 405 ? 'border-amber-200 text-amber-800' : '')} title={(labels[s.kind] ?? s.kind) + ' · 账号引用 ' + s.account_ref}>{s.role === 'screen' ? '筛查' : (s.role === 'reference' ? '参考' : '目标') + s.round} · {s.status || '未取得响应'}{s.business_code && '/' + s.business_code}{s.stale && ' · 旧结果'}</span></div>)}</div><p className="mb-1 text-xs text-muted-foreground">{confirmedUsage(run)}</p><p className="text-xs text-muted-foreground">{labels[run.reason] ?? run.reason} · {run.rule_version}{run.decision_id && ' · ' + run.decision_id}</p></div>)}</section>}
    </>}
    {open && <form aria-label="模型巡检配置" className="space-y-4 border-t pt-4" onSubmit={(e) => { e.preventDefault(); confirmSave() }}>
      <fieldset className="space-y-3 rounded-lg border p-3"><legend className="px-1 text-sm font-medium">基础网络巡检</legend>
        <label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={value.network_enabled ?? false} onChange={(e) => draft.edit({ network_enabled: e.target.checked })} />启用基础网络巡检（不需要账号）</label>
        <div className="grid gap-4 sm:grid-cols-2"><div className="space-y-1.5"><Label htmlFor="network-interval">网络巡检间隔（分钟）</Label><Input id="network-interval" type="number" min={1} max={1440} required value={value.network_interval_minutes ?? 30} onChange={(e) => draft.edit({ network_interval_minutes: Number(e.target.value) })} /></div><div className="space-y-1.5"><Label htmlFor="network-confirm">网络复核等待（秒）</Label><Input id="network-confirm" type="number" min={1} max={300} required value={value.network_confirm_seconds ?? 10} onChange={(e) => draft.edit({ network_confirm_seconds: Number(e.target.value) })} /></div></div>
        <p className="text-xs text-muted-foreground">首次异常不删除；再次异常且参考正常才按统一模式处置。网络复核的直连不带任何账号凭据，和下面的模型直连许可不同。</p>
      </fieldset>
      <label className="flex items-center gap-2 text-sm font-medium"><input type="checkbox" checked={value.enabled} onChange={(e) => draft.edit({ enabled: e.target.checked })} />启用模型巡检</label>
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3"><div className="space-y-1.5"><Label htmlFor="model-probe-mode">处置模式</Label><select id="model-probe-mode" className={selectClass} value={value.mode} onChange={(e) => draft.edit({ mode: e.target.value as ModelProbeSettings['mode'] })}><option value="observe">仅观察，不改变业务路由</option><option value="auto_retire">证据达标后严格退役</option></select></div>{numberFields.map((field) => <div key={field.key} className="space-y-1.5"><Label htmlFor={'probe-' + field.key}>{field.label}</Label><Input id={'probe-' + field.key} type="number" min={field.min} max={field.max} required value={value[field.key]} onChange={(e) => draft.edit({ [field.key]: Number(e.target.value) })} /></div>)}</div>
      <p className="text-xs text-muted-foreground">保存后后续轮次使用新间隔，无需重启。冷却、十分钟证据窗口和请求预算仍独立生效；参考与失败请求同样计入预算。</p>
      <fieldset className="space-y-2 rounded-lg border p-3"><legend className="px-1 text-sm font-medium">探测专用账号</legend><p className="text-xs text-muted-foreground">选择后退出普通业务选号；初始化／固定出口托管账号不能加入。至少两个账号才能完整交叉对照，关闭巡检不解除专用角色。</p><Label htmlFor="model-probe-search">搜索探测账号</Label><Input id="model-probe-search" value={search} onChange={(e) => setSearch(e.target.value)} placeholder="按账号名称搜索，每页最多 200 条" />{accountError && <p role="alert" className="text-xs text-destructive">读取账号失败：{errMsg(accountError)}</p>}<div className="grid max-h-48 gap-2 overflow-auto sm:grid-cols-2">{visibleAccounts.map((id) => { const a = accounts?.accounts.find((v) => v.id === id); const selected = value.account_ids.includes(id); return <label key={id} className="flex items-center gap-2 text-xs"><input type="checkbox" checked={selected} disabled={!selected && (!a?.enabled || !!a.initialization?.managed || a.status === 'invalid' || !!a.archived_at)} onChange={(e) => draft.edit({ account_ids: toggle(value.account_ids, id, e.target.checked) })} />{a?.name ?? id}{a?.initialization?.managed && '（固定／初始化托管，不可用）'}</label> })}</div>{!visibleAccounts.length && <p className="text-xs text-muted-foreground">没有可选账号，请先准备未托管的已获额度 JWT。</p>}</fieldset>
      <div className="grid gap-4 md:grid-cols-2">{(['target_proxy_ids', 'reference_proxy_ids'] as const).map((key) => <fieldset key={key} className="rounded-lg border p-3"><legend className="px-1 text-sm font-medium">{key === 'target_proxy_ids' ? '目标线路' : '参考线路'}</legend><div className="mb-2 flex gap-3"><button type="button" className="text-xs underline" onClick={() => draft.edit({ [key]: profiles.filter((p) => p.enabled).map((p) => p.id) })}>选择全部启用线路</button><button type="button" className="text-xs underline" onClick={() => draft.edit({ [key]: [] })}>清空选择</button></div><div className="grid max-h-48 gap-2 overflow-auto">{visibleIDs.map((id) => <label key={id} className="flex items-center gap-2 text-xs"><input type="checkbox" checked={value[key].includes(id)} disabled={!value[key].includes(id) && !profiles.find((p) => p.id === id)?.enabled} onChange={(e) => draft.edit({ [key]: toggle(value[key], id, e.target.checked) })} />{key === 'target_proxy_ids' ? '目标：' : '参考：'}{names.get(id) ?? '已删除 ' + id}</label>)}</div></fieldset>)}</div>
      <label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={value.allow_direct_control} onChange={(e) => draft.edit({ allow_direct_control: e.target.checked })} />允许探测专用账号使用直连作为参考</label>
      <fieldset className="rounded-lg border p-3"><legend className="px-1 text-sm font-medium">出口组声明</legend><p className="mb-3 text-xs text-muted-foreground">同一个家庭公网出口的不同端口填同一个组。自动退役必须有明确不同组的参考；这是你的配置声明，不是服务器自动确认的出口 IP。</p><div className="grid gap-3 sm:grid-cols-2">{groupIDs.map((id) => <div key={id} className="space-y-1.5"><Label htmlFor={'probe-group-' + id}>出口组：{id === 'direct' ? '直连' : names.get(id) ?? id}</Label><Input id={'probe-group-' + id} maxLength={64} value={value.egress_groups[id] ?? ''} onChange={(e) => draft.edit({ egress_groups: { ...value.egress_groups, [id]: e.target.value } })} placeholder="例如：家庭宽带 A" /></div>)}</div></fieldset>
      <div className="flex flex-wrap items-center justify-between gap-3 border-t pt-3"><p className="max-w-xl text-xs text-muted-foreground">模型固定为 glm-5.3-flash，最小回复上限 16 token。正常业务绑定不被探测改写，固定节点失效仍等待，不退回取额池。</p><Button type="submit" disabled={saving || !data?.settings}>{saving && <Loader2 className="animate-spin motion-reduce:animate-none" />}保存模型巡检配置</Button></div>
    </form>}
    {element}
  </PanelCard>
}
