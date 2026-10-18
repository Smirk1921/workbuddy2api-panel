package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 默认值必须与引入本开关前的实测口径逐字一致（1050ms / 200ms）——零回归。
func TestTaskPacingDefaults(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.TaskActionGapDur != 1050*time.Millisecond {
		t.Errorf("action gap=%v want 1050ms", c.TaskActionGapDur)
	}
	if c.TaskAccountGapDur != 200*time.Millisecond {
		t.Errorf("account gap=%v want 200ms", c.TaskAccountGapDur)
	}
}

// parseTaskGap：duration 字符串 / 纯数字（按秒）/ 空串回落 / 上限夹断 / 非法与负数报错。
func TestParseTaskGap(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
		bad  bool
	}{
		{"", defaultTaskActionGap, false},
		{"1050ms", 1050 * time.Millisecond, false},
		{"2s", 2 * time.Second, false},
		{"0.35", 350 * time.Millisecond, false}, // 纯数字按秒
		{"0", 0, false},                         // 0 = 不节流
		{"10m", maxTaskGap, false},              // 超上限夹断
		{"-1s", 0, true},
		{"abc", 0, true},
	}
	for _, c := range cases {
		got, err := parseTaskGap("test", c.raw, defaultTaskActionGap)
		if c.bad {
			if err == nil {
				t.Errorf("%q: 期望报错，实得 %v", c.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: 意外报错 %v", c.raw, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q: got %v want %v", c.raw, got, c.want)
		}
	}
}

// 配置文件里的两个键能落到运行时字段；非法值让 Load 快速失败（不静默忽略）。
func TestTaskPacingFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"task_action_gap":"3s","task_account_gap":"500ms"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.TaskActionGapDur != 3*time.Second || c.TaskAccountGapDur != 500*time.Millisecond {
		t.Errorf("action=%v account=%v", c.TaskActionGapDur, c.TaskAccountGapDur)
	}

	os.WriteFile(fp, []byte(`{"schedule":{"task_action_gap":"nope"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("非法 duration 应让 Load 报错（快速失败）")
	}
}
