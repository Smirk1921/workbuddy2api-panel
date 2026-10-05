// 账号分组（group）行为测试：SetGroup 的设置/清空/归一化（TrimSpace + 限长）与幂等、
// statusOf 透出、持久化往返与旧 state.json 零回归，以及组标签不影响选号/冻结/禁用
// 等任何池内行为（纯元数据）。
package pool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// groupDomain 曝露 entry 的 group 原始字段供测试断言（包内私有 helper）。
func (p *Pool) groupDomain(uid string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return ""
	}
	return e.group
}

// TestSetGroupSetClearAndNormalize 设置/清空分组 + 首尾空白归一化 + 超长截断。
func TestSetGroupSetClearAndNormalize(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	p.SetGroup("u1", "  主力  ") // 首尾空白归一化
	if g := p.groupDomain("u1"); g != "主力" {
		t.Fatalf("TrimSpace 后应为「主力」，实得 %q", g)
	}

	// 超长截断（> maxGroupLen 字节）。
	long := strings.Repeat("组", maxGroupLen+10)
	p.SetGroup("u1", long)
	if g := p.groupDomain("u1"); len(g) > maxGroupLen {
		t.Fatalf("超长应截断到 %d 字节，实得 %d", maxGroupLen, len(g))
	}

	// 空串 = 移出分组（幂等：已是空串时不报错）。
	p.SetGroup("u1", "")
	if g := p.groupDomain("u1"); g != "" {
		t.Fatalf("空串应移出分组，实得 %q", g)
	}
	p.SetGroup("u1", "") // 已是空串：幂等，不报错

	// 未知 uid 为空操作。
	p.SetGroup("nobody", "x")
}

// TestGroupIsMetadataOnly 组标签不影响选号/冻结/禁用/优先等任何池内行为：
// 同组的两个账号在选号、冻结阈值、优先上的表现与未分组完全一致。
func TestGroupIsMetadataOnly(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 500, 0)
	p.SetCredits("u2", 500, 0)
	p.SetGroup("u1", "A")
	p.SetGroup("u2", "A")

	// 冻结阈值行为不受组影响：跌破即冻结（与未分组相同）。
	p.SetFreezeThreshold("u1", 1000)
	if !p.internalHealthy("u2") {
		t.Fatal("u2 应可选")
	}
	for i := 0; i < 10; i++ {
		if got := p.Pick(); got == nil || got.UID != "u2" {
			t.Fatalf("组标签不应影响冻结行为：u1 冻结后应选 u2，实得 %+v", got)
		}
	}

	// 优先行为不受组影响。
	p.SetFreezeThreshold("u1", 0) // 解冻
	p.SetPriority("u1", true)
	for i := 0; i < 10; i++ {
		if got := p.Pick(); got == nil || got.UID != "u1" {
			t.Fatalf("组标签不应影响优先行为：u1 优先后应独占选号，实得 %+v", got)
		}
	}
}

// TestGroupStatusExposesGroup statusOf 把组名透到 Status.Group。
func TestGroupStatusExposesGroup(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetGroup("u1", "主力")
	st, ok := p.Status("u1")
	if !ok || st.Group != "主力" {
		t.Fatalf("Status.Group 应为「主力」，实得 %q ok=%v", st.Group, ok)
	}
	// 移出分组后：零值（omitempty，JSON 不含该字段，零回归）。
	p.SetGroup("u1", "")
	if st, _ := p.Status("u1"); st.Group != "" {
		t.Fatalf("清空后 Status.Group 应为空，实得 %q", st.Group)
	}
}

// TestGroupPersistRoundTrip 组名落盘/恢复往返 + 旧 state.json（无 group 字段）零回归。
func TestGroupPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"}) // u2 不分组 → 落盘不应含 group 字段
	p.SetGroup("u1", "主力")
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, `"group": "主力"`) {
		t.Fatalf("state.json 应含 group 字段: %s", s)
	}
	if strings.Count(s, `"group"`) != 1 {
		t.Fatalf("未分组的账号不应新增字段（omitempty）: %s", s)
	}

	// 重启恢复：组名原样回来。
	p2 := New(fp)
	if st, ok := p2.Status("u1"); !ok || st.Group != "主力" {
		t.Fatalf("重启后应恢复组名: %+v ok=%v", st, ok)
	}
	if st, ok := p2.Status("u2"); !ok || st.Group != "" {
		t.Fatalf("u2 应保持未分组: %+v ok=%v", st, ok)
	}

	// 旧 state.json（无 group 字段）：零值加载 → 未分组，行为与旧版一致。
	old := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(old, []byte(`{"accounts":{"u9":{"credits":50}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p3 := New(old)
	p3.Add(&auth.Auth{UID: "u9"})
	if st, _ := p3.Status("u9"); st.Group != "" {
		t.Fatalf("旧 state.json 加载后 u9 应为未分组，实得 %q", st.Group)
	}
}
