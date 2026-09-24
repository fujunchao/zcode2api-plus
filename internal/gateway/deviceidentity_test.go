// 设备身份归属的端到端守卫：模型请求头里**不带** X-Device-Mid，指纹在 body 的
// metadata.user_id.device_id 里，且与 X-Session-Id 头共用同一个会话 id。
//
// 这两件事必须一起成立。只做前半件（删头）会让设备身份彻底消失，只做后半件
// （加 body）会同时留下一个官方不发的头 —— 两头都是上游可交叉比对的差异。
// 见 docs/analysis-client-golden-diff-20260924.md 与 upstream/metadata.go。
package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

// postWithHeaders 发一次 /v1/messages，并附加自定义入站头。
func (f *fixture) postWithHeaders(t *testing.T, body map[string]any, extra map[string]string) (int, string) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, f.srv.URL+"/v1/messages", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test")
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// TestDeviceIdentityTravelsInBodyNotHeaders 头里没有、body 里必须有，且是本账号的指纹。
func TestDeviceIdentityTravelsInBodyNotHeaders(t *testing.T) {
	f := newFixture(t)
	f.respond = jsonResp(200, okUpstreamJSON)
	acc, err := f.st.AddAccount(model.ProviderZai, "dev-1", "sk-abc")
	if err != nil {
		t.Fatal(err)
	}

	status, raw := f.postWithHeaders(t, msgBody(), map[string]string{"x-session-id": "sess_e2e-1"})
	if status != 200 {
		t.Fatalf("应 200，实际 %d %s", status, raw)
	}
	call := f.lastCall()

	if got := call.Header.Get("X-Device-Mid"); got != "" {
		t.Fatalf("模型请求不应携带 X-Device-Mid（官方实测不带），实际 %q", got)
	}

	metadata, ok := call.Body["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("body 应带 metadata 对象: %v", call.Body)
	}
	rawUserID, ok := metadata["user_id"].(string)
	if !ok {
		t.Fatalf("metadata.user_id 应为 JSON 字符串: %#v", metadata["user_id"])
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(rawUserID), &parsed); err != nil {
		t.Fatalf("user_id 应是合法 JSON: %v（原文 %q）", err, rawUserID)
	}

	// 指纹必须是**本账号**的（每账号一份 ⇒ 账号隔离），不是全局兜底值。
	want := acc.DeviceMidOr(config.DeviceMid())
	if acc.VirtualDeviceMid == nil || *acc.VirtualDeviceMid == "" {
		t.Fatal("账号应已被分配独立设备指纹")
	}
	if parsed["device_id"] != want {
		t.Fatalf("body 的设备指纹 %v，期望本账号的 %q", parsed["device_id"], want)
	}
	if parsed["device_id"] == config.DeviceMid() {
		t.Fatal("body 用了全局设备指纹 —— 同机多账号会被上游按设备关联")
	}

	// 会话 id 与出站头同值（官方恒等）；下游的 sess_ 前缀按官方规则已剥掉。
	if got := call.Header.Get("X-Session-Id"); got != "e2e-1" {
		t.Fatalf("X-Session-Id 应沿用下游并剥掉 sess_ 前缀，实际 %q", got)
	}
	if parsed["session_id"] != "e2e-1" {
		t.Fatalf("body 会话 %v 与头会话 %q 不一致", parsed["session_id"], call.Header.Get("X-Session-Id"))
	}
}

