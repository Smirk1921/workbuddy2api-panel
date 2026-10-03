// 低积分自动冻结（frozen）行为测试：阈值设置触发的立即冻结/解冻、余额变化驱动的
// 自动冻结/自动解冻、与冷却/熔断/禁用维度的正交性、持久化往返与旧 state.json 零回归，
// 以及并发下的收敛不变式。
package pool

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// freezeDomain 曝露 entry 的冻结域原始字段供测试断言（包内私有 helper）。
func (p *Pool) freezeDomain(uid string) (threshold int64, frozen bool, reason string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return 0, false, ""
	}
	return e.freezeThreshold, e.frozen, e.frozenReason
}

// internalHealthyForModel 曝露 entry.healthyForModel 供测试断言（包内私有 helper）。
func (p *Pool) internalHealthyForModel(uid, model string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	return e.healthyForModel(time.Now(), model)
}

// internalModelExempt 曝露 entry.modelExempt 供测试断言（包内私有 helper）。
// 该谓词是探活（ServableForRealm）的"模型豁免"分支，与选号侧 healthyForModel 同口径。
func (p *Pool) internalModelExempt(uid string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	return e.modelExempt()
}

// TestSetFreezeThresholdImmediateFreezeAndPickSkip 阈值高于当前余额 → 立即冻结；
// 冻结号退出选号：账号级与模型级健康判定均不可选，Pick 永不选中。
func TestSetFreezeThresholdImmediateFreezeAndPickSkip(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 10, 0)
	p.SetCredits("u2", 10, 0)

	p.SetFreezeThreshold("u1", 100) // 10 < 100 → 立即冻结

	st, ok := p.Status("u1")
	if !ok || !st.Frozen || st.FreezeThreshold != 100 || st.FrozenReason != freezeReason {
		t.Fatalf("应呈现低积分冻结（frozen/threshold/reason）: %+v ok=%v", st, ok)
	}
	if st.Cooling {
		t.Errorf("冻结不是冷却，不应置 cooling: %+v", st)
	}
	if p.internalHealthy("u1") {
		t.Fatal("冻结号不应 healthy")
	}
	if p.internalHealthyForModel("u1", "glm-5.3") {
		t.Fatal("冻结号对任意模型都不应 healthy（冻结不是模型级限流，无模型豁免）")
	}
	for i := 0; i < 50; i++ {
		if got := p.Pick(); got == nil || got.UID != "u2" {
			t.Fatalf("冻结号不得被选中: %+v", got)
		}
	}
	// 未设阈值的账号状态 JSON 不含新字段（omitempty 零回归的口径）。
	st2, _ := p.Status("u2")
	if st2.FreezeThreshold != 0 || st2.Frozen || st2.FrozenReason != "" {
		t.Errorf("未开启冻结的账号应保持零值: %+v", st2)
	}
}

