// 仪表盘聚合测试：三档积分口径（含企业版 -1 不计总额）、分域、分组聚合、
// 到期分段、Top 账号、窗口参数夹断，以及端点级冒烟。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// dashStatus 造一个状态快照（只填断言要用的字段）。
func dashStatus(uid string, credits, total int64, realm, group string, mutate func(*pool.Status)) pool.Status {
	s := pool.Status{UID: uid, Credits: credits, CreditsTotal: total, Realm: realm, Group: group}
	if mutate != nil {
		mutate(&s)
	}
	return s
}

func TestBuildDashboardAggCredits(t *testing.T) {
	now := time.Now()
	list := []pool.Status{
		dashStatus("u1", 1000, 5000, "cn", "主力", nil),                                     // healthy
		dashStatus("u2", 500, 2000, "cn", "主力", func(s *pool.Status) { s.Paused = true }), // 暂停
		dashStatus("u3", 300, 1000, "cn", "", func(s *pool.Status) { s.Frozen = true }),   // 低积分冻结
		dashStatus("u4", 200, 0, "cn", "", func(s *pool.Status) { s.Disabled = true }),    // 禁用
		dashStatus("u5", 400, 0, "global", "", func(s *pool.Status) { s.Cooling = true }), // 冷却
		dashStatus("u6", 50, -1, "global", "备机", nil),                                     // 企业版不限量
		// 到期：今天 + 20 天 / + 45 天 / + 75 天
		dashStatus("u7", 10, 100, "cn", "", func(s *pool.Status) {
			s.CreditsEarliestExpiry = now.Add(20 * 24 * time.Hour)
			s.CreditsEarliestRemaining = 10
		}),
		dashStatus("u8", 10, 100, "cn", "", func(s *pool.Status) {
			s.CreditsEarliestExpiry = now.Add(45 * 24 * time.Hour)
			s.CreditsEarliestRemaining = 10
		}),
		dashStatus("u9", 10, 100, "cn", "", func(s *pool.Status) {
			s.CreditsEarliestExpiry = now.Add(75 * 24 * time.Hour)
			s.CreditsEarliestRemaining = 10
		}),
	}
	agg := buildDashboardAgg(list, dashboardCounts{Total: len(list)})

	// all：9 个账号；remain = 1000+500+300+200+400+50+10+10+10 = 2480；
	// total = 5000+2000+1000+0+0+100+100+100 = 8300（u6 是不限量 -1，不计入）。
	if got := agg.Credits["all"]; got.Accounts != 9 || got.Remain != 2480 || got.Total != 8300 || got.Unlimited != 1 {
		t.Errorf("all=%+v want accounts=9 remain=2480 total=8300 unlimited=1", got)
	}
	// exclude_frozen_paused：去掉 u2(500,2000) 与 u3(300,1000) → 7 个账号，remain=1680，total=5300
	if got := agg.Credits["exclude_frozen_paused"]; got.Accounts != 7 || got.Remain != 1680 || got.Total != 5300 {
		t.Errorf("exclude_frozen_paused=%+v want accounts=7 remain=1680 total=5300", got)
	}
	// servable_only：再去掉 u4(200) 与 u5(400) → 5 个账号，remain=1080
	if got := agg.Credits["servable_only"]; got.Accounts != 5 || got.Remain != 1080 {
		t.Errorf("servable_only=%+v want accounts=5 remain=1080", got)
	}

	// 分域
	if r := agg.Realm["cn"]; r.Counts.Total != 7 || r.Credits.Remain != 2030 {
		t.Errorf("cn=%+v want total=7 remain=2030", r)
	}
	if r := agg.Realm["global"]; r.Counts.Total != 2 || r.Credits.Remain != 450 {
		t.Errorf("global=%+v want total=2 remain=450", r)
	}

	// 到期分段（非重叠）
	if agg.Expiry.D30 != 1 || agg.Expiry.D60 != 1 || agg.Expiry.D90 != 1 {
		t.Errorf("expiry=%+v want d30/d60/d90 = 1/1/1", agg.Expiry)
	}
	if agg.Expiry.EarliestDate == "" || agg.Expiry.EarliestRemaining != 10 {
		t.Errorf("earliest=%q/%d", agg.Expiry.EarliestDate, agg.Expiry.EarliestRemaining)
	}

	// 分组：主力 2、备机 1、未分组 6
	byGroup := map[string]dashboardGroup{}
	for _, g := range agg.Groups {
		byGroup[g.Group] = g
	}
	if g := byGroup["主力"]; g.Accounts != 2 || g.Healthy != 1 || g.Paused != 1 {
		t.Errorf("主力=%+v", g)
	}
	if g := byGroup[""]; g.Accounts != 6 {
		t.Errorf("未分组 accounts=%d want 6", g.Accounts)
	}

	// Top：u1(1000) 第一
	if len(agg.Top) == 0 || agg.Top[0].UID != "u1" {
		t.Errorf("top[0]=%+v want u1", agg.Top)
	}
}

// classify 必须与 pool 的互斥优先级一致：disabled > paused > frozen > cooling > healthy。
func TestDashboardClassifyPriority(t *testing.T) {
	all := pool.Status{Disabled: true, Paused: true, Frozen: true, Cooling: true}
	h, c, f, p, d := classify(all)
	if h || c || f || p || !d {
		t.Fatalf("全置位时应只算 disabled，得到 h=%v c=%v f=%v p=%v d=%v", h, c, f, p, d)
	}
	// 未禁用但暂停 + 冻结 → paused 优先
	h, c, f, p, d = classify(pool.Status{Paused: true, Frozen: true})
	if h || c || f || !p || d {
		t.Fatalf("暂停优先于冻结，得到 h=%v c=%v f=%v p=%v d=%v", h, c, f, p, d)
	}
}

func TestParseDashboardHours(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"", dashboardDefaultHours},
		{"24", 24},
		{"0", dashboardDefaultHours},
		{"-5", dashboardDefaultHours},
		{"abc", dashboardDefaultHours},
		{"99999", dashboardMaxHours},
	}
	for _, c := range cases {
		if got := parseDashboardHours(c.raw); got != c.want {
			t.Errorf("%q: got %d want %d", c.raw, got, c.want)
		}
	}
}

// 端点冒烟：空池也要 200，且三个口径的键都在。
func TestDashboardEndpoint(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", Nickname: "n1"})
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pl})

	req := httptest.NewRequest("GET", "/panel/api/dashboard?hours=24", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		OK      bool           `json:"ok"`
		Hours   int            `json:"hours"`
		Counts  map[string]any `json:"counts"`
		Credits map[string]struct {
			Accounts int   `json:"accounts"`
			Remain   int64 `json:"remain"`
		} `json:"credits"`
		Groups []dashboardGroup `json:"groups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.Hours != 24 || out.Counts == nil {
		t.Fatalf("resp=%+v", out)
	}
	for _, k := range []string{"all", "exclude_frozen_paused", "servable_only"} {
		if _, ok := out.Credits[k]; !ok {
			t.Errorf("credits 缺口径 %q", k)
		}
	}
	if len(out.Groups) != 1 {
		t.Errorf("groups=%+v want 1（未分组）", out.Groups)
	}
}
