// 成长任务周期清扫的端到端测试：用 httptest 假上游跑真实链路。
//
// 覆盖：
//   - accept 只对 not_accepted 的任务发（默认口径与小程序口径分开发）；
//   - claim 只对「进度达标且未领取」的发，走 Web 域路径式端点；
//   - 幂等：第二轮清扫不再重复 accept/claim（假上游在动作后真实改状态）；
//   - 门控：global 与企业版账号一个上游请求都不发；
//   - 领奖失败只记错误，不中断、不挂住。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// sweepFakeTask 假上游里的一条任务（状态可变：accept/claim 会真实改它）。
type sweepFakeTask struct {
	Code    string
	Accept  string // accept_status
	Current int64
	Target  int64
	MPOnly  bool // 只在 mp 口径下发
}

// sweepFakeUpstream 假 growth 上游：默认口径 + 小程序口径两份任务表。
type sweepFakeUpstream struct {
	mu           sync.Mutex
	tasks        []sweepFakeTask
	accepted     []string // 收到的 accept 任务码（默认口径）
	acceptedMP   []string // 收到的 accept 任务码（mp 口径）
	claimedPaths []string // 收到的 claim 请求路径
	hits         int      // 总请求数（门控用例断言用）
	claimStatus  int      // 非 0 时 claim 直接返回该 HTTP 状态（错误用例）
}

func newSweepFakeUpstream() *sweepFakeUpstream {
	return &sweepFakeUpstream{tasks: []sweepFakeTask{
		{Code: "chat_task", Accept: "not_accepted", Target: 5},                   // 待接受
		{Code: "done_task", Accept: "accepted", Target: 5, Current: 5},           // 达标未领 → 该领
		{Code: "claimed_task", Accept: "claimed", Target: 5, Current: 5},         // 已领 → 不动
		{Code: "wip_task", Accept: "accepted", Target: 5, Current: 2},            // 进行中 → 不动
		{Code: "school_season", Accept: "not_accepted", Target: 1, MPOnly: true}, // mp 专属待接受
	}}
}

// snapshot 取一份动作记录副本（避免测试里持锁读）。
func (f *sweepFakeUpstream) snapshot() (accept, acceptMP, claim []string, hits int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.accepted...), append([]string(nil), f.acceptedMP...),
		append([]string(nil), f.claimedPaths...), f.hits
}

func (f *sweepFakeUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mp := r.Header.Get("X-Client-Platform") == "miniprogram"

		f.mu.Lock()
		f.hits++
		f.mu.Unlock()

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/activity/growth/tasks"):
			f.mu.Lock()
			items := make([]map[string]any, 0, len(f.tasks))
			for _, t := range f.tasks {
				if t.MPOnly && !mp {
					continue
				}
				items = append(items, map[string]any{
					"task_code":     t.Code,
					"accept_status": t.Accept,
					"status":        "in_progress",
					"reward_credit": 100,
					"reward_energy": 5,
					"has_reward":    true,
					"progress":      map[string]any{"current": t.Current, "target": t.Target},
				})
			}
			f.mu.Unlock()
			writeJSONBody(w, map[string]any{"tasks": items})

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/activity/growth/tasks/accept"):
			var body struct {
				TaskCodes []string `json:"task_codes"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			for _, c := range body.TaskCodes {
				if mp {
					f.acceptedMP = append(f.acceptedMP, c)
				} else {
					f.accepted = append(f.accepted, c)
				}
				for i := range f.tasks {
					if f.tasks[i].Code == c && f.tasks[i].Accept == "not_accepted" {
						f.tasks[i].Accept = "accepted" // 真实改状态：幂等才可验证
					}
				}
			}
			f.mu.Unlock()
			writeJSONBody(w, map[string]any{})

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claim"):
			f.mu.Lock()
			f.claimedPaths = append(f.claimedPaths, r.URL.Path)
			status := f.claimStatus
			if status == 0 {
				// /activity/growth/tasks/<code>/claim → 取倒数第二段
				parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
				if len(parts) >= 2 {
					code := parts[len(parts)-2]
					for i := range f.tasks {
						if f.tasks[i].Code == code {
							f.tasks[i].Accept = "claimed"
						}
					}
				}
			}
			f.mu.Unlock()
			if status != 0 {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"code":400,"msg":"task not completed"}`))
				return
			}
			writeJSONBody(w, map[string]any{"already_claimed": false, "credit": 100, "energy": 5})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func writeJSONBody(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK", "data": data})
}