// TestFreezeFrozenExcludedFromFallback 全冷却兜底路径（池内无 healthy 候选）同样不得
// 选中冻结号：freezeLocked 刻意保留 breakerUntil/degradeUntil（transition.go），这两个
// 截止都计入 expiry()，若不显式排除，「冻结中且熔断/降权未到期」的号会在兜底被选中并
// 打到上游——已知余额低于阈值，比 CoolHard 更确定必 402。
func TestFreezeFrozenExcludedFromFallback(t *testing.T) {
	// 场景 A：冻结号 + 未到期熔断；池内唯一另一号禁用（不参与兜底）→ 兜底无候选。
	t.Run("breaker", func(t *testing.T) {
		p := New("")
		p.Add(&auth.Auth{UID: "u1"}) // 冻结号（带未到期熔断）
		p.Add(&auth.Auth{UID: "u2"}) // 唯一另一号：禁用
		p.SetCredits("u1", 10, 0)
		p.SetCredits("u2", 10, 0)
		p.SetBreaker(1, time.Hour, time.Hour)
		p.NoteError("u1") // 阈值 1 → 立即熔断（breakerUntil 未到期）
		p.SetFreezeThreshold("u1", 100)
		p.Disable("u2", "manual disable")

		if bt, ok := p.breakerUntil("u1"); !ok || bt.IsZero() || !time.Now().Before(bt) {
			t.Fatalf("precondition: 冻结应保留未到期的 breakerUntil: %v ok=%v", bt, ok)
		}
		if got := p.Pick(); got != nil {
			t.Fatalf("冻结号不得参与全冷却兜底（会打到上游必 402）: %+v", got)
		}
	})
	// 场景 B：冻结号 + 未到期连败降权（同样被 freezeLocked 保留）。
	t.Run("degrade", func(t *testing.T) {
		p := New("")
		p.Add(&auth.Auth{UID: "u1"})
		p.Add(&auth.Auth{UID: "u2"})
		p.SetCredits("u1", 10, 0)
		p.SetCredits("u2", 10, 0)
		for i := 0; i < defaultDegradeThreshold; i++ {
			p.NoteFailures("u1") // 连败达阈 → degradeUntil 未到期
		}
		p.SetFreezeThreshold("u1", 100)
		p.Disable("u2", "manual disable")

		st, _ := p.Status("u1")
		if st.DegradeUntil.IsZero() || !time.Now().Before(st.DegradeUntil) {
			t.Fatalf("precondition: 冻结应保留未到期的 degradeUntil: %+v", st)
		}
		if got := p.Pick(); got != nil {
			t.Fatalf("冻结号不得参与全冷却兜底（会打到上游必 402）: %+v", got)
		}
	})
}

// TestFreezeServableExcludesFrozenWithModelCooldown 探活口径：冻结号可能在冻结**之后**
// 被在途请求回写模型级冷却（CooldownSoftForModel/BlockModelBackoff 不判 frozen，
// 条目存活至 2h/24h），此时 ServableForRealm / ServableNow 不得报「可服务」——
// 否则 /healthz（含 cn/global 分域）200 而 chat 因 pick 无候选 503。
func TestFreezeServableExcludesFrozenWithModelCooldown(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 10, 0)
	p.SetFreezeThreshold("u1", 100) // 冻结（同时清空 modelCooldowns）
	// 冻结后仍可能在途的 6004 响应回写模型级冷却（写入路径无 frozen 过滤）。
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "glm-5.3", "6004 rate limit")

	if p.internalModelExempt("u1") {
		t.Fatal("冻结号不得处于模型豁免形态（探活会谎报可服务）")
	}
	if p.ServableNow() {
		t.Fatal("冻结号不应让池子呈现可服务（/healthz 200 而 chat 503）")
	}
	if p.ServableForRealm("cn") {
		t.Fatal("cn 域分域探活同样不得报可服务")
	}
	// 解冻（余额恢复）后模型级冷却仍在 → 模型豁免恢复、探活恢复（口径与 healthyForModel 一致）。
	p.ReenableIfCredits("u1", 500, 0)
	if !p.internalModelExempt("u1") {
		t.Fatal("解冻后仍持有模型级软冷却 → 应恢复模型豁免形态")
	}
	if !p.ServableNow() {
		t.Fatal("解冻后该号对其他模型可选 → 探活应报可服务")
	}
}

// TestFreezeSetCreditsAutoFreezeUnfreeze 单号余额刷新（SetCredits，面板「余额」按钮）
// 与全量刷新同口径：余额跌破阈值即冻结、回到阈值以上即解冻。
// 此前该路径不判冻结（只有 SetCreditsDetailed/ReenableIfCredits 判），会出现
// frozen=true 而 credits>=阈值 的不自洽状态，靠周期刷新才自愈。
func TestFreezeSetCreditsAutoFreezeUnfreeze(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 500, 0)
	p.SetFreezeThreshold("u1", 100)
	if st, _ := p.Status("u1"); st.Frozen {
		t.Fatalf("precondition: 500 >= 100 不应冻结: %+v", st)
	}

	p.SetCredits("u1", 50, 0) // 跌破阈值 → 自动冻结
	st, _ := p.Status("u1")
	if !st.Frozen || st.FrozenReason != freezeReason || st.Credits != 50 {
		t.Fatalf("单号余额刷新跌破阈值应自动冻结: %+v", st)
	}
	if p.internalHealthy("u1") {
		t.Fatal("冻结号不应 healthy")
	}

	p.SetCredits("u1", 2000, 0) // 恢复到阈值以上 → 自动解冻
	st, _ = p.Status("u1")
	if st.Frozen || st.FrozenReason != "" {
		t.Fatalf("单号余额刷新恢复到阈值以上应自动解冻: %+v", st)
	}
	if !p.internalHealthy("u1") {
		t.Fatal("解冻后应恢复可选")
	}
}

