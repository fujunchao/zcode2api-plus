package store

import (
	"time"
	"zcode2api/internal/model"
)

// ProxyObservation 仅接收真实模型 HTTP 尝试的去敏归因；Complete 表示完整成功。
type ProxyObservation struct {
	RequestID, TargetOrigin, Model, ContentRef, ProfileRef, SessionRef, DeviceRef, BusinessCode string
	Status                                                                                      int
	Complete                                                                                    bool
	At                                                                                          time.Time
}

// ProxyDecision 描述严格规则的已提交删除结果，而不是供应商因果确认。
type ProxyDecision struct {
	Removed []string
	Reason  string
}

func (s *Store) ObserveProxyOutcome(acc *model.Account, egress ProxyEgress, o ProxyObservation) (ProxyDecision, error) {
	return ProxyDecision{Reason: "unconfirmed"}, nil
}

func (s *Store) AcquireProxyDispatch(acc *model.Account, egress ProxyEgress) (func(), error) {
	return func() {}, nil
}
