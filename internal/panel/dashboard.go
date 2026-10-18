// dashboard.go 仪表盘聚合：一屏看清「有多少号能用 / 多少积分真的可用 / 花在哪 / 何时到期」。
//
//	GET /panel/api/dashboard?hours=168&exclude=frozen_paused
//
// 全部由**现有内存状态 + 本地用量记录器**聚合，不新增任何上游请求——打开仪表盘
// 不会打上游（与「积分到期提醒」卡片按 overview 快照渲染同一原则）。
//
// 积分口径分三档（前端用同一个开关切换，账号池那张 #sCredits 也读同一开关）：
//   - all                  全部账号（历史口径，与旧版统计卡片一致）
//   - exclude_frozen_paused 排除「低积分冻结」与「暂停选号」——这两个状态不参与选号，
//     它们的积分对「还能花多少」没有意义
//   - servable_only         只算此刻真正可服务的号（再排除 disabled 与冷却中）
//
// 企业版 `credits_total == -1`（不限量）不计入总额，单独计数——旧前端直接累加会把
// 总额减 1，是个长期存在的小口径错误。
package panel

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// dashboardDefaultHours 默认窗口：7 天。
const dashboardDefaultHours = 24 * 7

// dashboardMaxHours 窗口上限：30 天（与用量记录器的小时上限一致）。
const dashboardMaxHours = 24 * 30

// dashboardCounts 账号计数（与 overview 同口径：disabled > paused > frozen > cooling > healthy）。
type dashboardCounts struct {
	Total        int `json:"total"`
	Healthy      int `json:"healthy"`
	Cooling      int `json:"cooling"`
	Frozen       int `json:"frozen"`
	Disabled     int `json:"disabled"`
	Paused       int `json:"paused"`
	InFlightFull int `json:"in_flight_full"`
}

// dashboardCredits 某一档口径下的积分汇总。
type dashboardCredits struct {
	Accounts  int   `json:"accounts"`
	Remain    int64 `json:"remain"`
	Total     int64 `json:"total"`
	Unlimited int   `json:"unlimited_accounts,omitempty"` // 企业版不限量（credits_total=-1）
}

// dashboardRealm 按域（cn / global）的一组计数 + 积分。
type dashboardRealm struct {
	Counts  dashboardCounts  `json:"counts"`
	Credits dashboardCredits `json:"credits"`
}

// dashboardGroup 按分组的聚合（空组名 = 未分组）。
type dashboardGroup struct {
	Group         string `json:"group"`
	Accounts      int    `json:"accounts"`
	Healthy       int    `json:"healthy"`
	Cooling       int    `json:"cooling"`
	Frozen        int    `json:"frozen"`
	Paused        int    `json:"paused"`
	Disabled      int    `json:"disabled"`
	CreditsRemain int64  `json:"credits_remain"`
	CreditsTotal  int64  `json:"credits_total"`
}

// dashboardExpiry 积分到期压力（非重叠三段：0-30 / 31-60 / 61-90 天）。
type dashboardExpiry struct {
	D30               int    `json:"d30"`
	D60               int    `json:"d60"`
	D90               int    `json:"d90"`
	EarliestDate      string `json:"earliest_date,omitempty"`
	EarliestRemaining int64  `json:"earliest_remaining,omitempty"`
}

// dashboardTopAccount 积分最多的账号（运维一眼看到「谁的积分快到期/最多」）。
type dashboardTopAccount struct {
	UID             string `json:"uid"`
	Nickname        string `json:"nickname,omitempty"`
	Realm           string `json:"realm,omitempty"`
	Group           string `json:"group,omitempty"`
	Credits         int64  `json:"credits"`
	CreditsTotal    int64  `json:"credits_total,omitempty"`
	CreditsExpiring int64  `json:"credits_expiring,omitempty"`
}

// dashboardAgg 纯聚合结果（无 I/O，供 handler 与单测共用）。
type dashboardAgg struct {
	Counts  dashboardCounts             `json:"counts"`
	Credits map[string]dashboardCredits `json:"credits"`
	Realm   map[string]dashboardRealm   `json:"realm"`
	Groups  []dashboardGroup            `json:"groups"`
	Expiry  dashboardExpiry             `json:"expiry"`
	Top     []dashboardTopAccount       `json:"top_accounts"`
}

