/* 儀表板／用量分析共用的面板小卡與空狀態 */
import { Card, CardContent } from '@/components/ui/card'

/* 指標卡：圖示＋標籤＋數值＋補充說明 */
export function MetricCard({ icon, tone, label, value, detail }: { icon: React.ReactNode; tone: string; label: string; value: string; detail: string }) {
  return (
    <Card>
      <CardContent className="flex items-center gap-3">
        <span className={`flex size-9 shrink-0 items-center justify-center rounded-lg bg-muted [&>svg]:size-4.5 ${tone}`}>{icon}</span>
        <span className="min-w-0 leading-tight">
          <span className="block text-xs text-muted-foreground">{label}</span>
          <strong className="block truncate text-xl tabular-nums">{value}</strong>
          <small className="block truncate text-[11px] text-muted-foreground">{detail}</small>
        </span>
      </CardContent>
    </Card>
  )
}

/* 面板卡：標題＋副標＋右上角徽章／連結 */
export function PanelCard({
  title,
  subtitle,
  badge,
  className,
  children,
}: {
  title: string
  subtitle: string
  badge?: React.ReactNode
  className?: string
  children: React.ReactNode
}) {
  return (
    <Card className={className}>
      <CardContent className="flex flex-col gap-4">
        <div className="flex items-start justify-between gap-2">
          <div>
            <div className="text-sm font-semibold">{title}</div>
            <div className="text-xs text-muted-foreground">{subtitle}</div>
          </div>
          {typeof badge === 'string' ? <PanelBadge>{badge}</PanelBadge> : badge}
        </div>
        {children}
      </CardContent>
    </Card>
  )
}

export function PanelBadge({ children }: { children: React.ReactNode }) {
  return <span className="rounded-full bg-muted px-2.5 py-1 text-xs text-muted-foreground">{children}</span>
}

export function Empty({ children }: { children: React.ReactNode }) {
  return <div className="py-8 text-center text-sm text-muted-foreground">{children}</div>
}
