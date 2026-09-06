/* 運維監控頁：健康分圓環、KPI、請求品質、主機資源、服務元件、運行資訊（輪詢 10 秒；版本號取自 /meta） */
import { Activity, RefreshCw } from 'lucide-react'
import { Link } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { Empty, PanelCard } from '@/components/panel'
import { Button } from '@/components/ui/button'
import { api } from '@/lib/api'
import { duration, fmt } from '@/lib/format'
import type { MonitorResponse } from '@/lib/types'

export function MonitorPage() {
  const { data, isFetching, refetch } = useQuery({
    queryKey: ['monitor'],
    queryFn: () => api<MonitorResponse>('GET', '/monitor'),
    refetchInterval: 10000,
  })
  const { data: meta } = useQuery({
    queryKey: ['meta'],
    queryFn: () => api<{ version: string }>('GET', '/meta'),
    staleTime: Infinity,
  })

  const r = data?.requests
  const a = data?.accounts
  const sys = data?.system
  const mem = sys?.memory
  const success = r?.success_rate ?? null
  const health = success == null ? 100 : Math.max(0, Math.min(100, success))
  const bad = (Number(a?.invalid) || 0) + (Number(a?.exhausted) || 0)
  const cpuPct = sys?.load_1m != null && sys.cpu_count ? Math.min(100, (sys.load_1m / sys.cpu_count) * 100) : 0
  const accountTotal = Number(a?.total) || 0

  return (
    <div className="mx-auto flex w-full max-w-6xl flex-col gap-6">
      {/* 頁首 */}
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">運維監控</h1>
          <p className="text-sm text-muted-foreground">服務健康、資源用量與請求品質</p>
        </div>
        <div className="flex items-center gap-2">
          <span className="mr-1 flex items-center gap-1.5 text-xs text-muted-foreground">
            <span className="size-1.5 animate-pulse rounded-full bg-emerald-500" />
            監控中
          </span>
          <Button variant="outline" size="sm" onClick={() => void refetch()} disabled={isFetching}>
            <RefreshCw className={isFetching ? 'animate-spin' : undefined} /> 重新整理
          </Button>
        </div>
      </div>

      {/* 服務健康主卡 */}
      <PanelCard
        title="服務健康"
        subtitle={data ? '最近更新：' + new Date(data.ts * 1000).toLocaleTimeString('zh-TW') : '等待資料'}
        badge={
          <span
            className={
              'flex items-center gap-1.5 rounded-full px-2.5 py-1 text-xs ' +
              (bad ? 'bg-amber-100 text-amber-700' : 'bg-emerald-100 text-emerald-700')
            }
          >
            <span className={'size-1.5 rounded-full ' + (bad ? 'bg-amber-500' : 'bg-emerald-500')} />
            {bad ? '需要留意' : '運行正常'}
          </span>
        }
      >
        <div className="flex flex-col gap-6 lg:flex-row lg:items-center">
          <div className="flex flex-col items-center gap-2">
            <div
              className="relative flex size-36 items-center justify-center rounded-full"
              style={{ background: `conic-gradient(#22c55e ${health * 3.6}deg, #e9edf3 0)` }}
            >
              <div className="flex size-[100px] flex-col items-center justify-center rounded-full bg-card">
                <strong className="text-3xl tabular-nums">{Math.round(health)}</strong>
                <span className="text-xs text-muted-foreground">健康分</span>
              </div>
            </div>
            <span className="text-xs text-muted-foreground">最近狀態</span>
          </div>
          <div className="grid flex-1 grid-cols-2 gap-3 xl:grid-cols-4">
            <Kpi label="請求數" value={fmt(r?.total)} detail="累計調度" />
            <Kpi
              label="成功率"
              value={success == null ? '--' : success.toFixed(2) + '%'}
              detail="依回應結果計算"
              tone={success == null ? undefined : success >= 99 ? 'text-emerald-600' : 'text-amber-600'}
            />
            <Kpi label="平均 QPS" value={Number(r?.average_qps || 0).toFixed(3)} detail="程序啟動後平均" />
            <Kpi label="程序運行" value={duration(data?.uptime_sec)} detail="服務啟動後" />
          </div>
        </div>
      </PanelCard>

      {/* 請求品質＋主機資源 */}
      <div className="grid gap-4 lg:grid-cols-2">
        <PanelCard title="請求品質" subtitle="閘道與上游帳號池" badge={bad ? '需要留意' : '穩定'}>
          <div className="grid grid-cols-2 gap-4">
            <QualityItem label="失敗請求" value={fmt(r?.errors)} />
            <QualityItem label="失敗率" value={r?.total ? (((r.errors || 0) / r.total) * 100).toFixed(2) + '%' : '0%'} />
            <QualityItem label="可用帳號" value={fmt(a?.active)} />
            <QualityItem label="額度耗盡" value={fmt(a?.exhausted)} />
          </div>
        </PanelCard>

        <PanelCard title="主機資源" subtitle="執行服務的主機即時資訊" badge={mem?.used_percent != null ? '正常' : '受限'}>
          <div className="flex flex-col gap-4">
            <ResourceRow
              label="CPU"
              value={sys?.load_1m == null ? '--' : sys.load_1m.toFixed(2)}
              pct={cpuPct}
              detail={`${fmt(sys?.cpu_count)} 核心 · 負載`}
            />
            <ResourceRow
              label="記憶體"
              value={mem?.used_percent == null ? '--' : mem.used_percent.toFixed(1) + '%'}
              pct={Number(mem?.used_percent) || 0}
              detail={
                mem?.total_mb == null
                  ? '主機資訊不可用'
                  : `${mem.total_mb.toFixed(0)} MB 總量 · ${mem.available_mb?.toFixed(0)} MB 可用`
              }
            />
          </div>
        </PanelCard>
      </div>

      {/* 服務元件 */}
      <PanelCard title="服務元件" subtitle="目前程序內的核心模組狀態" badge={`${data?.services?.length ?? 0} 個元件`}>
        {data?.services?.length ? (
          <div className="flex flex-col divide-y">
            {data.services.map((s) => (
              <div key={s.name} className="flex items-center gap-3 py-3 first:pt-0 last:pb-0">
                <span className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-muted text-muted-foreground [&>svg]:size-4">
                  <Activity />
                </span>
                <span className="min-w-0 flex-1 leading-tight">
                  <strong className="block truncate text-sm">{s.name}</strong>
                  <small className="block truncate text-xs text-muted-foreground">{s.detail || ''}</small>
                </span>
                <span
                  className={
                    'flex shrink-0 items-center gap-1.5 rounded-full px-2.5 py-1 text-xs ' +
                    (s.status === 'online' ? 'bg-emerald-100 text-emerald-700' : 'bg-muted text-muted-foreground')
                  }
                >
                  <span className={'size-1.5 rounded-full ' + (s.status === 'online' ? 'bg-emerald-500' : 'bg-muted-foreground/50')} />
                  {s.status === 'online' ? '運行中' : '待命'}
                </span>
              </div>
            ))}
          </div>
        ) : (
          <Empty>尚無服務資料</Empty>
        )}
      </PanelCard>

      {/* 帳號池健康＋運行資訊 */}
      <div className="grid gap-4 lg:grid-cols-2">
        <PanelCard
          title="帳號池健康"
          subtitle="提供商：Z.AI"
          badge={
            <Link to="/admin/accounts" className="text-xs font-normal text-muted-foreground underline-offset-4 hover:underline">
              管理帳號
            </Link>
          }
        >
          <div className="flex flex-col gap-3">
            <div className="h-2 overflow-hidden rounded-full bg-muted">
              <i
                className="block h-full rounded-full bg-emerald-500"
                style={{ width: `${accountTotal ? Math.max(0, ((Number(a?.active) || 0) / accountTotal) * 100) : 0}%` }}
              />
            </div>
            <div className="flex flex-wrap gap-4 text-xs text-muted-foreground">
              <span className="flex items-center gap-1.5"><i className="size-2 rounded-full bg-emerald-500" />正常 <strong className="text-foreground">{fmt(a?.active)}</strong></span>
              <span className="flex items-center gap-1.5"><i className="size-2 rounded-full bg-amber-500" />冷卻 <strong className="text-foreground">{fmt(a?.cooling)}</strong></span>
              <span className="flex items-center gap-1.5"><i className="size-2 rounded-full bg-red-500" />異常 <strong className="text-foreground">{fmt(a?.invalid)}</strong></span>
            </div>
          </div>
        </PanelCard>

        <PanelCard title="運行資訊" subtitle="環境與版本">
          <div className="grid grid-cols-2 gap-3">
            <RuntimeItem label="應用版本" value={meta?.version ? 'v' + meta.version : '--'} />
            <RuntimeItem label="CPU 核心" value={sys?.cpu_count ? fmt(sys.cpu_count) : '--'} />
            <RuntimeItem label="額度輪詢" value="背景輪詢" />
            <RuntimeItem label="服務狀態" value="Online" tone="text-emerald-600" />
          </div>
        </PanelCard>
      </div>
    </div>
  )
}

