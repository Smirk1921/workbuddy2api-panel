// probe 逐个账号向上游发一次最小对话请求，用于排查「风控 / 限流 / 失效」账号。
//
// 只读设计（重要）：只用 auths 里现有的 access token 发一次请求，**不刷新 token、
// 不写任何文件**——避免与正在运行的实例争抢 refresh token（部分上游的 refresh
// token 是一次性的，两个进程同时刷会互相作废）。
//
// 用法（项目根目录）：
//
//	go run ./cmd/probe -dir ./auths
//	go run ./cmd/probe -dir ./auths -c 1 -delay 1s     # 更保守（避免同 IP 突发）
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

type result struct {
	uid, nick, realm string
	status           string
	ok               bool
	detail           string
	ms               int64
}

// kindLabel 把上游错误分类翻成运维可读的中文标签；第二返回值表示「是否算正常」。
func kindLabel(k upstream.ErrKind) (string, bool) {
	switch k {
	case upstream.ErrNone:
		return "正常", true
	case upstream.ErrSoftRate:
		return "限流 429", false
	case upstream.ErrWafBlock:
		return "风控 WAF 403", false
	case upstream.ErrHardCredit:
		return "余额不足 402", false
	case upstream.ErrSessionDead:
		return "session 失效 12153", false
	case upstream.ErrAccountFault:
		return "账号故障 11140/14017", false
	case upstream.ErrModelBlocked:
		return "该后端无此模型 11102", false
	case upstream.ErrServer:
		return "上游 5xx", false
	case upstream.ErrNotFound:
		return "404 偶发", false
	case upstream.ErrContentBlocked:
		return "内容拦截", false
	case upstream.ErrBadParams:
		return "参数错误 11101", false
	case upstream.ErrPromptTooLong:
		return "上下文超限 11115", false
	case upstream.ErrImageInvalid:
		return "图片无效", false
	case upstream.ErrClient:
		return "其他 4xx/业务错误", false
	}
	return fmt.Sprintf("未知 Kind=%d", int(k)), false
}

