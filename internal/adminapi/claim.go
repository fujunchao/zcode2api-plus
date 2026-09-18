// 套餐领取端点（/admin/api/claim/*）：对应 Python 版 admin_api.py 的
// claim_preview / claim_plans，以及入池自动领取触发点（批量添加 / OAuth）。
//
// 本轮补齐三处（借鉴 zcode-switch 的 autoClaim 设计）：
//   - 领取状态落盘：含上游给的「下次可领时间」，重启不再把刚领过的账号重领一遍；
//   - 冷却分档：只拦自动路径，手动触发始终可强制；
//   - 单槽串行：批量导入多个账号时不再同时轰验证码池。
package adminapi

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"zcode2api/internal/claim"
	"zcode2api/internal/model"
	"zcode2api/internal/store"
	"zcode2api/internal/web"
)

// autoClaimTasks 强引用持有后台自动领取任务（对齐 Python _auto_claim_tasks）。
var autoClaimTasks sync.WaitGroup

// claimGate 容量固定的串行闸门：非阻塞抢占，抢不到即放弃。
type claimGate chan struct{}

func newClaimGate() claimGate { return make(claimGate, 1) }

// acquire 非阻塞抢占闸门。抢不到返回 false：入池触发的领取本轮放弃即可，
// 把 goroutine 堆在这儿等与并发并无区别，只是换了个地方排队。
func (g claimGate) acquire() bool {
	select {
	case g <- struct{}{}:
		return true
	default:
		return false
	}
}

func (g claimGate) release() { <-g }

// claimSlot 领取串行闸门。上游按账号维度做验证码与领取风控，并发领取只会互相
// 挤兑 captcha 池——批量导入 20 个账号时尤其明显。
var claimSlot = newClaimGate()

// previewGate 「刷新资格」的只读节流。内存态即可：重启后重新探测本就是期望行为。
var (
	previewGateMu sync.Mutex
	previewGateAt = map[string]time.Time{}
)

// jwtAccounts 全部/指定 ID 的 JWT 账号（对齐 Python _jwt_accounts）。
func (h *Handler) jwtAccounts(ids []string) []*model.Account {
	wanted := map[string]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	var out []*model.Account
	for _, acc := range h.Store.ListAccounts(model.ProviderZai) {
		if len(wanted) > 0 && !wanted[acc.ID] {
			continue
		}
		// 已归档账号不参与批量领取（显式指定单个账号时仍允许，便于排查）
		if acc.ArchivedAt != nil && len(wanted) == 0 {
			continue
		}
		if acc.Mode == "jwt" && acc.JWTToken != nil && *acc.JWTToken != "" {
			out = append(out, acc)
		}
	}
	return out
}

// ── 冷却与状态落盘 ──────────────────────────────────────────────────────────

// claimCooldownActive 自动路径是否仍在冷却中。
// 只拦自动路径：手动点按钮不受限——用户点了没反应是最糟的交互。
func claimCooldownActive(acc *model.Account, now time.Time) bool {
	next := acc.ClaimNextAt()
	return next != nil && float64(now.Unix()) < *next
}

// claimCooldownUntil 计算下次可领时间。优先用上游给的 ends_at——那是它自己算好的
// 节奏，比在本地拍一个时长更准；没有时才按失败成因分档（对齐 zcode-switch）：
// 验证码类与「已领过但上游没给时间」取长档，其余取短档。
// 两档时长来自后台设置（回退 config 默认值），改完即生效。
func claimCooldownUntil(outcomes []map[string]any, now time.Time, captchaSec, retrySec int) *float64 {
	long := float64(now.Unix()) + float64(captchaSec)
	short := float64(now.Unix()) + float64(retrySec)
	for _, o := range outcomes {
		if next, ok := numberOf(o["next_at"]); ok && next > 0 {
			v := next
			return &v
		}
	}
	for _, o := range outcomes {
		if captcha, _ := o["captcha"].(bool); captcha {
			return &long
		}
		if code, ok := numberOf(o["code"]); ok && (code == 1005 || code == 1003 || code == 3007) {
			return &long
		}
	}
	return &short
}

