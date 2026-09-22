/* 代理設定頁：線路清單、目前出口測試、新增／編輯／刪除／測試線路、短流探測 */
import { Activity, Pencil, Plus, Radio, Trash2 } from 'lucide-react'
import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { useConfirm } from '@/components/confirm'
import { Empty, PanelCard } from '@/components/panel'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { api, errMsg } from '@/lib/api'
import { proxyScheme } from '@/lib/format'
import type {
  ProbeResult,
  ProxyProfile,
  StreamProbeResult,
  TestAllResponse,
  UpstreamProbe,
} from '@/lib/types'

interface ProxiesResponse {
  profiles: ProxyProfile[]
}

/* 伺服器直連探測狀態 */
type CurrentResult =
  | { state: 'idle' }
  | { state: 'testing' }
  | { state: 'ok'; main: string; sub: string }
  | { state: 'error'; text: string }

/* 單線路探測結果；warn 用於「拿到了回應、但那是邊緣攔截頁」 */
interface RowResult {
  state: 'testing' | 'ok' | 'warn' | 'error'
  text: string
}

function maskUrl(url: string): string {
  return String(url || '').replace(/\/\/([^@/]+)@/, '//***@')
}

/* z.ai 側可達性的主文案：可達 / 被邊緣攔截 / 不可達 三態。 */
function formatUpstream(u?: UpstreamProbe): string {
  if (!u) return ''
  if (u.ok) return 'z.ai 可達' + (u.ms != null ? ` ${u.ms} ms` : '')
  if (u.blocked) return 'z.ai 被邊緣攔截'
  return 'z.ai 不可達'
}

/* 逐入口明細压成一行：zcode.z.ai 200 · api.z.ai 401 */
function upstreamDetail(u?: UpstreamProbe): string {
  return (u?.targets || [])
    .map((t) => (t.ok ? `${t.host} ${t.status ?? '-'}${t.blocked ? '（攔截頁）' : ''}` : `${t.host} 無回應`))
    .join(' · ')
}

/* 探測結果 → 行狀態。唯一結論是「能不能連上 z.ai」：拿到非攔截回應算可用，
   拿到攔截頁單獨標示（琥珀色），其餘算不通。 */
function describeProbe(d: ProbeResult): RowResult {
  const text = formatUpstream(d.upstream) || d.error || ''
  if (d.ok !== false) return { state: 'ok', text }
  return { state: d.upstream?.blocked ? 'warn' : 'error', text }
}

/* 刪除線路後的提示：說明原本綁在這條線上的帳號被怎麼處置 */
function deletedMessage(reassigned: number, direct: number): string {
  if (reassigned && direct) {
    return `代理已刪除：${reassigned} 個帳號已改派到空閒線路，${direct} 個無線路可補已改為直連`
  }
  if (reassigned) return `代理已刪除：${reassigned} 個帳號已改派到空閒線路`
  if (direct) return `代理已刪除：${direct} 個帳號無空閒線路可補，已改為直連`
  return '代理已刪除'
}

