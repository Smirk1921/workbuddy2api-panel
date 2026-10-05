// 「优先使用该账号积分」（priority）行为测试：硬优先分层对普通权重与成本分层的
// 压制、不可用时的回落、请求级 tried 的优先级、兜底路径的优先、会话虚拟实例加成、
// 持久化往返与旧 state.json 零回归，以及与冻结/禁用/Revive 的正交性。
package pool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// priorityDomain 曝露 entry 的 priority 原始字段供测试断言（包内私有 helper）。
func (p *Pool) priorityDomain(uid string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	return e.priority
}

// TestPriorityPrefersDesignatedAccount 优先号在加权随机中恒被选中：
// 即使它的积分远低于其他账号（credits 权重项最小），只要它可用就独占本轮选号。
func TestPriorityPrefersDesignatedAccount(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.Add(&auth.Auth{UID: "u3"})
	p.SetCredits("u1", 1, 0)     // 优先号：积分极低（普通权重下最不可能中签）
	p.SetCredits("u2", 10000, 0) // 非优先：积分最高
	p.SetCredits("u3", 10000, 0)

	p.SetPriority("u1", true)
	for i := 0; i < 30; i++ {
		got := p.Pick()
		if got == nil || got.UID != "u1" {
			t.Fatalf("第 %d 次选号应恒为优先号 u1，实得 %+v", i, got)
		}
	}

	// 取消优先 → 回到普通加权（u1 积分极低，不应再被选中）。
	p.SetPriority("u1", false)
	seenOther := false
	for i := 0; i < 30; i++ {
		if got := p.Pick(); got != nil && got.UID != "u1" {
			seenOther = true
		}
	}
	if !seenOther {
		t.Fatal("取消优先后不应仍恒选 u1")
	}
}

// TestPriorityBeatsCostTier 优先凌驾于成本分层：优先号实测收费（tier 2）、另一号
// 实测免费（tier 0）时仍选优先号——否则「优先」会被成本分层整层滤掉而失效。
func TestPriorityBeatsCostTier(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "paid"})
	p.Add(&auth.Auth{UID: "free"})
	p.SetCredits("paid", 500, 0)
	p.SetCredits("free", 500, 0)
	p.NoteModelCost("paid", "m1", 5, 1000) // per1k=5 → tier 2（收费）
	p.NoteModelCost("free", "m1", 0, 1000) // per1k=0 → tier 0（实测免费）

	// 未设优先：成本分层生效 → 免费号独占。
	for i := 0; i < 10; i++ {
		if got := p.PickExcludingForModel(nil, "m1"); got == nil || got.UID != "free" {
			t.Fatalf("未设优先时应选免费号，实得 %+v", got)
		}
	}

	// 设优先：收费的优先号压过免费号（人工显式意图 > 自动成本偏好）。
	p.SetPriority("paid", true)
	for i := 0; i < 10; i++ {
		if got := p.PickExcludingForModel(nil, "m1"); got == nil || got.UID != "paid" {
			t.Fatalf("设优先后应选优先号 paid，实得 %+v", got)
		}
	}
}

// TestPriorityFallsBackWhenUnavailable 优先号不可用时自动回落普通池：
// 禁用 / 低积分冻结 / 在途占满 三种不可用形态分别验证。
func TestPriorityFallsBackWhenUnavailable(t *testing.T) {
	cases := []struct {
		name    string
		unavail func(p *Pool)
	}{
		{"禁用", func(p *Pool) { p.Disable("u1", "test") }},
		{"低积分冻结", func(p *Pool) { p.SetFreezeThreshold("u1", 1000) }},
		{"在途占满", func(p *Pool) {
			p.SetMaxInFlight(1)
			if !p.Acquire("u1") {
				t.Fatal("占位失败")
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := New("")
			p.Add(&auth.Auth{UID: "u1"})
			p.Add(&auth.Auth{UID: "u2"})
			p.SetCredits("u1", 500, 0)
			p.SetCredits("u2", 500, 0)
			p.SetPriority("u1", true)
			tc.unavail(p)

			for i := 0; i < 10; i++ {
				got := p.Pick()
				if got == nil || got.UID != "u2" {
					t.Fatalf("%s：优先号不可用时应回落 u2，实得 %+v", tc.name, got)
				}
			}
		})
	}
}

// TestPriorityRespectsTried 请求级轮换（tried）优先于「优先」：优先号已被本轮试过时
// 不再选它，避免同一请求在同一个号上反复重试。
func TestPriorityRespectsTried(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 500, 0)
	p.SetCredits("u2", 500, 0)
	p.SetPriority("u1", true)

	for i := 0; i < 10; i++ {
		got := p.PickExcluding(map[string]bool{"u1": true})
		if got == nil || got.UID != "u2" {
			t.Fatalf("优先号已被 tried 时应选 u2，实得 %+v", got)
		}
	}
}