func main() {
	dir := flag.String("dir", "./auths", "auths 目录（只读）")
	modelCN := flag.String("model-cn", "cn:glm-5.3-flash", "CN 账号探测用模型（带域前缀）")
	modelGlobal := flag.String("model-global", "global:gemini-3.8-flash", "global 账号探测用模型（带域前缀）")
	conc := flag.Int("c", 2, "并发数（同一出口 IP 建议 ≤3，避免突发被判风控）")
	delay := flag.Duration("delay", 700*time.Millisecond, "请求启动间隔")
	timeout := flag.Duration("timeout", 60*time.Second, "单账号超时")
	flag.Parse()

	auths, err := auth.LoadDir(*dir)
	if err != nil {
		fmt.Println("加载 auths 失败:", err)
		return
	}
	sort.Slice(auths, func(i, j int) bool { return auths[i].UID < auths[j].UID })
	fmt.Printf("待探测账号 %d 个（并发 %d，间隔 %s）\n\n", len(auths), *conc, *delay)

	up := upstream.New()
	up.HeaderTimeout = *timeout
	up.HTTP.Timeout = *timeout

	var mu sync.Mutex
	var results []result
	sem := make(chan struct{}, *conc)
	var wg sync.WaitGroup

	for _, a := range auths {
		wg.Add(1)
		go func(a *auth.Auth) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			model := *modelCN
			if a.IsGlobal() {
				model = *modelGlobal
			}
			bare := model
			if i := strings.Index(bare, ":"); i >= 0 {
				bare = bare[i+1:] // 上游 body 要裸模型名（域前缀是网关路由约定）
			}
			nick := a.Nickname
			if nick == "" {
				nick = a.UID[:8]
			}
			realm := a.Realm()
			res := result{uid: a.UID, nick: nick, realm: realm}

			// access token 已过期：不刷新（只读原则），直接标注跳过。
			if a.ExpiresAt > 0 && time.Now().Unix() >= a.ExpiresAt {
				res.status = "token 已过期"
				res.detail = "需由运行实例自行刷新（探测不代刷）"
				mu.Lock()
				results = append(results, res)
				mu.Unlock()
				return
			}

			body := []byte(fmt.Sprintf(
				`{"model":%q,"messages":[{"role":"user","content":"hi"}],"max_tokens":1,"stream":true}`, bare))
			body = upstream.PrepareBodyOpt(body, false)
			meta := upstream.ChatMeta{ConversationRequestID: fmt.Sprintf("probe-%d-%s", time.Now().UnixNano(), a.UID[:8])}

			ctx, cancel := context.WithTimeout(context.Background(), *timeout)
			defer cancel()
			start := time.Now()
			rc, status, respBody, cerr := up.ChatStreamContext(ctx, a, body, "", meta)
			res.ms = time.Since(start).Milliseconds()

			switch {
			case cerr == nil && rc != nil:
				// 流已打开：读一点确认有数据（首帧到达即视为可用），随后关闭。
				// io.ReadCloser 没有 SetReadDeadline，用 goroutine + select 兜住停滞
				// （上游假流式/卡住时不能把探测挂死；Close 会解除阻塞的读）。
				buf := make([]byte, 2048)
				type readRes struct {
					n   int
					err error
				}
				ch := make(chan readRes, 1)
				go func() {
					n, rerr := rc.Read(buf)
					ch <- readRes{n, rerr}
				}()
				select {
				case rr := <-ch:
					rc.Close()
					if rr.n > 0 || rr.err == nil || errors.Is(rr.err, io.EOF) {
						res.status, res.ok = "正常", true
						res.detail = fmt.Sprintf("HTTP %d · 首帧 %d 字节", status, rr.n)
					} else {
						res.status = "流读取失败"
						res.detail = rr.err.Error()
					}
				case <-time.After(15 * time.Second):
					rc.Close() // 解除阻塞的读
					res.status, res.ok = "正常", true
					res.detail = fmt.Sprintf("HTTP %d · 首帧 15s 内未到达（疑似上游攒批/假流式）", status)
				}
			case cerr == nil:
				res.status, res.ok = "正常", true
				res.detail = fmt.Sprintf("HTTP %d（无流）", status)
			default:
				var uerr *upstream.Error
				if errors.As(cerr, &uerr) {
					res.status, res.ok = kindLabel(uerr.Kind)
					res.detail = fmt.Sprintf("HTTP %d · %s", uerr.Status, truncate(uerr.Msg, 120))
					if uerr.RetryAfter > 0 {
						res.detail += fmt.Sprintf(" · Retry-After %s", uerr.RetryAfter)
					}
				} else {
					res.status = "网络/超时"
					res.detail = truncate(cerr.Error(), 120)
				}
				if len(respBody) > 0 && res.detail == "" {
					res.detail = truncate(string(respBody), 120)
				}
			}

			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}(a)
		time.Sleep(*delay) // 启动间隔：避免同 IP 突发
	}
	wg.Wait()

	// 输出：按状态分组，便于一眼看出「谁被风控了」。
	sort.Slice(results, func(i, j int) bool { return results[i].uid < results[j].uid })
	fmt.Printf("%-10s %-14s %-7s %-22s %s\n", "UID", "昵称", "域", "结果", "详情")
	fmt.Println(strings.Repeat("-", 110))
	counts := map[string]int{}
	for _, r := range results {
		fmt.Printf("%-10s %-14s %-7s %-22s %s\n", r.uid[:8], truncate(r.nick, 14), r.realm, r.status, r.detail)
		counts[r.status]++
	}
	fmt.Println(strings.Repeat("-", 110))
	fmt.Println("汇总：")
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return counts[keys[i]] > counts[keys[j]] })
	for _, k := range keys {
		fmt.Printf("  %-24s %d\n", k, counts[k])
	}
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
