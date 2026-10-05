// 账号分组端点契约测试：POST /panel/api/accounts/{uid}/group（200/400/404/401）、
// GET /panel/api/groups、POST /panel/api/groups/{group}/{action}（priority/threshold/
// freeze/revive 的作用域与 affected、remove 的 confirm 保护）。
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

func newGroupPanel(t *testing.T) (*pool.Pool, *Panel, func(uid, body string, auth bool) *httptest.ResponseRecorder) {
	t.Helper()
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1"})
	pl.Add(&auth.Auth{UID: "u2"})
	pl.Add(&auth.Auth{UID: "u3"})
	pl.SetCredits("u1", 500, 0)
	pl.SetCredits("u2", 500, 0)
	pl.SetCredits("u3", 500, 0)
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pl})
	post := func(uid, body string, auth bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/panel/api/accounts/"+uid+"/group", strings.NewReader(body))
		if auth {
			req.Header.Set("Authorization", "Bearer test-key")
		}
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}
	return pl, p, post
}

// TestAccountSetGroupEndpoint 单号分组端点：设置/移出/字段缺失/未知 uid/未鉴权。
func TestAccountSetGroupEndpoint(t *testing.T) {
	pl, _, post := newGroupPanel(t)

	// 1) 设置分组 → 200 + 回读归一化组名，池内一致。
	rec := post("u1", `{"group":"  主力  "}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		OK    bool   `json:"ok"`
		Group string `json:"group"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.Group != "主力" {
		t.Fatalf("resp=%+v", got)
	}
	if st, _ := pl.Status("u1"); st.Group != "主力" {
		t.Fatalf("pool state not updated: %+v", st)
	}

	// 2) 空串 → 移出分组（200 + group=""）。
	rec = post("u1", `{"group":""}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if st, _ := pl.Status("u1"); st.Group != "" {
		t.Fatalf("空串应移出分组: %+v", st)
	}

	// 3) 字段缺失 / 拼错键名 → 400 且不改池状态（不得静默"移出分组"）。
	rec = post("u1", `{"group":"保持"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("setup: code=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = post("u1", `{}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing group: code=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = post("u1", `{"grop":"x"}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("typo key: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if st, _ := pl.Status("u1"); st.Group != "保持" {
		t.Fatalf("缺失/拼错字段不得改池状态: %+v", st)
	}

	// 4) body 非法 JSON → 400。
	rec = post("u1", `{"group":`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 5) 未知 uid → 404。
	rec = post("nobody", `{"group":"x"}`, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown uid: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 6) 未鉴权 → 401。
	rec = post("u1", `{"group":"x"}`, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: code=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestGroupsList 分组列表：含未分组哨兵与组名去重排序。
func TestGroupsList(t *testing.T) {
	pl, p, _ := newGroupPanel(t)
	pl.SetGroup("u1", "主力")
	pl.SetGroup("u2", "主力")
	pl.SetGroup("u3", "备机")

	req := httptest.NewRequest("GET", "/panel/api/groups", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Groups       []string `json:"groups"`
		HasUngrouped bool     `json:"has_ungrouped"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.HasUngrouped {
		t.Fatal("全员已分组 → 不应有未分组哨兵")
	}
	// sort.Strings 按 UTF-8 字节序："主"(e4b8bb) < "备"(e5a487)。
	if len(got.Groups) != 2 || got.Groups[0] != "主力" || got.Groups[1] != "备机" {
		t.Fatalf("groups=%v（应去重排序）", got.Groups)
	}
}

// TestGroupActionScopeAndProtection 批量操作的作用域与 remove 保护。
func TestGroupActionScopeAndProtection(t *testing.T) {
	pl, p, _ := newGroupPanel(t)
	pl.SetGroup("u1", "主力")
	pl.SetGroup("u2", "主力") // u3 未分组

	call := func(action, group, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/panel/api/groups/"+group+"/"+action, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-key")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}

	// 1) 对「主力」批量设优先 → affected=2（u1,u2），u3 不受影响。
	rec := call("priority", "%E4%B8%BB%E5%8A%9B", `{"value":1}`) // "主力" URL 编码
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var r1 struct {
		Affected int `json:"affected"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &r1); err != nil {
		t.Fatal(err)
	}
	if r1.Affected != 2 {
		t.Fatalf("affected=%d（应命中 u1+u2）", r1.Affected)
	}
	for _, uid := range []string{"u1", "u2"} {
		if st, _ := pl.Status(uid); !st.Priority {
			t.Fatalf("%s 应被设优先", uid)
		}
	}
	if st, _ := pl.Status("u3"); st.Priority {
		t.Fatal("u3 未分组，不应被波及")
	}

	// 2) 对「未分组」批量设阈值 → affected=1（仅 u3）。
	rec = call("threshold", "__ungrouped__", `{"value":100}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var r2 struct {
		Affected int `json:"affected"`
	}
	json.Unmarshal(rec.Body.Bytes(), &r2)
	if r2.Affected != 1 {
		t.Fatalf("affected=%d（应仅命中 u3）", r2.Affected)
	}
	if st, _ := pl.Status("u3"); st.FreezeThreshold != 100 {
		t.Fatalf("u3 阈值应为 100: %+v", st)
	}

	// 3) 对「全部」批量冻结 → affected=3。
	rec = call("freeze", "__all__", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var r3 struct {
		Affected int `json:"affected"`
	}
	json.Unmarshal(rec.Body.Bytes(), &r3)
	if r3.Affected != 3 {
		t.Fatalf("affected=%d（应命中全部）", r3.Affected)
	}

	// 4) remove 无 confirm → 400（不可逆保护）。
	rec = call("remove", "%E4%B8%BB%E5%8A%9B", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("remove without confirm: code=%d body=%s", rec.Code, rec.Body.String())
	}
	// remove confirm 与组名不符 → 400。
	rec = call("remove", "%E4%B8%BB%E5%8A%9B", `{"confirm":"备机"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("remove wrong confirm: code=%d body=%s", rec.Code, rec.Body.String())
	}
	// remove confirm 正确 → 移除「主力」两个账号（u1,u2）。
	rec = call("remove", "%E4%B8%BB%E5%8A%9B", `{"confirm":"主力"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("remove: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var r4 struct {
		Removed int `json:"removed"`
	}
	json.Unmarshal(rec.Body.Bytes(), &r4)
	if r4.Removed != 2 {
		t.Fatalf("removed=%d（应移除 u1+u2）", r4.Removed)
	}
	if _, ok := pl.Status("u1"); ok {
		t.Fatal("u1 应已移除")
	}
	if _, ok := pl.Status("u3"); !ok {
		t.Fatal("u3 不应被波及")
	}

	// 5) 未知 action → 404。
	rec = call("bogus", "__all__", `{}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown action: code=%d body=%s", rec.Code, rec.Body.String())
	}
}
