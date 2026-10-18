// batch.go 账号批量操作：统一的「对一组账号执行同一动作」实现。
//
// 两个入口共用同一份语义（不重复实现）：
//   - POST /panel/api/accounts/batch           —— 显式 uid 列表（前端表格多选）
//   - POST /panel/api/groups/{scope}/{action}  —— 组作用域（__all__ / __ungrouped__ / 组名）
//
// 动作与账号行按钮 **1:1 对齐**：pause / resume / disable / revive / threshold /
// priority / group / remove。批量只是「在作用域内循环调用既有单账号 setter」，
// 不引入任何新的池行为。
//
// 与旧实现的差异（都是实测出来的坑）：
//   - `freeze` 是 `disable` 的兼容别名。旧版组批量按钮叫「冻结」但实际是 Disable
//     （真禁用，需手动恢复），与账号状态里的 low-credit `frozen`（低积分自动冻结，
//     余额恢复会自己解冻）完全不是一回事——别名保留兼容，新前端一律用 disable。
//   - `affected` 只表示「动作被应用到的账号数」；新增 `changed` 表示「状态真的变了」。
//     旧版把幂等 no-op 也算进 affected，运维看不出到底动了几个。
//   - 每个账号先做**非阻塞** per-account 试锁：正在跑任务链路的账号本轮跳过并如实
//     回报，避免批量 remove/disable 与任务动作交错产生半状态。
//   - remove 的 confirm 与**原始作用域串**比对（旧实现把 `__ungrouped__` 归一化成
//     空串后再比对，导致「未分组」的批量移除永远 400）。
package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// maxBatchUIDs 单次批量请求的账号上限：防误粘超长列表把一次请求拖成几分钟。
const maxBatchUIDs = 500

// batchDisableReason 批量「禁用」写入的 reason。沿用旧 `group freeze (panel)` 字面量，
// 这样升级前被批量禁用过的账号在状态里看起来是同一类（运维按 reason 过滤不会断档）。
const batchDisableReason = "group freeze (panel)"

// supportedBatchActions 错误信息里列出的动作（文档用）。
const supportedBatchActions = "pause/resume/disable/revive/threshold/priority/group/remove"

// errUnknownBatchAction 未知动作：groupAction 对它的历史契约是 404（不是 400）。
var errUnknownBatchAction = errors.New("unknown batch action")

// batchOpt 批量动作的公共参数（按 action 取用）。
type batchOpt struct {
	Value   *float64 // threshold：阈值；priority：0/1（缺省 1）
	Group   *string  // group：目标组名（空串 = 移出分组）
	Confirm *string  // remove：确认串
}

// batchItem 单账号执行结果。
type batchItem struct {
	UID     string `json:"uid"`
	Status  string `json:"status"` // done | skipped | error
	Message string `json:"message,omitempty"`
}

// batchResult 批量执行汇总。
type batchResult struct {
	Total    int         `json:"total"`
	Affected int         `json:"affected"` // 动作被应用到的账号数（含幂等 no-op）
	Changed  int         `json:"changed"`  // 状态真的发生变化的账号数
	Skipped  int         `json:"skipped"`  // 试锁失败 / 账号不存在
	Removed  int         `json:"removed"`  // remove 专用：真正从池里移除的个数
	Items    []batchItem `json:"results,omitempty"`
	FileErrs []string    `json:"file_errors,omitempty"`
}

// canonicalBatchAction 归一化动作名（freeze 是 disable 的兼容别名）。
func canonicalBatchAction(a string) string {
	a = strings.TrimSpace(a)
	if a == "freeze" {
		return "disable"
	}
	return a
}

// batchResponse 统一响应体（groupAction 与 accountsBatch 共用）。
func batchResponse(res batchResult) map[string]any {
	out := map[string]any{
		"ok":       true,
		"total":    res.Total,
		"affected": res.Affected,
		"changed":  res.Changed,
		"skipped":  res.Skipped,
	}
	if res.Removed > 0 {
		out["removed"] = res.Removed
	}
	if len(res.Items) > 0 {
		out["results"] = res.Items
	}
	if len(res.FileErrs) > 0 {
		out["file_errors"] = res.FileErrs
	}
	return out
}