function Kpi({ label, value, detail, tone }: { label: string; value: string; detail: string; tone?: string }) {
  return (
    <div className="rounded-lg border p-3">
      <span className="block text-xs text-muted-foreground">{label}</span>
      <strong className={'block truncate text-lg tabular-nums ' + (tone ?? '')}>{value}</strong>
      <small className="block truncate text-[11px] text-muted-foreground">{detail}</small>
    </div>
  )
}

function QualityItem({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-center justify-between rounded-lg bg-muted/60 px-3 py-2 text-sm">
      <span className="text-muted-foreground">{label}</span>
      <strong className="tabular-nums">{value}</strong>
    </div>
  )
}

function ResourceRow({ label, value, pct, detail }: { label: string; value: string; pct: number; detail: string }) {
  const color = pct >= 85 ? '#ef4444' : pct >= 60 ? '#f59e0b' : '#22c55e'
  return (
    <div className="flex flex-col gap-1.5">
      <div className="flex items-center justify-between text-sm">
        <span className="text-muted-foreground">{label}</span>
        <strong className="tabular-nums">{value}</strong>
      </div>
      <div className="h-2 overflow-hidden rounded-full bg-muted">
        <i className="block h-full rounded-full" style={{ width: `${Math.max(0, Math.min(100, pct))}%`, background: color }} />
      </div>
      <small className="text-xs text-muted-foreground">{detail}</small>
    </div>
  )
}

function RuntimeItem({ label, value, tone }: { label: string; value: string; tone?: string }) {
  return (
    <div className="rounded-lg border p-3">
      <span className="block text-xs text-muted-foreground">{label}</span>
      <strong className={'block truncate text-sm ' + (tone ?? '')}>{value}</strong>
    </div>
  )
}