// TestFreezeAutoUnfreezeOnBalanceRefresh 余额刷新（签到/周期刷新）驱动的自动解冻：
// 恢复到阈值以上 → 解冻回池；再次跌破阈值 → 重新冻结。
func TestFreezeAutoUnfreezeOnBalanceRefresh(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 10, 0)
	p.SetFreezeThreshold("u1", 100)
	if st, _ := p.Status("u1"); !st.Frozen {
		t.Fatal("precondition: 余额 10 < 阈值 100 应已冻结")
	}

	// ReenableIfCredits（签到/余额刷新路径）：恢复到阈值以上 → 自动解冻。
	p.ReenableIfCredits("u1", 500, 0)
	st, _ := p.Status("u1")
	if st.Frozen || st.FrozenReason != "" {
		t.Fatalf("余额恢复到阈值以上应自动解冻: %+v", st)
	}
	if !p.internalHealthy("u1") {
		t.Fatal("解冻后应恢复可选")
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("解冻后应能被选中: %+v", got)
	}

	// SetCreditsDetailed（余额刷新路径）：跌破阈值 → 自动冻结。
	p.SetCreditsDetailed("u1", 5, 0, 0, time.Time{}, 0)
	st, _ = p.Status("u1")
	if !st.Frozen || st.FrozenReason != freezeReason {
		t.Fatalf("余额跌破阈值应自动冻结: %+v", st)
	}
	if p.internalHealthy("u1") {
		t.Fatal("自动冻结后不应 healthy")
	}
}

// TestFreezeOnModelCostConsumption 实测扣费压低余额（NoteModelCost）驱动的自动冻结：
// 消费到阈值以下即冻结，无需等签到/余额刷新。
func TestFreezeOnModelCostConsumption(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100, 0)
	p.SetFreezeThreshold("u1", 80)
	if st, _ := p.Status("u1"); st.Frozen {
		t.Fatal("precondition: 100 >= 80 不应冻结")
	}

	p.NoteModelCost("u1", "glm-5.3", 30, 1000) // 扣 30 → 70 < 80 → 冻结
	st, _ := p.Status("u1")
	if st.Credits != 70 {
		t.Fatalf("credits=%d want 70", st.Credits)
	}
	if !st.Frozen {
		t.Fatalf("消费跌破阈值应自动冻结: %+v", st)
	}
	if p.internalHealthy("u1") {
		t.Fatal("冻结号不应 healthy")
	}

	// 免费请求（credit=0）不动余额，也不改变冻结态。
	p.NoteModelCost("u1", "glm-5.3", 0, 1000)
	if st, _ := p.Status("u1"); !st.Frozen || st.Credits != 70 {
		t.Fatalf("免费请求不应改变余额/冻结态: %+v", st)
	}
}