// noSweepThrottle 关掉清扫节流（测试不该真睡 reportGap=1.05s）。
func noSweepThrottle(t *testing.T) {
	t.Helper()
	oldGap, oldReport := taskSweepAccountGap, reportGap
	taskSweepAccountGap, reportGap = 0, 0
	t.Cleanup(func() { taskSweepAccountGap, reportGap = oldGap, oldReport })
}

// newSweepPanel 组装一个指向假上游的面板。
func newSweepPanel(t *testing.T, f *sweepFakeUpstream, accts ...*auth.Auth) *Panel {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)

	pl := pool.New("")
	for _, a := range accts {
		pl.Add(a)
	}
	c := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	return New(Config{Version: "test", APIKey: "k", Pool: pl, Upstream: c})
}

func cnAccount(uid string) *auth.Auth {
	return &auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", Domain: "www.workbuddy.cn"}
}

func TestTaskSweepAcceptsAndClaims(t *testing.T) {
	noSweepThrottle(t)
	f := newSweepFakeUpstream()
	p := newSweepPanel(t, f, cnAccount("u1"))

	p.RunTaskSweepOnce()

	accept, acceptMP, claim, _ := f.snapshot()
	// accept：默认口径只该收 chat_task（claimed/已 accepted/进行中都不重报）。
	if len(accept) != 1 || accept[0] != "chat_task" {
		t.Errorf("默认口径 accept = %v，want [chat_task]", accept)
	}
	// mp 口径只该收 mp 专属的 school_season。
	if len(acceptMP) != 1 || acceptMP[0] != "school_season" {
		t.Errorf("mp 口径 accept = %v，want [school_season]", acceptMP)
	}
	// claim：只该领 done_task（5/5 未领）。
	if len(claim) != 1 {
		t.Fatalf("claim 请求 = %v，want 只有 done_task 一条", claim)
	}
	if want := "/activity/growth/tasks/done_task/claim"; claim[0] != want {
		t.Errorf("claim 路径 = %q，want %q（Web 域 + 任务码在路径里）", claim[0], want)
	}
}

// 幂等：假上游在 accept/claim 后真实改状态，第二轮清扫不该再产生任何动作。
func TestTaskSweepIdempotentAcrossRuns(t *testing.T) {
	noSweepThrottle(t)
	f := newSweepFakeUpstream()
	p := newSweepPanel(t, f, cnAccount("u1"))

	p.RunTaskSweepOnce()
	a1, m1, c1, _ := f.snapshot()
	if len(a1) == 0 || len(m1) == 0 || len(c1) == 0 {
		t.Fatalf("第一轮应有动作：accept=%v acceptMP=%v claim=%v", a1, m1, c1)
	}

	p.RunTaskSweepOnce()
	a2, m2, c2, _ := f.snapshot()
	if len(a2) != len(a1) || len(m2) != len(m1) || len(c2) != len(c1) {
		t.Errorf("第二轮不该新增动作：accept %d→%d, acceptMP %d→%d, claim %d→%d",
			len(a1), len(a2), len(m1), len(m2), len(c1), len(c2))
	}
}

// 门控：global 与企业版账号不发起任何上游请求。
func TestTaskSweepSkipsGlobalAndEnterprise(t *testing.T) {
	noSweepThrottle(t)
	f := newSweepFakeUpstream()

	g := cnAccount("global1")
	g.Domain = "www.workbuddy.ai"
	if _, err := auth.BackfillRealmFor(g, "global"); err != nil {
		t.Fatal(err)
	}
	e := cnAccount("ent1")
	e.EnterpriseID = "ent-1"

	p := newSweepPanel(t, f, g, e)
	p.RunTaskSweepOnce()

	if _, _, _, hits := f.snapshot(); hits != 0 {
		t.Errorf("global/企业版账号不该发起上游请求，实际 %d 次", hits)
	}
}

// 领奖失败（上游 400）只记错误，不中断、不挂住。
func TestTaskSweepClaimErrorDoesNotHang(t *testing.T) {
	noSweepThrottle(t)
	f := newSweepFakeUpstream()
	f.claimStatus = http.StatusBadRequest
	p := newSweepPanel(t, f, cnAccount("u1"))

	done := make(chan struct{})
	go func() { p.RunTaskSweepOnce(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("领奖失败不该让清扫挂住")
	}
	if _, _, claim, _ := f.snapshot(); len(claim) == 0 {
		t.Error("应至少尝试过一次领奖（失败也要记下来）")
	}
}
