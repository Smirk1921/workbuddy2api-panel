// tasksweep.go 成长任务周期清扫：把「进度已达标、但没人去领」的积分补回来。
//
// 为什么需要（用户实测现场）：新号跑成长任务，任务进度其实已经上报生效
// （服务端 progress 达标），但积分停在 2000/2100/2300 上不去 4000——差的正是
// 「领奖」这一步。既有链路只有 schedule.growth_hours（默认 01:00）的成长队列，
// 而它只处理「被扫进队列的那些任务」，两类场景覆盖不到：
//
//	(a) 用户在代理里真实对话点亮的任务——服务端进度自己涨，面板并不知情，
//	    没人去 claim；
//	(b) 历史上领奖端点故障期间漏领的达标任务——任务早已 completed，只是没人领。
//
// 本清扫只做两件事，且全部幂等，不跑任何行为链路（那是 RunGrowthQueueOnce 的职责）：
//
//  1. accept 所有 not_accepted 的待办任务（默认口径 + 小程序口径分别提交）；
//  2. claim 所有「进度达标且未领取」的任务（mp 码走 mp 口径，其余走 Web 口径）。
//
// 开销：每账号 2 次列表 + 少量领奖请求，故默认 15 分钟一轮
// （schedule.task_sweep_minutes，schedule.task_sweep_enabled 可关）。
package panel

