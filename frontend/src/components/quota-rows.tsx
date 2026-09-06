/* 帳號池額度列：模型名＋套餐名＋進度條＋剩餘/總量（語義照搬舊版 quotaCell） */
import { fmtCompact, fmtDate } from '@/lib/format'
import type { Account } from '@/lib/types'

const COLOR_EMPTY = '#c9c9cf'
const COLOR_LOW = '#b0632a'
const COLOR_OK = '#4c9168'

export function QuotaRows({ account }: { account: Account }) {
  const quota = account.quota || {}
  const keys = Object.keys(quota)
  if (!keys.length) {
    return <span className="text-xs text-muted-foreground">{account.mode === 'jwt' ? '未取得' : '—'}</span>
  }
  /* 多套餐帳號每列自帶 plan_name；單套餐帳號後端留空，整體回退到帳號層的方案名 */
  const hasPlanNames = keys.some((k) => !!quota[k]?.plan_name)

  return (
    <div className="flex min-w-0 flex-col gap-1.5">
      {keys.map((k) => {
        const w = quota[k]
        const rem = Number(w.remaining) || 0
        const tot = Number(w.total) || 0
        const pct = tot > 0 ? Math.max(0, Math.min(100, Math.round((rem / tot) * 100))) : 0
        const color = rem <= 0 ? COLOR_EMPTY : pct < 15 ? COLOR_LOW : COLOR_OK
        const period = w.period === 'daily' ? '每日' : '當期'
        const reset = w.period_end ? ` · 重置 ${fmtDate(Number(w.period_end))}` : ''
        const model = w.model || k
        const planName = w.plan_name || (hasPlanNames ? '' : account.plan_name || '')
        return (
          <div key={k} className="flex items-center gap-2 text-xs" title={`${k} · ${period}配額${reset}`}>
            <span className="flex min-w-0 shrink-0 flex-col leading-tight">
              <span className="truncate font-medium">{model}</span>
              {planName ? <span className="truncate text-[11px] text-muted-foreground">{planName}</span> : null}
            </span>
            <span className="h-1.5 min-w-8 flex-1 overflow-hidden rounded-full bg-muted">
              <span className="block h-full rounded-full" style={{ width: `${pct}%`, background: color }} />
            </span>
            <span className="shrink-0 tabular-nums text-muted-foreground">
              {period} {fmtCompact(rem)} / {fmtCompact(tot)}
            </span>
          </div>
        )
      })}
    </div>
  )
}
