import { useState } from 'react'
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from '@/components/ui/sheet'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { HealthNotices } from '@/components/health/severity-banner'
import { NetworkTable } from '@/components/health/network-table'
import { ModelTable } from '@/components/health/model-table'
import { HistoryList } from '@/components/health/history-list'
import { effectiveInterval, targetCount } from '@/components/health/labels'
import type { ModelProbeSummary, ProxyProfile } from '@/lib/types'

/** 详情抽屉：网络／模型／记录分标签，异常计数直接标在标签上，不用切换即可看出哪层有问题。 */
export function HealthDetailSheet({ open, onOpenChange, data, profiles, triggering, onCheck, historyTarget, onHistory }: {
  open: boolean
  onOpenChange: (open: boolean) => void
  data: ModelProbeSummary
  profiles: ProxyProfile[]
  triggering: string
  onCheck: (id: string) => void
  historyTarget: string
  onHistory: (id: string) => void
}) {
  const [expanded, setExpanded] = useState('')
  const suspects = data.runs.filter((r) => r.verdict === 'suspect').length
  const observed = data.observed_targets ?? 0
  const total = targetCount(data)
  return <Sheet open={open} onOpenChange={onOpenChange}>
    <SheetContent className="w-full overflow-y-auto sm:max-w-2xl">
      <SheetHeader>
        <SheetTitle>代理健康巡检详情</SheetTitle>
        <SheetDescription>
          统一调度：先检查网络，再按账号资格检查 5.3f。{data.settings.mode === 'observe' ? '当前仅观察，两层均不自动移除。' : '当前自动处置：网络须复核、模型须严格对照。'}
        </SheetDescription>
      </SheetHeader>
      <div className="space-y-4 px-4 pb-6">
        <HealthNotices data={data} />
        <div className="flex flex-wrap items-center gap-x-4 gap-y-1 rounded-lg border bg-muted/30 px-3 py-2 text-xs">
          <span className="font-medium">{data.settings.enabled ? (data.settings.mode === 'auto_retire' ? '自动严格处置' : '仅观察，不删除') : '模型巡检未启用'}</span>
          <span>单线检测周期约 {effectiveInterval(data)} 分钟</span>
          <span className="font-mono tabular-nums">24h {data.used_requests}/{data.settings.global_daily_limit} 次</span>
          <span>剩余 {data.remaining_requests} 次 · 在途 {data.active}</span>
          <span>近 30 分钟已观测 {observed}/{total} 条{(data.excluded_targets ?? 0) > 0 ? ` · 排除 ${data.excluded_targets} 条` : ''}</span>
          {suspects > 0 && <span className="text-amber-700">{suspects} 条线路待核查</span>}
        </div>
        <Tabs defaultValue="model">
          <TabsList>
            <TabsTrigger value="network">网络可用性</TabsTrigger>
            <TabsTrigger value="model">模型可用性{suspects > 0 ? ` (${suspects})` : ''}</TabsTrigger>
            <TabsTrigger value="history">诊断记录</TabsTrigger>
          </TabsList>
          <TabsContent value="network" className="pt-4">
            <NetworkTable profiles={profiles} enabled={data.settings.network_enabled ?? false} />
          </TabsContent>
          <TabsContent value="model" className="pt-4">
            <ModelTable data={data} profiles={profiles} triggering={triggering} onCheck={onCheck} onHistory={onHistory} historyTarget={historyTarget} />
          </TabsContent>
          <TabsContent value="history" className="pt-4">
            <HistoryList target={historyTarget} profiles={profiles} expanded={expanded} onToggle={setExpanded} />
          </TabsContent>
        </Tabs>
      </div>
    </SheetContent>
  </Sheet>
}
