package store

import "zcode2api/internal/model"

type startPlanProxyProof struct {
	Observation model.StartPlanObservation
	Route       ProxyEgress
}

// RecordStartPlanProxyObservation 给真实额度查询附加受控的运行期代次证明。
// 额度观察本来就不跨重启；每个账号最多保留四份，避免等待领取期间无限积累。
// 查询结果返回后才调用，但 route 必须在该次真实 client.Do 前冻结。
func (s *Store) RecordStartPlanProxyObservation(expected *model.Account, route ProxyEgress, observation model.StartPlanObservation) bool {
	if expected == nil || observation.ProxyID == "" || !observation.Matches(expected) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.proxyDispatchCurrentLocked(expected, route) {
		return false
	}
	if s.strictStartPlanProofs == nil {
		s.strictStartPlanProofs = map[string][]startPlanProxyProof{}
	}
	proofs := s.strictStartPlanProofs[expected.ID]
	for _, proof := range proofs {
		if proof.Observation == observation {
			return true // 缓存/合并查询不得把同一次 HTTP 算作第二次证据。
		}
	}
	proofs = append(proofs, startPlanProxyProof{Observation: observation, Route: route})
	if len(proofs) > 4 {
		proofs = proofs[len(proofs)-4:]
	}
	s.strictStartPlanProofs[expected.ID] = proofs
	return true
}

func (s *Store) startPlanProxyProofCurrentLocked(expected *model.Account, observation model.StartPlanObservation, route ProxyEgress) bool {
	for _, proof := range s.strictStartPlanProofs[expected.ID] {
		if proof.Observation == observation && proof.Route.credentialRef == route.credentialRef &&
			proof.Route.identity == route.identity && proof.Route.Generation == route.Generation &&
			proof.Route.RouteGeneration == route.RouteGeneration && s.proxyDispatchCurrentLocked(expected, proof.Route) {
			return true
		}
	}
	return false
}