// applyBatchAction 对 targets 执行同一动作。
//
// requiredConfirm 是 remove 必须逐字匹配的确认串：uid 列表模式固定为 "REMOVE"，
// 组作用域模式为**原始 scope 串**（组名 / __all__ / __ungrouped__）。
func (p *Panel) applyBatchAction(action string, targets []pool.Status, opt batchOpt, requiredConfirm string) (batchResult, error) {
	res := batchResult{Total: len(targets)}
	action = canonicalBatchAction(action)

	// 参数校验先行：不要命中一半才报参数错。
	switch action {
	case "threshold":
		if opt.Value == nil {
			return res, fmt.Errorf("threshold action requires body {value: number}")
		}
		if *opt.Value < 0 {
			return res, fmt.Errorf("threshold must be >= 0")
		}
	case "group":
		if opt.Group == nil {
			return res, fmt.Errorf(`group action requires body {group: "组名"}（空串 = 移出分组）`)
		}
		g := strings.TrimSpace(*opt.Group)
		if g == pool.GroupScopeAll || g == pool.GroupScopeUngrouped {
			return res, fmt.Errorf("组名 %q 是保留字（批量作用域哨兵），请换一个名字", g)
		}
		if len([]rune(g)) > 64 {
			return res, fmt.Errorf("组名最长 64 个字符（收到 %d 个）", len([]rune(g)))
		}
	case "remove":
		if opt.Confirm == nil || *opt.Confirm != requiredConfirm {
			return res, fmt.Errorf(`remove requires body {confirm: %q}（逐字匹配以确认不可逆操作）`, requiredConfirm)
		}
	case "pause", "resume", "disable", "revive", "priority":
		// 无额外参数
	default:
		return res, fmt.Errorf("%w: %q（支持 %s）", errUnknownBatchAction, action, supportedBatchActions)
	}

	for _, st := range targets {
		item := batchItem{UID: st.UID}
		// 与任务类动作互斥：非阻塞试锁，占用则本轮跳过（不排队、不阻塞）。
		if !p.tryLockAccount(st.UID) {
			item.Status, item.Message = "skipped", "该账号有任务动作在执行"
			res.Skipped++
			res.Items = append(res.Items, item)
			continue
		}

		if action == "remove" {
			a := p.cfg.Pool.Remove(st.UID)
			p.unlockAccount(st.UID)
			if a == nil {
				item.Status, item.Message = "skipped", "账号不存在（可能已被移除）"
				res.Skipped++
				res.Items = append(res.Items, item)
				continue
			}
			res.Affected++
			res.Changed++
			res.Removed++
			item.Status = "done"
			if a.FilePath != "" {
				if rerr := os.Remove(a.FilePath); rerr != nil && !os.IsNotExist(rerr) {
					res.FileErrs = append(res.FileErrs, st.UID+": "+rerr.Error())
					item.Status, item.Message = "error", "已移出池，但删除凭证文件失败"
				}
			}
			res.Items = append(res.Items, item)
			continue
		}

		changed, msg, err := p.applyBatchOne(action, st, opt)
		p.unlockAccount(st.UID)
		if err != nil {
			item.Status, item.Message = "error", err.Error()
			res.Items = append(res.Items, item)
			continue
		}
		item.Status, item.Message = "done", msg
		res.Affected++
		if changed {
			res.Changed++
		}
		res.Items = append(res.Items, item)
	}
	return res, nil
}

