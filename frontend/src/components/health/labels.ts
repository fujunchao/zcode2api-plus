import type { ModelProbeRun, ModelProbeSettings, ModelProbeSummary } from '@/lib/types'

/** 配置默认值：与后端 DefaultModelProbeSettings 对齐，仅作草稿初始值。 */
export const defaults: ModelProbeSettings = { enabled: false, mode: 'observe', model: 'glm-5.3-flash', interval_minutes: 30, concurrency: 1, timeout_seconds: 30, global_daily_limit: 200, account_daily_limit: 100, target_mode: 'auto', exclude_proxy_ids: [], account_mode: 'auto', account_ids: [], target_proxy_ids: [], reference_proxy_ids: [], egress_groups: {}, allow_direct_control: false, revision: 0 }

/** 巡检结论、运行状态与原因的稳定文案（取值见后端 modelprobe / store 的常量）。 */
export const labels: Record<string, string> = {
  unknown: '尚未检测', healthy_recent: '最近模型成功', suspect: '待核查', route_correlated: '线路相关证据成立',
  account_correlated: '更偏账号／身份相关', mixed: '有成功也有失败', inconclusive: '无法确认',
  queued: '等待执行', waiting: '等待对照', running: '正在检查', interrupted: '已中断',
  no_reference: '缺少可用参考线路', reference_unavailable: '参考组不可用，已停止扩散', no_eligible_account: '无符合条件的探测账号',
  reference_expired: '参考已过期，先重新确认参考线路', storage_error: '结果保存失败，已暂停新探测',
  awaiting_control: '等待账号冷却或下一组成对检查', diagnosis_rate_limit: '本目标一小时内已有诊断',
  account_not_ready: '账号仍受冷却、额度或停用限制', account_cooldown_exceeds_budget: '冷却超过本次诊断期限',
  budget_exhausted: '请求预算已用完', diagnosis_budget: '已到诊断次数或时间上限',
  evidence_incomplete_or_contradicted: '证据不足、过期或存在反证', qualified_observe: '已达标，观察模式未执行删除',
  retired: '已严格退役', decision_disabled: '自动决定门禁关闭', evidence_incomplete: '证据不完整，暂停删除',
  protected_proxy: '节点受保护，未自动删除', observe_only: '仅观察，未自动删除',
  stale_snapshot: '账号、线路或策略已变化，旧结果未处置', account_or_target_changed: '账号或目标版本变化',
  policy_changed: '策略已变更', process_restarted: '重启终止旧诊断', process_stopped: '进程关停', persist_failed: '处置保存失败，请检查严格恢复状态',
  risk_control: '405 风控响应', authentication_failed: '账号鉴权失败', quota_exhausted: '额度不足', rate_limited: '上游限流',
  captcha_required: '上游要求验证', incomplete_response: '响应未完整收到', invalid_response: '不是完整模型消息',
  connection_failed: '连接失败', timeout: '请求超时', canceled: '请求取消', invalid_request: '请求构造不满足契约',
  upstream_error: '其他上游错误', redirect_or_invalid_status: '重定向或异常状态', skipped_recent_success: '近期业务已成功，本轮免测', success: '完整模型成功',
  insufficient_probe_accounts: '可用探测账号不足两个，未执行删除',
  account_budget_exhausted: '等待账号巡检预算恢复', accounts_busy: '等待空闲探测账号',
  network_unavailable: '网络尚未通过检查，等待网络恢复',
}

/** 自动择优账号不可用的原因（后端 probeAccountEligibilityLocked）。 */
export const accountReasons: Record<string, string> = {
  missing: '账号已不存在', initialization_managed: '初始化／固定出口托管，不可借用', not_jwt: '不是 JWT 账号',
  not_selectable: '停用、归档、冷却或额度耗尽', risk_isolation: '风控静默隔离中', quota_unknown: '额度未知', quota_empty: '5.3f 额度已用完',
  account_budget_exhausted: '账号巡检预算已用完', business_busy: '正在处理业务，巡检暂避', probe_busy: '正在执行其他巡检',
}

export const numberFields = [
  { key: 'interval_minutes', label: '模型巡检间隔（分钟）', min: 1, max: 1440 },
  { key: 'concurrency', label: '并发检查数', min: 1, max: 2 },
  { key: 'timeout_seconds', label: '单次请求时限（秒）', min: 5, max: 60 },
  { key: 'global_daily_limit', label: '全局 24 小时请求预算', min: 1, max: 10000 },
  { key: 'account_daily_limit', label: '单账号 24 小时请求预算', min: 1, max: 10000 },
] as const

export const selectClass = 'w-full rounded-md border bg-background px-3 py-2 text-sm focus-visible:outline-ring'

export const time = (ms?: number) => ms ? new Date(ms).toLocaleString('zh-CN', { hour12: false }) : '—'

