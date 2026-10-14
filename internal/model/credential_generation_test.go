package model

import (
	"encoding/json"
	"testing"
)

func TestRequestCredentialGenerationIsInternalAndClonePreservesIt(t *testing.T) {
	a := Create(ProviderZai, "合成账号", "synthetic-key")
	before, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	a.CredentialGeneration = 7
	copy := a.Clone()
	if copy.CredentialGeneration != 7 || !a.SameCredential(copy) {
		t.Fatal("请求快照必须保留内部凭据代次")
	}
	after, err := json.Marshal(a)
	if err != nil || string(after) != string(before) {
		t.Fatalf("内部代次不能改变已有 Account JSON 契约：%v", err)
	}
	copy.CredentialGeneration++
	if a.SameCredential(copy) {
		t.Fatal("相同凭据字节不代表同一请求代次")
	}
	legacy, err := FromJSON(after)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.CredentialGeneration != 0 || a.SameCredential(legacy) {
		t.Fatal("未被 Store 赋代次的 JSON 不能冒充已冻结的请求快照")
	}
}

func TestStartPlanObservationIncludesCredentialGeneration(t *testing.T) {
	a := Create(ProviderZai, "合成账号", "synthetic-key")
	a.CredentialGeneration = 1
	before := NewStartPlanObservation(a, 123, true)
	next := a.Clone()
	next.CredentialGeneration = 3
	if before.Matches(next) {
		t.Fatal("额度合并/缓存身份不能在凭据 A→B→A 后仍命中旧结果")
	}
	if !before.Matches(a.Clone()) {
		t.Fatal("同代副本仍应复用有效额度结果")
	}
}
