import { Activity, Settings2 } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { effectiveInterval, healthStatus, levelDot, targetCount, time } from '@/components/health/labels'
import type { ModelProbeSummary } from '@/lib/types'

/**
 * 摘要行：常驻一行，只回答「现在有没有问题」。
 * 正常时不给指标以外的任何控件；详情与配置都走抽屉，避免日常使用者绕过配置区才能看到状态。
 */
export function HealthSummaryCard({ data, error, loading, onDetail, onSettings }: {
  data?: ModelProbeSummary
  error?: unknown
  loading: boolean
  onDetail: () => void
  onSettings: () => void
}) {
  const actions = <div className="flex shrink-0 gap-2">
    <Button variant="outline" size="sm" onClick={onDetail} disabled={!data}>详情</Button>
    <Button variant="outline" size="sm" onClick={onSettings} aria-expanded={false}><Settings2 />配置健康巡检</Button>
  </div>

  if (error) {
    return <section aria-label="代理健康巡检" className="flex flex-wrap items-center justify-between gap-3 rounded-xl border bg-card p-4">
      <p role="alert" className="text-sm text-destructive">读取巡检失败</p>
      {actions}
    </section>
  }
  if (loading || !data) {
    return <section aria-label="代理健康巡检" className="flex flex-wrap items-center justify-between gap-3 rounded-xl border bg-card p-4">
      <p role="status" className="text-sm text-muted-foreground">正在读取巡检配置…</p>
      {actions}
    </section>
  }

  const status = healthStatus(data)
  const enabled = data.settings.enabled || (data.settings.network_enabled ?? false)
  // 下次巡检时刻取所有目标里最早的一个；无目标时留空。
  const nextAt = Object.values(data.next_due ?? {}).filter((v) => v > 0).sort((a, b) => a - b)[0]
  return <section aria-label="代理健康巡检" className="flex flex-wrap items-center justify-between gap-3 rounded-xl border bg-card p-4">
    <div className="flex min-w-0 flex-wrap items-center gap-x-4 gap-y-1">
      <span className="flex items-center gap-2 text-sm font-semibold">
        <span aria-hidden className={'size-2 shrink-0 rounded-full ' + levelDot[status.level]} />
        代理健康巡检
      </span>
      <span className="flex items-center gap-1.5 text-sm">
        <Activity className="size-3.5 text-muted-foreground" />
        {status.text}
      </span>
      {enabled && <>
        {data.settings.enabled ? <>
          <span className="text-xs text-muted-foreground">主动覆盖 {data.checked_targets ?? 0}/{targetCount(data)} · 参考轮转 {effectiveInterval(data)} 分钟</span>
          <span className="font-mono text-xs tabular-nums text-muted-foreground">预算 {data.used_requests}/{data.settings.global_daily_limit}</span>
        </> : <span className="text-xs text-muted-foreground">网络覆盖 {data.network?.checked ?? 0}/{data.network?.total ?? 0}</span>}
        {nextAt ? <span className="text-xs text-muted-foreground">下次 {time(nextAt)}</span> : null}
      </>}
    </div>
    {actions}
  </section>
}
