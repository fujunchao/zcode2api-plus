import { Card, CardContent } from '@/components/ui/card'
import { fmt, fmtCompact } from '@/lib/format'
import type { ModelQuotaSummary } from '@/lib/types'

/** 只展示后端汇总，不再把不同模型的额度相加；未知数值不绘制进度条。 */
export function ModelQuotaCards({ quotas }: { quotas: ModelQuotaSummary[] }) {
  return (
    <section aria-label="模型额度" className="flex flex-col gap-3">
      <div>
        <h2 className="text-sm font-semibold">模型额度</h2>
        <p className="text-xs text-muted-foreground">非归档账号的最近额度快照（含停用、冷却及失效账号），不等同于当前可调度额度。</p>
      </div>
      <div className="grid gap-3 md:grid-cols-2">
        {quotas.map((quota) => {
          const { model, total, used, remaining, partial } = quota
          const percent = !partial && total != null && total > 0 && remaining != null
            ? Math.max(0, Math.min(100, remaining / total * 100))
            : null
          return (
            <Card key={model} role="region" aria-label={`${model} 额度`}>
              <CardContent className="flex flex-col gap-4">
                <div className="flex flex-wrap items-center justify-between gap-2">
                  <h3 className="font-mono text-sm font-semibold">{model}</h3>
                  <span className="text-xs text-muted-foreground">{fmt(quota.accounts)} 个账号 · {fmt(quota.items)} 个额度项目</span>
                </div>
                <dl className="grid grid-cols-3 gap-3">
                  <QuotaValue label="剩余额度" value={remaining} prominent />
                  <QuotaValue label="总额度" value={total} />
                  <QuotaValue label="已用额度" value={used} />
                </dl>
                {percent != null && (
                  <div
                    role="progressbar"
                    aria-label={`${model} 剩余额度比例`}
                    aria-valuemin={0}
                    aria-valuemax={100}
                    aria-valuenow={Number(percent.toFixed(1))}
                    className="h-1.5 overflow-hidden rounded-full bg-muted"
                  >
                    <div className={`h-full rounded-full ${percent < 15 ? 'bg-amber-500' : 'bg-emerald-500'}`} style={{ width: `${percent}%` }} />
                  </div>
                )}
                {(quota.items === 0 || partial) && (
                  <p className="text-xs text-muted-foreground">
                    {quota.items === 0 ? '尚未获取该模型额度，请在账号池刷新额度。' : '部分额度数据缺失，仅汇总已知数值。'}
                  </p>
                )}
              </CardContent>
            </Card>
          )
        })}
        {!quotas.length && <p className="text-sm text-muted-foreground">尚未获取模型额度数据。</p>}
      </div>
    </section>
  )
}

function QuotaValue({ label, value, prominent = false }: { label: string; value: number | null; prominent?: boolean }) {
  return (
    <div className="min-w-0">
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className={`truncate tabular-nums ${prominent ? 'text-2xl font-semibold' : 'pt-1 text-lg font-medium'}`} title={value == null ? '未知' : fmt(value)}>
        {value == null ? '--' : fmtCompact(value)}
      </dd>
    </div>
  )
}
