/* 對照後端 public_view（app/models.py）與 /admin/api 回應的型別定義 */

export interface QuotaWindow {
  total?: number | null
  used?: number | null
  remaining?: number | null
  available?: number | null
  period?: string | null
  period_start?: number | string | null
  period_end?: number | string | null
  expires_at?: number | string | null
  model: string
  plan_name: string
  plan_is_trial: boolean
}

export interface TotalTokens {
  input: number
  output: number
  cache_creation: number
  cache_read: number
}

export type AccountStatus = 'active' | 'exhausted' | 'cooling' | 'invalid' | 'disabled'

export interface Account {
  id: string
  name: string
  email: string | null
  provider: string
  mode: 'jwt' | 'apiKey'
  token_masked: string
  enabled: boolean
  status: AccountStatus
  quota: Record<string, QuotaWindow>
  exhausted_models: string[]
  disabled_models: string[]
  plan: Record<string, unknown>
  plans: Record<string, unknown>[]
  plan_name: string
  plan_is_trial: boolean
  use_count: number
  fail_count: number
  total_tokens: TotalTokens
  last_used_at: number | null
  last_checked_at: number | null
  cooling_until: number | null
  last_error: string | null
  /** 最近一次失敗的歸類（後端 model.ErrorKind* 的穩定字串）。 */
  last_error_kind: AccountErrorKind | null
  /** 最近一次失敗的時間（Unix 秒）。 */
  last_error_at: number | null
  proxy_url: string | null
  proxy_id: string | null
  created_at: number
  archived_at: number | null
  user_id: string | null
  virtual_device_mid: string | null
  claim: ClaimState | null
}

/** 套餐领取状态（后端 model.ClaimState）。 */
export interface ClaimState {
  claimed_at: number | null
  /** 下次可领时间（上游给的 ends_at，或按失败成因推算）。 */
  next_at: number | null
  last_error: string | null
}

export interface Stats {
  total: number
  active: number
  exhausted: number
  cooling: number
  invalid: number
  disabled: number
  calls: number
  fail: number
  tokens_in: number
  tokens_out: number
  tokens_cache: number
}

export interface ProxyProfile {
  id: string
  name: string
  url: string
  enabled: boolean
}

export interface AccountsResponse {
  accounts: Account[]
  stats: Stats
  providers: string[]
  models: string[]
  proxies: ProxyProfile[]
  ts: number
}

export interface StatusResponse {
  providers: string[]
  gateway_key_set: boolean
  quota_refresh_interval: number
  quota_pool: Record<string, number>
}

export interface SettingsResponse {
  admin_key: string
  gateway_key: string
  quota_refresh_interval: number
  claim_auto_enabled: boolean
  claim_schedule_enabled: boolean
  claim_schedule_time: string
  claim_captcha_cooldown: number
  claim_retry_cooldown: number
  claim_preview_cooldown: number
  proxy_health_enabled: boolean
  proxy_health_interval: number
  /* 風控冷卻階梯：逗號分隔的秒數。檔位數同時是升級點——連續命中超過檔位數則帳號失效 */
  risk_cooling_steps: string
  /* 上游 503 冷卻階梯：逗號分隔的秒數。超過檔位數封頂於固定冷卻秒數，不升級為失效 */
  upstream_503_cooling_steps: string
  /* Async 強制直連：排障開關，開啟後 async 池忽略帳號代理、恆直連上游 */
  async_force_direct: boolean
}

/* z.ai 側的單個探測目標（主站 / 備援站） */
export interface UpstreamTarget {
  url: string
  host: string
  ok: boolean
  status?: number
  ms?: number
  blocked?: boolean
  error?: string
}

/* z.ai 側可達性：探測上游真實入口，回答「這條線路能不能真的用來跑 z.ai」。
   與 ip/asn 那組欄位互相獨立——出口查詢站通了不代表 z.ai 認這個出口。 */
export interface UpstreamProbe {
  ok: boolean
  blocked: boolean
  ms?: number
  error?: string
  targets?: UpstreamTarget[]
}

/* 探測結果：只回答一個問題——經這條線路能不能連上 z.ai。
   出口 IP / ASN 不再查詢，它們證明不了 z.ai 認不認這個出口。 */
export interface ProbeResult {
  ok?: boolean
  error?: string
  upstream?: UpstreamProbe
}

/* 一鍵測試全部線路的單條結果：探測結論 + 該線路的標識。 */
export interface ProxyTestResult extends ProbeResult {
  id: string
  name: string
  ok: boolean
}

export interface TestAllResponse {
  results: ProxyTestResult[]
  summary: { total: number; ok: number; fail: number }
}

export interface UsageRankingRow {
  name: string
  provider: string
  requests: number
  errors: number
  tokens: number
}

export interface UsageResponse {
  ts: number
  window: string
  summary: Stats
  ranking: UsageRankingRow[]
}

