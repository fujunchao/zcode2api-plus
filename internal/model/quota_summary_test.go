package model

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"
)

func TestSummarizeModelQuotas(t *testing.T) {
	archived := 1.0
	accounts := []*Account{
		{Quota: map[string]map[string]any{
			"体验":    {"model": " GLM_5.3 ", "total": 100, "used": 30, "remaining": 70},
			"活动":    {"model": "glm--5.3", "total": "50", "used": json.Number("10"), "remaining": 40.0},
			"Flash": {"model": "GLM-5.3-Flash", "total": 1000, "used": 100, "remaining": 900},
			"未知":    {"model": "unknown", "total": 9999, "used": 0, "remaining": 9999},
		}},
		// 旧快照没有 model 字段，且不会因为暂停调度而丢失账面额度。
		{Status: StatusDisabled, Quota: map[string]map[string]any{
			"GLM-5.3": {"total": 200, "used": 100, "remaining": 100},
		}},
		{ArchivedAt: &archived, Quota: map[string]map[string]any{
			"GLM-5.3": {"total": 9999, "used": 0, "remaining": 9999},
		}},
		nil,
	}
	before, _ := json.Marshal(accounts)
	got := SummarizeModelQuotas(accounts, []string{"glm-5.3-flash", "GLM-5.3"})
	if len(got) != 2 || got[0].Model != "glm-5.3-flash" || got[1].Model != "GLM-5.3" {
		t.Fatalf("模型清单与顺序不符: %+v", got)
	}
	for i, want := range [][5]float64{{1000, 100, 900, 1, 1}, {350, 140, 210, 2, 3}} {
		q := got[i]
		if q.Total == nil || q.Used == nil || q.Remaining == nil {
			t.Fatalf("模型 %s 缺少汇总值: %+v", q.Model, q)
		}
		values := [5]float64{*q.Total, *q.Used, *q.Remaining, float64(q.Accounts), float64(q.Items)}
		if values != want || q.Partial {
			t.Errorf("模型 %s 汇总=%v, partial=%v，期望 %v", q.Model, values, q.Partial, want)
		}
	}
	after, _ := json.Marshal(accounts)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("汇总不得修改账号快照")
	}
}

func TestSummarizeModelQuotasMissingAndZero(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries map[string]map[string]any
		items   int
		partial bool
		want    *float64
	}{
		{name: "无快照"},
		{name: "其他模型不混入", entries: map[string]map[string]any{"GLM-5.3-Flash": {"remaining": 10}}},
		{name: "缺字段", entries: map[string]map[string]any{"GLM-5.3": {}}, items: 1, partial: true},
		{name: "确认用尽", entries: map[string]map[string]any{"GLM-5.3": {"total": 100, "used": 100, "remaining": 0}}, items: 1, want: floatPtr(0)},
		{name: "部分已知", entries: map[string]map[string]any{
			"GLM-5.3": {"total": 100, "used": 20, "remaining": 80},
			"额外套餐":    {"model": "GLM-5.3", "total": nil, "used": nil, "remaining": nil},
		}, items: 2, partial: true, want: floatPtr(80)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SummarizeModelQuotas([]*Account{{Quota: tc.entries}}, []string{"GLM-5.3"})[0]
			if got.Items != tc.items || got.Partial != tc.partial || !reflect.DeepEqual(got.Remaining, tc.want) {
				t.Fatalf("汇总不符: %+v，期望 items=%d partial=%v remaining=%v", got, tc.items, tc.partial, tc.want)
			}
			if tc.items == 0 && (got.Total != nil || got.Used != nil || got.Accounts != 0) {
				t.Fatalf("未知额度不能伪装成零: %+v", got)
			}
		})
	}
}

func TestSummarizeModelQuotasInvalidNumbers(t *testing.T) {
	for _, raw := range []any{nil, "", "bad", "NaN", "Inf", math.NaN(), math.Inf(1), -1, true, []any{1}} {
		got := SummarizeModelQuotas([]*Account{{Quota: map[string]map[string]any{
			"GLM-5.3": {"total": raw, "used": 0, "remaining": 5},
		}}}, []string{"GLM-5.3"})[0]
		if got.Total != nil || !got.Partial || got.Used == nil || *got.Used != 0 || got.Remaining == nil || *got.Remaining != 5 {
			t.Errorf("非法值 %#v 不应污染其他字段: %+v", raw, got)
		}
		if _, err := json.Marshal(got); err != nil {
			t.Fatalf("汇总必须可以 JSON 编码: %v", err)
		}
	}
}

func floatPtr(value float64) *float64 { return &value }
