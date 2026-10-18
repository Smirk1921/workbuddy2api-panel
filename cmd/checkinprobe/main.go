// checkinprobe 国际版（global realm）签到能力探测：验证「登录即签到」是否成立。
//
// 只读设计（与 cmd/probe 同口径）：用 auths 里现有的 access token，**不刷新
// token、不写任何文件**——避免与正在运行的实例争抢一次性 refresh token。
// 刻意不用 auth.LoadDir：它会对缺 realm 的旧文件做 backfill 并落盘。
//
// 探测顺序（每步都打印结果与余额，便于判断「到底哪一步给了积分」）：
//  1. 余额基线
//  2. 活跃上报 /v2/report（调度器每天本来就会做一次，幂等）
//  3. 余额（上报后）—— 看「登录/活跃」本身是否给积分
//  4. 签到 POST /billing/meter/daily-checkin（global 路径优先，404 自动回退 /v2/...）
//  5. 余额（签到后）
//  6. 连登 /v2/activity/growth/streak
//
// 用法（项目根目录）：
//
//	go run ./cmd/checkinprobe -dir ./auths              # 挑第一个 global 账号
//	go run ./cmd/checkinprobe -dir ./auths -uid <uid>
//	go run ./cmd/checkinprobe -auth ./auths/workbuddy-xxx.json
//	go run ./cmd/checkinprobe -dir ./auths -uid <uid> -report=false
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func main() {
	authPath := flag.String("auth", "", "单个 auth 文件路径")
	dir := flag.String("dir", "./auths", "auths 目录（只读）")
	uid := flag.String("uid", "", "只探测该 uid（配合 -dir；缺省取第一个 global 账号）")
	doReport := flag.Bool("report", true, "先发一次活跃上报 /v2/report（调度器每天本来就会做）")
	timeout := flag.Duration("timeout", 60*time.Second, "单请求超时")
	flag.Parse()

	a, err := loadOne(*authPath, *dir, *uid)
	if err != nil {
		fmt.Println("加载账号失败:", err)
		os.Exit(1)
	}

	c := upstream.New()
	c.HTTP.Timeout = *timeout

	fmt.Println("=== 国际版签到能力探测 ===")
	fmt.Printf("账号：uid=%s  nick=%s  realm=%s  domain=%s\n\n",
		a.UID, a.Nickname, a.Realm(), a.DomainValue())

	credit := func(label string) int64 {
		remain, total, err := c.UserResource(a)
		if err != nil {
			fmt.Printf("%-24s 查询失败: %v\n", label, err)
			return -1
		}
		if total > 0 {
			fmt.Printf("%-24s %d / %d\n", label, remain, total)
		} else {
			fmt.Printf("%-24s %d\n", label, remain)
		}
		return remain
	}

	before := credit("1) 余额（基线）")

	if *doReport {
		rid := fmt.Sprintf("wb2api-checkinprobe-%d", time.Now().UnixMilli())
		if err := c.ReportChatActivity(a, rid, ""); err != nil {
			fmt.Printf("%-24s 失败: %v\n", "2) 活跃上报 /v2/report", err)
		} else {
			fmt.Printf("%-24s 成功\n", "2) 活跃上报 /v2/report")
		}
		time.Sleep(2 * time.Second) // 给上游事件处理留时间（与调度器同口径）
		credit("3) 余额（上报后）")
	}

	cerr := c.DailyCheckin(a)
	switch {
	case cerr == nil:
		fmt.Printf("%-24s 成功\n", "4) 签到 daily-checkin")
	case upstream.IsAlreadyCheckin(cerr):
		fmt.Printf("%-24s 已签到（幂等）\n", "4) 签到 daily-checkin")
	default:
		fmt.Printf("%-24s 失败: %v\n", "4) 签到 daily-checkin", cerr)
	}
	time.Sleep(2 * time.Second)
	after := credit("5) 余额（签到后）")

	if days, err := c.GrowthStreak(a); err != nil {
		fmt.Printf("%-24s 查询失败: %v\n", "6) 连登 streak", err)
	} else {
		fmt.Printf("%-24s %d 天\n", "6) 连登 streak", days)
	}

	fmt.Println()
	var ue *upstream.Error
	status := 0
	if errors.As(cerr, &ue) {
		status = ue.Status
	}
	switch {
	case cerr == nil || upstream.IsAlreadyCheckin(cerr):
		fmt.Println("结论：国际版 **支持** 签到 —— daily-checkin 端点可用。")
		switch {
		case before >= 0 && after > before:
			fmt.Printf("      本轮积分 +%d（%d → %d）。\n", after-before, before, after)
		case before >= 0 && after >= 0:
			fmt.Printf("      本轮积分未变化（%d → %d）：签到可能只记连登/今日已计过，或积分走次日发放。\n", before, after)
		}
	case status == 400 && ue != nil &&
		(strings.Contains(ue.Msg, "签到活动") || strings.Contains(ue.Msg, "未开启") || strings.Contains(ue.Msg, "过期")):
		fmt.Printf("结论：端点存在，但国际版 **签到活动未开启 / 已过期**（HTTP %d，code 10001）。\n", status)
		fmt.Println("      「登录即签到」在国际版体现为**连登（streak）**，由每日活跃上报推进，")
		fmt.Println("      但不直接发积分——国际版的积分增益只有一次性 trial。")
		fmt.Println("      => 无需为 global 实现 daily-checkin；现有每天一次 /v2/report 已等效覆盖。")
	case status == 404 || status == 403:
		fmt.Printf("结论：国际版 **没有** 签到体系（daily-checkin 返回 HTTP %d）。\n", status)
		fmt.Println("      「登录即签到」不成立；国际版的日常增益只有活跃上报与一次性 trial。")
	case status == 401:
		fmt.Println("结论：无法判定 —— access token 已失效（401），先刷新 token 再探测。")
	default:
		fmt.Printf("结论：无法判定（签到返回 %v）。\n", cerr)
	}
}

// loadOne 只读加载一个账号：单文件优先；否则扫目录挑 uid 匹配（缺省第一个 global）。
// 不用 auth.LoadDir —— 它会 backfill 并 SaveAtomic 落盘，违背「不写文件」的约束。
func loadOne(file, dir, uid string) (*auth.Auth, error) {
	if file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		a, err := auth.Parse(raw)
		if err != nil {
			return nil, err
		}
		a.FilePath = file
		return a, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			continue
		}
		if uid != "" {
			if a.UID != uid {
				continue
			}
		} else if !a.IsGlobal() {
			continue
		}
		a.FilePath = path
		return a, nil
	}
	if uid != "" {
		return nil, fmt.Errorf("目录 %s 里找不到 uid=%s", dir, uid)
	}
	return nil, fmt.Errorf("目录 %s 里没有 global 账号（可用 -uid 指定 CN 账号）", dir)
}
