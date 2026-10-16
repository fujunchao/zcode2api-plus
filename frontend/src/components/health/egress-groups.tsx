import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from '@/components/ui/sheet'
import type { ModelProbeSettings, ProxyProfile } from '@/lib/types'

/**
 * 出口组声明：同一家庭公网出口的不同端口填同一个组。
 *
 * 独立于巡检配置——它是**一次性拓扑声明**，此前为每条线路生成一个输入框，
 * 使巡检表单长度随线路数量增长。自动退役要求目标与参考属不同出口组。
 *
 * ⚠️ 这是使用者的配置声明，不是服务器探测到的出口 IP；文案不得暗示系统已验证出口。
 */
export function EgressGroupsSheet({ open, onOpenChange, value, edit, profiles, saving, onSave }: {
  open: boolean
  onOpenChange: (open: boolean) => void
  value: ModelProbeSettings
  edit: (patch: Partial<ModelProbeSettings>) => void
  profiles: ProxyProfile[]
  saving: boolean
  onSave: () => void
}) {
  const names = new Map(profiles.map((p) => [p.id, p.name]))
  const ids = [...new Set([...profiles.map((p) => p.id), ...value.reference_proxy_ids, ...(value.allow_direct_control ? ['direct'] : [])])]
  return <Sheet open={open} onOpenChange={onOpenChange}>
    <SheetContent className="w-full overflow-y-auto sm:max-w-xl">
      <SheetHeader>
        <SheetTitle>线路拓扑 · 出口组</SheetTitle>
        <SheetDescription>同一个家庭公网出口的不同端口填同一个组。自动退役必须有明确不同组的参考；这是你的配置声明，不是服务器自动确认的出口 IP。</SheetDescription>
      </SheetHeader>
      <div className="space-y-4 px-4 pb-6">
        <div className="grid gap-3 sm:grid-cols-2">{ids.map((id) => <div key={id} className="space-y-1.5"><Label htmlFor={'probe-group-' + id}>出口组：{id === 'direct' ? '直连' : names.get(id) ?? id}</Label><Input id={'probe-group-' + id} maxLength={64} value={value.egress_groups[id] ?? ''} onChange={(e) => edit({ egress_groups: { ...value.egress_groups, [id]: e.target.value } })} placeholder="例如：家庭宽带 A" /></div>)}</div>
        {ids.length === 0 && <p className="text-sm text-muted-foreground">还没有线路可声明出口组。</p>}
        <div className="flex justify-end border-t pt-3"><Button type="button" disabled={saving} onClick={onSave}>保存出口组声明</Button></div>
      </div>
    </SheetContent>
  </Sheet>
}
