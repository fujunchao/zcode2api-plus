package modelprobe_test

import (
	"context"
	"testing"
	"time"
)

// 单点复检轮不得推进全量巡检定时器：否则活跃期（复检间隔数十秒）30 分钟全量
// 永远等不到，空闲线路上的坏点从未被扫到（2026-10-17 线上复盘 3.4 节）。
func TestNetworkFullRoundNotStarvedByRechecks(t *testing.T) {
	f := newNetworkFixture(t)
	f.blocked.Store(false)

	if err := f.m.QueueNetwork(""); err != nil {
		t.Fatal(err)
	}
	if worked, err := f.m.RunNetworkDue(context.Background()); err != nil || !worked {
		t.Fatalf("首轮全量巡检未执行：worked=%v err=%v", worked, err)
	}

	// 30 分钟间隔内连续做三次单点复检。
	for i := 0; i < 3; i++ {
		f.now = f.now.Add(5 * time.Minute)
		if err := f.m.QueueNetwork(f.bad.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.m.RunNetworkDue(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	// 越过原定间隔：即便刚做过复检，也必须重新起全量轮。
	f.now = f.now.Add(16 * time.Minute)
	worked, err := f.m.RunNetworkDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !worked {
		t.Fatal("单点复检不得顺延全量巡检（全量轮被饿死）")
	}
}
