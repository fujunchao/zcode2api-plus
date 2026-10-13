import { useEffect, useRef, useState, type FormEvent } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Loader2, RefreshCw } from 'lucide-react'
import { toast } from 'sonner'
import { useConfirm } from '@/components/confirm'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { api, errMsg } from '@/lib/api'
import type { SubscriptionConfig, SubscriptionRun, SubscriptionView, SubscriptionBlock } from '@/lib/types'

const base = '/proxies/subscription'
const statusLabels: Record<string, string> = {
  fetching: '读取订阅', filtering: '筛选候选', probing: '复检入口', committing: '提交结果',
  completed: '完成', canceled: '已取消', failed: '失败', interrupted: '重启中断',
}
const timeText = (ms: number) => ms ? new Date(ms).toLocaleString() : '—'

/** 延迟展开，避免未使用订阅的页面产生额外轮询。沿用现有中性主题和紧凑操作区。 */
export function ProxySubscription() {
  const [open, setOpen] = useState(false)
  return <section className="rounded-xl border bg-card p-4" aria-label="自动补充代理">
    <div className="flex flex-wrap items-center justify-between gap-3">
      <div>
        <h2 className="text-sm font-semibold">自动补充代理 <span className="ml-2 rounded bg-muted px-2 py-0.5 text-xs font-normal">试运行</span></h2>
        <p className="mt-1 text-xs text-muted-foreground">从 proxy-checker 读取合格线路，本机复检后增量入池；不主动改派账号。</p>
      </div>
      <Button variant="outline" size="sm" aria-expanded={open} onClick={() => setOpen(!open)}>{open ? '收起订阅设置' : '配置自动补充'}</Button>
    </div>
    {open && <SubscriptionContent />}
  </section>
}