// TestDeviceIdentityPerAccount 同一次请求打到不同账号时，body 里的指纹必须跟着账号变
// （否则账号隔离形同虚设）。用两个账号各发一次，比对两次出站的 device_id。
func TestDeviceIdentityPerAccount(t *testing.T) {
	f := newFixture(t)
	f.respond = jsonResp(200, okUpstreamJSON)
	first, err := f.st.AddAccount(model.ProviderZai, "dev-a", "sk-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.st.AddAccount(model.ProviderZai, "dev-b", "sk-2")
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	for range 2 {
		if status, raw := f.post(t, msgBody(), "sk-test"); status != 200 {
			t.Fatalf("应 200，实际 %d %s", status, raw)
		}
		metadata := f.lastCall().Body["metadata"].(map[string]any)
		var parsed map[string]any
		if err := json.Unmarshal([]byte(metadata["user_id"].(string)), &parsed); err != nil {
			t.Fatal(err)
		}
		seen[parsed["device_id"].(string)] = true
	}

	if len(seen) != 2 {
		t.Fatalf("两个账号应各带自己的设备指纹，实际只见到 %v", seen)
	}
	for _, acc := range []*model.Account{first, second} {
		if !seen[*acc.VirtualDeviceMid] {
			t.Fatalf("账号 %s 的指纹未出现在出站 body 里: %v", acc.Name, seen)
		}
	}
}

// TestDiagBodyHashStableAcrossAccounts 加了每账号设备身份之后，bodyhash 必须仍然
// **跨账号可比**：同一份内容打到不同账号时指纹相同、字节数相同。
//
// 这是「设备身份进 body」的必然副作用：最终 payload 逐账号不同（device_id 不同）。
// 若指纹继续取 payload，「同一个 body 打过几个账号」这条线上判读就废了 —— 所以指纹
// 取注入账号身份**之前**的内容视图（见 ReqDiag.SetBody）。
func TestDiagBodyHashStableAcrossAccounts(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"ok"}]}`)
	})
	f := newDiagFixture(t, up)
	for _, name := range []string{"hash-a", "hash-b"} {
		if _, err := f.st.AddAccount(model.ProviderZai, name, "sk-"+name); err != nil {
			t.Fatal(err)
		}
	}

	f.post(t, msgBody()) // 账号 A
	f.post(t, msgBody()) // 账号 B：内容完全相同

	lines := f.buf.diagLines(t)
	if len(lines) != 2 {
		t.Fatalf("期望 2 条诊断行，实得 %d：%v", len(lines), lines)
	}
	if a, b := diagField(t, lines[0], "acc"), diagField(t, lines[1], "acc"); a == b {
		t.Fatalf("两次请求应落在不同账号上，否则本用例证明不了跨账号可比：%s", a)
	}
	h1, h2 := diagField(t, lines[0], "bodyhash"), diagField(t, lines[1], "bodyhash")
	if h1 != h2 {
		t.Errorf("同一份内容在不同账号上的指纹必须一致：%s vs %s\n行1=%s\n行2=%s",
			h1, h2, lines[0], lines[1])
	}
	// 字节数也应一致：设备指纹与请求 id 都是定长 UUID ⇒ 换号不改体量，
	// 09-24 事故里「4 个固定 body 尺寸」那条判据依然成立。
	if b1, b2 := diagField(t, lines[0], "body"), diagField(t, lines[1], "body"); b1 != b2 {
		t.Errorf("换号不应改变请求体字节数：%s vs %s", b1, b2)
	}
}

// TestDeviceIdentityUsesBodySessionWhenHeaderMissing 下游只把会话写在 body 的
// metadata.user_id 里（没有 x-session-id 头）时，出站头必须用**同一个**会话，
// 而不是另造一个 UUID —— 否则头与 body 自相矛盾。
func TestDeviceIdentityUsesBodySessionWhenHeaderMissing(t *testing.T) {
	f := newFixture(t)
	f.respond = jsonResp(200, okUpstreamJSON)
	if _, err := f.st.AddAccount(model.ProviderZai, "dev-1", "sk-abc"); err != nil {
		t.Fatal(err)
	}

	body := msgBody()
	body["metadata"] = map[string]any{
		"user_id": `{"device_id":"client-supplied","account_uuid":"","session_id":"sess-from-body"}`,
	}
	if status, raw := f.post(t, body, "sk-test"); status != 200 {
		t.Fatalf("应 200，实际 %d %s", status, raw)
	}
	call := f.lastCall()

	// 账号隔离：客户端在 body 里指定的 device_id 必须被本账号指纹覆盖。
	metadata := call.Body["metadata"].(map[string]any)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(metadata["user_id"].(string)), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["device_id"] == "client-supplied" {
		t.Fatal("客户端在 body 里指定的设备指纹不得生效")
	}

	// 头与 body 的会话必须一致，且沿用下游声明的那个（无前缀需剥离）。
	if got := call.Header.Get("X-Session-Id"); got != "sess-from-body" {
		t.Fatalf("X-Session-Id 应沿用 body 声明的会话，实际 %q", got)
	}
	if parsed["session_id"] != "sess-from-body" {
		t.Fatalf("body 会话 %v 与头 %q 不一致", parsed["session_id"], call.Header.Get("X-Session-Id"))
	}
}
