import { useState } from 'react'
import { ChevronRight, Loader2 } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from '@/components/ui/sheet'
import { accountReasons, numberFields, selectClass, toggle } from '@/components/health/labels'
import type { ModelProbeSettings, ModelProbeSummary, ProxyProfile } from '@/lib/types'

type Draft = ReturnType<typeof import('@/hooks/use-draft-group').useDraftGroup<ModelProbeSettings>>

/** 周期巡检只配置意图；范围与账号选择属于调度器，不把算法交给管理员。 */
export function HealthSettingsSheet({ open, onOpenChange, data, profiles, value, edit, saving, onSubmit, onTopology }: {
  open: boolean
  onOpenChange: (open: boolean) => void
  data: ModelProbeSummary
  profiles: ProxyProfile[]
  value: ModelProbeSettings
  edit: Draft['edit']
  saving: boolean
  onSubmit: () => void
  onTopology: () => void
}) {
  const [advanced, setAdvanced] = useState(false)
  const excluded = value.exclude_proxy_ids ?? []
  const legacy = value.target_mode !== 'auto' || value.account_mode !== 'auto' || value.account_ids.length > 0 || value.reference_proxy_ids.length > 0
  const targets = profiles.filter((p) => p.enabled && !excluded.includes(p.id)).length
  return <Sheet open={open} onOpenChange={onOpenChange}>
    <SheetContent className="w-full overflow-y-auto sm:max-w-2xl">
      <SheetHeader>
        <SheetTitle>健康巡检配置</SheetTitle>
        <SheetDescription>全部启用节点自动纳入，账号按近期可靠性自动选择。保存后生效，无需重启或逐项勾选。</SheetDescription>
      </SheetHeader>
      <form aria-label="模型巡检配置" className="space-y-4 px-4 pb-6" onSubmit={(e) => { e.preventDefault(); onSubmit() }}>
        <div className="space-y-1 rounded-lg border bg-muted/30 p-3 text-sm">
          <div className="font-medium">自动覆盖 {targets} 条启用线路 · 自动选择合格账号</div>
          <p className="text-xs text-muted-foreground">新增节点自动加入；账号留在业务池，探测不改持久绑定。近期业务成功单独作为旁证，不替代主动检查。</p>
          {legacy && <p className="text-xs text-amber-700">检测到旧手工策略，保存后统一采用自动覆盖、自动选号与自动参考；旧探测专用角色将解除。</p>}
        </div>
        <fieldset className="space-y-3 rounded-lg border p-3"><legend className="px-1 text-sm font-medium">基础网络巡检</legend>
          <label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={value.network_enabled ?? false} onChange={(e) => edit({ network_enabled: e.target.checked })} />启用基础网络巡检（不需要账号）</label>
          <div className="grid gap-4 sm:grid-cols-2">
            <div className="space-y-1.5"><Label htmlFor="network-interval">网络巡检间隔（分钟）</Label><Input id="network-interval" type="number" min={1} max={1440} required value={value.network_interval_minutes ?? 30} onChange={(e) => edit({ network_interval_minutes: Number(e.target.value) })} /></div>
            <div className="space-y-1.5"><Label htmlFor="network-confirm">网络复核等待（秒）</Label><Input id="network-confirm" type="number" min={1} max={300} required value={value.network_confirm_seconds ?? 10} onChange={(e) => edit({ network_confirm_seconds: Number(e.target.value) })} /></div>
          </div>
          <p className="text-xs text-muted-foreground">首次异常不删除，再次异常且参考正常才按处置模式处理。缺少模型账号不影响网络维护。</p>
        </fieldset>
        <label className="flex items-center gap-2 text-sm font-medium"><input type="checkbox" checked={value.enabled} onChange={(e) => edit({ enabled: e.target.checked })} />启用模型巡检</label>
        <div className="grid gap-4 sm:grid-cols-2">
          <div className="space-y-1.5"><Label htmlFor="model-probe-mode">处置模式</Label><select id="model-probe-mode" className={selectClass} value={value.mode} onChange={(e) => edit({ mode: e.target.value as ModelProbeSettings['mode'] })}><option value="observe">仅观察，不改变业务路由</option><option value="auto_retire">证据达标后严格退役</option></select></div>
          <div className="space-y-1.5"><Label htmlFor="probe-interval_minutes">模型巡检间隔（分钟）</Label><Input id="probe-interval_minutes" type="number" min={1} max={1440} required value={value.interval_minutes} onChange={(e) => edit({ interval_minutes: Number(e.target.value) })} /></div>
        </div>
        <p className="text-xs text-muted-foreground">按间隔分批推进全部节点，预算不足时完整轮转会延长；未发送不算完成，资源恢复后自动继续。模型固定为 glm-5.3-flash，最多回复 16 token。</p>
        <section aria-label="账号自动择优依据" className="space-y-2 rounded-lg border p-3">
          <div className="text-sm font-medium">账号自动择优依据</div>
          <p className="text-xs text-muted-foreground">先排除额度、冷却、托管、预算和在途限制，再按近期可靠性择优；业务与主动探测分开统计，结合样本量和新鲜度，不将线路故障一律扣到账号。</p>
          <div className="grid max-h-48 gap-2 overflow-auto">{(data.accounts ?? []).map((a) => <div key={a.id} className="flex flex-wrap items-center gap-2 text-xs">
            <span className={'shrink-0 rounded-md border px-1.5 py-0.5 tabular-nums ' + (a.eligible ? 'border-emerald-200 text-emerald-800' : 'text-muted-foreground')}>{a.eligible ? `#${a.rank ?? '-'}` : '暂不可用'}</span>
            <span className="max-w-40 truncate">{a.name}</span>
            <span className="text-muted-foreground">{a.eligible ? `业务成功 ${a.recent_success}/${a.recent_total} · 探测成功 ${a.probe_success ?? 0}/${a.probe_total ?? 0} · 剩余预算 ${a.remaining_requests ?? '—'}` : accountReasons[a.reason ?? ''] ?? a.reason ?? '不可用'}</span>
          </div>)}</div>
          {!(data.accounts ?? []).length && <p className="text-xs text-muted-foreground">暂无账号统计。系统会自动使用未托管、额度可用的 JWT 账号，不需要在这里指定。</p>}
        </section>
        <div className="rounded-lg border">
          <button type="button" className="flex w-full items-center gap-2 p-3 text-left text-sm font-medium" aria-expanded={advanced} onClick={() => setAdvanced(!advanced)}>
            <ChevronRight className={'size-4 transition-transform motion-reduce:transition-none ' + (advanced ? 'rotate-90' : '')} />高级选项
            <span className="ml-auto text-xs font-normal text-muted-foreground">预算、并发、巡检例外与拓扑</span>
          </button>
          {advanced && <div className="space-y-4 border-t p-3">
            <div className="grid gap-4 sm:grid-cols-2">{numberFields.filter((field) => field.key !== 'interval_minutes').map((field) => <div key={field.key} className="space-y-1.5"><Label htmlFor={'probe-' + field.key}>{field.label}</Label><Input id={'probe-' + field.key} type="number" min={field.min} max={field.max} required value={value[field.key]} onChange={(e) => edit({ [field.key]: Number(e.target.value) })} /></div>)}</div>
            <fieldset className="space-y-2 rounded-lg border p-3"><legend className="px-1 text-sm font-medium">模型目标排除（可选）</legend>
              <p className="text-xs text-muted-foreground">不勾选即全部参与。这里只排除模型目标，不停止网络维护，也不禁止作为参考；停用节点始终不参与。</p>
              <div className="grid max-h-48 gap-2 overflow-auto">{profiles.map((p) => <label key={p.id} className="flex items-center gap-2 text-xs"><input type="checkbox" checked={excluded.includes(p.id)} disabled={!p.enabled} onChange={(e) => edit({ exclude_proxy_ids: toggle(excluded, p.id, e.target.checked) })} />排除：{p.name}{!p.enabled && '（已停用）'}</label>)}</div>
            </fieldset>
            <div className="space-y-2 text-xs text-muted-foreground"><p>参考线路自动选择。严格退役仍要求不同出口组与完整对照证据；未声明拓扑只能观察，不会误删。</p><Button type="button" variant="outline" size="sm" onClick={onTopology}>声明线路拓扑</Button></div>
            <label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={value.allow_direct_control} onChange={(e) => edit({ allow_direct_control: e.target.checked })} />允许探测账号使用直连作为参考</label>
          </div>}
        </div>
        <div className="flex flex-wrap items-center justify-between gap-3 border-t pt-3"><p className="max-w-xl text-xs text-muted-foreground">探测会消耗所选账号额度，405 冷却仍按现有规则生效。固定出口托管账号不被借用，不改绑、不自动直连。</p><Button type="submit" disabled={saving}>{saving && <Loader2 className="animate-spin motion-reduce:animate-none" />}保存健康巡检配置</Button></div>
      </form>
    </SheetContent>
  </Sheet>
}