function SubscriptionContent() {
  const qc = useQueryClient()
  const { confirm, element } = useConfirm()
  const [acting, setActing] = useState(false)
  const [showBlocks, setShowBlocks] = useState(false)
  const query = useQuery({
    queryKey: ['proxy-subscription'],
    queryFn: () => api<SubscriptionView>('GET', base),
    refetchInterval: (q) => q.state.data?.current ? 1000 : 15000,
  })
  const blocksQuery = useQuery({
    queryKey: ['proxy-subscription-blocks'],
    queryFn: () => api<{ blocks: SubscriptionBlock[] }>('GET', base + '/blocks'),
    enabled: showBlocks,
  })
  const last = query.data?.last_result ?? query.data?.runs[0]
  const finished = useRef('')
  useEffect(() => {
    if (last?.finished_at && last.id !== finished.current) {
      finished.current = last.id
      void qc.invalidateQueries({ queryKey: ['proxies'] })
      void qc.invalidateQueries({ queryKey: ['proxy-subscription-blocks'] })
    }
  }, [last?.id, last?.finished_at, qc])
  const refresh = () => { void qc.invalidateQueries({ queryKey: ['proxy-subscription'] }) }

  async function start(mode: 'preview' | 'import') {
    setActing(true)
    try {
      await api<SubscriptionRun>('POST', base + '/runs', { mode })
      toast.success(mode === 'preview' ? '预览任务已启动，不会导入线路' : '导入任务已启动，实际结果见任务记录')
    } catch (e) { toast.error(errMsg(e)) }
    finally { setActing(false); refresh() }
  }

  async function cancel(id: string) {
    setActing(true)
    try { await api('POST', base + '/runs/' + encodeURIComponent(id) + '/cancel'); toast.success('已请求取消；以最终任务结果为准') }
    catch (e) { toast.error(errMsg(e)) }
    finally { setActing(false); refresh() }
  }

  if (query.isPending) return <p role="status" className="mt-4 text-sm">正在读取订阅配置…</p>
  if (!query.data) return <div role="alert" className="mt-4 text-sm text-destructive">
    无法读取订阅配置：{errMsg(query.error)} <Button variant="outline" size="sm" onClick={() => void query.refetch()}>重试</Button>
  </div>
  const d = query.data
  const busy = acting || !!d.current
  const selected = d.current ?? last
  return <div className="mt-5 space-y-5">
    {element}
    {!d.target_supported && <p role="alert" className="text-sm text-destructive">当前自定义上游不适用默认 ZCode 配置探针，不能启动订阅任务。</p>}
    {d.import_paused && <p role="alert" className="text-sm text-destructive">禁止记录已达到预算，导入已闭锁。请检查订阅状态，不能通过清除有效记录自动放行。</p>}
    <div className="flex flex-wrap gap-x-6 gap-y-2 rounded-lg bg-muted/50 px-3 py-2 text-xs">
      <span>自动同步：<strong>{d.config.auto ? '已开启' : '关闭'}</strong></span>
      <span>来源线路：<strong className="tabular-nums">{d.owned} / {d.config.capacity}</strong>（含停用）</span>
      <span>下次运行：{d.config.auto ? timeText(d.next_run_at) : '不自动运行'}</span>
    </div>
    <ConfigEditor key={d.config.revision} initial={d.config} onSaved={refresh} />
    <div className="flex flex-wrap gap-2 border-t pt-4">
      <Button variant="outline" size="sm" disabled={busy || !d.config.configured || !d.target_supported}
        onClick={() => void start('preview')}>预览并复检（不导入）</Button>
      <Button size="sm" disabled={busy || !d.config.configured || !d.target_supported || d.import_paused}
        onClick={() => confirm({
          title: '立即导入订阅线路？',
          description: '通过复检的线路会增量入池，最多新增 ' + d.config.import_limit + ' 条。新线路将' +
            (d.config.enable_imported ? '启用并参与后续选线' : '保持停用，需手动启用') + '；不会直接改派现有账号。',
          confirmText: '开始导入', onConfirm: () => start('import'),
        })}>立即导入</Button>
      {d.current && <Button variant="outline" size="sm" disabled={acting} onClick={() => void cancel(d.current!.id)}>取消当前任务</Button>}
      <Button variant="ghost" size="sm" onClick={refresh}><RefreshCw className="size-3.5" />刷新状态</Button>
    </div>
    <p className="text-xs text-muted-foreground">历史成功时间和连续次数仅供排序参考，不再作为入库门槛。必须通过本次配置入口复检；不使用账号凭据，不保证模型业务成功或不会触发 405。修改配置会取消当前任务。</p>
    {selected ? <RunResult run={selected} /> : <p className="text-sm text-muted-foreground">尚无任务。保存 JSON 订阅地址后，建议先执行预览。</p>}
    <details className="border-t pt-3">
      <summary className="cursor-pointer text-sm">同步历史（最近 {d.runs.length} 轮）</summary>
      <div className="mt-3 max-h-72 space-y-3 overflow-auto">
        {d.runs.map((r) => <details key={r.id} className="rounded border p-2">
          <summary className="cursor-pointer text-xs">{timeText(r.started_at)} · {r.mode === 'preview' ? '预览' : '导入'} · {statusLabels[r.status] ?? r.status} · 新增 {r.added}</summary>
          <RunResult run={r} />
        </details>)}
      </div>
    </details>
    <div className="border-t pt-3">
      <Button variant="outline" size="sm" onClick={() => { setShowBlocks(!showBlocks); void blocksQuery.refetch() }}>管理禁止／暂缓重入</Button>
      {showBlocks && <div className="mt-3 max-h-64 space-y-2 overflow-auto">
        {blocksQuery.isError && <p role="alert">无法读取暂缓记录</p>}
        {blocksQuery.data?.blocks.length === 0 && <p className="text-xs text-muted-foreground">没有有效的禁止或暂缓记录。</p>}
        {blocksQuery.data?.blocks.map((b) => <div key={b.fingerprint} className="flex flex-wrap items-center gap-3 rounded border p-2 text-xs">
          <code title={b.fingerprint}>{b.fingerprint.slice(0, 12)}</code>
          <span>{b.reason} · {b.until ? '暂缓至 ' + timeText(b.until) : '人工禁止'}</span>
          <Button variant="outline" size="sm" onClick={() => confirm({
            title: '解除这条线路的重入限制？', description: '解除后仍需新的有效记录和复检，不会立即导入或启用。',
            confirmText: '解除限制', onConfirm: async () => {
              try { await api('DELETE', base + '/blocks/' + encodeURIComponent(b.fingerprint)); await blocksQuery.refetch(); toast.success('已解除重入限制') }
              catch (e) { toast.error(errMsg(e)) }
            },
          })}>解除</Button>
        </div>)}
      </div>}
    </div>
  </div>
}