// TestFreezeThresholdOffAndLowering 关阈值（<=0）立即清冻结态且不再冻结；
// 阈值调低到当前余额以下时立即解冻（无须等下一次余额刷新）。
func TestFreezeThresholdOffAndLowering(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 10, 0)
	p.SetFreezeThreshold("u1", 100)
	if st, _ := p.Status("u1"); !st.Frozen {
		t.Fatal("precondition: 应已冻结")
	}

	// 关阈值 = 0：清冻结态，且余额仍在阈值以下也不再冻结。
	p.SetFreezeThreshold("u1", 0)
	st, _ := p.Status("u1")
	if st.Frozen || st.FrozenReason != "" || st.FreezeThreshold != 0 {
		t.Fatalf("关阈值应清冻结态: %+v", st)
	}
	if !p.internalHealthy("u1") {
		t.Fatal("关阈值后应恢复可选")
	}
	p.SetCreditsDetailed("u1", 10, 0, 0, time.Time{}, 0) // 余额变更也不再触发冻结（阈值 0 = 关闭）
	if st, _ := p.Status("u1"); st.Frozen {
		t.Fatalf("阈值 0 = 关闭：不得再冻结: %+v", st)
	}

	// 负值同关闭语义。
	p.SetFreezeThreshold("u1", -5)
	if st, _ := p.Status("u1"); st.Frozen {
		t.Fatalf("负阈值同关闭: %+v", st)
	}

	// 阈值调低到当前余额（10）以下且账号已冻结 → 立即解冻。
	p.SetFreezeThreshold("u1", 100)
	if st, _ := p.Status("u1"); !st.Frozen {
		t.Fatal("precondition: 阈值 100 > 余额 10 应已冻结")
	}
	p.SetFreezeThreshold("u1", 5) // 10 >= 5 且 frozen → 立即解冻
	if st, _ := p.Status("u1"); st.Frozen {
		t.Fatalf("阈值调低后应立即解冻: %+v", st)
	}
}

// TestFreezeClearsCoolingKeepsBreakerAndCounters 正交性：冻结清冷却域
// （until/coolKind/reason/softStreak/modelCooldowns），但保留熔断观测
// （breakerUntil）与 sessionDeadFails/consecutiveFails；解冻也不解除熔断。
func TestFreezeClearsCoolingKeepsBreakerAndCounters(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 10, 0)
	// 熔断域：1 次 NoteError 即熔断（threshold=1）。
	p.SetBreaker(1, time.Hour, time.Hour)
	p.NoteError("u1")
	// 连败计数与 12153 计数：各 1 次（未达阈，仅作观测保留断言）。
	p.NoteFailures("u1")
	if p.NoteSessionDead("u1") {
		t.Fatal("1 次 NoteSessionDead 不应达禁用阈值")
	}
	// 冷却域：软冷却 + 6004 模型级冷却 + 固定时长冷却（softStreak 累计）。
	p.Cooldown("u1", CoolSoft, 600*time.Second, "429")
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")

	p.SetFreezeThreshold("u1", 100)

	st, _ := p.Status("u1")
	if !st.Frozen || st.FrozenReason != freezeReason {
		t.Fatalf("应冻结: %+v", st)
	}
	until, kind, reason, streak, mc := coolingDomain(t, p, "u1")
	if !until.IsZero() || kind != 0 || reason != "" || streak != 0 || mc != 0 {
		t.Errorf("冻结应清冷却域: until=%v kind=%v reason=%q streak=%d modelCooldowns=%d",
			until, kind, reason, streak, mc)
	}
	if bt, ok := p.breakerUntil("u1"); !ok || bt.IsZero() {
		t.Fatal("冻结不得清熔断观测（breakerUntil）")
	}
	p.mu.RLock()
	e := p.byUID["u1"]
	sessionDeadFails, consecutiveFails := e.sessionDeadFails, e.consecutiveFails
	p.mu.RUnlock()
	if sessionDeadFails != 1 || consecutiveFails != 1 {
		t.Errorf("冻结不得清 sessionDeadFails/consecutiveFails: sessionDeadFails=%d consecutiveFails=%d",
			sessionDeadFails, consecutiveFails)
	}

	// 余额恢复 → 解冻，但熔断仍在（独立信号，到期/成功才恢复）→ 仍不可选。
	p.ReenableIfCredits("u1", 500, 0)
	if st, _ := p.Status("u1"); st.Frozen {
		t.Fatalf("余额恢复应解冻: %+v", st)
	}
	if p.internalHealthy("u1") {
		t.Fatal("解冻不等于可选：熔断（breakerUntil）仍在，healthy 应为 false")
	}
}

