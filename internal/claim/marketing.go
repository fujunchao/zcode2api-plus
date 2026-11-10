// 营销触达（marketing/touch）：优惠套餐（start plan 等）的**真实投放与领取入口**。
//
// 2026-11-10 现场核实（docs/analysis-claim-marketing-touch-20261110.md）：
// billing/preview 现在恒返回 {"code":0,"msg":"","logid":...}（没有 data.plans），
// 官方客户端自己的 getManualClaimPlanPreviews 同样拿不到套餐并抛
// manual_claim_preview_missing_data；可领套餐号只在营销触达的 delivery 里：
//
//	GET /api/v1/marketing/touch?seq=<n>
//	  headers: Authorization / X-Device-Mid / X-Client-Language / X-ZCode-App-Version
//	  data.deliveries[].banner|popup.buttons[].action{type:"claim_zcode_plan", args:{plan_id}}
//	  delivery.campaign_id 用于领取成功后的回执
//
// 领取成功后的回执（官方在动作成功之后才发，失败只 warn、不阻断）：
//
//	POST /api/v1/marketing/touch/action {"campaign_id":..., "action_type":"confirm"}
//
// 与 billing 同一出站口径：都走 Service.clientFor(acc)（账号线路 / 初始化护栏），
// 真实客户端的 event/report、marketing/touch 与 billing 永远同 IP。
package claim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/web"
)

// MarketingBaseURL 营销触达端点基址（默认与上游 origin 同源）。
//
// 与 EventReportURL 同一约定：测试覆写指向 mock，避免用例打真实上游。
var MarketingBaseURL = "https://zcode.z.ai/api/v1"

// 营销触达子路径。
const (
	marketingTouchPath  = "/marketing/touch"
	marketingActionPath = "/marketing/touch/action"
)

// claimPlanActionType delivery 按钮里「领取 ZCode 套餐」动作的类型。
const claimPlanActionType = "claim_zcode_plan"

// marketingTimeout 官方 marketing/touch 与 claimManualPlan 的 timeoutMs=15e3。
// 领取任务串行执行，这条链路不能比计费端点更耐等。
const marketingTimeout = 15 * time.Second

// marketingSeq 进程内自增查询序号。官方 Xde=0 后置自增（首调 seq=0），
// 上游用它做同账号连续查询的去重/归因，跳号或固定值都是可识别的差异。
var marketingSeq atomic.Uint64

// MarketingPlan 营销触达里的一条可领套餐。
type MarketingPlan struct {
	PlanID     string
	Name       string
	Priority   float64
	CampaignID string
	Grants     []any
}

// marketingHeaders 营销触达请求头。官方这一路**只有四项**：
// 视身份携带的 Authorization，以及设备/语言/版本三项。
// 刻意不带 billing 的来源头（X-Title / X-Platform / X-Os-* 等）与 Content-Type。
//
// X-Device-Mid 必须是合法 UUID：官方对 deviceMid 有 uuid() 断言，实测非 UUID
// （如 "n/a"）会得到 HTTP 400 / code 3001 parameter error，静默拿不到 delivery。
func marketingHeaders(acc *model.Account) map[string]string {
	headers := map[string]string{
		"X-Device-Mid":        acc.DeviceMidOr(config.DeviceMid()),
		"X-Client-Language":   config.ZcodeClientLanguage,
		"X-ZCode-App-Version": config.ZcodeClientVersion,
	}
	if acc.Mode == "jwt" && acc.JWTToken != nil && *acc.JWTToken != "" {
		headers["Authorization"] = "Bearer " + *acc.JWTToken
	}
	return headers
}