import (
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// taskSweepAccountGap 账号之间的节流：清扫是周期性的，没必要把上游打满。
// 测试可置 0（与 reportGap 同口径）。
var taskSweepAccountGap = 200 * time.Millisecond

// taskSweepStats 一轮清扫的汇总（供日志与测试断言）。
type taskSweepStats struct {
	Accounts int   `json:"accounts"` // 实际清扫的账号数
	Skipped  int   `json:"skipped"`  // 因 per-account 锁被占而跳过的账号数
	Accepted int   `json:"accepted"` // 新接受的任务数
	Claimed  int   `json:"claimed"`  // 新领取的任务数
	Credits  int64 `json:"credits"`  // 本轮新到账积分
	Energy   int64 `json:"energy"`   // 本轮新到账能量
	Errors   int   `json:"errors"`   // 列表/领奖出错的账号数
}

// RunTaskSweepOnce 对所有可用账号做一轮幂等清扫。供后台周期任务
// （scheduler.StartTaskSweep → SetSweepHook）调用。
func (p *Panel) RunTaskSweepOnce() {
	if p.cfg.Upstream == nil {
		return
	}
	st := p.sweepAllAccounts()
	if st.Accepted == 0 && st.Claimed == 0 && st.Errors == 0 {
		// 无事可做不打日志：15 分钟一条「什么都没发生」会淹没日志。
		return
	}
	log.Printf("panel: 任务清扫完成：清扫 %d 个账号（跳过 %d）· 接受 %d · 领奖 %d（+%d 分 +%d 能）· 出错 %d",
		st.Accounts, st.Skipped, st.Accepted, st.Claimed, st.Credits, st.Energy, st.Errors)
}

// sweepAllAccounts 遍历池内可用账号执行清扫，返回汇总。
// 串行执行（不清扫是延迟敏感路径，15 分钟一轮不值得为它引并发）；
// 账号间 200ms 节流，42 账号最坏约一分钟。
func (p *Panel) sweepAllAccounts() taskSweepStats {
	var st taskSweepStats
	for _, s := range p.cfg.Pool.List() {
		if s.Disabled {
			continue
		}
		a := p.cfg.Pool.AuthByUID(s.UID)
		if a == nil {
			continue
		}
		// 门控与任务中心扫描一致：global 账号无 CN 成长任务体系；企业版无个人成长
		// 体系（GET /v2/activity/growth/tasks 上游 403）。两者都不发起任何上游调用。
		if a.IsGlobal() || a.IsEnterprise() {
			continue
		}
		// 与单任务 / 一键完成 / 成长队列共用 per-account 锁：正在跑动作的账号本轮
		// 跳过——它自己那条链路本来就会 accept + claim。
		if !p.tryLockAccount(s.UID) {
			st.Skipped++
			continue
		}
		accepted, claimed, credits, energy, err := p.sweepAccount(a)
		p.unlockAccount(s.UID)

		st.Accounts++
		st.Accepted += accepted
		st.Claimed += claimed
		st.Credits += credits
		st.Energy += energy
		if err != nil {
			st.Errors++
			log.Printf("panel: 任务清扫 uid=%s: %v", s.UID, err)
		}
		if g := accountGap(); g > 0 {
			time.Sleep(g)
		}
	}
	return st
}

// sweepAccount 单账号清扫：返回（新接受数, 新领取数, 新增积分, 新增能量, 首个错误）。
//
// 列表拉取失败即整体失败（没有任务表就无从判断）；accept 与单项领奖失败都只记日志
// 不中断——下一轮清扫会再试（幂等）。
func (p *Panel) sweepAccount(a *auth.Auth) (accepted, claimed int, credits, energy int64, err error) {
	tasks, lerr := p.cfg.Upstream.ListTasks(a)
	if lerr != nil {
		return 0, 0, 0, 0, lerr
	}
	// 小程序口径任务只在 mp 列表下发（mp 列表是默认口径的超集），合并时按
	// task_code 去重；mp 列表失败静默（无 mp 任务的部署/活动结束时零影响）。
	if mpTasks, mperr := p.cfg.Upstream.ListTasksMP(a); mperr == nil {
		seen := make(map[string]bool, len(tasks))
		for _, t := range tasks {
			seen[t.TaskCode] = true
		}
		for _, t := range mpTasks {
			if !seen[t.TaskCode] {
				tasks = append(tasks, t)
			}
		}
	}

	// 阶段 1：accept 尚未接受的任务。默认口径与 mp 口径端标记不同，分开提交。
	// 过滤条件与既有队列/一键完成同口径（claimed / locked / 已接受 / 已完成都不重报）。
	var plainCodes, mpCodes []string
	for _, t := range tasks {
		if t.Claimed || t.Locked || t.AcceptStatus == "accepted" || t.AcceptStatus == "completed" {
			continue
		}
		if isMPTaskCode(t.TaskCode) {
			mpCodes = append(mpCodes, t.TaskCode)
		} else {
			plainCodes = append(plainCodes, t.TaskCode)
		}
	}
	if len(plainCodes) > 0 {
		if aerr := p.cfg.Upstream.AcceptTasks(a, plainCodes); aerr != nil {
			log.Printf("panel: 任务清扫 accept uid=%s: %v（不阻塞领奖）", a.UID, aerr)
		} else {
			accepted += len(plainCodes)
		}
	}
	if len(mpCodes) > 0 {
		if aerr := p.cfg.Upstream.AcceptTasksMP(a, mpCodes); aerr != nil {
			log.Printf("panel: 任务清扫 accept-mp uid=%s: %v（不阻塞领奖）", a.UID, aerr)
		} else {
			accepted += len(mpCodes)
		}
	}
	if accepted > 0 {
		// 刚 accept 的任务要给上游状态流转留时间再领（reportGap 是既有实测口径）。
		// 只在真的接受了任务时才等——绝大多数轮次 accepted==0，不拖慢清扫。
		time.Sleep(actionGap())
	}

	// 阶段 2：领「进度达标且未领取」的奖。
	//
	// 判据只用 progress（Task.Claimable），**不**拿 accept_status=="completed" 当依据：
	// 实测它会在回读时短暂转 completed 随后被判定无效整体回滚（见 autotask.go 记载），
	// 据此领奖会误领。领奖本身幂等（重复领返回 already_claimed → 0 分 0 能），
	// 所以宁可多试一轮也不漏领。
	for _, t := range tasks {
		if !t.Claimable || t.Locked {
			continue
		}
		var c, e int64
		var cerr error
		if isMPTaskCode(t.TaskCode) {
			c, e, cerr = p.cfg.Upstream.ClaimRewardMP(a, t.TaskCode)
		} else {
			c, e, cerr = p.cfg.Upstream.ClaimReward(a, t.TaskCode)
		}
		if cerr != nil {
			log.Printf("panel: 任务清扫 claim uid=%s code=%s: %v", a.UID, t.TaskCode, cerr)
			if err == nil {
				err = cerr
			}
			continue
		}
		if c > 0 || e > 0 {
			claimed++
			credits += c
			energy += e
			log.Printf("panel: 任务清扫补领 uid=%s code=%s: +%d 分 +%d 能", a.UID, t.TaskCode, c, e)
		}
	}
	return accepted, claimed, credits, energy, err
}