// dashboard 聚合视图 handler。
func (p *Panel) dashboard(w http.ResponseWriter, r *http.Request) {
	hours := parseDashboardHours(r.URL.Query().Get("hours"))
	list := p.cfg.Pool.List()

	total, healthy, cooling, frozen, disabled, paused, inFlightFull := p.cfg.Pool.CountsDetailedWithPaused()
	agg := buildDashboardAgg(list, dashboardCounts{
		Total: total, Healthy: healthy, Cooling: cooling, Frozen: frozen,
		Disabled: disabled, Paused: paused, InFlightFull: inFlightFull,
	})

	out := map[string]any{
		"ok":           true,
		"generated_at": time.Now().Format(time.RFC3339),
		"hours":        hours,
		"counts":       agg.Counts,
		"credits":      agg.Credits,
		"realm":        agg.Realm,
		"groups":       agg.Groups,
		"expiry":       agg.Expiry,
		"top_accounts": agg.Top,
		"model_locks":  p.cfg.Pool.ModelLockView(),
	}
	// 用量时序：复用记录器的 Snapshot（与本地面板「用量」页同一口径），
	// 不额外打上游。未配置记录器时省略该字段，前端按空态渲染。
	if p.cfg.Usage != nil {
		nicks := make(map[string]string, len(list))
		for _, s := range list {
			nicks[s.UID] = s.Nickname
		}
		snap := p.cfg.Usage.Snapshot(hours, nicks)
		out["usage"] = map[string]any{
			"totals":            snap.Totals,
			"series":            snap.Series,
			"by_model":          snap.ByModel,
			"by_account":        snap.ByAccount,
			"credit_by_account": snap.CreditByAccount,
			"credit_by_model":   snap.CreditByModel,
			"buckets":           snap.Buckets,
			"window_from":       snap.WindowFrom,
			"window_to":         snap.WindowTo,
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// parseDashboardHours 解析窗口参数：非法/缺省 → 7 天；夹到 [1, 720]。
func parseDashboardHours(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return dashboardDefaultHours
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return dashboardDefaultHours
	}
	if n > dashboardMaxHours {
		return dashboardMaxHours
	}
	return n
}

// buildDashboardAgg 纯聚合：输入池状态快照 + 已算好的计数，输出各口径汇总。
// 不读配置、不碰网络、不持锁（list 已是快照），便于单测。
func buildDashboardAgg(list []pool.Status, counts dashboardCounts) dashboardAgg {
	return dashboardAgg{
		Counts: counts,
		Credits: map[string]dashboardCredits{
			"all": creditBucket(list, func(pool.Status) bool { return true }),
			// 冻结（低积分保护）与暂停选号都不参与选号，它们的积分不算「能花的」。
			"exclude_frozen_paused": creditBucket(list, func(s pool.Status) bool {
				return !s.Frozen && !s.Paused
			}),
			// 只算此刻真正可服务的：再排除禁用与冷却中。
			"servable_only": creditBucket(list, func(s pool.Status) bool {
				return !s.Disabled && !s.Paused && !s.Frozen && !s.Cooling
			}),
		},
		Realm: map[string]dashboardRealm{
			"cn":     realmSummary(list, "cn"),
			"global": realmSummary(list, "global"),
		},
		Groups: groupSummary(list),
		Expiry: expirySummary(list),
		Top:    topAccounts(list, 8),
	}
}

// creditBucket 按谓词过滤后汇总积分。企业版不限量账号单独计数、不计入总额。
func creditBucket(list []pool.Status, keep func(pool.Status) bool) dashboardCredits {
	var c dashboardCredits
	for _, s := range list {
		if !keep(s) {
			continue
		}
		c.Accounts++
		c.Remain += s.Credits
		if s.CreditsTotal == -1 {
			c.Unlimited++
			continue
		}
		if s.CreditsTotal > 0 {
			c.Total += s.CreditsTotal
		}
	}
	return c
}

// classify 与 pool.countsDetailedForRealm 同口径的分类（disabled > paused > frozen > cooling > healthy）。
func classify(s pool.Status) (healthy, cooling, frozen, paused, disabled bool) {
	switch {
	case s.Disabled:
		return false, false, false, false, true
	case s.Paused:
		return false, false, false, true, false
	case s.Frozen:
		return false, false, true, false, false
	case s.Cooling:
		return false, true, false, false, false
	default:
		return true, false, false, false, false
	}
}

// realmSummary 单域计数 + 积分（积分口径用 all，便于与顶部卡片对齐）。
func realmSummary(list []pool.Status, realm string) dashboardRealm {
	var r dashboardRealm
	var sub []pool.Status
	for _, s := range list {
		if s.Realm != realm {
			continue
		}
		sub = append(sub, s)
		h, c, f, p, d := classify(s)
		r.Counts.Total++
		if h {
			r.Counts.Healthy++
		}
		if c {
			r.Counts.Cooling++
		}
		if f {
			r.Counts.Frozen++
		}
		if p {
			r.Counts.Paused++
		}
		if d {
			r.Counts.Disabled++
		}
	}
	r.Credits = creditBucket(sub, func(pool.Status) bool { return true })
	return r
}

// groupSummary 按分组聚合，按账号数降序（空组名 = 未分组，排最后也无所谓——有账号数排序）。
func groupSummary(list []pool.Status) []dashboardGroup {
	by := make(map[string]*dashboardGroup, 8)
	for _, s := range list {
		g := by[s.Group]
		if g == nil {
			g = &dashboardGroup{Group: s.Group}
			by[s.Group] = g
		}
		g.Accounts++
		g.CreditsRemain += s.Credits
		if s.CreditsTotal > 0 {
			g.CreditsTotal += s.CreditsTotal
		}
		h, c, f, p, d := classify(s)
		if h {
			g.Healthy++
		}
		if c {
			g.Cooling++
		}
		if f {
			g.Frozen++
		}
		if p {
			g.Paused++
		}
		if d {
			g.Disabled++
		}
	}
	out := make([]dashboardGroup, 0, len(by))
	for _, g := range by {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Accounts != out[j].Accounts {
			return out[i].Accounts > out[j].Accounts
		}
		return out[i].Group < out[j].Group
	})
	return out
}

// expirySummary 积分到期压力：以各账号「最早到期批次」为口径，分成非重叠三段。
func expirySummary(list []pool.Status) dashboardExpiry {
	now := time.Now()
	var e dashboardExpiry
	var earliest time.Time
	for _, s := range list {
		t := s.CreditsEarliestExpiry
		if t.IsZero() {
			continue
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
			e.EarliestRemaining = s.CreditsEarliestRemaining
		}
		days := int(t.Sub(now).Hours() / 24)
		if days < 0 {
			days = 0
		}
		switch {
		case days <= 30:
			e.D30++
		case days <= 60:
			e.D60++
		case days <= 90:
			e.D90++
		}
	}
	if !earliest.IsZero() {
		e.EarliestDate = earliest.Format("2006-01-02")
	}
	return e
}

// topAccounts 按剩余积分降序取前 n 个（同分按昵称/uid 稳定排序）。
func topAccounts(list []pool.Status, n int) []dashboardTopAccount {
	if n <= 0 || len(list) == 0 {
		return nil
	}
	sorted := make([]pool.Status, len(list))
	copy(sorted, list)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Credits != sorted[j].Credits {
			return sorted[i].Credits > sorted[j].Credits
		}
		return sorted[i].UID < sorted[j].UID
	})
	if len(sorted) > n {
		sorted = sorted[:n]
	}
	out := make([]dashboardTopAccount, 0, len(sorted))
	for _, s := range sorted {
		out = append(out, dashboardTopAccount{
			UID:             s.UID,
			Nickname:        s.Nickname,
			Realm:           s.Realm,
			Group:           s.Group,
			Credits:         s.Credits,
			CreditsTotal:    s.CreditsTotal,
			CreditsExpiring: s.CreditsExpiring,
		})
	}
	return out
}
