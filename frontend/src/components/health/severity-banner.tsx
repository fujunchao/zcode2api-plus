import { AlertTriangle, Info, XCircle } from 'lucide-react'
import { useState } from 'react'
import type { ModelProbeSummary } from '@/lib/types'
import { time } from '@/components/health/labels'

export type Severity = 'error' | 'warning' | 'info'

/* 三级视觉：error 用 destructive、warning 用琥珀、info 用中性，替代此前四条同等样式的提示。 */
const styles: Record<Severity, string> = {
  error: 'border-destructive/40 bg-destructive/5 text-destructive',
  warning: 'border-amber-300 bg-amber-50 text-amber-800',
  info: 'border-border bg-muted/40 text-muted-foreground',
}
const icons = { error: XCircle, warning: AlertTriangle, info: Info }

export function SeverityBanner({ severity, children, onDismiss }: { severity: Severity; children: React.ReactNode; onDismiss?: () => void }) {
  const Icon = icons[severity]
  return <div role={severity === 'error' ? 'alert' : 'status'} className={'flex items-start gap-2 rounded-lg border p-3 text-sm ' + styles[severity]}>
    <Icon className="mt-0.5 size-4 shrink-0" />
    <div className="min-w-0 flex-1">{children}</div>
    {onDismiss && <button type="button" onClick={onDismiss} aria-label="关闭提示" className="shrink-0 text-xs underline">关闭</button>}
  </div>
}

/**
 * 巡检的异常与提示：按严重性分层，取代此前四条同权重平铺的段落。
 * 一次性迁移提示可关闭，按 revision 记录，配置再变化后会重新提示。
 */
export function HealthNotices({ data }: { data: ModelProbeSummary }) {
  const notice = data.settings.migration_notice
  const key = 'health-panel:migration-notice:' + data.settings.revision
  const [dismissed, setDismissed] = useState(() => {
    try { return localStorage.getItem(key) === '1' } catch { return false }
  })
  const now = Date.now()
  return <>
    {data.storage_error && <SeverityBanner severity="error">巡检结果保存失败，已暂停新探测。修复存储后重新保存配置或重启；旧请求不会自动重放。</SeverityBanner>}
    {data.paused_until > now && <SeverityBanner severity="warning">参考组不可用，主动对照暂停至 {time(data.paused_until)}；不会因此清空代理池。</SeverityBanner>}
    {(data.evidence_incomplete_until ?? 0) > now && <SeverityBanner severity="warning">短窗口证据因重启或容量裁剪不完整，新的严格删除暂停至 {time(data.evidence_incomplete_until)}；已有禁止记录不受影响。</SeverityBanner>}
    {notice && !dismissed && <SeverityBanner severity="info" onDismiss={() => {
      try { localStorage.setItem(key, '1') } catch { /* 隐私模式下不可写：仅本次会话生效 */ }
      setDismissed(true)
    }}>{notice}</SeverityBanner>}
  </>
}
