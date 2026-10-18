package panel

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// flexString 宽容字符串：同一个字段在不同导出方那里写法不同——cockpit tools 把
// status 写成字符串（"active"），另一些导出方写成数字（1）。Go 的 encoding/json 对
// 类型不匹配是**硬错误**，一个字段不符就让整份文件导入失败，报错形如
//
//	invalid json: json: cannot unmarshal number into Go struct field cockpitAccount.status of type string
//
// 用户看到这个完全无从下手（既不知道是哪个文件、也不知道该改什么）。这里对
// string / number / bool / null 一律收下并转成字符串：导入端只把这些字段当提示
// 用（真正的判据是 uid/access_token 与 domain 推断的 realm），类型宽松没有风险。
type flexString string

func (s *flexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*s = ""
		return nil
	}
	if b[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*s = flexString(v)
		return nil
	}
	// 数字 / true / false 等：保留字面量文本（数字 1 → "1"）。
	*s = flexString(b)
	return nil
}

// trim 取修剪后的值——导入端所有字段都是「先 TrimSpace 再用」。
func (s flexString) trim() string { return strings.TrimSpace(string(s)) }

// cockpitAccount 映射 cockpit tools 导出格式的单个账号。
//
// 文本字段一律用 flexString（见上）：导出方把 id/uid/status 写成数字是实测存在的，
// 用 string 会让整份文件报废。数值字段保持 int64（int 类型不符同样会报错，但实测
// 各导出方在时间戳上都是数字，且 convertInt 已做量级自适应）。
type cockpitAccount struct {
	ID             flexString `json:"id"`
	Email          flexString `json:"email"`
	UID            flexString `json:"uid"`
	Nickname       flexString `json:"nickname"`
	AccessToken    flexString `json:"access_token"`
	RefreshToken   flexString `json:"refresh_token"`
	TokenType      flexString `json:"token_type"`
	ExpiresAt      int64      `json:"expires_at"`
	Domain         flexString `json:"domain"`
	DosageNotify   flexString `json:"dosage_notify_code"`
	PaymentType    flexString `json:"payment_type"`
	Status         flexString `json:"status"`
	UsageUpdatedAt int64      `json:"usage_updated_at"`
	LastCheckin    int64      `json:"last_checkin_time"`
	CheckinStreak  int        `json:"checkin_streak"`
	CreatedAt      int64      `json:"created_at"`
	LastUsed       int64      `json:"last_used"`
}

// importPlan 单条 cockpit 记录归一化后的导入方案（planImportAccount 的产物）。
type importPlan struct {
	auth     *auth.Auth
	realm    string
	warnings []string
}

// jwtHint JWT payload 里会用到的声明。
//
// 导入端只用 Exp/Sub（过期时间与 uid 回填）；导出端另用 PreferredUsername/Email——
// 实例落盘的昵称是**打码形态**（如 703****0），导出时用 token 里的全量用户名还原，
// 并与打码值交叉校验（见 export.go 的 maskMatches）。
type jwtHint struct {
	Exp               int64  `json:"exp"`
	Sub               string `json:"sub"`
	PreferredUsername string `json:"preferred_username"`
	Email             string `json:"email"`
}

// importCockpit 接收 cockpit tools 导出的 JSON 文件，批量导入账号到池中。
//
//	POST /panel/api/import/cockpit
//	Content-Type: multipart/form-data
//	Body: file=<json>
//
// 返回 {ok, total, imported, skipped, errors, warnings}。
//
// errors  = 被跳过的账号及原因（一个都没导入时前端按失败展示）；
// warnings = 已导入但需要用户知情的点（如缺 refresh_token）。
func (p *Panel) importCockpit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "parse form: "+err.Error())
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing file field: "+err.Error())
		return
	}
	defer file.Close()

	raw, err := io.ReadAll(file)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read file: "+err.Error())
		return
	}

	records, err := decodeCockpitAccounts(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if len(records) == 0 {
		writeErr(w, http.StatusBadRequest, "empty accounts array")
		return
	}

	var imported, skipped int
	var errs, warns []string

	for i, rec := range records {
		// 单条记录本身类型不符：只跳过这一条（带序号，便于用户定位到具体哪一条）。
		if rec.err != nil {
			skipped++
			errs = append(errs, fmt.Sprintf("第 %d 条记录解析失败：%v", i+1, rec.err))
			continue
		}
		plan, err := planImportAccount(rec.acc, p.cfg.AuthDir)
		if err != nil {
			skipped++
			errs = append(errs, fmt.Sprintf("第 %d 条：%v", i+1, err))
			continue
		}
		if err := plan.auth.SaveAtomic(); err != nil {
			skipped++
			errs = append(errs, fmt.Sprintf("uid=%s: save auth failed: %v", plan.auth.UID, err))
			continue
		}
		warns = append(warns, plan.warnings...)

		uid := plan.auth.UID
		p.cfg.Pool.Add(plan.auth)
		p.cfg.Pool.Revive(uid)

		// 顺带签到/激活（幂等；失败仅记日志，不阻断导入）。
		// Upstream 为 nil（单测/裁剪部署）时整体跳过，导入本身不依赖它。
		if p.cfg.Upstream != nil {
			if plan.realm == "global" {
				if activated, err := p.cfg.Upstream.GlobalCompleteRegistration(plan.auth); err != nil {
					log.Printf("panel: import global 注册激活 uid=%s: %v", uid, err)
				} else if activated {
					log.Printf("panel: import global 注册激活 uid=%s 完成", uid)
				}
				if claimed, err := p.cfg.Upstream.ClaimTrial(plan.auth); err != nil {
					log.Printf("panel: import global trial uid=%s: %v", uid, err)
				} else if claimed {
					log.Printf("panel: import global trial uid=%s 已领", uid)
				}
			} else {
				if err := p.cfg.Upstream.DailyCheckin(plan.auth); err != nil {
					log.Printf("panel: import checkin uid=%s: %v", uid, err)
				}
			}
			if rm, tt, err := p.cfg.Upstream.UserResource(plan.auth); err == nil {
				p.cfg.Pool.ReenableIfCredits(uid, rm, tt)
			}
		}

		imported++
	}

	total := len(records)
	log.Printf("panel: cockpit import finished total=%d imported=%d skipped=%d warnings=%d",
		total, imported, skipped, len(warns))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"total":    total,
		"imported": imported,
		"skipped":  skipped,
		"errors":   errs,
		"warnings": warns,
	})
}

