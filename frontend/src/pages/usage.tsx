/* 用量分析頁：指標卡、Token 趨勢、帳號調度分布圓環、帳號用量排行（輪詢 15 秒） */
import { ChartColumnBig, CircleAlert, Coins, Users } from 'lucide-react'
import { useQuery } from '@tanstack/react-query'
import { Empty, MetricCard, PanelCard } from '@/components/panel'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { api } from '@/lib/api'
import { fmt, fmtCompact } from '@/lib/format'
import type { UsageResponse } from '@/lib/types'

const PALETTE = ['#3b82f6', '#10b981', '#8b5cf6', '#f59e0b', '#94a3b8']

export function UsagePage() {
  const { data, isFetching, isError, refetch } = useQuery({
    queryKey: ['usage'],
    queryFn: () => api<UsageResponse>('GET', '/usage'),
    refetchInterval: 15000,
  })

  const s = data?.summary
  const ranking = data?.ranking ?? []
  const calls = Number(s?.calls) || 0
  const input = Number(s?.tokens_in) || 0
  const output = Number(s?.tokens_out) || 0
  const cache = Number(s?.tokens_cache) || 0
  const total = input + output + cache
  const successRate = calls ? ((calls - (Number(s?.fail) || 0)) / calls) * 100 : null

  return (
    <div className="mx-auto flex w-full max-w-6xl flex-col gap-6">
      {/* 頁首 */}
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">用量分析</h1>
          <p className="text-sm text-muted-foreground">查看模型、Token 與帳號調度的累計使用狀況</p>
        </div>
        <div className="flex items-center gap-2">
          <span
            className="mr-1 flex items-center gap-1.5 text-xs text-muted-foreground"
            title={data ? '更新於 ' + new Date(data.ts * 1000).toLocaleTimeString('zh-TW') : undefined}
          >
            <span className={isError ? 'size-1.5 rounded-full bg-red-500' : 'size-1.5 animate-pulse rounded-full bg-emerald-500'} />
            {isError ? '載入失敗' : '即時資料'}
          </span>
          <Button variant="outline" size="sm" onClick={() => void refetch()} disabled={isFetching}>
            重新整理
          </Button>
        </div>
      </div>

      {/* 指標卡 */}
      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <MetricCard icon={<ChartColumnBig />} tone="text-blue-600" label="累計請求" value={fmt(calls)} detail={`成功 ${successRate != null ? successRate.toFixed(1) + '%' : '--'}`} />
        <MetricCard icon={<Coins />} tone="text-cyan-600" label="累計 Token" value={fmtCompact(total)} detail={`輸入 ${fmtCompact(input)} · 輸出 ${fmtCompact(output)}`} />
        <MetricCard icon={<Users />} tone="text-violet-600" label="活躍帳號" value={fmt(s?.active)} detail={`總帳號 ${fmt(s?.total)}`} />
        <MetricCard icon={<CircleAlert />} tone="text-amber-600" label="失敗請求" value={fmt(s?.fail)} detail="目前累計" />
      </div>

      {/* Token 趨勢＋調度分布 */}
      <div className="grid gap-4 lg:grid-cols-5">
        <PanelCard title="Token 使用趨勢" subtitle="依帳號累計調度量彙總" badge="累計資料" className="lg:col-span-3">
          {total ? (
            <>
              <div className="flex flex-col gap-3">
                {(
                  [
                    ['輸入', input, '#3b82f6'],
                    ['輸出', output, '#10b981'],
                    ['快取', cache, '#8b5cf6'],
                  ] as [string, number, string][]
                ).map(([label, value, color]) => {
                  const max = Math.max(input, output, cache, 1)
                  const pct = total ? (value / total) * 100 : 0
                  return (
                    <div key={label} className="flex items-center gap-3">
                      <span className="w-10 shrink-0 text-sm">{label}</span>
                      <strong className="w-16 shrink-0 text-right text-sm tabular-nums">{fmtCompact(value)}</strong>
                      <span className="h-2.5 flex-1 overflow-hidden rounded-full bg-muted">
                        <i className="block h-full min-w-0.5 rounded-full" style={{ width: `${Math.max(2, (value / max) * 100)}%`, background: color }} />
                      </span>
                      <small className="w-12 shrink-0 text-right text-xs tabular-nums text-muted-foreground">{pct.toFixed(1)}%</small>
                    </div>
                  )
                })}
              </div>
              {/* 裝飾性走勢線（沿用舊版示意曲線） */}
              <div className="mt-4 h-20 text-emerald-500/70 [&_svg]:h-full [&_svg]:w-full" aria-hidden="true">
                <svg viewBox="0 0 600 120" preserveAspectRatio="none">
                  <path d="M0 94 C55 72 78 83 126 60 S205 80 252 45 S336 77 386 38 S458 66 516 24 S566 45 600 18" fill="none" stroke="currentColor" strokeWidth="3" />
                  <path d="M0 110 C70 100 110 104 162 91 S254 103 304 74 S389 95 436 71 S530 84 600 57" fill="none" stroke="currentColor" strokeWidth="2" opacity="0.55" />
                </svg>
              </div>
              <div className="mt-2 flex gap-4 text-xs text-muted-foreground">
                <span className="flex items-center gap-1.5"><i className="size-2 rounded-full bg-blue-500" />輸入</span>
                <span className="flex items-center gap-1.5"><i className="size-2 rounded-full bg-emerald-500" />輸出</span>
                <span className="flex items-center gap-1.5"><i className="size-2 rounded-full bg-violet-500" />快取</span>
              </div>
            </>
          ) : (
            <Empty>尚無 Token 調度資料</Empty>
          )}
        </PanelCard>

        <PanelCard title="帳號調度分布" subtitle="依請求數排序" className="lg:col-span-2">
          <Donut ranking={ranking} calls={calls} />
        </PanelCard>
      </div>

      {/* 帳號用量排行 */}
      <PanelCard title="帳號用量排行" subtitle="目前服務程序啟動後的累計調度" badge={fmt(calls)}>
        <Card className="overflow-x-auto py-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>帳號</TableHead>
                <TableHead>提供商</TableHead>
                <TableHead className="text-right">請求</TableHead>
                <TableHead className="text-right">失敗</TableHead>
                <TableHead className="text-right">Token</TableHead>
                <TableHead className="w-40">佔比</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {ranking.length ? (
                ranking.map((r) => {
                  const pct = calls ? (r.requests / calls) * 100 : 0
                  return (
                    <TableRow key={r.name}>
                      <TableCell className="font-medium">{r.name}</TableCell>
                      <TableCell>
                        <span className="rounded-full bg-emerald-100 px-2 py-0.5 text-xs text-emerald-700">{r.provider}</span>
                      </TableCell>
                      <TableCell className="text-right tabular-nums">{fmt(r.requests)}</TableCell>
                      <TableCell className="text-right tabular-nums">{fmt(r.errors)}</TableCell>
                      <TableCell className="text-right tabular-nums">{fmtCompact(r.tokens)}</TableCell>
                      <TableCell>
                        <div className="flex items-center gap-2">
                          <span className="h-1.5 flex-1 overflow-hidden rounded-full bg-muted">
                            <i className="block h-full rounded-full bg-primary" style={{ width: `${pct}%` }} />
                          </span>
                          <small className="w-11 shrink-0 text-right text-xs tabular-nums text-muted-foreground">{pct.toFixed(1)}%</small>
                        </div>
                      </TableCell>
                    </TableRow>
                  )
                })
              ) : (
                <TableRow>
                  <TableCell colSpan={6} className="py-8 text-center text-sm text-muted-foreground">
                    尚無帳號用量資料
                  </TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
        </Card>
      </PanelCard>
    </div>
  )
}

/* 調度分布圓環：前五名帳號占比 */
function Donut({ ranking, calls }: { ranking: UsageResponse['ranking']; calls: number }) {
  const top = ranking.slice(0, 5)
  let cursor = 0
  const stops = top.map((r, i) => {
    const pct = calls ? (Number(r.requests) / calls) * 100 : 0
    const seg = `${PALETTE[i]} ${cursor}% ${cursor + pct}%`
    cursor += pct
    return seg
  })
  stops.push(`#e9edf3 ${cursor}% 100%`)
  return (
    <div className="flex items-center gap-6">
      <div
        className="relative flex size-32 shrink-0 items-center justify-center rounded-full"
        style={{ background: `conic-gradient(${stops.join(',')})` }}
      >
        <div className="flex size-[86px] flex-col items-center justify-center rounded-full bg-card">
          <strong className="text-xl tabular-nums">{fmt(calls)}</strong>
          <span className="text-xs text-muted-foreground">請求</span>
        </div>
      </div>
      <div className="flex min-w-0 flex-1 flex-col gap-2 text-sm">
        {top.length ? (
          top.map((r, i) => (
            <div key={r.name} className="flex items-center gap-2">
              <span className="size-2 shrink-0 rounded-full" style={{ background: PALETTE[i] }} />
              <span className="min-w-0 flex-1 truncate text-muted-foreground">{r.name}</span>
              <strong className="shrink-0 tabular-nums">{calls ? ((r.requests / calls) * 100).toFixed(1) : '0'}%</strong>
            </div>
          ))
        ) : (
          <Empty>尚無用量資料</Empty>
        )}
      </div>
    </div>
  )
}
