// 账号批量操作端点契约测试：uid 列表批量动作、changed/affected 区分、
// per-account 锁占用跳过、remove 的 confirm 保护、未分组作用域的 remove 回归、
// 组名保留字、freeze 兼容别名。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// batchResp 解析批量响应（只取断言要用的字段）。
type batchResp struct {
	OK       bool `json:"ok"`
	Total    int  `json:"total"`
	Affected int  `json:"affected"`
	Changed  int  `json:"changed"`
	Skipped  int  `json:"skipped"`
	Removed  int  `json:"removed"`
	Results  []struct {
		UID     string `json:"uid"`
		Status  string `json:"status"`
		Message string `json:"message"`
	} `json:"results"`
}

func newBatchPanel(t *testing.T) (*pool.Pool, *Panel, func(body string) *httptest.ResponseRecorder) {
	t.Helper()
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1"})
	pl.Add(&auth.Auth{UID: "u2"})
	pl.Add(&auth.Auth{UID: "u3"})
	pl.SetCredits("u1", 500, 0)
	pl.SetCredits("u2", 500, 0)
	pl.SetCredits("u3", 500, 0)
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pl})
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/panel/api/accounts/batch", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-key")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}
	return pl, p, post
}

func decodeBatch(t *testing.T, rec *httptest.ResponseRecorder) batchResp {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var r batchResp
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// uid 列表批量暂停/恢复：只命中指定账号，changed 与 affected 一致。
func TestAccountsBatchPauseResume(t *testing.T) {
	pl, _, post := newBatchPanel(t)

	r := decodeBatch(t, post(`{"action":"pause","uids":["u1","u2"]}`))
	if r.Total != 2 || r.Affected != 2 || r.Changed != 2 {
		t.Fatalf("pause resp=%+v（应 2/2/2）", r)
	}
	for _, uid := range []string{"u1", "u2"} {
		if st, _ := pl.Status(uid); !st.Paused {
			t.Fatalf("%s 应已暂停", uid)
		}
	}
	if st, _ := pl.Status("u3"); st.Paused {
		t.Fatal("u3 不该被波及")
	}

	// 重复暂停：affected 仍计数，changed=0（幂等 no-op 不再被当成「动了」）。
	r = decodeBatch(t, post(`{"action":"pause","uids":["u1","u2"]}`))
	if r.Affected != 2 || r.Changed != 0 {
		t.Fatalf("重复 pause 应 affected=2 changed=0，得到 %+v", r)
	}
	if len(r.Results) != 2 || r.Results[0].Message != "已是暂停选号状态" {
		t.Fatalf("幂等项应带说明，得到 %+v", r.Results)
	}

	// 恢复
	r = decodeBatch(t, post(`{"action":"resume","uids":["u1"]}`))
	if r.Changed != 1 {
		t.Fatalf("resume 应 changed=1，得到 %+v", r)
	}
	if st, _ := pl.Status("u1"); st.Paused {
		t.Fatal("u1 应已恢复选号")
	}
}

// freeze 是 disable 的兼容别名：行为完全等价。
func TestAccountsBatchFreezeAliasIsDisable(t *testing.T) {
	pl, _, post := newBatchPanel(t)

	r := decodeBatch(t, post(`{"action":"freeze","uids":["u1"]}`))
	if r.Changed != 1 {
		t.Fatalf("freeze 应等效 disable，得到 %+v", r)
	}
	st, _ := pl.Status("u1")
	if !st.Disabled {
		t.Fatal("freeze 别名应把账号置为 disabled")
	}
	if st.DisabledReason != batchDisableReason {
		t.Fatalf("reason=%q want %q", st.DisabledReason, batchDisableReason)
	}

	// revive 全清
	r = decodeBatch(t, post(`{"action":"revive","uids":["u1"]}`))
	if r.Changed != 1 {
		t.Fatalf("revive 应 changed=1，得到 %+v", r)
	}
	if st, _ := pl.Status("u1"); st.Disabled {
		t.Fatal("revive 后不应再是 disabled")
	}
}

// 阈值 / 优先 / 移组三个带参动作 + 幂等判定。
func TestAccountsBatchValueActions(t *testing.T) {
	pl, _, post := newBatchPanel(t)

	r := decodeBatch(t, post(`{"action":"threshold","uids":["u1","u2"],"value":100}`))
	if r.Changed != 2 {
		t.Fatalf("threshold changed=%d want 2 (%+v)", r.Changed, r)
	}
	if st, _ := pl.Status("u1"); st.FreezeThreshold != 100 {
		t.Fatalf("u1 阈值=%d want 100", st.FreezeThreshold)
	}
	// 同值再设：changed=0
	r = decodeBatch(t, post(`{"action":"threshold","uids":["u1"],"value":100}`))
	if r.Changed != 0 {
		t.Fatalf("同阈值应 changed=0，得到 %+v", r)
	}
	// 负阈值 → 400
	if rec := post(`{"action":"threshold","uids":["u1"],"value":-1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("负阈值应 400，得到 %d", rec.Code)
	}

	r = decodeBatch(t, post(`{"action":"priority","uids":["u2"]}`)) // 缺省 value = 开
	if r.Changed != 1 {
		t.Fatalf("priority changed=%d want 1", r.Changed)
	}
	if st, _ := pl.Status("u2"); !st.Priority {
		t.Fatal("u2 应已设优先")
	}

	r = decodeBatch(t, post(`{"action":"group","uids":["u1","u2"],"group":"  主力  "}`))
	if r.Changed != 2 {
		t.Fatalf("group changed=%d want 2 (%+v)", r.Changed, r)
	}
	for _, uid := range []string{"u1", "u2"} {
		if st, _ := pl.Status(uid); st.Group != "主力" {
			t.Fatalf("%s 分组=%q want 主力", uid, st.Group)
		}
	}
	// 空串 = 移出分组
	r = decodeBatch(t, post(`{"action":"group","uids":["u1"],"group":""}`))
	if r.Changed != 1 {
		t.Fatalf("移出分组 changed=%d want 1", r.Changed)
	}
	if st, _ := pl.Status("u1"); st.Group != "" {
		t.Fatalf("u1 应已移出分组，得到 %q", st.Group)
	}
	// 保留字 → 400
	if rec := post(`{"action":"group","uids":["u1"],"group":"__all__"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("保留字组名应 400，得到 %d", rec.Code)
	}
}

// per-account 锁被占用 → skipped，不改状态。
func TestAccountsBatchSkipsLockedAccount(t *testing.T) {
	pl, p, post := newBatchPanel(t)
	if !p.tryLockAccount("u2") {
		t.Fatal("测试前置：应能拿到 u2 的锁")
	}
	r := decodeBatch(t, post(`{"action":"pause","uids":["u1","u2"]}`))
	p.unlockAccount("u2")

	if r.Affected != 1 || r.Skipped != 1 {
		t.Fatalf("应 1 命中 1 跳过，得到 %+v", r)
	}
	if st, _ := pl.Status("u2"); st.Paused {
		t.Fatal("被锁的 u2 不该被改动")
	}
	var skippedMsg string
	for _, it := range r.Results {
		if it.UID == "u2" {
			skippedMsg = it.Message
		}
	}
	if !strings.Contains(skippedMsg, "任务动作") {
		t.Fatalf("跳过原因应说明任务占用，得到 %q", skippedMsg)
	}
}

// remove：confirm 必须是 "REMOVE"；缺失 400；成功后池与文件都清掉。
func TestAccountsBatchRemoveConfirm(t *testing.T) {
	pl, _, post := newBatchPanel(t)

	if rec := post(`{"action":"remove","uids":["u1"]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 confirm 应 400，得到 %d", rec.Code)
	}
	if rec := post(`{"action":"remove","uids":["u1"],"confirm":"u1"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("confirm 不是 REMOVE 应 400，得到 %d", rec.Code)
	}
	r := decodeBatch(t, post(`{"action":"remove","uids":["u1","u3"],"confirm":"REMOVE"}`))
	if r.Removed != 2 {
		t.Fatalf("removed=%d want 2 (%+v)", r.Removed, r)
	}
	if _, ok := pl.Status("u1"); ok {
		t.Fatal("u1 应已移除")
	}
	if _, ok := pl.Status("u2"); !ok {
		t.Fatal("u2 不该被波及")
	}
}

// 回归：`__ungrouped__` 作用域的批量移除曾经永远 400（scope 被归一化成 ""），
// 现在 confirm 比对的是原始作用域串。
func TestGroupRemoveUngroupedScope(t *testing.T) {
	pl, p, _ := newBatchPanel(t)
	pl.SetGroup("u1", "主力")
	// u2/u3 未分组

	post := func(confirm string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST",
			"/panel/api/groups/"+pool.GroupScopeUngrouped+"/remove", strings.NewReader(confirm))
		req.Header.Set("Authorization", "Bearer test-key")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}
	// confirm 用组名 → 400
	if rec := post(`{"confirm":"主力"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("confirm 不符应 400，得到 %d", rec.Code)
	}
	// confirm 用原始哨兵串 → 成功移除未分组的 u2/u3
	r := decodeBatch(t, post(`{"confirm":"__ungrouped__"}`))
	if r.Removed != 2 {
		t.Fatalf("removed=%d want 2（未分组的 u2/u3）", r.Removed)
	}
	if _, ok := pl.Status("u1"); !ok {
		t.Fatal("u1 在「主力」组，不该被移除")
	}
}

// 未知动作仍是 404（保持旧契约）；缺 uids / 动作 → 400。
func TestAccountsBatchBadRequests(t *testing.T) {
	_, _, post := newBatchPanel(t)

	if rec := post(`{"action":"bogus","uids":["u1"]}`); rec.Code != http.StatusNotFound {
		t.Fatalf("未知动作应 404，得到 %d", rec.Code)
	}
	if rec := post(`{"action":"pause"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 uids 应 400，得到 %d", rec.Code)
	}
	if rec := post(`{"uids":["u1"]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 action 应 400，得到 %d", rec.Code)
	}
	if rec := post(`not json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("坏 JSON 应 400，得到 %d", rec.Code)
	}
}

// 请求里包含池中不存在的 uid：如实回报为 skipped，不影响其余账号。
func TestAccountsBatchMissingUID(t *testing.T) {
	_, _, post := newBatchPanel(t)
	r := decodeBatch(t, post(`{"action":"pause","uids":["u1","ghost"]}`))
	if r.Affected != 1 || r.Skipped != 1 || r.Total != 2 {
		t.Fatalf("resp=%+v（应 1 命中 1 跳过 / total 2）", r)
	}
}
