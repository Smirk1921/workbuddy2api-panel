// pacing.go 任务间节流（可配置）：旅行 / 活跃上报 / 夜猫子的账号间限速，以及
// 领养前置上报后的等待间隔。
//
// 与 panel/pacing.go 同款设计：包级 atomic 存纳秒，-1 = 未显式配置 → 回落到
// 各任务族的既有实测默认值（800ms / 800ms / 1050ms）。既有测试仍可直接改写
// travelAccountDelay / activityAccountDelay / adoptReportGap，无需改动。
package scheduler

import (
	"sync/atomic"
	"time"
)

// taskGapUnset 表示「未显式配置」，回落到包级默认值。
const taskGapUnset int64 = -1

var (
	taskActionGapNS  atomic.Int64
	taskAccountGapNS atomic.Int64
)

func init() {
	taskActionGapNS.Store(taskGapUnset)
	taskAccountGapNS.Store(taskGapUnset)
}

// SetTaskDelays 热更新任务节奏（启动装配与面板保存配置时调用）。
// actionGap 用于领养前置上报后的等待；accountGap 用于旅行 / 活跃上报 / 夜猫子的
// 账号间限速。负值按 0 处理；调度器内部若被热改，下一轮循环即生效。
func (s *Scheduler) SetTaskDelays(actionGap, accountGap time.Duration) {
	if actionGap < 0 {
		actionGap = 0
	}
	if accountGap < 0 {
		accountGap = 0
	}
	taskActionGapNS.Store(int64(actionGap))
	taskAccountGapNS.Store(int64(accountGap))
}

// travelDelay 旅行账号间的限速。
func travelDelay() time.Duration {
	if v := taskAccountGapNS.Load(); v >= 0 {
		return time.Duration(v)
	}
	return travelAccountDelay
}

// activityDelay 活跃上报 / 夜猫子的账号间限速。
func activityDelay() time.Duration {
	if v := taskAccountGapNS.Load(); v >= 0 {
		return time.Duration(v)
	}
	return activityAccountDelay
}

// adoptDelay 领养前置上报后、发 buddy/first 之前的等待。
func adoptDelay() time.Duration {
	if v := taskActionGapNS.Load(); v >= 0 {
		return time.Duration(v)
	}
	return adoptReportGap
}