// applyClaimOutcome 把一次领取结果写回账号（含上游给的下次可领时间）并落库。
//
// acc 是**领取用的账号副本**，不能拿它整体回写（领取期间 store 可能已改过该账号，
// 例如删除线路改派了 ProxyURL；整体回写会用副本的旧值把它覆盖回去）。因此只把
// 领取状态这一个字段经 UpdateClaimState 落到 store 的当前对象上；读取当前状态
// 与替换也在该方法的锁内完成，避免与另一路领取任务互相覆盖。
// ClaimState 视为不可变：基于副本生成新对象、整体替换。
func (h *Handler) applyClaimOutcome(acc *model.Account, outcomes []map[string]any, now time.Time) {
	captchaSec, retrySec, _ := h.Store.ClaimCooldowns()
	nextAt := claimCooldownUntil(outcomes, now, captchaSec, retrySec)
	succeeded := false
	for _, o := range outcomes {
		if ok, _ := o["ok"].(bool); ok {
			succeeded = true
			break
		}
	}
	if _, err := h.Store.UpdateClaimState(acc.Provider, acc.ID, func(cur *model.ClaimState) *model.ClaimState {
		next := *cur
		next.NextAt = nextAt
		if succeeded {
			ts := float64(now.Unix())
			next.ClaimedAt = &ts
			next.LastError = nil
		} else if len(outcomes) > 0 {
			if msg, _ := outcomes[0]["message"].(string); msg != "" {
				next.LastError = &msg
			}
		}
		return &next
	}); err != nil {
		web.Warn("claim", "领取状态落库失败: "+err.Error())
	}
}