// TestFreezeReviveClearsFrozen 人工复活（Revive）清冻结态但保留阈值配置。
func TestFreezeReviveClearsFrozen(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 10, 0)
	p.SetFreezeThreshold("u1", 100)
	if st, _ := p.Status("u1"); !st.Frozen {
		t.Fatal("precondition: 应已冻结")
	}
	if !p.Revive("u1") {
		t.Fatal("Revive 应返回 true")
	}
	st, _ := p.Status("u1")
	if st.Frozen || st.FrozenReason != "" {
		t.Fatalf("Revive 应清冻结态: %+v", st)
	}
	if st.FreezeThreshold != 100 {
		t.Fatalf("Revive 不应改阈值: %d", st.FreezeThreshold)
	}
	if !p.internalHealthy("u1") {
		t.Fatal("Revive 后应恢复可选")
	}
}

// TestFreezePersistRoundTrip 持久化往返：冻结态落盘并在重启后恢复（不重新参与选号）；
// 未开启冻结的账号不新增字段；旧 state.json（无冻结字段）加载零行为变化。
func TestFreezePersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"}) // u2 不设阈值 → 落盘不应含冻结字段
	p.SetCredits("u1", 10, 0)
	p.SetCredits("u2", 10, 0)
	p.SetFreezeThreshold("u1", 100)
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, `"freeze_threshold": 100`) ||
		!strings.Contains(s, `"frozen": true`) ||
		!strings.Contains(s, freezeReason) {
		t.Fatalf("state.json 应含冻结字段: %s", s)
	}
	if strings.Count(s, "freeze_threshold") != 1 || strings.Count(s, `"frozen"`) != 1 {
		t.Fatalf("未开启冻结的账号不应新增字段（omitempty）: %s", s)
	}

	// 重启恢复：冻结态原样回来（不重新参与选号）。
	p2 := New(fp)
	st, ok := p2.Status("u1")
	if !ok || !st.Frozen || st.FreezeThreshold != 100 || st.FrozenReason != freezeReason {
		t.Fatalf("重启后应恢复冻结态: %+v ok=%v", st, ok)
	}
	if p2.internalHealthy("u1") {
		t.Fatal("重启后冻结号不得可选")
	}

	// 旧 state.json（无冻结字段）：零值加载、照常可选（向后兼容）。
	old := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(old, []byte(`{"accounts":{"u9":{"credits":50}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p3 := New(old)
	p3.Add(&auth.Auth{UID: "u9"})
	st9, ok := p3.Status("u9")
	if !ok || st9.Frozen || st9.FreezeThreshold != 0 || st9.FrozenReason != "" {
		t.Fatalf("旧文件应零值加载: %+v ok=%v", st9, ok)
	}
	if !p3.internalHealthy("u9") {
		t.Fatal("旧文件加载的账号不应因冻结逻辑变得不可选")
	}
}

// TestFreezeLoadReconcilesInconsistentState 恢复侧幂等对账：历史脏数据
// （frozen=true 而 credits 已达阈值 / 阈值已关闭）在加载时自愈，不跨重启保留；
// 合法的冻结态（threshold>0 且 credits<threshold）照常恢复。
func TestFreezeLoadReconcilesInconsistentState(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	raw := `{"accounts":{
	  "dirty_high":{"credits":500,"freeze_threshold":100,"frozen":true,"frozen_reason":"低积分自动冻结"},
	  "dirty_off":{"credits":10,"frozen":true,"frozen_reason":"低积分自动冻结"},
	  "clean":{"credits":10,"freeze_threshold":100,"frozen":true,"frozen_reason":"低积分自动冻结"}
	}}`
	if err := os.WriteFile(fp, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)

	if st, _ := p.Status("dirty_high"); st.Frozen || st.FrozenReason != "" {
		t.Fatalf("余额已达阈值的脏冻结态应自愈（解冻）: %+v", st)
	}
	if st, _ := p.Status("dirty_off"); st.Frozen || st.FrozenReason != "" {
		t.Fatalf("阈值关闭（0）的残留冻结态应自愈: %+v", st)
	}
	if st, _ := p.Status("clean"); !st.Frozen || st.FrozenReason != freezeReason || st.FreezeThreshold != 100 {
		t.Fatalf("合法冻结态应原样恢复: %+v", st)
	}
}

// TestFreezeCountsDetailedSeparatesFrozenFromCooling 汇总计数：冻结号单列 frozen，
// 不与 cooling 混计（冻结无倒计时，运维需从汇总看出冻结规模）；分类互斥且
// disabled > frozen > cooling > healthy。
func TestFreezeCountsDetailedSeparatesFrozenFromCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "f1"}) // 冻结
	p.Add(&auth.Auth{UID: "c1"}) // 冷却
	p.Add(&auth.Auth{UID: "d1"}) // 禁用（且余额低于自身阈值 → 仍计入 disabled）
	p.Add(&auth.Auth{UID: "h1"}) // 健康
	for _, uid := range []string{"f1", "c1", "d1", "h1"} {
		p.SetCredits(uid, 10, 0)
	}
	p.SetFreezeThreshold("f1", 100)
	p.SetFreezeThreshold("d1", 100)
	p.Cooldown("c1", CoolSoft, time.Hour, "429")
	p.Disable("d1", "manual disable")

	total, healthy, cooling, frozen, disabled, inFlightFull := p.CountsDetailed()
	if total != 4 || healthy != 1 || cooling != 1 || frozen != 1 || disabled != 1 || inFlightFull != 0 {
		t.Fatalf("counts: total=%d healthy=%d cooling=%d frozen=%d disabled=%d inFlightFull=%d (want 4/1/1/1/1/0)",
			total, healthy, cooling, frozen, disabled, inFlightFull)
	}
	total, healthy, cooling, frozen, disabled, _ = p.CountsDetailedForRealm("cn")
	if total != 4 || healthy != 1 || cooling != 1 || frozen != 1 || disabled != 1 {
		t.Fatalf("realm counts: total=%d healthy=%d cooling=%d frozen=%d disabled=%d (want 4/1/1/1/1)",
			total, healthy, cooling, frozen, disabled)
	}
	// 解冻后回 healthy，frozen 归零（不会同时出现在 cooling 里）。
	p.Revive("f1")
	_, healthy, cooling, frozen, _, _ = p.CountsDetailed()
	if healthy != 2 || cooling != 1 || frozen != 0 {
		t.Fatalf("解冻后: healthy=%d cooling=%d frozen=%d (want 2/1/0)", healthy, cooling, frozen)
	}
}

// TestFreezeConcurrentCreditsAndThreshold 并发不变式：多 goroutine 并发改余额/阈值后，
// 冻结态与（阈值, 余额）始终一致——frozen ⇔ 阈值 > 0 且 credits < 阈值，
// 冻结号一律不可选、原因恒为固定文案（-race 下验证无数据竞争）。
func TestFreezeConcurrentCreditsAndThreshold(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 500, 0)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 4 {
			case 0:
				p.SetCreditsDetailed("u1", int64(10+i), 0, 0, time.Time{}, 0)
			case 1:
				p.SetCreditsDetailed("u1", int64(1000+i), 0, 0, time.Time{}, 0)
			case 2:
				p.SetFreezeThreshold("u1", int64(100+i))
			case 3:
				p.SetFreezeThreshold("u1", 0)
			}
		}(i)
	}
	wg.Wait()

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("uid missing")
	}
	thr, frozen, reason := p.freezeDomain("u1")
	if frozen != (thr > 0 && st.Credits < thr) {
		t.Fatalf("冻结态与余额/阈值不一致: frozen=%v threshold=%d credits=%d", frozen, thr, st.Credits)
	}
	if frozen {
		if reason != freezeReason {
			t.Fatalf("冻结原因应为固定文案: %q", reason)
		}
		if p.internalHealthy("u1") {
			t.Fatal("冻结号不得 healthy")
		}
	} else if reason != "" {
		t.Fatalf("未冻结时原因应为空: %q", reason)
	}
}