function ConfigEditor({ initial, onSaved }: { initial: SubscriptionConfig; onSaved: () => void }) {
  const [c, setC] = useState(initial)
  const [url, setURL] = useState('')
  const [clear, setClear] = useState(false)
  const [saving, setSaving] = useState(false)
  async function save(e: FormEvent) {
    e.preventDefault()
    setSaving(true)
    const { configured: _configured, source_display: _display, max_age_minutes: _legacyAge, ...values } = c
    try {
      await api('PUT', base, { ...values, ...(clear ? { clear_url: true } : url.trim() ? { url: url.trim() } : {}) })
      toast.success('订阅配置已保存')
      onSaved()
    } catch (error) { toast.error(errMsg(error)) }
    finally { setSaving(false) }
  }
  const numbers: { key: 'interval_minutes' | 'candidates' | 'import_limit' | 'capacity' | 'concurrency' | 'budget_seconds'; label: string; min: number; max: number }[] = [
    { key: 'interval_minutes', label: '同步间隔（分钟）', min: 5, max: 1440 },
    { key: 'candidates', label: '每轮复检上限', min: 1, max: 500 },
    { key: 'import_limit', label: '每轮新增上限', min: 1, max: 100 },
    { key: 'capacity', label: '来源池容量', min: 1, max: 500 },
    { key: 'concurrency', label: '复检并发', min: 1, max: 8 },
    { key: 'budget_seconds', label: '任务预算（秒）', min: 30, max: 900 },
  ]
  return <form onSubmit={(e) => void save(e)} className="space-y-4" aria-label="订阅配置">
    <div className="grid gap-2">
      <Label htmlFor="subscription-url">JSON 订阅地址</Label>
      <Input id="subscription-url" type="password" autoComplete="off" value={url} disabled={clear}
        placeholder={initial.configured ? '已配置；留空保持原地址' : '输入 proxy-checker 的 JSON 订阅地址'} onChange={(e) => setURL(e.target.value)} />
      <p className="break-all text-xs text-muted-foreground">当前来源：{initial.source_display || '未配置'}；完整地址不会回显。</p>
      {initial.configured && <label className="flex items-center gap-2 text-xs"><input type="checkbox" checked={clear} onChange={(e) => { setClear(e.target.checked); if (e.target.checked) setC({ ...c, auto: false }) }} />清空地址并关闭自动同步</label>}
    </div>
    <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
      {numbers.map(({ key, label, min, max }) => <div className="grid gap-2" key={key}>
        <Label htmlFor={'subscription-' + key}>{label}</Label>
        <Input id={'subscription-' + key} type="number" min={min} max={max} required value={c[key]} onChange={(e) => setC({ ...c, [key]: Number(e.target.value) })} />
      </div>)}
    </div>
    <div className="flex flex-wrap gap-x-6 gap-y-3 text-sm">
      <label className="flex items-center gap-2"><input type="checkbox" disabled={clear} checked={c.auto} onChange={(e) => setC({ ...c, auto: e.target.checked })} />自动同步</label>
      <label className="flex items-center gap-2"><input type="checkbox" checked={c.enable_imported} onChange={(e) => setC({ ...c, enable_imported: e.target.checked })} />新增线路自动启用</label>
    </div>
    <p className="text-xs text-muted-foreground">默认先预览，再少量导入停用线路。修改容量不会删除现有线路，修改启用默认值不会重新启用旧线路。</p>
    <Button variant="outline" size="sm" type="submit" disabled={saving}>{saving && <Loader2 className="size-4 animate-spin motion-reduce:animate-none" />}保存订阅配置</Button>
  </form>
}

function RunResult({ run: r }: { run: SubscriptionRun }) {
  const legacy = r.history_policy !== 'reference_only'
  return <div className="mt-3 rounded-lg border p-3 text-xs" aria-live="polite">
    <p className="font-medium">{r.mode === 'preview' ? '预览' : '导入'} · {statusLabels[r.status] ?? r.status}{r.automatic ? ' · 定时任务' : ''}</p>
    <p className="mt-1 text-muted-foreground">{timeText(r.started_at)}{r.finished_at ? ' → ' + timeText(r.finished_at) : ''}</p>
    <div className="mt-3 flex flex-wrap gap-x-4 gap-y-2 font-mono tabular-nums">
      <span>收到 {r.received}</span><span>候选 {r.candidates}</span><span>已复检 {r.probed}</span><span>通过 {r.passed}</span><strong>新增 {r.added}</strong>
    </div>
    <p className="mt-2 text-muted-foreground">{legacy ? '旧规则格式／门槛拒绝' : '格式／目标拒绝'} {r.rejected}
      {legacy && r.expired > 0 ? ' · 旧规则历史过期 ' + r.expired : ''}
      {' · 已有／重复 ' + r.existing + ' · 暂缓 ' + r.blocked + ' · 提交跳过 ' + r.commit_skipped}
    </p>
    {r.error && <p role="alert" className="mt-2 text-destructive">{r.error}</p>}
    {!!r.details?.length && <details className="mt-3">
      <summary className="cursor-pointer">查看复检明细（最多 50 条）</summary>
      <ul className="mt-2 max-h-56 space-y-2 overflow-auto">
        {r.details.map((item) => <li key={item.fingerprint} className="break-all"><code>{item.endpoint}</code> · {item.ms} ms · {item.reason}</li>)}
      </ul>
    </details>}
  </div>
}
