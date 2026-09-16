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