// TestPriorityPreferredInFallbackPath 全冷却兜底路径同样「优先号优先」：
// 无 healthy 候选时，优先号即使到期更晚也先被选中（层内仍取最早到期者）。
func TestPriorityPreferredInFallbackPath(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 0, 0)
	p.SetCredits("u2", 0, 0)
	// u2 到期更早（兜底默认会选它）；u1 设优先后应改选 u1。
	p.Cooldown("u1", CoolSoft, 2*time.Hour, "429")
	p.Cooldown("u2", CoolSoft, time.Hour, "429")

	if got := p.Pick(); got == nil || got.UID != "u2" {
		t.Fatalf("未设优先时兜底应选最早到期 u2，实得 %+v", got)
	}
	p.SetPriority("u1", true)
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("设优先后兜底应选 u1，实得 %+v", got)
	}
}

// TestPrioritySessionSlots 新会话候选集的虚拟实例权重：优先号出现
// priorityVirtualSlots 次、普通号 1 次（会话分配据此形成明显偏置）。
func TestPrioritySessionSlots(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 500, 0)
	p.SetCredits("u2", 500, 0)
	p.SetPriority("u1", true)

	count := func(uids []string, uid string) int {
		n := 0
		for _, u := range uids {
			if u == uid {
				n++
			}
		}
		return n
	}
	got := p.WeightedAvailableUIDsForModelRealm("", "")
	if c := count(got, "u1"); c != priorityVirtualSlots {
		t.Fatalf("优先号应占 %d 个虚拟实例，实得 %d（列表 %v）", priorityVirtualSlots, c, got)
	}
	if c := count(got, "u2"); c != 1 {
		t.Fatalf("普通号应占 1 个虚拟实例，实得 %d（列表 %v）", c, got)
	}
}

// TestPriorityReviveKeepsFlag Revive（人工解冻）只清惩罚态，不清偏好：
// priority 不是惩罚维度，解冻后仍应保持优先。
func TestPriorityReviveKeepsFlag(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 500, 0)
	p.SetPriority("u1", true)
	p.Disable("u1", "test")

	if !p.Revive("u1") {
		t.Fatal("Revive 应返回 true")
	}
	if !p.priorityDomain("u1") {
		t.Fatal("Revive 后应保留 priority（偏好不是惩罚态）")
	}
	if st, _ := p.Status("u1"); !st.Priority {
		t.Fatalf("Status 应透出 priority: %+v", st)
	}
}

// TestSetPriorityIdempotentAndPersist 幂等设置 + 落盘/恢复往返 + 旧 state.json 零回归。
func TestSetPriorityIdempotentAndPersist(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"}) // u2 不设优先 → 落盘不应含 priority 字段
	p.SetCredits("u1", 500, 0)
	p.SetCredits("u2", 500, 0)
	p.SetPriority("u1", true)
	p.SetPriority("u1", true) // 幂等：重复设置不报错、状态不变
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, `"priority": true`) {
		t.Fatalf("state.json 应含 priority 字段: %s", s)
	}
	if strings.Count(s, "priority") != 1 {
		t.Fatalf("未设优先的账号不应新增字段（omitempty）: %s", s)
	}

	// 重启恢复：优先开关原样回来。
	p2 := New(fp)
	if st, ok := p2.Status("u1"); !ok || !st.Priority {
		t.Fatalf("重启后应恢复优先开关: %+v ok=%v", st, ok)
	}
	if got := p2.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("重启后优先号应仍独占选号，实得 %+v", got)
	}

	// 旧 state.json（无 priority 字段）：零值加载 → 全员非优先，行为与旧版一致。
	old := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(old, []byte(`{"accounts":{"u9":{"credits":50},"u8":{"credits":50}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p3 := New(old)
	p3.Add(&auth.Auth{UID: "u9"})
	p3.Add(&auth.Auth{UID: "u8"})
	for _, uid := range []string{"u9", "u8"} {
		if p3.priorityDomain(uid) {
			t.Fatalf("旧 state.json 加载后 %s 不应为优先号", uid)
		}
	}
	// 两个非优先号 → 选号在两者间轮换（不会恒选同一个）。
	seen := map[string]bool{}
	for i := 0; i < 40; i++ {
		if got := p3.Pick(); got != nil {
			seen[got.UID] = true
		}
	}
	if !seen["u9"] || !seen["u8"] {
		t.Fatalf("旧 state.json 下应为普通轮换，实得分布 %v", seen)
	}
}
