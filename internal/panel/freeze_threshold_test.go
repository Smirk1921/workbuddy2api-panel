// 低积分冻结阈值端点契约测试：POST /panel/api/accounts/{uid}/freeze_threshold
// 200（回读 frozen 结果并落池）、400（threshold < 0 / body 非法）、404（账号不存在）、
// 401（未鉴权）。冻结状态的展示口径（freeze_threshold/frozen/frozen_reason）由
// /panel/api/overview 的账号状态透出，见 internal/pool 的 Status 字段。
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

func TestAccountSetFreezeThresholdEndpoint(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1"})
	pl.SetCredits("u1", 10, 0)

	p := New(Config{Version: "test", APIKey: "test-key", Pool: pl})
	post := func(uid, body string, withAuth bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/panel/api/accounts/"+uid+"/freeze_threshold", strings.NewReader(body))
		if withAuth {
			req.Header.Set("Authorization", "Bearer test-key")
		}
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}

	type resp struct {
		OK        bool  `json:"ok"`
		Threshold int64 `json:"threshold"`
		Frozen    bool  `json:"frozen"`
	}

	// 1) 阈值 100 > 余额 10 → 200 且立即冻结（响应回读 frozen=true，池内一致）。
	rec := post("u1", `{"threshold":100}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got resp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.Threshold != 100 || !got.Frozen {
		t.Fatalf("resp=%+v", got)
	}
	st, ok := pl.Status("u1")
	if !ok || !st.Frozen || st.FreezeThreshold != 100 || st.FrozenReason == "" {
		t.Fatalf("pool state not updated: %+v ok=%v", st, ok)
	}

	// 2) 阈值 0 = 关闭 → 200 + frozen=false，池内冻结态清除。
	rec = post("u1", `{"threshold":0}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.Threshold != 0 || got.Frozen {
		t.Fatalf("resp=%+v", got)
	}
	if st, _ := pl.Status("u1"); st.Frozen || st.FreezeThreshold != 0 {
		t.Fatalf("关闭后池内应清冻结态: %+v", st)
	}

	// 3) 负阈值 → 400（不合法，不改池状态）。
	rec = post("u1", `{"threshold":-1}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative threshold: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if st, _ := pl.Status("u1"); st.FreezeThreshold != 0 {
		t.Fatalf("非法请求不应改池状态: %+v", st)
	}

	// 4) body 非法 JSON → 400。
	rec = post("u1", `{"threshold":`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 5) 未知 uid → 404。
	rec = post("nobody", `{"threshold":5}`, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown uid: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 6) 未鉴权 → 401（与其他 /panel/api/* 同口径）。
	rec = post("u1", `{"threshold":5}`, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 7) 字段缺失（`{}` / 键名拼错）→ 400，且不改池状态：不得被解成零值静默"关闭"
	// 阈值并清掉既有配置（调用方无法区分"显式关阈值"与"字段写错"）。
	rec = post("u1", `{"threshold":100}`, true) // 先建立既有配置（余额 10 < 100 → 冻结）
	if rec.Code != http.StatusOK {
		t.Fatalf("setup: code=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = post("u1", `{}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing threshold: code=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = post("u1", `{"treshold":100}`, true) // 键名拼错同"缺失"
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("typo key: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if st, _ := pl.Status("u1"); st.FreezeThreshold != 100 || !st.Frozen {
		t.Fatalf("缺失/拼错字段不得改池状态: %+v", st)
	}
}