// numberOf 宽松取数值（JSON 解析一律 float64，代码内构造的可能是 int）。
func numberOf(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// previewCooling 只读探测是否仍在节流窗口内；窗口时长来自后台设置。
func previewCooling(id string, now time.Time, previewSec int) bool {
	if previewSec <= 0 {
		return false
	}
	previewGateMu.Lock()
	defer previewGateMu.Unlock()
	last, ok := previewGateAt[id]
	return ok && now.Sub(last) < time.Duration(previewSec)*time.Second
}

// markPreview 记录本次探测时间。
func markPreview(id string, now time.Time) {
	previewGateMu.Lock()
	defer previewGateMu.Unlock()
	previewGateAt[id] = now
}

// ── 触发点 ──────────────────────────────────────────────────────────────────

// scheduleAutoClaim 入池后后台自动领取（fire-and-forget；对齐 _schedule_auto_claim）。
// 受后台「入池自動領取」开关约束——刻意只拦自动路径，手动按钮永远可用；
// 冷却中直接跳过：上游已在响应里给出下次可领时间，到点前重试只是白打一次上游。
func (h *Handler) scheduleAutoClaim(acc *model.Account) {
	// 只取走用得到的标量：后台 goroutine 不该再碰 store 的内部对象（见 claimUnderGate）。
	name := acc.Name
	if !h.Store.ClaimAutoEnabled() {
		web.Ok("claim", fmt.Sprintf("账号 %s 已关闭入池自动领取，跳过", name))
		return
	}
	if claimCooldownActive(acc, time.Now()) {
		web.Ok("claim", fmt.Sprintf("账号 %s 仍在领取冷却期，跳过自动领取", name))
		return
	}
	autoClaimTasks.Add(1)
	go func() {
		defer autoClaimTasks.Done()
		defer func() {
			if r := recover(); r != nil {
				web.Warn("claim", "自动领取任务异常（已兜底）")
			}
		}()
		if !claimSlot.acquire() {
			web.Ok("claim", fmt.Sprintf("账号 %s 已有领取任务在跑，本轮跳过", name))
			return
		}
		h.claimUnderGate(acc)
	}()
}

// claimUnderGate 在已持有串行闸门的前提下领取单个账号并落盘结果。
// 调用方负责抢占闸门，释放由这里的 defer 兜底；返回是否至少领到一个套餐。
func (h *Handler) claimUnderGate(acc *model.Account) (succeeded bool) {
	defer claimSlot.release()
	// 领取可能持续数十秒，期间后台仍在改账号（删除线路会改派 ProxyURL/ProxyID、
	// 额度刷新会改 Status/Quota）。这里先取一份副本再开工——直接拿 store 的内部
	// 指针会让整个领取过程与那些写入相撞（CI 的 -race 实测到过）。
	// 副本上的改动不会进 store，故结果由 applyClaimOutcome 按 ID 回写。
	snap := h.Store.SnapshotAccount(acc.Provider, acc.ID)
	if snap == nil {
		// 账号已被删除：不是错误，本轮无事可做。
		return false
	}
	svc := claim.NewService(h.Captcha)
	outcomes := svc.AutoClaimAllPlans(snap)
	h.applyClaimOutcome(snap, outcomes, time.Now())
	for _, o := range outcomes {
		if ok, _ := o["ok"].(bool); ok {
			return true
		}
	}
	return false
}

// ── 每日定时领取 ────────────────────────────────────────────────────────────

// claimScheduleTick 定时调度的轮询间隔：每轮实时读设置，改完在该间隔内生效。
const claimScheduleTick = 30 * time.Second

// claimSlotWait 定时批量抢占闸门的等待上限。一天一次的批量宁可排队慢一点，
// 也不该把账号静默丢掉（与入池路径"抢不到即放弃"刻意不同）。
const claimSlotWait = 30 * time.Second

// claimBatchGap 批量内相邻账号的间隔，避免把上游打得太密。
const claimBatchGap = time.Second

// ClaimScheduler 每日定时领取：到设置页指定的时间点（本地时区）对池内全部
// JWT 账号领取一次。受「每日定時領取」开关约束；尊重账号的 claim.next_at
// ——那是上游自己给的节奏，刚领过的账号会被跳过。
type ClaimScheduler struct {
	h     *Handler
	start sync.Once
	stop  chan struct{}
	done  chan struct{}

	lastFiredMu sync.Mutex
	lastFired   string // 已触发过的日期（YYYY-MM-DD），防同一天重复触发
}

// NewClaimScheduler 创建定时领取调度器（调用 Start 启动）。
func NewClaimScheduler(h *Handler) *ClaimScheduler {
	return &ClaimScheduler{h: h, stop: make(chan struct{}), done: make(chan struct{})}
}

// Start 启动调度循环；重复调用无操作。
func (s *ClaimScheduler) Start() {
	s.start.Do(func() { go s.loop() })
}

// Stop 停止调度循环并等待退出。正在进行的单个账号领取会先跑完（与额度监控一致）。
func (s *ClaimScheduler) Stop() {
	close(s.stop)
	<-s.done
}

// shouldFireClaim 判定本次 tick 是否应当触发（纯函数，便于单测）。
// 开关关闭或目标时间非法 → false；今天已触发过 → false；
// 本地 HH:MM 恰好等于目标 → true。
func shouldFireClaim(now time.Time, lastFired, target string, enabled bool) bool {
	if !enabled || !store.ValidClaimScheduleTime(target) {
		return false
	}
	if lastFired == now.Format("2006-01-02") {
		return false
	}
	return now.Format("15:04") == target
}

// loop 调度循环：每 claimScheduleTick 醒一次，实时读设置（改完即生效），
// 到点触发一次批量并用日期去重。
func (s *ClaimScheduler) loop() {
	defer close(s.done)
	// 启动先避让 5s，让监听与账号加载先跑完（对齐额度监控）。
	select {
	case <-s.stop:
		return
	case <-time.After(5 * time.Second):
	}
	for {
		now := time.Now()
		s.lastFiredMu.Lock()
		last := s.lastFired
		s.lastFiredMu.Unlock()
		if shouldFireClaim(now, last, s.h.Store.ClaimScheduleTime(), s.h.Store.ClaimScheduleEnabled()) {
			s.lastFiredMu.Lock()
			s.lastFired = now.Format("2006-01-02")
			s.lastFiredMu.Unlock()
			// 批量内再兜一层 recover：单个账号的异常不该终结调度循环。
			func() {
				defer func() {
					if r := recover(); r != nil {
						web.Warn("claim", "定时领取任务异常（已兜底）")
					}
				}()
				s.h.runScheduledClaims(now)
			}()
		}
		select {
		case <-s.stop:
			return
		case <-time.After(claimScheduleTick):
		}
	}
}

// runScheduledClaims 执行一次定时批量：对池内全部 JWT 账号（非归档）逐个领取。
// 刻意的取舍：
//   - 尊重 claim.next_at（上游自己给的下次可领时间），刚领过的账号直接跳过；
//   - 抢闸门限时等待而非丢账号——一天一次的批量，慢点没关系；
//   - 错过不补跑：23:00 时进程没在跑就等下一天，避免用户无感知的补打上游。
func (h *Handler) runScheduledClaims(now time.Time) {
	accounts := h.jwtAccounts(nil)
	if len(accounts) == 0 {
		web.Ok("claim", "定时领取：池内无 JWT 账号，跳过")
		return
	}
	var okCount, failCount, coolCount, busyCount int
	for _, acc := range accounts {
		if claimCooldownActive(acc, now) {
			coolCount++
			continue
		}
		select {
		case claimSlot <- struct{}{}:
		case <-time.After(claimSlotWait):
			busyCount++
			web.Warn("claim", fmt.Sprintf("账号 %s 等待领取闸门超时，本轮跳过", acc.Name))
			continue
		}
		if h.claimUnderGate(acc) {
			okCount++
		} else {
			failCount++
		}
		time.Sleep(claimBatchGap)
	}
	web.Ok("claim", fmt.Sprintf(
		"定时领取完成：成功 %d，失败 %d，冷却跳过 %d，闸门占用跳过 %d",
		okCount, failCount, coolCount, busyCount))
}

// handleClaimPreview GET /admin/api/claim/preview?account_id=
// 立即拉取可领取套餐（全部/单个 JWT 账号）；先上报激活事件（失败不阻断）。
func (h *Handler) handleClaimPreview(w http.ResponseWriter, r *http.Request) {
	var ids []string
	if v := strings.TrimSpace(r.URL.Query().Get("account_id")); v != "" {
		ids = []string{v}
	}
	svc := claim.NewService(h.Captcha)
	now := time.Now()
	_, _, previewSec := h.Store.ClaimCooldowns()
	out := []map[string]any{}
	for _, acc := range h.jwtAccounts(ids) {
		if !acc.IsSelectable(now) && acc.Status == model.StatusCooling {
			out = append(out, map[string]any{
				"account_id": acc.ID, "account_name": acc.Name, "plans": []any{},
				"error":     "賬號冷卻中（風控/限流），已跳過上游查詢",
				"activated": false, "activation_error": nil,
			})
			continue
		}
		// 这是纯只读探测，但每次都要打两次上游（激活上报 + preview），
		// 页面反复加载时会累积成无谓流量，故加一层短节流。
		if previewCooling(acc.ID, now, previewSec) {
			out = append(out, map[string]any{
				"account_id": acc.ID, "account_name": acc.Name, "plans": []any{},
				"error":     "刷新過於頻繁，請稍後再試",
				"activated": false, "activation_error": nil,
			})
			continue
		}
		markPreview(acc.ID, now)
		activationError := claim.ReportActivationEvents(acc)
		activated := activationError == ""
		entry := map[string]any{
			"account_id": acc.ID, "account_name": acc.Name,
			"plans": []any{}, "error": nil,
			"activated": activated, "activation_error": nil,
		}
		if activationError != "" {
			entry["activation_error"] = activationError
		}
		plans, err := svc.PreviewPlans(acc)
		if err != nil {
			entry["error"] = err.Error()
		} else {
			entry["plans"] = plans
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"preview": out})
}

// handleClaim POST /admin/api/claim（body 可选 account_ids / plan_id）
// 缺省对全部 JWT 账号自动选最优套餐；冷却账号跳过（上游写流量，风控期不加剧）。
//
// 与自动路径的区别：不检查冷却（手动即强制），但仍走同一个串行闸门。
func (h *Handler) handleClaim(w http.ResponseWriter, r *http.Request) {
	payload, apiErr := decodeBody(r)
	if apiErr != nil {
		payload = map[string]any{} // Python Body(default=None)：空体全量领取
	}
	var ids []string
	if raw, ok := payload["account_ids"].([]any); ok {
		for _, item := range raw {
			if s, isStr := item.(string); isStr && s != "" {
				ids = append(ids, s)
			}
		}
	}
	planID := strings.TrimSpace(strOf(payload["plan_id"]))

	candidates := h.jwtAccounts(ids)
	outcomes := []map[string]any{}
	for _, acc := range candidates {
		if acc.Status == model.StatusCooling {
			outcomes = append(outcomes, map[string]any{
				"account_id": acc.ID, "account_name": acc.Name, "ok": false,
				"message": "賬號冷卻中（風控/限流），已跳過領取",
			})
			continue
		}
		// 领取前补一次当日活跃上报：上游以「官方客户端当日活跃」为活动套餐的投放
		// 资格，而手动领取不像「刷新资格」那样经过 handleClaimPreview。漏掉这一步
		// 时 preview 会直接返回空套餐（对照 zcode-switch：claim_refresh 必先上报）。
		// 放在闸门之外——它不改账号状态、无需串行；失败也不阻断，上游按
		// device_mid + 日期去重，同一账号重复上报无害。
		if activationError := claim.ReportActivationEvents(acc); activationError != "" {
			web.Warn("claim", fmt.Sprintf("账号 %s 激活上报失败: %s", acc.Name, activationError))
		}
		if !claimSlot.acquire() {
			outcomes = append(outcomes, map[string]any{
				"account_id": acc.ID, "account_name": acc.Name, "ok": false,
				"message": "已有領取任務在執行，請稍後重試",
			})
			continue
		}
		svc := claim.NewService(h.Captcha)
		result, err := func() (map[string]any, error) {
			defer claimSlot.release() // 领取本身串行，之后的额度刷新不必占着闸门
			return svc.Claim(acc, planID)
		}()
		now := time.Now()
		if err != nil {
			web.Warn("claim", "账号 "+acc.Name+" 领取失败: "+err.Error())
			outcome := claim.FailureOutcome(acc, planID, err)
			outcomes = append(outcomes, outcome)
			h.applyClaimOutcome(acc, []map[string]any{outcome}, now)
			continue
		}
		h.Quota.RefreshAccounts([]*model.Account{acc})
		outcome := map[string]any{
			"account_id": acc.ID, "account_name": acc.Name, "ok": true,
		}
		for k, v := range result {
			outcome[k] = v
		}
		outcomes = append(outcomes, outcome)
		h.applyClaimOutcome(acc, []map[string]any{outcome}, now)
	}
	ok := 0
	for _, o := range outcomes {
		if b, isBool := o["ok"].(bool); isBool && b {
			ok++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"outcomes": outcomes,
		"summary":  map[string]any{"ok": ok, "fail": len(outcomes) - ok},
	})
}