// decodedAccount 一条已解码（或解码失败）的记录。err 非 nil 表示**该条**类型不符，
// 只跳过这一条并把原因回报给面板，不影响同一文件里的其他账号。
type decodedAccount struct {
	acc cockpitAccount
	err error
}

// decodeCockpitAccounts 把上传的 JSON 归一化为账号列表。
//
// 兼容三种实际形态（此前只认第一种，其余全部以「invalid json」告终）：
//   - 顶层数组 [{...}]                     —— 契约形态（导出脚本 combined/split 都是它）
//   - 顶层单对象 {...}                      —— 手工/单号导出
//   - 顶层信封 {"accounts"|"data"|"list"|"items": [...]}
//
// 另外剥掉 UTF-8 BOM：Go 的 encoding/json **不吞 BOM**，而 Windows PowerShell 5.1
// 的 `Set-Content -Encoding utf8` 会写 BOM——带 BOM 的文件在这里会得到
// "invalid character 'ï' looking for beginning of value"，用户完全无从下手。
//
// **逐条解码**：先只解出数组元素（[]json.RawMessage），再逐条解成 cockpitAccount。
// 这样某一条记录字段类型不符时只跳过该条并回报「第 N 条：…」，而不是像整份一次性
// 解码那样让整个文件报废——一个字段写错就丢掉几十个账号，用户还得自己二分定位。
// 返回的 error 只表示**结构性**问题（不是数组/对象、空文件、语法错误）。
func decodeCockpitAccounts(raw []byte) ([]decodedAccount, error) {
	b := bytes.TrimSpace(bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF}))
	if len(b) == 0 {
		return nil, errors.New("文件为空")
	}
	switch b[0] {
	case '[':
		return decodeRecords(b)
	case '{':
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(b, &probe); err != nil {
			return nil, err
		}
		for _, key := range []string{"accounts", "data", "list", "items"} {
			if v, ok := probe[key]; ok && len(v) > 0 && v[0] == '[' {
				return decodeRecords(v)
			}
		}
		var one cockpitAccount
		if err := json.Unmarshal(b, &one); err != nil {
			return nil, err
		}
		return []decodedAccount{{acc: one}}, nil
	default:
		return nil, errors.New("顶层需为账号数组 [{...}]")
	}
}

// decodeRecords 逐条解码数组元素，收集每条的失败原因（不因单条失败而中止）。
// 数组本身语法坏了仍按**结构性**错误返回——那是文件级问题，报成「第 1 条失败」会误导。
func decodeRecords(arr []byte) ([]decodedAccount, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(arr, &raws); err != nil {
		return nil, err
	}
	out := make([]decodedAccount, 0, len(raws))
	for _, r := range raws {
		var a cockpitAccount
		if err := json.Unmarshal(r, &a); err != nil {
			out = append(out, decodedAccount{err: err})
			continue
		}
		out = append(out, decodedAccount{acc: a})
	}
	return out, nil
}

