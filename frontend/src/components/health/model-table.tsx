import { Loader2 } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Empty } from '@/components/panel'
import { describe, labels, time } from '@/components/health/labels'
import type { ModelProbeSummary, ProxyProfile } from '@/lib/types'

/**
 * 模型可用性表：目标集合反映**已保存的策略**（自动模式列出全部启用且未排除的线路，
 * 手工模式列出所选），而非当前草稿——避免未保存的编辑改变运维看到的读数。
 */
export function ModelTable({ data, profiles, triggering, onCheck, onHistory, historyTarget }: {
  data: ModelProbeSummary
  profiles: ProxyProfile[]
  triggering: string
  onCheck: (id: string) => void
  onHistory: (id: string) => void
  historyTarget: string
}) {
  const names = new Map(profiles.map((p) => [p.id, p.name]))
  const savedMode = data.settings.target_mode ?? 'manual'
  const tableIDs = savedMode === 'auto'
    ? profiles.filter((p) => p.enabled && !(data.settings.exclude_proxy_ids ?? []).includes(p.id)).map((p) => p.id)
    : data.settings.target_proxy_ids ?? []
  if (tableIDs.length === 0) {
    return <Empty>{savedMode === 'auto' ? '没有可巡检的启用线路：全部停用或在排除名单中。' : '尚未选择目标。可切换为自动覆盖全部启用线路。'}</Empty>
  }
  return <div className="overflow-x-auto"><table className="w-full text-left text-xs"><thead><tr className="border-b text-muted-foreground"><th className="pb-2 font-medium">目标线路</th><th className="pb-2 font-medium">模型结论</th><th className="pb-2 font-medium">最近记录／下一次</th><th className="pb-2 text-right font-medium">操作</th></tr></thead><tbody>
    {tableIDs.map((id) => {
      const run = data.runs.find((r) => r.target_id === id)
      const busy = !!run && ['queued', 'running', 'waiting'].includes(run.state)
      return <tr key={id} className="border-b last:border-0"><td className="py-3 pr-3 font-medium">{names.get(id) ?? '已删除线路 ' + id}</td><td className="py-3 pr-3"><div>{describe(run)}</div><div className="mt-1 max-w-72 text-muted-foreground">{run ? labels[run.reason] ?? run.reason : '网络连通不等于模型成功'}</div></td><td className="py-3 pr-3 tabular-nums"><div>主动：{time(data.last_checks?.[id])}</div><div className="mt-1 text-muted-foreground">下次：{time(busy ? run?.next_at : data.next_due[id])}</div></td><td className="py-3 text-right"><div className="flex justify-end gap-2"><Button size="sm" variant="outline" disabled={!data.settings.enabled || busy || !!triggering || !profiles.find((p) => p.id === id)?.enabled} onClick={() => onCheck(id)} aria-label={'立即模型检查：' + (names.get(id) ?? id)}>{triggering === id && <Loader2 className="animate-spin motion-reduce:animate-none" />}立即模型检查</Button><Button size="sm" variant="ghost" onClick={() => onHistory(historyTarget === id ? '' : id)}>记录</Button></div></td></tr>
    })}
  </tbody></table></div>
}