// applyBatchOne 对单账号执行非 remove 动作，返回（是否真的改变了状态, 附加说明, 错误）。
// 说明文字用于前端「批量结果」里解释幂等 no-op（例如「已是禁用状态」）。
func (p *Panel) applyBatchOne(action string, st pool.Status, opt batchOpt) (changed bool, msg string, err error) {
	switch action {
	case "pause":
		if st.Paused {
			return false, "已是暂停选号状态", nil
		}
		p.cfg.Pool.Pause(st.UID)
		return true, "", nil
	case "resume":
		if !st.Paused {
			return false, "未处于暂停状态", nil
		}
		p.cfg.Pool.Resume(st.UID)
		return true, "", nil
	case "disable":
		if st.Disabled {
			return false, "已是禁用状态", nil
		}
		p.cfg.Pool.Disable(st.UID, batchDisableReason)
		return true, "", nil
	case "revive":
		// Revive 是全清：禁用 + 暂停 + 冷却 + 熔断 + 连败降权 + 低积分冻结。
		hadPenalty := st.Disabled || st.Paused || st.Frozen || st.Cooling ||
			st.BreakerFails > 0 || !st.DegradeUntil.IsZero()
		p.cfg.Pool.Revive(st.UID)
		if !hadPenalty {
			return false, "没有可恢复的惩罚态", nil
		}
		return true, "", nil
	case "threshold":
		th := int64(*opt.Value)
		if st.FreezeThreshold == th {
			return false, "阈值未变化", nil
		}
		p.cfg.Pool.SetFreezeThreshold(st.UID, th)
		return true, "", nil
	case "priority":
		on := true
		if opt.Value != nil {
			on = *opt.Value != 0
		}
		if st.Priority == on {
			return false, "优先状态未变化", nil
		}
		p.cfg.Pool.SetPriority(st.UID, on)
		return true, "", nil
	case "group":
		g := strings.TrimSpace(*opt.Group)
		if st.Group == g {
			return false, "分组未变化", nil
		}
		p.cfg.Pool.SetGroup(st.UID, g)
		return true, "", nil
	}
	return false, "", fmt.Errorf("%w: %q", errUnknownBatchAction, action)
}

// targetsByUIDs 按请求顺序解析 uid 列表：去重后转成池状态快照，
// 池里不存在的 uid 单独回报（不让用户以为所有选中项都处理了）。
func (p *Panel) targetsByUIDs(uids []string) (targets []pool.Status, missing []string) {
	byUID := make(map[string]pool.Status)
	for _, st := range p.cfg.Pool.List() {
		byUID[st.UID] = st
	}
	seen := make(map[string]bool, len(uids))
	for _, raw := range uids {
		uid := strings.TrimSpace(raw)
		if uid == "" || seen[uid] {
			continue
		}
		seen[uid] = true
		if st, ok := byUID[uid]; ok {
			targets = append(targets, st)
		} else {
			missing = append(missing, uid)
		}
	}
	return targets, missing
}

// accountsBatch 对显式 uid 列表批量执行动作（面板表格多选）。
//
//	POST /panel/api/accounts/batch
//	{"action":"pause","uids":["…"]}
//	{"action":"threshold","uids":["…"],"value":100}
//	{"action":"group","uids":["…"],"group":"主力"}
//	{"action":"remove","uids":["…"],"confirm":"REMOVE"}
func (p *Panel) accountsBatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action  string   `json:"action"`
		UIDs    []string `json:"uids"`
		Value   *float64 `json:"value"`
		Group   *string  `json:"group"`
		Confirm *string  `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if strings.TrimSpace(body.Action) == "" {
		writeErr(w, http.StatusBadRequest, "missing action")
		return
	}
	if len(body.UIDs) == 0 {
		writeErr(w, http.StatusBadRequest, "uids must not be empty")
		return
	}
	if len(body.UIDs) > maxBatchUIDs {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("一次最多 %d 个账号（收到 %d 个）", maxBatchUIDs, len(body.UIDs)))
		return
	}

	targets, missing := p.targetsByUIDs(body.UIDs)
	res, err := p.applyBatchAction(body.Action, targets,
		batchOpt{Value: body.Value, Group: body.Group, Confirm: body.Confirm}, "REMOVE")
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errUnknownBatchAction) {
			status = http.StatusNotFound
		}
		writeErr(w, status, err.Error())
		return
	}
	for _, uid := range missing {
		res.Items = append(res.Items, batchItem{UID: uid, Status: "skipped", Message: "账号不存在（可能已被移除）"})
		res.Skipped++
		res.Total++
	}
	log.Printf("panel: 批量动作 action=%s 请求 %d 个 uid → 命中 %d 变更 %d 跳过 %d",
		canonicalBatchAction(body.Action), len(body.UIDs), res.Affected, res.Changed, res.Skipped)
	writeJSON(w, http.StatusOK, batchResponse(res))
}
