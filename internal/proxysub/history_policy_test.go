package proxysub

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// 合成复现用户看到的 211 条一次成功 + 110 条多次成功、全部已有 27 小时的分布。
func TestSubscriptionHistoryDoesNotReject321Candidates(t *testing.T) {
	now := time.Unix(1791898949, 0)
	rows := []any{}
	for i := range 321 {
		item := fixture(fmt.Sprintf("http://8.8.8.8:%d", 10000+i), now.Add(-27*time.Hour))
		if i < 211 {
			item["pass_streak"] = 1
		}
		rows = append(rows, item)
	}
	raw, _ := json.Marshal(rows)
	for _, minutes := range []int{5, 120, 1440} {
		c := DefaultConfig()
		c.MaxAgeMinutes = minutes
		got, report, err := Parse(raw, c, now)
		if err != nil || len(got) != 321 || report.Rejected != 0 || report.Expired != 0 {
			t.Fatalf("旧配置 %d 分钟不能继续过滤历史：候选=%d，拒绝=%d，过期=%d，err=%v", minutes, len(got), report.Rejected, report.Expired, err)
		}
	}
}

func TestSubscriptionHistoryMetadataIsOptional(t *testing.T) {
	now := time.Now()
	for _, patch := range []func(map[string]any){
		func(x map[string]any) { delete(x, "last_ok"); delete(x, "pass_streak") },
		func(x map[string]any) {
			x["last_ok"] = "unavailable"
			x["pass_streak"] = map[string]any{"unknown": true}
		},
		func(x map[string]any) { x["last_ok"] = now.Add(time.Hour).UnixMilli(); x["pass_streak"] = -1 },
	} {
		row := fixture("http://8.8.8.8:8080", now)
		patch(row)
		raw, _ := json.Marshal([]any{row})
		got, report, err := Parse(raw, DefaultConfig(), now)
		if err != nil || len(got) != 1 || report.Rejected != 0 || report.Expired != 0 {
			t.Fatalf("参考信息不能当成硬门槛：候选=%d report=%+v err=%v", len(got), report, err)
		}
		if got[0].LastOK != 0 || got[0].Streak != 0 {
			t.Fatal("异常历史应降为未知，不能取得额外排序优势")
		}
	}
}

func TestSubscriptionHistoryOnlyOrdersCandidates(t *testing.T) {
	now := time.Now()
	recent := fixture("http://8.8.8.8:8080", now.Add(-time.Hour))
	recent["pass_streak"] = 1
	repeated := fixture("http://8.8.8.8:8081", now.Add(-time.Hour))
	repeated["pass_streak"] = 3
	old := fixture("http://8.8.8.8:8082", now.Add(-48*time.Hour))
	old["pass_streak"] = 1
	future := fixture("http://8.8.8.8:8083", now.Add(time.Hour))
	future["pass_streak"] = 999
	raw, _ := json.Marshal([]any{future, old, recent, repeated})
	got, _, err := Parse(raw, DefaultConfig(), now)
	if err != nil || len(got) != 4 {
		t.Fatalf("候选被历史过滤: %v", err)
	}
	for i, want := range []string{"http://8.8.8.8:8081", "http://8.8.8.8:8080", "http://8.8.8.8:8082", "http://8.8.8.8:8083"} {
		if got[i].URL != want {
			t.Fatalf("排序 %d: %s != %s", i, got[i].URL, want)
		}
	}
}
