import { ArrowRight } from 'lucide-react'
import { useQuery } from '@tanstack/react-query'
import { api, errMsg } from '@/lib/api'
import { confirmedUsage, describe, labels, time } from '@/components/health/labels'
import type { ModelProbeRun, ProxyProfile } from '@/lib/types'

/** 单个样本徽章：stale 灰、complete 绿、405 琥珀，其余中性。 */
function sampleTone(s: ModelProbeRun['samples'][number]) {
  if (s.stale) return 'text-muted-foreground'
  if (s.complete) return 'border-emerald-200 text-emerald-800'
  if (s.status === 405) return 'border-amber-200 text-amber-800'
  return ''
}

/**
 * 诊断记录：默认每次诊断只占一行摘要，点击才展开样本链。
 * 此前每次固定渲染最多 8 个徽章，20 次即 160 个，长到无法扫读。
 */
export function HistoryList({ target, profiles, expanded, onToggle }: {
  target: string
  profiles: ProxyProfile[]
  expanded: string
  onToggle: (id: string) => void
}) {
  const names = new Map(profiles.map((p) => [p.id, p.name]))
  const { data, error } = useQuery({ queryKey: ['model-probe-history', target], enabled: !!target, queryFn: () => api<{ runs: ModelProbeRun[] }>('GET', '/proxies/' + encodeURIComponent(target) + '/model-probe/history?limit=20'), refetchInterval: 10_000 })
  if (!target) return <p className="text-xs text-muted-foreground">在上方表格点「记录」查看某条线路的最近诊断。</p>
  return <section aria-label="模型对照记录" className="rounded-lg border p-3">
    <div className="mb-3 text-sm font-medium">{names.get(target) ?? target} · 最近 20 次诊断</div>
    {error ? <p role="alert">读取记录失败：{errMsg(error)}</p> : !data ? <p role="status">正在读取记录…</p> : data.runs.length === 0 ? <p className="text-xs text-muted-foreground">还没有模型检查记录。</p> : data.runs.map((run) => {
      const open = expanded === run.id
      return <div key={run.id} className="border-b py-3 last:border-0">
        <div className="flex flex-wrap items-center gap-3 text-xs">
          <strong>{describe(run)}</strong>
          <span>{time(run.created_at)}</span>
          <span>{run.attempts}/8 次出站 · {run.confirmed_rounds}/2 轮对照</span>
          <button type="button" className="ml-auto text-xs underline" aria-expanded={open} onClick={() => onToggle(open ? '' : run.id)}>{open ? '收起' : '展开'}</button>
        </div>
        {open && <>
          <div className="my-2 flex flex-wrap items-center gap-2">{run.samples.map((s, i) => <div key={s.request_id || i} className="flex items-center gap-2">{i > 0 && <ArrowRight className="size-3 text-muted-foreground" />}<span className={'rounded-md border px-2 py-1 text-xs ' + sampleTone(s)} title={(labels[s.kind] ?? s.kind) + ' · 账号引用 ' + s.account_ref}>{s.role === 'screen' ? '筛查' : (s.role === 'reference' ? '参考' : '目标') + s.round} · {s.status || '未取得响应'}{s.business_code && '/' + s.business_code}{s.stale && ' · 旧结果'}</span></div>)}</div>
          <p className="mb-1 text-xs text-muted-foreground">{confirmedUsage(run)}</p>
          <p className="text-xs text-muted-foreground">{labels[run.reason] ?? run.reason} · {run.rule_version}{run.decision_id && ' · ' + run.decision_id}</p>
        </>}
      </div>
    })}
  </section>
}
