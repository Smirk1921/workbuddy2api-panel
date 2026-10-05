// 「优先使用该账号积分」端点契约测试：POST /panel/api/accounts/{uid}/priority
// 200（回读开关并落池）、400（字段缺失 / 键名拼错 / body 非法）、404（账号不存在）、
// 401（未鉴权）。开关状态经 /panel/api/overview 的账号状态（priority 字段）透出，
// 见 internal/pool 的 Status 字段。
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

func TestAccountSetPriorityEndpoint(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1"})
	pl.Add(&auth.Auth{UID: "u2"})
	pl.SetCredits("u1", 10, 0)
	pl.SetCredits("u2", 500, 0)

	p := New(Config{Version: "test", APIKey: "test-key", Pool: pl})
	post := func(uid, body string, withAuth bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/panel/api/accounts/"+uid+"/priority", strings.NewReader(body))
		if withAuth {
			req.Header.Set("Authorization", "Bearer test-key")
		}
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}

	type resp struct {
		OK       bool `json:"ok"`
		Priority bool `json:"priority"`
	}

	// 1) 开启优先 → 200 + 回读 priority=true，池内一致；选号随即独占该号。
	rec := post("u1", `{"priority":true}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got resp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || !got.Priority {
		t.Fatalf("resp=%+v", got)
	}
	if st, ok := pl.Status("u1"); !ok || !st.Priority {
		t.Fatalf("pool state not updated: %+v ok=%v", st, ok)
	}
	if acct := pl.Pick(); acct == nil || acct.UID != "u1" {
		t.Fatalf("设优先后选号应独占 u1，实得 %+v", acct)
	}

	// 2) 关闭优先 → 200 + priority=false，池内清除。
	rec = post("u1", `{"priority":false}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.Priority {
		t.Fatalf("resp=%+v", got)
	}
	if st, _ := pl.Status("u1"); st.Priority {
		t.Fatalf("关闭后池内应清优先: %+v", st)
	}

	// 3) body 非法 JSON → 400。
	rec = post("u1", `{"priority":`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 4) 未知 uid → 404。
	rec = post("nobody", `{"priority":true}`, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown uid: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 5) 未鉴权 → 401（与其他 /panel/api/* 同口径）。
	rec = post("u1", `{"priority":true}`, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 6) 字段缺失（`{}` / 键名拼错）→ 400 且不改池状态：不得被解成零值静默"取消优先"
	// 并清掉既有开关（调用方无法区分"显式关闭"与"字段写错"）。
	rec = post("u1", `{"priority":true}`, true) // 先建立既有开关
	if rec.Code != http.StatusOK {
		t.Fatalf("setup: code=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = post("u1", `{}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing priority: code=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = post("u1", `{"prioirty":true}`, true) // 键名拼错同"缺失"
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("typo key: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if st, _ := pl.Status("u1"); !st.Priority {
		t.Fatalf("缺失/拼错字段不得改池状态: %+v", st)
	}
}