export const toggle = (values: string[], id: string, checked: boolean) => checked ? [...new Set([...values, id])] : values.filter((v) => v !== id)

export const sameIDs = (a: string[], b: string[]) => JSON.stringify([...a].sort()) === JSON.stringify([...b].sort())

export function describe(run?: ModelProbeRun) {
  if (!run) return '尚未检测'
  if (['queued', 'waiting', 'running', 'interrupted'].includes(run.state)) return labels[run.state]
  if (run.verdict === 'healthy_recent' && Date.now() - (run.observed_at || run.updated_at) > 30 * 60_000) return '成功记录已过期'
  return labels[run.verdict] ?? '无法确认'
}

export function confirmedUsage(run: ModelProbeRun) {
  const sum = run.samples.reduce((v, s) => ({ input: v.input + (s.usage?.Input ?? 0), output: v.output + (s.usage?.Output ?? 0), read: v.read + (s.usage?.CacheRead ?? 0), write: v.write + (s.usage?.CacheCreation ?? 0) }), { input: 0, output: 0, read: 0, write: 0 })
  return `已确认 token：输入 ${sum.input} / 输出 ${sum.output} / 缓存读 ${sum.read} / 缓存写 ${sum.write}`
}

/** 摘要行状态级别：色点与文案由它决定，判定只依赖 ModelProbeSummary 已有字段。 */
export type HealthLevel = 'error' | 'warning' | 'active' | 'ok' | 'off'

/**
 * 把后端 14 个字段压成一句话，回答「现在有没有问题」。
 * 优先级：存储失败 > 对照暂停 > 证据不完整 > 未启用 > 在途 > 待核查 > 正常。
 */
export function healthStatus(data: ModelProbeSummary, now = Date.now()): { level: HealthLevel; text: string } {
  if (data.storage_error) return { level: 'error', text: '巡检已暂停：结果保存失败' }
  if (data.paused_until > now) return { level: 'warning', text: '对照已暂停（参考组不可用）' }
  if ((data.evidence_incomplete_until ?? 0) > now) return { level: 'warning', text: '严格删除暂停（证据不完整）' }
  if (!data.settings.enabled && !(data.settings.network_enabled ?? false)) return { level: 'off', text: '未启用' }
  if (data.settings.network_enabled && (data.network?.failed ?? 0) > 0) return { level: 'warning', text: `${data.network!.failed} 条线路网络异常` }
  if (data.active > 0) return { level: 'active', text: `正在检查 ${data.active} 条线路` }
  const suspects = Math.max(data.failed_targets ?? 0, data.runs.filter((r) => r.verdict === 'suspect').length)
  if (suspects > 0) return { level: 'warning', text: `${suspects} 条线路待核查` }
  if (!data.settings.enabled) {
    const network = data.network
    return network && network.total > 0 && network.checked === network.total
      ? { level: 'ok', text: '网络正常（模型未启用）' }
      : { level: 'warning', text: '网络覆盖不足（模型未启用）' }
  }
  if (data.remaining_requests <= 0 || (data.waiting_budget ?? 0) > 0) return { level: 'warning', text: '等待巡检预算恢复' }
  const eligible = data.eligible_accounts ?? (data.accounts ?? []).filter((a) => a.eligible).length
  if (eligible === 0 || (data.waiting_accounts ?? 0) > 0) return { level: 'warning', text: '等待合格的空闲账号' }
  if ((data.queued ?? 0) > 0) return { level: 'active', text: `${data.queued} 条线路等待检查` }
  const total = targetCount(data)
  if (total === 0 || (data.checked_targets ?? 0) < total) return { level: 'warning', text: `主动覆盖不足 ${data.checked_targets ?? 0}/${total}` }
  if (data.settings.network_enabled && (!data.network || data.network.checked < data.network.total || data.network.total === 0)) return { level: 'warning', text: '网络覆盖不足' }
  if (data.fresh_healthy_targets !== undefined && data.fresh_healthy_targets < total) return { level: 'warning', text: '部分模型记录已过期，等待刷新' }
  return { level: 'ok', text: '正常' }
}

export const levelDot: Record<HealthLevel, string> = {
  error: 'bg-destructive',
  warning: 'bg-amber-500',
  active: 'bg-blue-500',
  ok: 'bg-emerald-500',
  off: 'bg-muted-foreground',
}

/** 单线检测周期（分钟）：后端未返回时回落到配置的间隔。 */
export const effectiveInterval = (data: ModelProbeSummary) => data.effective_interval_minutes ?? data.settings.interval_minutes

/** 受管线路总数：自动覆盖模式用后端计数，手工模式回落到所选数量。 */
export const targetCount = (data: ModelProbeSummary) => data.total_targets ?? data.settings.target_proxy_ids?.length ?? 0
