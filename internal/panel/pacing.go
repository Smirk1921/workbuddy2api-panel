// pacing.go 任务节奏（节流间隔）：把「动作之间 / 账号之间」两个硬编码延迟变成可配置项
// （schedule.task_action_gap / schedule.task_account_gap）。
//
// 设计：包级 atomic 存纳秒，-1 = 未显式配置 → 回落到包级 var 的既有实测默认值
// （reportGap 1050ms / taskSweepAccountGap 200ms）。这样：
//   - 生产通过 SetTaskPacing 注入配置值（支持在线改，下一轮生效）；
//   - 既有测试仍可直接改写 reportGap / taskSweepAccountGap，不需要改一行。
//
// 刻意**不**纳入本开关的间隔（它们不是「节奏」，调了会坏）：
//   - mpChatEventGap（45s+抖动）：小程序对话事件的反作弊硬约束；
//   - claimPollGap/claimPollAttempts：异步计分回读的轮询预算；
//   - expertSummonGap：按实测成功率校准的固定值。
package panel

import (
	"sync/atomic"
	"time"
)

// gapUnset 表示「未显式配置」，回落到包级默认值。
const gapUnset int64 = -1

var (
	actionGapNS  atomic.Int64
	accountGapNS atomic.Int64
)

func init() {
	actionGapNS.Store(gapUnset)
	accountGapNS.Store(gapUnset)
}

// SetTaskPacing 热更新任务节奏（启动装配与面板保存配置时调用）。负值按 0 处理。
func SetTaskPacing(action, account time.Duration) {
	if action < 0 {
		action = 0
	}
	if account < 0 {
		account = 0
	}
	actionGapNS.Store(int64(action))
	accountGapNS.Store(int64(account))
}

// actionGap 返回生效的「动作/上报之间」间隔。
func actionGap() time.Duration {
	if v := actionGapNS.Load(); v >= 0 {
		return time.Duration(v)
	}
	return reportGap
}

// accountGap 返回生效的「账号之间」间隔。
func accountGap() time.Duration {
	if v := accountGapNS.Load(); v >= 0 {
		return time.Duration(v)
	}
	return taskSweepAccountGap
}
