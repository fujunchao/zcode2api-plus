package model

import "crypto/sha256"

// StartPlanObservation 是有效额度响应的运行期证据，不作为数据库契约字段。
// 同时绑定出口与凭据，避免旧线路/旧令牌的空额度被拿来淘汰新线路。
// 重启后归零，必须先重新查询额度；不能从 LastCheckedAt 推断查询成功。
type StartPlanObservation struct {
	CheckedAt      float64
	Missing        bool
	ProxyID        string
	ProxyURL       string
	CredentialHash [32]byte
}

func NewStartPlanObservation(a *Account, checkedAt float64, missing bool) StartPlanObservation {
	o := StartPlanObservation{CheckedAt: checkedAt, Missing: missing, CredentialHash: sha256.Sum256([]byte(a.Secret()))}
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

// MissingStartPlanEvidence 限定在尚未获初始额度的账号；已成功使用或领取过的账号
// 不因后来额度为空而淘汰代理。只是订阅已耗尽也不属于未获配额。
func (a *Account) MissingStartPlanEvidence() bool {
	if a == nil || !a.StartPlanObservation.Missing || !a.StartPlanObservation.Matches(a) ||
		a.UseCount > 0 || len(a.Plans) > 0 || len(a.Plan) > 0 {
		return false
	}
	state := a.ClaimView()
	return state == nil || state.ClaimedAt == nil
}
