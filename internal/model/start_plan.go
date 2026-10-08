package model

import "crypto/sha256"

// StartPlanObservation 是有效额度响应的运行期证据，不作为数据库契约字段。
// 同时绑定出口与凭据，避免旧线路/旧令牌的空额度被拿来淘汰新线路。
// 重启后归零，必须先重新查询额度；不能从 LastCheckedAt 推断查询成功。
type StartPlanObservation struct {
	AccountID      string
	Provider       string
	CheckedAt      float64
	Missing        bool
	ProxyID        string
	ProxyURL       string
	CredentialHash [32]byte
}

func NewStartPlanObservation(a *Account, checkedAt float64, missing bool) StartPlanObservation {
	o := StartPlanObservation{AccountID: a.ID, Provider: a.Provider, CheckedAt: checkedAt,
		Missing: missing, CredentialHash: sha256.Sum256([]byte(a.Secret()))}
	if a.ProxyID != nil {
		o.ProxyID = *a.ProxyID
	}
	if a.ProxyURL != nil {
		o.ProxyURL = *a.ProxyURL
	}
	return o
}

func (o StartPlanObservation) Matches(a *Account) bool {
	if a == nil || o.CheckedAt <= 0 {
		return false
	}
	current := NewStartPlanObservation(a, o.CheckedAt, o.Missing)
	return o == current
}

// MissingStartPlanEvidence 表示查询确认尚未获初始额度，且没有历史使用/领取记录。
// 单份证据不能授权删除代理；淘汰只由新账号入池流程携带两次独立查询证据发起。
func (a *Account) MissingStartPlanEvidence() bool {
	if a == nil || !a.StartPlanObservation.Missing || !a.StartPlanObservation.Matches(a) ||
		a.UseCount > 0 || len(a.Plans) > 0 || len(a.Plan) > 0 {
		return false
	}
	state := a.ClaimView()
	return state == nil || state.ClaimedAt == nil
}