export function ProxiesPage() {
  const qc = useQueryClient()
  const { confirm, element: confirmElement } = useConfirm()

  const { data } = useQuery({
    queryKey: ['proxies'],
    queryFn: () => api<ProxiesResponse>('GET', '/proxies'),
  })
  const profiles = data?.profiles ?? []

  const [current, setCurrent] = useState<CurrentResult>({ state: 'idle' })
  const [testingCurrent, setTestingCurrent] = useState(false)
  const [rowResults, setRowResults] = useState<Record<string, RowResult>>({})
  const [testingAll, setTestingAll] = useState(false)
  /* 短流探測結果（與可達性探測分開展示：兩者回答的問題不同） */
  const [streamResults, setStreamResults] = useState<Record<string, { state: 'testing' | 'ok' | 'error'; text: string }>>({})

  const [modalOpen, setModalOpen] = useState(false)
  const [editingId, setEditingId] = useState('')
  const [name, setName] = useState('')
  const [url, setUrl] = useState('')
  const [saving, setSaving] = useState(false)

  function invalidate() {
    void qc.invalidateQueries({ queryKey: ['proxies'] })
  }

  function openModal(p?: ProxyProfile) {
    setEditingId(p?.id ?? '')
    setName(p?.name ?? '')
    setUrl(p?.url ?? '')
    setModalOpen(true)
  }

  async function save() {
    if (!url.trim()) {
      toast.error('請輸入代理地址')
      return
    }
    setSaving(true)
    try {
      await api(editingId ? 'PUT' : 'POST', editingId ? '/proxies/' + encodeURIComponent(editingId) : '/proxies', {
        name: name.trim(),
        url: url.trim(),
        enabled: true,
      })
      setModalOpen(false)
      toast.success(editingId ? '代理已更新' : '代理已建立')
      invalidate()
    } catch (e) {
      toast.error('儲存失敗：' + errMsg(e))
    } finally {
      setSaving(false)
    }
  }

  function deleteProxy(p: ProxyProfile) {
    confirm({
      title: '刪除代理',
      danger: true,
      description: (
        <>
          確認刪除 <code className="rounded bg-muted px-1 py-0.5">{p.name}</code>？使用此線路的帳號會自動改派到其它空閒線路，
          沒有空閒線路時才改為直連。
        </>
      ),
      onConfirm: async () => {
        try {
          const d = await api<{ ok: boolean; reassigned: number; direct_fallback: number }>(
            'DELETE',
            '/proxies/' + encodeURIComponent(p.id),
          )
          toast.success(deletedMessage(d.reassigned || 0, d.direct_fallback || 0))
          invalidate()
        } catch (e) {
          toast.error('刪除失敗：' + errMsg(e))
        }
      },
    })
  }

  async function testProxy(p: ProxyProfile) {
    setRowResults((m) => ({ ...m, [p.id]: { state: 'testing', text: '正在探測 z.ai…' } }))
    try {
      const d = await api<ProbeResult>('POST', '/proxies/' + encodeURIComponent(p.id) + '/test')
      const result = describeProbe(d)
      setRowResults((m) => ({ ...m, [p.id]: result }))
      if (result.state === 'ok') toast.success('線路可用：z.ai 可達')
      else if (result.state === 'warn') toast.warning('線路被 z.ai 邊緣攔截')
      else toast.error('線路無法連上 z.ai')
    } catch (e) {
      setRowResults((m) => ({ ...m, [p.id]: { state: 'error', text: errMsg(e) } }))
      toast.error('代理測試失敗：' + errMsg(e))
    }
  }

  /* 帶憑據的短流探測：借這條線路綁定的 JWT 帳號發一條最小流式請求，驗證
     「響應頭能到、首個數據行多久到、流能否走到 message_stop」。與可達性探測
     互補——「能連上、能協商、但流在中途被掐」只有這個看得到。單次 ≤30 秒。 */
  async function streamTest(p: ProxyProfile) {
    setStreamResults((m) => ({ ...m, [p.id]: { state: 'testing', text: '正在發起短流探測（≤30 秒）…' } }))
    try {
      const d = await api<StreamProbeResult>(
        'POST',
        '/proxies/' + encodeURIComponent(p.id) + '/stream-test',
      )
      const extra = [
        d.first_data_ms != null ? `首數據 ${d.first_data_ms} ms` : '',
        d.total_ms != null ? `總耗時 ${(d.total_ms / 1000).toFixed(1)} s` : '',
      ]
        .filter(Boolean)
        .join(' · ')
      const text = d.verdict + (extra ? `（${extra}）` : '') + (d.error ? `：${d.error}` : '')
      setStreamResults((m) => ({ ...m, [p.id]: { state: d.ok ? 'ok' : 'error', text } }))
      if (d.ok) toast.success(`${p.name}：${d.verdict}`)
      else toast.error(`${p.name}：${d.verdict}`)
    } catch (e) {
      setStreamResults((m) => ({ ...m, [p.id]: { state: 'error', text: errMsg(e) } }))
      toast.error('短流探測失敗：' + errMsg(e))
    }
  }

  /* 伺服器自身（不走代理）到 z.ai 的可達性——外網斷了或 z.ai 掛了，所有線路
     看起來都會「不通」，先排除這一層。 */
  async function testCurrentLine() {
    setTestingCurrent(true)
    setCurrent({ state: 'testing' })
    try {
      const d = await api<ProbeResult>('POST', '/proxies/test-current')
      if (d.ok === false) {
        setCurrent({ state: 'error', text: formatUpstream(d.upstream) || d.error || '無法連上 z.ai' })
        toast.warning('伺服器直連無法連上 z.ai')
        return
      }
      setCurrent({
        state: 'ok',
        main: formatUpstream(d.upstream) || '探測完成',
        sub: upstreamDetail(d.upstream),
      })
      toast.success('伺服器直連正常')
    } catch (e) {
      setCurrent({ state: 'error', text: errMsg(e) })
      toast.error('直連測試失敗：' + errMsg(e))
    } finally {
      setTestingCurrent(false)
    }
  }

  /* 一鍵測試全部「啟用」線路：後端併發探測（8 路），返回後逐行回填結果。
     單條最壞 12s（兩個 z.ai 入口依次 6s 逾時），故測試期間按鈕禁用。 */
  async function testAll() {
    const targets = profiles.filter((p) => p.enabled)
    if (!targets.length) {
      toast.info('沒有可測試的線路')
      return
    }
    setTestingAll(true)
    setRowResults((m) => {
      const next = { ...m }
      for (const p of targets) next[p.id] = { state: 'testing', text: '正在探測 z.ai…' }
      return next
    })
    try {
      const d = await api<TestAllResponse>('POST', '/proxies/test-all')
      setRowResults((m) => {
        const next = { ...m }
        for (const r of d.results) {
          const result = describeProbe(r)
          // 完全沒有出口資訊時才回退到後端給的錯誤文案
          next[r.id] = result.state === 'error' && !result.text ? { state: 'error', text: r.error || '測試失敗' } : result
        }
        return next
      })
      if (d.summary.fail > 0) {
        toast.warning(`線路測試完成：可用 ${d.summary.ok}／有問題 ${d.summary.fail}`)
      } else {
        toast.success(`線路測試完成：${d.summary.total} 條全部可用`)
      }
    } catch (e) {
      toast.error('批量測試失敗：' + errMsg(e))
    } finally {
      setTestingAll(false)
    }
  }

  return (
    <div className="mx-auto flex w-full max-w-6xl flex-col gap-6">
      {/* 頁首 */}
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">代理設定</h1>
          <p className="text-sm text-muted-foreground">集中管理 HTTP／SOCKS5 出口線路</p>
        </div>
        <div className="flex items-center gap-2">
          <Button variant="outline" size="sm" onClick={() => void testCurrentLine()} disabled={testingCurrent}>
            <Activity /> 測試直連
          </Button>
          <Button
            variant="outline"
            size="sm"
            onClick={() => void testAll()}
            disabled={testingAll || !profiles.length}
          >
            <Activity /> {testingAll ? '正在測試全部…' : '測試全部線路'}
          </Button>
          <Button size="sm" onClick={() => openModal()}>
            <Plus /> 新增代理
          </Button>
        </div>
      </div>

      {/* 伺服器目前出口 */}
      <div
        aria-live="polite"
        className={
          'flex items-center gap-3 rounded-xl border bg-card px-4 py-3 ' +
          (current.state === 'error' ? 'border-destructive/40' : '')
        }
      >
        <span
          className={
            'size-2.5 shrink-0 rounded-full ' +
            (current.state === 'ok'
              ? 'bg-emerald-500'
              : current.state === 'error'
                ? 'bg-red-500'
                : current.state === 'testing'
                  ? 'animate-pulse bg-amber-500'
                  : 'bg-muted-foreground/30')
          }
        />
        <div className="min-w-0 flex-1 leading-tight">
          <span className="block text-xs text-muted-foreground">伺服器直連 z.ai</span>
          <strong className="block truncate text-sm">
            {current.state === 'ok'
              ? current.main
              : current.state === 'testing'
                ? '正在探測 z.ai…'
                : current.state === 'error'
                  ? '無法連上 z.ai'
                  : '尚未測試'}
          </strong>
          <small className="block truncate text-xs text-muted-foreground">
            {current.state === 'ok'
              ? current.sub
              : current.state === 'testing'
                ? '正在連線至 z.ai 側入口'
                : current.state === 'error'
                  ? current.text
                  : '經伺服器自身網路（不使用代理）探測 z.ai 側入口'}
          </small>
        </div>
      </div>

      {/* 線路清單＋連線格式說明 */}
      <div className="grid gap-4 lg:grid-cols-5">
        <PanelCard
          title="代理線路"
          subtitle="測試只驗證一件事：這條線路能不能連上 z.ai"
          badge={`${profiles.length} 條線路`}
          className="lg:col-span-3"
        >
          {profiles.length ? (
            <div className="flex flex-col divide-y">
              {profiles.map((p) => {
                const result = rowResults[p.id]
                return (
                  <div key={p.id} className="flex flex-wrap items-center gap-3 py-3 first:pt-0 last:pb-0">
                    <span className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-muted text-muted-foreground [&>svg]:size-4">
                      <Activity />
                    </span>
                    <div className="min-w-0 flex-1 leading-tight">
                      <strong className="block truncate text-sm">{p.name}</strong>
                      <small className="block truncate font-mono text-xs text-muted-foreground">{maskUrl(p.url)}</small>
                      {result && (
                        <em
                          className={
                            'mt-0.5 block truncate text-xs not-italic ' +
                            (result.state === 'ok'
                              ? 'text-emerald-600'
                              : result.state === 'warn'
                                ? 'text-amber-600'
                                : result.state === 'error'
                                  ? 'text-destructive'
                                  : 'text-muted-foreground')
                          }
                        >
                          {result.text}
                        </em>
                      )}
                      {streamResults[p.id] && (
                        <em
                          className={
                            'mt-0.5 block truncate text-xs not-italic ' +
                            (streamResults[p.id].state === 'ok'
                              ? 'text-emerald-600'
                              : streamResults[p.id].state === 'error'
                                ? 'text-destructive'
                                : 'text-muted-foreground')
                          }
                          title={streamResults[p.id].text}
                        >
                          短流：{streamResults[p.id].text}
                        </em>
                      )}
                    </div>
                    {(p.truncate_total ?? 0) > 0 && (
                      <span
                        title="線路斷流計數（連續／累計；記憶體態，重啟歸零）。連續達到熔斷閾值會自動移除線路並改派帳號。"
                        className="flex items-center gap-1 rounded-full bg-red-100 px-2.5 py-1 text-xs font-medium text-red-700"
                      >
                        斷流 {p.truncate_streak ?? 0}/{p.truncate_total}
                      </span>
                    )}
                    <span className="rounded-md bg-muted px-2 py-1 text-xs font-medium">{proxyScheme(p.url)}</span>
                    <span
                      className={
                        'flex items-center gap-1.5 rounded-full px-2.5 py-1 text-xs ' +
                        (p.enabled ? 'bg-emerald-100 text-emerald-700' : 'bg-muted text-muted-foreground')
                      }
                    >
                      <span className={'size-1.5 rounded-full ' + (p.enabled ? 'bg-emerald-500' : 'bg-muted-foreground/50')} />
                      {p.enabled ? '啟用' : '停用'}
                    </span>
                    <span className="flex gap-0.5">
                      <Button variant="ghost" size="icon-sm" title="測試線路（z.ai 可達性）" onClick={() => void testProxy(p)}>
                        <Activity />
                      </Button>
                      <Button
                        variant="ghost"
                        size="icon-sm"
                        title="流式探測（借綁定帳號發真實短流，驗證能否完整承載，≤30 秒）"
                        onClick={() => void streamTest(p)}
                      >
                        <Radio />
                      </Button>
                      <Button variant="ghost" size="icon-sm" title="編輯" onClick={() => openModal(p)}>
                        <Pencil />
                      </Button>
                      <Button
                        variant="ghost"
                        size="icon-sm"
                        className="text-destructive hover:text-destructive"
                        title="刪除"
                        onClick={() => deleteProxy(p)}
                      >
                        <Trash2 />
                      </Button>
                    </span>
                  </div>
                )
              })}
            </div>
          ) : (
            <Empty>尚未建立代理線路</Empty>
          )}
        </PanelCard>

        <PanelCard title="連線格式" subtitle="支援帶認證的代理" className="lg:col-span-2">
          <div className="flex flex-col gap-3">
            {(
              [
                ['HTTP', 'http://host:port', 'bg-blue-100 text-blue-700'],
                ['S5', 'socks5://host:port', 'bg-violet-100 text-violet-700'],
                ['AUTH', 'http://user:pass@host:port', 'bg-amber-100 text-amber-700'],
              ] as [string, string, string][]
            ).map(([mark, sample, cls]) => (
              <div key={mark} className="flex items-center gap-3">
                <span className={'w-12 shrink-0 rounded-md px-2 py-1 text-center text-xs font-semibold ' + cls}>{mark}</span>
                <code className="truncate rounded-md bg-muted px-2.5 py-1.5 font-mono text-xs">{sample}</code>
              </div>
            ))}
            <p className="rounded-lg bg-muted/60 px-3 py-2 text-xs text-muted-foreground">
              僅接受 HTTP、HTTPS、SOCKS4、SOCKS5 代理地址，可包含 user:pass 認證。帳號是否使用代理、使用哪條線路，請在「帳號池」的帳號設定中選擇。
            </p>
          </div>
        </PanelCard>
      </div>

      {/* 新增／編輯代理對話框 */}
      <Dialog open={modalOpen} onOpenChange={setModalOpen}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{editingId ? '編輯代理出口' : '新增代理出口'}</DialogTitle>
            <DialogDescription>支援帶認證的 HTTP／SOCKS5 地址。</DialogDescription>
          </DialogHeader>
          <div className="flex flex-col gap-4">
            <div className="flex flex-col gap-2">
              <Label htmlFor="proxy-name">名稱</Label>
              <Input id="proxy-name" placeholder="例如：香港出口 1" value={name} onChange={(e) => setName(e.target.value)} />
            </div>
            <div className="flex flex-col gap-2">
              <Label htmlFor="proxy-url">地址</Label>
              <Input
                id="proxy-url"
                placeholder="http://host:port 或 socks5://host:port"
                autoComplete="off"
                value={url}
                onChange={(e) => setUrl(e.target.value)}
              />
            </div>
            <p className="text-xs text-muted-foreground">僅接受 HTTP、HTTPS、SOCKS4、SOCKS5 代理地址，可包含 user:pass 認證。</p>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setModalOpen(false)}>取消</Button>
            <Button disabled={saving} onClick={() => void save()}>儲存</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {confirmElement}
    </div>
  )
}
