import { useState, type FormEvent } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { ArrowRight, Loader2 } from 'lucide-react'
import { toast } from 'sonner'
import { PanelCard } from '@/components/panel'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { useDraftGroup } from '@/hooks/use-draft-group'
import { api, errMsg } from '@/lib/api'
import type { NewAccountProxyPolicy, ProxyProfile } from '@/lib/types'

type Fields = Omit<NewAccountProxyPolicy, 'revision'>
const defaults: Fields = { enabled: false, default_proxy_id: '', pool_mode: 'all_enabled', proxy_ids: [], max_attempts: 32, attempt_timeout_seconds: 120, round_budget_seconds: 900 }
const selectClass = 'h-9 w-full rounded-md border border-input bg-background px-3 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring'

function fields(p: NewAccountProxyPolicy): Fields {
  return { enabled: p.enabled, default_proxy_id: p.default_proxy_id, pool_mode: p.pool_mode, proxy_ids: p.proxy_ids ?? [], max_attempts: p.max_attempts, attempt_timeout_seconds: p.attempt_timeout_seconds, round_budget_seconds: p.round_budget_seconds }
}

export function NewAccountPolicy({ profiles }: { profiles: ProxyProfile[] }) {
  const qc = useQueryClient()
  const { data, error } = useQuery({ queryKey: ['new-account-policy'], queryFn: () => api<NewAccountProxyPolicy>('GET', '/proxies/new-account-policy') })
  const draft = useDraftGroup<Fields>(data ? fields(data) : undefined, defaults)
  const value = draft.value
  const [saving, setSaving] = useState(false)
  const target = profiles.find((p) => p.id === value.default_proxy_id)
  const candidates = profiles.filter((p) => p.enabled && p.id !== value.default_proxy_id)

  async function save(event: FormEvent) {
    event.preventDefault()
    if (!data) return
    const version = draft.beginSave()
    setSaving(true)
    try {
      const saved = await api<NewAccountProxyPolicy>('PUT', '/proxies/new-account-policy', { ...value, revision: data.revision })
      qc.setQueryData(['new-account-policy'], saved)
      draft.accept(version)
      void qc.invalidateQueries({ queryKey: ['accounts'] })
      toast.success('初始化配置已保存，不会自动迁移已有账号')
    } catch (e) { toast.error('保存初始化配置失败：' + errMsg(e)) }
    finally { setSaving(false) }
  }

  return <PanelCard title="新账号初始化" subtitle="临时取额，到账后固定出口；不影响已有账号">
    {!data ? <p className="text-sm text-muted-foreground" role={error ? 'alert' : 'status'}>{error ? '读取初始化配置失败：' + errMsg(error) : '正在读取初始化配置…'}</p> :
      <form className="space-y-5" onSubmit={(event) => void save(event)}>
        <label className="flex items-center gap-2 text-sm font-medium"><input type="checkbox" checked={value.enabled} onChange={(e) => draft.edit({ enabled: e.target.checked })} />启用新账号初始化</label>
        <div className="flex flex-wrap items-center gap-2 rounded-lg border bg-muted/30 px-3 py-2 text-xs text-muted-foreground"><span>取额代理池</span><ArrowRight className="size-3" /><span>确认可用额度</span><ArrowRight className="size-3" /><span className="font-medium text-foreground">{target?.name ?? '默认业务代理'}</span></div>
        <div className="grid gap-4 md:grid-cols-2">
          <div className="space-y-2">
            <Label htmlFor="initialization-default">默认业务代理</Label>
            <select id="initialization-default" className={selectClass} value={value.default_proxy_id} onChange={(e) => draft.edit({ default_proxy_id: e.target.value, proxy_ids: value.proxy_ids.filter((id) => id !== e.target.value) })}>
              <option value="">请选择已添加的代理节点</option>
              {value.default_proxy_id && !target && <option value={value.default_proxy_id}>原默认代理已删除，请重新选择</option>}
              {profiles.map((p) => <option key={p.id} value={p.id} disabled={!p.enabled}>{p.name}{p.enabled ? '' : '（已停用）'}</option>)}
            </select>
            <p className="text-xs text-muted-foreground">在这里选择你的本地服务器节点。更改默认节点不会自动迁移已有账号。</p>
            {value.enabled && (!target || !target.enabled) && <p role="alert" className="text-xs text-destructive">默认业务代理不可用，请添加或启用节点后再保存。</p>}
          </div>
          <div className="space-y-2">
            <Label htmlFor="initialization-pool">取额代理池</Label>
            <select id="initialization-pool" className={selectClass} value={value.pool_mode} onChange={(e) => draft.edit({ pool_mode: e.target.value as Fields['pool_mode'] })}><option value="all_enabled">全部启用节点（排除默认业务代理）</option><option value="selected">手动选择节点</option></select>
            <p className="text-xs text-muted-foreground">同一路由别名会去重，停用、退役与默认业务出口不参与取额。</p>
          </div>
        </div>
        {value.pool_mode === 'selected' && <fieldset className="grid max-h-48 gap-2 overflow-auto rounded-md border p-3 sm:grid-cols-2"><legend className="px-1 text-xs text-muted-foreground">选择取额节点</legend>{candidates.length ? candidates.map((p) => <label key={p.id} className="flex min-w-0 items-center gap-2 text-sm"><input type="checkbox" checked={value.proxy_ids.includes(p.id)} onChange={(e) => draft.edit({ proxy_ids: e.target.checked ? [...value.proxy_ids, p.id] : value.proxy_ids.filter((id) => id !== p.id) })} /><span className="truncate" title={p.name}>{p.name}</span></label>) : <p className="text-xs text-muted-foreground">请先添加默认代理以外的取额节点。</p>}</fieldset>}
        <div className="grid gap-4 sm:grid-cols-3">
          <div className="space-y-2"><Label htmlFor="initialization-attempts">每轮最多节点数</Label><Input id="initialization-attempts" type="number" min={1} max={1000} required value={value.max_attempts} onChange={(e) => draft.edit({ max_attempts: Number(e.target.value) })} /></div>
          <div className="space-y-2"><Label htmlFor="initialization-timeout">单节点时限（秒）</Label><Input id="initialization-timeout" type="number" min={10} max={300} required value={value.attempt_timeout_seconds} onChange={(e) => draft.edit({ attempt_timeout_seconds: Number(e.target.value) })} /></div>
          <div className="space-y-2"><Label htmlFor="initialization-budget">整轮时限（秒）</Label><Input id="initialization-budget" type="number" min={60} max={3600} required value={value.round_budget_seconds} onChange={(e) => draft.edit({ round_budget_seconds: Number(e.target.value) })} /></div>
        </div>
        <div className="flex flex-wrap items-center justify-between gap-3 border-t pt-4"><p className="max-w-2xl text-xs text-muted-foreground">仅作用于自动代理模式的新入库 JWT 账号，且需开启设置页的“入池自动领取”。一轮耗尽后等待手动重试；固定节点失效时不会自动直连。</p><Button type="submit" disabled={saving}>{saving && <Loader2 className="animate-spin" />}保存初始化配置</Button></div>
      </form>}
  </PanelCard>
}