// marketingRequest 营销触达统一出站：账号线路 + 15s 上限 + 不跟随重定向。
// 返回解析后的 JSON 体；HTTP 非 2xx、非 JSON、业务码非 0 都算失败，由调用方决定回退。
func (s *Service) marketingRequest(acc *model.Account, method, path string, headers map[string]string, payload []byte) (map[string]any, error) {
	ctx := s.context()
	var cancel context.CancelFunc
	if ctx.Err() == nil {
		ctx, cancel = context.WithTimeout(ctx, marketingTimeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, method, MarketingBaseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("营销触达请求构造失败: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client, err := s.clientFor(acc)
	if err != nil {
		return nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("营销触达网络错误: %w", err)
	}
	defer res.Body.Close()
	raw, readErr := io.ReadAll(res.Body)
	if readErr != nil {
		return nil, fmt.Errorf("营销触达响应读取失败: %w", readErr)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("营销触达 HTTP %d: %s", res.StatusCode, truncateStr(string(raw), 120))
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("营销触达响应非 JSON: %w", err)
	}
	if code := BusinessCode(body); code != 0 {
		return nil, fmt.Errorf("营销触达业务码异常(%d): %s", code, truncateStr(string(raw), 120))
	}
	return body, nil
}

// MarketingPlans 拉取账号当前可领的营销套餐。
// 无 delivery、字段缺失都返回空列表且不报错——「没有活动」不是故障。
func (s *Service) MarketingPlans(acc *model.Account) ([]MarketingPlan, error) {
	seq := marketingSeq.Add(1) - 1 // 官方后置自增：首调 0
	path := fmt.Sprintf("%s?seq=%d", marketingTouchPath, seq)
	body, err := s.marketingRequest(acc, http.MethodGet, path, marketingHeaders(acc), nil)
	if err != nil {
		return nil, err
	}
	data, _ := body["data"].(map[string]any)
	deliveries, _ := data["deliveries"].([]any)
	out := make([]MarketingPlan, 0, len(deliveries))
	for _, raw := range deliveries {
		delivery, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		campaignID, _ := delivery["campaign_id"].(string)
		meta := deliveryZcodePlan(delivery)
		for _, args := range deliveryClaimActions(delivery) {
			planID, _ := args["plan_id"].(string)
			if strings.TrimSpace(planID) == "" {
				continue
			}
			plan := MarketingPlan{PlanID: strings.TrimSpace(planID), CampaignID: campaignID}
			if meta != nil {
				if name, ok := meta["name"].(string); ok {
					plan.Name = name
				}
				plan.Priority = asFloat(meta["priority"])
				if entitlements, ok := meta["entitlements"].([]any); ok {
					plan.Grants = planGrants(entitlements)
				}
			}
			out = append(out, plan)
		}
	}
	return out, nil
}

// deliveryClaimActions 取 delivery 里所有「领取套餐」动作的 args。
//
// delivery 是按 resource_position 判别的联合：banner 形态与 popup 形态互斥，
// 按钮挂在各自的 banner.buttons / popup.buttons 上，因此两处都要看。
func deliveryClaimActions(delivery map[string]any) []map[string]any {
	var out []map[string]any
	for _, holder := range []string{"banner", "popup"} {
		node, ok := delivery[holder].(map[string]any)
		if !ok {
			continue
		}
		buttons, _ := node["buttons"].([]any)
		for _, raw := range buttons {
			button, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			action, _ := button["action"].(map[string]any)
			if action == nil {
				continue
			}
			if kind, _ := action["type"].(string); kind != claimPlanActionType {
				continue
			}
			if args, ok := action["args"].(map[string]any); ok {
				out = append(out, args)
			}
		}
	}
	return out
}

// deliveryZcodePlan 取投递里携带的套餐元数据（名称 / 优先级 / 权益）。
//
// 官方三处都可能挂 zcode_plan，逐处兜底：
//   - banner.background.args.zcode_plan（banner 背景包）
//   - banner.success_popup.hero.args.zcode_plan（banner 的成功弹窗）
//   - popup.hero.args.zcode_plan（popup 形态）
//
// 只取第一处命中即可：一个 delivery 只投一个活动、一个套餐。
func deliveryZcodePlan(delivery map[string]any) map[string]any {
	if banner, ok := delivery["banner"].(map[string]any); ok {
		if background, ok := banner["background"].(map[string]any); ok {
			if plan := zcodePlanArg(background); plan != nil {
				return plan
			}
		}
		if popup, ok := banner["success_popup"].(map[string]any); ok {
			if hero, ok := popup["hero"].(map[string]any); ok {
				if plan := zcodePlanArg(hero); plan != nil {
					return plan
				}
			}
		}
	}
	if popup, ok := delivery["popup"].(map[string]any); ok {
		if hero, ok := popup["hero"].(map[string]any); ok {
			if plan := zcodePlanArg(hero); plan != nil {
				return plan
			}
		}
	}
	return nil
}

// zcodePlanArg 取节点 args.zcode_plan。
func zcodePlanArg(node map[string]any) map[string]any {
	args, _ := node["args"].(map[string]any)
	if args == nil {
		return nil
	}
	plan, _ := args["zcode_plan"].(map[string]any)
	return plan
}

// marketingPlanView 把营销套餐转成与 ParsePlan 同形的可领套餐视图，
// 额外带 campaign_id 供领取成功后回执使用。
func marketingPlanView(plan MarketingPlan) map[string]any {
	grants := plan.Grants
	if grants == nil {
		grants = []any{}
	}
	name := plan.Name
	if name == "" {
		name = plan.PlanID
	}
	return map[string]any{
		"plan_id":     plan.PlanID,
		"name":        name,
		"description": "",
		"priority":    plan.Priority,
		"grants":      grants,
		"campaign_id": plan.CampaignID,
	}
}

// marketingPlansView 营销触达套餐列表（按优先级降序，与 preview 排序口径一致）。
func (s *Service) marketingPlansView(acc *model.Account) ([]map[string]any, error) {
	plans, err := s.MarketingPlans(acc)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(plans))
	for _, plan := range plans {
		out = append(out, marketingPlanView(plan))
	}
	sort.Slice(out, func(i, j int) bool {
		pi, pj := asFloat(out[i]["priority"]), asFloat(out[j]["priority"])
		if pi != pj {
			return pi > pj
		}
		return out[i]["plan_id"].(string) < out[j]["plan_id"].(string)
	})
	return out, nil
}

// MarketingNoDeliveryHint 无投递时的诊断串。
//
// 上游把两种完全不同的成因回成同一个响应（HTTP 200 + code 0 + `deliveries:[]`）：
// 「本账号确实没有活动」与「伪装身份漂移导致看不到活动」。2026-11-10 实测（claim-verify）：
// 缺 Authorization、缺 X-ZCode-App-Version、或版本号改成未知值（9.9.9），全部返回空投递；
// 缺 X-Device-Mid / 值非法则是 HTTP 400 + code 3001，反而更好认。
// 所以这条日志是区分「活动没投放」与「版本/凭据漂移」的唯一线索——别把它删了。
func MarketingNoDeliveryHint(acc *model.Account) string {
	credential := "无"
	if acc.Mode == "jwt" && acc.JWTToken != nil && *acc.JWTToken != "" {
		credential = "有"
	}
	return fmt.Sprintf("app_version=%s 凭据=%s", config.ZcodeClientVersion, credential)
}

// campaignForPlan 为「显式 plan_id」的领取补一次 campaign_id。
//
// 官方只在营销触达的 delivery 上拿到回执键；显式 plan_id 的调用（后台手动领取）
// 没有走发现阶段，这里按 plan_id 反查一次。查不到就返回空串——回执只是归因信息，
// 绝不能影响领取结论，也不能让领取失败。
func (s *Service) campaignForPlan(acc *model.Account, planID string) string {
	plans, err := s.MarketingPlans(acc)
	if err != nil {
		return ""
	}
	for _, plan := range plans {
		if plan.PlanID == planID {
			return plan.CampaignID
		}
	}
	return ""
}

// ReportMarketingClaim 领取成功后回报营销触达（action_type=confirm）。
// 官方在动作成功之后才回执，且回报失败只 warn。这里同样**绝不**影响领取结论：
// 失败只记日志——回执丢失最多让活动归因少一条，重新领取走的是 claim 幂等语义。
func (s *Service) ReportMarketingClaim(acc *model.Account, campaignID string) {
	if campaignID == "" {
		return
	}
	payload, err := json.Marshal(map[string]string{"campaign_id": campaignID, "action_type": "confirm"})
	if err != nil {
		return
	}
	headers := marketingHeaders(acc)
	headers["Content-Type"] = "application/json"
	if _, err := s.marketingRequest(acc, http.MethodPost, marketingActionPath, headers, payload); err != nil {
		web.Warn("claim", fmt.Sprintf("账号 %s 营销触达回执失败（不影响领取）: %v", acc.Name, err))
	}
}