export interface LoginStartResponse {
  flow_id: string
  authorize_url: string
}

export interface LoginCompleteResponse {
  status: 'ready' | 'failed' | 'pending'
  message?: string
  account?: Account
}

export interface CaptchaConfig {
  sceneId?: string
  region?: string
  prefix?: string
  enabled?: boolean
}

export interface RefreshSummary {
  ok: number
  fail: number
}

export const STATUS_LABEL: Record<AccountStatus, string> = {
  active: '正常',
  exhausted: '用完',
  cooling: '限流',
  invalid: '異常',
  disabled: '停用',
}

/**
 * 帳號最近一次錯誤的歸類。取值是後端 `model.ErrorKind*` 的穩定字串，
 * 一字不可改（會落進 accounts.data，並成為篩選值）。
 */
export type AccountErrorKind =
  | 'upstream_overload'
  | 'rate_limited'
  | 'quota_exhausted'
  | 'model_busy'
  | 'auth_failed'
  | 'connection_failed'
  | 'upstream_unavailable'
  | 'captcha_failed'
  | 'invalid_response'
  | 'client_canceled'
  | 'upstream_error'
  | 'quota_query_failed'
  | 'risk_control'

/** 錯誤類型標籤（繁體）。改文案只動這裡，不動上面的取值。 */
export const ERROR_KIND_LABEL: Record<AccountErrorKind, string> = {
  upstream_overload: '上游過載',
  rate_limited: '上游限流',
  quota_exhausted: '額度耗盡',
  model_busy: '併發受限',
  auth_failed: '憑證異常',
  connection_failed: '連線失敗',
  upstream_unavailable: '上游不可用',
  captcha_failed: '驗證碼失敗',
  invalid_response: '回應無效',
  client_canceled: '用戶端取消',
  upstream_error: '上游錯誤',
  quota_query_failed: '額度查詢失敗',
  risk_control: '風控攔截',
}

/**
 * 錯誤類型配色。與 STATUS_BADGE 同一套語義：
 * 紅＝疑似帳號自身問題，琥珀＝暫時受限，灰＝與帳號健康無關。
 */
export const ERROR_KIND_BADGE: Record<AccountErrorKind, string> = {
  upstream_overload: 'bg-muted text-muted-foreground',
  rate_limited: 'bg-amber-100 text-amber-700',
  quota_exhausted: 'bg-purple-100 text-purple-700',
  model_busy: 'bg-muted text-muted-foreground',
  auth_failed: 'bg-red-100 text-red-700',
  connection_failed: 'bg-red-100 text-red-700',
  upstream_unavailable: 'bg-red-100 text-red-700',
  captcha_failed: 'bg-amber-100 text-amber-700',
  invalid_response: 'bg-red-100 text-red-700',
  client_canceled: 'bg-muted text-muted-foreground',
  upstream_error: 'bg-red-100 text-red-700',
  quota_query_failed: 'bg-muted text-muted-foreground',
  risk_control: 'bg-red-100 text-red-700',
}

/**
 * 是否歸因於帳號自身（後端 `model.ErrorKindAccountFault` 的前端鏡像）。
 *
 * 判據是「換個帳號是否可能成功」：憑證異常／連不上／上游 503／回應無效算故障；
 * 平台過載、額度耗盡、併發受限、驗證碼、用戶端取消、額度輪詢失敗都不算——
 * 否則平台一過載，「帳號故障」視圖會被整池帳號刷滿。
 */
export const ERROR_KIND_IS_FAULT: Record<AccountErrorKind, boolean> = {
  upstream_overload: false,
  rate_limited: true,
  quota_exhausted: false,
  model_busy: false,
  auth_failed: true,
  connection_failed: true,
  upstream_unavailable: true,
  captcha_failed: false,
  invalid_response: true,
  client_canceled: false,
  upstream_error: true,
  quota_query_failed: false,
  risk_control: true,
}

/** 全部錯誤類型，順序與後端 `model.ErrorKindAll` 一致（下拉選單用）。 */
export const ERROR_KIND_ALL: AccountErrorKind[] = [
  'upstream_overload',
  'rate_limited',
  'quota_exhausted',
  'model_busy',
  'auth_failed',
  'connection_failed',
  'upstream_unavailable',
  'captcha_failed',
  'invalid_response',
  'client_canceled',
  'upstream_error',
  'quota_query_failed',
  'risk_control',
]

/* 儀表板用的完整狀態標籤與配色 */
export const STATUS_LABEL_LONG: Record<AccountStatus, string> = {
  active: '正常',
  exhausted: '額度用完',
  cooling: '冷卻中',
  invalid: '憑證異常',
  disabled: '已停用',
}

export const STATUS_COLOR: Record<AccountStatus, string> = {
  active: '#22c55e',
  exhausted: '#a855f7',
  cooling: '#f59e0b',
  invalid: '#ef4444',
  disabled: '#94a3b8',
}