// planImportAccount 把一条 cockpit 记录归一化为待落盘的凭证。纯函数：不碰池、不碰
// 网络、不落盘（FilePath 只赋值），便于单测覆盖各种导出形态。
//
// 必填只有 uid + access_token——**refresh_token 允许为空**。
// cockpit tools 对「只拿到 access token」的账号会导出 refresh_token="" 且
// expires_at=0；旧实现把它与「字段全缺」合并成同一个跳过分支，用户看到的是
// 「导入完成：成功 0 个，跳过 N 个」且原因只进 console.warn，面板上什么都不显示。
// 但 access token 自带 JWT exp，账号在到期前完全可用，没有理由拒绝：
//   - 过期时间按 expires_at（毫秒/秒自适应）→ access_token 的 JWT exp 依次回退；
//   - uid 缺失时回退 JWT 的 sub（同一份 JWT 里 sub 就是 uid）。
//
// 无 refresh_token 时给出警告：access token 到期后无法自动续期，届时 handler 会
// 明确禁用该账号并提示重新登录（而不是每次请求都在刷新失败上打转）。
func planImportAccount(acc cockpitAccount, authDir string) (importPlan, error) {
	at := acc.AccessToken.trim()
	if at == "" {
		return importPlan{}, fmt.Errorf("missing access_token (id=%s)", acc.ID)
	}
	hints := jwtClaims(at)

	uid := acc.UID.trim()
	if uid == "" {
		uid = strings.TrimSpace(hints.Sub)
	}
	if uid == "" {
		return importPlan{}, fmt.Errorf("missing uid (id=%s)", acc.ID)
	}
	if !validImportUID(uid) {
		return importPlan{}, fmt.Errorf("invalid uid %q (id=%s)", uid, acc.ID)
	}

	rt := acc.RefreshToken.trim()
	expiresAt := normalizeExpiry(acc.ExpiresAt)
	if expiresAt <= 0 {
		expiresAt = hints.Exp
	}

	var warnings []string
	switch {
	case rt == "" && expiresAt > 0:
		warnings = append(warnings, fmt.Sprintf(
			"uid=%s: 无 refresh_token——access token 将于 %s 到期，之后无法自动续期（届时账号被禁用，需重新登录或重新导入）",
			uid, time.Unix(expiresAt, 0).Format("2006-01-02 15:04")))
	case rt == "":
		warnings = append(warnings, fmt.Sprintf(
			"uid=%s: 无 refresh_token 且无法确定 access token 过期时间——首次请求即会被判为需刷新并禁用，请重新登录获取完整凭证", uid))
	case expiresAt <= 0:
		warnings = append(warnings, fmt.Sprintf(
			"uid=%s: 无法确定 access token 过期时间（expires_at 无效且非 JWT），进池后立即刷新", uid))
	}

	nickname := acc.Nickname.trim()
	if nickname == "" {
		nickname = acc.Email.trim()
	}

	a := &auth.Auth{
		AccessToken:  at,
		RefreshToken: rt,
		ExpiresAt:    expiresAt,
		Domain:       acc.Domain.trim(),
		UID:          uid,
		Nickname:     nickname,
		FilePath:     filepath.Join(authDir, fmt.Sprintf("workbuddy-%s.json", uid)),
	}

	// 按 domain 推断 realm：workbuddy.ai 家族 → global，否则 cn。
	realm := auth.ResolveRealm("", acc.Domain.trim())
	if realm == "global" {
		if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
			return importPlan{}, fmt.Errorf("uid=%s: set realm failed: %v", uid, err)
		}
	} else {
		_, _ = a.BackfillRealm()
	}
	return importPlan{auth: a, realm: realm, warnings: warnings}, nil
}

// normalizeExpiry 把导入端 expires_at 归一化为 Unix 秒。
//
// 契约是毫秒（cockpit tools 输出，导出脚本也 ×1000），但实测存在直接给秒、或给 0
// 的导出。旧实现无条件 /1000：把「秒」当毫秒除会得到 1970 年的过期时间（非 0，
// 绕过 fallback），账号一进池就被 NeedsRefresh 判为需刷新。这里按量级判别：
// ≥1e12 视为毫秒（2001-09-09 之后），≥1e9 视为已是秒，其余量级荒谬 → 0（交给
// JWT exp 兜底）。
func normalizeExpiry(v int64) int64 {
	switch {
	case v <= 0:
		return 0
	case v >= 1e12:
		return v / 1000
	case v >= 1e9:
		return v
	default:
		return 0
	}
}

// jwtClaims 解出 JWT payload 里的 exp/sub。
//
// **不校验签名**：这里只把 token 自带的信息当作「过期时间/uid 的提示」，不构成任何
// 信任判断（真正的信任在上游对每次请求的响应里）。非 JWT 或解不出时返回零值。
func jwtClaims(token string) jwtHint {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return jwtHint{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return jwtHint{}
	}
	var h jwtHint
	if json.Unmarshal(payload, &h) != nil {
		return jwtHint{}
	}
	return h
}

// validImportUID 校验导入 uid 是否可用于拼文件名（同 login.go validUID 口径）。
func validImportUID(uid string) bool {
	if uid == "" || len(uid) > 64 {
		return false
	}
	for _, c := range uid {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}
