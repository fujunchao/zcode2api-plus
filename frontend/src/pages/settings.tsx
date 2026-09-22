/* 系統設定頁：後台密碼、網關 API Key、額度刷新間隔與使用說明 */
import { useEffect, useState, type FormEvent } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Loader2 } from 'lucide-react'
import { toast } from 'sonner'
import { Card, CardContent } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { adminKey } from '@/lib/admin-key'
import { api, errMsg } from '@/lib/api'
import type { SettingsResponse } from '@/lib/types'

export function SettingsPage() {
  const qc = useQueryClient()
  const { data } = useQuery({
    queryKey: ['settings'],
    queryFn: () => api<SettingsResponse>('GET', '/settings'),
  })

  const [adminKeyInput, setAdminKeyInput] = useState('')
  const [gatewayKey, setGatewayKey] = useState('')
  const [quotaInterval, setQuotaInterval] = useState('60')
  const [showKeys, setShowKeys] = useState(false)
  const [saving, setSaving] = useState(false)

  /* ── 套餐自動領取 ── */
  const [claimAuto, setClaimAuto] = useState(true)
  const [claimSchedule, setClaimSchedule] = useState(false)
  const [claimTime, setClaimTime] = useState('23:00')
  const [claimCaptchaCooldown, setClaimCaptchaCooldown] = useState('3600')
  const [claimRetryCooldown, setClaimRetryCooldown] = useState('600')
  const [claimPreviewCooldown, setClaimPreviewCooldown] = useState('60')
  const [savingClaim, setSavingClaim] = useState(false)

  /* ── 線路自動巡檢 ── */
  const [proxyHealth, setProxyHealth] = useState(true)
  const [proxyHealthInterval, setProxyHealthInterval] = useState('30')
  const [savingProxyHealth, setSavingProxyHealth] = useState(false)

  /* ── 風控冷卻（上游 405 + 風控文案） ── */
  const [riskCoolingSteps, setRiskCoolingSteps] = useState('300,900,3600')
  const [savingRiskCooling, setSavingRiskCooling] = useState(false)

  /* ── 上游 503 冷卻 ── */
  const [upstream503Steps, setUpstream503Steps] = useState('30,60,120')
  const [saving503Steps, setSaving503Steps] = useState(false)

  /* ── Async 強制直連（排障控制開關） ── */
  const [asyncForceDirect, setAsyncForceDirect] = useState(false)
  const [savingAsyncForceDirect, setSavingAsyncForceDirect] = useState(false)

  /* 載入完成後填入表單（僅在尚未編輯時同步） */
  useEffect(() => {
    if (!data) return
    setAdminKeyInput(data.admin_key || '')
    setGatewayKey(data.gateway_key || '')
    setQuotaInterval(String(data.quota_refresh_interval ?? 60))
    setClaimAuto(data.claim_auto_enabled)
    setClaimSchedule(data.claim_schedule_enabled)
    setClaimTime(data.claim_schedule_time || '23:00')
    setClaimCaptchaCooldown(String(data.claim_captcha_cooldown ?? 3600))
    setClaimRetryCooldown(String(data.claim_retry_cooldown ?? 600))
    setClaimPreviewCooldown(String(data.claim_preview_cooldown ?? 60))
    setProxyHealth(data.proxy_health_enabled)
    setProxyHealthInterval(String(data.proxy_health_interval ?? 30))
    setRiskCoolingSteps(data.risk_cooling_steps || '300,900,3600')
    setUpstream503Steps(data.upstream_503_cooling_steps || '30,60,120')
    setAsyncForceDirect(data.async_force_direct === true)
  }, [data])

  async function save(e: FormEvent) {
    e.preventDefault()
    if (!adminKeyInput.trim()) {
      toast.error('後台密碼不能為空')
      return
    }
    if (!gatewayKey.trim()) {
      toast.error('網關 API Key 不能為空')
      return
    }
    const interval = parseInt(quotaInterval, 10)
    if (isNaN(interval) || interval < 0) {
      toast.error('刷新間隔必須是非負整數')
      return
    }
    setSaving(true)
    try {
      await api('PUT', '/settings', {
        admin_key: adminKeyInput.trim(),
        gateway_key: gatewayKey.trim(),
        quota_refresh_interval: interval,
      })
      /* 同步本機儲存的密鑰，避免改密後被登出 */
      await adminKey.set(adminKeyInput.trim())
      toast.success('已儲存')
    } catch (err) {
      toast.error('儲存失敗：' + errMsg(err))
    } finally {
      setSaving(false)
    }
  }

  /* 套餐自動領取：独立表单，只提交领取相关字段 */
  async function saveClaim(e: FormEvent) {
    e.preventDefault()
    if (!/^\d{2}:\d{2}$/.test(claimTime)) {
      toast.error('定時時間需為 HH:MM')
      return
    }
    const nums: [string, string][] = [
      ['驗證碼冷卻', claimCaptchaCooldown],
      ['失敗冷卻', claimRetryCooldown],
      ['刷新節流', claimPreviewCooldown],
    ]
    for (const [label, v] of nums) {
      if (isNaN(parseInt(v, 10)) || parseInt(v, 10) < 0) {
        toast.error(label + '必須是非負整數')
        return
      }
    }
    setSavingClaim(true)
    try {
      await api('PUT', '/settings', {
        claim_auto_enabled: claimAuto,
        claim_schedule_enabled: claimSchedule,
        claim_schedule_time: claimTime,
        claim_captcha_cooldown: parseInt(claimCaptchaCooldown, 10),
        claim_retry_cooldown: parseInt(claimRetryCooldown, 10),
        claim_preview_cooldown: parseInt(claimPreviewCooldown, 10),
      })
      toast.success('已儲存')
      void qc.invalidateQueries({ queryKey: ['settings'] })
    } catch (err) {
      toast.error('儲存失敗：' + errMsg(err))
    } finally {
      setSavingClaim(false)
    }
  }

  /* 線路自動巡檢：独立表单，只提交巡检相关字段 */
  async function saveProxyHealth(e: FormEvent) {
    e.preventDefault()
    const interval = parseInt(proxyHealthInterval, 10)
    if (isNaN(interval) || interval < 1) {
      toast.error('巡檢間隔必須是 ≥1 的整數（分鐘）')
      return
    }
    setSavingProxyHealth(true)
    try {
      await api('PUT', '/settings', {
        proxy_health_enabled: proxyHealth,
        proxy_health_interval: interval,
      })
      toast.success('已儲存')
      void qc.invalidateQueries({ queryKey: ['settings'] })
    } catch (err) {
      toast.error('儲存失敗：' + errMsg(err))
    } finally {
      setSavingProxyHealth(false)
    }
  }

  /* 風控冷卻階梯：独立表单，只提交这一个字段。
     校验必须严格（整串都是 ≥1 的整数）——档位数同时是升级点，静默丢一档会给出一个
     管理员自己都不知道有几档的阶梯。 */
  async function saveRiskCooling(e: FormEvent) {
    e.preventDefault()
    const steps = riskCoolingSteps
      .split(',')
      .map((s) => s.trim())
      .filter((s) => s !== '')
    if (steps.length === 0 || steps.some((s) => !/^\d+$/.test(s) || parseInt(s, 10) < 1)) {
      toast.error('階梯需為逗號分隔的正整數秒，如 300,900,3600')
      return
    }
    setSavingRiskCooling(true)
    try {
      await api('PUT', '/settings', { risk_cooling_steps: steps.join(',') })
      toast.success('已儲存')
      void qc.invalidateQueries({ queryKey: ['settings'] })
    } catch (err) {
      toast.error('儲存失敗：' + errMsg(err))
    } finally {
      setSavingRiskCooling(false)
    }
  }

  /* 上游 503 冷卻階梯：独立表单，只提交这一个字段。校验与風控階梯同一套严格规则。 */
  async function saveUpstream503Steps(e: FormEvent) {
    e.preventDefault()
    const steps = upstream503Steps
      .split(',')
      .map((s) => s.trim())
      .filter((s) => s !== '')
    if (steps.length === 0 || steps.some((s) => !/^\d+$/.test(s) || parseInt(s, 10) < 1)) {
      toast.error('階梯需為逗號分隔的正整數秒，如 30,60,120')
      return
    }
    setSaving503Steps(true)
    try {
      await api('PUT', '/settings', { upstream_503_cooling_steps: steps.join(',') })
      toast.success('已儲存')
      void qc.invalidateQueries({ queryKey: ['settings'] })
    } catch (err) {
      toast.error('儲存失敗：' + errMsg(err))
    } finally {
      setSaving503Steps(false)
    }
  }

  /* Async 強制直連：獨立表單，只提交這一個開關。 */
  async function saveAsyncForceDirect(e: FormEvent) {
    e.preventDefault()
    setSavingAsyncForceDirect(true)
    try {
      await api('PUT', '/settings', { async_force_direct: asyncForceDirect })
      toast.success('已儲存')
      void qc.invalidateQueries({ queryKey: ['settings'] })
    } catch (err) {
      toast.error('儲存失敗：' + errMsg(err))
    } finally {
      setSavingAsyncForceDirect(false)
    }
  }

  return (
    <div className="mx-auto flex w-full max-w-3xl flex-col gap-6">
      {/* 頁首 */}
      <div>
        <h1 className="text-xl font-semibold tracking-tight">系統設定</h1>
        <p className="text-sm text-muted-foreground">後台鑑權密鑰與網關存取控制</p>
      </div>

      {/* 鑑權設定 */}
      <Card>
        <CardContent className="flex flex-col gap-5">
          <div className="text-sm font-semibold">鑑權</div>
          <form className="flex flex-col gap-5" onSubmit={save}>
            <div className="flex flex-col gap-2">
              <Label htmlFor="set-admin-key">後台密碼</Label>
              <div className="text-xs text-muted-foreground">用於登入此管理後台。修改後需用新密碼重新登入。</div>
              <Input
                id="set-admin-key"
                type={showKeys ? 'text' : 'password'}
                value={adminKeyInput}
                onChange={(e) => setAdminKeyInput(e.target.value)}
              />
            </div>
            <div className="flex flex-col gap-2">
              <Label htmlFor="set-gateway-key">網關 API Key</Label>
              <div className="text-xs text-muted-foreground">
                一律必填（fail-closed）：呼叫 <code className="rounded bg-muted px-1">/v1/messages</code>、
                <code className="rounded bg-muted px-1">/async/v1/*</code>、
                <code className="rounded bg-muted px-1">/v1/models</code> 須攜帶{' '}
                <code className="rounded bg-muted px-1">Authorization: Bearer &lt;key&gt;</code> 或{' '}
                <code className="rounded bg-muted px-1">x-api-key</code>。留空儲存會被拒絕。
              </div>
              <Input
                id="set-gateway-key"
                type={showKeys ? 'text' : 'password'}
                value={gatewayKey}
                onChange={(e) => setGatewayKey(e.target.value)}
              />
            </div>
            <label className="flex w-fit cursor-pointer items-center gap-2 text-xs text-muted-foreground">
              <Checkbox checked={showKeys} onCheckedChange={(v) => setShowKeys(v === true)} />
              顯示密鑰明文
            </label>
            <div className="flex flex-col gap-2">
              <Label htmlFor="set-quota-interval">額度刷新間隔（秒）</Label>
              <div className="text-xs text-muted-foreground">
                後台自動刷新各帳號額度與狀態的週期。設為 0 關閉自動刷新（仍可手動刷新）。修改後即時生效。
              </div>
              <Input
                id="set-quota-interval"
                type="number"
                min={0}
                step={5}
                value={quotaInterval}
                onChange={(e) => setQuotaInterval(e.target.value)}
              />
            </div>
            <div className="flex justify-end">
              <Button type="submit" disabled={saving}>
                {saving ? <Loader2 className="animate-spin" /> : null}
                儲存
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>

      {/* 套餐自動領取 */}
      <Card>
        <CardContent className="flex flex-col gap-5">
          <div className="text-sm font-semibold">套餐自動領取</div>
          <form className="flex flex-col gap-5" onSubmit={saveClaim}>
            <label className="flex cursor-pointer items-start gap-2 text-sm">
              <Checkbox checked={claimAuto} onCheckedChange={(v) => setClaimAuto(v === true)} />
              <span>
                入池自動領取
                <span className="block text-xs text-muted-foreground">
                  批量添加 / OAuth 登錄入池後，自動領取一次全部可領活動套餐。
                </span>
              </span>
            </label>
            <label className="flex cursor-pointer items-start gap-2 text-sm">
              <Checkbox checked={claimSchedule} onCheckedChange={(v) => setClaimSchedule(v === true)} />
              <span>
                每日定時領取
                <span className="block text-xs text-muted-foreground">
                  到指定時間對池內全部 JWT 帳號領取一次（本地時區）。尊重上游給的下次可領時間，
                  剛領過的帳號會跳過；進程不在運行時錯過不補跑。改動即時生效。
                </span>
              </span>
            </label>
            <div className="flex flex-col gap-2">
              <Label htmlFor="set-claim-time">定時領取時間</Label>
              <Input
                id="set-claim-time"
                type="time"
                className="w-40"
                value={claimTime}
                onChange={(e) => setClaimTime(e.target.value)}
              />
            </div>
            <div className="grid gap-4 sm:grid-cols-3">
              <div className="flex flex-col gap-2">
                <Label htmlFor="set-claim-captcha">驗證碼冷卻（秒）</Label>
                <div className="text-xs text-muted-foreground">
                  驗證碼不可用、或已領過但上游未給時間時的冷卻。
                </div>
                <Input
                  id="set-claim-captcha"
                  type="number"
                  min={60}
                  value={claimCaptchaCooldown}
                  onChange={(e) => setClaimCaptchaCooldown(e.target.value)}
                />
              </div>
              <div className="flex flex-col gap-2">
                <Label htmlFor="set-claim-retry">失敗冷卻（秒）</Label>
                <div className="text-xs text-muted-foreground">其他領取失敗（網路等）後的冷卻。</div>
                <Input
                  id="set-claim-retry"
                  type="number"
                  min={30}
                  value={claimRetryCooldown}
                  onChange={(e) => setClaimRetryCooldown(e.target.value)}
                />
              </div>
              <div className="flex flex-col gap-2">
                <Label htmlFor="set-claim-preview">刷新節流（秒）</Label>
                <div className="text-xs text-muted-foreground">「刷新資格」探測的節流，0 關閉。</div>
                <Input
                  id="set-claim-preview"
                  type="number"
                  min={0}
                  value={claimPreviewCooldown}
                  onChange={(e) => setClaimPreviewCooldown(e.target.value)}
                />
              </div>
            </div>
            <p className="text-xs text-muted-foreground">
              冷卻與開關只約束自動路徑；帳號頁的手動「領取套餐」按鈕永遠可用。
            </p>
            <div className="flex justify-end">
              <Button type="submit" disabled={savingClaim}>
                {savingClaim ? <Loader2 className="animate-spin" /> : null}
                儲存
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>

      {/* 線路自動巡檢 */}
      <Card>
        <CardContent className="flex flex-col gap-5">
          <div className="text-sm font-semibold">線路自動巡檢</div>
          <form className="flex flex-col gap-5" onSubmit={saveProxyHealth}>
            <label className="flex cursor-pointer items-start gap-2 text-sm">
              <Checkbox checked={proxyHealth} onCheckedChange={(v) => setProxyHealth(v === true)} />
              <span>
                自動巡檢代理線路
                <span className="block text-xs text-muted-foreground">
                  每隔一段時間對全部已啟用線路做 z.ai 可達性檢測（與手動「全部測試」同口徑）。
                  檢測不通過的線路會被<strong>自動移除</strong>，其綁定帳號按「空閒線路優先 →
                  綁定數最少 → 直連兜底」自動改派。關閉後線路只由人工管理。
                </span>
              </span>
            </label>
            <div className="flex flex-col gap-2">
              <Label htmlFor="set-proxy-health-interval">巡檢間隔（分鐘）</Label>
              <div className="text-xs text-muted-foreground">
                最小 1 分鐘。若某一輪全部線路失敗且直連也不可达，會判定為本機網路故障而跳過移除，
                不會清空線路池；停用線路不參與巡檢。
              </div>
              <Input
                id="set-proxy-health-interval"
                type="number"
                min={1}
                className="w-40"
                value={proxyHealthInterval}
                onChange={(e) => setProxyHealthInterval(e.target.value)}
              />
            </div>
            <p className="text-xs text-muted-foreground">
              改動即時生效（間隔從下一輪起按新值計）；每輪的移除與改派都會寫入日誌（前綴 proxy-health）。
            </p>
            <div className="flex justify-end">
              <Button type="submit" disabled={savingProxyHealth}>
                {savingProxyHealth ? <Loader2 className="animate-spin" /> : null}
                儲存
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>

      {/* 風控冷卻 */}
      <Card>
        <CardContent className="flex flex-col gap-5">
          <div className="text-sm font-semibold">風控冷卻</div>
          <form className="flex flex-col gap-5" onSubmit={saveRiskCooling}>
            <div className="flex flex-col gap-2">
              <Label htmlFor="set-risk-cooling-steps">冷卻階梯（秒，逗號分隔）</Label>
              <div className="text-xs text-muted-foreground">
                上游用 HTTP 405 + <code>unusual activity</code> 表示風控攔截。它看的是身分維度
                （帳號、裝置指紋、出口 IP、請求標頭），與請求的哪個模型無關，所以處置是
                <strong>停整個帳號</strong>——換模型照樣被攔。連續第 N 次命中取第 N 檔；
                <strong>連續次數超過檔位數</strong>則帳號直接置為失效，需人工介入。
              </div>
              <Input
                id="set-risk-cooling-steps"
                className="w-64"
                placeholder="300,900,3600"
                value={riskCoolingSteps}
                onChange={(e) => setRiskCoolingSteps(e.target.value)}
              />
            </div>
            <p className="text-xs text-muted-foreground">
              預設 <span className="tabular-nums">300,900,3600</span>（5／15／60 分鐘）。
              <strong>檔位數就是升級點</strong>：想多給帳號一次自證機會就多加一檔，想更早封禁就減一檔。
              冷卻期內該帳號不參與調度、也不做套餐領取；冷卻到期後成功調用一次即回到最低檔。
              單檔上限 7 天。判定同時看 body，因此「缺少 system 注入」這類我方請求缺陷不會被誤判成風控。
            </p>
            <div className="flex justify-end">
              <Button type="submit" disabled={savingRiskCooling}>
                {savingRiskCooling ? <Loader2 className="animate-spin" /> : null}
                儲存
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>

      {/* 上游 503 冷卻 */}
      <Card>
        <CardContent className="flex flex-col gap-5">
          <div className="text-sm font-semibold">上游 503 冷卻</div>
          <form className="flex flex-col gap-5" onSubmit={saveUpstream503Steps}>
            <div className="flex flex-col gap-2">
              <Label htmlFor="set-503-steps">冷卻階梯（秒，逗號分隔）</Label>
              <div className="text-xs text-muted-foreground">
                上游 503（服務不可用）是<strong>上游健康信號</strong>而非帳號問題。連續第 N 次收到
                503 取第 N 檔冷卻該帳號；<strong>超過檔位數封頂</strong>於固定冷卻秒數（預設 300s），
                不會像風控那樣升級為失效。帳號成功調用一次後計數歸零。
              </div>
              <Input
                id="set-503-steps"
                className="w-64"
                placeholder="30,60,120"
                value={upstream503Steps}
                onChange={(e) => setUpstream503Steps(e.target.value)}
              />
            </div>
            <p className="text-xs text-muted-foreground">
              預設 <span className="tabular-nums">30,60,120</span>：上游瞬時抖動秒級退避即可吸收，
              避免一次抖動把整池帳號清空（2026-09-20 事故）；持續不可用的帳號逐級加重至封頂。
              日誌與 503「無可用帳號」錯誤詳情均按此口徑展示各帳號冷卻成因與最早恢復時間。
              單檔上限 7 天。
            </p>
            <div className="flex justify-end">
              <Button type="submit" disabled={saving503Steps}>
                {saving503Steps ? <Loader2 className="animate-spin" /> : null}
                儲存
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>

      {/* Async 強制直連 */}
      <Card>
        <CardContent className="flex flex-col gap-5">
          <div className="text-sm font-semibold">Async 強制直連</div>
          <form className="flex flex-col gap-5" onSubmit={saveAsyncForceDirect}>
            <label className="flex cursor-pointer items-start gap-2 text-sm">
              <Checkbox
                checked={asyncForceDirect}
                onCheckedChange={(v) => setAsyncForceDirect(v === true)}
              />
              <span>
                忽略帳號代理，async 請求恆直連上游
                <span className="block text-xs text-muted-foreground">
                  僅在排障取證時開啟：關閉時 async 與網關一致，走該帳號綁定的線路；
                  開啟後這條路徑會以<strong>伺服器真實 IP</strong> 直連上游——IP 綁定、
                  地區要求、避開風控全部失效。改動即時生效。
                </span>
              </span>
            </label>
            <p className="text-xs text-muted-foreground">
              這個開關的用途是保留「線路側 vs 上游側」斷流歸因的<strong>對照組</strong>：
              拿網關（走線路）與 async（直連）對比，就能判斷固定時長牆屬於出口線路
              還是上游模型側。診斷行的 <code>route=</code> 讀數會如實跟隨本開關
              （直連時報 <code>direct</code>，否則報線路名）。
            </p>
            <div className="flex justify-end">
              <Button type="submit" disabled={savingAsyncForceDirect}>
                {savingAsyncForceDirect ? <Loader2 className="animate-spin" /> : null}
                儲存
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>

      {/* 使用說明 */}
      <Card>
        <CardContent className="flex flex-col gap-3">
          <div className="text-sm font-semibold">使用說明</div>
          <ul className="list-disc space-y-1.5 pl-5 text-sm leading-relaxed text-muted-foreground marker:text-muted-foreground/60">
            <li>在「帳號池」貼上 Coding Plan JWT 或 API Key 即可加入輪詢。</li>
            <li>請求按 round-robin 分發；某帳號額度用完會自動切到下一個帳號。</li>
            <li>帳號額度、狀態在「帳號池」頁即時刷新展示。</li>
            <li>
              對話端點：<code className="rounded bg-muted px-1 py-0.5 font-mono text-xs">{location.origin}/v1/messages</code>
              （相容 Anthropic Messages 協議）。
            </li>
          </ul>
        </CardContent>
      </Card>
    </div>
  )
}
