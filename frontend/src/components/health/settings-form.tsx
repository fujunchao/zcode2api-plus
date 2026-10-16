import { ChevronRight, Loader2 } from 'lucide-react'
import { useState } from 'react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from '@/components/ui/sheet'
import { accountReasons, numberFields, selectClass, toggle } from '@/components/health/labels'
import type { Account, ModelProbeSettings, ModelProbeSummary, ProxyProfile } from '@/lib/types'

type Draft = ReturnType<typeof import('@/hooks/use-draft-group').useDraftGroup<ModelProbeSettings>>

/**
 * 配置抽屉：基础项常显，高级项默认折叠。
 * 自动覆盖／自动择优（v2.10.10 默认）下不再渲染线路与账号列表——表单长度不随线路数量增长。
 */
export function HealthSettingsSheet({ open, onOpenChange, data, profiles, value, edit, saving, onSubmit, search, onSearch, accounts, accountError }: {
  open: boolean
  onOpenChange: (open: boolean) => void
  data: ModelProbeSummary
  profiles: ProxyProfile[]
  value: ModelProbeSettings
  edit: Draft['edit']
  saving: boolean
  onSubmit: () => void
  search: string
  onSearch: (v: string) => void
  accounts?: { accounts: Account[] }
  accountError?: unknown
}) {
  const [advanced, setAdvanced] = useState(false)
  const targetMode = value.target_mode ?? 'auto'
  const accountMode = value.account_mode ?? 'auto'
  const excludedIDs = value.exclude_proxy_ids ?? []
  const names = new Map(profiles.map((p) => [p.id, p.name]))
  const visibleIDs = [...new Set([...profiles.map((p) => p.id), ...value.target_proxy_ids, ...value.reference_proxy_ids])]
  const visibleAccounts = [...new Set([...(accounts?.accounts ?? []).filter((a) => a.mode === 'jwt').map((a) => a.id), ...value.account_ids])]

  return <Sheet open={open} onOpenChange={onOpenChange}>
    <SheetContent className="w-full overflow-y-auto sm:max-w-2xl">
      <SheetHeader>
        <SheetTitle>健康巡检配置</SheetTitle>
        <SheetDescription>统一调度网络与模型两层，两层共用处置模式。保存后后续轮次使用新间隔，无需重启。</SheetDescription>
      </SheetHeader>
      <form aria-label="模型巡检配置" className="space-y-4 px-4 pb-6" onSubmit={(e) => { e.preventDefault(); onSubmit() }}>
        <fieldset className="space-y-3 rounded-lg border p-3"><legend className="px-1 text-sm font-medium">基础网络巡检</legend>
          <label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={value.network_enabled ?? false} onChange={(e) => edit({ network_enabled: e.target.checked })} />启用基础网络巡检（不需要账号）</label>
          <div className="grid gap-4 sm:grid-cols-2"><div className="space-y-1.5"><Label htmlFor="network-interval">网络巡检间隔（分钟）</Label><Input id="network-interval" type="number" min={1} max={1440} required value={value.network_interval_minutes ?? 30} onChange={(e) => edit({ network_interval_minutes: Number(e.target.value) })} /></div><div className="space-y-1.5"><Label htmlFor="network-confirm">网络复核等待（秒）</Label><Input id="network-confirm" type="number" min={1} max={300} required value={value.network_confirm_seconds ?? 10} onChange={(e) => edit({ network_confirm_seconds: Number(e.target.value) })} /></div></div>
          <p className="text-xs text-muted-foreground">首次异常不删除；再次异常且参考正常才按统一模式处置。网络复核的直连不带任何账号凭据，和下面的模型直连许可不同。</p>
        </fieldset>

        <label className="flex items-center gap-2 text-sm font-medium"><input type="checkbox" checked={value.enabled} onChange={(e) => edit({ enabled: e.target.checked })} />启用模型巡检</label>
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3"><div className="space-y-1.5"><Label htmlFor="model-probe-mode">处置模式</Label><select id="model-probe-mode" className={selectClass} value={value.mode} onChange={(e) => edit({ mode: e.target.value as ModelProbeSettings['mode'] })}><option value="observe">仅观察，不改变业务路由</option><option value="auto_retire">证据达标后严格退役</option></select></div>{numberFields.map((field) => <div key={field.key} className="space-y-1.5"><Label htmlFor={'probe-' + field.key}>{field.label}</Label><Input id={'probe-' + field.key} type="number" min={field.min} max={field.max} required value={value[field.key]} onChange={(e) => edit({ [field.key]: Number(e.target.value) })} /></div>)}</div>
        <p className="text-xs text-muted-foreground">冷却、十分钟证据窗口和请求预算仍独立生效；参考与失败请求同样计入预算。</p>

        <div className="rounded-lg border">
          <button type="button" className="flex w-full items-center gap-2 p-3 text-left text-sm font-medium" aria-expanded={advanced} onClick={() => setAdvanced(!advanced)}>
            <ChevronRight className={'size-4 transition-transform motion-reduce:transition-none ' + (advanced ? 'rotate-90' : '')} />高级选项
            <span className="ml-auto text-xs font-normal text-muted-foreground">目标与参考线路、探测账号、直连许可</span>
          </button>
          {advanced && <div className="space-y-4 border-t p-3">
            <fieldset className="space-y-2 rounded-lg border p-3"><legend className="px-1 text-sm font-medium">探测账号</legend><div className="space-y-1.5"><Label htmlFor="model-probe-account-mode">账号来源</Label><select id="model-probe-account-mode" className={selectClass} value={accountMode} onChange={(e) => edit({ account_mode: e.target.value as ModelProbeSettings['account_mode'] })}><option value="auto">自动择优（按近期成功率，账号留在业务池）</option><option value="dedicated">手工指定专用账号（退出业务选号）</option></select></div>{accountMode === 'auto' ? <>
              <p className="text-xs text-muted-foreground">每次诊断按近期成功率从全部合格 JWT 账号中自动挑选，账号留在业务池、持久绑定不被改写；初始化／固定出口托管账号不可借用。探测会消耗所选账号的少量额度，收到 405 仍按现有规则冷却该账号。</p>
              <div className="grid max-h-48 gap-2 overflow-auto sm:grid-cols-2">{(data.accounts ?? []).map((a) => <div key={a.id} className="flex items-center gap-2 text-xs"><span className={'shrink-0 rounded-md border px-1.5 py-0.5 tabular-nums ' + (a.eligible ? 'border-emerald-200 text-emerald-800' : 'text-muted-foreground')}>{a.eligible ? `#${a.rank ?? '-'}` : '不可用'}</span><span className="truncate">{a.name}</span><span className="truncate text-muted-foreground">{a.eligible ? `近期成功 ${a.recent_success}/${a.recent_total}` : accountReasons[a.reason ?? ''] ?? a.reason ?? '不可用'}</span></div>)}</div>
              {!(data.accounts ?? []).length && <p className="text-xs text-muted-foreground">暂无合格账号：需要未托管的 JWT 账号，且 5.3f 额度已知且可用。</p>}
            </> : <>
              <p className="text-xs text-muted-foreground">选择后退出普通业务选号；初始化／固定出口托管账号不能加入。至少两个账号才能完整交叉对照，关闭巡检不解除专用角色。</p>
              <Label htmlFor="model-probe-search">搜索探测账号</Label><Input id="model-probe-search" value={search} onChange={(e) => onSearch(e.target.value)} placeholder="按账号名称搜索，每页最多 200 条" />
              {accountError ? <p role="alert" className="text-xs text-destructive">读取账号失败</p> : null}
              <div className="grid max-h-48 gap-2 overflow-auto sm:grid-cols-2">{visibleAccounts.map((id) => { const a = accounts?.accounts.find((v) => v.id === id); const selected = value.account_ids.includes(id); return <label key={id} className="flex items-center gap-2 text-xs"><input type="checkbox" checked={selected} disabled={!selected && (!a?.enabled || !!a.initialization?.managed || a.status === 'invalid' || !!a.archived_at)} onChange={(e) => edit({ account_ids: toggle(value.account_ids, id, e.target.checked) })} />{a?.name ?? id}{a?.initialization?.managed && '（固定／初始化托管，不可用）'}</label> })}</div>
              {!visibleAccounts.length && <p className="text-xs text-muted-foreground">没有可选账号，请先准备未托管的已获额度 JWT。</p>}
            </>}</fieldset>

            <fieldset className="rounded-lg border p-3"><legend className="px-1 text-sm font-medium">目标线路</legend><div className="mb-2 space-y-1.5"><Label htmlFor="model-probe-target-mode">覆盖范围</Label><select id="model-probe-target-mode" className={selectClass} value={targetMode} onChange={(e) => edit({ target_mode: e.target.value as ModelProbeSettings['target_mode'] })}><option value="auto">自动覆盖全部启用线路</option><option value="manual">手工指定目标线路</option></select></div>{targetMode === 'auto' ? <>
              <p className="mb-2 text-xs text-muted-foreground">动态覆盖全部启用线路，新增／订阅补充的线路自动纳入；每轮按预算挑最久未检测的线路，周期内全部覆盖。近 30 分钟有同模型业务成功的线路自动免测。</p>
              <div className="mb-2 text-xs font-medium">排除名单（勾选后不参与巡检）</div>
              <div className="grid max-h-48 gap-2 overflow-auto">{profiles.map((p) => <label key={p.id} className="flex items-center gap-2 text-xs"><input type="checkbox" checked={excludedIDs.includes(p.id)} disabled={!p.enabled} onChange={(e) => edit({ exclude_proxy_ids: toggle(excludedIDs, p.id, e.target.checked) })} />排除：{p.name}{!p.enabled && '（已停用）'}</label>)}</div>
            </> : <>
              <div className="mb-2 flex gap-3"><button type="button" className="text-xs underline" onClick={() => edit({ target_proxy_ids: profiles.filter((p) => p.enabled).map((p) => p.id) })}>选择全部启用线路</button><button type="button" className="text-xs underline" onClick={() => edit({ target_proxy_ids: [] })}>清空选择</button></div>
              <div className="grid max-h-48 gap-2 overflow-auto">{visibleIDs.map((id) => <label key={id} className="flex items-center gap-2 text-xs"><input type="checkbox" checked={value.target_proxy_ids.includes(id)} disabled={!value.target_proxy_ids.includes(id) && !profiles.find((p) => p.id === id)?.enabled} onChange={(e) => edit({ target_proxy_ids: toggle(value.target_proxy_ids, id, e.target.checked) })} />目标：{names.get(id) ?? '已删除 ' + id}</label>)}</div>
            </>}</fieldset>

            <fieldset className="rounded-lg border p-3"><legend className="px-1 text-sm font-medium">参考线路</legend><p className="mb-2 text-xs text-muted-foreground">留空时自动从其余启用线路中挑选独立参考（别名与已声明同出口组不互证）；手工指定后只用所选线路。自动退役仍要求目标与参考都有声明且属不同出口组，否则该目标只观察。</p><div className="mb-2 flex gap-3"><button type="button" className="text-xs underline" onClick={() => edit({ reference_proxy_ids: profiles.filter((p) => p.enabled).map((p) => p.id) })}>选择全部启用线路</button><button type="button" className="text-xs underline" onClick={() => edit({ reference_proxy_ids: [] })}>清空选择</button></div><div className="grid max-h-48 gap-2 overflow-auto">{visibleIDs.map((id) => <label key={id} className="flex items-center gap-2 text-xs"><input type="checkbox" checked={value.reference_proxy_ids.includes(id)} disabled={!value.reference_proxy_ids.includes(id) && !profiles.find((p) => p.id === id)?.enabled} onChange={(e) => edit({ reference_proxy_ids: toggle(value.reference_proxy_ids, id, e.target.checked) })} />参考：{names.get(id) ?? '已删除 ' + id}</label>)}</div></fieldset>

            <label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={value.allow_direct_control} onChange={(e) => edit({ allow_direct_control: e.target.checked })} />允许探测账号使用直连作为参考</label>
          </div>}
        </div>

        <div className="flex flex-wrap items-center justify-between gap-3 border-t pt-3"><p className="max-w-xl text-xs text-muted-foreground">模型固定为 glm-5.3-flash，最小回复上限 16 token。正常业务绑定不被探测改写，固定节点失效仍等待，不退回取额池；自动择优账号的额度消耗与 405 冷却按现有账号规则生效。出口组在「线路拓扑」中按线路声明。</p><Button type="submit" disabled={saving}>{saving && <Loader2 className="animate-spin motion-reduce:animate-none" />}保存模型巡检配置</Button></div>
      </form>
    </SheetContent>
  </Sheet>
}
